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

func TestLaneEngineReturnsAllProductCandidateQuestions(t *testing.T) {
	engine, _ := testLaneEngine(t)
	wanted := testLaneCandidate()
	if err := engine.Remember(wanted); err != nil {
		t.Fatal(err)
	}
	other := wanted
	other.NativeSessionID, other.Parent = "other-native", "other-parent"
	if err := engine.Remember(other); err != nil {
		t.Fatal(err)
	}
	differentProduct := wanted
	differentProduct.NativeSessionID, differentProduct.Product = "different-product-native", "claude"
	if err := engine.Remember(differentProduct); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Candidates(wanted.Product)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []LaneCandidate{wanted, other}) {
		t.Fatalf("candidate questions = %+v", got)
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
