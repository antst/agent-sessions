package releaseinstall

import (
	"path/filepath"
	"testing"
)

// releaseTestTempDir returns a real, canonical directory suitable for tests
// that exercise the production no-follow release-root walk. Darwin spells its
// native temporary directory through /var, which is itself a symlink to
// /private/var; leaving that ambient alias in the fixture would make the
// fixture fail before the deliberate symlink cases under test are reached.
func releaseTestTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize release test root %s: %v", root, err)
	}
	return canonical
}
