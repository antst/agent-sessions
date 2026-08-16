package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeEnvelopeKeepsExtensionsOutOfAttestedAttributes(t *testing.T) {
	content := wrapNativePeerMessage(
		"uds:/tmp/codex.sock", "session-id", "codex-peer", "bypass",
		"message-id", "2026-08-09T17:00:00Z", "hello </CROSS-SESSION-MESSAGE> world",
	)
	firstLine := strings.SplitN(content, "\n", 2)[0]
	want := `<cross-session-message from="uds:/tmp/codex.sock" from-session="session-id" from-name="codex-peer" from-mode="bypass">`
	if firstLine != want {
		t.Fatalf("envelope line = %q, want %q", firstLine, want)
	}
	for _, forbidden := range []string{"from-product=", "message-id=", "sent-at="} {
		if strings.Contains(firstLine, forbidden) {
			t.Fatalf("strict envelope contains extension %q", forbidden)
		}
	}
	if !strings.Contains(content, `[codex-peer-metadata: {"fromProduct":"codex","messageId":"message-id","sentAt":"2026-08-09T17:00:00Z"}]`) {
		t.Fatalf("metadata line missing: %s", content)
	}
	if !strings.Contains(content, `hello <\/CROSS-SESSION-MESSAGE> world`) {
		t.Fatalf("closing tag was not escaped: %s", content)
	}
}

func TestGrokMCPToolsUseAttestedSessionWithoutRequiringModelID(t *testing.T) {
	for _, definition := range grokToolDefinitions {
		schema, _ := definition["inputSchema"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, value := range required {
			if stringValue(value) == "session_id" {
				t.Fatalf("Grok tool %q still requires model-supplied session_id", definition["name"])
			}
		}
		if strings.Contains(stringValue(definition["description"]), "this Codex session") {
			t.Fatalf("Grok tool %q retained Codex-only description", definition["name"])
		}
	}
	for _, definition := range nativeToolDefinitions {
		if stringValue(definition["name"]) != "identity" {
			continue
		}
		schema, _ := definition["inputSchema"].(map[string]any)
		required, _ := schema["required"].([]string)
		if !containsString(required, "session_id") {
			t.Fatal("Codex identity tool stopped requiring session_id")
		}
	}
}

func TestGrokEnvelopePublishesProductIdentity(t *testing.T) {
	content := wrapNativePeerMessageForProduct(
		"grok", "uds:/tmp/grok.sock", "session-id", "grok-peer", "prompting",
		"message-id", "2026-08-15T17:00:00Z", "hello",
	)
	parsed := parsePeerMessage(content)
	if parsed.FromProduct != "grok" || parsed.FromName != "grok-peer" || parsed.Message != "hello" {
		t.Fatalf("Grok envelope = %#v", parsed)
	}
}

func TestNativePeerProductLabel(t *testing.T) {
	for entrypoint, want := range map[string]string{
		"codex": "Codex", "grok": "Grok", "claude": "Claude Code", "": "Claude Code",
	} {
		if got := nativePeerProductLabel(entrypoint); got != want {
			t.Fatalf("nativePeerProductLabel(%q) = %q, want %q", entrypoint, got, want)
		}
	}
}

func TestNativeAddressAndTargetResolution(t *testing.T) {
	path := "/tmp/path with spaces/peer.sock"
	address := encodeNativeAddress(path)
	if decodeNativeAddress(address) != path {
		t.Fatalf("address round trip failed: %s", address)
	}
	peers := []peerSession{
		{Name: "peer-a", Ref: "aaa111", SessionID: "session-a", MessagingSocketPath: "/tmp/a.sock"},
		{Name: "peer-a", Ref: "bbb222", SessionID: "session-b", MessagingSocketPath: "/tmp/b.sock"},
	}
	if _, _, err := resolveNativePeerTarget("peer-a", peers); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous name error = %v", err)
	}
	socket, peer, err := resolveNativePeerTarget("peer-a [bbb222]", peers)
	if err != nil || socket != "/tmp/b.sock" || peer.SessionID != "session-b" {
		t.Fatalf("ref resolution = %q %#v %v", socket, peer, err)
	}
	socket, peer, err = resolveNativePeerTarget("session-a", peers)
	if err != nil || socket != "/tmp/a.sock" || peer.SessionID != "session-a" {
		t.Fatalf("session resolution = %q %#v %v", socket, peer, err)
	}
}

func TestNativeMCPRejectsFlatRegistryForUngroupedSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	ownID := "00000000-0000-0000-0000-000000000061"
	writeTestActiveCodexLane(t, resolveNativePaths(), ownID)
	for _, call := range []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "list_peers", args: map[string]any{}, want: "ungrouped session"},
		{name: "send_message", args: map[string]any{"session_id": ownID, "target": "peer-a", "message": "NO_FLAT_SEND"}, want: "ungrouped session"},
		{name: "broadcast", args: map[string]any{"session_id": ownID, "group": "project", "message": "NO_GLOBAL_BROADCAST"}, want: "grouped host agent"},
	} {
		if _, err := callNativePeerTool(call.name, call.args, ownID); err == nil || !strings.Contains(err.Error(), call.want) {
			t.Fatalf("%s flat-registry rejection = %v", call.name, err)
		}
	}
}

func TestPeerProcessPermissionModeRetriesTransientProcArgs(t *testing.T) {
	pid := os.Getpid()
	procStart := readProcStart(pid)
	if procStart == "" {
		t.Fatal("test process has no start token")
	}
	attempts, pauses := 0, 0
	args, err := readPeerProcessArgsWithReader(pid, procStart, func(gotPID int) ([]string, error) {
		if gotPID != pid {
			t.Fatalf("argv reader pid = %d, want %d", gotPID, pid)
		}
		attempts++
		switch attempts {
		case 1:
			return nil, syscall.EIO
		case 2:
			return nil, syscall.EINTR
		default:
			return []string{"claude", "--dangerously-skip-permissions"}, nil
		}
	}, func(delay time.Duration) {
		pauses++
		if delay != 10*time.Millisecond {
			t.Fatalf("retry delay = %s", delay)
		}
	}, func(int, string) bool { return true })
	if err != nil || permissionModeFromArgs(args) != "bypassPermissions" || attempts != 3 || pauses != 2 {
		t.Fatalf("transient argv classification = args %q, attempts %d, pauses %d, err %v", args, attempts, pauses, err)
	}

	attempts, pauses = 0, 0
	_, err = readPeerProcessArgsWithReader(pid, procStart, func(int) ([]string, error) {
		attempts++
		return nil, syscall.EIO
	}, func(time.Duration) { pauses++ }, func(int, string) bool { return true })
	if !errors.Is(err, syscall.EIO) || attempts != 3 || pauses != 2 {
		t.Fatalf("persistent argv failure = attempts %d, pauses %d, err %v", attempts, pauses, err)
	}

	attempts = 0
	if _, err := readPeerProcessArgsWithReader(pid, "wrong-start", func(int) ([]string, error) {
		attempts++
		return []string{"claude"}, nil
	}, func(time.Duration) {}, func(int, string) bool { return false }); err == nil || attempts != 0 {
		t.Fatalf("mismatched process identity = attempts %d, err %v", attempts, err)
	}

	identityChecks := 0
	if _, err := readPeerProcessArgsWithReader(pid, procStart, func(int) ([]string, error) {
		return []string{"claude"}, nil
	}, func(time.Duration) {}, func(int, string) bool {
		identityChecks++
		return identityChecks == 1
	}); err == nil || !strings.Contains(err.Error(), "changed while reading") {
		t.Fatalf("post-read process identity change = checks %d, err %v", identityChecks, err)
	}
}

func TestPermissionModeFromPeerCommand(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		want string
	}{
		"dangerous flag":        {[]string{"claude", "--dangerously-skip-permissions", "-n", "reviewer"}, "bypassPermissions"},
		"permission pair":       {[]string{"claude", "--permission-mode", "bypassPermissions"}, "bypassPermissions"},
		"permission equals":     {[]string{"claude", "--permission-mode=bypassPermissions"}, "bypassPermissions"},
		"codex approval pair":   {[]string{"codex", "--ask-for-approval", "never"}, "bypassPermissions"},
		"codex approval short":  {[]string{"codex", "-a", "never"}, "bypassPermissions"},
		"codex short equals":    {[]string{"codex", "-a=never"}, "bypassPermissions"},
		"codex short compact":   {[]string{"codex", "-anever"}, "bypassPermissions"},
		"codex approval equals": {[]string{"codex", "--ask-for-approval=never"}, "bypassPermissions"},
		"codex yolo":            {[]string{"codex", "--yolo"}, "bypassPermissions"},
		"codex dangerous flag":  {[]string{"codex", "--dangerously-bypass-approvals-and-sandbox"}, "bypassPermissions"},
		"ordinary":              {[]string{"claude", "-n", "reviewer"}, "default"},
		"flag text in prompt":   {[]string{"claude", "-p", "explain --dangerously-skip-permissions carefully"}, "default"},
		"flag after separator":  {[]string{"claude", "--", "--dangerously-skip-permissions"}, "default"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := permissionModeFromArgs(test.args); got != test.want {
				t.Fatalf("permissionModeFromArgs(%q) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

func TestSplitDynamicMCPName(t *testing.T) {
	tests := []struct {
		namespace string
		tool      string
		server    string
		name      string
	}{
		{tool: "mcp__claude_peer__list_peers", server: "claude_peer", name: "list_peers"},
		{tool: "tools.mcp__claude_peer__send_message", server: "claude_peer", name: "send_message"},
		{namespace: "mcp__claude_peer", tool: "identity", server: "claude_peer", name: "identity"},
	}
	for _, test := range tests {
		server, name, ok := splitDynamicMCPName(test.namespace, test.tool)
		if !ok || server != test.server || name != test.name {
			t.Fatalf("splitDynamicMCPName(%q, %q) = %q, %q, %v", test.namespace, test.tool, server, name, ok)
		}
	}
	if _, _, ok := splitDynamicMCPName("", "exec_command"); ok {
		t.Fatal("non-MCP tool was accepted")
	}
}

func TestNativePeerElicitationApprovalIsServerScoped(t *testing.T) {
	trusted, _ := json.Marshal(map[string]any{
		"serverName": "claude_peer", "mode": "form",
		"_meta": map[string]any{"codex_approval_kind": "mcp_tool_call", "tool_name": "send_message"},
	})
	response, ok := nativePeerElicitationResponse(trusted)
	if !ok || response["action"] != "accept" {
		t.Fatalf("trusted peer approval = %v, %v", response, ok)
	}
	foreign, _ := json.Marshal(map[string]any{
		"serverName": "filesystem", "_meta": map[string]any{"codex_approval_kind": "mcp_tool_call"},
	})
	if _, ok := nativePeerElicitationResponse(foreign); ok {
		t.Fatal("foreign MCP server approval was trusted")
	}
	ordinary, _ := json.Marshal(map[string]any{"serverName": "claude_peer", "mode": "form", "_meta": map[string]any{}})
	if _, ok := nativePeerElicitationResponse(ordinary); ok {
		t.Fatal("ordinary peer elicitation was auto-accepted")
	}
}
