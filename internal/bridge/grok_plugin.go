package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func runGrokPluginVerify(args []string) int {
	if len(args) != 2 || args[0] != "--root" || args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: agent-session-runtime grok-plugin-verify --root <user-plugin-root>")
		return 2
	}
	if err := verifyGrokPluginInspection(os.Stdin, args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok-plugin-verify: %v\n", err)
		return 1
	}
	return 0
}

func verifyGrokPluginInspection(reader io.Reader, expectedRoot string) error {
	root, err := canonicalGrokPluginPath(expectedRoot)
	if err != nil {
		return fmt.Errorf("resolve expected user plugin root: %w", err)
	}
	wantedTarget, err := canonicalGrokPluginPath(filepath.Join(root, "scripts", "native-entry"))
	if err != nil {
		return fmt.Errorf("resolve expected native entry: %w", err)
	}
	var inspection map[string]any
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&inspection); err != nil {
		return fmt.Errorf("decode grok inspect --json: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode grok inspect --json: trailing JSON value")
		}
		return fmt.Errorf("decode grok inspect --json trailing data: %w", err)
	}
	if err := verifyGrokUserPlugin(inspection["plugins"], root); err != nil {
		return err
	}
	return verifyGrokPluginMCP(inspection["mcpServers"], root, wantedTarget)
}

func verifyGrokUserPlugin(rawPlugins any, root string) error {
	pluginMatches := 0
	for _, raw := range grokInspectionSlice(rawPlugins) {
		plugin, _ := raw.(map[string]any)
		if stringValue(plugin["name"]) != "agent-sessions" {
			continue
		}
		pluginMatches++
		path, pathErr := canonicalGrokPluginPath(stringValue(plugin["path"]))
		enabled, enabledOK := plugin["enabled"].(bool)
		provides, _ := plugin["provides"].(map[string]any)
		if pathErr != nil || path != root || stringValue(plugin["scope"]) != "user" ||
			!enabledOK || !enabled || intValue(provides["mcpServers"]) != 1 {
			return errors.New("agent-sessions is not the enabled user plugin at the expected path")
		}
	}
	if pluginMatches != 1 {
		return fmt.Errorf("grok inspect returned %d agent-sessions plugin rows", pluginMatches)
	}
	return nil
}

func verifyGrokPluginMCP(rawServers any, root, wantedTarget string) error {
	mcpMatches := 0
	for _, raw := range grokInspectionSlice(rawServers) {
		server, _ := raw.(map[string]any)
		if stringValue(server["name"]) != "agent_sessions" {
			continue
		}
		mcpMatches++
		target, targetErr := canonicalGrokPluginPath(stringValue(server["target"]))
		source, _ := server["source"].(map[string]any)
		sourcePath, sourceErr := canonicalGrokPluginPath(stringValue(source["path"]))
		if targetErr != nil || sourceErr != nil || target != wantedTarget || sourcePath != root ||
			stringValue(server["transport"]) != "stdio" || stringValue(source["type"]) != "plugin" ||
			stringValue(source["plugin_name"]) != "agent-sessions" {
			return errors.New("agent_sessions MCP is not sourced from the expected user plugin")
		}
	}
	if mcpMatches != 1 {
		return fmt.Errorf("grok inspect returned %d agent_sessions MCP rows", mcpMatches)
	}
	return nil
}

func canonicalGrokPluginPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func grokInspectionSlice(value any) []any {
	items, _ := value.([]any)
	return items
}
