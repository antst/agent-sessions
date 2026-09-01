package dsh

import (
	"fmt"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type SandboxMode string
type ApprovalMode string

const (
	SandboxWorkspaceWrite   SandboxMode  = "workspace-write"
	SandboxDangerFullAccess SandboxMode  = "danger-full-access"
	ApprovalAsk             ApprovalMode = "ask"
	ApprovalNever           ApprovalMode = "never"
)

// NativePolicy is an exact DSH preset/approval pair. Network and process
// policy are intentionally absent: the DSH sandbox does not represent them.
type NativePolicy struct {
	Sandbox  SandboxMode
	Approval ApprovalMode
}

func MapPermission(mode permissionmode.Mode) (NativePolicy, error) {
	switch mode {
	case permissionmode.Default:
		return NativePolicy{Sandbox: SandboxWorkspaceWrite, Approval: ApprovalAsk}, nil
	case permissionmode.BypassPermissions:
		return NativePolicy{Sandbox: SandboxDangerFullAccess, Approval: ApprovalNever}, nil
	default:
		return NativePolicy{}, fmt.Errorf("%w: DSH cannot exactly represent %q", productruntime.ErrUnsupportedPolicy, mode)
	}
}
