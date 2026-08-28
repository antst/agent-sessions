//go:build linux

package federation

import "golang.org/x/sys/unix"

func hubStatDevice(stat *unix.Stat_t) uint64 { return stat.Dev }

func hubStatMode(stat *unix.Stat_t) uint32 { return stat.Mode }
