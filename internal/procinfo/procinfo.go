// Package procinfo provides the host-native process identity shared by the
// local session runtime and the federator.
package procinfo

import (
	"path/filepath"
	"strings"
)

// Status describes whether a process identity probe produced evidence.
type Status uint8

const (
	// Unknown means the host could neither identify the process nor prove absence.
	Unknown Status = iota
	// Absent means the host proved that the PID does not exist.
	Absent
	// Known means all returned identity fields came from one host snapshot.
	Known
)

// Info is the stable process information used for ownership and registry
// compatibility. Start matches Claude's procStart contract on each platform;
// StrongStart preserves the kernel's finer-grained identity for destructive
// cleanup authorization.
type Info struct {
	Status      Status
	State       string
	Start       string
	StrongStart string
	Parent      int
}

// LooksLikeCodexHost recognizes the native and Node-hosted Codex entrypoints
// used when walking a prospective lane owner's process ancestry.
func LooksLikeCodexHost(args []string) bool {
	if len(args) == 0 {
		return false
	}
	base := filepath.Base(args[0])
	if base == "codex" {
		return true
	}
	if base == "node" || strings.HasPrefix(base, "node-") {
		for _, argument := range args[1:] {
			if filepath.Base(argument) == "codex.js" {
				return true
			}
		}
	}
	return false
}
