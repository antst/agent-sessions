// Package statestore provides role-neutral, revisioned, crash-durable JSON
// records beneath one owned state root.
package statestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// RecordSchemaVersion is the role-neutral record envelope schema.
const RecordSchemaVersion = 1

// Revision is a monotonically increasing record CAS identity.
type Revision uint64

var (
	// ErrRevisionConflict reports a stale compare-and-swap expectation.
	ErrRevisionConflict = errors.New("state record revision conflict")
	// ErrCorruptRecord reports malformed or unsupported durable state.
	ErrCorruptRecord = errors.New("state record is corrupt")
	// ErrUnsafeRecord reports an unsafe path, type, owner, or traversal.
	ErrUnsafeRecord = errors.New("state record path or type is unsafe")
)

// FaultPoint identifies one injectable crash-durability boundary.
type FaultPoint string

const (
	// FaultCreateTemporary occurs before allocating a same-directory temporary.
	FaultCreateTemporary FaultPoint = "create_temporary"
	// FaultWriteTemporary occurs before writing the bounded envelope.
	FaultWriteTemporary FaultPoint = "write_temporary"
	// FaultAfterTemporarySync occurs after file durability but before rename.
	FaultAfterTemporarySync FaultPoint = "after_temporary_sync"
	// FaultAfterRename occurs after authority changes but before directory sync.
	FaultAfterRename FaultPoint = "after_rename"
	// FaultSyncParentDirectory occurs before committing directory metadata.
	FaultSyncParentDirectory FaultPoint = "sync_parent_directory"
)

// Options configures one owned, bounded atomic record root.
type Options struct {
	Root           string
	MaxRecordBytes int64
	InjectFault    func(FaultPoint) error
}

// Store provides crash-durable revisioned JSON records.
type Store struct {
	root           string
	maxRecordBytes int64
	injectFault    func(FaultPoint) error
	mu             sync.Mutex
}

type recordEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Revision      Revision        `json:"revision"`
	Value         json.RawMessage `json:"value"`
}

// Open validates or creates one owner-only store root.
func Open(options Options) (*Store, error) {
	if err := validateOpenOptions(options); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(options.Root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByCurrentUser(info) {
			return nil, fmt.Errorf("%w: state root is not an owned real directory", ErrUnsafeRecord)
		}
		if info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(options.Root, 0o700); err != nil { //nolint:gosec // Owner-only directory requires execute permission.
				return nil, fmt.Errorf("secure state root: %w", err)
			}
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(options.Root, 0o700); err != nil {
			return nil, fmt.Errorf("create state root: %w", err)
		}
		if err := os.Chmod(options.Root, 0o700); err != nil { //nolint:gosec // Owner-only directory requires execute permission.
			return nil, fmt.Errorf("secure state root: %w", err)
		}
	} else {
		return nil, fmt.Errorf("inspect state root: %w", err)
	}
	return &Store{
		root: options.Root, maxRecordBytes: options.MaxRecordBytes, injectFault: options.InjectFault,
	}, nil
}

// OpenExisting opens one already-created owner-only store without changing
// filesystem state. It is the read-only/offline counterpart to Open: missing
// roots remain os.IsNotExist and unsafe permissions are rejected, never fixed.
func OpenExisting(options Options) (*Store, error) {
	if err := validateOpenOptions(options); err != nil {
		return nil, err
	}
	info, err := os.Lstat(options.Root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: state root is not an owner-only owned real directory", ErrUnsafeRecord)
	}
	return &Store{
		root: options.Root, maxRecordBytes: options.MaxRecordBytes, injectFault: options.InjectFault,
	}, nil
}

func validateOpenOptions(options Options) error {
	if options.MaxRecordBytes <= 0 {
		return errors.New("state store requires a positive record bound")
	}
	if !filepath.IsAbs(options.Root) || filepath.Clean(options.Root) != options.Root || options.Root == string(filepath.Separator) {
		return fmt.Errorf("%w: state root must be a clean absolute non-root path", ErrUnsafeRecord)
	}
	return nil
}

// RecordPath returns a safe key's canonical record path or an empty string.
func (s *Store) RecordPath(key string) string {
	clean, err := cleanRecordKey(key)
	if err != nil {
		return ""
	}
	return filepath.Join(s.root, filepath.FromSlash(clean)+".json")
}

func (s *Store) Read(ctx context.Context, key string, value any) (Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path, err := s.safeRecordPath(key, false)
	if err != nil {
		return 0, err
	}
	envelope, err := s.readEnvelope(path)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, errors.New("state record destination is nil")
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return 0, fmt.Errorf("%w: decode value: %w", ErrCorruptRecord, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return 0, fmt.Errorf("%w: trailing value JSON", ErrCorruptRecord)
	}
	return envelope.Revision, nil
}

// CompareAndSwap atomically commits value when expected is current.
//
//nolint:gocyclo // Crash-durable CAS stages stay together so no commit boundary is implicit.
func (s *Store) CompareAndSwap(ctx context.Context, key string, expected Revision, value any) (Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.safeRecordPath(key, true)
	if err != nil {
		return 0, err
	}
	current := Revision(0)
	if envelope, readErr := s.readEnvelope(path); readErr == nil {
		current = envelope.Revision
	} else if !os.IsNotExist(readErr) {
		return 0, readErr
	}
	if current != expected {
		return 0, fmt.Errorf("%w: current=%d expected=%d", ErrRevisionConflict, current, expected)
	}
	next := current + 1
	valueBody, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("encode state value: %w", err)
	}
	body, err := json.Marshal(recordEnvelope{SchemaVersion: RecordSchemaVersion, Revision: next, Value: valueBody})
	if err != nil {
		return 0, fmt.Errorf("encode state record: %w", err)
	}
	if int64(len(body)) > s.maxRecordBytes {
		return 0, errors.New("state record exceeds its configured bound")
	}

	parent := filepath.Dir(path)
	if err := s.fault(FaultCreateTemporary); err != nil {
		return 0, err
	}
	temporary, err := os.CreateTemp(parent, ".agent-sessions-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("create state temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return 0, fmt.Errorf("secure state temporary: %w", err)
	}
	if err := s.fault(FaultWriteTemporary); err != nil {
		_ = temporary.Close()
		return 0, err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return 0, fmt.Errorf("write state temporary: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return 0, fmt.Errorf("sync state temporary: %w", err)
	}
	if err := s.fault(FaultAfterTemporarySync); err != nil {
		_ = temporary.Close()
		return 0, err
	}
	if err := temporary.Close(); err != nil {
		return 0, fmt.Errorf("close state temporary: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return 0, fmt.Errorf("commit state record: %w", err)
	}
	if err := s.fault(FaultAfterRename); err != nil {
		return 0, err
	}
	if err := s.fault(FaultSyncParentDirectory); err != nil {
		return 0, err
	}
	if err := syncDirectory(parent); err != nil {
		return 0, fmt.Errorf("sync state parent: %w", err)
	}
	return next, nil
}

// Recover removes only same-root regular temporary records. Complete renamed
// records are already authoritative and are validated lazily on read.
func (s *Store) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return filepath.WalkDir(s.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == s.root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("%w: unsafe state directory %q", ErrUnsafeRecord, path)
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".agent-sessions-") && filepath.Ext(entry.Name()) == ".tmp" {
			if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
				return fmt.Errorf("%w: unsafe temporary record %q", ErrUnsafeRecord, path)
			}
			if err := os.Remove(path); err != nil { //nolint:gosec // Walk is confined to the owned root and re-attests every entry before removal.
				return err
			}
			return syncDirectory(filepath.Dir(path))
		}
		return nil
	})
}

func (s *Store) safeRecordPath(key string, createParents bool) (string, error) {
	clean, err := cleanRecordKey(key)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.root, filepath.FromSlash(clean)+".json")
	relative, err := filepath.Rel(s.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: record escaped state root", ErrUnsafeRecord)
	}
	parent := filepath.Dir(path)
	if createParents {
		if err := ensureOwnedDirectories(s.root, parent); err != nil {
			return "", err
		}
	} else if err := validateOwnedDirectories(s.root, parent); err != nil {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
			return "", fmt.Errorf("%w: record is not an owned regular file", ErrUnsafeRecord)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

func (s *Store) readEnvelope(path string) (recordEnvelope, error) {
	file, err := os.Open(path) //nolint:gosec // path is already bounded to the owned store root.
	if err != nil {
		return recordEnvelope{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return recordEnvelope{}, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Size() < 1 || info.Size() > s.maxRecordBytes {
		return recordEnvelope{}, fmt.Errorf("%w: invalid record metadata", ErrUnsafeRecord)
	}
	body, err := io.ReadAll(io.LimitReader(file, s.maxRecordBytes+1))
	if err != nil {
		return recordEnvelope{}, err
	}
	if int64(len(body)) > s.maxRecordBytes {
		return recordEnvelope{}, fmt.Errorf("%w: record exceeds bound", ErrUnsafeRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope recordEnvelope
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		envelope.SchemaVersion != RecordSchemaVersion || envelope.Revision == 0 || len(envelope.Value) == 0 {
		return recordEnvelope{}, fmt.Errorf("%w: invalid record envelope", ErrCorruptRecord)
	}
	return envelope, nil
}

func (s *Store) fault(point FaultPoint) error {
	if s.injectFault == nil {
		return nil
	}
	return s.injectFault(point)
}

func cleanRecordKey(key string) (string, error) {
	if key == "" || filepath.IsAbs(key) || strings.ContainsRune(key, '\x00') {
		return "", fmt.Errorf("%w: invalid record key", ErrUnsafeRecord)
	}
	normalized := filepath.ToSlash(filepath.Clean(filepath.FromSlash(key)))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || normalized != key {
		return "", fmt.Errorf("%w: invalid record key", ErrUnsafeRecord)
	}
	return normalized, nil
}

func ensureOwnedDirectories(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: state directory escaped root", ErrUnsafeRecord)
	}
	current := root
	if relative == "." {
		return validateOwnedDirectories(root, root)
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		switch {
		case os.IsNotExist(statErr):
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
		case statErr != nil:
			return statErr
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByCurrentUser(info):
			return fmt.Errorf("%w: unsafe state directory %q", ErrUnsafeRecord, current)
		}
	}
	return nil
}

func validateOwnedDirectories(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: state directory escaped root", ErrUnsafeRecord)
	}
	current := root
	components := []string{}
	if relative != "." {
		components = strings.Split(relative, string(filepath.Separator))
	}
	for _, component := range append([]string{""}, components...) {
		if component != "" {
			current = filepath.Join(current, component)
		}
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByCurrentUser(info) {
			return fmt.Errorf("%w: unsafe state directory %q", ErrUnsafeRecord, current)
		}
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // validated store-owned directory.
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
