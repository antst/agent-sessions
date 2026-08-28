//go:build !darwin

package bridge

import (
	"os"
	"testing"
)

// TestMain keeps unit tests independent of a production daemon attachment
// inherited from the developer's current peer session.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("AGENT_SESSIONS_AGENT_RUNTIME_DIR"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
