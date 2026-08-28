package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalTempDirReturnsRealPrivateFixtureRoot(t *testing.T) {
	root := CanonicalTempDir(t)
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if root != canonical {
		t.Fatalf("canonical temp root retained a path alias: root=%q canonical=%q", root, canonical)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("canonical temp root mode = %v, want real directory", info.Mode())
	}
}

func TestShortSocketRootCanonicalizesAmbientTempAlias(t *testing.T) {
	realParent, err := os.MkdirTemp("/tmp", "asrp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(realParent) })
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
