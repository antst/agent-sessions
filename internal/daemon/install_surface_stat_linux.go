//go:build linux

package daemon

import "golang.org/x/sys/unix"

func hostSurfaceStatMode(stat *unix.Stat_t) uint32 { return stat.Mode }
