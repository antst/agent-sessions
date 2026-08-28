//go:build darwin

package daemon

import "golang.org/x/sys/unix"

func productionLegacyStatDevice(stat *unix.Stat_t) uint64 {
	// Darwin's dev_t is signed. Filesystem identity compares its underlying
	// 32-bit value, so this conversion deliberately preserves that bit pattern.
	return uint64(uint32(stat.Dev)) //nolint:gosec // Intentional signed dev_t normalization, not numeric narrowing.
}

func productionLegacyStatMode(stat *unix.Stat_t) uint32 { return uint32(stat.Mode) }
