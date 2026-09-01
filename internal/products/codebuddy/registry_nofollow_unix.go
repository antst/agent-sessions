//go:build linux || darwin

package codebuddy

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func readNoFollowAt(directory *os.File, name string, maximum int64) ([]byte, os.FileInfo, error) {
	if directory == nil || name == "" {
		return nil, nil, errors.New("invalid directory-relative registry read")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	return readBoundedFD(fd, name, maximum)
}

func readBoundedFD(fd int, name string, maximum int64) ([]byte, os.FileInfo, error) {
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maximum {
		return nil, nil, errors.New("file byte bound exceeded")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) || after.Size() != int64(len(body)) {
		return nil, nil, errors.New("file changed while reading")
	}
	return body, after, nil
}

func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}
