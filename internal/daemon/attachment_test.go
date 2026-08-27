package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAttachmentLifecycleRequiresExactEvidenceAndRetainsPreferences(t *testing.T) {
	ctx := context.Background()
	adapter := &attachmentTestAdapter{}
	registry := newAttachmentTestRegistry(t, 7, adapter)

	prepared, capability, err := registry.Prepare(ctx, AttachmentPrepareRequest{
		Product: "qwen", Kind: "interactive", ProfileIdentity: map[string]any{"home": "/profiles/qwen"},
		Cwd: "/work", Name: "reviewer", Groups: []string{"team", "private:parent"},
		PermissionMode: "yolo", ExpectedNativeActor: map[string]any{"pid": 101, "proc_start": "start-101"},
	})
	if err != nil {
		t.Fatalf("prepare attachment: %v", err)
	}
	if prepared.State != AttachmentStatePrepared || prepared.SessionID != "" || capability == "" {
		t.Fatalf("prepared attachment = %#v, capability present=%t", prepared, capability != "")
	}
	if prepared.LaunchCapabilityHash == capability {
		t.Fatal("durable attachment stored the raw launch capability")
	}

	if _, err := registry.Adopt(ctx, AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: "wrong", SessionID: "native-qwen-1",
		NativeActor: map[string]any{"pid": 101, "proc_start": "start-101"},
	}); !errors.Is(err, ErrAttachmentNotAttested) {
		t.Fatalf("adopt with wrong capability error = %v, want ErrAttachmentNotAttested", err)
	}
	if _, err := registry.Adopt(ctx, AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: "native-qwen-1",
		NativeActor: map[string]any{"pid": 202, "proc_start": "changed"},
	}); !errors.Is(err, ErrAttachmentEvidenceChanged) {
		t.Fatalf("adopt with changed actor error = %v, want ErrAttachmentEvidenceChanged", err)
	}

	attached, err := registry.Adopt(ctx, AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: "native-qwen-1",
		NativeActor: map[string]any{"pid": 101, "proc_start": "start-101"},
	})
	if err != nil {
		t.Fatalf("adopt attachment: %v", err)
	}
	if attached.State != AttachmentStateAttached || attached.SessionID != "native-qwen-1" || attached.Revision <= prepared.Revision {
		t.Fatalf("attached record = %#v", attached)
	}

	refreshed, err := registry.Refresh(ctx, AttachmentRefreshRequest{
		AttachmentID: prepared.AttachmentID, SessionID: "native-qwen-1",
		NativeActor: map[string]any{"pid": 101, "proc_start": "start-101", "event_cursor": "9"},
	})
	if err != nil {
		t.Fatalf("refresh attachment: %v", err)
	}
	if refreshed.SessionID != attached.SessionID || refreshed.Revision <= attached.Revision {
		t.Fatalf("refreshed record = %#v", refreshed)
	}
	if _, err := registry.Refresh(ctx, AttachmentRefreshRequest{
		AttachmentID: prepared.AttachmentID, SessionID: "other-native-id",
		NativeActor: map[string]any{"pid": 101, "proc_start": "start-101"},
	}); !errors.Is(err, ErrAttachmentIdentityChanged) {
		t.Fatalf("refresh changed identity error = %v, want ErrAttachmentIdentityChanged", err)
	}

	detached, err := registry.Detach(ctx, prepared.AttachmentID, "native_exit")
	if err != nil {
		t.Fatalf("detach attachment: %v", err)
	}
	if detached.State != AttachmentStateDetached || detached.SessionID != "native-qwen-1" {
		t.Fatalf("detached record = %#v", detached)
	}
	prefs, err := registry.SessionPreferences(ctx, "qwen", "native-qwen-1")
	if err != nil {
		t.Fatalf("load retained preferences: %v", err)
	}
	if !reflect.DeepEqual(prefs.Groups, []string{"private:parent", "team"}) || prefs.PermissionMode != "yolo" {
		t.Fatalf("retained preferences = %#v", prefs)
	}
}

func TestAttachmentLateSelectionConnectorReconnectAndBareSessionRefusal(t *testing.T) {
	ctx := context.Background()
	adapter := &attachmentTestAdapter{}
	registry := newAttachmentTestRegistry(t, 3, adapter)
	prepared, capability, err := registry.Prepare(ctx, AttachmentPrepareRequest{
		Product: "claude", Kind: "interactive", Cwd: "/work", Name: "claude-peer",
		ExpectedNativeActor: map[string]any{"pid": 303, "proc_start": "start-303"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := registry.AttestConnector(ctx, ConnectorAttestation{
		Product: "claude", AttachmentID: prepared.AttachmentID, Capability: capability,
		NativeActor: map[string]any{"pid": 303, "proc_start": "start-303"},
	}); !errors.Is(err, ErrAttachmentSelecting) {
		t.Fatalf("connector before selection error = %v, want ErrAttachmentSelecting", err)
	}
	if _, err := registry.AttestConnector(ctx, ConnectorAttestation{
		Product: "claude", SessionID: "model-supplied-only",
		NativeActor: map[string]any{"pid": 303, "proc_start": "start-303"},
	}); !errors.Is(err, ErrAttachmentNotAttested) {
		t.Fatalf("bare connector error = %v, want ErrAttachmentNotAttested", err)
	}

	if _, err := registry.Adopt(ctx, AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: "claude-session-1",
		NativeActor: map[string]any{"pid": 303, "proc_start": "start-303"},
	}); err != nil {
		t.Fatal(err)
	}
	first, err := registry.AttestConnector(ctx, ConnectorAttestation{
		Product: "claude", AttachmentID: prepared.AttachmentID, Capability: capability,
		NativeActor: map[string]any{"pid": 303, "proc_start": "start-303"},
	})
	if err != nil {
		t.Fatalf("attest first connector: %v", err)
	}
	second, err := registry.AttestConnector(ctx, ConnectorAttestation{
		Product: "claude", AttachmentID: prepared.AttachmentID, Capability: capability,
		NativeActor: map[string]any{"pid": 303, "proc_start": "start-303"},
	})
	if err != nil {
		t.Fatalf("attest replacement connector: %v", err)
	}
	if first.AttachmentID != second.AttachmentID || first.SessionID != second.SessionID || second.Revision <= first.Revision {
		t.Fatalf("connector reconnect changed identity: first=%#v second=%#v", first, second)
	}
}

func TestAttachmentLateSelectionAdoptsOnlyObservedConnectorAncestry(t *testing.T) {
	ctx := context.Background()
	adapter := &observedAttachmentTestAdapter{attachmentTestAdapter: attachmentTestAdapter{}}
	registry := newAttachmentTestRegistry(t, 4, adapter)
	prepared, capability, err := registry.Prepare(ctx, AttachmentPrepareRequest{
		Product: "claude", Kind: "interactive", Cwd: "/work", Name: "late-selection",
		ExpectedNativeActor: map[string]any{"pid": 707, "proc_start": "native-start"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.AdoptObservedConnector(ctx, "claude", prepared.AttachmentID, "wrong", ConnectorProcessEvidence{PID: 808}); !errors.Is(err, ErrAttachmentNotAttested) {
		t.Fatalf("wrong observed capability error = %v", err)
	}
	attached, err := registry.AdoptObservedConnector(ctx, "claude", prepared.AttachmentID, capability, ConnectorProcessEvidence{
		PID: 808, ProcStart: "connector-start", StrongStart: "connector-strong",
	})
	if err != nil {
		t.Fatalf("adopt observed connector: %v", err)
	}
	if attached.State != AttachmentStateAttached || attached.SessionID != "observed-session" || adapter.observedPID != 808 {
		t.Fatalf("observed attachment = %#v, pid=%d", attached, adapter.observedPID)
	}
}

func TestAttachmentSelectionRejectsAmbiguousNamesAndAcceptsExactAddress(t *testing.T) {
	ctx := context.Background()
	registry := newAttachmentTestRegistry(t, 1, &attachmentTestAdapter{})
	for index, product := range []string{"codex", "grok"} {
		prepared, capability, err := registry.Prepare(ctx, AttachmentPrepareRequest{
			Product: product, Kind: "interactive", Cwd: "/work", Name: "duplicate",
			ExpectedNativeActor: map[string]any{"pid": 400 + index, "proc_start": "stable"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Adopt(ctx, AttachmentAdoptRequest{
			AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: product + "-session",
			NativeActor: map[string]any{"pid": 400 + index, "proc_start": "stable"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := registry.Select(ctx, AttachmentSelector{Name: "duplicate"}); !errors.Is(err, ErrAttachmentAmbiguous) {
		t.Fatalf("ambiguous name error = %v, want ErrAttachmentAmbiguous", err)
	}
	exact, err := registry.Select(ctx, AttachmentSelector{HostID: "host-test", SessionID: "grok-session"})
	if err != nil {
		t.Fatalf("select exact address: %v", err)
	}
	if exact.Product != "grok" {
		t.Fatalf("exact selection = %#v, want grok", exact)
	}
}

func TestAttachmentRestartPreservesNativeIdentityAcrossDaemonGeneration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := OpenStateStore(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &attachmentTestAdapter{}
	first, err := NewAttachmentRegistry(AttachmentRegistryOptions{
		State: store, Generation: 11, HostID: "host-test", Now: attachmentTestClock(),
		Capability: func() (string, error) { return "restart-capability", nil },
		Adapters:   map[string]AttachmentAdapter{"codex": adapter},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, capability, err := first.Prepare(ctx, AttachmentPrepareRequest{
		Product: "codex", Kind: "interactive", Cwd: "/work", Name: "long-lived",
		ExpectedNativeActor: map[string]any{"pid": 501, "proc_start": "start-501", "thread_id": "thread-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := first.Adopt(ctx, AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: "thread-1",
		NativeActor: map[string]any{"pid": 501, "proc_start": "start-501", "thread_id": "thread-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	second, err := NewAttachmentRegistry(AttachmentRegistryOptions{
		State: store, Generation: 12, HostID: "host-test", Now: attachmentTestClock(),
		Capability: func() (string, error) { return "unused", nil },
		Adapters:   map[string]AttachmentAdapter{"codex": adapter},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile attachment: %v", err)
	}
	recovered, err := second.Select(ctx, AttachmentSelector{HostID: "host-test", SessionID: "thread-1"})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.AttachmentID != attached.AttachmentID || recovered.SessionID != attached.SessionID ||
		recovered.Generation != 12 || recovered.Revision <= attached.Revision {
		t.Fatalf("recovered attachment = %#v, prior = %#v", recovered, attached)
	}
	if !reflect.DeepEqual(recovered.NativeActor, attached.NativeActor) {
		t.Fatalf("native identity changed across generation: got %#v want %#v", recovered.NativeActor, attached.NativeActor)
	}
}

type attachmentTestAdapter struct {
	reconnectErr error
}

type observedAttachmentTestAdapter struct {
	attachmentTestAdapter
	observedPID int
}

func (adapter *observedAttachmentTestAdapter) ObserveConnector(
	_ context.Context,
	_ AttachmentRecord,
	evidence ConnectorProcessEvidence,
) (string, map[string]any, error) {
	adapter.observedPID = evidence.PID
	return "observed-session", map[string]any{"pid": float64(707), "proc_start": "native-start"}, nil
}

func (adapter *attachmentTestAdapter) PrepareInteractive(_ context.Context, request AttachmentPrepareRequest) (NativeLaunchPlan, error) {
	return NativeLaunchPlan{
		Executable: request.Product, Arguments: append([]string(nil), request.Intent.NativeArguments...), Cwd: request.Cwd,
		ExpectedNativeActor: cloneAttachmentEvidence(request.ExpectedNativeActor),
	}, nil
}

func (adapter *attachmentTestAdapter) Corroborate(_ context.Context, record AttachmentRecord, evidence map[string]any) (map[string]any, error) {
	expectedPID := record.NativeActor["pid"]
	if expectedPID != nil && !reflect.DeepEqual(expectedPID, evidence["pid"]) {
		return nil, ErrAttachmentEvidenceChanged
	}
	expectedStart := record.NativeActor["proc_start"]
	if expectedStart != nil && !reflect.DeepEqual(expectedStart, evidence["proc_start"]) {
		return nil, ErrAttachmentEvidenceChanged
	}
	return cloneAttachmentEvidence(evidence), nil
}

func (adapter *attachmentTestAdapter) Reconnect(_ context.Context, record AttachmentRecord) (map[string]any, error) {
	if adapter.reconnectErr != nil {
		return nil, adapter.reconnectErr
	}
	return cloneAttachmentEvidence(record.NativeActor), nil
}

func newAttachmentTestRegistry(t *testing.T, generation uint64, adapter AttachmentAdapter) *AttachmentRegistry {
	t.Helper()
	store, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewAttachmentRegistry(AttachmentRegistryOptions{
		State: store, Generation: generation, HostID: "host-test", Now: attachmentTestClock(),
		Capability: func() (string, error) { return "test-capability", nil },
		Adapters: map[string]AttachmentAdapter{
			"codex": adapter, "claude": adapter, "grok": adapter, "qwen": adapter,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func attachmentTestClock() func() time.Time {
	now := time.Unix(1787820000, 0)
	return func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}
}
