package kilocode

import (
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestKiloPermissionMappingIsExactAndFailsClosed(t *testing.T) {
	tests := []struct {
		mode   permissionmode.Mode
		action string
	}{
		{mode: permissionmode.Default, action: "ask"},
		{mode: permissionmode.BypassPermissions, action: "allow"},
	}
	for _, test := range tests {
		rules, err := MapPermission(test.mode)
		if err != nil || len(rules) != 1 || rules[0].Permission != "*" || rules[0].Pattern != "*" || rules[0].Action != test.action {
			t.Fatalf("%s = %#v, %v", test.mode, rules, err)
		}
	}
	if _, err := MapPermission(permissionmode.Mode("accept-edits")); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unknown mode = %v", err)
	}
}
