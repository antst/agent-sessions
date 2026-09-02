package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeLifecycleRejectsSecondLiveAuthority(t *testing.T) {
	root := shortDaemonTestRoot(t)
	first, err := StartRuntime(context.Background(), RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := StartRuntime(context.Background(), RuntimeConfig{StateRoot: root}); err == nil || !strings.Contains(err.Error(), "existing authority") {
		t.Fatalf("duplicate runtime error = %v", err)
	}
	response, err := CallControl(context.Background(), first.Endpoint(), ControlRequest{
		ID: "status", Role: RoleAdmin, Operation: "status", Generation: first.Generation(),
	})
	if err != nil || !response.OK || !strings.Contains(string(response.Payload), `"service_state":"running"`) {
		t.Fatalf("first authority status = %+v, %v", response, err)
	}
}

func TestRuntimeLifecycleExplicitStopRemainsStoppedAndWorkflowCallsDoNotBootstrap(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := runtime.Endpoint()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control endpoint survived explicit stop: %v", err)
	}
	_, err = CallControl(context.Background(), endpoint, ControlRequest{ID: "workflow", Role: RoleLauncher, Operation: "lane.status", Generation: runtime.Generation()})
	if err == nil {
		t.Fatal("workflow command reached a stopped daemon")
	}
	if _, err := os.Lstat(endpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workflow command bootstrapped runtime endpoint: %v", err)
	}
}

func TestRuntimeLifecycleStartsSuccessorWithAnEmptyLiveRegistry(t *testing.T) {
	root := shortDaemonTestRoot(t)
	adapter := AttachmentAdapter{
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{ThreadID: "recovered-thread"}, nil
		},
		Adopt: func(_ context.Context, _ ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
			return evidence, nil
		},
		Refresh: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
			return attachment.Evidence, nil
		},
		Detach:   func(context.Context, ManagedAttachment) error { return nil },
		Rollback: func(context.Context, ManagedAttachment) error { return nil },
	}
	config := RuntimeConfig{StateRoot: root, Adapters: map[string]AttachmentAdapter{"codex": adapter}}
	first, err := StartRuntime(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Attachments().Prepare(context.Background(), ManagedAttachment{ID: "survivor", CapabilityHash: CapabilityDigest("cap"), Product: "codex", ProfileIdentity: "profile"}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Attachments().Adopt(context.Background(), "survivor", NativeEvidence{ThreadID: "recovered-thread"}); err != nil {
		t.Fatal(err)
	}
	firstGeneration := first.Generation()
	if err := first.crashForTest(errors.New("simulated crash")); err != nil {
		t.Fatal(err)
	}
	successor, err := StartRuntime(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	if successor.Generation() != firstGeneration+1 {
		t.Fatalf("successor generation = %d, want %d", successor.Generation(), firstGeneration+1)
	}
	if active, err := successor.Attachments().ListActive(); err != nil || len(active) != 0 {
		t.Fatalf("unrefreshed predecessor attachment remained active: %+v, %v", active, err)
	}
	response, err := CallControl(context.Background(), successor.Endpoint(), ControlRequest{
		ID: "doctor", Role: RoleAdmin, Operation: "doctor", Generation: successor.Generation(),
	})
	if err != nil || !response.OK {
		t.Fatalf("successor doctor = %+v, %v", response, err)
	}
}

func TestRuntimeComponentFailureCancelsWholeAuthority(t *testing.T) {
	root := shortDaemonTestRoot(t)
	sentinel := errors.New("component failed")
	siblingCancelled := make(chan struct{})
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: root,
		Components: []RuntimeComponent{
			func(ctx context.Context) error {
				<-ctx.Done()
				close(siblingCancelled)
				return nil
			},
			func(context.Context) error { return sentinel },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Wait(); !errors.Is(err, sentinel) {
		t.Fatalf("runtime wait error = %v", err)
	}
	select {
	case <-siblingCancelled:
	case <-time.After(time.Second):
		t.Fatal("sibling component was not cancelled")
	}
	if _, err := os.Lstat(runtime.Endpoint()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed runtime endpoint survived: %v", err)
	}
}

func TestRuntimeComposesLiveConnectorRelay(t *testing.T) {
	root := shortDaemonTestRoot(t)
	var externalCalls atomic.Int32
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: root,
		Handler: func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
			externalCalls.Add(1)
			return json.RawMessage(`{"accepted":true}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	call := func(request ControlRequest) ControlResponse {
		t.Helper()
		request.Generation = runtime.Generation()
		response, err := CallControl(context.Background(), runtime.Endpoint(), request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	runtime.Attachments().ReportLive("attachment", "worker", "codex", []string{"team"}, false)
	if response := call(ControlRequest{ID: "wrong-product", Role: RoleConnector, Operation: "connector.call", IdempotencyKey: "wrong-product", AttachmentID: "attachment", Payload: json.RawMessage(`{"product":"claude"}`)}); response.OK || response.Error == nil || response.Error.Code != ErrorInactive {
		t.Fatalf("wrong product connector response = %+v", response)
	}
	if response := call(ControlRequest{ID: "exact", Role: RoleConnector, Operation: "connector.call", IdempotencyKey: "exact", AttachmentID: "attachment", Payload: json.RawMessage(`{"product":"codex"}`)}); !response.OK {
		t.Fatalf("exact connector response = %+v", response)
	}
	if externalCalls.Load() != 1 {
		t.Fatalf("external handler calls = %d", externalCalls.Load())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "run", "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime socket survived close: %v", err)
	}
}
