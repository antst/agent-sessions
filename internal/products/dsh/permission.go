package dsh

import (
	"fmt"

	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/productruntime"
)

type SandboxMode string
type ApprovalMode string

const (
	SandboxWorkspaceWrite   SandboxMode  = "workspace-write"
	SandboxDangerFullAccess SandboxMode  = "danger-full-access"
	ApprovalNever           ApprovalMode = "never"
)

// NativePolicy is an exact DSH preset/approval pair. Network and process
// policy are intentionally absent: the DSH sandbox does not represent them.
type NativePolicy struct {
	Sandbox  SandboxMode
	Approval ApprovalMode
	Preset   string
}

func MapPermission(mode permissionmode.Mode) (NativePolicy, error) {
	switch mode {
	case permissionmode.Default:
		return NativePolicy{Sandbox: SandboxWorkspaceWrite, Approval: ApprovalNever, Preset: "workspace-write-noninteractive"}, nil
	case permissionmode.BypassPermissions:
		return NativePolicy{Sandbox: SandboxDangerFullAccess, Approval: ApprovalNever, Preset: "danger-full-access"}, nil
	default:
		return NativePolicy{}, fmt.Errorf("%w: DSH cannot exactly represent %q", productruntime.ErrUnsupportedPolicy, mode)
	}
}
