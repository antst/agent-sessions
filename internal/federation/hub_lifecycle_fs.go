//go:build linux || darwin

package federation

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type hubFileIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
	size   int64
}

type hubDefinitionFSHooks struct {
	afterSourceOpen func()
	afterTargetOpen func()
	beforeMutation  func()
	syncDirectory   func(*os.File) error
}

func hubIdentity(stat *unix.Stat_t) hubFileIdentity {
	return hubFileIdentity{
		device: hubStatDevice(stat), inode: stat.Ino, mode: hubStatMode(stat), uid: stat.Uid, size: stat.Size,
	}
}

func sameHubIdentity(left, right hubFileIdentity) bool {
	return left == right
}

func currentHubUID() (uint32, error) {
	uid := os.Getuid()
	if uid < 0 || uint64(uid) > uint64(^uint32(0)) {
		return 0, errors.New("current hub UID is outside the platform owner range")
	}
	return uint32(uid), nil
}

func openHubLifecycleDirectory(path string, create bool) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return nil, errors.New("hub lifecycle directory must be clean and absolute")
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
		return nil, errors.New("open hub lifecycle filesystem root")
	}
	trimmed := strings.TrimPrefix(path, string(filepath.Separator))
	if trimmed == "" {
		return directory, nil
	}
	for _, component := range strings.Split(trimmed, string(filepath.Separator)) {
		child, openErr := openHubLifecycleChild(directory, component, create)
		if openErr != nil {
			_ = directory.Close()
			return nil, openErr
		}
		_ = directory.Close()
		directory = child
	}
	return directory, nil
}

func openHubLifecycleChild(parent *os.File, component string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	childDescriptor, err := unix.Openat(int(parent.Fd()), component, flags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		mkdirErr := unix.Mkdirat(int(parent.Fd()), component, 0o700)
		if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
			return nil, mkdirErr
		}
		if mkdirErr == nil {
			if syncErr := parent.Sync(); syncErr != nil {
				return nil, syncErr
			}
		}
		childDescriptor, err = unix.Openat(int(parent.Fd()), component, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(childDescriptor), filepath.Join(parent.Name(), component))
	if child == nil {
		_ = unix.Close(childDescriptor)
		return nil, errors.New("open hub lifecycle directory component")
	}
	return child, nil
}

func openHubLifecycleParent(path string, create bool) (*os.File, string, error) {
	if !hubCleanAbsolutePath(path) {
		return nil, "", errors.New("hub lifecycle file path must be clean, absolute and non-root")
	}
	parent, err := openHubLifecycleDirectory(filepath.Dir(path), create)
	if err != nil {
		return nil, "", err
	}
	wantUID, err := currentHubUID()
	if err != nil {
		_ = parent.Close()
		return nil, "", err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != wantUID {
		_ = parent.Close()
		return nil, "", errors.New("hub lifecycle parent is not a current-user-owned real directory")
	}
	return parent, filepath.Base(path), nil
}

func verifyHubLifecycleDirectory(path string, expected *os.File) error {
	current, err := openHubLifecycleDirectory(path, false)
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()
	var want, got unix.Stat_t
	if err := unix.Fstat(int(expected.Fd()), &want); err != nil {
		return err
	}
	if err := unix.Fstat(int(current.Fd()), &got); err != nil {
		return err
	}
	if want.Dev != got.Dev || want.Ino != got.Ino || got.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("hub lifecycle parent changed identity")
	}
	return nil
}

func readHubRegularAt(
	parent *os.File,
	name string,
	limit int64,
	afterOpen func(),
) ([]byte, os.FileMode, hubFileIdentity, error) {
	if filepath.Base(name) != name || name == "." || limit <= 0 {
		return nil, 0, hubFileIdentity{}, errors.New("hub bounded file selection is invalid")
	}
	descriptor, err := unix.Openat(
		int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return nil, 0, hubFileIdentity{}, err
	}
	file := os.NewFile(uintptr(descriptor), filepath.Join(parent.Name(), name))
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, 0, hubFileIdentity{}, errors.New("open hub bounded regular file")
	}
	defer func() { _ = file.Close() }()
	if afterOpen != nil {
		afterOpen()
	}
	before, err := validateOpenedHubRegular(descriptor, limit)
	if err != nil {
		return nil, 0, hubFileIdentity{}, err
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) != before.Size || int64(len(body)) > limit {
		return nil, 0, hubFileIdentity{}, errors.New("hub file changed or exceeded its bound while being read")
	}
	if err := reattestHubRegularRead(parent, name, descriptor, hubIdentity(&before)); err != nil {
		return nil, 0, hubFileIdentity{}, err
	}
	return body, os.FileMode(before.Mode & 0o777), hubIdentity(&before), nil
}

func validateOpenedHubRegular(descriptor int, limit int64) (unix.Stat_t, error) {
	wantUID, err := currentHubUID()
	if err != nil {
		return unix.Stat_t{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Uid != wantUID || stat.Size < 0 || stat.Size > limit {
		return unix.Stat_t{}, errors.New("hub file is not one bounded current-user-owned regular file")
	}
	return stat, nil
}

func reattestHubRegularRead(parent *os.File, name string, descriptor int, expected hubFileIdentity) error {
	var after, selected unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil || !sameHubIdentity(expected, hubIdentity(&after)) {
		return errors.New("hub file changed identity while being read")
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &selected, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameHubIdentity(expected, hubIdentity(&selected)) {
		return errors.New("hub file path changed identity while being read")
	}
	return nil
}

func readHubOwnedRegularFile(
	path string,
	limit int64,
	afterOpen func(),
) ([]byte, os.FileMode, hubFileIdentity, error) {
	parent, name, err := openHubLifecycleParent(path, false)
	if err != nil {
		return nil, 0, hubFileIdentity{}, err
	}
	defer func() { _ = parent.Close() }()
	body, mode, identity, err := readHubRegularAt(parent, name, limit, afterOpen)
	if err != nil {
		return nil, 0, hubFileIdentity{}, err
	}
	if err := verifyHubLifecycleDirectory(filepath.Dir(path), parent); err != nil {
		return nil, 0, hubFileIdentity{}, err
	}
	return body, mode, identity, nil
}

func snapshotHubServiceDefinition(path string, afterOpen func()) (hubServiceDefinitionSnapshot, error) {
	parent, name, err := openHubLifecycleParent(path, false)
	if errors.Is(err, unix.ENOENT) {
		return hubServiceDefinitionSnapshot{}, nil
	}
	if err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	defer func() { _ = parent.Close() }()
	body, mode, identity, err := readHubRegularAt(parent, name, maxHubLifecyclePlanBytes, afterOpen)
	if errors.Is(err, unix.ENOENT) {
		if verifyErr := verifyHubLifecycleDirectory(filepath.Dir(path), parent); verifyErr != nil {
			return hubServiceDefinitionSnapshot{}, verifyErr
		}
		return hubServiceDefinitionSnapshot{}, nil
	}
	if err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := verifyHubLifecycleDirectory(filepath.Dir(path), parent); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	return hubServiceDefinitionSnapshot{present: true, body: body, mode: mode, identity: identity}, nil
}

func requireHubExpectedDefinition(parent *os.File, name string, expected hubServiceDefinitionSnapshot) error {
	var selected unix.Stat_t
	if !expected.present {
		if err := unix.Fstatat(int(parent.Fd()), name, &selected, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			return nil
		} else if err != nil {
			return err
		}
		return errors.New("hub service definition appeared after its absence was recorded")
	}
	body, mode, identity, err := readHubRegularAt(parent, name, maxHubLifecyclePlanBytes, nil)
	if err != nil {
		return err
	}
	if mode != expected.mode || !sameHubIdentity(identity, expected.identity) || !bytes.Equal(body, expected.body) {
		return errors.New("hub service definition changed after it was snapshotted")
	}
	return nil
}

func createHubTemporaryRegular(parent *os.File) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			return "", nil, err
		}
		name := ".agent-sessions-hub-" + hex.EncodeToString(random)
		descriptor, err := unix.Openat(
			int(parent.Fd()), name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600,
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		file := os.NewFile(uintptr(descriptor), filepath.Join(parent.Name(), name))
		if file == nil {
			_ = unix.Close(descriptor)
			_ = unix.Unlinkat(int(parent.Fd()), name, 0)
			return "", nil, errors.New("create hub temporary regular file")
		}
		return name, file, nil
	}
	return "", nil, errors.New("create unique hub temporary regular file")
}

func syncHubLifecycleDirectory(parent *os.File, hooks *hubDefinitionFSHooks) error {
	if hooks != nil && hooks.syncDirectory != nil {
		return hooks.syncDirectory(parent)
	}
	return parent.Sync()
}

//nolint:gocyclo // The descriptor, expected-identity, rename, and durability gates form one mutation.
func replaceHubServiceDefinition(
	path string,
	body []byte,
	mode os.FileMode,
	expected hubServiceDefinitionSnapshot,
	hooks *hubDefinitionFSHooks,
) (hubServiceDefinitionSnapshot, error) {
	if len(body) == 0 || len(body) > maxHubLifecyclePlanBytes || mode.Perm() == 0 || mode&^os.ModePerm != 0 {
		return hubServiceDefinitionSnapshot{}, errors.New("hub service definition content or mode is invalid")
	}
	parent, name, err := openHubLifecycleParent(path, true)
	if err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	defer func() { _ = parent.Close() }()
	temporary, file, err := createHubTemporaryRegular(parent)
	if err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	defer func() { _ = unix.Unlinkat(int(parent.Fd()), temporary, 0) }()
	if written, writeErr := file.Write(body); writeErr != nil || written != len(body) {
		_ = file.Close()
		return hubServiceDefinitionSnapshot{}, errors.Join(writeErr, io.ErrShortWrite)
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return hubServiceDefinitionSnapshot{}, err
	}
	var temporaryStat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &temporaryStat); err != nil {
		_ = file.Close()
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := file.Close(); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	if hooks != nil && hooks.beforeMutation != nil {
		hooks.beforeMutation()
	}
	if err := verifyHubLifecycleDirectory(filepath.Dir(path), parent); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := requireHubExpectedDefinition(parent, name, expected); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := unix.Renameat(int(parent.Fd()), temporary, int(parent.Fd()), name); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	writtenIdentity := hubIdentity(&temporaryStat)
	var selected unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &selected, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameHubIdentity(writtenIdentity, hubIdentity(&selected)) {
		return hubServiceDefinitionSnapshot{}, errors.New("hub service definition replacement changed before durability")
	}
	if err := syncHubLifecycleDirectory(parent, hooks); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := verifyHubLifecycleDirectory(filepath.Dir(path), parent); err != nil {
		return hubServiceDefinitionSnapshot{}, err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &selected, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!sameHubIdentity(writtenIdentity, hubIdentity(&selected)) {
		return hubServiceDefinitionSnapshot{}, errors.New("hub service definition replacement changed after durability")
	}
	return hubServiceDefinitionSnapshot{
		present: true, body: append([]byte(nil), body...), mode: mode.Perm(), identity: writtenIdentity,
	}, nil
}

func removeHubServiceDefinition(
	path string,
	expected hubServiceDefinitionSnapshot,
	hooks *hubDefinitionFSHooks,
) error {
	parent, name, err := openHubLifecycleParent(path, false)
	if errors.Is(err, unix.ENOENT) && !expected.present {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if hooks != nil && hooks.beforeMutation != nil {
		hooks.beforeMutation()
	}
	if err := verifyHubLifecycleDirectory(filepath.Dir(path), parent); err != nil {
		return err
	}
	if err := requireHubExpectedDefinition(parent, name, expected); err != nil {
		return err
	}
	if !expected.present {
		return nil
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return err
	}
	if err := syncHubLifecycleDirectory(parent, hooks); err != nil {
		return err
	}
	if err := verifyHubLifecycleDirectory(filepath.Dir(path), parent); err != nil {
		return err
	}
	var selected unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &selected, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		if err == nil {
			return errors.New("hub service definition reappeared after durable removal")
		}
		return err
	}
	return nil
}
