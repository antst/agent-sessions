package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/statestore"
)

func TestStateStoreRoundTripsLaneWithoutTurnOrInputState(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{
		Lanes: map[string]LaneCandidate{
			"native": {
				NativeSessionID: "native", Parent: "parent", Product: "codex",
				PrimaryGroup: "session:host/parent", SecondaryGroups: []string{"project"},
			},
		},
	}
	committed, err := store.Commit(0, catalog)
	if err != nil {
		t.Fatal(err)
	}
	caller := committed.Catalog
	lane := caller.Lanes["native"]
	lane.SecondaryGroups[0] = "mutated"
	caller.Lanes["native"] = lane
	loaded, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Catalog.Lanes["native"].SecondaryGroups[0]; got != "project" {
		t.Fatalf("caller mutation leaked into store: %q", got)
	}
	body, err := json.Marshal(loaded.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, deleted := range []string{"turns", "lane_inputs", "input_sequence"} {
		if json.Valid(body) && containsJSONKey(body, deleted) {
			t.Fatalf("deleted durable key %q remains in %s", deleted, body)
		}
	}
}

func TestStateStoreNormalizesRemainingMaps(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	legacyBody := json.RawMessage(`{"host":{"user":"1000","host":"pdev","generation":1},"lanes":{}}`)
	if _, err := store.store.Commit(0, map[string]json.RawMessage{catalogRecord: legacyBody}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog.Lanes == nil {
		t.Fatalf("remaining maps were not normalized: %#v", snapshot.Catalog)
	}
}

func TestStateStoreRejectsCandidateRewriteAndStaleRevision(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Commit(0, Catalog{Lanes: map[string]LaneCandidate{
		"native-a": {
			NativeSessionID: "native-a", Product: "codex", Parent: "parent",
			PrimaryGroup: "session:host/parent",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mutated := first.Catalog
	lane := mutated.Lanes["native-a"]
	lane.Parent = "other-parent"
	mutated.Lanes["native-a"] = lane
	if _, err := store.Commit(first.Revision, mutated); err == nil {
		t.Fatal("candidate rewrite was accepted")
	}
	if _, err := store.Commit(0, first.Catalog); !errors.Is(err, statestore.ErrConflict) {
		t.Fatalf("stale commit error = %v", err)
	}
}

func containsJSONKey(body []byte, key string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	_, ok := object[key]
	return ok
}
