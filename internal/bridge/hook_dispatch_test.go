package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestHookDispatcherForwardsOnlyDeclaredExactManagedEvents(t *testing.T) {
	root := shortSocketTestRoot(t, "hd-")
	var got []hookDispatchPayload
	server, err := daemonpkg.StartControlServer(context.Background(), root, 5, func(_ context.Context, request daemonpkg.ControlRequest) (json.RawMessage, error) {
		if request.AttachmentID != "codex-thread" || request.Capability != "codex-capability" {
			t.Fatalf("attestation = %q / %q", request.AttachmentID, request.Capability)
		}
		var payload hookDispatchPayload
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, payload)
		return json.Marshal(map[string]any{"event": payload.Event})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	dispatcher, err := NewHookDispatcher(HookDispatchConfig{
		Product: "codex", Endpoint: server.Endpoint(),
		Events:     map[string]bool{"SessionStart": true, "UserPromptSubmit": true, "Stop": true, "SessionEnd": true},
		Generation: func(context.Context) (uint64, error) { return 5, nil },
		Attest: func(_ context.Context, event string, input json.RawMessage) (HookAttestation, error) {
			var values map[string]string
			if err := json.Unmarshal(input, &values); err != nil || values["session_id"] != "model-family" {
				t.Fatalf("%s input = %s, %v", event, input, err)
			}
			return HookAttestation{
				AttachmentID: "codex-thread", Capability: "codex-capability",
				Evidence: daemonpkg.NativeEvidence{ThreadID: "codex-thread", SocketPath: "/profile/app-server.sock"},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		output, err := dispatcher.Dispatch(context.Background(), event, json.RawMessage(`{"session_id":"model-family"}`))
		if err != nil || !reflect.DeepEqual(output, json.RawMessage(`{"event":"`+event+`"}`)) {
			t.Fatalf("dispatch %s = %s, %v", event, output, err)
		}
	}
	if output, err := dispatcher.Dispatch(context.Background(), "ImaginaryProductHook", json.RawMessage(`{"session_id":"model-family"}`)); err != nil || output != nil {
		t.Fatalf("unknown event = %s, %v", output, err)
	}
	if len(got) != 4 {
		t.Fatalf("daemon events = %+v", got)
	}
	for _, payload := range got {
		if payload.Product != "codex" || payload.Evidence.ThreadID != "codex-thread" {
			t.Fatalf("hook evidence = %+v", payload)
		}
	}
}

func TestHookDispatcherIsSilentForBareSessionsAndProductsWithoutHooks(t *testing.T) {
	root := shortSocketTestRoot(t, "hd-")
	calls := 0
	server, err := daemonpkg.StartControlServer(context.Background(), root, 2, func(context.Context, daemonpkg.ControlRequest) (json.RawMessage, error) {
		calls++
		return nil, errors.New("unexpected hook mutation")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	bare, err := NewHookDispatcher(HookDispatchConfig{
		Product: "codex", Endpoint: server.Endpoint(), Events: map[string]bool{"SessionStart": true},
		Generation: func(context.Context) (uint64, error) { return 2, nil },
		Attest: func(context.Context, string, json.RawMessage) (HookAttestation, error) {
			return HookAttestation{}, ErrConnectorInactive
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := bare.Dispatch(context.Background(), "SessionStart", json.RawMessage(`{"session_id":"ordinary"}`)); err != nil || output != nil {
		t.Fatalf("bare hook = %s, %v", output, err)
	}
	claude, err := NewHookDispatcher(HookDispatchConfig{
		Product: "claude", Endpoint: server.Endpoint(), Events: map[string]bool{},
		Generation: func(context.Context) (uint64, error) { return 2, nil },
		Attest: func(context.Context, string, json.RawMessage) (HookAttestation, error) {
			t.Fatal("Claude no-hook surface attempted attestation")
			return HookAttestation{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := claude.Dispatch(context.Background(), "SessionStart", nil); err != nil || output != nil || calls != 0 {
		t.Fatalf("Claude no-hook dispatch = %s, %v; daemon calls=%d", output, err, calls)
	}
}
