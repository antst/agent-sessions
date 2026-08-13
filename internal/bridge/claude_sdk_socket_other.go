//go:build !darwin

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
	if pid <= 1 || !samePath(path, filepath.Join(directory, strconv.Itoa(pid)+".sock")) {
		return false
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
