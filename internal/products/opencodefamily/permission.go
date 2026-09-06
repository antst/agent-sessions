package opencodefamily

import (
	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/productruntime"
)

// MapPermissionRules is the common native ruleset representation. Product
// packages own the exported mapper so future fork divergence remains explicit.
func MapPermissionRules(mode permissionmode.Mode) ([]PermissionRule, error) {
	action := "ask"
	switch mode {
	case permissionmode.Default:
	case permissionmode.BypassPermissions:
		action = "allow"
	default:
		return nil, productruntime.ErrUnsupportedPolicy
	}
	return []PermissionRule{{Permission: "*", Pattern: "*", Action: action}}, nil
}
