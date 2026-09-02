package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPreseedQwenNativeNameUsesRemoteInputCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preseedQwenNativeName(path, "product owned name"); err != nil {
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
