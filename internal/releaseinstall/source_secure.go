package releaseinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maximumReleaseSourceEntries = 100_000
	maximumReleaseSourceFile    = int64(1 << 30)
	maximumReleaseSourceBytes   = int64(4 << 30)
)

// secureReleaseSource anchors every source operation to one no-follow
// directory descriptor. Child components are opened relative to an already
// opened parent with O_NOFOLLOW, so a rename or symlink swap cannot redirect a
// validation, checksum, or staging read outside the selected source tree.
type secureReleaseSource struct {
	directory *os.File
	parent    *os.File
	name      string
}

func openSecureReleaseSource(root string) (*secureReleaseSource, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, errors.New("release source must be a clean absolute non-root directory")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("release source is not a real directory")
	}
	directory := os.NewFile(uintptr(fd), string(filepath.Separator))
	components := strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		childFD, openErr := unix.Openat(int(directory.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = directory.Close()
			return nil, errors.New("release source is not a real no-follow directory")
		}
		child := os.NewFile(uintptr(childFD), filepath.Join(directory.Name(), component))
		if index == len(components)-1 {
			info, statErr := child.Stat()
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				_ = child.Close()
				_ = directory.Close()
				return nil, errors.New("release source is not a real directory")
			}
			return &secureReleaseSource{directory: child, parent: directory, name: component}, nil
		}
		_ = directory.Close()
		directory = child
	}
	_ = directory.Close()
	return nil, errors.New("release source is not a real directory")
}

func (source *secureReleaseSource) close() error {
	if source == nil || source.directory == nil {
		return nil
	}
	return errors.Join(source.directory.Close(), source.parent.Close())
}

func (source *secureReleaseSource) reattestRoot() error {
	var opened, selected unix.Stat_t
	if err := unix.Fstat(int(source.directory.Fd()), &opened); err != nil {
		return err
	}
	if err := unix.Fstatat(int(source.parent.Fd()), source.name, &selected, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return errors.New("release source root changed during the bounded operation")
	}
	if opened.Dev != selected.Dev || opened.Ino != selected.Ino || selected.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("release source root changed during the bounded operation")
	}
	return nil
}

func canonicalReleaseRelativePath(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, '\x00') || strings.Contains(raw, `\`) {
		return "", errors.New("release source path is not canonical")
	}
	relative := filepath.Clean(filepath.FromSlash(raw))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.ToSlash(relative) != raw {
		return "", errors.New("release source path is not canonical")
	}
	return relative, nil
}

func (source *secureReleaseSource) open(relative string) (*os.File, os.FileInfo, error) {
	relative, err := canonicalReleaseRelativePath(relative)
	if err != nil {
		return nil, nil, err
	}
	components := strings.Split(filepath.ToSlash(relative), "/")
	parentFD, err := unix.Openat(int(source.directory.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(parentFD), source.directory.Name())
	for _, component := range components[:len(components)-1] {
		childFD, openErr := unix.Openat(int(parent.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = parent.Close()
			return nil, nil, fmt.Errorf("release source component %q is absent or indirect", component)
		}
		child := os.NewFile(uintptr(childFD), filepath.Join(parent.Name(), component))
		_ = parent.Close()
		parent = child
	}
	name := components[len(components)-1]
	fd, openErr := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	_ = parent.Close()
	if openErr != nil {
		return nil, nil, fmt.Errorf("release source path %q is absent or indirect", filepath.ToSlash(relative))
	}
	file := os.NewFile(uintptr(fd), filepath.Join(source.directory.Name(), relative))
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, nil, statErr
	}
	return file, info, nil
}

func (source *secureReleaseSource) readRegular(relative string, minimum, maximum int64) ([]byte, error) {
	file, info, err := source.open(relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if !info.Mode().IsRegular() || info.Size() < minimum || info.Size() > maximum {
		return nil, fmt.Errorf("release source path %q is not a bounded regular file", relative)
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(body)) != info.Size() || int64(len(body)) > maximum {
		return nil, fmt.Errorf("release source path %q changed while being read", relative)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Size() != info.Size() {
		return nil, fmt.Errorf("release source path %q changed while being read", relative)
	}
	return body, nil
}

type secureReleaseVisitor func(relative string, info os.FileInfo, file *os.File) error

func (source *secureReleaseSource) walk(visitor secureReleaseVisitor) error {
	fd, err := unix.Openat(int(source.directory.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	root := os.NewFile(uintptr(fd), source.directory.Name())
	defer func() { _ = root.Close() }()
	entries, bytes := 0, int64(0)
	if err := walkSecureReleaseDirectory(root, "", &entries, &bytes, visitor); err != nil {
		return err
	}
	return source.reattestRoot()
}

//nolint:gocyclo // The recursive descriptor walk keeps type, entry, and byte bounds adjacent to each open.
func walkSecureReleaseDirectory(
	directory *os.File,
	prefix string,
	entries *int,
	bytes *int64,
	visitor secureReleaseVisitor,
) error {
	children, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(children, func(left, right int) bool { return children[left].Name() < children[right].Name() })
	for _, child := range children {
		*entries++
		if *entries > maximumReleaseSourceEntries {
			return errors.New("release source contains too many entries")
		}
		name := child.Name()
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
			return errors.New("release source contains an unsafe entry name")
		}
		fd, openErr := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if openErr != nil {
			return fmt.Errorf("release source entry %q is indirect or unsupported", filepath.ToSlash(filepath.Join(prefix, name)))
		}
		path := filepath.Join(directory.Name(), name)
		file := os.NewFile(uintptr(fd), path)
		var opened unix.Stat_t
		if statErr := unix.Fstat(fd, &opened); statErr != nil {
			_ = file.Close()
			return statErr
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return statErr
		}
		relative := filepath.ToSlash(filepath.Join(prefix, name))
		switch {
		case info.IsDir():
			if err := visitor(relative, info, file); err != nil {
				_ = file.Close()
				return err
			}
			if err := walkSecureReleaseDirectory(file, filepath.Join(prefix, name), entries, bytes, visitor); err != nil {
				_ = file.Close()
				return err
			}
		case info.Mode().IsRegular():
			if info.Size() < 0 || info.Size() > maximumReleaseSourceFile || *bytes > maximumReleaseSourceBytes-info.Size() {
				_ = file.Close()
				return fmt.Errorf("release source file %q exceeds the bounded payload budget", relative)
			}
			*bytes += info.Size()
			if err := visitor(relative, info, file); err != nil {
				_ = file.Close()
				return err
			}
		default:
			_ = file.Close()
			return fmt.Errorf("release source contains unsupported type %q", relative)
		}
		var selected unix.Stat_t
		if statErr := unix.Fstatat(int(directory.Fd()), name, &selected, unix.AT_SYMLINK_NOFOLLOW); statErr != nil ||
			opened.Dev != selected.Dev || opened.Ino != selected.Ino || opened.Mode&unix.S_IFMT != selected.Mode&unix.S_IFMT {
			_ = file.Close()
			return fmt.Errorf("release source entry %q changed during the bounded operation", relative)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func secureReleaseContentIdentity(source *secureReleaseSource) (string, error) {
	hash := sha256.New()
	err := source.walk(func(relative string, info os.FileInfo, file *os.File) error {
		if relative == "manifest.json" {
			return nil
		}
		kind := "d"
		if info.Mode().IsRegular() {
			kind = "f"
		}
		_, _ = io.WriteString(hash, kind+"\x00"+relative+"\x00"+
			strconv.FormatUint(uint64(immutableReleaseMode(info)), 8)+"\x00"+strconv.FormatInt(info.Size(), 10)+"\x00")
		if info.Mode().IsRegular() {
			written, err := io.CopyN(hash, file, info.Size()+1)
			if !errors.Is(err, io.EOF) || written != info.Size() {
				return fmt.Errorf("release source file %q changed while hashing", relative)
			}
		}
		_, _ = io.WriteString(hash, "\x00")
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func copySecureReleaseTree(sourceRoot, destination string) (string, error) {
	source, err := openSecureReleaseSource(sourceRoot)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.close() }()
	hash := sha256.New()
	err = source.walk(func(relative string, info os.FileInfo, input *os.File) error {
		target := filepath.Join(destination, filepath.FromSlash(relative))
		bindIdentity := relative != "manifest.json"
		if bindIdentity {
			kind := "d"
			if info.Mode().IsRegular() {
				kind = "f"
			}
			_, _ = io.WriteString(hash, kind+"\x00"+relative+"\x00"+
				strconv.FormatUint(uint64(immutableReleaseMode(info)), 8)+"\x00"+strconv.FormatInt(info.Size(), 10)+"\x00")
		}
		if info.IsDir() {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
		} else {
			mode := os.FileMode(0o600)
			if info.Mode()&0o111 != 0 {
				mode = 0o700
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // Fresh role-owned stage and canonical source-relative name.
			if err != nil {
				return err
			}
			writer := io.Writer(output)
			if bindIdentity {
				writer = io.MultiWriter(output, hash)
			}
			written, copyErr := io.CopyN(writer, input, info.Size()+1)
			syncErr := output.Sync()
			closeErr := output.Close()
			if !errors.Is(copyErr, io.EOF) || written != info.Size() {
				return errors.Join(fmt.Errorf("release source file %q changed while staging", relative), syncErr, closeErr)
			}
			if err := errors.Join(syncErr, closeErr); err != nil {
				return err
			}
		}
		if bindIdentity {
			_, _ = io.WriteString(hash, "\x00")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func immutableReleaseMode(info os.FileInfo) os.FileMode {
	if info.IsDir() || info.Mode()&0o111 != 0 {
		return 0o555
	}
	return 0o444
}
