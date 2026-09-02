package bridge

import (
	"os"
	"testing"
)

func shortSocketTestRoot(t *testing.T, prefix string) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}
