package releaseinstall

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maximumReleaseJournalBytes = int64(1 << 20)

func currentReleaseUID() (uint32, error) {
	uid := os.Getuid()
	if uid < 0 || uint64(uid) > uint64(^uint32(0)) {
		return 0, errors.New("current release UID is outside the platform owner range")
	}
	return uint32(uid), nil
}

func openOwnedReleaseDirectory(path string, create bool) (*os.File, error) {
	return openOwnedReleaseDirectoryWithParentSync(path, create, unix.Fsync)
}

//nolint:gocyclo // The descriptor walk keeps create, no-follow, owner, type, durability, and exact-mode checks together.
func openOwnedReleaseDirectoryWithParentSync(
	path string,
	create bool,
	syncParent func(int) error,
) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return nil, errors.New("release state directory must be clean, absolute, and non-root")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), string(filepath.Separator))
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		childFD, openErr := unix.Openat(
			int(directory.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0,
		)
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(int(directory.Fd()), component, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = directory.Close()
				return nil, mkdirErr
			}
			if mkdirErr == nil {
				if syncErr := syncParent(int(directory.Fd())); syncErr != nil {
					_ = unix.Unlinkat(int(directory.Fd()), component, unix.AT_REMOVEDIR)
					_ = directory.Close()
					return nil, syncErr
				}
			}
			childFD, openErr = unix.Openat(
				int(directory.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0,
			)
		}
		if openErr != nil {
			_ = directory.Close()
			return nil, openErr
		}
		child := os.NewFile(uintptr(childFD), filepath.Join(directory.Name(), component))
		_ = directory.Close()
		directory = child
	}
	wantUID, err := currentReleaseUID()
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Uid != wantUID || stat.Mode&0o777 != 0o700 {
		_ = directory.Close()
		return nil, errors.New("release state directory is not private and current-user-owned")
	}
	return directory, nil
}

//nolint:gocyclo // The bounded descriptor read keeps type, owner, mode, size, and same-inode checks at one boundary.
func readOwnedReleaseStateFile(directory *os.File, name string, limit int64) ([]byte, error) {
	if filepath.Base(name) != name || name == "." || limit <= 0 {
		return nil, errors.New("release state file selection is invalid")
	}
	fd, err := unix.Openat(
		int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.Name(), name))
	defer func() { _ = file.Close() }()
	wantUID, err := currentReleaseUID()
	if err != nil {
		return nil, err
	}
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG ||
		before.Uid != wantUID || before.Mode&0o777 != 0o600 || before.Size < 1 || before.Size > limit {
		return nil, errors.New("release state file is not one bounded current-user-owned regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) != before.Size {
		return nil, errors.New("release state file changed while being read")
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino ||
		before.Size != after.Size || after.Mode&unix.S_IFMT != unix.S_IFREG || after.Uid != wantUID {
		return nil, errors.New("release state file changed while being read")
	}
	return body, nil
}

func writeOwnedReleaseStateFile(directory *os.File, name string, body []byte) error {
	if filepath.Base(name) != name || name == "." || len(body) == 0 || int64(len(body)) > maximumReleaseJournalBytes {
		return errors.New("release state write is invalid or unbounded")
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporaryName := ".journal-" + hex.EncodeToString(random)
	fd, err := unix.Openat(
		int(directory.Fd()), temporaryName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.Name(), temporaryName))
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = unix.Unlinkat(int(directory.Fd()), temporaryName, 0)
		}
	}()
	if written, writeErr := file.Write(body); writeErr != nil || written != len(body) {
		return errors.Join(writeErr, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), temporaryName, int(directory.Fd()), name); err != nil {
		return err
	}
	removeTemporary = false
	return unix.Fsync(int(directory.Fd()))
}
