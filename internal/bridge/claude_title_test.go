package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeNativeSessionTitleReadsProductTranscript(t *testing.T) {
	root := t.TempDir()
	id := "00000000-0000-4000-8000-000000000123"
	project := filepath.Join(root, "projects", "workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"sessionId":"` + id + `","type":"user"}` + "\n" +
		`{"sessionId":"` + id + `","type":"custom-title","customTitle":"product worker"}` + "\n"
	if err := os.WriteFile(filepath.Join(project, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	name, ok := ClaudeNativeSessionTitle(root, id)
	if !ok || name != "product worker" {
		t.Fatalf("title = %q, ok=%v", name, ok)
	}
	if _, ok := ClaudeNativeSessionTitle(root, "00000000-0000-4000-8000-000000000999"); ok {
		t.Fatal("unknown Claude UUID was confirmed")
	}
}
