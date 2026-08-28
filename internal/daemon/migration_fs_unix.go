package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type productionLegacyFileIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
}

func productionLegacyIdentity(stat unix.Stat_t) productionLegacyFileIdentity {
	return productionLegacyFileIdentity{
		device: uint64(stat.Dev), //nolint:unconvert // Darwin exposes Dev as a signed integer; identity uses one portable width.
		inode:  uint64(stat.Ino), mode: uint32(stat.Mode), uid: stat.Uid,
	}
}

func (identity productionLegacyFileIdentity) same(other productionLegacyFileIdentity) bool {
	return identity.device == other.device && identity.inode == other.inode &&
		identity.mode == other.mode && identity.uid == other.uid
}

func (identity productionLegacyFileIdentity) ownedRegular() bool {
	return identity.mode&unix.S_IFMT == unix.S_IFREG && int(identity.uid) == os.Getuid()
}

func (identity productionLegacyFileIdentity) ownedDirectory() bool {
	return identity.mode&unix.S_IFMT == unix.S_IFDIR && int(identity.uid) == os.Getuid()
}

func (identity productionLegacyFileIdentity) ownedSocket() bool {
	return identity.mode&unix.S_IFMT == unix.S_IFSOCK && int(identity.uid) == os.Getuid()
}

type productionLegacyAnchoredFile struct {
	path     string
	name     string
	parentFD int
	file     *os.File
	identity productionLegacyFileIdentity
}

func openProductionLegacyParent(path string) (int, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return -1, "", errors.New("legacy artifact path is not canonical")
	}
	name := filepath.Base(path)
	parent := filepath.Dir(path)
	fd, err := openProductionLegacyDirectoryPath(parent)
	if err != nil {
		return -1, "", err
	}
	return fd, name, nil
}

func openProductionLegacyDirectoryPath(path string) (int, error) {
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	rootFD, err := unix.Open(
		string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return -1, fmt.Errorf("open legacy filesystem root: %w", err)
	}
	currentFD := rootFD
	for index := 0; index < len(components); index++ {
		component := components[index]
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(
			currentFD, component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
		)
		if openErr != nil && index == 0 {
			resolved, ok := productionLegacyDarwinSystemDirectoryAlias(component)
			if ok {
				_ = unix.Close(currentFD)
				components = append(strings.Split(resolved, string(filepath.Separator)), components[index+1:]...)
				currentFD, err = unix.Open(
					string(filepath.Separator),
					unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
				)
				if err != nil {
					return -1, fmt.Errorf("reopen legacy filesystem root: %w", err)
				}
				index = -1
				continue
			}
		}
		if openErr != nil {
			_ = unix.Close(currentFD)
			componentPath := string(filepath.Separator) + filepath.Join(components[:index+1]...)
			if errors.Is(openErr, unix.ENOENT) {
				return -1, &os.PathError{Op: "open", Path: componentPath, Err: openErr}
			}
			return -1, fmt.Errorf("open anchored legacy directory %q: %w", componentPath, openErr)
		}
		_ = unix.Close(currentFD)
		currentFD = nextFD
	}
	return currentFD, nil
}

func productionLegacyDarwinSystemDirectoryAlias(component string) (string, bool) {
	if runtime.GOOS != "darwin" || component != "tmp" && component != "var" {
		return "", false
	}
	target, err := os.Readlink(string(filepath.Separator) + component)
	if err != nil || target != "private/"+component {
		return "", false
	}
	return target, true
}

func openProductionLegacyRegular(path string, write bool) (*productionLegacyAnchoredFile, error) {
	parentFD, name, err := openProductionLegacyParent(path)
	if err != nil {
		return nil, err
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if write {
		flags = unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	}
	fd, err := unix.Openat(parentFD, name, flags, 0)
	if err != nil {
		_ = unix.Close(parentFD)
		if errors.Is(err, unix.ENOENT) {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return nil, fmt.Errorf("open anchored legacy file %q: %w", path, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		_ = unix.Close(parentFD)
		return nil, fmt.Errorf("stat anchored legacy file %q: %w", path, err)
	}
	identity := productionLegacyIdentity(stat)
	if !identity.ownedRegular() {
		_ = unix.Close(fd)
		_ = unix.Close(parentFD)
		return nil, errors.New("legacy artifact is not an owned regular file")
	}
	return &productionLegacyAnchoredFile{
		path: path, name: name, parentFD: parentFD, file: os.NewFile(uintptr(fd), path), identity: identity,
	}, nil
}

func (file *productionLegacyAnchoredFile) close() {
	if file == nil {
		return
	}
	if file.file != nil {
		_ = file.file.Close()
		file.file = nil
	}
	if file.parentFD >= 0 {
		_ = unix.Close(file.parentFD)
		file.parentFD = -1
	}
}

func (file *productionLegacyAnchoredFile) readBounded(minimum, maximum int64) ([]byte, error) {
	info, err := file.file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < minimum || info.Size() > maximum {
		return nil, errors.New("legacy artifact is outside its byte bound")
	}
	body, err := io.ReadAll(io.LimitReader(file.file, maximum+1))
	if err != nil || int64(len(body)) > maximum {
		return nil, errors.New("legacy artifact read is unbounded")
	}
	return body, nil
}

func (file *productionLegacyAnchoredFile) unlink(expected productionLegacyFileIdentity) error {
	if !file.identity.same(expected) {
		return errors.New("legacy artifact inode or type changed after reattestation")
	}
	var current unix.Stat_t
	if err := unix.Fstatat(file.parentFD, file.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("reattest legacy artifact before unlink: %w", err)
	}
	if !productionLegacyIdentity(current).same(expected) {
		return errors.New("legacy artifact inode or type changed before unlink")
	}
	if err := unix.Unlinkat(file.parentFD, file.name, 0); err != nil {
		return fmt.Errorf("unlink exact legacy artifact: %w", err)
	}
	return nil
}

//nolint:unparam // The identity result binds specialized migration readers to the descriptor they consumed.
func readProductionLegacyRegular(path string, minimum, maximum int64) ([]byte, productionLegacyFileIdentity, error) {
	file, err := openProductionLegacyRegular(path, false)
	if err != nil {
		return nil, productionLegacyFileIdentity{}, err
	}
	defer file.close()
	body, err := file.readBounded(minimum, maximum)
	return body, file.identity, err
}

func observeProductionLegacyRegular(path string) (productionLegacyFileIdentity, bool, error) {
	file, err := openProductionLegacyRegular(path, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return productionLegacyFileIdentity{}, false, nil
		}
		return productionLegacyFileIdentity{}, false, err
	}
	defer file.close()
	return file.identity, true, nil
}

func openProductionLegacyAttestedRegular(
	path string,
	expected productionLegacyObservedFile,
	write bool,
) (*productionLegacyAnchoredFile, error) {
	file, err := openProductionLegacyRegular(path, write)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !expected.present {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if !expected.present || !file.identity.same(expected.identity) {
		file.close()
		return nil, errors.New("legacy artifact inode or type changed after reattestation")
	}
	return file, nil
}

func readProductionLegacyOwnedDirectory(path string, maximum int) ([]os.DirEntry, error) {
	parentFD, name, err := openProductionLegacyParent(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(parentFD) }()
	fd, err := unix.Openat(
		parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return nil, fmt.Errorf("open anchored legacy directory %q: %w", path, err)
	}
	directory := os.NewFile(uintptr(fd), path)
	defer func() { _ = directory.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("stat anchored legacy directory %q: %w", path, err)
	}
	if !productionLegacyIdentity(stat).ownedDirectory() {
		return nil, errors.New("legacy inventory directory is not an owned real directory")
	}
	entries, err := directory.ReadDir(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maximum {
		return nil, errors.New("legacy inventory directory exceeds its entry bound")
	}
	return entries, nil
}

func observeProductionLegacyPath(path string) (productionLegacyFileIdentity, bool, error) {
	parentFD, name, err := openProductionLegacyParent(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return productionLegacyFileIdentity{}, false, nil
		}
		return productionLegacyFileIdentity{}, false, err
	}
	defer func() { _ = unix.Close(parentFD) }()
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return productionLegacyFileIdentity{}, false, nil
		}
		return productionLegacyFileIdentity{}, false, fmt.Errorf("observe anchored legacy artifact %q: %w", path, err)
	}
	return productionLegacyIdentity(stat), true, nil
}

func unlinkProductionLegacySocket(
	path string,
	expected productionLegacyFileIdentity,
) error {
	parentFD, name, err := openProductionLegacyParent(path)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parentFD) }()
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("reattest legacy endpoint before unlink: %w", err)
	}
	identity := productionLegacyIdentity(current)
	if !identity.ownedSocket() || !identity.same(expected) {
		return errors.New("legacy endpoint inode or type changed before unlink")
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return fmt.Errorf("unlink exact legacy endpoint: %w", err)
	}
	return nil
}
