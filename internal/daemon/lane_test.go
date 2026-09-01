package daemon

import (
	"testing"
	"time"
)

func TestLaneEngineStoresOnlyLaneRoutingState(t *testing.T) {
	engine, store := testLaneEngine(t)
	lane := testDurableLane("lane", "codex")
	if err := engine.Create(lane); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	stored := snapshot.Catalog.Lanes[lane.ID]
	if stored.ID != lane.ID || stored.Product != lane.Product || stored.State != "idle" {
		t.Fatalf("stored lane = %+v", stored)
	}
}

func TestLaneEngineUpdatePreservesNativeSession(t *testing.T) {
	engine, store := testLaneEngine(t)
	lane := testDurableLane("lane", "codex")
	lane.NativeSessionID = "native-a"
	if err := engine.Create(lane); err != nil {
		t.Fatal(err)
	}
	omitted := lane
	omitted.NativeSessionID = ""
	if err := engine.Update(omitted); err != nil {
		t.Fatal(err)
	}
	conflicting := lane
	conflicting.NativeSessionID = "native-b"
	if err := engine.Update(conflicting); err == nil {
		t.Fatal("Update replaced the product-owned native session UUID")
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Catalog.Lanes[lane.ID].NativeSessionID; got != "native-a" {
		t.Fatalf("native session = %q", got)
	}
}

func TestLaneEngineTransitionsWithoutTurnState(t *testing.T) {
	engine, store := testLaneEngine(t)
	lane := testDurableLane("lane", "qwen")
	if err := engine.Create(lane); err != nil {
		t.Fatal(err)
	}
	if err := engine.TransitionLane(lane.ID, "running", "capability"); err != nil {
		t.Fatal(err)
	}
	if err := engine.TransitionLane(lane.ID, "terminal", ""); err != nil {
		t.Fatal(err)
	}
	if err := engine.TransitionLane(lane.ID, "archived", ""); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	stored := snapshot.Catalog.Lanes[lane.ID]
	if stored.State != "archived" || stored.CapabilityHash != "" || stored.ArchiveRevision != 1 {
		t.Fatalf("archived lane = %+v", stored)
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
