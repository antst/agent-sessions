package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestQwenNativeSessionInfoReturnsProductCwdAndTitle(t *testing.T) {
	home := t.TempDir()
	id := "00000000-0000-4000-8000-000000000123"
	cwd := t.TempDir()
	path := filepath.Join(home, "projects", "project", "chats", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{
		{"sessionId": id, "cwd": cwd, "type": "user"},
		{"sessionId": id, "cwd": cwd, "type": "system", "subtype": "custom_title", "systemPayload": map[string]any{"customTitle": "worker", "titleSource": "manual"}},
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := json.NewEncoder(file).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	name, gotCwd, ok := QwenNativeSessionInfo(home, id)
	if !ok || name != "worker" || gotCwd != cwd {
		t.Fatalf("session info = %q, %q, %v", name, gotCwd, ok)
	}
}
