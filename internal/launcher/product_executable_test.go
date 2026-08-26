package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProductExecutableEnvironmentAcceptsPATHCommandName(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "test-native")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	for _, environmentName := range []string{"CODEX_PEER_CODEX_BIN", "CLAUDE_PEER_CLAUDE_BIN", "QWEN_PEER_QWEN_BIN"} {
		t.Run(environmentName, func(t *testing.T) {
			t.Setenv(environmentName, "test-native")
			path, err := productExecutable(environmentName, "missing-fallback")
			if err != nil || path != executable {
				t.Fatalf("resolved executable = %q, %v", path, err)
			}
		})
	}
}

func TestProductExecutableEnvironmentRejectsMissingOverride(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("QWEN_PEER_QWEN_BIN", "missing-qwen")
	_, err := qwenExecutable()
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 127 {
		t.Fatalf("missing configured executable = %v", err)
	}
}
