package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeEntryPayloadsRemainOneExactContract(t *testing.T) {
	root := filepath.Join("..", "..")
	want, err := os.ReadFile(filepath.Join(root, "scripts", "native-entry"))
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"grok/scripts/native-entry", "qwen/scripts/native-entry"} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("native entry %s = %v, %v", relative, info, statErr)
		}
		if string(body) != string(want) {
			t.Errorf("native entry %s drifted from the shared contract", relative)
		}
	}
}
