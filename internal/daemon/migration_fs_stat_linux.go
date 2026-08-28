//go:build linux

package daemon

import "golang.org/x/sys/unix"

func productionLegacyStatDevice(stat *unix.Stat_t) uint64 { return stat.Dev }

func productionLegacyStatMode(stat *unix.Stat_t) uint32 { return stat.Mode }
