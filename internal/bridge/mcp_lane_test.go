package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestMCPLaneArgumentsRejectInvalidAndOversizedValues(t *testing.T) {
	if _, err := mcpLaneArguments("not-an-array"); err == nil {
		t.Fatal("non-array lane arguments were accepted")
	}
	if _, err := mcpLaneArguments([]any{"ok", 7}); err == nil {
		t.Fatal("non-string lane argument was accepted")
	}
	if _, err := mcpLaneArguments([]any{"bad\x00value"}); err == nil {
		t.Fatal("NUL lane argument was accepted")
	}
	if _, err := mcpLaneArguments([]any{strings.Repeat("x", 4097)}); err == nil {
		t.Fatal("oversized lane argument was accepted")
	}
	want := []string{"--name", "worker", "-"}
	got, err := mcpLaneArguments([]any{"--name", "worker", "-"})
	if err != nil || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("lane arguments = %#v, %v", got, err)
	}
}

func TestMCPLaneCappedBufferReportsTruncationWithoutShortWrite(t *testing.T) {
	buffer := &mcpCappedBuffer{limit: 4}
	if count, err := buffer.Write([]byte("abcdef")); err != nil || count != 6 {
		t.Fatalf("capped write = %d, %v", count, err)
	}
	if buffer.String() != "cdef" || !buffer.truncated {
		t.Fatalf("capped buffer = %q, truncated=%v", buffer.String(), buffer.truncated)
	}
	if count, err := buffer.Write([]byte("gh")); err != nil || count != 2 || buffer.String() != "efgh" {
		t.Fatalf("rolling capped write = %d, %v, %q", count, err, buffer.String())
	}
}

func TestMCPLaneDeadlineTracksNativeTimeoutAndPreservesLongWaits(t *testing.T) {
	for _, test := range []struct {
		request mcpLaneRequest
		want    time.Duration
	}{
		{request: mcpLaneRequest{command: "doctor"}, want: mcpLaneShortTimeout},
		{request: mcpLaneRequest{command: "wait", args: []string{"wait", "lane", "--timeout", "2700"}}, want: 46 * time.Minute},
		{request: mcpLaneRequest{command: "wait", args: []string{"wait", "lane", "--timeout", "0"}}, want: mcpLaneMaximumTimeout},
		{request: mcpLaneRequest{command: "run", args: []string{"run", "-"}}, want: mcpLaneMaximumTimeout},
	} {
		got, err := mcpLaneRequestTimeout(test.request)
		if err != nil || got != test.want {
			t.Fatalf("MCP deadline for %#v = %s, %v; want %s", test.request, got, err, test.want)
		}
	}
	if _, err := mcpLaneRequestTimeout(mcpLaneRequest{
		command: "wait", args: []string{"wait", "lane", "--timeout", "86400"},
	}); err == nil {
		t.Fatal("MCP accepted a native timeout beyond its transport deadline")
	}
}

func TestLocalMCPLaneProxyUsesAttestedParentEnvironment(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "runtime")
	body := "#!/bin/sh\n" +
		"printf 'argv=%s\\n' \"$*\"\n" +
		"printf 'session=%s product=%s agent=%s remote=%s data=%s claude=%s codex=%s\\n' \"$AGENT_SESSIONS_SESSION_ID\" \"$AGENT_SESSIONS_PRODUCT\" \"$AGENT_SESSIONS_AGENT_RUNTIME_DIR\" \"$AGENT_SESSIONS_REMOTE_PARENT_CONTEXT\" \"$AGENT_SESSIONS_STATE_ROOT\" \"$CLAUDE_PEER_CLAUDE_CONFIG_DIR\" \"$CODEX_HOME\" >&2\n"
	if err := os.WriteFile(fake, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	previous := mcpLaneRuntimeExecutable
	mcpLaneRuntimeExecutable = func() (string, error) { return fake, nil }
	t.Cleanup(func() { mcpLaneRuntimeExecutable = previous })
	t.Setenv("AGENT_SESSIONS_REMOTE_PARENT_CONTEXT", "stale-remote-parent")
	paths := nativePaths{
		dataRoot: filepath.Join(root, "data"), claudeRoot: filepath.Join(root, "claude"),
		codexHome: filepath.Join(root, "codex"),
	}
	stdout, stderr := &mcpCappedBuffer{limit: 4096}, &mcpCappedBuffer{limit: 4096}
	exit, err := runLocalMCPParentLane(context.Background(), paths, filepath.Join(root, "agent"), federator.ParentContext{
		SessionID: "parent-session", Product: "codex",
	}, "claude-lane", []string{"doctor", "--json"}, "", stdout, stderr)
	if err != nil || exit != 0 {
		t.Fatalf("local MCP lane proxy = %d, %v: %s", exit, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "argv=claude-lane doctor --json") {
		t.Fatalf("runtime argv = %q", stdout.String())
	}
	want := "session=parent-session product=codex agent=" + filepath.Join(root, "agent") +
		" remote= data=" + paths.dataRoot + " claude=" + paths.claudeRoot + " codex=" + paths.codexHome
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("runtime environment = %q, want %q", stderr.String(), want)
	}
}

func TestNativeMCPPublishesAttestedLaneProxy(t *testing.T) {
	var lane map[string]any
	for _, definition := range nativeToolDefinitions {
		if stringValue(definition["name"]) == "lane" {
			lane = definition
			break
		}
	}
	if lane == nil {
		t.Fatal("native MCP omitted lane tool")
	}
	schema, _ := lane["inputSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if properties["product"] == nil || properties["command"] == nil || properties["session_id"] == nil {
		t.Fatalf("lane tool schema = %#v", schema)
	}
}

func TestEveryProductMCPPublishesAllFourLaneTargets(t *testing.T) {
	for _, orchestrator := range []string{"codex", "claude", "grok", "qwen"} {
		var lane map[string]any
		for _, definition := range ProductMCPTools(orchestrator) {
			if stringValue(definition["name"]) == "lane" {
				lane = definition
				break
			}
		}
		if lane == nil {
			t.Fatalf("%s MCP omitted the lane tool", orchestrator)
		}
		schema, _ := lane["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		product, _ := properties["product"].(map[string]any)
		values, _ := product["enum"].([]any)
		got := make([]string, 0, len(values))
		for _, value := range values {
			got = append(got, stringValue(value))
		}
		for _, want := range []string{"codex", "claude", "grok", "qwen"} {
			if !containsString(got, want) {
				t.Fatalf("%s MCP lane products = %v; missing %q", orchestrator, got, want)
			}
		}
		if orchestrator == "codex" {
			continue
		}
		for _, required := range qwenRequiredSchemaProperties(schema["required"]) {
			if required == "session_id" {
				t.Fatalf("%s MCP lane tool trusts a model-supplied session ID", orchestrator)
			}
		}
	}
}
