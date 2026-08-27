//go:build linux

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func platformRuntimeRoot(_ int) (string, error) {
	base, err := absoluteEnvironmentPath("XDG_RUNTIME_DIR", "")
	if err != nil {
		return "", fmt.Errorf("resolve Linux runtime root: %w", err)
	}
	if info, statErr := os.Lstat(base); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("XDG_RUNTIME_DIR %q is not a real directory", base)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
			return "", fmt.Errorf("XDG_RUNTIME_DIR %q is not owned by the current user", base)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect XDG_RUNTIME_DIR: %w", statErr)
	}
	return filepath.Join(base, "agent-sessions"), nil
}
