//go:build linux || darwin

package federator

import (
	"os"
	"syscall"
)

func durableFileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true //nolint:unconvert // Darwin exposes Dev as int32; Linux already exposes the portable width.
}
