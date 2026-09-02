package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSubmitQwenNativeNameUsesRemoteInputCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := submitQwenNativeName(path, "product owned name"); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var command struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if scanner := bufio.NewScanner(file); !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &command) != nil {
		t.Fatal("Qwen native name command was not framed")
	}
	if command.Type != "submit" || command.Text != "/rename product owned name" {
		t.Fatalf("Qwen native name command = %+v", command)
	}
}

func TestQwenSessionRegistrationIsExactNativeReadinessBoundary(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	const wanted = "11111111-2222-4333-8444-555555555555"
	if qwenSessionRegistered(home, wanted) {
		t.Fatal("missing Qwen registry row reported ready")
	}
	for name, body := range map[string]string{
		"ignored.txt": `{"sessionId":"` + wanted + `"}`,
		"broken.json": `{`,
		"other.json":  `{"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`,
	} {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if qwenSessionRegistered(home, wanted) {
		t.Fatal("unrelated Qwen registry rows reported ready")
	}
	if err := os.WriteFile(filepath.Join(sessions, "native.json"), []byte(`{"sessionId":"`+wanted+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !qwenSessionRegistered(home, wanted) {
		t.Fatal("exact Qwen product registry row did not report ready")
	}
}
