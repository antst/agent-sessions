package daemon

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestAttachmentTransactionPreservesEveryProductEvidenceVariant(t *testing.T) {
	tests := []struct {
		product   string
		expected  NativeEvidence
		observed  NativeEvidence
		refreshed NativeEvidence
	}{
		{
			product:   "codex",
			expected:  NativeEvidence{Process: testAttachmentProcess(101), Executable: "/opt/codex", SocketPath: "/state/app-server.sock", ThreadID: "codex-thread"},
			observed:  NativeEvidence{Process: testAttachmentProcess(101), Executable: "/opt/codex", SocketPath: "/state/app-server.sock", ThreadID: "codex-thread", ArtifactRevision: "loaded-1"},
			refreshed: NativeEvidence{Process: testAttachmentProcess(101), Executable: "/opt/codex", SocketPath: "/state/app-server.sock", ThreadID: "codex-thread", ArtifactRevision: "loaded-2"},
		},
		{
			product:   "claude",
			expected:  NativeEvidence{Process: testAttachmentProcess(201), RegistryPath: "/profile/claude-peers/id.json", SocketPath: "/profile/peer.sock", ArtifactPath: "/profile/launch.key", ArtifactType: "regular", ArtifactMode: 0o600, ArtifactOwner: 1000},
			observed:  NativeEvidence{Process: testAttachmentProcess(201), RegistryPath: "/profile/claude-peers/native.json", SocketPath: "/profile/peer.sock", ArtifactPath: "/profile/launch.key", ArtifactType: "regular", ArtifactMode: 0o600, ArtifactOwner: 1000, ArtifactRevision: "native-row-1"},
			refreshed: NativeEvidence{Process: testAttachmentProcess(201), RegistryPath: "/profile/claude-peers/native.json", SocketPath: "/profile/peer.sock", ArtifactPath: "/profile/launch.key", ArtifactType: "regular", ArtifactMode: 0o600, ArtifactOwner: 1000, ArtifactRevision: "native-row-2"},
		},
		{
			product:   "grok",
			expected:  NativeEvidence{Process: testAttachmentProcess(301), Ancestry: []procinfo.Identity{testAttachmentProcess(300)}, Executable: "/opt/grok", LaunchTokenHash: "token-hash", LeaderIdentity: "private-leader"},
			observed:  NativeEvidence{Process: testAttachmentProcess(301), Ancestry: []procinfo.Identity{testAttachmentProcess(300)}, Executable: "/opt/grok", LaunchTokenHash: "token-hash", LeaderIdentity: "private-leader", RosterRevision: "roster-1"},
			refreshed: NativeEvidence{Process: testAttachmentProcess(301), Ancestry: []procinfo.Identity{testAttachmentProcess(300)}, Executable: "/opt/grok", LaunchTokenHash: "token-hash", LeaderIdentity: "private-leader", RosterRevision: "roster-2"},
		},
		{
			product:   "qwen",
			expected:  NativeEvidence{Process: testAttachmentProcess(401), Ancestry: []procinfo.Identity{testAttachmentProcess(400)}, Executable: "/opt/qwen", SocketPath: "/runtime/qwen.sock", ArtifactPath: "/runtime/events/session.json", ArtifactType: "regular", ArtifactMode: 0o600, ArtifactOwner: 1000},
			observed:  NativeEvidence{Process: testAttachmentProcess(401), Ancestry: []procinfo.Identity{testAttachmentProcess(400)}, Executable: "/opt/qwen", SocketPath: "/runtime/qwen.sock", ArtifactPath: "/runtime/events/session.json", ArtifactType: "regular", ArtifactMode: 0o600, ArtifactOwner: 1000, ArtifactRevision: "event-1"},
			refreshed: NativeEvidence{Process: testAttachmentProcess(401), Ancestry: []procinfo.Identity{testAttachmentProcess(400)}, Executable: "/opt/qwen", SocketPath: "/runtime/qwen.sock", ArtifactPath: "/runtime/events/session.json", ArtifactType: "regular", ArtifactMode: 0o600, ArtifactOwner: 1000, ArtifactRevision: "event-2"},
		},
	}

	for _, test := range tests {
		t.Run(test.product, func(t *testing.T) {
			store, err := OpenState(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			var calls []string
			adapter := AttachmentAdapter{
				Prepare: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
					calls = append(calls, "prepare:"+attachment.State)
					return test.expected, nil
				},
				Adopt: func(_ context.Context, attachment ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
					calls = append(calls, "adopt:"+attachment.State)
					if !reflect.DeepEqual(evidence, test.observed) {
						t.Fatalf("adopt evidence = %+v, want %+v", evidence, test.observed)
					}
					return evidence, nil
				},
				Refresh: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
					calls = append(calls, "refresh:"+attachment.State)
					if !reflect.DeepEqual(attachment.Evidence, test.observed) {
						t.Fatalf("refresh attachment evidence = %+v, want %+v", attachment.Evidence, test.observed)
					}
					return test.refreshed, nil
				},
				Detach: func(_ context.Context, attachment ManagedAttachment) error {
					calls = append(calls, "detach:"+attachment.State)
					if !reflect.DeepEqual(attachment.Evidence, test.refreshed) {
						t.Fatalf("detach attachment evidence = %+v, want %+v", attachment.Evidence, test.refreshed)
					}
					return nil
				},
				Rollback: func(_ context.Context, attachment ManagedAttachment) error {
					calls = append(calls, "rollback:"+attachment.State)
					return nil
				},
			}
			engine, err := NewAttachmentEngine(store, 7, map[string]AttachmentAdapter{test.product: adapter})
			if err != nil {
				t.Fatal(err)
			}
			id := test.product + "-attachment"
			prepared, err := engine.Prepare(context.Background(), ManagedAttachment{
				ID: id, CapabilityHash: "capability", Product: test.product, ProfileIdentity: test.product + "-profile",
				LaunchIntent: "fresh", Cwd: "/workspace", Groups: []string{"project"}, PermissionMode: "default",
			})
			active := mustActiveAttachments(t, engine)
			if err != nil || prepared.State != "prepared" || !reflect.DeepEqual(prepared.ExpectedEvidence, test.expected) || len(active) != 0 {
				t.Fatalf("prepare = %+v, %v; active=%+v", prepared, err, active)
			}
			adopted, err := engine.Adopt(context.Background(), id, test.observed)
			if err != nil || adopted.State != "attached" || !reflect.DeepEqual(adopted.Evidence, test.observed) {
				t.Fatalf("adopt = %+v, %v", adopted, err)
			}
			active = mustActiveAttachments(t, engine)
			if len(active) != 1 || active[0].ID != id {
				t.Fatalf("active attachments = %+v", active)
			}
			refreshed, err := engine.Refresh(context.Background(), id)
			if err != nil || refreshed.State != "attached" || !reflect.DeepEqual(refreshed.Evidence, test.refreshed) {
				t.Fatalf("refresh = %+v, %v", refreshed, err)
			}
			detached, err := engine.Detach(context.Background(), id, "normal-exit")
			active = mustActiveAttachments(t, engine)
			if err != nil || detached.State != "detached" || len(active) != 0 {
				t.Fatalf("detach = %+v, %v; active=%+v", detached, err, active)
			}
			wantCalls := []string{"prepare:preparing", "adopt:selecting", "refresh:attached", "detach:detaching"}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("callback order = %v, want %v", calls, wantCalls)
			}
		})
	}
}

func TestAttachmentRollbackAndCleanupDebtAreExactAndRetryable(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rollbackFailure := errors.New("native rollback remains ambiguous")
	adapter := AttachmentAdapter{
		Prepare: func(_ context.Context, _ ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{Process: testAttachmentProcess(500), ArtifactPath: "/owned/launch"}, nil
		},
		Adopt: func(_ context.Context, _ ManagedAttachment, _ NativeEvidence) (NativeEvidence, error) {
			return NativeEvidence{}, errors.New("native evidence mismatch")
		},
		Refresh: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
			return attachment.Evidence, nil
		},
		Detach: func(_ context.Context, _ ManagedAttachment) error { return nil },
		Rollback: func(_ context.Context, attachment ManagedAttachment) error {
			if attachment.ID == "debt" {
				return rollbackFailure
			}
			return nil
		},
	}
	engine, err := NewAttachmentEngine(store, 9, map[string]AttachmentAdapter{"codex": adapter})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"clean", "debt"} {
		if _, err := engine.Prepare(context.Background(), ManagedAttachment{ID: id, CapabilityHash: "cap", Product: "codex", ProfileIdentity: "profile"}); err != nil {
			t.Fatalf("prepare %s: %v", id, err)
		}
		_, err := engine.Adopt(context.Background(), id, NativeEvidence{ThreadID: "wrong"})
		if err == nil || !errors.Is(err, ErrAttachmentAdoption) {
			t.Fatalf("adopt %s error = %v", id, err)
		}
	}

	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Catalog.Attachments["clean"].State; got != "detached" {
		t.Fatalf("clean rollback state = %q", got)
	}
	if got := snapshot.Catalog.Attachments["debt"].State; got != "debt" {
		t.Fatalf("ambiguous rollback state = %q", got)
	}
	debt, ok := snapshot.Catalog.CleanupDebts[attachmentDebtID("debt")]
	if !ok || debt.Resource != "attachment:debt" || debt.Operation != "rollback-codex" || debt.LastVerifiedState != "unknown" {
		t.Fatalf("cleanup debt = %+v, present=%v", debt, ok)
	}
	if active := mustActiveAttachments(t, engine); len(active) != 0 {
		t.Fatalf("rolled back attachments remained addressable: %+v", active)
	}

	adapter.Rollback = func(context.Context, ManagedAttachment) error { return nil }
	engine.SetAdapter("codex", adapter)
	retried, err := engine.Rollback(context.Background(), "debt", "retry-cleanup")
	if err != nil || retried.State != "detached" {
		t.Fatalf("retry rollback = %+v, %v", retried, err)
	}
	snapshot, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Catalog.CleanupDebts[attachmentDebtID("debt")]; ok {
		t.Fatal("successful retry retained cleanup debt")
	}
}

func TestAttachmentPreparationFailureRollsBackBeforeAuthority(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack := false
	engine, err := NewAttachmentEngine(store, 3, map[string]AttachmentAdapter{"qwen": {
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) {
			return NativeEvidence{}, errors.New("native preparation failed")
		},
		Rollback: func(_ context.Context, attachment ManagedAttachment) error {
			rolledBack = attachment.State == "preparing"
			return nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Prepare(context.Background(), ManagedAttachment{ID: "failed", CapabilityHash: "cap", Product: "qwen", ProfileIdentity: "profile"})
	if err == nil || !errors.Is(err, ErrAttachmentPreparation) || !rolledBack {
		t.Fatalf("prepare error = %v, rolledBack=%v", err, rolledBack)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Catalog.Attachments["failed"].State; got != "detached" {
		t.Fatalf("failed preparation state = %q", got)
	}
	if len(mustActiveAttachments(t, engine)) != 0 {
		t.Fatal("failed preparation became active")
	}
}

func TestAttachmentListActiveIsStableAndReturnsCopies(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	adapter := AttachmentAdapter{
		Prepare: func(context.Context, ManagedAttachment) (NativeEvidence, error) { return NativeEvidence{}, nil },
		Adopt: func(_ context.Context, _ ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
			return evidence, nil
		},
	}
	engine, err := NewAttachmentEngine(store, 1, map[string]AttachmentAdapter{"codex": adapter})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"z", "a"} {
		if _, err := engine.Prepare(context.Background(), ManagedAttachment{ID: id, CapabilityHash: "cap", Product: "codex", ProfileIdentity: "profile", Groups: []string{"group"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Adopt(context.Background(), id, NativeEvidence{}); err != nil {
			t.Fatal(err)
		}
	}
	active := mustActiveAttachments(t, engine)
	ids := []string{active[0].ID, active[1].ID}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("active order = %v", ids)
	}
	active[0].Groups[0] = "mutated"
	again := mustActiveAttachments(t, engine)
	if again[0].Groups[0] != "group" {
		t.Fatalf("caller mutated durable attachment: %+v", again[0])
	}
}

func testAttachmentProcess(pid int) procinfo.Identity {
	return procinfo.Identity{PID: pid, Start: "start", StrongStart: "strong"}
}

func mustActiveAttachments(t *testing.T, engine *AttachmentEngine) []ManagedAttachment {
	t.Helper()
	attachments, err := engine.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	return attachments
}

func TestAttachmentNativeTitleIsGenerationLocalAndNeverDurable(t *testing.T) {
	engine := activeAttachmentEngineForTest(t, "peer")
	if err := engine.ObserveNativeTitle("peer", "native-peer", "native-at-launch"); err != nil {
		t.Fatal(err)
	}
	if title, observed, err := engine.LiveNativeTitle("peer"); err != nil || !observed || title != "native-at-launch" {
		t.Fatalf("live title = %q, observed=%v, err=%v", title, observed, err)
	}
	restarted, err := NewAttachmentEngine(engine.store, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if title, observed, err := restarted.LiveNativeTitle("peer"); err != nil || observed || title != "" {
		t.Fatalf("restarted title = %q, observed=%v, err=%v", title, observed, err)
	}
}

func activeAttachmentEngineForTest(t *testing.T, id string) *AttachmentEngine {
	t.Helper()
	state, err := OpenState(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Host.Generation = 1
	catalog.Attachments[id] = ManagedAttachment{
		ID: id, Product: "codex", NativeSessionID: "native-" + id, State: "attached", DaemonGeneration: 1,
	}
	if _, err := state.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine, err := NewAttachmentEngine(state, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
