//go:build darwin

package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/antst/agent-sessions/internal/socketpath"
)

func TestDarwinProductionControlEndpointUsesLiteralShortTmpRoot(t *testing.T) {
	t.Setenv("TMPDIR", "/private/var/folders/an/intentionally/long/vendor-selected/temp/root")
	t.Setenv("XDG_RUNTIME_DIR", "/private/var/folders/xdg-must-not-select-the-darwin-endpoint")

	got, err := productionControlEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/agent-sessions-%d/daemon.sock", os.Getuid())
	if got != want {
		t.Fatalf("productionControlEndpoint() = %q, want literal fixed Darwin endpoint %q", got, want)
	}
	if len([]byte(got)) > socketpath.Limit() {
		t.Fatalf("Darwin endpoint is %d bytes, exceeds AF_UNIX limit %d", len([]byte(got)), socketpath.Limit())
	}
}
