package pathidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingNoFollowIdentityPreservesCanonicalTypeAndMode(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "payload")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := ExistingNoFollow(file)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Path != canonical || identity.Kind != KindRegular || identity.Mode.Perm() != 0o600 {
		t.Fatalf("file identity = %+v, want path=%q kind=%q mode=0600", identity, canonical, KindRegular)
	}
	directoryIdentity, err := ExistingNoFollow(directory)
	if err != nil || directoryIdentity.Kind != KindDirectory || directoryIdentity.Mode.Perm() != 0o700 {
		t.Fatalf("directory identity = %+v, %v", directoryIdentity, err)
	}
}

func TestExistingNoFollowIdentityRejectsIntermediateAndLeafSymlinks(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(realDirectory, "payload")
	if err := os.WriteFile(realFile, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(root, "intermediate")
	leaf := filepath.Join(root, "leaf")
	if err := os.Symlink(realDirectory, intermediate); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(realFile, leaf); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(intermediate, "payload"), leaf} {
		if _, err := ExistingNoFollow(path); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("no-follow identity accepted %q: %v", path, err)
		}
	}
}
