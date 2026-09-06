// Package testutil provides cross-package test infrastructure.
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/sessionbus/internal/socketpath"
)

// ShortSocketRoot creates a private test directory whose longest declared
// socket path fits the current platform without embedding testing.T's name.
func ShortSocketRoot(t testing.TB, pattern, longestRelative string) string {
	t.Helper()
	parents := []string{os.TempDir()}
	if filepath.Clean(os.TempDir()) != "/tmp" {
		parents = append(parents, "/tmp")
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
