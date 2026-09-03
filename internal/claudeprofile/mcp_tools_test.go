package claudeprofile

import (
	"reflect"
	"testing"
)

func TestManagedAgentSessionsMCPToolsAreOriginQualified(t *testing.T) {
	want := []string{
		"mcp__plugin_agent-sessions_agent_sessions__list_peers",
		"mcp__plugin_agent-sessions_agent_sessions__send_message",
		"mcp__plugin_agent-sessions_agent_sessions__lane",
	}
	if got := ManagedAgentSessionsMCPTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("managed Agent Sessions MCP tools = %v, want %v", got, want)
	}
	if ProjectMCPServerName != "agent_sessions" {
		t.Fatalf("project MCP server name = %q", ProjectMCPServerName)
	}
}
