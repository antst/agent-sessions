// Package testutil provides cross-package test infrastructure.
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/socketpath"
)

// ShortSocketRoot creates a private test directory whose longest declared
// socket path fits the current platform without embedding testing.T's name.
func ShortSocketRoot(t testing.TB, pattern, longestRelative string) string {
	t.Helper()
	parents := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, candidate := range []string{os.TempDir(), "/tmp"} {
		parent, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		parent = filepath.Clean(parent)
		if _, duplicate := seen[parent]; duplicate {
			continue
		}
		seen[parent] = struct{}{}
		parents = append(parents, parent)
	}
	for _, parent := range parents {
		root, err := os.MkdirTemp(parent, pattern)
		if err != nil {
			continue
		}
		if socketpath.Fits(filepath.Join(root, longestRelative)) {
			t.Cleanup(func() {
				if err := os.RemoveAll(root); err != nil {
					t.Errorf("remove short socket test root: %v", err)
				}
			})
			return root
		}
		_ = os.RemoveAll(root)
	}
	t.Fatalf("cannot create a test root fitting socket path %q (platform limit %d)", longestRelative, socketpath.Limit())
	return ""
}
