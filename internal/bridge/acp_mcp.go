package bridge

import (
	"errors"
	"os"
	"sort"
	"strings"
)

// nativeRuntimeACPMCPServer describes one stdio MCP server backed by the
// currently executing Agent Sessions runtime. Product adapters use this one
// wire contract instead of independently spelling ACP command, argv, and
// environment shapes.
func nativeRuntimeAgentSessionsMCPServer(role string, environment map[string]string) (map[string]any, error) {
	if strings.TrimSpace(role) == "" {
		return nil, errors.New("ACP MCP server requires a runtime role")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("resolve lane MCP runtime")
	}
	server := map[string]any{
		"name": "agent_sessions", "command": executable, "args": []any{role},
	}
	if len(environment) == 0 {
		return server, nil
	}
	names := make([]string, 0, len(environment))
	for variable := range environment {
		names = append(names, variable)
	}
	sort.Strings(names)
	values := make([]any, 0, len(names))
	for _, variable := range names {
		values = append(values, map[string]any{"name": variable, "value": environment[variable]})
	}
	server["env"] = values
	return server, nil
}
