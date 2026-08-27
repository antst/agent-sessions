package releaseinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContentIdentityBindsTreeBytesModesAndIgnoresOnlyManifest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bin", "agent-sessions")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := ContentIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"content_identity":"self"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	withManifest, err := ContentIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if withManifest != first {
		t.Fatalf("manifest changed self-referential content identity: %q / %q", first, withManifest)
	}
	if err := os.WriteFile(path, []byte("second"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := ContentIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("release byte change retained the same content identity")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := ContentIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if modeChanged == second {
		t.Fatal("release mode change retained the same content identity")
	}
}

func TestContentIdentityRejectsSymlinkedReleaseEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("payload", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := ContentIdentity(root); err == nil {
		t.Fatal("content identity accepted a symlinked release entry")
	}
}
