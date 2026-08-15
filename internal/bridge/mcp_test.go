package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestNativeMCPListsAndSendsToLivePeer(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	dataRoot := filepath.Join(root, "state")
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", claudeRoot)
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataRoot)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	if err := os.MkdirAll(filepath.Join(claudeRoot, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}

	peerSocket := filepath.Join(root, "peer.sock")
	peerListener, err := net.Listen("unix", peerSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peerListener.Close() }()
	received := make(chan map[string]any, 1)
	go func() {
		for {
			conn, err := peerListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				line, _ := bufio.NewReader(conn).ReadBytes('\n')
				if len(line) == 0 {
					return
				}
				var frame map[string]any
				if json.Unmarshal(line, &frame) == nil {
					received <- frame
				}
			}()
		}
	}()

	owner := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Process.Kill()
		_, _ = owner.Process.Wait()
	}()
	peer := map[string]any{
		"pid": owner.Process.Pid, "sessionId": "claude-session", "cwd": "/tmp/project",
		"name": "peer-a", "status": "idle", "entrypoint": "claude", "procStart": readProcStart(owner.Process.Pid),
		"messagingSocketPath": peerSocket, "startedAt": time.Now().UnixMilli(),
	}
	registry := filepath.Join(claudeRoot, "sessions", fmt.Sprintf("%d.json", owner.Process.Pid))
	if err := writeJSONAtomic(registry, peer); err != nil {
		t.Fatal(err)
	}

	ownListener, err := net.Listen("unix", filepath.Join(root, "own.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ownListener.Close() }()
	go func() {
		for {
			conn, err := ownListener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	ownID := "00000000-0000-0000-0000-000000000061"
	if err := writeJSONAtomic(filepath.Join(dataRoot, "sessions", sessionKey(ownID), "state.json"), map[string]any{
		"sessionId": ownID, "name": "codex-review", "permissionMode": "bypassPermissions",
		"socketPath": ownListener.Addr().String(),
	}); err != nil {
		t.Fatal(err)
	}
	paths := resolveNativePaths()
	writeTestAttachedInteractiveOwner(t, paths, ownID)

	listed, err := callNativePeerTool("list_peers", map[string]any{}, ownID)
	if err != nil || !strings.Contains(stringValue(listed["text"]), "peer-a") {
		t.Fatalf("list result = %#v, %v", listed, err)
	}
	result, err := callNativePeerTool("send_message", map[string]any{
		"session_id": ownID, "target": "peer-a", "message": "ROUNDTRIP",
	}, ownID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringValue(result["text"]), "Message sent to peer-a") {
		t.Fatalf("send result = %#v", result)
	}
	select {
	case frame := <-received:
		message := frame["message"].(map[string]any)
		envelope := parsePeerMessage(stringValue(message["content"]))
		if envelope.Message != "ROUNDTRIP" || envelope.FromProduct != "codex" || envelope.FromMode != "bypass" {
			t.Fatalf("received envelope = %#v", envelope)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for peer frame")
	}
	if err := writeJSONAtomic(laneStatePath(paths, ownID), laneState{
		Type: "lane", ThreadID: ownID, SessionID: ownID, OwnerSessionID: "claude-session",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sendNativePeerMessage(paths, ownID, "peer-a", "OWNER_MESSAGE", "owned lane"); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		message := frame["message"].(map[string]any)
		envelope := parsePeerMessage(stringValue(message["content"]))
		if envelope.Message != "OWNER_MESSAGE" || envelope.FromMode != "prompting" {
			t.Fatalf("parent-owned lane envelope = %#v", envelope)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for parent-owned lane frame")
	}
	if _, err := sendNativePeerMessageWithTargetMode(resolveNativePaths(), ownID, "peer-a", "TERMINAL_POINTER", "terminal", true); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		message := frame["message"].(map[string]any)
		envelope := parsePeerMessage(stringValue(message["content"]))
		if envelope.Message != "TERMINAL_POINTER" || envelope.FromMode != "prompting" {
			t.Fatalf("target-matched infrastructure envelope = %#v", envelope)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for target-matched peer frame")
	}
	if _, err := sendNativePeerMessageWithTargetMode(resolveNativePaths(), ownID, encodeNativeAddress(peerSocket), "RAW_TERMINAL_POINTER", "terminal", true); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		message := frame["message"].(map[string]any)
		envelope := parsePeerMessage(stringValue(message["content"]))
		if envelope.Message != "RAW_TERMINAL_POINTER" || envelope.FromMode != "prompting" {
			t.Fatalf("raw-address target-matched envelope = %#v", envelope)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw-address target-matched frame")
	}
	if _, err := sendNativePeerMessageWithTargetMode(resolveNativePaths(), ownID, "uds:"+filepath.Join(root, "unregistered.sock"), "UNSAFE", "terminal", true); err == nil || !strings.Contains(err.Error(), "not a discoverable live peer") {
		t.Fatalf("unregistered infrastructure target error = %v", err)
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

func TestNativeSenderMatchingTargetModeRejectsUncorroboratedFederatedAttestation(t *testing.T) {
	own := map[string]any{"permissionMode": "default", "name": "lane"}
	resolved := &peerSession{
		PID: 999999, Name: "remote-owner", FederatedBy: "peer-federator",
		Version: "peer-federator/0.0.2", PermissionMode: "bypassPermissions",
		MessagingSocketPath: "/unused",
	}
	if _, err := nativeSenderMatchingTargetMode(own, "remote-owner", "/unused", resolved, nil); err == nil {
		t.Fatal("named target trusted an uncorroborated federated registry row")
	}
	if _, err := nativeSenderMatchingTargetMode(own, "uds:/unused", "/unused", nil, []peerSession{*resolved}); err == nil {
		t.Fatal("raw UDS target trusted an uncorroborated federated registry row")
	}
	if got := stringValue(own["permissionMode"]); got != "default" {
		t.Fatalf("original sender permissionMode mutated to %q", got)
	}

	resolved.PermissionMode = "invalid"
	if _, err := nativeSenderMatchingTargetMode(own, "remote-owner", "/unused", resolved, nil); err == nil {
		t.Fatal("federated target with an invalid permission assertion was accepted")
	}
}

func TestValidatePeerFederatorShadowArgsRequiresLiveControlAndOwner(t *testing.T) {
	control := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", control)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	args := []string{
		"/home/user/.local/bin/peer-federator", "shadow",
		"--listen", "/run/user/1000/peer-federator/shadows/example.sock",
		"--control", control, "--owner-pid", strconv.Itoa(os.Getpid()),
	}
	if err := validatePeerFederatorShadowArgs(args, "/run/user/1000/peer-federator/shadows/example.sock"); err != nil {
		t.Fatal(err)
	}
	args[1] = "agent"
	if err := validatePeerFederatorShadowArgs(args, "/run/user/1000/peer-federator/shadows/example.sock"); err == nil {
		t.Fatal("non-shadow peer-federator process was accepted")
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
