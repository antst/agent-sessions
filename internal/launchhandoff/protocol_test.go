package launchhandoff

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

const testSecret = "native-secret-SENTINEL-8e17c4"

func testCommand() productruntime.NativeCommand {
	return productruntime.NativeCommand{
		Path: "native", Args: []string{"--mode", "peer"}, Cwd: "/work",
		Env: []productruntime.EnvVar{{Name: "VISIBLE", Value: "ordinary"}},
		SensitiveEnv: []productruntime.SensitiveEnvVar{{
			Name: "BOOTSTRAP_SECRET", Value: productruntime.NewSensitiveValue(testSecret),
		}},
	}
}

func TestBinaryCommandRoundTripAndExactEnvelopeEnvironment(t *testing.T) {
	limits := DefaultLimits()
	encoded, err := encodeCommand(testCommand(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(testSecret)) {
		t.Fatal("test did not exercise secret binary payload")
	}
	decoded, err := decodeCommand(encoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := exactEnvironment(decoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"VISIBLE=ordinary", "BOOTSTRAP_SECRET=" + testSecret}
	if strings.Join(environment, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("environment = %q, want %q", environment, want)
	}
	if _, err := json.Marshal(decoded); err == nil {
		t.Fatal("native command became JSON serializable")
	}
	zero(encoded)
	if bytes.Contains(encoded, []byte(testSecret)) {
		t.Fatal("encoded secret buffer was not cleared")
	}
}

func TestCommandValidationRejectsLeaksDuplicatesAndBounds(t *testing.T) {
	limits := DefaultLimits()
	for _, test := range []struct {
		name    string
		mutate  func(*productruntime.NativeCommand)
		wantErr error
	}{
		{name: "argv leak", mutate: func(command *productruntime.NativeCommand) {
			command.Args = append(command.Args, "--token="+testSecret)
		}, wantErr: ErrInvalid},
		{name: "ordinary env leak", mutate: func(command *productruntime.NativeCommand) {
			command.Env = append(command.Env, productruntime.EnvVar{Name: "LEAK", Value: testSecret})
		}, wantErr: ErrInvalid},
		{name: "duplicate", mutate: func(command *productruntime.NativeCommand) {
			command.Env = append(command.Env, productruntime.EnvVar{Name: "BOOTSTRAP_SECRET", Value: "not-secret"})
		}, wantErr: ErrInvalid},
		{name: "nul", mutate: func(command *productruntime.NativeCommand) { command.Args = append(command.Args, "bad\x00arg") }, wantErr: ErrInvalid},
		{name: "too many args", mutate: func(command *productruntime.NativeCommand) { command.Args = make([]string, limits.MaxArguments+1) }, wantErr: ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := testCommand()
			test.mutate(&command)
			if _, err := encodeCommand(command, limits); !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	command := testCommand()
	command.SensitiveEnv[0].Value = productruntime.NewSensitiveValue(strings.Repeat("x", limits.MaxFieldBytes+1))
	if _, err := encodeCommand(command, limits); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized secret error = %v", err)
	}
}

func TestCommandValidationAllowsEmptyArgument(t *testing.T) {
	command := testCommand()
	command.Args = append(command.Args, "")
	encoded, err := encodeCommand(command, DefaultLimits())
	if err != nil {
		t.Fatalf("empty argv element rejected: %v", err)
	}
	decoded, err := decodeCommand(encoded, DefaultLimits())
	zero(encoded)
	if err != nil || decoded.Args[len(decoded.Args)-1] != "" {
		t.Fatalf("decoded empty argument = %#v, %v", decoded.Args, err)
	}
}

func TestDecodedStringTemporaryBufferIsZeroed(t *testing.T) {
	body := []byte(testSecret)
	value := stringAndZero(body)
	if value != testSecret {
		t.Fatalf("decoded value = %q", value)
	}
	for index, character := range body {
		if character != 0 {
			t.Fatalf("temporary byte %d retained %#x", index, character)
		}
	}
}

func TestProtocolRejectsMalformedFramesAndWrongDigest(t *testing.T) {
	ticket := Ticket{ID: strings.Repeat("a", 32), Contract: ContractVersion}
	claim, err := encodeClaim(ticket)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeClaim(claim)
	if err != nil || decoded != ticket {
		t.Fatalf("claim = %+v, %v", decoded, err)
	}
	claim = append(claim, 0)
	if _, err := decodeClaim(claim); !errors.Is(err, ErrProtocol) {
		t.Fatalf("trailing claim error = %v", err)
	}
	var digest [sha256.Size]byte
	digest[0] = 1
	ack := encodeAck(digest)
	digest[0] = 2
	if err := decodeAck(ack, digest); !errors.Is(err, ErrProtocol) {
		t.Fatalf("wrong digest error = %v", err)
	}
	if err := decodeServerError(encodeError(ErrProtocol)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("protocol error category = %v", err)
	}
}

func FuzzDecodeCommand(f *testing.F) {
	seed, _ := encodeCommand(testCommand(), DefaultLimits())
	f.Add(seed)
	f.Add([]byte("not-a-command"))
	f.Fuzz(func(t *testing.T, body []byte) {
		command, err := decodeCommand(body, DefaultLimits())
		if err != nil {
			return
		}
		if validateCommand(command, DefaultLimits()) != nil {
			t.Fatal("decoder admitted a command rejected by validation")
		}
	})
}
