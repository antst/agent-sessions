package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteJSONAtomicProducesOwnerOnlyDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteJSONAtomic(path, map[string]any{"ready": true}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{\n  \"ready\": true\n}\n" {
		t.Fatalf("body = %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}
