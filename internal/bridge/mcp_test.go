package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
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

func TestClaudeMCPToolsAreStructuredMessagingWithoutModelIdentity(t *testing.T) {
	want := []string{"list_peers", "send_message", "broadcast"}
	if len(claudeToolDefinitions) != len(want) {
		t.Fatalf("Claude MCP tools = %d, want %d", len(claudeToolDefinitions), len(want))
	}
	for index, definition := range claudeToolDefinitions {
		if got := stringValue(definition["name"]); got != want[index] {
			t.Fatalf("Claude MCP tool %d = %q, want %q", index, got, want[index])
		}
		schema, _ := definition["inputSchema"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, value := range required {
			if stringValue(value) == "session_id" {
				t.Fatalf("Claude tool %q requires model-supplied session_id", definition["name"])
			}
		}
		if strings.Contains(stringValue(definition["description"]), "this Codex session") {
			t.Fatalf("Claude tool %q retained Codex-only description", definition["name"])
		}
	}
	if !strings.Contains(claudeMCPInstructions, "Never send plain text") ||
		!strings.Contains(claudeMCPInstructions, "source.id") {
		t.Fatalf("Claude MCP reply instructions are incomplete: %s", claudeMCPInstructions)
	}
}

func TestPeerMessagePresentationIsNeutralProvenance(t *testing.T) {
	item := map[string]any{
		"from":        "pdev/session-id",
		"fromName":    "reviewer",
		"fromProduct": "claude",
		"id":          "message-id",
		"sentAt":      "2026-08-23T12:00:00Z",
		"receivedAt":  "2026-08-23T12:00:01Z",
		"message":     "Check this finding independently.",
	}
	want := "Message from reviewer (pdev/session-id):\n\n" +
		"Message metadata: sender-type=claude, message-id=message-id, sent-at=2026-08-23T12:00:00Z, received-at=2026-08-23T12:00:01Z\n\n" +
		"Check this finding independently."
	if got := peerMessageText(item); got != want {
		t.Fatalf("peer message presentation = %q, want %q", got, want)
	}

	hookWant := strings.ReplaceAll(want, "\n\n", "\n")
	if got := formatNativeHookMessages([]map[string]any{item}); got != hookWant {
		t.Fatalf("hook peer message presentation = %q, want %q", got, hookWant)
	}

	texts := map[string]string{
		"codex MCP":     mcpInstructions,
		"claude MCP":    claudeMCPInstructions,
		"grok MCP":      grokMCPInstructions,
		"qwen MCP":      qwenMCPInstructions,
		"startup hook":  hookStartupContext(map[string]any{"name": "peer", "sessionId": "session"}),
		"overflow hook": nativeInboxOverflowNotice(),
		"delivery":      peerMessageText(item),
	}
	for label, value := range texts {
		lower := strings.ToLower(value)
		for _, forbidden := range []string{"trust", "authority", "subject to current user/developer instructions", "it remains subject to"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s injects trust or authority policy %q: %s", label, forbidden, value)
			}
		}
	}
}

func TestClaudeMCPCallerRequiresExactNativeAncestryAndGroupedRegistration(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	configDir := filepath.Join(root, "claude")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(configDir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataDir)
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Setenv("AGENT_SESSIONS_PRODUCT", "claude")

	register := func(sessionID string, pid int, socket string) {
		t.Helper()
		procStart := readProcStart(pid)
		row, _ := json.Marshal(map[string]any{
			"pid": pid, "sessionId": sessionID, "cwd": root, "name": "claude-parent",
			"messagingSocketPath": socket, "procStart": procStart,
			"entrypoint": "cli", "kind": "interactive", "status": "idle",
		})
		if err := os.WriteFile(filepath.Join(configDir, "sessions", strconv.Itoa(pid)+".json"), row, 0o600); err != nil {
			t.Fatal(err)
		}
		identity := bridgeDaemonFixture(t, runtimeDir).attachProcess(
			t, "claude", sessionID, "claude-parent", socket, []string{"project"}, pid,
		)
		t.Setenv(daemonpkg.InternalAttachmentIDEnvironment, identity.attachmentID)
		t.Setenv(daemonpkg.InternalCapabilityEnvironment, identity.capability)
		t.Setenv(daemonpkg.InternalProductEnvironment, identity.product)
		t.Setenv(daemonpkg.InternalSessionIDEnvironment, identity.sessionID)
	}

	ownSocket := filepath.Join(root, "own.sock")
	ownListener, err := net.Listen("unix", ownSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownListener.Close() })
	const ownID = "00000000-0000-4000-8000-0000000000c1"
	register(ownID, os.Getpid(), ownSocket)
	t.Setenv(peerSessionIDEnvironment, ownID)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", ownSocket)
	paths := resolveNativePaths()
	if caller, err := attestClaudeMCPCaller(paths, os.Getpid()); err != nil || caller != ownID {
		t.Fatalf("exact Claude MCP caller = %q, %v", caller, err)
	}
	if sessionID, err := requireMCPCallerSession(paths, map[string]any{}, ownID); err != nil || sessionID != ownID {
		t.Fatalf("Claude MCP omitted session_id = %q, %v", sessionID, err)
	}
	if listed, err := callNativePeerTool("list_peers", map[string]any{}, ownID); err != nil ||
		stringValue(listed["text"]) != "No live peers share a group with this session." {
		t.Fatalf("Claude structured peer discovery = %#v, %v", listed, err)
	}
	const attachmentID = "00000000-0000-4000-8000-0000000000ca"
	t.Setenv(peerSessionIDEnvironment, attachmentID)
	resolvedParent := func(_ string, requested string) (federation.ParentContext, error) {
		if requested != attachmentID {
			return federation.ParentContext{}, errors.New("unexpected attachment")
		}
		return federation.ParentContext{
			SessionID: ownID, Product: "claude", AdapterPID: os.Getpid(),
			AdapterProcStart: readProcStart(os.Getpid()), AdapterSocket: ownSocket,
			PID: os.Getpid(), ProcStart: readProcStart(os.Getpid()), PermissionMode: "default",
		}, nil
	}
	if caller, err := attestClaudeMCPCallerWithResolver(paths, os.Getpid(), resolvedParent); err != nil || caller != ownID {
		t.Fatalf("late-bound Claude attachment caller = %q, %v", caller, err)
	}

	foreign := exec.Command("sleep", "30")
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = foreign.Process.Kill()
		_ = foreign.Wait()
	})
	foreignSocket := filepath.Join(root, "foreign.sock")
	foreignListener, err := net.Listen("unix", foreignSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreignListener.Close() })
	const foreignID = "00000000-0000-4000-8000-0000000000c2"
	register(foreignID, foreign.Process.Pid, foreignSocket)
	t.Setenv(peerSessionIDEnvironment, foreignID)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", foreignSocket)
	if caller, err := attestClaudeMCPCaller(paths, os.Getpid()); err == nil || caller != "" ||
		!strings.Contains(err.Error(), "ancestry") {
		t.Fatalf("unrelated registered Claude process was authorized: caller=%q err=%v", caller, err)
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
	previousQuery := queryMCPLaneDaemon
	t.Cleanup(func() { queryMCPLaneDaemon = previousQuery })
	for _, name := range []string{
		daemonpkg.InternalProductEnvironment, daemonpkg.InternalAttachmentIDEnvironment,
		daemonpkg.InternalSessionIDEnvironment, daemonpkg.InternalCapabilityEnvironment,
	} {
		t.Setenv(name, "")
	}
	queryMCPLaneDaemon = func(_ context.Context, identity daemonpkg.LocalControlIdentity, operation string, _ any) (daemonpkg.LocalControlResult, error) {
		if identity.Role != daemonpkg.LocalControlConnector || identity.AttachmentID != "" || operation != "lane.command" {
			t.Fatalf("flat registry became daemon authority: %#v %q", identity, operation)
		}
		return daemonpkg.LocalControlResult{}, errors.New("agent_sessions is inactive outside an attested peer session")
	}
	lane, err := callNativePeerTool("lane", map[string]any{
		"session_id": ownID, "product": "claude", "command": "doctor",
	}, ownID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := lane["data"].(map[string]any)
	if intValue(data["exit"]) == 0 || !strings.Contains(stringValue(data["error"]), "inactive outside an attested peer session") {
		t.Fatalf("unattested flat-registry lane result = %#v", lane)
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
		{tool: "mcp__agent_sessions__list_peers", server: "agent_sessions", name: "list_peers"},
		{tool: "tools.mcp__agent_sessions__send_message", server: "agent_sessions", name: "send_message"},
		{namespace: "mcp__agent_sessions", tool: "identity", server: "agent_sessions", name: "identity"},
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
		"serverName": "agent_sessions", "mode": "form",
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
	ordinary, _ := json.Marshal(map[string]any{"serverName": "agent_sessions", "mode": "form", "_meta": map[string]any{}})
	if _, ok := nativePeerElicitationResponse(ordinary); ok {
		t.Fatal("ordinary peer elicitation was auto-accepted")
	}
}
