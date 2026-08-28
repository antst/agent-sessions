package releaseinstall

import (
	"testing"

	"github.com/antst/agent-sessions/internal/testutil"
)

// releaseTestTempDir returns a real, canonical directory suitable for tests
// that exercise the production no-follow release-root walk. Darwin spells its
// native temporary directory through /var, which is itself a symlink to
// /private/var; leaving that ambient alias in the fixture would make the
// fixture fail before the deliberate symlink cases under test are reached.
func releaseTestTempDir(t *testing.T) string {
	t.Helper()
	return testutil.CanonicalTempDir(t)
}
