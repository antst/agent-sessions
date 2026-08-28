package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShortSocketRootCanonicalizesAmbientTempAlias(t *testing.T) {
	realParent := t.TempDir()
	canonicalParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(t.TempDir(), "temp-alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", aliasParent)

	root := ShortSocketRoot(t, "asr-", "daemon.sock")
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if root != canonical {
		t.Fatalf("short socket root retained an ambient path alias: root=%q canonical=%q", root, canonical)
	}
	if filepath.Dir(root) != canonicalParent {
		t.Fatalf("short socket root parent = %q, want canonical ambient parent %q", filepath.Dir(root), canonicalParent)
	}
}
