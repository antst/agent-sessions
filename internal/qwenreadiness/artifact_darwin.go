//go:build darwin

package qwenreadiness

import (
	"os"
	"syscall"
)

func durableArtifactIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true //nolint:gosec // dev_t normalization is deliberate opaque identity.
}
