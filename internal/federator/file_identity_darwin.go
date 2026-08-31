//go:build darwin

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
	// Darwin exposes dev_t as int32. Its kernel bit pattern is an opaque file
	// identity component, so normalization to the repository's uint64 storage
	// type is intentional and stable for equality checks.
	return uint64(stat.Dev), stat.Ino, true //nolint:gosec // G115 is the deliberate dev_t normalization described above.
}
