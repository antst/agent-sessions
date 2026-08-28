//go:build darwin

package daemon

import "golang.org/x/sys/unix"

func hostSurfaceStatMode(stat *unix.Stat_t) uint32 { return uint32(stat.Mode) }
