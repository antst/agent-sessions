package bridge

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeRuntimeACPMCPServerUsesDeterministicSharedContract(t *testing.T) {
	t.Parallel()

	server, err := nativeRuntimeAgentSessionsMCPServer("mcp", map[string]string{
		"SECOND": "two", "FIRST": "one",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := stringValue(server["command"])
	if !filepath.IsAbs(command) || !reflect.DeepEqual(server["args"], []any{"mcp"}) ||
		!reflect.DeepEqual(server["env"], []any{
			map[string]any{"name": "FIRST", "value": "one"},
			map[string]any{"name": "SECOND", "value": "two"},
		}) {
		t.Fatalf("native ACP MCP server = %#v", server)
	}
	withoutEnvironment, err := nativeRuntimeAgentSessionsMCPServer("grok-mcp", nil)
	if err != nil || withoutEnvironment["env"] != nil || !reflect.DeepEqual(withoutEnvironment["args"], []any{"grok-mcp"}) {
		t.Fatalf("environment-free native ACP MCP server = %#v, %v", withoutEnvironment, err)
	}
}
