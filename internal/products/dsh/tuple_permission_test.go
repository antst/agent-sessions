package dsh

import (
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestExactTupleRejectsEveryMismatchedMemberAndRequiresPNPM(t *testing.T) {
	exact := PinnedTuple()
	if err := exact.Validate(); err != nil {
		t.Fatalf("pinned tuple: %v", err)
	}
	mutations := []struct {
		name string
		edit func(*Tuple)
	}{
		{"cli", func(tuple *Tuple) { tuple.CLI = "0.1.2-alpha.4" }},
		{"package-manager", func(tuple *Tuple) { tuple.PackageManager = "npm" }},
		{"pnpm-version", func(tuple *Tuple) { tuple.PNPMVersion = "10.27.0" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := exact
			mutation.edit(&candidate)
			if err := candidate.Validate(); !errors.Is(err, productruntime.ErrIncompatible) {
				t.Fatalf("Validate() error = %v, want ErrIncompatible", err)
			}
		})
	}
}

func TestPermissionMapperIsExactAndFailClosed(t *testing.T) {
	tests := []struct {
		mode permissionmode.Mode
		want NativePolicy
	}{
		{permissionmode.Default, NativePolicy{Sandbox: SandboxWorkspaceWrite, Approval: ApprovalNever, Preset: "workspace-write-noninteractive"}},
		{permissionmode.BypassPermissions, NativePolicy{Sandbox: SandboxDangerFullAccess, Approval: ApprovalNever, Preset: "danger-full-access"}},
	}
	for _, test := range tests {
		got, err := MapPermission(test.mode)
		if err != nil || got != test.want {
			t.Fatalf("MapPermission(%q) = %+v, %v; want %+v", test.mode, got, err, test.want)
		}
	}
	if _, err := MapPermission(permissionmode.Mode("restricted")); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unknown permission error = %v, want ErrUnsupportedPolicy", err)
	}
}
