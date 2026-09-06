package dsh

import (
	"fmt"
	"strings"

	"github.com/antst/sessionbus/internal/productruntime"
)

const (
	ProductID     = "dsh"
	PinnedVersion = "0.1.2-rc.1"
	RequiredPNPM  = "pnpm"
	PinnedPNPM    = "10.28.1"
)

// Tuple is the indivisible DSH compatibility boundary. DSH is a developer
// preview, so no member is interpreted as a semver range.
type Tuple struct {
	CLI            string
	PackageManager string
	PNPMVersion    string
}

func PinnedTuple() Tuple {
	return Tuple{
		CLI:            PinnedVersion,
		PackageManager: RequiredPNPM, PNPMVersion: PinnedPNPM,
	}
}

func (tuple Tuple) Validate() error {
	actual := []struct {
		name string
		got  string
		want string
	}{
		{"cli", tuple.CLI, PinnedVersion},
		{"package-manager", tuple.PackageManager, RequiredPNPM},
		{"pnpm-version", tuple.PNPMVersion, PinnedPNPM},
	}
	for _, member := range actual {
		if strings.TrimSpace(member.got) != member.want {
			return fmt.Errorf("%w: DSH %s is %q, require exact %q", productruntime.ErrIncompatible, member.name, member.got, member.want)
		}
	}
	return nil
}
