// Package pi owns Pi-specific runtime policy and constructors.
package pi

import (
	"fmt"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/pifamily"
)

// MapPermission is fail closed. Pi has no approval-prompt model, so the normal
// policy exposes only read/search tools. Full native tools require the explicit
// Agent Sessions bypassPermissions mode.
func MapPermission(mode permissionmode.Mode) (pifamily.PermissionPolicy, error) {
	switch mode {
	case permissionmode.Default:
		return pifamily.NewPermissionPolicy("restricted-tools", "--tools", "read,grep,find,ls"), nil
	case permissionmode.BypassPermissions:
		return pifamily.NewPermissionPolicy("full-tools"), nil
	default:
		return pifamily.PermissionPolicy{}, fmt.Errorf("%w: Pi cannot map %q", productruntime.ErrUnsupportedPolicy, mode)
	}
}
