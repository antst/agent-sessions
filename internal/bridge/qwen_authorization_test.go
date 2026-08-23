package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestQwenMCPToolsUseExactProcessAttestationWithoutModelIdentity(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	configDir := filepath.Join(root, "claude")
	dataDir := filepath.Join(root, "data")
	peerRuntimeDir := filepath.Join(root, "peer-runtime")
	if err := os.MkdirAll(peerRuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataDir)
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Setenv("AGENT_SESSIONS_PRODUCT", "qwen")
	const sessionID = "00000000-0000-4000-8000-0000000000d1"
	if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: sessionID, Product: "qwen", Groups: []string{"project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	peer := newDaemon(map[string]string{
		"session-id": sessionID, "cwd": root, "name": "qwen-parent", "entrypoint": "qwen",
		"data-dir": dataDir, "claude-config-dir": configDir,
		"codex-home": filepath.Join(root, "codex"), "runtime-dir": peerRuntimeDir,
		"agent-runtime-dir": runtimeDir,
	})
	qwenTestSetCapability(t, peer, "qwen-test-capability")
	if err := peer.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.shutdown)
	t.Setenv(peerSessionIDEnvironment, sessionID)
	paths := resolveNativePaths()
	caller, err := attestQwenMCPCaller(paths, os.Getpid())
	if err != nil || caller != sessionID {
		t.Fatalf("attested Qwen caller = %q, %v", caller, err)
	}
	owner, ok := inferQwenParent(paths, os.Getpid())
	if !ok || owner.SessionID != sessionID || owner.PID != os.Getpid() || owner.ProcStart != readProcStart(os.Getpid()) {
		t.Fatalf("attested Qwen lane owner = %+v, %v", owner, ok)
	}
	if got, err := requireMCPCallerSession(paths, map[string]any{}, sessionID); err != nil || got != sessionID {
		t.Fatalf("Qwen omitted model identity = %q, %v", got, err)
	}

	for _, definition := range qwenToolDefinitions {
		schema, _ := definition["inputSchema"].(map[string]any)
		for _, required := range qwenRequiredSchemaProperties(schema["required"]) {
			if required == "session_id" {
				t.Fatalf("Qwen tool %q requires model identity", definition["name"])
			}
		}
		if strings.Contains(stringValue(definition["description"]), "this Codex session") {
			t.Fatalf("Qwen tool %q retained Codex-only description", definition["name"])
		}
	}
	if !strings.Contains(qwenMCPInstructions, "Qwen") || !strings.Contains(qwenMCPInstructions, "process") {
		t.Fatalf("Qwen MCP instructions do not explain attestation: %s", qwenMCPInstructions)
	}

	t.Setenv("AGENT_SESSIONS_PRODUCT", "")
	if caller, err := attestQwenMCPCaller(paths, os.Getpid()); err == nil || caller != "" {
		t.Fatalf("bare Qwen was authorized: caller=%q err=%v", caller, err)
	}
}

func qwenRequiredSchemaProperties(value any) []string {
	switch required := value.(type) {
	case []string:
		return required
	case []any:
		result := make([]string, 0, len(required))
		for _, item := range required {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func TestQwenMCPGroupedMessagingUsesOneAgentRouteAndExactSockets(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	configDir := filepath.Join(root, "claude")
	dataDir := filepath.Join(root, "data")
	peerRuntimeDir := filepath.Join(root, "peer-runtime")
	if err := os.MkdirAll(peerRuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataDir)
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)

	const sourceID = "00000000-0000-4000-8000-0000000000d2"
	const targetID = "00000000-0000-4000-8000-0000000000d3"
	for _, item := range []struct{ id, product string }{{sourceID, "qwen"}, {targetID, "codex"}} {
		if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
			SessionID: item.id, Product: item.product, Groups: []string{"project"}, GroupsSpecified: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	source := newDaemon(map[string]string{
		"session-id": sourceID, "cwd": root, "name": "qwen-source", "entrypoint": "qwen", "status": "idle",
		"data-dir": dataDir, "claude-config-dir": configDir, "codex-home": filepath.Join(root, "codex"),
		"runtime-dir": peerRuntimeDir, "agent-runtime-dir": runtimeDir,
	})
	qwenTestSetCapability(t, source, "qwen-grouped-capability")
	target := newDaemon(map[string]string{
		"session-id": targetID, "cwd": root, "name": "codex-target", "entrypoint": "codex", "status": "busy",
		"data-dir": dataDir, "claude-config-dir": configDir, "codex-home": filepath.Join(root, "codex"),
		"runtime-dir": peerRuntimeDir, "agent-runtime-dir": runtimeDir,
	})
	for _, peer := range []*daemon{source, target} {
		if err := peer.start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(peer.shutdown)
		info, err := os.Lstat(peer.stableSocket)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("published Qwen test endpoint %q = %v, %v", peer.stableSocket, info, err)
		}
	}

	previousRoute := routeMCPAgentFrame
	routes := 0
	routeMCPAgentFrame = func(runtime, caller string, frame federator.AgentFrame) (federator.AgentFrameResult, error) {
		routes++
		return federator.RouteAgentFrame(runtime, caller, frame)
	}
	t.Cleanup(func() { routeMCPAgentFrame = previousRoute })

	listed, err := callNativePeerTool("list_peers", map[string]any{}, sourceID)
	if err != nil || !strings.Contains(stringValue(listed["text"]), "codex-target") {
		t.Fatalf("Qwen discovery = %#v, %v", listed, err)
	}
	if routes != 1 {
		t.Fatalf("discovery agent routes = %d, want 1", routes)
	}
	if _, err := callNativePeerTool("send_message", map[string]any{
		"target": "codex-target", "message": "QWEN_DIRECT",
	}, sourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := callNativePeerTool("send_message", map[string]any{
		"targets": []string{"codex-target"}, "message": "QWEN_MULTICAST",
	}, sourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := callNativePeerTool("broadcast", map[string]any{
		"group": "project", "message": "QWEN_BROADCAST",
	}, sourceID); err != nil {
		t.Fatal(err)
	}
	if routes != 4 {
		t.Fatalf("one-route messaging count = %d, want discover+3 operations", routes)
	}

	qwenTestWaitForPendingMessages(t, target.pendingDir, 3)
	seen := qwenPendingMessages(t, target.pendingDir)
	for _, message := range []string{"QWEN_DIRECT", "QWEN_MULTICAST", "QWEN_BROADCAST"} {
		if !seen[message] {
			t.Fatalf("target messages = %v; missing %s", seen, message)
		}
	}

	frame := federator.AgentFrame{
		Version: federator.AgentFrameVersion, Type: "send", MessageID: "qwen-duplicate",
		Targets: []string{"codex-target"}, Content: "QWEN_DUPLICATE",
	}
	if _, err := federator.RouteAgentFrame(runtimeDir, sourceID, frame); err != nil {
		t.Fatal(err)
	}
	if _, err := federator.RouteAgentFrame(runtimeDir, sourceID, frame); err != nil {
		t.Fatal(err)
	}
	qwenTestWaitForPendingMessages(t, target.pendingDir, 4)
	if got := qwenPendingMessageCount(t, target.pendingDir, "QWEN_DUPLICATE"); got != 1 {
		t.Fatalf("duplicate delivery count = %d", got)
	}

	if _, err := routeMCPAgentFrame(runtimeDir, targetID, federator.AgentFrame{
		Version: federator.AgentFrameVersion, Type: "send", MessageID: "qwen-reply",
		Targets: []string{sourceID}, Content: "EXACT_SENDER_REPLY",
	}); err != nil {
		t.Fatal(err)
	}
	qwenTestWaitForPendingMessages(t, source.pendingDir, 1)
	entries, err := os.ReadDir(source.pendingDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("source reply queue = %v, %v", entries, err)
	}
	body, err := os.ReadFile(filepath.Join(source.pendingDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if json.Unmarshal(body, &reply) != nil || stringValue(reply["fromSession"]) != targetID || stringValue(reply["message"]) != "EXACT_SENDER_REPLY" {
		t.Fatalf("exact-sender reply = %s", body)
	}
}

func qwenTestWaitForPendingMessages(t *testing.T, directory string, count int) {
	t.Helper()
	if !waitForCondition(2*time.Second, func() bool {
		entries, _ := os.ReadDir(directory)
		return len(entries) == count
	}) {
		t.Fatalf("pending queue %s did not reach %d entries", directory, count)
	}
}

func qwenPendingMessages(t *testing.T, directory string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var item map[string]any
		if json.Unmarshal(body, &item) != nil {
			t.Fatalf("pending item = %s", body)
		}
		seen[stringValue(item["message"])] = true
	}
	return seen
}

func qwenPendingMessageCount(t *testing.T, directory, message string) int {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var item map[string]any
		if json.Unmarshal(body, &item) == nil && stringValue(item["message"]) == message {
			count++
		}
	}
	return count
}

func TestQwenMCPAttestationRejectsMismatchedLifecycleAndSocket(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "qwen.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	const sessionID = "00000000-0000-4000-8000-0000000000d4"
	t.Setenv("AGENT_SESSIONS_PRODUCT", "qwen")
	t.Setenv(peerSessionIDEnvironment, sessionID)
	capability := "qwen-mismatch-capability"
	digest := sha256.Sum256([]byte(capability))
	t.Setenv("AGENT_SESSIONS_QWEN_CAPABILITY", capability)
	resolver := func(_ string, _ string) (federator.ParentContext, error) {
		return federator.ParentContext{
			SessionID: sessionID, Product: "qwen", AdapterPID: os.Getpid(), AdapterProcStart: readProcStart(os.Getpid()),
			AdapterSocket: socket, PID: os.Getpid(), ProcStart: "wrong-start",
			QwenCapabilityDigest: "sha256:" + hex.EncodeToString(digest[:]),
		}, nil
	}
	if caller, err := attestQwenMCPCallerWithResolver(resolveNativePaths(), os.Getpid(), resolver); err == nil || caller != "" {
		t.Fatalf("mismatched Qwen lifecycle was authorized: caller=%q err=%v", caller, err)
	}
}

func qwenTestSetCapability(t *testing.T, daemon *daemon, capability string) {
	t.Helper()
	digest := sha256.Sum256([]byte(capability))
	value := "sha256:" + hex.EncodeToString(digest[:])
	t.Setenv("AGENT_SESSIONS_QWEN_CAPABILITY", capability)
	daemon.registrationOverride = func(registration federator.PeerRegistration) federator.PeerRegistration {
		registration.QwenCapabilityDigest = value
		return registration
	}
}
