package bridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeEntryPayloadsRemainOneExactContract(t *testing.T) {
	root := filepath.Join("..", "..")
	want, err := os.ReadFile(filepath.Join(root, "scripts", "native-entry"))
	if err != nil {
		t.Fatal(err)
	}
	contract := string(want)
	for _, required := range []string{"AGENT_SESSIONS_HOST_BINARY", "host/current/bin/agent-sessions", "command -v agent-sessions"} {
		if !strings.Contains(contract, required) {
			t.Errorf("native entry omits canonical host selector %q", required)
		}
	}
	for _, forbidden := range []string{"native-runtime-path", "AGENT_SESSIONS_NATIVE_RUNTIME", "agent-session-runtime"} {
		if strings.Contains(contract, forbidden) {
			t.Errorf("native entry retains legacy runtime selector %q", forbidden)
		}
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

func TestNativeEntryExecutesTheExactUnifiedHostBinary(t *testing.T) {
	root := filepath.Join("..", "..")
	entry := filepath.Join(root, "scripts", "native-entry")
	host := filepath.Join(t.TempDir(), "agent-sessions")
	if err := os.WriteFile(host, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(entry, "connector", "qwen", "mcp")
	command.Env = append(os.Environ(), "AGENT_SESSIONS_HOST_BINARY="+host)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run native entry: %v: %s", err, output)
	}
	if string(output) != "connector\nqwen\nmcp\n" {
		t.Fatalf("native entry argv = %q", output)
	}
}
