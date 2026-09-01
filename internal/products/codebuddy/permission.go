package codebuddy

import (
	"fmt"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type NativePermission struct {
	Mode string
	Env  []productruntime.EnvVar
}

// MapPermission is the ordinary fail-closed product mapping. It never infers
// the explicit sandbox authorization needed by CodeBuddy's full bypass mode.
func MapPermission(mode permissionmode.Mode) (NativePermission, error) {
	return MapLanePermission(mode, false)
}

// MapLanePermission maps an explicitly scoped lane permission. The native
// default preserves CodeBuddy's ordinary prompts. Full bypass is representable
// only for an explicitly sandbox-authorized lane and never becomes a peer
// setting.
func MapLanePermission(mode permissionmode.Mode, allowSandboxBypass bool) (NativePermission, error) {
	switch mode {
	case permissionmode.Default:
		return NativePermission{Mode: "default"}, nil
	case permissionmode.BypassPermissions:
		if !allowSandboxBypass {
			return NativePermission{}, fmt.Errorf("%w: CodeBuddy bypass requires explicit sandbox lane authorization", productruntime.ErrUnsupportedPolicy)
		}
		return NativePermission{
			Mode: "bypassPermissions",
			Env:  []productruntime.EnvVar{{Name: SandboxBypassEnv, Value: "1"}},
		}, nil
	default:
		return NativePermission{}, fmt.Errorf("%w: unknown permission mode %q", productruntime.ErrUnsupportedPolicy, mode)
	}
}
