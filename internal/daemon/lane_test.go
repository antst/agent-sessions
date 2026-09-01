package daemon

import (
	"reflect"
	"testing"
)

func TestLaneEngineRemembersOnlyImmutableOfflineLookupCandidate(t *testing.T) {
	engine, store := testLaneEngine(t)
	candidate := testLaneCandidate()
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatalf("idempotent remember: %v", err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Catalog.Lanes[candidate.NativeSessionID]; !reflect.DeepEqual(got, candidate) {
		t.Fatalf("stored candidate = %+v, want %+v", got, candidate)
	}
}

func TestLaneEngineRejectsCandidateRewrite(t *testing.T) {
	engine, store := testLaneEngine(t)
	candidate := testLaneCandidate()
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	conflicting := candidate
	conflicting.SecondaryGroups = []string{"different"}
	if err := engine.Remember(conflicting); err == nil {
		t.Fatal("Remember rewrote an immutable lookup candidate")
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Catalog.Lanes[candidate.NativeSessionID]; !reflect.DeepEqual(got, candidate) {
		t.Fatalf("candidate changed after rejection: %+v", got)
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

func testLaneCandidate() LaneCandidate {
	return LaneCandidate{
		NativeSessionID: "native", Product: "codex", Parent: "parent",
		PrimaryGroup: "session:host/parent", SecondaryGroups: []string{"project"},
	}
}
