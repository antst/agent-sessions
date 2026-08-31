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
	snapshot, err := first.State().Read()
	if err != nil || snapshot.Catalog.Host.Generation != first.Generation() {
		t.Fatalf("durable authority = %+v, %v", snapshot.Catalog.Host, err)
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
	snapshot, err := runtime.State().Read()
	if err != nil || snapshot.Catalog.Host.ServiceState != "stopped" {
		t.Fatalf("stopped state = %+v, %v", snapshot.Catalog.Host, err)
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

func TestRuntimeLifecycleRecoversOneSuccessorAfterCrash(t *testing.T) {
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
	before, err := first.State().Read()
	if err != nil || before.Catalog.Host.ServiceState != "running" {
		t.Fatalf("unclean state = %+v, %v", before.Catalog.Host, err)
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
	if _, err := successor.Attachments().Refresh(context.Background(), "survivor"); err != nil {
		t.Fatal(err)
	}
	if active, err := successor.Attachments().ListActive(); err != nil || len(active) != 1 || active[0].ID != "survivor" {
		t.Fatalf("refreshed successor attachment = %+v, %v", active, err)
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
	snapshot, err := runtime.State().Read()
	if err != nil || snapshot.Catalog.Host.ServiceState != "failed" {
		t.Fatalf("failed service state = %+v, %v", snapshot.Catalog.Host, err)
	}
}

func TestRuntimeDoesNotAdvertiseReadyBeforeInitializationCompletes(t *testing.T) {
	root := shortDaemonTestRoot(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan struct {
		runtime *Runtime
		err     error
	}, 1)
	go func() {
		runtime, err := StartRuntime(context.Background(), RuntimeConfig{
			StateRoot: root,
			Initialize: func(*Runtime) error {
				close(entered)
				<-release
				return nil
			},
		})
		result <- struct {
			runtime *Runtime
			err     error
		}{runtime: runtime, err: err}
	}()
	<-entered
	endpoint, err := ControlEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	response, err := CallControl(context.Background(), endpoint, ControlRequest{
		ID: "before-ready", Role: RoleAdmin, Operation: "doctor", Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != ErrorHandler {
		t.Fatalf("control response before initialization = %+v", response)
	}
	close(release)
	started := <-result
	if started.err != nil {
		t.Fatal(started.err)
	}
	t.Cleanup(func() { _ = started.runtime.Close() })
	response, err = CallControl(context.Background(), endpoint, ControlRequest{
		ID: "after-ready", Role: RoleAdmin, Operation: "doctor", Generation: started.runtime.Generation(),
	})
	if err != nil || !response.OK {
		t.Fatalf("control response after initialization = %+v, %v", response, err)
	}
}

func TestRuntimeComposesAttachmentAndExactRelayAuthorization(t *testing.T) {
	root := shortDaemonTestRoot(t)
	var externalCalls atomic.Int32
	adapter := AttachmentAdapter{
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{ThreadID: "native-thread", SocketPath: "/profile/native.sock"}, nil
		},
		Adopt: func(_ context.Context, _ ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
			if evidence.ThreadID != "native-thread" {
				return NativeEvidence{}, errors.New("wrong native thread")
			}
			return evidence, nil
		},
		Refresh: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
			return attachment.Evidence, nil
		},
		Detach: func(context.Context, ManagedAttachment) error { return nil },
		Rollback: func(context.Context, ManagedAttachment) error {
			return nil
		},
	}
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: root, Adapters: map[string]AttachmentAdapter{"codex": adapter},
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
	capability := "runtime-capability"
	preparePayload, _ := json.Marshal(map[string]any{"attachment": ManagedAttachment{
		ID: "attachment", CapabilityHash: CapabilityDigest(capability), Product: "codex", ProfileIdentity: "profile",
	}})
	if response := call(ControlRequest{ID: "prepare", Role: RoleLauncher, Operation: "attachment.prepare", IdempotencyKey: "prepare", Payload: preparePayload}); !response.OK {
		t.Fatalf("prepare response = %+v", response)
	}
	adoptPayload, _ := json.Marshal(map[string]any{"id": "attachment", "evidence": NativeEvidence{ThreadID: "native-thread", SocketPath: "/profile/native.sock"}})
	if response := call(ControlRequest{ID: "adopt", Role: RoleLauncher, Operation: "attachment.adopt", IdempotencyKey: "adopt", Payload: adoptPayload}); !response.OK {
		t.Fatalf("adopt response = %+v", response)
	}
	if response := call(ControlRequest{ID: "forged", Role: RoleConnector, Operation: "connector.call", IdempotencyKey: "forged", AttachmentID: "attachment", Capability: "wrong", Payload: json.RawMessage(`{}`)}); response.OK || response.Error == nil || response.Error.Code != ErrorInactive || response.Error.Message != CanonicalInactiveMessage {
		t.Fatalf("forged connector response = %+v", response)
	}
	if response := call(ControlRequest{ID: "exact", Role: RoleConnector, Operation: "connector.call", IdempotencyKey: "exact", AttachmentID: "attachment", Capability: capability, Payload: json.RawMessage(`{}`)}); !response.OK {
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

func TestRuntimeAuthorizesRunningLaneConnectorByOneWayCapabilityOnly(t *testing.T) {
	var calls atomic.Int32
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: shortDaemonTestRoot(t),
		Handler: func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"accepted":true}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	capability := "lane-capability"
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = Lane{
		ID: "lane", ParentAttachmentID: "parent", Product: "grok", State: "running",
		CapabilityHash: CapabilityDigest(capability),
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"product":"grok"}`)
	call := func(id, capabilityValue string) ControlResponse {
		response, callErr := CallControl(context.Background(), runtime.Endpoint(), ControlRequest{
			ID: id, Role: RoleConnector, Operation: "connector.call", Generation: runtime.Generation(),
			IdempotencyKey: id, AttachmentID: "lane", Capability: capabilityValue, Payload: payload,
		})
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}
	if response := call("exact-lane", capability); !response.OK {
		t.Fatalf("exact lane connector response = %+v", response)
	}
	if response := call("forged-lane", "wrong"); response.OK || response.Error == nil || response.Error.Code != ErrorInactive {
		t.Fatalf("forged lane connector response = %+v", response)
	}
	if calls.Load() != 1 {
		t.Fatalf("authorized handler calls = %d", calls.Load())
	}
}

func TestRuntimeAuthorizesCapabilitylessLaneOnlyThroughNativeAuthorizer(t *testing.T) {
	var calls atomic.Int32
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: shortDaemonTestRoot(t),
		LaneConnectorAuthorizer: func(_ context.Context, lane Lane, evidence NativeEvidence) error {
			if lane.ID != "codex-lane" || lane.Product != "codex" || evidence.ThreadID != "native-thread" {
				return errors.New("unexpected native lane evidence")
			}
			return nil
		},
		Handler: func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"accepted":true}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Lanes["codex-lane"] = Lane{
		ID: "codex-lane", ParentAttachmentID: "parent", Product: "codex", State: "running",
		NativeSessionID: "native-thread", CapabilityHash: CapabilityDigest("unavailable-to-app-server"),
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	call := func(id, thread string) ControlResponse {
		payload, _ := json.Marshal(map[string]any{"product": "codex", "evidence": NativeEvidence{ThreadID: thread}})
		response, callErr := CallControl(context.Background(), runtime.Endpoint(), ControlRequest{
			ID: id, Role: RoleConnector, Operation: "connector.call", Generation: runtime.Generation(),
			IdempotencyKey: id, AttachmentID: "codex-lane", Payload: payload,
		})
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}
	if response := call("exact-native", "native-thread"); !response.OK {
		t.Fatalf("exact native lane response = %+v", response)
	}
	if response := call("wrong-native", "other-thread"); response.OK || response.Error == nil || response.Error.Code != ErrorInactive {
		t.Fatalf("wrong native lane response = %+v", response)
	}
	if calls.Load() != 1 {
		t.Fatalf("authorized handler calls = %d", calls.Load())
	}
}
