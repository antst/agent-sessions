package codebuddy

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLsofSocketOwnerRequiresTwoIdenticalExactRecords(t *testing.T) {
	const output = "p4321\x00ccodebuddy\x00f17\x00tIPv4\x00PTCP\x00D0x12\x00i9988\x00n127.0.0.1:7788\x00TST=LISTEN\x00\n"
	var calls int
	verifier := newLsofSocketOwnerVerifier(CommandRunnerFunc(func(_ context.Context, path string, arguments ...string) (CommandResult, error) {
		calls++
		if path != darwinLsofPath || !reflect.DeepEqual(arguments, []string{
			"-nP", "-a", "-p", "4321", "-iTCP@127.0.0.1:7788", "-sTCP:LISTEN", "-F0pftPDinT",
		}) {
			t.Fatalf("lsof invocation = %q %#v", path, arguments)
		}
		return CommandResult{Stdout: output}, nil
	}))
	observation, err := verifier.VerifyOwner(context.Background(), "http://127.0.0.1:7788", 4321)
	if err != nil || calls != 2 || observation.PID != 4321 || observation.Identity == "" {
		t.Fatalf("observation = %#v, calls=%d, err=%v", observation, calls, err)
	}
}

func TestLsofSocketOwnerRejectsWrongPIDEndpointStateAmbiguityAndReuse(t *testing.T) {
	valid := "p4321\x00f17\x00tIPv4\x00PTCP\x00D0x12\x00i9988\x00n127.0.0.1:7788\x00TST=LISTEN\x00\n"
	for name, output := range map[string]string{
		"wrong-pid":      "p4322\x00f17\x00tIPv4\x00PTCP\x00D0x12\x00i9988\x00n127.0.0.1:7788\x00TST=LISTEN\x00\n",
		"wrong-endpoint": "p4321\x00f17\x00tIPv4\x00PTCP\x00D0x12\x00i9988\x00n127.0.0.1:7789\x00TST=LISTEN\x00\n",
		"not-listening":  "p4321\x00f17\x00tIPv4\x00PTCP\x00D0x12\x00i9988\x00n127.0.0.1:7788\x00TST=ESTABLISHED\x00\n",
		"ambiguous":      valid + valid,
	} {
		t.Run(name, func(t *testing.T) {
			verifier := newLsofSocketOwnerVerifier(CommandRunnerFunc(func(context.Context, string, ...string) (CommandResult, error) {
				return CommandResult{Stdout: output}, nil
			}))
			if _, err := verifier.VerifyOwner(context.Background(), "http://127.0.0.1:7788", 4321); !errors.Is(err, ErrSocketOwner) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	responses := []string{valid, "p4321\x00f18\x00tIPv4\x00PTCP\x00D0x12\x00i9999\x00n127.0.0.1:7788\x00TST=LISTEN\x00\n"}
	verifier := newLsofSocketOwnerVerifier(CommandRunnerFunc(func(context.Context, string, ...string) (CommandResult, error) {
		result := responses[0]
		responses = responses[1:]
		return CommandResult{Stdout: result}, nil
	}))
	if _, err := verifier.VerifyOwner(context.Background(), "http://127.0.0.1:7788", 4321); !errors.Is(err, ErrSocketOwner) {
		t.Fatalf("fd/node reuse error = %v", err)
	}
}
