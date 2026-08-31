package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestRuntimeRecoveryPreservesFourIdleBusyPeersWithoutRedispatch(t *testing.T) {
	products := []string{"codex", "claude", "grok", "qwen"}
	var (
		refreshMu sync.Mutex
		refreshes = map[string]int{}
	)
	adapters := map[string]AttachmentAdapter{}
	for _, product := range products {
		product := product
		adapters[product] = AttachmentAdapter{
			Prepare: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
				return NativeEvidence{ThreadID: "native-" + attachment.ID}, nil
			},
			Adopt: func(_ context.Context, attachment ManagedAttachment, evidence NativeEvidence) (NativeEvidence, error) {
				if evidence.ThreadID != "native-"+attachment.ID {
					return NativeEvidence{}, errors.New("native evidence mismatch")
				}
				return evidence, nil
			},
			Refresh: func(_ context.Context, attachment ManagedAttachment) (NativeEvidence, error) {
				refreshMu.Lock()
				refreshes[product]++
				refreshMu.Unlock()
				return attachment.Evidence, nil
			},
			Detach:   func(context.Context, ManagedAttachment) error { return nil },
			Rollback: func(context.Context, ManagedAttachment) error { return nil },
		}
	}

	root := shortDaemonTestRoot(t)
	first, err := StartRuntime(context.Background(), RuntimeConfig{StateRoot: root, Adapters: adapters})
	if err != nil {
		t.Fatal(err)
	}
	lanes, err := NewLaneEngine(first.State())
	if err != nil {
		t.Fatal(err)
	}
	for index, product := range products {
		attachmentID := "attachment-" + product
		evidence := NativeEvidence{ThreadID: "native-" + attachmentID}
		if _, err := first.Attachments().Prepare(context.Background(), ManagedAttachment{ID: attachmentID, CapabilityHash: CapabilityDigest("cap-" + product), Product: product, ProfileIdentity: "profile-" + product}); err != nil {
			t.Fatal(err)
		}
		if _, err := first.Attachments().Adopt(context.Background(), attachmentID, evidence); err != nil {
			t.Fatal(err)
		}
		lane := Lane{ID: "lane-" + product, ParentAttachmentID: attachmentID, Product: product, NativeSessionID: "session-" + product}
		turn := Turn{ID: "turn-" + product, LaneID: lane.ID}
		if err := lanes.Create(lane, turn); err != nil {
			t.Fatal(err)
		}
		if index%2 == 0 {
			turn.NativeDispatchID = "dispatch-" + product
			if err := lanes.Dispatch(lane, turn); err != nil {
				t.Fatal(err)
			}
			if err := lanes.SetNativeDispatchID(turn.ID, turn.NativeDispatchID); err != nil {
				t.Fatal(err)
			}
		} else {
			turn.Outcome, turn.ExitCode, turn.CompletedAt = "completed", 0, int64(index+1)
			if _, err := lanes.Complete(lane, turn); err != nil {
				t.Fatal(err)
			}
			if _, err := lanes.Collect(lane.ID, turn.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	firstGeneration := first.Generation()
	if err := first.crashForTest(errors.New("simulated process death")); err != nil {
		t.Fatal(err)
	}

	recoverDecisions := 0
	successor, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: root, Adapters: adapters,
		Initialize: func(runtime *Runtime) error {
			for _, product := range products {
				if _, err := runtime.Attachments().Refresh(context.Background(), "attachment-"+product); err != nil {
					return err
				}
			}
			engine, err := NewLaneEngine(runtime.State())
			if err != nil {
				return err
			}
			return engine.ReconcileRestart(func(lane Lane, turn Turn) bool {
				recoverDecisions++
				return lane.NativeSessionID != "" && turn.NativeDispatchID != ""
			}, "metadata-only restart interruption")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	if successor.Generation() != firstGeneration+1 {
		t.Fatalf("successor generation = %d, want %d", successor.Generation(), firstGeneration+1)
	}
	active, err := successor.Attachments().ListActive()
	if err != nil || len(active) != 4 {
		t.Fatalf("recovered attachments = %+v, %v", active, err)
	}
	for _, attachment := range active {
		if attachment.DaemonGeneration != successor.Generation() {
			t.Fatalf("attachment %s retained generation %d", attachment.ID, attachment.DaemonGeneration)
		}
	}
	if recoverDecisions != 2 {
		t.Fatalf("busy recovery decisions = %d, want 2", recoverDecisions)
	}
	refreshMu.Lock()
	defer refreshMu.Unlock()
	for _, product := range products {
		if refreshes[product] != 1 {
			t.Fatalf("%s refreshes = %d, want 1", product, refreshes[product])
		}
	}

	snapshot, err := successor.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Catalog.Attachments) != 4 || len(snapshot.Catalog.Lanes) != 4 || len(snapshot.Catalog.Turns) != 4 || len(snapshot.Catalog.CleanupDebts) != 0 {
		t.Fatalf("restart residue attachments=%d lanes=%d turns=%d debts=%d", len(snapshot.Catalog.Attachments), len(snapshot.Catalog.Lanes), len(snapshot.Catalog.Turns), len(snapshot.Catalog.CleanupDebts))
	}
	for index, product := range products {
		lane := snapshot.Catalog.Lanes["lane-"+product]
		turn := snapshot.Catalog.Turns["turn-"+product]
		if index%2 == 0 {
			if lane.State != "running" || turn.State != "dispatched" || turn.NativeDispatchID != "dispatch-"+product || turn.Sequence != 1 {
				t.Fatalf("busy %s was redispatched or rewritten: lane=%+v turn=%+v", product, lane, turn)
			}
		} else if lane.State != "idle" || turn.State != "collected" || turn.Sequence != 1 {
			t.Fatalf("idle %s changed across restart: lane=%+v turn=%+v", product, lane, turn)
		}
		if lane.NativeSessionID != "session-"+product {
			t.Fatalf("%s native session changed to %q", product, lane.NativeSessionID)
		}
	}
	if snapshot.Catalog.Host.ServiceState != "running" {
		t.Fatalf("successor service state = %s", snapshot.Catalog.Host.ServiceState)
	}
}
