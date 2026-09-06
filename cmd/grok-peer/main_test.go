package main

import (
	"context"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/wrappers/host"
)

func TestLaneModeRejectsArguments(t *testing.T) {
	t.Setenv(host.TokenEnv, "token")
	if err := run(context.Background(), []string{"mcp"}); err == nil || err.Error() != "lane mode accepts no arguments" {
		t.Fatalf("run = %v", err)
	}
}

func TestMCPRequiresManagedLauncher(t *testing.T) {
	t.Setenv("AGENTBUS_LANE_SOCKET", "")
	err := runMCP(context.Background())
	if err == nil || !strings.Contains(err.Error(), "start Grok with grok-peer") {
		t.Fatalf("runMCP = %v", err)
	}
}
