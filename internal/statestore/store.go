// Package statestore provides one bounded, revisioned, atomic local state file.
package statestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/pathidentity"
)

const (
	stateFilename = "state.json"
	lockFilename  = "state.lock"
	stateSchema   = 1
)

var (
	// ErrConflict means the caller's expected revision is stale.
	ErrConflict = errors.New("state revision conflict")
	// ErrTooLarge means a persisted or proposed snapshot exceeds its bound.
	ErrTooLarge = errors.New("state snapshot exceeds configured bound")
)

// Snapshot is one isolated committed state revision.
type Snapshot struct {
	Revision uint64
	Records  map[string]json.RawMessage
}

type diskSnapshot struct {
	Schema   int                        `json:"schema"`
	Revision uint64                     `json:"revision"`
	Records  map[string]json.RawMessage `json:"records"`
}

// Store owns one bounded CAS state file under a private no-follow directory.
type Store struct {
	root          string
	path          string
	lockPath      string
	maxBytes      int64
	beforeReplace func(string) error
}

// Open validates or creates a private store root. maxBytes bounds both reads
// and commits.
func Open(root string, maxBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		return nil, errors.New("state snapshot bound must be positive")
	}
	canonical, err := pathidentity.FuturePath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve state root: %w", err)
	}
	if err := os.MkdirAll(canonical, 0o700); err != nil {
		return nil, fmt.Errorf("create state root: %w", err)
	}
	identity, err := pathidentity.ExistingNoFollow(canonical)
	if err != nil {
		return nil, fmt.Errorf("validate state root: %w", err)
	}
	if identity.Kind != pathidentity.KindDirectory {
		return nil, fmt.Errorf("state root is not a real directory: kind=%s", identity.Kind)
	}
	if identity.Mode.Perm() != 0o700 {
		// #nosec G302 -- this is a directory; 0700 is the required private mode.
		if err := os.Chmod(canonical, 0o700); err != nil {
			return nil, fmt.Errorf("secure state root: %w", err)
		}
		identity, err = pathidentity.ExistingNoFollow(canonical)
		if err != nil || identity.Kind != pathidentity.KindDirectory || identity.Mode.Perm() != 0o700 {
			return nil, errors.New("state root did not retain private directory mode")
		}
	}
	store := &Store{
		root: canonical, path: filepath.Join(canonical, stateFilename),
		lockPath: filepath.Join(canonical, lockFilename), maxBytes: maxBytes,
	}
	lock, err := os.OpenFile(store.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("secure state lock: %w", err)
	}
	if err := lock.Close(); err != nil {
		return nil, fmt.Errorf("close state lock: %w", err)
	}
	return store, nil
}

// Read returns an isolated snapshot. A new store reads as revision zero.
func (s *Store) Read() (Snapshot, error) {
	var snapshot Snapshot
	err := s.withLock(func() error {
		var err error
		snapshot, err = s.readLocked()
		return err
	})
	return snapshot, err
}

// Commit atomically replaces the snapshot only when expectedRevision matches
// the current committed revision.
func (s *Store) Commit(expectedRevision uint64, records map[string]json.RawMessage) (Snapshot, error) {
	var committed Snapshot
	err := s.withLock(func() error {
		current, err := s.readLocked()
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return fmt.Errorf("%w: current=%d expected=%d", ErrConflict, current.Revision, expectedRevision)
		}
		candidate := Snapshot{Revision: current.Revision + 1, Records: cloneRecords(records)}
		for key, value := range candidate.Records {
			if strings.TrimSpace(key) == "" || !json.Valid(value) {
				return fmt.Errorf("state record %q is invalid JSON", key)
			}
		}
		body, err := json.Marshal(diskSnapshot{Schema: stateSchema, Revision: candidate.Revision, Records: candidate.Records})
		if err != nil {
			return fmt.Errorf("encode state snapshot: %w", err)
		}
		if int64(len(body)) > s.maxBytes {
			return ErrTooLarge
		}
		if err := s.replaceLocked(body); err != nil {
			return err
		}
		committed = cloneSnapshot(candidate)
		return nil
	})
	return committed, err
}

func (s *Store) withLock(operation func() error) (err error) {
	lock, err := os.OpenFile(s.lockPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer func() {
		if closeErr := lock.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close state lock: %w", closeErr)
		}
	}()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	return operation()
}

//nolint:gocyclo // Bounded no-follow decoding validates every on-disk invariant before returning state.
func (s *Store) readLocked() (Snapshot, error) {
	info, err := os.Lstat(s.path)
	if os.IsNotExist(err) {
		return Snapshot{Records: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("inspect state snapshot: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, errors.New("state snapshot is not a regular no-follow file")
	}
	if info.Size() <= 0 {
		return Snapshot{}, errors.New("state snapshot is empty")
	}
	if info.Size() > s.maxBytes {
		return Snapshot{}, ErrTooLarge
	}
	file, err := os.Open(s.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open state snapshot: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, s.maxBytes+1))
	closeErr := file.Close()
	if err != nil {
		return Snapshot{}, fmt.Errorf("read state snapshot: %w", err)
	}
	if closeErr != nil {
		return Snapshot{}, fmt.Errorf("close state snapshot: %w", closeErr)
	}
	if int64(len(body)) > s.maxBytes {
		return Snapshot{}, ErrTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var disk diskSnapshot
	if err := decoder.Decode(&disk); err != nil {
		return Snapshot{}, fmt.Errorf("decode state snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Snapshot{}, errors.New("state snapshot contains trailing data")
		}
		return Snapshot{}, fmt.Errorf("decode trailing state snapshot data: %w", err)
	}
	if disk.Schema != stateSchema || disk.Revision == 0 || disk.Records == nil {
		return Snapshot{}, errors.New("state snapshot identity is invalid")
	}
	for key, value := range disk.Records {
		if strings.TrimSpace(key) == "" || !json.Valid(value) {
			return Snapshot{}, fmt.Errorf("state record %q is invalid JSON", key)
		}
	}
	return cloneSnapshot(Snapshot{Revision: disk.Revision, Records: disk.Records}), nil
}

func (s *Store) replaceLocked(body []byte) (err error) {
	temporary, err := os.CreateTemp(s.root, ".state-*")
	if err != nil {
		return fmt.Errorf("create state temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure state temporary: %w", err)
	}
	if _, err := temporary.Write(body); err != nil {
		return fmt.Errorf("write state temporary: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync state temporary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state temporary: %w", err)
	}
	if s.beforeReplace != nil {
		if err := s.beforeReplace(temporaryPath); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace state snapshot: %w", err)
	}
	directory, err := os.Open(s.root)
	if err != nil {
		return fmt.Errorf("open state root for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync state root: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close state root: %w", err)
	}
	return nil
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	return Snapshot{Revision: snapshot.Revision, Records: cloneRecords(snapshot.Records)}
}

func cloneRecords(records map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(records))
	for key, value := range records {
		cloned[key] = append(json.RawMessage(nil), value...)
	}
	return cloned
}
