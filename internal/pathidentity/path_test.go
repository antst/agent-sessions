package pathidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingDirectoryResolvesRealTarget(t *testing.T) {
	realDirectory := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := ExistingDirectory(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realDirectory)
	if err != nil || got != want {
		t.Fatalf("existing directory = %q, %v; want %q", got, err, want)
	}
}

func TestFuturePathAllowsMissingLeafAndRejectsMutableSymlink(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "future", "profile")
	got, err := FuturePath(missing)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || got != filepath.Join(canonicalRoot, "future", "profile") {
		t.Fatalf("future path = %q, %v", got, err)
	}
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := FuturePath(filepath.Join(link, "profile")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("mutable symlink error = %v", err)
	}
}
