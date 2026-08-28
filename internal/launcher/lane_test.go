package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
)

func TestLaneHelpDoesNotContactDaemon(t *testing.T) {
	previousQuery, previousInput, previousOutput := queryLaneDaemon, laneInput, laneOutput
	t.Cleanup(func() {
		queryLaneDaemon, laneInput, laneOutput = previousQuery, previousInput, previousOutput
	})
	queryLaneDaemon = func(context.Context, daemon.LocalControlIdentity, string, any) (daemon.LocalControlResult, error) {
		t.Fatal("lane help contacted the daemon")
		return daemon.LocalControlResult{}, nil
	}

	for _, role := range []string{"lane", "claude-lane", "grok-lane", "qwen-lane"} {
		if err := RunLane(role, []string{"--help"}); err != nil {
			t.Fatalf("%s help: %v", role, err)
		}
	}
}

func TestLaneCommandRoutesCanonicalArgumentsAndAttestedParentThroughDaemon(t *testing.T) {
	previousQuery, previousInput, previousOutput := queryLaneDaemon, laneInput, laneOutput
	t.Cleanup(func() {
		queryLaneDaemon, laneInput, laneOutput = previousQuery, previousInput, previousOutput
	})
	t.Setenv(daemon.InternalProductEnvironment, "codex")
	t.Setenv(daemon.InternalAttachmentIDEnvironment, "parent-attachment")
	t.Setenv(daemon.InternalSessionIDEnvironment, "parent-session")
	t.Setenv(daemon.InternalCapabilityEnvironment, "parent-capability")
	laneInput = strings.NewReader("review this change")
	output := &bytes.Buffer{}
	laneOutput = output

	wantArguments := []string{"--name", "reviewer", "--group", "project", "--inherit-groups", "--timeout", "2.5"}
	queryLaneDaemon = func(_ context.Context, identity daemon.LocalControlIdentity, operation string, payload any) (daemon.LocalControlResult, error) {
		if operation != "lane.command" {
			t.Fatalf("operation = %q", operation)
		}
		if identity.Role != daemon.LocalControlLauncher || identity.Product != "codex" ||
			identity.AttachmentID != "parent-attachment" || identity.SessionID != "parent-session" ||
			identity.Capability != "parent-capability" {
			t.Fatalf("launcher identity = %#v", identity)
		}
		request, ok := payload.(daemon.LaneCommandRequest)
		if !ok {
			t.Fatalf("payload type = %T", payload)
		}
		if request.Product != "claude" || request.Command != "start" || request.Host != "macbook" || request.Input != "review this change" ||
			!reflect.DeepEqual(request.Arguments, wantArguments) {
			t.Fatalf("lane request = %#v", request)
		}
		return daemon.LocalControlResult{Result: json.RawMessage(`{"type":"lane.started","lane":{"lane_id":"lane-1"}}`)}, nil
	}

	arguments := append([]string{"start", "--host", "macbook"}, wantArguments...)
	if err := RunLane("claude-lane", arguments); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "{\"type\":\"lane.started\",\"lane\":{\"lane_id\":\"lane-1\"}}\n" {
		t.Fatalf("lane output = %q", got)
	}
}

func TestLaneHostOptionIsCanonicalAndNeverForwardedAsProductArgument(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		wantError string
	}{
		{name: "equals", arguments: []string{"--host=macbook", "--name", "worker"}},
		{name: "separate", arguments: []string{"--host", "macbook", "--name", "worker"}},
		{name: "duplicate", arguments: []string{"--host", "macbook", "--host=linux"}, wantError: "only once"},
		{name: "missing", arguments: []string{"--host"}, wantError: "requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, remaining, err := extractLaneHost(test.arguments)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("extract error = %v", err)
				}
				return
			}
			if err != nil || host != "macbook" || !reflect.DeepEqual(remaining, []string{"--name", "worker"}) {
				t.Fatalf("extract = host %q remaining %#v error %v", host, remaining, err)
			}
		})
	}
}

func TestRemoteLaneRejectsSourcePromptFileBeforeReadingOrDaemonDispatch(t *testing.T) {
	previousQuery, previousInput, previousOutput := queryLaneDaemon, laneInput, laneOutput
	t.Cleanup(func() {
		queryLaneDaemon, laneInput, laneOutput = previousQuery, previousInput, previousOutput
	})
	queryLaneDaemon = func(context.Context, daemon.LocalControlIdentity, string, any) (daemon.LocalControlResult, error) {
		t.Fatal("unsupported remote prompt file reached the daemon")
		return daemon.LocalControlResult{}, nil
	}
	laneInput = strings.NewReader("stdin must not be read after an invalid remote option")
	err := RunLane("qwen-lane", []string{
		"start", "--host", "macbook", "--name", "worker", "--prompt-file", "/source-only/prompt.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "not supported for remote lanes") ||
		!strings.Contains(err.Error(), "stdin") {
		t.Fatalf("remote prompt-file error = %v", err)
	}
}

func TestLaneUnavailableReturnsDaemonErrorWithoutOutput(t *testing.T) {
	previousQuery, previousInput, previousOutput := queryLaneDaemon, laneInput, laneOutput
	t.Cleanup(func() {
		queryLaneDaemon, laneInput, laneOutput = previousQuery, previousInput, previousOutput
	})
	want := errors.New("daemon unavailable")
	queryLaneDaemon = func(_ context.Context, identity daemon.LocalControlIdentity, operation string, payload any) (daemon.LocalControlResult, error) {
		if identity.Role != daemon.LocalControlLauncher || operation != "lane.command" {
			t.Fatalf("daemon query = %#v %q %#v", identity, operation, payload)
		}
		return daemon.LocalControlResult{}, want
	}
	output := &bytes.Buffer{}
	laneOutput = output

	if err := RunLane("qwen-lane", []string{"list", "--all"}); !errors.Is(err, want) {
		t.Fatalf("RunLane error = %v, want %v", err, want)
	}
	if output.Len() != 0 {
		t.Fatalf("unavailable daemon produced output %q", output.String())
	}
}
