package pi

import (
	"errors"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestMapPermissionFailsClosed(t *testing.T) {
	restricted, err := MapPermission(permissionmode.Default)
	if err != nil || restricted.Name != "restricted-tools" || !reflect.DeepEqual(restricted.Args, []string{"--tools", "read,grep,find,ls"}) {
		t.Fatalf("default mapping = %#v, %v", restricted, err)
	}
	bypass, err := MapPermission(permissionmode.BypassPermissions)
	if err != nil || bypass.Name != "full-tools" || len(bypass.Args) != 0 {
		t.Fatalf("bypass mapping = %#v, %v", bypass, err)
	}
	if _, err := MapPermission(permissionmode.Mode("future")); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unknown policy error = %v", err)
	}
}
