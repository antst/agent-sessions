package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func newLaneInputTestEngine(t *testing.T, limits LaneInputLimits) (*LaneInputEngine, *StateStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := OpenState(filepath.Join(root, "state"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	catalog := emptyCatalog()
	catalog.Host = HostRuntime{User: "1000", Host: "test", Generation: 7}
	catalog.Lanes["lane"] = Lane{ID: "lane", Product: "codex", ProfileIdentity: "profile", NativeSessionID: "native", State: "running"}
	catalog.Turns["turn"] = Turn{ID: "turn", LaneID: "lane", Sequence: 1, State: "dispatched"}
	if _, err := store.Commit(0, catalog); err != nil {
		t.Fatal(err)
	}
	spoolRoot := filepath.Join(root, "spool")
	engine, err := NewLaneInputEngine(store, spoolRoot, limits)
	if err != nil {
		t.Fatal(err)
	}
	engine.now = func() time.Time { return time.Unix(100, 0) }
	return engine, store, spoolRoot
}

func newUnboundLaneInputTestEngine(t *testing.T) (*LaneInputEngine, *StateStore, Lane) {
	t.Helper()
	root := t.TempDir()
	store, err := OpenState(filepath.Join(root, "state"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	lane := Lane{ID: "lane", Product: "codex", ProfileIdentity: "profile", State: "terminal"}
	catalog := emptyCatalog()
	catalog.Host = HostRuntime{User: "1000", Host: "test", Generation: 7}
	catalog.Lanes[lane.ID] = lane
	if _, err := store.Commit(0, catalog); err != nil {
		t.Fatal(err)
	}
	engine, err := NewLaneInputEngine(store, filepath.Join(root, "spool"), DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	engine.now = func() time.Time { return time.Unix(100, 0) }
	return engine, store, lane
}

func TestLaneInputAdmissionPersistsPrivateVerifiedBodyBeforeAcknowledgment(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	body := []byte("secret prompt body")
	receipt, err := engine.Admit("lane", body)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != ReceiptQueued || receipt.Sequence != 1 || receipt.Digest != sha256.Sum256(body) || receipt.Bytes != int64(len(body)) {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	rootInfo, err := os.Stat(spoolRoot)
	if err != nil || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("spool root mode: info=%v err=%v", rootInfo, err)
	}
	entries, err := os.ReadDir(spoolRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("spool entries=%v err=%v", entries, err)
	}
	objectInfo, err := entries[0].Info()
	if err != nil || objectInfo.Mode().Perm() != 0o600 || !objectInfo.Mode().IsRegular() {
		t.Fatalf("spool object mode: info=%v err=%v", objectInfo, err)
	}
	reader, metadata, err := engine.OpenVerified(receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, body) || metadata.Digest != receipt.Digest || metadata.Bytes != receipt.Bytes {
		t.Fatalf("verified read body=%q metadata=%+v read=%v close=%v", got, metadata, readErr, closeErr)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog.Lanes["lane"].InputSequence != 1 || snapshot.Catalog.LaneInputs[receipt.ReceiptID].ReceiptID != receipt.ReceiptID {
		t.Fatalf("durable admission missing: %+v", snapshot.Catalog)
	}
	encoded := mustJSON(t, snapshot.Catalog)
	if bytes.Contains(encoded, body) || bytes.Contains(encoded, []byte(spoolRoot)) {
		t.Fatalf("catalog leaked body or spool path: %s", encoded)
	}
}

func TestLaneInputAdmissionFailureLeavesRecoverableOrphanAndNeverAcknowledges(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	engine.afterSpoolSync = func() error { return errors.New("crash before receipt commit") }
	if _, err := engine.Admit("lane", []byte("orphan")); err == nil {
		t.Fatal("admission unexpectedly succeeded")
	}
	snapshot, _ := store.Read()
	if len(snapshot.Catalog.LaneInputs) != 0 || snapshot.Catalog.Lanes["lane"].InputSequence != 0 {
		t.Fatalf("failed admission became durable: %+v", snapshot.Catalog.LaneInputs)
	}
	entries, _ := os.ReadDir(spoolRoot)
	if len(entries) != 1 {
		t.Fatalf("want one crash orphan, got %d", len(entries))
	}
	engine.afterSpoolSync = nil
	report, err := engine.Recover()
	if err != nil || report.OrphansRemoved != 1 {
		t.Fatalf("recover report=%+v err=%v", report, err)
	}
	entries, _ = os.ReadDir(spoolRoot)
	if len(entries) != 0 {
		t.Fatalf("orphan remains after recovery: %v", entries)
	}
}

func TestLaneInputQuotasAreEnforcedBeforeDurableAcceptance(t *testing.T) {
	limits := LaneInputLimits{MaxInputBytes: 4, MaxLaneBytes: 6, MaxLaneObjects: 2, MaxHostBytes: 8, MaxHostObjects: 3}
	engine, store, _ := newLaneInputTestEngine(t, limits)
	if _, err := engine.Admit("lane", []byte("12345")); !errors.Is(err, ErrLaneInputTooLarge) {
		t.Fatalf("oversize error=%v", err)
	}
	if _, err := engine.Admit("lane", []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Admit("lane", []byte("123")); !errors.Is(err, ErrLaneInputQuota) {
		t.Fatalf("lane quota error=%v", err)
	}
	snapshot, _ := store.Read()
	if len(snapshot.Catalog.LaneInputs) != 1 || snapshot.Catalog.Lanes["lane"].InputSequence != 1 {
		t.Fatalf("quota rejection mutated state: %+v", snapshot.Catalog.LaneInputs)
	}
}

func TestLaneInputAdmissionKeyIsDurablyIdempotentAndConflictsFailClosed(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	first, err := engine.AdmitWithID("delivery-key", "lane", []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.AdmitWithID("delivery-key", "lane", []byte("same"))
	if err != nil || second.ReceiptID != first.ReceiptID || second.Sequence != first.Sequence {
		t.Fatalf("idempotent admission=%+v err=%v", second, err)
	}
	if _, err := engine.AdmitWithID("delivery-key", "lane", []byte("different")); !errors.Is(err, ErrLaneInputConflict) {
		t.Fatalf("conflicting admission error=%v", err)
	}
	snapshot, _ := store.Read()
	if len(snapshot.Catalog.LaneInputs) != 1 || snapshot.Catalog.Lanes["lane"].InputSequence != 1 {
		t.Fatalf("idempotency mutated authority: %+v", snapshot.Catalog.LaneInputs)
	}
	if entries, _ := os.ReadDir(spoolRoot); len(entries) != 1 {
		t.Fatalf("idempotency leaked spool objects: %v", entries)
	}
}

func TestLaneInputAdmissionAndSelectionFailClosedForNonLiveOrDebtedLane(t *testing.T) {
	for _, state := range []string{"retiring", "archived", "cleanup-debt"} {
		t.Run(state, func(t *testing.T) {
			engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
			snapshot, _ := store.Read()
			catalog := snapshot.Catalog
			lane := catalog.Lanes["lane"]
			if state == "archived" {
				lane.State = "terminal"
				catalog.Lanes["lane"] = lane
				committed, err := store.Commit(snapshot.Revision, catalog)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, catalog, lane = committed, committed.Catalog, committed.Catalog.Lanes["lane"]
			}
			lane.State = state
			catalog.Lanes["lane"] = lane
			if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.AdmitWithID("blocked", "lane", []byte("body")); !errors.Is(err, ErrLaneInputUnavailable) {
				t.Fatalf("state %s admission error=%v", state, err)
			}
			if _, _, err := engine.EarliestQueued("lane"); !errors.Is(err, ErrLaneInputUnavailable) {
				t.Fatalf("state %s selection error=%v", state, err)
			}
		})
	}

	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	receipt, err := engine.Admit("lane", []byte("queued"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Read()
	catalog := snapshot.Catalog
	catalog.CleanupDebts[laneInputDebtID(receipt.ReceiptID)] = CleanupDebt{
		ID: laneInputDebtID(receipt.ReceiptID), Resource: "lane-input:" + receipt.ReceiptID,
		BaselineIdentity: receipt.SpoolObjectID, IntendedState: "absent", LastVerifiedState: "unknown", RetryRevision: 1, Operation: "retire-lane-input",
	}
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.EarliestQueued("lane"); !errors.Is(err, ErrLaneInputCleanupDebt) {
		t.Fatalf("debt selection error=%v", err)
	}
}

func TestLaneInputTerminalLaneAcceptsAndSelectsWhileOlderTurnRemainsCollectable(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	snapshot, _ := store.Read()
	catalog := snapshot.Catalog
	lane := catalog.Lanes["lane"]
	lane.State = "terminal"
	catalog.Lanes["lane"] = lane
	turn := catalog.Turns["turn"]
	turn.State, turn.Outcome, turn.CompletedAt, turn.TerminalRevision = "terminal", "completed", 99, 1
	catalog.Turns["turn"] = turn
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	receipt, err := engine.AdmitWithID("terminal-queued", "lane", []byte("next input"))
	if err != nil || receipt.State != ReceiptQueued {
		t.Fatalf("terminal admission=%+v err=%v", receipt, err)
	}
	selected, ok, err := engine.EarliestQueued("lane")
	if err != nil || !ok || selected.ReceiptID != receipt.ReceiptID {
		t.Fatalf("terminal selection=%+v ok=%v err=%v", selected, ok, err)
	}
	snapshot, _ = store.Read()
	if snapshot.Catalog.Lanes["lane"].State != "terminal" || snapshot.Catalog.Turns["turn"].State != "terminal" ||
		snapshot.Catalog.Turns["turn"].CollectionRevision != 0 {
		t.Fatalf("queue admission consumed collection debt: lane=%+v turn=%+v", snapshot.Catalog.Lanes["lane"], snapshot.Catalog.Turns["turn"])
	}
}

func TestLaneInputDispatchRequeueInjectionAmbiguityAndRetirement(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	receipt, err := engine.Admit("lane", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	queued, ok, err := engine.EarliestQueued("lane")
	if err != nil || !ok || queued.ReceiptID != receipt.ReceiptID {
		t.Fatalf("earliest=%+v ok=%v err=%v", queued, ok, err)
	}
	dispatching, err := engine.MarkDispatching(receipt.ReceiptID, "turn", "attempt-one")
	if err != nil || dispatching.State != ReceiptDispatching {
		t.Fatalf("dispatching=%+v err=%v", dispatching, err)
	}
	requeued, err := engine.RequeueUnsupportedSteer(receipt.ReceiptID)
	if err != nil || requeued.State != ReceiptQueued || requeued.Sequence != receipt.Sequence || requeued.ReceiptID != receipt.ReceiptID {
		t.Fatalf("requeued=%+v err=%v", requeued, err)
	}
	if _, err := engine.MarkDispatching(receipt.ReceiptID, "turn", "attempt-two"); err != nil {
		t.Fatal(err)
	}
	ambiguous, err := engine.MarkAmbiguous(receipt.ReceiptID, AmbiguityNativeAcceptanceUnproven)
	if err != nil || ambiguous.State != ReceiptAmbiguous {
		t.Fatalf("ambiguous=%+v err=%v", ambiguous, err)
	}
	acceptance := NativeAcceptanceRef{NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 101}
	injected, err := engine.MarkInjected(receipt.ReceiptID, acceptance)
	if err != nil || injected.State != ReceiptInjected || injected.NativeAcceptance == nil {
		t.Fatalf("injected=%+v err=%v", injected, err)
	}
	retired, err := engine.Retire(receipt.ReceiptID)
	if err != nil || retired.State != ReceiptRetired {
		t.Fatalf("retired=%+v err=%v", retired, err)
	}
	if entries, _ := os.ReadDir(spoolRoot); len(entries) != 0 {
		t.Fatalf("retired object remains: %v", entries)
	}
	snapshot, _ := store.Read()
	if snapshot.Catalog.LaneInputs[receipt.ReceiptID].State != ReceiptRetired {
		t.Fatalf("retirement not durable: %+v", snapshot.Catalog.LaneInputs[receipt.ReceiptID])
	}
}

func TestLaneInputAtomicTurnAcceptanceAndDispatchIntentShareOneRevision(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	laneEngine, err := NewLaneEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := laneEngine.Complete(
		Lane{ID: "lane", Product: "codex", NativeSessionID: "native", State: "terminal"},
		Turn{ID: "turn", LaneID: "lane", State: "terminal", Outcome: "completed", CompletedAt: 1},
	); err != nil {
		t.Fatal(err)
	}
	receipt, err := engine.AdmitWithID("atomic-receipt", "lane", []byte("exact body"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	dispatching, err := engine.AcceptTurnAndMarkDispatching(
		receipt.ReceiptID,
		Lane{ID: "lane", Product: "codex", NativeSessionID: "native"},
		Turn{ID: "atomic-turn", LaneID: "lane"},
		"atomic-attempt",
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+1 || dispatching.State != ReceiptDispatching ||
		after.Catalog.Turns["atomic-turn"].State != "accepted" ||
		after.Catalog.LaneInputs[receipt.ReceiptID].TargetTurnID != "atomic-turn" {
		t.Fatalf("atomic acceptance revision: before=%d after=%d turn=%+v receipt=%+v",
			before.Revision, after.Revision, after.Catalog.Turns["atomic-turn"], after.Catalog.LaneInputs[receipt.ReceiptID])
	}
	accepted, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "native", NativeMessageID: "native-turn", AcceptedAt: receipt.AcceptedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptedSnapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if acceptedSnapshot.Revision != after.Revision+1 || accepted.State != ReceiptInjected ||
		accepted.NativeAcceptance == nil || accepted.NativeAcceptance.NativeMessageID != "native-turn" ||
		acceptedSnapshot.Catalog.Turns["atomic-turn"].State != "dispatched" ||
		acceptedSnapshot.Catalog.Turns["atomic-turn"].NativeDispatchID != "native-turn" {
		t.Fatalf("atomic native acceptance revision: before=%d after=%d turn=%+v receipt=%+v",
			after.Revision, acceptedSnapshot.Revision, acceptedSnapshot.Catalog.Turns["atomic-turn"], accepted)
	}
}

func TestDeferredNativeSessionBindingCommitsLaneReceiptAndTurnInOneCAS(t *testing.T) {
	engine, store, lane := newUnboundLaneInputTestEngine(t)
	receipt, err := engine.AdmitWithID("deferred-binding-receipt", lane.ID, []byte("first turn"))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != ReceiptQueued {
		t.Fatalf("first input was not queued by lane ID: %+v", receipt)
	}
	turn := Turn{ID: "deferred-binding-turn", LaneID: lane.ID}
	if _, err := engine.AcceptTurnAndMarkDispatching(receipt.ReceiptID, lane, turn, "deferred-binding-attempt"); err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "product-generated-session", NativeMessageID: "product-native-turn", AcceptedAt: receipt.AcceptedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+1 || after.Catalog.Host.LaneRevision != before.Catalog.Host.LaneRevision+1 {
		t.Fatalf("deferred binding used more than one CAS: before=%+v after=%+v", before, after)
	}
	boundLane := after.Catalog.Lanes[lane.ID]
	boundTurn := after.Catalog.Turns[turn.ID]
	if boundLane.NativeSessionID != "product-generated-session" || accepted.State != ReceiptInjected ||
		accepted.NativeAcceptance == nil || accepted.NativeAcceptance.NativeSessionID != boundLane.NativeSessionID ||
		boundTurn.State != "dispatched" || boundTurn.NativeDispatchID != "product-native-turn" {
		t.Fatalf("atomic deferred binding facts diverged: lane=%+v receipt=%+v turn=%+v", boundLane, accepted, boundTurn)
	}

	replayBefore := after.Revision
	replayedAcceptance := *accepted.NativeAcceptance
	replayedAcceptance.AcceptedAt++
	if _, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, replayedAcceptance); err != nil {
		t.Fatalf("exact acceptance replay failed: %v", err)
	}
	replayed, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Revision != replayBefore {
		t.Fatalf("exact replay mutated revision: before=%d after=%d", replayBefore, replayed.Revision)
	}
	if _, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "different-session", NativeMessageID: "product-native-turn", AcceptedAt: receipt.AcceptedAt,
	}); err == nil {
		t.Fatal("bound lane accepted a different native session")
	}
	conflict, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Revision != replayBefore || conflict.Catalog.Lanes[lane.ID].NativeSessionID != "product-generated-session" {
		t.Fatalf("conflicting replay mutated bound authority: %+v", conflict)
	}
}

func TestLaneInputExistingLaneMutationsCannotBindOrReplaceNativeSession(t *testing.T) {
	t.Run("queued turn acceptance", func(t *testing.T) {
		engine, store, lane := newUnboundLaneInputTestEngine(t)
		laneEngine, err := NewLaneEngine(store)
		if err != nil {
			t.Fatal(err)
		}
		if err := laneEngine.SetNativeSessionID(lane.ID, "native-a"); err != nil {
			t.Fatal(err)
		}
		receipt, err := engine.AdmitWithID("existing-lane-receipt", lane.ID, []byte("queued"))
		if err != nil {
			t.Fatal(err)
		}
		replacement := lane
		replacement.NativeSessionID = "native-b"
		if _, err := engine.AcceptTurnAndMarkDispatching(
			receipt.ReceiptID, replacement, Turn{ID: "replacement-turn", LaneID: lane.ID}, "replacement-attempt",
		); err == nil {
			t.Fatal("queued turn acceptance replaced a bound native session")
		}
		omitted := lane
		omitted.NativeSessionID = ""
		if _, err := engine.AcceptTurnAndMarkDispatching(
			receipt.ReceiptID, omitted, Turn{ID: "preserved-turn", LaneID: lane.ID}, "preserved-attempt",
		); err != nil {
			t.Fatalf("queued turn did not preserve omitted durable native session: %v", err)
		}
		snapshot, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Catalog.Lanes[lane.ID].NativeSessionID != "native-a" {
			t.Fatalf("queued turn changed bound native authority: %+v", snapshot.Catalog.Lanes[lane.ID])
		}
	})

	t.Run("command resume staging", func(t *testing.T) {
		engine, store, lane := newUnboundLaneInputTestEngine(t)
		laneEngine, err := NewLaneEngine(store)
		if err != nil {
			t.Fatal(err)
		}
		if err := laneEngine.SetNativeSessionID(lane.ID, "native-a"); err != nil {
			t.Fatal(err)
		}
		replacement := lane
		replacement.NativeSessionID = "native-b"
		if _, err := engine.UpdateLaneAdmitAndMarkDispatching(
			"command-replacement-receipt", replacement,
			Turn{ID: "command-replacement-turn", LaneID: lane.ID}, "command-replacement-attempt", []byte("resume"),
		); err == nil {
			t.Fatal("command resume staging replaced a bound native session")
		}
		if entries, err := os.ReadDir(engine.spool.root); err != nil || len(entries) != 0 {
			t.Fatalf("rejected replacement wrote a spool object: entries=%v err=%v", entries, err)
		}
		omitted := lane
		omitted.NativeSessionID = ""
		if _, err := engine.UpdateLaneAdmitAndMarkDispatching(
			"command-preserved-receipt", omitted,
			Turn{ID: "command-preserved-turn", LaneID: lane.ID}, "command-preserved-attempt", []byte("resume"),
		); err != nil {
			t.Fatalf("command resume did not preserve omitted durable native session: %v", err)
		}
		snapshot, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Catalog.Lanes[lane.ID].NativeSessionID != "native-a" {
			t.Fatalf("command resume changed bound native authority: %+v", snapshot.Catalog.Lanes[lane.ID])
		}
	})
}

func TestDeferredNativeSessionBindingFailsClosedWithoutExactAcceptance(t *testing.T) {
	engine, store, lane := newUnboundLaneInputTestEngine(t)
	receipt, err := engine.AdmitWithID("deferred-ambiguous-receipt", lane.ID, []byte("possibly written"))
	if err != nil {
		t.Fatal(err)
	}
	turn := Turn{ID: "deferred-ambiguous-turn", LaneID: lane.ID}
	if _, err := engine.AcceptTurnAndMarkDispatching(receipt.ReceiptID, lane, turn, "deferred-ambiguous-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "invented-placeholder", AcceptedAt: receipt.AcceptedAt,
	}); err == nil {
		t.Fatal("incomplete acceptance invented a deferred native binding")
	}
	if _, err := engine.MarkAmbiguous(receipt.ReceiptID, AmbiguityNativeAcceptanceUnproven); err != nil {
		t.Fatal(err)
	}
	ambiguous, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.Catalog.Lanes[lane.ID].NativeSessionID != "" || ambiguous.Catalog.LaneInputs[receipt.ReceiptID].State != ReceiptAmbiguous {
		t.Fatalf("possible write acquired fake authority: lane=%+v receipt=%+v", ambiguous.Catalog.Lanes[lane.ID], ambiguous.Catalog.LaneInputs[receipt.ReceiptID])
	}
	if _, err := engine.MarkInjected(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "uncorrelated-session", NativeMessageID: "uncorrelated-turn", AcceptedAt: receipt.AcceptedAt,
	}); err == nil {
		t.Fatal("uncoupled receipt proof adopted an unbound lane")
	}
	stillAmbiguous, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if stillAmbiguous.Catalog.Lanes[lane.ID].NativeSessionID != "" || stillAmbiguous.Catalog.LaneInputs[receipt.ReceiptID].State != ReceiptAmbiguous {
		t.Fatal("failed adoption changed durable authority")
	}

	if _, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "authoritatively-reconciled-session", NativeMessageID: "authoritatively-reconciled-turn", AcceptedAt: receipt.AcceptedAt,
	}); err != nil {
		t.Fatalf("authoritative ambiguity reconciliation failed: %v", err)
	}
	bound, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if bound.Catalog.Lanes[lane.ID].NativeSessionID != "authoritatively-reconciled-session" ||
		bound.Catalog.LaneInputs[receipt.ReceiptID].State != ReceiptInjected ||
		bound.Catalog.Turns[turn.ID].NativeDispatchID != "authoritatively-reconciled-turn" {
		t.Fatalf("authoritative reconciliation was not atomic: %+v", bound.Catalog)
	}
}

func TestUnboundLaneRestartIsNoNativeAndRedispatchesQueuedFirstInputByLaneID(t *testing.T) {
	engine, store, lane := newUnboundLaneInputTestEngine(t)
	receipt, err := engine.AdmitWithID("deferred-restart-receipt", lane.ID, []byte("retry after restart"))
	if err != nil {
		t.Fatal(err)
	}
	firstTurn := Turn{ID: "deferred-restart-first-turn", LaneID: lane.ID}
	if _, err := engine.AcceptTurnAndMarkDispatching(receipt.ReceiptID, lane, firstTurn, "deferred-restart-first-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RecoverAcceptedTurnAndRequeue(receipt.ReceiptID, "restart before native I/O"); err != nil {
		t.Fatal(err)
	}
	queued, found, err := engine.EarliestQueued(lane.ID)
	if err != nil || !found || queued.ReceiptID != receipt.ReceiptID {
		t.Fatalf("unbound first input was not addressable by lane ID: %+v found=%v err=%v", queued, found, err)
	}
	recovered, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Catalog.Lanes[lane.ID].NativeSessionID != "" ||
		recovered.Catalog.Turns[firstTurn.ID].State != "terminal" || recovered.Catalog.LaneInputs[receipt.ReceiptID].State != ReceiptQueued {
		t.Fatalf("unbound restart fabricated native state: %+v", recovered.Catalog)
	}
	secondTurn := Turn{ID: "deferred-restart-second-turn", LaneID: lane.ID}
	if _, err := engine.AcceptTurnAndMarkDispatching(receipt.ReceiptID, recovered.Catalog.Lanes[lane.ID], secondTurn, "deferred-restart-second-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.MarkInjectedAndSetNativeDispatch(receipt.ReceiptID, NativeAcceptanceRef{
		NativeSessionID: "session-created-on-redispatch", NativeMessageID: "native-redispatch-turn", AcceptedAt: receipt.AcceptedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInitialLaneCreateStagesQueuedThenAtomicallyAcceptsTurnAndDispatchIntent(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane := Lane{ID: "initial-lane", Product: "claude", NativeSessionID: "initial-native", State: "idle"}
	turn := Turn{ID: "initial-turn", LaneID: lane.ID}
	receipt, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-initial-receipt", lane, turn, "initial-attempt", []byte("initial body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+2 || after.Catalog.Lanes[lane.ID].State != "idle" ||
		after.Catalog.Turns[turn.ID].State != "accepted" || receipt.State != ReceiptDispatching ||
		receipt.TargetTurnID != turn.ID || receipt.Sequence != 1 || receipt.Revision != 2 {
		t.Fatalf("atomic initial acceptance: before=%d after=%d lane=%+v turn=%+v receipt=%+v",
			before.Revision, after.Revision, after.Catalog.Lanes[lane.ID], after.Catalog.Turns[turn.ID], receipt)
	}
	replayed, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-initial-receipt", lane, Turn{ID: "ignored-replay-turn", LaneID: lane.ID}, "ignored-replay-attempt", []byte("initial body"),
	)
	if err != nil || replayed.ReceiptID != receipt.ReceiptID || replayed.Sequence != receipt.Sequence {
		t.Fatalf("stable initial replay = %+v err=%v", replayed, err)
	}
}

func TestInitialLaneStableRetryAfterQueuedCommitUsesExactReceiptWithoutDuplicate(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	crash := errors.New("crash after queued commit")
	engine.afterQueuedCommit = func() error { return crash }
	lane := Lane{ID: "stable-initial-lane", Product: "claude", NativeSessionID: "stable-initial-native"}
	body := []byte("stable initial body")
	queued, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-stable-initial-receipt", lane, Turn{ID: "unused-crash-turn", LaneID: lane.ID}, "unused-crash-attempt", body,
	)
	if !errors.Is(err, crash) || queued.State != ReceiptQueued || queued.Revision != 1 {
		t.Fatalf("queued crash boundary receipt=%+v err=%v", queued, err)
	}
	staged, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if staged.Catalog.Lanes[lane.ID].State != "idle" || staged.Catalog.Turns["unused-crash-turn"].ID != "" || len(staged.Catalog.LaneInputs) != 1 {
		t.Fatalf("staged initial state lane=%+v turns=%+v receipts=%+v", staged.Catalog.Lanes[lane.ID], staged.Catalog.Turns, staged.Catalog.LaneInputs)
	}
	engine.afterQueuedCommit = nil
	replayed, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-stable-initial-receipt", lane, Turn{ID: "stable-retry-turn", LaneID: lane.ID}, "stable-retry-attempt", body,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != queued {
		t.Fatalf("queued replay changed receipt: queued=%+v replayed=%+v", queued, replayed)
	}
	dispatching, err := engine.AcceptStagedTurnAndMarkDispatching(
		queued.ReceiptID, lane, Turn{ID: "stable-retry-turn", LaneID: lane.ID}, "stable-retry-attempt",
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if dispatching.ReceiptID != queued.ReceiptID || dispatching.Sequence != queued.Sequence ||
		dispatching.SpoolObjectID != queued.SpoolObjectID || dispatching.State != ReceiptDispatching ||
		len(after.Catalog.LaneInputs) != 1 || len(after.Catalog.Turns) != len(staged.Catalog.Turns)+1 ||
		after.Catalog.Turns["stable-retry-turn"].State != "accepted" {
		t.Fatalf("stable retry receipt=%+v turns=%+v receipts=%+v", dispatching, after.Catalog.Turns, after.Catalog.LaneInputs)
	}
}

func TestAcceptedIdleCrashTerminalizesTurnAndRequeuesExactReceiptAtomically(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	lane := Lane{ID: "crash-lane", Product: "qwen", State: "idle"}
	turn := Turn{ID: "crash-turn", LaneID: lane.ID}
	receipt, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-crash-receipt", lane, turn, "crash-attempt", []byte("body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	requeued, err := engine.RecoverAcceptedTurnAndRequeue(receipt.ReceiptID, "daemon restarted before native I/O")
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	terminal := after.Catalog.Turns[turn.ID]
	if after.Revision != before.Revision+1 || requeued.State != ReceiptQueued || requeued.TargetTurnID != "" ||
		after.Catalog.Lanes[lane.ID].State != "terminal" || terminal.State != "terminal" ||
		terminal.Outcome != "interrupted" || terminal.Diagnostic == "" {
		t.Fatalf("accepted crash recovery: before=%d after=%d lane=%+v turn=%+v receipt=%+v",
			before.Revision, after.Revision, after.Catalog.Lanes[lane.ID], terminal, requeued)
	}
}

func TestStagedCommandIsInertUntilExactReplayClaim(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	engine.afterQueuedCommit = func() error { return errors.New("stop after stage") }
	lane := Lane{ID: "staged-inert-lane", ParentAttachmentID: "owner", Product: "qwen"}
	queued, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-staged-inert", lane, Turn{ID: "discarded-turn", LaneID: lane.ID}, "discarded-attempt", []byte("body"),
	)
	if err == nil || queued.State != ReceiptQueued || queued.Revision != 1 {
		t.Fatalf("staged receipt=%+v err=%v", queued, err)
	}
	if _, err := engine.AcceptTurnAndMarkDispatching(
		queued.ReceiptID, lane, Turn{ID: "ordinary-turn", LaneID: lane.ID}, "ordinary-attempt",
	); err == nil || !strings.Contains(err.Error(), "not dispatch eligible") {
		t.Fatalf("ordinary staged claim error=%v", err)
	}
	snapshot, readErr := store.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if snapshot.Catalog.Turns["ordinary-turn"].ID != "" || snapshot.Catalog.Turns["discarded-turn"].ID != "" ||
		snapshot.Catalog.LaneInputs[queued.ReceiptID].State != ReceiptQueued {
		t.Fatalf("staged command became dispatch eligible: turns=%+v receipt=%+v", snapshot.Catalog.Turns, snapshot.Catalog.LaneInputs[queued.ReceiptID])
	}
	claimed, err := engine.AcceptStagedTurnAndMarkDispatching(
		queued.ReceiptID, lane, Turn{ID: "replay-turn", LaneID: lane.ID}, "replay-attempt",
	)
	if err != nil || claimed.State != ReceiptDispatching || claimed.TargetTurnID != "replay-turn" {
		t.Fatalf("exact replay claim=%+v err=%v", claimed, err)
	}
}

func TestQueuedLaneInputClaimAndFreshResumeEnforceFIFO(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	first, err := engine.AdmitWithID("fifo-first", "lane", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.AdmitWithID("fifo-second", "lane", []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane := snapshot.Catalog.Lanes["lane"]
	if _, err := engine.AcceptTurnAndMarkDispatching(
		second.ReceiptID, lane, Turn{ID: "second-turn", LaneID: lane.ID}, "second-attempt",
	); !errors.Is(err, ErrLaneInputEarlierQueued) {
		t.Fatalf("out-of-order claim error=%v", err)
	}
	if _, err := engine.UpdateLaneAdmitAndMarkDispatching(
		"command-fresh-resume", lane, Turn{ID: "resume-turn", LaneID: lane.ID}, "resume-attempt", []byte("resume"),
	); !errors.Is(err, ErrLaneInputEarlierQueued) {
		t.Fatalf("fresh resume with older queued error=%v", err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Catalog.LaneInputs[first.ReceiptID].State != ReceiptQueued || len(after.Catalog.LaneInputs) != 2 ||
		after.Catalog.Turns["second-turn"].ID != "" || after.Catalog.Turns["resume-turn"].ID != "" {
		t.Fatalf("FIFO rejection mutated state: receipts=%+v turns=%+v", after.Catalog.LaneInputs, after.Catalog.Turns)
	}
}

func TestRetireStagedLaneArchivesReceiptAndLane(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	engine.afterQueuedCommit = func() error { return errors.New("stop after stage") }
	lane := Lane{ID: "abandoned-stage", ParentAttachmentID: "owner", Product: "claude"}
	queued, err := engine.CreateLaneAdmitAndMarkDispatching(
		"command-abandoned-stage", lane, Turn{ID: "unused-turn", LaneID: lane.ID}, "unused-attempt", []byte("body"),
	)
	if err == nil {
		t.Fatal("expected staged boundary failure")
	}
	retired, err := engine.RetireStagedLane(queued.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != ReceiptRetired || snapshot.Catalog.Lanes[lane.ID].State != "archived" || snapshot.Catalog.Turns["unused-turn"].ID != "" {
		t.Fatalf("staged retirement lane=%+v receipt=%+v turns=%+v", snapshot.Catalog.Lanes[lane.ID], retired, snapshot.Catalog.Turns)
	}
	if _, err := os.Stat(filepath.Join(spoolRoot, queued.SpoolObjectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged spool still exists: %v", err)
	}
}

func TestResumeLaneUpdateStagesQueuedThenAtomicallyAcceptsTurnAndDispatchIntent(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	lane := catalog.Lanes["lane"]
	lane.State = "terminal"
	catalog.Lanes[lane.ID] = lane
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane.Name, lane.State = "updated", "idle"
	receipt, err := engine.UpdateLaneAdmitAndMarkDispatching(
		"command-resume-receipt", lane, Turn{ID: "resume-turn", LaneID: lane.ID}, "resume-attempt", []byte("resume body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+2 || after.Catalog.Lanes[lane.ID].Name != "updated" ||
		after.Catalog.Turns["resume-turn"].State != "accepted" || receipt.State != ReceiptDispatching ||
		receipt.TargetTurnID != "resume-turn" {
		t.Fatalf("atomic resume acceptance: before=%d after=%d lane=%+v turn=%+v receipt=%+v",
			before.Revision, after.Revision, after.Catalog.Lanes[lane.ID], after.Catalog.Turns["resume-turn"], receipt)
	}
}

func TestResumeLaneStableRetryAfterQueuedCommitUsesExactReceiptWithoutDuplicate(t *testing.T) {
	engine, store, _ := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	lane := catalog.Lanes["lane"]
	lane.State = "terminal"
	catalog.Lanes[lane.ID] = lane
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("crash after resume queued commit")
	engine.afterQueuedCommit = func() error { return crash }
	body := []byte("stable resume body")
	queued, err := engine.UpdateLaneAdmitAndMarkDispatching(
		"command-stable-resume-receipt", lane, Turn{ID: "unused-resume-turn", LaneID: lane.ID}, "unused-resume-attempt", body,
	)
	if !errors.Is(err, crash) || queued.State != ReceiptQueued || queued.Revision != 1 {
		t.Fatalf("queued resume crash boundary receipt=%+v err=%v", queued, err)
	}
	staged, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if staged.Catalog.Lanes[lane.ID].State != "terminal" || staged.Catalog.Turns["unused-resume-turn"].ID != "" ||
		len(staged.Catalog.LaneInputs) != 1 {
		t.Fatalf("staged resume state lane=%+v turns=%+v receipts=%+v", staged.Catalog.Lanes[lane.ID], staged.Catalog.Turns, staged.Catalog.LaneInputs)
	}
	engine.afterQueuedCommit = nil
	replayed, err := engine.UpdateLaneAdmitAndMarkDispatching(
		"command-stable-resume-receipt", lane, Turn{ID: "stable-resume-turn", LaneID: lane.ID}, "stable-resume-attempt", body,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != queued {
		t.Fatalf("queued replay changed receipt: queued=%+v replayed=%+v", queued, replayed)
	}
	dispatching, err := engine.AcceptStagedTurnAndMarkDispatching(
		queued.ReceiptID, lane, Turn{ID: "stable-resume-turn", LaneID: lane.ID}, "stable-resume-attempt",
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if dispatching.ReceiptID != queued.ReceiptID || dispatching.Sequence != queued.Sequence ||
		dispatching.SpoolObjectID != queued.SpoolObjectID || dispatching.State != ReceiptDispatching ||
		len(after.Catalog.LaneInputs) != 1 || after.Catalog.Turns["stable-resume-turn"].State != "accepted" {
		t.Fatalf("stable resume retry receipt=%+v turns=%+v receipts=%+v", dispatching, after.Catalog.Turns, after.Catalog.LaneInputs)
	}
}

func TestLaneInputChangedObjectIsPreservedAsCleanupDebt(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	receipt, err := engine.Admit("lane", []byte("owned"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spoolRoot, receipt.SpoolObjectID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Retire(receipt.ReceiptID); !errors.Is(err, ErrLaneInputCleanupDebt) {
		t.Fatalf("retire changed object error=%v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "unrelated" {
		t.Fatalf("unrelated object was changed: %q err=%v", got, err)
	}
	snapshot, _ := store.Read()
	if len(snapshot.Catalog.CleanupDebts) != 1 || snapshot.Catalog.LaneInputs[receipt.ReceiptID].State == ReceiptRetired {
		t.Fatalf("cleanup debt not preserved: receipts=%+v debts=%+v", snapshot.Catalog.LaneInputs, snapshot.Catalog.CleanupDebts)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	retired, err := engine.Retire(receipt.ReceiptID)
	if err != nil || retired.State != ReceiptRetired {
		t.Fatalf("debt resolution retirement=%+v err=%v", retired, err)
	}
	snapshot, _ = store.Read()
	if _, ok := snapshot.Catalog.CleanupDebts[laneInputDebtID(receipt.ReceiptID)]; ok {
		t.Fatalf("resolved cleanup debt remains: %+v", snapshot.Catalog.CleanupDebts)
	}
}

func TestLaneInputRecoveryPreservesQueuedAndReportsDispatchingWithoutReplay(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	queued, _ := engine.Admit("lane", []byte("queued"))
	dispatching, _ := engine.Admit("lane", []byte("dispatching"))
	if _, err := engine.MarkDispatching(dispatching.ReceiptID, "turn", "attempt"); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Dispatching) != 1 || report.Dispatching[0].ReceiptID != dispatching.ReceiptID || report.Queued != 1 {
		t.Fatalf("recovery report=%+v", report)
	}
	if entries, _ := os.ReadDir(spoolRoot); len(entries) != 2 {
		t.Fatalf("recovery removed active bodies: %v", entries)
	}
	snapshot, _ := store.Read()
	if snapshot.Catalog.LaneInputs[queued.ReceiptID].State != ReceiptQueued || snapshot.Catalog.LaneInputs[dispatching.ReceiptID].State != ReceiptDispatching {
		t.Fatalf("recovery replayed or rewrote receipts: %+v", snapshot.Catalog.LaneInputs)
	}
}

func TestLaneInputOpenRejectsSymlinkAndIdentityReplacement(t *testing.T) {
	engine, _, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	receipt, _ := engine.Admit("lane", []byte("owned"))
	path := filepath.Join(spoolRoot, receipt.SpoolObjectID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(spoolRoot, "target")
	if err := os.WriteFile(target, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.OpenVerified(receipt.ReceiptID); err == nil {
		t.Fatal("symlinked spool object was accepted")
	}
}

func TestLaneInputOpenRejectsDigestMutationOnSameInode(t *testing.T) {
	engine, _, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	receipt, _ := engine.Admit("lane", []byte("owned"))
	path := filepath.Join(spoolRoot, receipt.SpoolObjectID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("other")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.OpenVerified(receipt.ReceiptID); err == nil {
		t.Fatal("same-inode digest mutation was accepted")
	}
}

func TestLaneInputRecoveryRetiresInjectedAndDebtsMissingQueued(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	injected, _ := engine.Admit("lane", []byte("injected"))
	if _, err := engine.MarkDispatching(injected.ReceiptID, "turn", "injected-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.MarkInjected(injected.ReceiptID, NativeAcceptanceRef{NativeSessionID: "native", AcceptedAt: 100}); err != nil {
		t.Fatal(err)
	}
	missing, _ := engine.Admit("lane", []byte("missing"))
	snapshot, _ := store.Read()
	if err := os.Remove(filepath.Join(spoolRoot, snapshot.Catalog.LaneInputs[missing.ReceiptID].SpoolObjectID)); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Recover()
	if err != nil || report.ObjectsRetired != 1 || len(report.CleanupDebtIDs) != 1 {
		t.Fatalf("recover report=%+v err=%v", report, err)
	}
	snapshot, _ = store.Read()
	if snapshot.Catalog.LaneInputs[injected.ReceiptID].State != ReceiptRetired || snapshot.Catalog.LaneInputs[missing.ReceiptID].State != ReceiptQueued {
		t.Fatalf("unexpected recovered receipt states: %+v", snapshot.Catalog.LaneInputs)
	}
	if _, ok := snapshot.Catalog.CleanupDebts[laneInputDebtID(missing.ReceiptID)]; !ok {
		t.Fatalf("missing queued object did not become cleanup debt: %+v", snapshot.Catalog.CleanupDebts)
	}
}

func TestLaneInputRecoveryPreservesSuspiciousOrphanAndRecordsDebt(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(target, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	trap := filepath.Join(spoolRoot, "spool-malformed")
	if err := os.Symlink(target, trap); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Recover()
	if err != nil || len(report.CleanupDebtIDs) != 1 || report.OrphansRemoved != 0 {
		t.Fatalf("recover report=%+v err=%v", report, err)
	}
	if info, err := os.Lstat(trap); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("suspicious orphan was changed: info=%v err=%v", info, err)
	}
	snapshot, _ := store.Read()
	if _, ok := snapshot.Catalog.CleanupDebts[report.CleanupDebtIDs[0]]; !ok {
		t.Fatalf("orphan debt missing: %+v", snapshot.Catalog.CleanupDebts)
	}
}

func TestLaneInputAdmissionUsesNoFollowExclusiveObjects(t *testing.T) {
	engine, _, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	engine.randomID = func() (string, error) { return "fixed", nil }
	trap := filepath.Join(spoolRoot, ".admit-fixed")
	if err := os.Symlink(filepath.Join(spoolRoot, "victim"), trap); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Admit("lane", []byte("body")); err == nil {
		t.Fatal("exclusive no-follow trap was overwritten")
	}
	var stat unix.Stat_t
	if err := unix.Lstat(trap, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("trap changed: stat=%+v err=%v", stat, err)
	}
}

func TestLaneInputConcurrentEnginesCommitUniqueMonotonicSequences(t *testing.T) {
	engine, store, spoolRoot := newLaneInputTestEngine(t, DefaultLaneInputLimits())
	second, err := NewLaneInputEngine(store, spoolRoot, DefaultLaneInputLimits())
	if err != nil {
		t.Fatal(err)
	}
	const admissions = 16
	errorsSeen := make(chan error, admissions)
	var group sync.WaitGroup
	for index := 0; index < admissions; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			candidate := engine
			if index%2 != 0 {
				candidate = second
			}
			_, err := candidate.Admit("lane", []byte{byte(index)})
			errorsSeen <- err
		}(index)
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Catalog.LaneInputs) != admissions || snapshot.Catalog.Lanes["lane"].InputSequence != admissions {
		t.Fatalf("concurrent admission state: count=%d highwater=%d", len(snapshot.Catalog.LaneInputs), snapshot.Catalog.Lanes["lane"].InputSequence)
	}
	sequences := make(map[uint64]bool, admissions)
	for _, receipt := range snapshot.Catalog.LaneInputs {
		if sequences[receipt.Sequence] {
			t.Fatalf("duplicate sequence %d", receipt.Sequence)
		}
		sequences[receipt.Sequence] = true
	}
}
