package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestLaneEngineExistingMutationsCannotBindOrReplaceNativeSession(t *testing.T) {
	t.Run("dispatch cannot bind outside exact Open", func(t *testing.T) {
		engine, store := testLaneEngine(t)
		lane := testDurableLane("lane-unbound", "codex")
		turn := Turn{ID: "turn-unbound", LaneID: lane.ID}
		if err := engine.Create(lane, turn); err != nil {
			t.Fatal(err)
		}
		candidate := lane
		candidate.NativeSessionID = "invented-at-dispatch"
		if err := engine.Dispatch(candidate, turn); err == nil {
			t.Fatal("Dispatch bound an unbound lane outside an explicit binding boundary")
		}
		if err := engine.SetNativeSessionID(lane.ID, "native-at-open"); err != nil {
			t.Fatal(err)
		}
		candidate.NativeSessionID = ""
		if err := engine.Dispatch(candidate, turn); err != nil {
			t.Fatalf("Dispatch did not preserve omitted durable native identity: %v", err)
		}
		snapshot, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Catalog.Lanes[lane.ID].NativeSessionID != "native-at-open" {
			t.Fatalf("Dispatch changed exact native identity: %+v", snapshot.Catalog.Lanes[lane.ID])
		}
	})

	t.Run("accept rejects replacement and preserves omission", func(t *testing.T) {
		engine, store := testLaneEngine(t)
		lane := testDurableLane("lane-bound", "codex")
		lane.NativeSessionID = "native-a"
		firstTurn := Turn{ID: "turn-first", LaneID: lane.ID}
		if err := engine.Create(lane, firstTurn); err != nil {
			t.Fatal(err)
		}
		firstTurn.Outcome, firstTurn.CompletedAt = "completed", 1
		if _, err := engine.Complete(lane, firstTurn); err != nil {
			t.Fatal(err)
		}
		replacement := lane
		replacement.NativeSessionID = "native-b"
		if err := engine.AcceptQueuedTurn(replacement, Turn{ID: "turn-replacement", LaneID: lane.ID}); err == nil {
			t.Fatal("AcceptQueuedTurn replaced a bound native identity")
		}
		omitted := lane
		omitted.NativeSessionID = ""
		if err := engine.AcceptQueuedTurn(omitted, Turn{ID: "turn-omitted", LaneID: lane.ID}); err != nil {
			t.Fatalf("AcceptQueuedTurn did not preserve omitted durable native identity: %v", err)
		}
		snapshot, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Catalog.Lanes[lane.ID].NativeSessionID != "native-a" {
			t.Fatalf("AcceptQueuedTurn changed exact native identity: %+v", snapshot.Catalog.Lanes[lane.ID])
		}
	})

	t.Run("complete preserves authority and converges diagnostic", func(t *testing.T) {
		engine, store := testLaneEngine(t)
		lane := testDurableLane("lane-terminal", "qwen")
		lane.NativeSessionID = "native-a"
		turn := Turn{ID: "turn-terminal", LaneID: lane.ID}
		if err := engine.Create(lane, turn); err != nil {
			t.Fatal(err)
		}
		conflicting := lane
		conflicting.NativeSessionID = "native-b"
		terminal := turn
		terminal.Outcome, terminal.Result, terminal.CompletedAt = "completed", "native result", 1
		if already, err := engine.Complete(conflicting, terminal); err != nil || already {
			t.Fatalf("conflicting terminal convergence = already %v, %v", already, err)
		}
		snapshot, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		storedLane, storedTurn := snapshot.Catalog.Lanes[lane.ID], snapshot.Catalog.Turns[turn.ID]
		if storedLane.NativeSessionID != "native-a" || storedTurn.Outcome != "failed" || storedTurn.ExitCode != 1 ||
			!strings.Contains(storedTurn.Diagnostic, "did not match durable authority") {
			t.Fatalf("terminal mismatch did not converge truthfully: lane=%+v turn=%+v", storedLane, storedTurn)
		}
		if already, err := engine.Complete(conflicting, terminal); err != nil || !already {
			t.Fatalf("conflicting terminal replay = already %v, %v", already, err)
		}
	})
}

func TestLaneEngineSerializesTurnsAndCollectsEveryTerminalExactlyOnce(t *testing.T) {
	engine, store := testLaneEngine(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	engine.now = func() time.Time { return now }
	lane := testDurableLane("lane-a", "codex")
	turnA := Turn{ID: "turn-a", LaneID: lane.ID}
	if err := engine.Create(lane, turnA); err != nil {
		t.Fatal(err)
	}
	lane.State, lane.NativeSessionID, lane.CapabilityHash = "running", "native-a", "capability"
	if err := engine.SetNativeSessionID(lane.ID, lane.NativeSessionID); err != nil {
		t.Fatal(err)
	}
	turnA.NativeDispatchID, turnA.StartedAt = "dispatch-a", now.UnixMilli()
	if err := engine.Dispatch(lane, turnA); err != nil {
		t.Fatal(err)
	}
	turnA.Outcome, turnA.Result, turnA.CompletedAt = "completed", "first", now.Add(time.Second).UnixMilli()
	if already, err := engine.Complete(lane, turnA); err != nil || already {
		t.Fatalf("first terminal commit = already %v, %v", already, err)
	}
	if already, err := engine.Complete(lane, turnA); err != nil || !already {
		t.Fatalf("terminal replay = already %v, %v", already, err)
	}

	turnB := Turn{ID: "turn-b", LaneID: lane.ID}
	if err := engine.AcceptTurn(lane, turnB); err == nil || !strings.Contains(err.Error(), "collect outstanding") {
		t.Fatalf("explicit resume with debt = %v", err)
	}
	if err := engine.AcceptQueuedTurn(lane, turnB); err != nil {
		t.Fatal(err)
	}
	turnB.NativeDispatchID, turnB.StartedAt = "dispatch-b", now.Add(2*time.Second).UnixMilli()
	if err := engine.Dispatch(lane, turnB); err != nil {
		t.Fatal(err)
	}
	turnB.Outcome, turnB.Result, turnB.CompletedAt = "completed", "second", now.Add(3*time.Second).UnixMilli()
	if _, err := engine.Complete(lane, turnB); err != nil {
		t.Fatal(err)
	}
	oldest, ok, err := engine.OldestCollectable(lane.ID)
	if err != nil || !ok || oldest.ID != turnA.ID || oldest.Sequence != 1 {
		t.Fatalf("oldest turn = %+v, %v, %v", oldest, ok, err)
	}
	first, err := engine.Collect(lane.ID, turnA.ID, time.Minute)
	if err != nil || !first.RemainingDebt || first.AutoArchiveAt != 0 {
		t.Fatalf("first collection = %+v, %v", first, err)
	}
	second, err := engine.Collect(lane.ID, turnB.ID, time.Minute)
	if err != nil || second.RemainingDebt || second.AutoArchiveAt != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("second collection = %+v, %v", second, err)
	}
	if _, err := engine.Collect(lane.ID, turnB.ID, time.Minute); err == nil {
		t.Fatal("collected turn was acknowledged twice")
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog.Turns[turnA.ID].CollectionRevision <= snapshot.Catalog.Turns[turnA.ID].TerminalRevision ||
		snapshot.Catalog.Turns[turnB.ID].Sequence != 2 || snapshot.Catalog.Lanes[lane.ID].State != "idle" {
		t.Fatalf("durable lane catalog = %+v", snapshot.Catalog)
	}
}

func TestLaneEngineRestartNoticeArchiveAndCleanupDebtAreDurable(t *testing.T) {
	engine, store := testLaneEngine(t)
	now := time.Unix(1_700_000_100, 0).UTC()
	engine.now = func() time.Time { return now }
	for _, product := range []string{"codex", "qwen"} {
		lane := testDurableLane("lane-"+product, product)
		turn := Turn{ID: "turn-" + product, LaneID: lane.ID}
		if err := engine.Create(lane, turn); err != nil {
			t.Fatal(err)
		}
		lane.State, lane.NativeSessionID = "running", "native-"+product
		if err := engine.SetNativeSessionID(lane.ID, lane.NativeSessionID); err != nil {
			t.Fatal(err)
		}
		turn.NativeDispatchID = "dispatch-" + product
		if err := engine.Dispatch(lane, turn); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.ReconcileRestart(func(lane Lane, _ Turn) bool {
		return lane.Product == "codex" && lane.NativeSessionID != ""
	}, "daemon restart interrupted native turn"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog.Lanes["lane-codex"].State != "running" || snapshot.Catalog.Lanes["lane-qwen"].State != "terminal" ||
		snapshot.Catalog.Turns["turn-qwen"].Outcome != "interrupted" {
		t.Fatalf("restart reconciliation = %+v", snapshot.Catalog)
	}

	notice := Delivery{ID: "lane-terminal-stable", CorrelationID: "turn-qwen", Sender: "lane-qwen", Destinations: []string{"parent"}, State: "accepted"}
	if acknowledged, err := engine.PrepareTerminalNotice(notice); err != nil || acknowledged {
		t.Fatalf("prepare notice = %v, %v", acknowledged, err)
	}
	if err := engine.TransitionTerminalNotice(notice.ID, "retryable", "destination-unavailable"); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := engine.PrepareTerminalNotice(notice); err != nil || acknowledged {
		t.Fatalf("retry notice = %v, %v", acknowledged, err)
	}
	if err := engine.TransitionTerminalNotice(notice.ID, "presented", ""); err != nil {
		t.Fatal(err)
	}
	if err := engine.TransitionTerminalNotice(notice.ID, "acknowledged", ""); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := engine.PrepareTerminalNotice(notice); err != nil || !acknowledged {
		t.Fatalf("acknowledged notice replay = %v, %v", acknowledged, err)
	}

	debt := CleanupDebt{ID: "lane-debt", Resource: "/owned/native", BaselineIdentity: "pid/start", IntendedState: "absent", LastVerifiedState: "unknown", Operation: "archive-qwen"}
	if err := engine.RecordCleanupDebt("lane-qwen", debt); err != nil {
		t.Fatal(err)
	}
	if err := engine.ResolveCleanupDebt("lane-qwen", debt.ID, "archived"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog.Deliveries[notice.ID].State != "acknowledged" ||
		snapshot.Catalog.Lanes["lane-qwen"].State != "archived" || len(snapshot.Catalog.CleanupDebts) != 0 {
		t.Fatalf("notice/archive/debt state = %+v", snapshot.Catalog)
	}
}

func testLaneEngine(t *testing.T) (*LaneEngine, *StateStore) {
	t.Helper()
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewLaneEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	return engine, store
}

func testDurableLane(id, product string) Lane {
	return Lane{
		ID: id, ParentAttachmentID: "parent", Product: product, Name: id,
		Cwd: "/workspace", Groups: []string{"peer-dev"}, PermissionMode: "bypassPermissions",
		Persistent: true, AutoArchive: true, AutoArchiveDelayMS: time.Minute.Milliseconds(),
	}
}
