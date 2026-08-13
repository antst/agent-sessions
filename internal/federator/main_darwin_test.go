//go:build darwin

package federator

import (
	"os"
	"testing"
)

// Darwin's default per-user TMPDIR leaves too little room for the Unix socket
// suffixes exercised by the federator tests. Production runtime-root selection
// is unchanged; only this package's test-created paths use the compact root.
func TestMain(m *testing.M) {
	if err := os.Setenv("TMPDIR", "/tmp"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
