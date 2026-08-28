package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
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

func TestLocalMCPLaneRoutesThroughDaemonControlWithoutSubprocess(t *testing.T) {
	previous := queryMCPLaneDaemon
	t.Cleanup(func() { queryMCPLaneDaemon = previous })
	t.Setenv(daemonpkg.InternalProductEnvironment, "codex")
	t.Setenv(daemonpkg.InternalAttachmentIDEnvironment, "parent-attachment")
	t.Setenv(daemonpkg.InternalSessionIDEnvironment, "parent-session")
	t.Setenv(daemonpkg.InternalCapabilityEnvironment, "parent-capability")
	queryMCPLaneDaemon = func(_ context.Context, identity daemonpkg.LocalControlIdentity, operation string, payload any) (daemonpkg.LocalControlResult, error) {
		if identity.Role != daemonpkg.LocalControlConnector || identity.Product != "codex" ||
			identity.AttachmentID != "parent-attachment" || identity.SessionID != "parent-session" ||
			identity.Capability != "parent-capability" {
			t.Fatalf("connector identity = %#v", identity)
		}
		request, ok := payload.(daemonpkg.LaneCommandRequest)
		if !ok || operation != "lane.command" || request.Product != "claude" || request.Command != "start" ||
			request.Input != "review this" || strings.Join(request.Arguments, " ") != "--name reviewer" {
			t.Fatalf("daemon lane request = %q %#v", operation, payload)
		}
		return daemonpkg.LocalControlResult{Result: json.RawMessage(`{"type":"lane.started","lane":{"lane_id":"lane-1"}}`)}, nil
	}
	stdout, stderr := &mcpCappedBuffer{limit: 4096}, &mcpCappedBuffer{limit: 4096}
	exit, err := runLocalMCPParentLane(context.Background(), nativePaths{}, "parent-session",
		"claude-lane", []string{"start", "--name", "reviewer"}, "review this", "", stdout, stderr)
	if err != nil || exit != 0 {
		t.Fatalf("local MCP lane proxy = %d, %v: %s", exit, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"lane_id":"lane-1"`) || stderr.Len() != 0 {
		t.Fatalf("daemon lane output = stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestRemoteMCPLaneRoutesThroughDaemonWithoutLegacyFederatorFallback(t *testing.T) {
	previous := queryMCPLaneDaemon
	t.Cleanup(func() { queryMCPLaneDaemon = previous })
	t.Setenv(daemonpkg.InternalProductEnvironment, "codex")
	t.Setenv(daemonpkg.InternalAttachmentIDEnvironment, "parent-attachment")
	t.Setenv(daemonpkg.InternalSessionIDEnvironment, "parent-session")
	t.Setenv(daemonpkg.InternalCapabilityEnvironment, "parent-capability")
	queryMCPLaneDaemon = func(_ context.Context, _ daemonpkg.LocalControlIdentity, operation string, payload any) (daemonpkg.LocalControlResult, error) {
		request, ok := payload.(daemonpkg.LaneCommandRequest)
		if !ok || operation != "lane.command" || request.Host != "macbook" || request.Product != "qwen" ||
			request.Command != "start" || request.Input != "remote work" {
			t.Fatalf("remote daemon lane request = %q %#v", operation, payload)
		}
		return daemonpkg.LocalControlResult{Result: json.RawMessage(`{"type":"lane.start","host":"macbook"}`)}, nil
	}
	stdout, stderr := &mcpCappedBuffer{limit: 4096}, &mcpCappedBuffer{limit: 4096}
	exit, err := runMCPParentLaneRequest(context.Background(), nativePaths{}, "parent-session", mcpLaneRequest{
		product: "qwen", role: "qwen-lane", command: "start", host: "macbook",
		args: []string{"start", "--name", "remote"}, input: "remote work",
	}, stdout, stderr)
	if err != nil || exit != 0 || !strings.Contains(stdout.String(), `"host":"macbook"`) || stderr.Len() != 0 {
		t.Fatalf("remote MCP daemon proxy = %d, %v stdout=%q stderr=%q", exit, err, stdout.String(), stderr.String())
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
	if properties["product"] == nil || properties["command"] == nil || properties["input"] == nil || properties["session_id"] == nil {
		t.Fatalf("lane tool schema = %#v", schema)
	}
}

func TestQwenParentMCPPublishesAllFourLaneTargets(t *testing.T) {
	var lane map[string]any
	for _, definition := range qwenToolDefinitions {
		if stringValue(definition["name"]) == "lane" {
			lane = definition
			break
		}
	}
	if lane == nil {
		t.Fatal("Qwen MCP omitted the lane tool")
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
			t.Fatalf("Qwen MCP lane products = %v; missing %q", got, want)
		}
	}
	for _, required := range qwenRequiredSchemaProperties(schema["required"]) {
		if required == "session_id" {
			t.Fatal("Qwen MCP lane tool trusts a model-supplied session ID")
		}
	}
}
