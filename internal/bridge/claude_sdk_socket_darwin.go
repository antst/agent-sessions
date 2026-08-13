//go:build darwin

package bridge

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func ownedClaudeSDKSocketPath(path string, pid int) bool {
	return ownedClaudeSDKSocketPathUnder(path, pid, filepath.Join("/tmp", "cc-socks"))
}

func ownedClaudeSDKSocketPathUnder(path string, pid int, directory string) bool {
	expected := filepath.Join(directory, strconv.Itoa(pid)+".sock")
	resolvedPath, pathErr := resolveDarwinSocketPath(path)
	resolvedExpected, expectedErr := resolveDarwinSocketPath(expected)
	if pid <= 1 || pathErr != nil || expectedErr != nil || resolvedPath != resolvedExpected {
		return false
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func resolveDarwinSocketPath(path string) (string, error) {
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, filepath.Base(path)), nil
}
