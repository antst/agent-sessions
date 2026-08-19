package launcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCodexHomeCreatesConfiguredDirectoryBeforeProfileResolution(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	if err := os.MkdirAll(realRoot, 0700); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(aliasRoot, "missing", "codex")
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := filepath.EvalSymlinks(codexHome); err == nil {
		t.Fatal("test CODEX_HOME unexpectedly existed before bootstrap")
	}
	if err := ensureCodexHome(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(codexHome)
	if err != nil || !info.IsDir() {
		t.Fatalf("fresh CODEX_HOME was not established: info=%v err=%v", info, err)
	}
	if err := ensureCodexHome(); err != nil {
		t.Fatalf("existing CODEX_HOME was not idempotent: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(codexHome)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realRoot, "missing", "codex")
	if resolved != want {
		t.Fatalf("fresh aliased CODEX_HOME resolved to %q, want %q", resolved, want)
	}
}

func TestEnsureCodexHomeRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-file")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", path)
	if err := ensureCodexHome(); err == nil {
		t.Fatal("regular-file CODEX_HOME was accepted")
	}
}
