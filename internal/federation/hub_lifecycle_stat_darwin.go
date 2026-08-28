//go:build darwin

package federation

import "golang.org/x/sys/unix"

func hubStatDevice(stat *unix.Stat_t) uint64 { return uint64(uint32(stat.Dev)) }

func hubStatMode(stat *unix.Stat_t) uint32 { return uint32(stat.Mode) }
