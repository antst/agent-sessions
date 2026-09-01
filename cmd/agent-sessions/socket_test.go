package main

import (
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/testutil"
)

// shortDaemonTestRoot avoids testing.T.TempDir's test-name component in paths
// that will contain a Unix-domain socket. Darwin permits only 103 pathname
// bytes in sockaddr_un.sun_path.
func shortDaemonTestRoot(t testing.TB) string {
	t.Helper()
	return testutil.ShortSocketRoot(t, "as-", filepath.Join("run", "c-00000000000000000000.sock"))
}
