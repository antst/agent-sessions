package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestAttachmentRegistryIsLiveAndProcessLocal(t *testing.T) {
	var calls []string
	adapter := AttachmentAdapter{
		Prepare: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
			calls = append(calls, "prepare:"+attachment.State)
			return NativeEvidence{ThreadID: "native"}, nil
		},
		Adopt: func(_ context.Context, attachment ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
			calls = append(calls, "adopt:"+attachment.State)
			return evidence, nil
		},
		Detach: func(_ context.Context, attachment ManagedAttachment) error {
			calls = append(calls, "detach:"+attachment.State)
			return nil
		},
		Rollback: func(context.Context, ManagedAttachment) error { return nil },
	}
	engine, err := NewAttachmentEngine(7, map[string]AttachmentAdapter{"codex": adapter})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := engine.Prepare(context.Background(), ManagedAttachment{
		ID: "peer", CapabilityHash: "cap", Product: "codex", ProfileIdentity: "profile",
		NativeSessionID: "native", Groups: []string{"project"},
	})
	if err != nil || prepared.State != "prepared" {
		t.Fatalf("prepare = %+v, %v", prepared, err)
	}
	if active, _ := engine.ListActive(); len(active) != 0 {
		t.Fatalf("prepared attachment was listed: %+v", active)
	}
	adopted, err := engine.Adopt(context.Background(), "peer", NativeEvidence{ThreadID: "native"})
	if err != nil || adopted.State != "attached" {
		t.Fatalf("adopt = %+v, %v", adopted, err)
	}
	active, err := engine.ListActive()
	if err != nil || len(active) != 1 || active[0].ID != "peer" {
		t.Fatalf("active = %+v, %v", active, err)
	}
	active[0].Groups[0] = "mutated"
	again, _ := engine.ListActive()
	if again[0].Groups[0] != "project" {
		t.Fatalf("caller mutated live registry: %+v", again[0])
	}
	restarted, err := NewAttachmentEngine(8, map[string]AttachmentAdapter{"codex": adapter})
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := restarted.ListActive(); len(active) != 0 {
		t.Fatalf("new process restored live peers: %+v", active)
	}
	if title, ok, err := restarted.LiveNativeTitle("peer"); err != nil || ok || title != "" {
		t.Fatalf("new process restored title: %q, %v, %v", title, ok, err)
	}
	if _, err := engine.Detach(context.Background(), "peer", "exit"); err != nil {
		t.Fatal(err)
	}
	if active, _ := engine.ListActive(); len(active) != 0 {
		t.Fatalf("detached peer remained live: %+v", active)
	}
	if want := []string{"prepare:preparing", "adopt:selecting", "detach:detaching"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("callbacks = %v, want %v", calls, want)
	}
}

func TestAttachmentFailuresDoNotCreatePersistentRecoveryState(t *testing.T) {
	rolledBack := false
	engine, err := NewAttachmentEngine(1, map[string]AttachmentAdapter{"qwen": {
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{}, errors.New("product refused")
		},
		Rollback: func(context.Context, ManagedAttachment) error {
			rolledBack = true
			return nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Prepare(context.Background(), ManagedAttachment{
		ID: "peer", CapabilityHash: "cap", Product: "qwen", ProfileIdentity: "profile",
	})
	if err == nil || !errors.Is(err, ErrAttachmentPreparation) || !rolledBack {
		t.Fatalf("prepare error = %v, rollback=%v", err, rolledBack)
	}
	if active, _ := engine.ListActive(); len(active) != 0 {
		t.Fatalf("failed prepare remained live: %+v", active)
	}
}

func TestAttachmentSelectsProductResolvedNativeSession(t *testing.T) {
	engine, err := NewAttachmentEngine(1, map[string]AttachmentAdapter{"grok": {
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{ThreadID: "provisional"}, nil
		},
		Adopt: func(_ context.Context, attachment ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
			if attachment.NativeSessionID != "11111111-1111-4111-8111-111111111111" {
				t.Fatalf("selected native session = %q", attachment.NativeSessionID)
			}
			return evidence, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Prepare(context.Background(), ManagedAttachment{
		ID: "provisional", CapabilityHash: "cap", Product: "grok", ProfileIdentity: "profile",
	}); err != nil {
		t.Fatal(err)
	}
	selected := "11111111-1111-4111-8111-111111111111"
	if _, err := engine.SelectNative("provisional", selected, "/work", "default"); err != nil {
		t.Fatal(err)
	}
	adopted, err := engine.Adopt(context.Background(), "provisional", NativeEvidence{ThreadID: selected})
	if err != nil || adopted.NativeSessionID != selected || adopted.State != "attached" {
		t.Fatalf("adopted = %+v, %v", adopted, err)
	}
}
