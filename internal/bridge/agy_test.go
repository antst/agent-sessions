package bridge

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgyHookAttachesLiveLaunchAndInjectsPeerMessage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	token := strings.Repeat("a", 64)
	t.Setenv(agyLaunchTokenEnvironment, token)
	conversationID := "90608e67-2149-420b-81ae-a0f6d284e36d"
	recordPath := agentLaunchRecordPath(paths, token)
	record := agentLaunchRecord{
		Product:  "agy",
		OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid()), Cwd: root,
		Name: "agy-test", PermissionMode: "default", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(recordPath, record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := authorizedAgentLaunch(paths, token, "grok", false); err == nil {
		t.Fatal("Antigravity launch token authorized another product adapter")
	}

	socket := filepath.Join(root, "agy.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	controls := make(chan map[string]any, 8)
	go collectAgyTestControls(listener, controls)
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(conversationID), "state.json")
	if err := writeJSONAtomic(statePath, map[string]any{
		"pid": os.Getpid(), "sessionId": conversationID, "entrypoint": "agy", "socketPath": socket,
		"name": "agy-test", "cwd": root, "permissionMode": "default",
		"ownerPid": os.Getpid(), "ownerProcStart": readProcStart(os.Getpid()),
	}); err != nil {
		t.Fatal(err)
	}

	output, err := handleAgyHook("PreInvocation", agyHookInput{ConversationID: conversationID, WorkspacePaths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(output)
	if !strings.Contains(string(encoded), "Agent Sessions peer messaging is active") || !strings.Contains(string(encoded), conversationID) {
		t.Fatalf("startup injection = %s", encoded)
	}
	assertAgyTestStatus(t, controls, "busy")
	attached := readJSONMap(recordPath)
	if stringValue(attached["conversationId"]) != conversationID || !authorizedPeerSessionNative(paths, conversationID) {
		t.Fatalf("launch was not attached: %#v", attached)
	}
	owner, ok := inferPeerParent(paths, os.Getpid())
	if !ok || owner.PID != os.Getpid() || owner.SessionID != conversationID || owner.PermissionMode != "default" {
		t.Fatalf("Antigravity launch was not accepted as a lane owner: %#v, ok=%v", owner, ok)
	}

	pending := filepath.Join(paths.dataRoot, "sessions", sessionKey(conversationID), "inbox", "pending")
	if err := writeJSONAtomic(filepath.Join(pending, "0001-message.json"), map[string]any{
		"type": "message", "id": "message-1", "from": "uds:/tmp/source.sock", "fromName": "codex-source",
		"fromProduct": "codex", "message": "AGY-PEER-HELLO",
	}); err != nil {
		t.Fatal(err)
	}
	output, err = handleAgyHook("PostInvocation", agyHookInput{ConversationID: conversationID, WorkspacePaths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(output)
	if !strings.Contains(string(encoded), "AGY-PEER-HELLO") || stringValue(output["terminationBehavior"]) != "force_continue" {
		t.Fatalf("message injection = %s", encoded)
	}
	assertAgyTestStatus(t, controls, "waiting")
	if entries, _ := os.ReadDir(pending); len(entries) != 0 {
		t.Fatalf("delivered inbox still contains %d entries", len(entries))
	}
	if _, err := handleAgyHook("Stop", agyHookInput{ConversationID: conversationID, WorkspacePaths: []string{root}}); err != nil {
		t.Fatal(err)
	}
	assertAgyTestStatus(t, controls, "idle")
}

func TestAgyHookDoesNotPublishOrdinarySessionWithoutLaunchToken(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv(agyLaunchTokenEnvironment, "")
	output, err := handleAgyHook("PreInvocation", agyHookInput{ConversationID: "90608e67-2149-420b-81ae-a0f6d284e36d", WorkspacePaths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 0 {
		t.Fatalf("ordinary session received bridge output: %#v", output)
	}
}

func TestAgyLaunchTokenCannotAttachTwoConversations(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	paths := resolveNativePaths()
	token := strings.Repeat("b", 64)
	t.Setenv(agyLaunchTokenEnvironment, token)
	first := "90608e67-2149-420b-81ae-a0f6d284e36d"
	record := agentLaunchRecord{
		Product:  "agy",
		OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid()), ConversationID: first,
		Cwd: root, PermissionMode: "default", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(agentLaunchRecordPath(paths, token), record); err != nil {
		t.Fatal(err)
	}
	if _, err := handleAgyHook("PreInvocation", agyHookInput{ConversationID: "019fe660-1c86-7700-b462-6ff16de00fc5"}); err == nil {
		t.Fatal("launch token attached to a second conversation")
	}
}

func collectAgyTestControls(listener net.Listener, controls chan<- map[string]any) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		body, _ := ioReadAllLimit(connection, 64*1024)
		var control map[string]any
		if json.Unmarshal(body, &control) == nil {
			controls <- control
		}
		_ = connection.Close()
	}
}

func assertAgyTestStatus(t *testing.T, controls <-chan map[string]any, want string) {
	t.Helper()
	select {
	case control := <-controls:
		if stringValue(control["action"]) != "update" || stringValue(control["status"]) != want {
			t.Fatalf("control = %#v, want update status %q", control, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for status %q", want)
	}
}

func ioReadAllLimit(connection net.Conn, limit int64) ([]byte, error) {
	buffer := make([]byte, limit)
	read, err := connection.Read(buffer)
	return buffer[:read], err
}
