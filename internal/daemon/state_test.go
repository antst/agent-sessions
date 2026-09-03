package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/statestore"
)

func TestPrepareStateRootDeletesOnlyExactLegacyShape(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"agents", "native", "sessions"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := map[string]any{
		"schema": 1, "revision": 9, "records": map[string]any{"catalog": map[string]any{
			"attachments": map[string]any{}, "deliveries": map[string]any{},
			"turns": map[string]any{}, "lanes": map[string]any{},
		}},
	}
	body, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, "state.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := PrepareStateRoot(root, 1<<20, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy root survived: %v", err)
	}
	if !strings.Contains(output.String(), "Removed incompatible pre-0.4.0") || !strings.Contains(output.String(), root) {
		t.Fatalf("removal diagnostic = %q", output.String())
	}
}

func TestPrepareStateRootLeavesOneTableBytesUntouchedAndFailsClosedOtherwise(t *testing.T) {
	root := t.TempDir()
	store, err := OpenState(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(0, Catalog{Lanes: map[string]LaneCandidate{}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareStateRoot(root, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("one-table root was rewritten")
	}

	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "state.json"), []byte(`{"schema":1,"records":{"catalog":{"attachments":{},"deliveries":{},"turns":{},"lanes":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agents", "native", "sessions"} {
		if err := os.Mkdir(filepath.Join(broken, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(broken, "user-file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStateRoot(broken, 1<<20, nil); err == nil {
		t.Fatal("ambiguous root did not fail closed")
	}
	if _, err := os.Stat(filepath.Join(broken, "user-file")); err != nil {
		t.Fatalf("ambiguous root was altered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(broken, "state.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous root was mutated with a lock file: %v", err)
	}

	currentWithUnknownEntry := t.TempDir()
	unknownStore, err := OpenState(currentWithUnknownEntry, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unknownStore.Commit(0, Catalog{Lanes: map[string]LaneCandidate{}}); err != nil {
		t.Fatal(err)
	}
	unknownState := filepath.Join(currentWithUnknownEntry, "state.json")
	unknownBefore, err := os.ReadFile(unknownState)
	if err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(currentWithUnknownEntry, "user-file")
	if err := os.WriteFile(userFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStateRoot(currentWithUnknownEntry, 1<<20, nil); err == nil {
		t.Fatal("one-table root with an unknown entry did not fail closed")
	}
	unknownAfter, err := os.ReadFile(unknownState)
	if err != nil || !bytes.Equal(unknownBefore, unknownAfter) {
		t.Fatalf("ambiguous one-table root was altered: %v", err)
	}
	if body, err := os.ReadFile(userFile); err != nil || string(body) != "keep" {
		t.Fatalf("unknown entry was altered: %q, %v", body, err)
	}
}

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
