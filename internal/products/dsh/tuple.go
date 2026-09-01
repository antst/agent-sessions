package dsh

import (
	"context"
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
)

const (
	ProductID     = "dsh"
	PinnedVersion = "0.1.2-alpha.3"
	RequiredPNPM  = "pnpm"
	PinnedPNPM    = "10.28.1"

	CLIPackage     = "@deepseek-ai/dsh"
	ACPAppPackage  = "@deepseek-ai/dsh-acp-app"
	PluginPackage  = "@agent-sessions/dsh-plugin"
	ProfilePackage = "@agent-sessions/dsh-profile"
)

// Tuple is the indivisible DSH compatibility boundary. DSH is a developer
// preview, so no member is interpreted as a semver range.
type Tuple struct {
	CLI            string
	ACPApp         string
	Plugin         string
	Profile        string
	PackageManager string
	PNPMVersion    string
}

func PinnedTuple() Tuple {
	return Tuple{
		CLI: PinnedVersion, ACPApp: PinnedVersion, Plugin: PinnedVersion,
		Profile: PinnedVersion, PackageManager: RequiredPNPM, PNPMVersion: PinnedPNPM,
	}
}

func (tuple Tuple) Validate() error {
	actual := []struct {
		name string
		got  string
		want string
	}{
		{"cli", tuple.CLI, PinnedVersion},
		{"acp-app", tuple.ACPApp, PinnedVersion},
		{"plugin", tuple.Plugin, PinnedVersion},
		{"profile", tuple.Profile, PinnedVersion},
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

// TupleVerifier probes one explicitly selected profile without reading or
// modifying credentials.
type TupleVerifier interface {
	VerifyTuple(context.Context, string) (Tuple, error)
}

type StaticTupleVerifier Tuple

func (verifier StaticTupleVerifier) VerifyTuple(context.Context, string) (Tuple, error) {
	tuple := Tuple(verifier)
	return tuple, tuple.Validate()
}

func verifyPinnedTuple(ctx context.Context, verifier TupleVerifier, profile string) (Tuple, error) {
	if verifier == nil {
		return Tuple{}, fmt.Errorf("%w: DSH tuple verifier is unavailable", productruntime.ErrUnavailable)
	}
	tuple, err := verifier.VerifyTuple(ctx, profile)
	if err != nil {
		return Tuple{}, err
	}
	if err := tuple.Validate(); err != nil {
		return Tuple{}, err
	}
	return tuple, nil
}
