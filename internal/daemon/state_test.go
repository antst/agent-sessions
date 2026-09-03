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

func TestPrepareStateRootRemovesEveryNonCurrentSafeRoot(t *testing.T) {
	legacyCatalog := `{"schema":1,"revision":9,"records":{"catalog":{"attachments":{},"deliveries":{},"turns":{},"lanes":{}}}}`
	tests := []struct {
		name   string
		setup  func(*testing.T, string)
		reason string
	}{
		{"spec shape", func(t *testing.T, root string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(root, "native"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(legacyCatalog), 0o600); err != nil {
				t.Fatal(err)
			}
		}, `unknown entry "native"`},
		{"pdev shape", func(t *testing.T, root string) {
			t.Helper()
			for _, name := range []string{"agents", "native", "sessions"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(legacyCatalog), 0o600); err != nil {
				t.Fatal(err)
			}
		}, `unknown entry "agents"`},
		{"garbage", func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "random-file"), []byte("garbage"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, `unknown entry "random-file"`},
		{"schema zero", func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"schema":0,"records":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a current one-table store"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			var output bytes.Buffer
			if err := PrepareStateRoot(root, 1<<20, &output); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incompatible root survived: %v", err)
			}
			if !strings.Contains(output.String(), root) || !strings.Contains(output.String(), test.reason) {
				t.Fatalf("removal diagnostic = %q", output.String())
			}
		})
	}
}

func TestPrepareStateRootLeavesCurrentAndEmptyRootsUntouched(t *testing.T) {
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

	for _, empty := range []struct {
		name string
		run  bool
	}{
		{"empty", false},
		{"runtime and lock only", true},
	} {
		t.Run(empty.name, func(t *testing.T) {
			emptyRoot := t.TempDir()
			if empty.run {
				if err := os.Mkdir(filepath.Join(emptyRoot, "run"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(emptyRoot, "state.lock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := PrepareStateRoot(emptyRoot, 1<<20, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(emptyRoot); err != nil {
				t.Fatalf("compatible empty root was removed: %v", err)
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
