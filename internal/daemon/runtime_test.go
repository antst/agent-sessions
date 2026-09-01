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
	if response.OK || response.Error == nil {
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
		ParentConnectorAuthorizer: func(_ context.Context, attachment ManagedAttachment, evidence NativeEvidence) error {
			if attachment.ID != "attachment" || attachment.Product != "codex" || evidence.ThreadID != "native-thread" {
				return errors.New("wrong parent connector evidence")
			}
			return nil
		},
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
	if response := call(ControlRequest{ID: "exact", Role: RoleConnector, Operation: "connector.call", IdempotencyKey: "exact", AttachmentID: "attachment", Capability: capability, Payload: json.RawMessage(`{"product":"codex","evidence":{"thread_id":"native-thread"}}`)}); !response.OK {
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

func TestRuntimeParentConnectorFailsClosedWithoutDedicatedAuthorization(t *testing.T) {
	for _, test := range []struct {
		name       string
		authorizer ParentConnectorAuthorizer
	}{
		{name: "nil"},
		{name: "denied", authorizer: func(context.Context, ManagedAttachment, NativeEvidence) error {
			return errors.New("native parent denied")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var genericCalls, handlerCalls atomic.Int32
			adapter := AttachmentAdapter{
				Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
					return NativeEvidence{ThreadID: "native-thread"}, nil
				},
				Adopt: func(_ context.Context, _ ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
					return evidence, nil
				},
				Authorize: func(context.Context, ManagedAttachment, NativeEvidence) error {
					genericCalls.Add(1)
					return nil
				},
				Detach:   func(context.Context, ManagedAttachment) error { return nil },
				Rollback: func(context.Context, ManagedAttachment) error { return nil },
			}
			runtime, err := StartRuntime(context.Background(), RuntimeConfig{
				StateRoot: shortDaemonTestRoot(t), Adapters: map[string]AttachmentAdapter{"codex": adapter},
				ParentConnectorAuthorizer: test.authorizer,
				Handler: func(context.Context, ControlRequest) (json.RawMessage, error) {
					handlerCalls.Add(1)
					return json.RawMessage(`{"accepted":true}`), nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			prepareRuntimeAttachment(t, runtime, "parent", "codex", "parent-cap", NativeEvidence{ThreadID: "native-thread"})

			response, err := CallControl(context.Background(), runtime.Endpoint(), ControlRequest{
				ID: "parent", Role: RoleConnector, Operation: "connector.call", Generation: runtime.Generation(),
				IdempotencyKey: "parent", AttachmentID: "parent", Capability: "parent-cap",
				Payload: json.RawMessage(`{"product":"codex","evidence":{"thread_id":"native-thread"}}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.OK || response.Error == nil || response.Error.Code != ErrorInactive {
				t.Fatalf("parent connector response = %+v", response)
			}
			if genericCalls.Load() != 0 || handlerCalls.Load() != 0 {
				t.Fatalf("generic/handler calls = %d/%d", genericCalls.Load(), handlerCalls.Load())
			}
		})
	}
}

func TestRuntimeParentConnectorRechecksAttachmentAfterCallback(t *testing.T) {
	var runtime *Runtime
	var handlerCalls atomic.Int32
	adapter := AttachmentAdapter{
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{ThreadID: "native-thread"}, nil
		},
		Adopt: func(_ context.Context, _ ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
			return evidence, nil
		},
		Detach:   func(context.Context, ManagedAttachment) error { return nil },
		Rollback: func(context.Context, ManagedAttachment) error { return nil },
	}
	var err error
	runtime, err = StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: shortDaemonTestRoot(t), Adapters: map[string]AttachmentAdapter{"codex": adapter},
		ParentConnectorAuthorizer: func(context.Context, ManagedAttachment, NativeEvidence) error {
			_, detachErr := runtime.Attachments().Detach(context.Background(), "parent", "attestation-race")
			return detachErr
		},
		Handler: func(context.Context, ControlRequest) (json.RawMessage, error) {
			handlerCalls.Add(1)
			return json.RawMessage(`{"accepted":true}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	prepareRuntimeAttachment(t, runtime, "parent", "codex", "parent-cap", NativeEvidence{ThreadID: "native-thread"})

	response, err := CallControl(context.Background(), runtime.Endpoint(), ControlRequest{
		ID: "parent-race", Role: RoleConnector, Operation: "connector.call", Generation: runtime.Generation(),
		IdempotencyKey: "parent-race", AttachmentID: "parent",
		Payload: json.RawMessage(`{"product":"codex","evidence":{"thread_id":"native-thread"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != ErrorInactive || handlerCalls.Load() != 0 {
		t.Fatalf("post-callback attachment recheck = %+v, handler calls = %d", response, handlerCalls.Load())
	}
}

func prepareRuntimeAttachment(
	t *testing.T,
	runtime *Runtime,
	id, product, capability string,
	evidence NativeEvidence,
) {
	t.Helper()
	if _, err := runtime.Attachments().Prepare(context.Background(), ManagedAttachment{
		ID: id, Product: product, ProfileIdentity: "profile-" + product,
		CapabilityHash: CapabilityDigest(capability),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Attachments().Adopt(context.Background(), id, evidence); err != nil {
		t.Fatal(err)
	}
}
