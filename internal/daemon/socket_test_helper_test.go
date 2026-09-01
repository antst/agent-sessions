package daemon

import (
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/testutil"
)

func shortDaemonTestRoot(t testing.TB) string {
	t.Helper()
	return testutil.ShortSocketRoot(t, "as-", filepath.Join("run", "daemon.sock"))
}
