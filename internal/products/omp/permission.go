// Package omp owns Oh My Pi-specific runtime policy and constructors.
package omp

import (
	"fmt"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/pifamily"
)

// MapPermission fails closed when the requested shared mode cannot run
// unattended. OMP's always-ask policy requires extension_ui_request/response
// mediation, which the frozen host permission contract does not expose.
func MapPermission(mode permissionmode.Mode) (pifamily.PermissionPolicy, error) {
	switch mode {
	case permissionmode.Default:
		return pifamily.PermissionPolicy{}, fmt.Errorf("%w: OMP default requires unavailable RPC approval mediation", productruntime.ErrUnsupportedPolicy)
	case permissionmode.BypassPermissions:
		return pifamily.NewPermissionPolicy("yolo", "--approval-mode=yolo"), nil
	default:
		return pifamily.PermissionPolicy{}, fmt.Errorf("%w: OMP cannot map %q", productruntime.ErrUnsupportedPolicy, mode)
	}
}
