package daemon

import (
	"reflect"
	"testing"
)

func TestRemoteLaneCLIRequiresCanonicalHostProductSeparator(t *testing.T) {
	host, product, command, err := parseRemoteLaneCLI([]string{
		"--host", "workstation-b", "--product", "qwen", "--", "start", "--name", "review-a", "-",
	})
	if err != nil || host != "workstation-b" || product != "qwen" ||
		!reflect.DeepEqual(command, []string{"start", "--name", "review-a", "-"}) {
		t.Fatalf("canonical remote lane parse = host %q product %q command %q, %v", host, product, command, err)
	}
	for _, arguments := range [][]string{
		{"--product", "qwen", "--", "doctor"},
		{"--host", "workstation-b", "--", "doctor"},
		{"--host", "workstation-b", "--product", "unknown", "--", "doctor"},
		{"--host", "workstation-b", "--product", "qwen", "doctor"},
		{"--host", "workstation-b", "--product", "qwen", "--"},
		{"--host", "workstation-b", "--host", "workstation-c", "--product", "qwen", "--", "doctor"},
		{"--host", "workstation-b", "--product", "qwen", "--product", "codex", "--", "doctor"},
		{"--host", "--product", "qwen", "--", "doctor"},
		{"--host", "workstation-b", "--product", "--", "doctor"},
		{"--runtime-dir", "/tmp/alternate", "--host", "workstation-b", "--product", "qwen", "--", "doctor"},
	} {
		if _, _, _, err := parseRemoteLaneCLI(arguments); err == nil {
			t.Errorf("invalid remote lane arguments were accepted: %q", arguments)
		}
	}
}
