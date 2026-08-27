package statestore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

type storeTestRecord struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestStoreRoleNeutralAtomicRecord(t *testing.T) {
	root := t.TempDir()
	store := openStoreTest(t, root, Options{})
	key := "arbitrary/widgets/primary"
	first := storeTestRecord{Name: "first", Count: 1}

	revision, err := store.CompareAndSwap(context.Background(), key, 0, first)
	if err != nil {
		t.Fatalf("create role-neutral record: %v", err)
	}
	if revision != 1 {
		t.Fatalf("initial revision = %d, want 1", revision)
	}
	assertStoredRecord(t, store, key, revision, first)

	path := store.RecordPath(key)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat committed record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %o, want 600", info.Mode().Perm())
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat store root: %v", err)
	}
	if rootInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("store root mode = %o, want owner-only", rootInfo.Mode().Perm())
	}

	readerErrors := make(chan error, 1)
	stopReaders := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stopReaders:
				return
			default:
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				readerErrors <- readErr
				return
			}
			var envelope struct {
				SchemaVersion int             `json:"schema_version"`
				Revision      Revision        `json:"revision"`
				Value         storeTestRecord `json:"value"`
			}
			if decodeErr := json.Unmarshal(body, &envelope); decodeErr != nil {
				readerErrors <- decodeErr
				return
			}
			if envelope.SchemaVersion != RecordSchemaVersion || envelope.Revision == 0 || envelope.Value.Name == "" {
				readerErrors <- errors.New("reader observed an incomplete record envelope")
				return
			}
		}
	}()

	current := revision
	for count := 2; count <= 64; count++ {
		current, err = store.CompareAndSwap(context.Background(), key, current, storeTestRecord{Name: "next", Count: count})
		if err != nil {
			close(stopReaders)
			readers.Wait()
			t.Fatalf("atomic update %d: %v", count, err)
		}
	}
	close(stopReaders)
	readers.Wait()
	select {
	case err := <-readerErrors:
		t.Fatalf("concurrent raw reader observed non-atomic state: %v", err)
	default:
	}
	assertStoredRecord(t, store, key, current, storeTestRecord{Name: "next", Count: 64})
}

func TestStoreCompareAndSwapRejectsStaleRevision(t *testing.T) {
	store := openStoreTest(t, t.TempDir(), Options{})
	key := "records/cas"
	revision, err := store.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "original"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.CompareAndSwap(context.Background(), key, revision+1, storeTestRecord{Name: "stale-writer"})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale CAS error = %v, want %v", err, ErrRevisionConflict)
	}
	assertStoredRecord(t, store, key, revision, storeTestRecord{Name: "original"})
}

func TestStoreCompareAndSwapCommitsExactlyOneConcurrentWriter(t *testing.T) {
	store := openStoreTest(t, t.TempDir(), Options{})
	key := "records/concurrent-cas"
	revision, err := store.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "original"})
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"writer-a", "writer-b"} {
		name := name
		go func() {
			<-ready
			_, updateErr := store.CompareAndSwap(context.Background(), key, revision, storeTestRecord{Name: name})
			results <- updateErr
		}()
	}
	close(ready)

	var successes, conflicts int
	for range 2 {
		switch updateErr := <-results; {
		case updateErr == nil:
			successes++
		case errors.Is(updateErr, ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent CAS returned unexpected error: %v", updateErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent CAS successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}

	var got storeTestRecord
	gotRevision, err := store.Read(context.Background(), key, &got)
	if err != nil {
		t.Fatal(err)
	}
	if gotRevision != revision+1 || (got.Name != "writer-a" && got.Name != "writer-b") {
		t.Fatalf("winning record = revision %d, value %+v; want one complete writer at revision %d", gotRevision, got, revision+1)
	}
}

func TestStoreRejectsCorruption(t *testing.T) {
	for name, replacement := range map[string]string{
		"malformed JSON":     `{"schema_version":1,`,
		"unsupported schema": `{"schema_version":999,"revision":1,"value":{"name":"bad","count":1}}`,
		"zero revision":      `{"schema_version":1,"revision":0,"value":{"name":"bad","count":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := openStoreTest(t, t.TempDir(), Options{})
			key := "records/corrupt"
			if _, err := store.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "valid"}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.RecordPath(key), []byte(replacement), 0o600); err != nil {
				t.Fatal(err)
			}
			var got storeTestRecord
			if _, err := store.Read(context.Background(), key, &got); !errors.Is(err, ErrCorruptRecord) {
				t.Fatalf("read corrupted record error = %v, want %v", err, ErrCorruptRecord)
			}
		})
	}
}

func TestStoreRejectsSymlinkAndChangedType(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		store := openStoreTest(t, root, Options{})
		key := "records/link"
		path := store.RecordPath(key)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "outside.json")
		if err := os.WriteFile(target, []byte(`{"schema_version":1,"revision":1,"value":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		var got storeTestRecord
		if _, err := store.Read(context.Background(), key, &got); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("read symlink error = %v, want %v", err, ErrUnsafeRecord)
		}
	})

	t.Run("changed to directory", func(t *testing.T) {
		store := openStoreTest(t, t.TempDir(), Options{})
		key := "records/type-change"
		if _, err := store.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "valid"}); err != nil {
			t.Fatal(err)
		}
		path := store.RecordPath(key)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var got storeTestRecord
		if _, err := store.Read(context.Background(), key, &got); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("read changed-type record error = %v, want %v", err, ErrUnsafeRecord)
		}
	})
}

func TestStoreRejectsRecordPathsOutsideOwnedRoot(t *testing.T) {
	t.Run("traversal", func(t *testing.T) {
		root := t.TempDir()
		store := openStoreTest(t, root, Options{})
		key := "records/../../escaped"
		escaped := filepath.Join(filepath.Dir(root), "escaped.json")
		_ = os.Remove(escaped)

		var got storeTestRecord
		if _, err := store.Read(context.Background(), key, &got); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("traversal read error = %v, want %v", err, ErrUnsafeRecord)
		}
		if _, err := store.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "escape"}); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("traversal CAS error = %v, want %v", err, ErrUnsafeRecord)
		}
		if _, err := os.Lstat(escaped); !os.IsNotExist(err) {
			t.Fatalf("traversal CAS touched %q: %v", escaped, err)
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		store := openStoreTest(t, root, Options{})
		key := filepath.Join(outside, "escaped")

		var got storeTestRecord
		if _, err := store.Read(context.Background(), key, &got); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("absolute-path read error = %v, want %v", err, ErrUnsafeRecord)
		}
		if _, err := store.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "escape"}); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("absolute-path CAS error = %v, want %v", err, ErrUnsafeRecord)
		}
		if _, err := os.Lstat(filepath.Join(outside, "escaped.json")); !os.IsNotExist(err) {
			t.Fatalf("absolute-path CAS touched outside root: %v", err)
		}
	})

	t.Run("symlinked intermediate directory", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		store := openStoreTest(t, root, Options{})
		if err := os.Symlink(outside, filepath.Join(root, "records")); err != nil {
			t.Fatal(err)
		}
		outsideRecord := filepath.Join(outside, "escaped.json")
		if err := os.WriteFile(outsideRecord, []byte(`{"schema_version":1,"revision":1,"value":{"name":"outside","count":1}}`), 0o600); err != nil {
			t.Fatal(err)
		}

		var got storeTestRecord
		if _, err := store.Read(context.Background(), "records/escaped", &got); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("intermediate-symlink read error = %v, want %v", err, ErrUnsafeRecord)
		}
		if _, err := store.CompareAndSwap(context.Background(), "records/new", 0, storeTestRecord{Name: "escape"}); !errors.Is(err, ErrUnsafeRecord) {
			t.Fatalf("intermediate-symlink CAS error = %v, want %v", err, ErrUnsafeRecord)
		}
		if _, err := os.Lstat(filepath.Join(outside, "new.json")); !os.IsNotExist(err) {
			t.Fatalf("intermediate-symlink CAS touched outside root: %v", err)
		}
	})
}

func TestStoreResourceExhaustionPreservesCommittedRevision(t *testing.T) {
	for name, pointAndError := range map[string]struct {
		point FaultPoint
		err   error
	}{
		"disk full":             {point: FaultWriteTemporary, err: syscall.ENOSPC},
		"file descriptor limit": {point: FaultCreateTemporary, err: syscall.EMFILE},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			base := openStoreTest(t, root, Options{})
			key := "records/resource"
			revision, err := base.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "committed"})
			if err != nil {
				t.Fatal(err)
			}

			fault := pointAndError
			failing := openStoreTest(t, root, Options{InjectFault: func(point FaultPoint) error {
				if point == fault.point {
					return fault.err
				}
				return nil
			}})
			if _, err := failing.CompareAndSwap(context.Background(), key, revision, storeTestRecord{Name: "uncommitted"}); !errors.Is(err, fault.err) {
				t.Fatalf("resource exhaustion error = %v, want underlying %v", err, fault.err)
			}

			reopened := openStoreTest(t, root, Options{})
			assertStoredRecord(t, reopened, key, revision, storeTestRecord{Name: "committed"})
			assertNoTemporaryRecords(t, root)
		})
	}
}

func TestStoreCrashRecoveryAtAtomicCommitBoundaries(t *testing.T) {
	crash := errors.New("simulated process crash")
	for name, testCase := range map[string]struct {
		point         FaultPoint
		wantName      string
		revisionDelta Revision
	}{
		"before rename preserves old revision":  {point: FaultAfterTemporarySync, wantName: "old", revisionDelta: 0},
		"after rename adopts complete revision": {point: FaultAfterRename, wantName: "new", revisionDelta: 1},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			base := openStoreTest(t, root, Options{})
			key := "records/crash"
			revision, err := base.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "old"})
			if err != nil {
				t.Fatal(err)
			}

			injected := false
			crashing := openStoreTest(t, root, Options{InjectFault: func(point FaultPoint) error {
				if !injected && point == testCase.point {
					injected = true
					return crash
				}
				return nil
			}})
			if _, err := crashing.CompareAndSwap(context.Background(), key, revision, storeTestRecord{Name: "new"}); !errors.Is(err, crash) {
				t.Fatalf("injected crash error = %v, want %v", err, crash)
			}
			orphan := filepath.Join(filepath.Dir(crashing.RecordPath(key)), ".agent-sessions-crash.tmp")
			if err := os.WriteFile(orphan, []byte(`{"partial":`), 0o600); err != nil {
				t.Fatalf("write simulated crash residue: %v", err)
			}

			recovered := openStoreTest(t, root, Options{})
			if err := recovered.Recover(context.Background()); err != nil {
				t.Fatalf("recover store: %v", err)
			}
			assertStoredRecord(t, recovered, key, revision+testCase.revisionDelta, storeTestRecord{Name: testCase.wantName})
			assertNoTemporaryRecords(t, root)
		})
	}
}

func TestStoreSyncsParentDirectoryAndSurfacesSyncFailure(t *testing.T) {
	t.Run("successful commit reaches parent sync", func(t *testing.T) {
		var parentSyncs int
		store := openStoreTest(t, t.TempDir(), Options{InjectFault: func(point FaultPoint) error {
			if point == FaultSyncParentDirectory {
				parentSyncs++
			}
			return nil
		}})
		if _, err := store.CompareAndSwap(context.Background(), "records/parent-sync", 0, storeTestRecord{Name: "committed"}); err != nil {
			t.Fatalf("commit record: %v", err)
		}
		if parentSyncs == 0 {
			t.Fatal("commit omitted required parent-directory sync")
		}
	})

	t.Run("parent sync failure is not reported as durable success", func(t *testing.T) {
		root := t.TempDir()
		base := openStoreTest(t, root, Options{})
		key := "records/parent-sync-failure"
		revision, err := base.CompareAndSwap(context.Background(), key, 0, storeTestRecord{Name: "old"})
		if err != nil {
			t.Fatal(err)
		}

		failing := openStoreTest(t, root, Options{InjectFault: func(point FaultPoint) error {
			if point == FaultSyncParentDirectory {
				return syscall.EIO
			}
			return nil
		}})
		if _, err := failing.CompareAndSwap(context.Background(), key, revision, storeTestRecord{Name: "new"}); !errors.Is(err, syscall.EIO) {
			t.Fatalf("parent-directory sync error = %v, want underlying %v", err, syscall.EIO)
		}

		recovered := openStoreTest(t, root, Options{})
		if err := recovered.Recover(context.Background()); err != nil {
			t.Fatalf("recover after parent-sync failure: %v", err)
		}
		assertStoredRecord(t, recovered, key, revision+1, storeTestRecord{Name: "new"})
	})
}

func openStoreTest(t *testing.T, root string, options Options) *Store {
	t.Helper()
	options.Root = root
	if options.MaxRecordBytes == 0 {
		options.MaxRecordBytes = 1 << 20
	}
	store, err := Open(options)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func assertStoredRecord(t *testing.T, store *Store, key string, wantRevision Revision, want storeTestRecord) {
	t.Helper()
	var got storeTestRecord
	revision, err := store.Read(context.Background(), key, &got)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	if revision != wantRevision || got != want {
		t.Fatalf("read %q = revision %d, value %+v; want revision %d, value %+v", key, revision, got, wantRevision, want)
	}
}

func assertNoTemporaryRecords(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".tmp" {
			t.Errorf("temporary record survived recovery: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store root: %v", err)
	}
}
