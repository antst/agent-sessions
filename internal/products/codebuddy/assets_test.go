package codebuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParentAssetsInjectOneCodeBuddyConnectorAndExplainTerminalNotice(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", ".."))
	mcpBytes, err := os.ReadFile(filepath.Join(root, "integrations", ProductID, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	server, ok := manifest.Servers["agent_sessions"]
	if !ok || len(manifest.Servers) != 1 || server.Type != "stdio" || server.Command != "agent-sessions" ||
		len(server.Args) != 4 || server.Args[0] != "connector" || server.Args[1] != ProductID || server.Args[2] != "--release-identity" {
		t.Fatalf("MCP manifest = %#v", manifest)
	}
	if strings.Contains(strings.ToLower(string(mcpBytes)), "password") || strings.Contains(strings.ToLower(string(mcpBytes)), "sidecar") {
		t.Fatal("parent MCP manifest contains peer credential/sidecar material")
	}
	for _, relative := range []string{
		"CODEBUDDY.md",
		filepath.Join(".codebuddy", "commands", "agent-sessions.md"),
		filepath.Join(".codebuddy", "skills", "agent-sessions", "SKILL.md"),
		filepath.Join(".codebuddy", "skills", "codebuddy-lane", "SKILL.md"),
	} {
		body, err := os.ReadFile(filepath.Join(root, "integrations", ProductID, relative))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(body))
		if !strings.Contains(text, "agent sessions") || !strings.Contains(text, "fail") && !strings.Contains(text, "inactive") && !strings.Contains(text, "unavailable") {
			t.Fatalf("asset %s omits Agent Sessions fail-closed guidance", relative)
		}
	}
	laneSkill, _ := os.ReadFile(filepath.Join(root, "integrations", ProductID, ".codebuddy", "skills", "codebuddy-lane", "SKILL.md"))
	if text := strings.ToLower(string(laneSkill)); !strings.Contains(text, "terminal notice") || !strings.Contains(text, "collect") {
		t.Fatal("CodeBuddy lane skill omits terminal-notice collection behavior")
	}
}
