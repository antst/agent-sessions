package bridge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyGrokPluginInspectionRequiresExactUserPluginAndMCP(t *testing.T) {
	root := t.TempDir()
	nativeEntry := filepath.Join(root, "scripts", "native-entry")
	if err := os.MkdirAll(filepath.Dir(nativeEntry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeEntry, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	valid := map[string]any{
		"plugins": []any{map[string]any{
			"name":    "agent-sessions",
			"scope":   "user",
			"path":    root,
			"enabled": true,
			"provides": map[string]any{
				"skills":     2,
				"mcpServers": 1,
			},
		}},
		"mcpServers": []any{map[string]any{
			"name":      "agent_sessions",
			"transport": "stdio",
			"target":    nativeEntry,
			"source": map[string]any{
				"type":        "plugin",
				"plugin_name": "agent-sessions",
				"path":        root,
			},
		}},
	}
	encode := func(t *testing.T, inspection map[string]any) []byte {
		t.Helper()
		payload, err := json.Marshal(inspection)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	clone := func(t *testing.T) map[string]any {
		t.Helper()
		var copied map[string]any
		if err := json.Unmarshal(encode(t, valid), &copied); err != nil {
			t.Fatal(err)
		}
		return copied
	}

	if err := verifyGrokPluginInspection(bytes.NewReader(encode(t, valid)), root); err != nil {
		t.Fatalf("valid Grok inspection rejected: %v", err)
	}
	if err := verifyGrokPluginInspection(bytes.NewReader(append(encode(t, valid), []byte("\n{}")...)), root); err == nil ||
		!strings.Contains(err.Error(), "trailing JSON value") {
		t.Fatalf("trailing Grok inspection error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "disabled plugin",
			mutate: func(inspection map[string]any) {
				inspection["plugins"].([]any)[0].(map[string]any)["enabled"] = false
			},
			want: "not the enabled user plugin",
		},
		{
			name: "project plugin",
			mutate: func(inspection map[string]any) {
				inspection["plugins"].([]any)[0].(map[string]any)["scope"] = "project"
			},
			want: "not the enabled user plugin",
		},
		{
			name: "missing messaging skill",
			mutate: func(inspection map[string]any) {
				inspection["plugins"].([]any)[0].(map[string]any)["provides"].(map[string]any)["skills"] = 1
			},
			want: "not the enabled user plugin",
		},
		{
			name: "wrong MCP target",
			mutate: func(inspection map[string]any) {
				inspection["mcpServers"].([]any)[0].(map[string]any)["target"] = root
			},
			want: "not sourced from the expected user plugin",
		},
		{
			name: "wrong MCP source",
			mutate: func(inspection map[string]any) {
				inspection["mcpServers"].([]any)[0].(map[string]any)["source"].(map[string]any)["type"] = "mcpJson"
			},
			want: "not sourced from the expected user plugin",
		},
		{
			name: "duplicate plugin",
			mutate: func(inspection map[string]any) {
				plugins := inspection["plugins"].([]any)
				inspection["plugins"] = append(plugins, plugins[0])
			},
			want: "2 agent-sessions plugin rows",
		},
		{
			name: "duplicate MCP",
			mutate: func(inspection map[string]any) {
				servers := inspection["mcpServers"].([]any)
				inspection["mcpServers"] = append(servers, servers[0])
			},
			want: "2 agent_sessions MCP rows",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := clone(t)
			test.mutate(inspection)
			err := verifyGrokPluginInspection(bytes.NewReader(encode(t, inspection)), root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verification error = %v, want text %q", err, test.want)
			}
		})
	}
}
