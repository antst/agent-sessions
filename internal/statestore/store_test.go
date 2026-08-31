package statestore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStoreCommitsBoundedAtomicSnapshotsAndRejectsStaleWriters(t *testing.T) {
	root := t.TempDir()
	first, err := Open(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]json.RawMessage{"host": json.RawMessage(`{"generation":1}`)}
	committed, err := first.Commit(0, records)
	if err != nil || committed.Revision != 1 || !reflect.DeepEqual(committed.Records, records) {
		t.Fatalf("first commit = %+v, %v", committed, err)
	}
	if _, err := second.Commit(0, map[string]json.RawMessage{"host": json.RawMessage(`{"generation":2}`)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale writer error = %v, want ErrConflict", err)
	}
	loaded, err := second.Read()
	if err != nil || loaded.Revision != 1 || !reflect.DeepEqual(loaded.Records, records) {
		t.Fatalf("snapshot after conflict = %+v, %v", loaded, err)
	}
	loaded.Records["host"][0] = 'x'
	again, err := first.Read()
	if err != nil || string(again.Records["host"]) != `{"generation":1}` {
		t.Fatalf("store leaked caller mutation: %+v, %v", again, err)
	}
	if info, err := os.Stat(filepath.Join(root, stateFilename)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file metadata = %+v, %v", info, err)
	}
}

func TestStoreRejectsOversizeAndCrashBeforeReplaceWithoutChangingCommittedState(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, 256)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := store.Commit(0, map[string]json.RawMessage{"value": json.RawMessage(`"baseline"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(baseline.Revision, map[string]json.RawMessage{"value": json.RawMessage(`"` + strings.Repeat("x", 512) + `"`)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize commit error = %v, want ErrTooLarge", err)
	}
	store.beforeReplace = func(string) error { return errors.New("injected crash") }
	if _, err := store.Commit(baseline.Revision, map[string]json.RawMessage{"value": json.RawMessage(`"replacement"`)}); err == nil || err.Error() != "injected crash" {
		t.Fatalf("crash injection error = %v", err)
	}
	store.beforeReplace = nil
	loaded, err := store.Read()
	if err != nil || loaded.Revision != baseline.Revision || string(loaded.Records["value"]) != `"baseline"` {
		t.Fatalf("committed snapshot changed after failed writes: %+v, %v", loaded, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != stateFilename && entry.Name() != lockFilename {
			t.Fatalf("failed commit left artifact %q", entry.Name())
		}
	}
}

func TestStoreRejectsCorruptOrUnboundedSnapshots(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, 128)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, stateFilename)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("corrupt snapshot was accepted")
	}
	if err := os.WriteFile(path, make([]byte, 129), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("unbounded snapshot error = %v, want ErrTooLarge", err)
	}
}
