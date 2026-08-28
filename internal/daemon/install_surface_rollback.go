package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	hostSurfaceRollbackSchemaVersion = 1
	maxHostSurfaceRollbackBytes      = 2 << 20
	maxHostSurfaceServiceBytes       = 1 << 20
	maxHostSurfaceAliasTargetBytes   = 16 << 10
)

var hostSurfaceTemporaryCounter atomic.Uint64

type hostSurfaceMutationHooks struct {
	beforeMutation func()
	syncDirectory  func(*os.File) error
}

type hostSurfaceEntryExpectation struct {
	present  bool
	identity *unix.Stat_t
}

type hostSurfaceRollbackRecord struct {
	SchemaVersion           int                        `json:"schema_version"`
	Prefix                  string                     `json:"prefix"`
	StateRoot               string                     `json:"state_root"`
	MigrationID             string                     `json:"migration_id,omitempty"`
	MigrationTargetIdentity string                     `json:"migration_target_identity,omitempty"`
	Service                 hostSurfaceServiceRollback `json:"service"`
	Aliases                 []hostSurfaceAliasRollback `json:"aliases"`
}

type hostSurfaceServiceRollback struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Mode     uint32 `json:"mode,omitempty"`
	Body     []byte `json:"body,omitempty"`
	observed *unix.Stat_t
}

type hostSurfaceAliasRollback struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Target   string `json:"target,omitempty"`
	observed *unix.Stat_t
}

func hostSurfaceRollbackPath(prefix string) string {
	return filepath.Join(prefix, "libexec", "agent-sessions", "host", "transactions", "host-surface-rollback.json")
}

func captureHostSurfaceRollback(
	prefix, stateRoot, migrationID, migrationTargetIdentity string,
) (hostSurfaceRollbackRecord, error) {
	record := hostSurfaceRollbackRecord{
		SchemaVersion: hostSurfaceRollbackSchemaVersion, Prefix: prefix, StateRoot: stateRoot,
		MigrationID: migrationID, MigrationTargetIdentity: migrationTargetIdentity,
		Service: hostSurfaceServiceRollback{Path: hostServiceDefinitionPath()},
	}
	service, err := snapshotHostSurfaceService(record.Service.Path, nil)
	if err != nil {
		return hostSurfaceRollbackRecord{}, err
	}
	record.Service = service
	binRoot := filepath.Join(prefix, "bin")
	for _, name := range hostAliasNames() {
		alias, snapshotErr := snapshotHostSurfaceAlias(filepath.Join(binRoot, name), nil)
		if snapshotErr != nil {
			return hostSurfaceRollbackRecord{}, snapshotErr
		}
		record.Aliases = append(record.Aliases, alias)
	}
	if err := record.validate(prefix, stateRoot); err != nil {
		return hostSurfaceRollbackRecord{}, err
	}
	return record, nil
}

func (record hostSurfaceRollbackRecord) validate(prefix, stateRoot string) error {
	if record.SchemaVersion != hostSurfaceRollbackSchemaVersion || record.Prefix != prefix ||
		record.StateRoot != stateRoot || record.Service.Path != hostServiceDefinitionPath() ||
		len(record.Aliases) != len(hostAliasNames()) {
		return errors.New("host surface rollback provenance has incomplete exact identity")
	}
	if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix ||
		!filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return errors.New("host surface rollback provenance has noncanonical roots")
	}
	if err := record.validateMigrationBinding(); err != nil {
		return err
	}
	if err := record.validateService(); err != nil {
		return err
	}
	return record.validateAliases(prefix)
}

func (record hostSurfaceRollbackRecord) validateMigrationBinding() error {
	if (record.MigrationID == "") != (record.MigrationTargetIdentity == "") {
		return errors.New("host surface rollback provenance has an incomplete migration binding")
	}
	if record.MigrationID == "" {
		return nil
	}
	if !durableRecordID.MatchString(record.MigrationID) ||
		!strings.HasPrefix(record.MigrationTargetIdentity, "sha256:") ||
		len(record.MigrationTargetIdentity) != len("sha256:")+sha256.Size*2 {
		return errors.New("host surface rollback provenance has an invalid migration binding")
	}
	_, err := hex.DecodeString(strings.TrimPrefix(record.MigrationTargetIdentity, "sha256:"))
	if err != nil {
		return errors.New("host surface rollback provenance has an invalid migration target identity")
	}
	return nil
}

func (record hostSurfaceRollbackRecord) validateService() error {
	if record.Service.Present {
		if len(record.Service.Body) == 0 || len(record.Service.Body) > maxHostSurfaceServiceBytes ||
			record.Service.Mode == 0 || record.Service.Mode&^uint32(0o777) != 0 {
			return errors.New("host surface rollback service snapshot is invalid")
		}
	} else if len(record.Service.Body) != 0 || record.Service.Mode != 0 {
		return errors.New("absent host service rollback retains content")
	}
	return nil
}

func (record hostSurfaceRollbackRecord) validateAliases(prefix string) error {
	for index, name := range hostAliasNames() {
		alias := record.Aliases[index]
		if alias.Path != filepath.Join(prefix, "bin", name) || alias.Present != (alias.Target != "") ||
			len(alias.Target) > maxHostSurfaceAliasTargetBytes || strings.ContainsRune(alias.Target, 0) {
			return errors.New("host surface rollback alias inventory is not exact")
		}
	}
	return nil
}

func saveHostSurfaceRollback(record hostSurfaceRollbackRecord) error {
	if err := record.validate(record.Prefix, record.StateRoot); err != nil {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil || len(body) > maxHostSurfaceRollbackBytes {
		return errors.New("host surface rollback provenance exceeds its bound")
	}
	return replaceHostSurfaceRegularFile(hostSurfaceRollbackPath(record.Prefix), body, 0o600, 0o700, nil)
}

func loadHostSurfaceRollback(prefix, stateRoot string) (hostSurfaceRollbackRecord, error) {
	body, _, err := readDaemonBoundedRegularFileSnapshot(
		hostSurfaceRollbackPath(prefix), maxHostSurfaceRollbackBytes, nil, true, nil,
	)
	if err != nil {
		return hostSurfaceRollbackRecord{}, err
	}
	var record hostSurfaceRollbackRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return hostSurfaceRollbackRecord{}, errors.New("host surface rollback provenance is malformed")
	}
	if err := record.validate(prefix, stateRoot); err != nil {
		return hostSurfaceRollbackRecord{}, err
	}
	return record, nil
}

func restoreHostSurfaceRollback(record hostSurfaceRollbackRecord) error {
	if err := record.validate(record.Prefix, record.StateRoot); err != nil {
		return err
	}
	var result error
	for index := len(record.Aliases) - 1; index >= 0; index-- {
		alias := record.Aliases[index]
		result = errors.Join(result, restoreHostSurfaceAlias(alias, nil))
	}
	result = errors.Join(result, restoreHostSurfaceService(record.Service, nil))
	return result
}

func removeHostSurfaceRollback(prefix string) error {
	return removeHostSurfaceEntry(hostSurfaceRollbackPath(prefix), unix.S_IFREG, nil)
}

func readDaemonBoundedRegularFile(path string, limit int64) ([]byte, error) {
	return readDaemonBoundedRegularFileWithHook(path, limit, nil)
}

func readDaemonBoundedRegularFileWithHook(path string, limit int64, afterOpen func()) ([]byte, error) {
	body, _, err := readDaemonBoundedRegularFileSnapshot(path, limit, afterOpen, false, nil)
	return body, err
}

func snapshotHostSurfaceService(path string, afterOpen func()) (hostSurfaceServiceRollback, error) {
	record := hostSurfaceServiceRollback{Path: path}
	var observed *unix.Stat_t
	body, mode, err := readDaemonBoundedRegularFileSnapshot(
		path, maxHostSurfaceServiceBytes, afterOpen, true, &observed,
	)
	if errors.Is(err, unix.ENOENT) {
		if verifyErr := verifyAbsentHostSurfaceEntry(path); verifyErr != nil {
			return hostSurfaceServiceRollback{}, verifyErr
		}
		return record, nil
	}
	if err != nil {
		return hostSurfaceServiceRollback{}, fmt.Errorf("snapshot host service definition: %w", err)
	}
	if len(body) == 0 || mode.Perm() == 0 {
		return hostSurfaceServiceRollback{}, errors.New("existing host service definition is not rollback-safe")
	}
	record.Present, record.Mode, record.Body, record.observed = true, uint32(mode.Perm()), body, observed
	return record, nil
}

func snapshotHostSurfaceAlias(path string, afterStat func()) (hostSurfaceAliasRollback, error) {
	record := hostSurfaceAliasRollback{Path: path}
	parent, name, err := openDaemonParentDirectory(path, false, 0)
	if errors.Is(err, unix.ENOENT) {
		return record, nil
	}
	if err != nil {
		return hostSurfaceAliasRollback{}, fmt.Errorf("open host alias parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		if verifyErr := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); verifyErr != nil {
			return hostSurfaceAliasRollback{}, verifyErr
		}
		return record, nil
	} else if err != nil {
		return hostSurfaceAliasRollback{}, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFLNK {
		return hostSurfaceAliasRollback{}, fmt.Errorf("host alias %q changed to a non-symlink", path)
	}
	if !hostSurfaceOwnedByCurrentUser(before.Uid) {
		return hostSurfaceAliasRollback{}, fmt.Errorf("host alias %q is not current-user-owned", path)
	}
	if afterStat != nil {
		afterStat()
	}
	target := make([]byte, maxHostSurfaceAliasTargetBytes+1)
	n, err := unix.Readlinkat(int(parent.Fd()), name, target)
	if err != nil {
		return hostSurfaceAliasRollback{}, err
	}
	if n <= 0 || n > maxHostSurfaceAliasTargetBytes {
		return hostSurfaceAliasRollback{}, errors.New("host alias target exceeds its bound")
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameHostSurfaceIdentity(&before, &after) {
		return hostSurfaceAliasRollback{}, errors.New("host alias changed while being snapshotted")
	}
	if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
		return hostSurfaceAliasRollback{}, err
	}
	identity := before
	record.Present, record.Target, record.observed = true, string(target[:n]), &identity
	return record, nil
}

// readDaemonBoundedRegularFileSnapshot keeps the parent and file descriptors open for the
// complete observation. stablePath is reserved for rollback provenance, which must reject a
// concurrent pathname replacement instead of merely continuing to read the already-open file.
//
//nolint:gocyclo // Descriptor lifetime, bounded read, owner/type checks, and identity revalidation are one observation.
func readDaemonBoundedRegularFileSnapshot(
	path string, limit int64, afterOpen func(), stablePath bool, observed **unix.Stat_t,
) ([]byte, os.FileMode, error) {
	if limit <= 0 {
		return nil, 0, errors.New("bounded file read requires a positive limit")
	}
	parent, name, err := openDaemonParentDirectory(path, false, 0)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = parent.Close() }()
	descriptor, err := unix.Openat(
		int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, 0, errors.New("open bounded regular file")
	}
	defer func() { _ = file.Close() }()
	if afterOpen != nil {
		afterOpen()
	}
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil || !hostSurfaceOwnedByCurrentUser(before.Uid) {
		return nil, 0, errors.New("bounded regular file is not current-user-owned")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, 0, errors.New("bounded file is not a supported regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) != info.Size() || int64(len(body)) > limit {
		return nil, 0, errors.New("bounded regular file exceeds its limit")
	}
	var after unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil || !sameHostSurfaceIdentity(&before, &after) {
		return nil, 0, errors.New("bounded regular file changed while being read")
	}
	afterInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, afterInfo) || info.Mode() != afterInfo.Mode() ||
		info.Size() != afterInfo.Size() || !info.ModTime().Equal(afterInfo.ModTime()) {
		return nil, 0, errors.New("bounded regular file changed while being read")
	}
	if stablePath {
		var pathAfter unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &pathAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
			!sameHostSurfaceIdentity(&before, &pathAfter) {
			return nil, 0, errors.New("bounded regular file path changed while being read")
		}
		if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
			return nil, 0, err
		}
	}
	if observed != nil {
		identity := after
		*observed = &identity
	}
	return body, info.Mode().Perm(), nil
}

func syncHostSurfaceDirectory(path string) error {
	directory, err := openDaemonDirectory(path, false, 0)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := requireCurrentUserOwnedDirectory(directory); err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	return verifyHostSurfaceDirectoryPath(path, directory)
}

func restoreHostSurfaceAlias(alias hostSurfaceAliasRollback, hooks *hostSurfaceMutationHooks) error {
	if !alias.Present {
		return removeHostSurfaceEntry(alias.Path, unix.S_IFLNK, hooks)
	}
	return replaceHostSurfaceAlias(alias.Path, alias.Target, hooks)
}

func restoreHostSurfaceService(service hostSurfaceServiceRollback, hooks *hostSurfaceMutationHooks) error {
	if !service.Present {
		return removeHostSurfaceEntry(service.Path, unix.S_IFREG, hooks)
	}
	return replaceHostSurfaceRegularFile(service.Path, service.Body, os.FileMode(service.Mode), 0o700, hooks)
}

func replaceHostSurfaceAlias(path, target string, hooks *hostSurfaceMutationHooks) error {
	return replaceHostSurfaceAliasExpected(path, target, hooks, nil)
}

func replaceHostSurfaceAliasExpected(
	path, target string, hooks *hostSurfaceMutationHooks, expected *hostSurfaceEntryExpectation,
) error {
	if target == "" || len(target) > maxHostSurfaceAliasTargetBytes || strings.ContainsRune(target, 0) {
		return errors.New("host alias target is invalid")
	}
	parent, name, err := openDaemonParentDirectory(path, true, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := requireCurrentUserOwnedDirectory(parent); err != nil {
		return err
	}
	temporary, err := createHostSurfaceTemporarySymlink(parent, target)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(parent.Fd()), temporary, 0) }()
	var temporaryIdentity unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), temporary, &temporaryIdentity, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if hooks != nil && hooks.beforeMutation != nil {
		hooks.beforeMutation()
	}
	if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
		return err
	}
	if err := validateHostSurfaceReplacementTarget(parent, name, unix.S_IFLNK, expected); err != nil &&
		(expected != nil || !errors.Is(err, unix.ENOENT)) {
		return err
	}
	if err := unix.Renameat(int(parent.Fd()), temporary, int(parent.Fd()), name); err != nil {
		return err
	}
	return finishHostSurfaceMutation(path, parent, name, &temporaryIdentity, hooks)
}

func replaceHostSurfaceRegularFile(
	path string, body []byte, mode, directoryMode os.FileMode, hooks *hostSurfaceMutationHooks,
) error {
	return replaceHostSurfaceRegularFileExpected(path, body, mode, directoryMode, hooks, nil)
}

//nolint:gocyclo // Temporary-file durability and before/after identity gates form one atomic replacement.
func replaceHostSurfaceRegularFileExpected(
	path string, body []byte, mode, directoryMode os.FileMode, hooks *hostSurfaceMutationHooks,
	expected *hostSurfaceEntryExpectation,
) error {
	if len(body) == 0 || mode.Perm() == 0 || mode&^os.ModePerm != 0 {
		return errors.New("host surface regular file content or mode is invalid")
	}
	parent, name, err := openDaemonParentDirectory(path, true, directoryMode)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := requireCurrentUserOwnedDirectory(parent); err != nil {
		return err
	}
	temporary, file, err := createHostSurfaceTemporaryRegular(parent)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(parent.Fd()), temporary, 0) }()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	var temporaryIdentity unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &temporaryIdentity); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if hooks != nil && hooks.beforeMutation != nil {
		hooks.beforeMutation()
	}
	if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
		return err
	}
	if err := validateHostSurfaceReplacementTarget(parent, name, unix.S_IFREG, expected); err != nil &&
		(expected != nil || !errors.Is(err, unix.ENOENT)) {
		return err
	}
	if err := unix.Renameat(int(parent.Fd()), temporary, int(parent.Fd()), name); err != nil {
		return err
	}
	return finishHostSurfaceMutation(path, parent, name, &temporaryIdentity, hooks)
}

func removeHostSurfaceEntry(path string, expectedType uint32, hooks *hostSurfaceMutationHooks) error {
	parent, name, err := openDaemonParentDirectory(path, false, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := requireCurrentUserOwnedDirectory(parent); err != nil {
		return err
	}
	if hooks != nil && hooks.beforeMutation != nil {
		hooks.beforeMutation()
	}
	if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
		return err
	}
	if err := validateHostSurfaceReplacementTarget(parent, name, expectedType, nil); errors.Is(err, unix.ENOENT) {
		return finishHostSurfaceRemoval(path, parent, name, hooks)
	} else if err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return err
	}
	return finishHostSurfaceRemoval(path, parent, name, hooks)
}

func finishHostSurfaceRemoval(
	path string, parent *os.File, name string, hooks *hostSurfaceMutationHooks,
) error {
	if err := syncHostSurfaceMutationDirectory(parent, hooks); err != nil {
		return err
	}
	if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		if err == nil {
			return errors.New("host surface entry reappeared during removal")
		}
		return err
	}
	return nil
}

func finishHostSurfaceMutation(
	path string, parent *os.File, name string, expected *unix.Stat_t, hooks *hostSurfaceMutationHooks,
) error {
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameHostSurfaceIdentity(expected, &after) {
		return errors.New("host surface replacement identity changed before durability")
	}
	if err := syncHostSurfaceMutationDirectory(parent, hooks); err != nil {
		return err
	}
	if err := verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent); err != nil {
		return err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameHostSurfaceIdentity(expected, &after) {
		return errors.New("host surface replacement identity changed after durability")
	}
	return nil
}

func syncHostSurfaceMutationDirectory(parent *os.File, hooks *hostSurfaceMutationHooks) error {
	if hooks != nil && hooks.syncDirectory != nil {
		return hooks.syncDirectory(parent)
	}
	return parent.Sync()
}

func validateHostSurfaceReplacementTarget(
	parent *os.File, name string, expectedType uint32, expected *hostSurfaceEntryExpectation,
) error {
	var identity unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &identity, unix.AT_SYMLINK_NOFOLLOW)
	if expected != nil && !expected.present {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("host surface target appeared after its absence snapshot")
	}
	if err != nil {
		return err
	}
	mode := hostSurfaceStatMode(&identity)
	if mode&unix.S_IFMT != expectedType {
		return errors.New("host surface target changed filesystem type")
	}
	if !hostSurfaceOwnedByCurrentUser(identity.Uid) {
		return errors.New("host surface target is not current-user-owned")
	}
	if expected != nil && (expected.identity == nil || !sameHostSurfaceIdentity(expected.identity, &identity)) {
		return errors.New("host surface target identity changed after snapshot")
	}
	return nil
}

func createHostSurfaceTemporarySymlink(parent *os.File, target string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name := nextHostSurfaceTemporaryName()
		if err := unix.Symlinkat(target, int(parent.Fd()), name); err == nil {
			return name, nil
		} else if !errors.Is(err, unix.EEXIST) {
			return "", err
		}
	}
	return "", errors.New("create unique host surface temporary symlink")
}

func createHostSurfaceTemporaryRegular(parent *os.File) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name := nextHostSurfaceTemporaryName()
		descriptor, err := unix.Openat(
			int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
		)
		if err == nil {
			file := os.NewFile(uintptr(descriptor), name)
			if file == nil {
				_ = unix.Close(descriptor)
				return "", nil, errors.New("create host surface temporary regular file")
			}
			return name, file, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("create unique host surface temporary regular file")
}

func nextHostSurfaceTemporaryName() string {
	return fmt.Sprintf(".agent-sessions-host-surface-%d-%d", os.Getpid(), hostSurfaceTemporaryCounter.Add(1))
}

func requireCurrentUserOwnedDirectory(directory *os.File) error {
	var identity unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &identity); err != nil {
		return err
	}
	if identity.Mode&unix.S_IFMT != unix.S_IFDIR || !hostSurfaceOwnedByCurrentUser(identity.Uid) {
		return errors.New("host surface parent is not a current-user-owned directory")
	}
	return nil
}

func sameHostSurfaceIdentity(left, right *unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode == right.Mode &&
		left.Uid == right.Uid && left.Size == right.Size
}

func hostSurfaceOwnedByCurrentUser(owner uint32) bool {
	current := os.Getuid()
	if current < 0 || uint64(current) > uint64(^uint32(0)) {
		return false
	}
	return owner == uint32(current)
}

func verifyHostSurfaceDirectoryPath(path string, opened *os.File) error {
	current, err := openDaemonDirectory(path, false, 0)
	if err != nil {
		return errors.New("host surface parent path changed during operation")
	}
	defer func() { _ = current.Close() }()
	openedInfo, openedErr := opened.Stat()
	currentInfo, currentErr := current.Stat()
	if openedErr != nil || currentErr != nil || !os.SameFile(openedInfo, currentInfo) {
		return errors.New("host surface parent path changed during operation")
	}
	return nil
}

func verifyAbsentHostSurfaceEntry(path string) error {
	parent, name, err := openDaemonParentDirectory(path, false, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	var identity unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &identity, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		if err == nil {
			return errors.New("host surface entry appeared during absence snapshot")
		}
		return err
	}
	return verifyHostSurfaceDirectoryPath(filepath.Dir(path), parent)
}

func openDaemonParentDirectory(path string, create bool, mode os.FileMode) (*os.File, string, error) {
	if !validDaemonAbsolutePath(path, false) {
		return nil, "", errors.New("host surface path must be clean, absolute, and non-root")
	}
	parent, err := openDaemonDirectory(filepath.Dir(path), create, mode)
	if err != nil {
		return nil, "", err
	}
	return parent, filepath.Base(path), nil
}

func openDaemonDirectory(path string, create bool, mode os.FileMode) (*os.File, error) {
	if !validDaemonAbsolutePath(path, true) {
		return nil, errors.New("daemon directory path must be clean and absolute")
	}
	descriptor, err := unix.Open(
		string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(descriptor), string(filepath.Separator))
	if directory == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("open filesystem root")
	}
	trimmed := strings.TrimPrefix(path, string(filepath.Separator))
	if trimmed == "" {
		return directory, nil
	}
	for _, component := range strings.Split(trimmed, string(filepath.Separator)) {
		childDescriptor, openErr := unix.Openat(
			int(directory.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0,
		)
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(int(directory.Fd()), component, uint32(mode.Perm()))
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = directory.Close()
				return nil, mkdirErr
			}
			if mkdirErr == nil {
				if syncErr := directory.Sync(); syncErr != nil {
					_ = directory.Close()
					return nil, syncErr
				}
			}
			childDescriptor, openErr = unix.Openat(
				int(directory.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0,
			)
		}
		if openErr != nil {
			_ = directory.Close()
			return nil, openErr
		}
		child := os.NewFile(uintptr(childDescriptor), filepath.Join(directory.Name(), component))
		if child == nil {
			_ = unix.Close(childDescriptor)
			_ = directory.Close()
			return nil, errors.New("open daemon directory component")
		}
		_ = directory.Close()
		directory = child
	}
	return directory, nil
}

func validDaemonAbsolutePath(path string, allowRoot bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return false
	}
	return allowRoot || path != string(filepath.Separator)
}
