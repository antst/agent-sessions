package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/statestore"
)

func TestLifecycleTransitionsRemainProductNeutral(t *testing.T) {
	for _, test := range []struct {
		kind     string
		from, to string
		want     bool
	}{
		{kind: "attachment", from: "preparing", to: "prepared", want: true},
		{kind: "attachment", from: "attached", to: "detaching", want: true},
		{kind: "attachment", from: "detached", to: "attached"},
		{kind: "lane", from: "preparing", to: "idle", want: true},
		{kind: "lane", from: "idle", to: "running", want: true},
		{kind: "lane", from: "terminal", to: "archived", want: true},
		{kind: "lane", from: "archived", to: "running"},
	} {
		t.Run(test.kind+"/"+test.from+"/"+test.to, func(t *testing.T) {
			if got := ValidLifecycleTransition(test.kind, test.from, test.to); got != test.want {
				t.Fatalf("ValidLifecycleTransition(%q,%q,%q) = %v, want %v", test.kind, test.from, test.to, got, test.want)
			}
		})
	}
}

func TestStateStoreRoundTripsLaneWithoutTurnOrInputState(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{
		Host: HostRuntime{User: "1000", Host: "pdev", Generation: 7},
		Lanes: map[string]Lane{
			"lane": {ID: "lane", ParentAttachmentID: "parent", Product: "codex", NativeSessionID: "native", State: "idle", Groups: []string{"project"}},
		},
	}
	committed, err := store.Commit(0, catalog)
	if err != nil {
		t.Fatal(err)
	}
	caller := committed.Catalog
	lane := caller.Lanes["lane"]
	lane.Groups[0] = "mutated"
	caller.Lanes["lane"] = lane
	loaded, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Catalog.Lanes["lane"].Groups[0]; got != "project" {
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
	if snapshot.Catalog.Attachments == nil || snapshot.Catalog.ComponentBindings == nil ||
		snapshot.Catalog.ComponentSessions == nil {
		t.Fatalf("remaining maps were not normalized: %#v", snapshot.Catalog)
	}
}

func TestStateStoreRejectsNativeSessionRewriteAndStaleRevision(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Commit(0, Catalog{Lanes: map[string]Lane{
		"lane": {ID: "lane", Product: "codex", NativeSessionID: "native-a", State: "idle"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mutated := first.Catalog
	lane := mutated.Lanes["lane"]
	lane.NativeSessionID = "native-b"
	mutated.Lanes["lane"] = lane
	if _, err := store.Commit(first.Revision, mutated); err == nil {
		t.Fatal("native session rewrite was accepted")
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
