package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const grokFakeProcessEnv = "AGENT_SESSIONS_GROK_FAKE_PROCESS"

func TestGrokFakeProcess(_ *testing.T) {
	if os.Getenv(grokFakeProcessEnv) != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}
	recordGrokFake(map[string]any{"kind": "argv", "args": args, "pid": os.Getpid()})
	if containsAdjacent(args, "agent", "leader") {
		runGrokFakeLeader(args)
		return
	}
	runGrokFakeACP()
}

func containsAdjacent(args []string, left, right string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == left && args[i+1] == right {
			return true
		}
	}
	return false
}

func grokFakeArgument(args []string, wanted string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == wanted {
			return args[i+1]
		}
	}
	return ""
}

func runGrokFakeLeader(args []string) {
	socket := grokFakeArgument(args, "--leader-socket")
	_ = os.Remove(socket)
	_ = os.WriteFile(filepath.Join(filepath.Dir(socket), "leader.lock"), []byte("fake lock\n"), 0o600)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(31)
	}
	defer func() { _ = listener.Close() }()
	if childFile := os.Getenv("GROK_FAKE_CHILD_FILE"); childFile != "" {
		child := exec.Command("sleep", "30")
		if child.Start() == nil {
			_ = os.WriteFile(childFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
	}
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		_ = conn.Close()
	}
}

func runGrokFakeACP() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		recordGrokFake(map[string]any{"kind": "request", "request": request, "pid": os.Getpid()})
		method := stringValue(request["method"])
		result := map[string]any{}
		switch method {
		case "initialize":
			result["protocolVersion"] = "1"
			if os.Getenv("GROK_FAKE_NO_CACHED_TOKEN") != "1" {
				result["authMethods"] = []map[string]any{{"id": "cached_token", "name": "Cached token"}}
			}
		case "authenticate":
			if os.Getenv("GROK_FAKE_AUTH_REJECT") == "1" {
				writeGrokFakeResponse(request["id"], nil, map[string]any{"code": -32001, "message": "bad cached token"})
				continue
			}
		case "session/load":
			if os.Getenv("GROK_FAKE_LOAD_REJECT") == "1" {
				writeGrokFakeResponse(request["id"], nil, map[string]any{"code": -32000, "message": "session not ready"})
				continue
			}
			if marker := os.Getenv("GROK_FAKE_EXIT_AFTER_LOAD_ONCE"); marker != "" {
				file, createErr := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if createErr == nil {
					_ = file.Close()
					writeGrokFakeResponse(request["id"], result, nil)
					return
				}
			}
			writeGrokFakeNotification("session/update", map[string]any{"sessionId": "unrelated"})
		case "_x.ai/sessions/list":
			if os.Getenv("GROK_FAKE_BAD_ROSTER") == "1" {
				result["result"] = map[string]any{"sessions": []any{}}
				break
			}
			yolo := os.Getenv("GROK_FAKE_YOLO") == "1"
			if path := os.Getenv("GROK_FAKE_YOLO_FILE"); path != "" {
				body, _ := os.ReadFile(path)
				yolo = strings.TrimSpace(string(body)) == "1"
			}
			result["result"] = map[string]any{"sessions": []any{map[string]any{
				"sessionId": os.Getenv(grokSessionIDEnv), "resident": true, "yolo": yolo,
			}}}
		case "session/prompt":
			if delay, _ := strconv.Atoi(os.Getenv("GROK_FAKE_PROMPT_DELAY_MS")); delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			result["stopReason"] = "end_turn"
		}
		writeGrokFakeResponse(request["id"], result, nil)
	}
}

func writeGrokFakeNotification(method string, params map[string]any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	_, _ = fmt.Fprintln(os.Stdout, string(body))
}

func writeGrokFakeResponse(id any, result, rpcErr map[string]any) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	body, _ := json.Marshal(response)
	_, _ = fmt.Fprintln(os.Stdout, string(body))
}

func recordGrokFake(value map[string]any) {
	path := os.Getenv("GROK_FAKE_RECORD")
	if path == "" {
		return
	}
	body, _ := json.Marshal(value)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(body, '\n'))
	_ = file.Close()
}

func TestGrokHostACPWakeIsSerializedAndIdempotent(t *testing.T) {
	host, cancel, result, record := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-idempotent")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	item := map[string]any{"id": "message-1", "message": "do exactly one turn", "from": "peer"}
	request := map[string]any{
		"action": "wake", "sessionId": host.config.SessionID,
		"launchToken": host.config.LaunchToken, "item": item,
	}
	response, err := requestControl(host.paths.ControlSocket, request, 2*time.Second)
	if err != nil || stringValue(response["delivery"]) != "accepted" {
		t.Fatalf("first wake = %#v, %v", response, err)
	}
	duplicate, err := requestControl(host.paths.ControlSocket, request, 2*time.Second)
	if err != nil || !containsString([]string{"queued", "in_flight", "delivered"}, stringValue(duplicate["delivery"])) {
		t.Fatalf("duplicate wake = %#v, %v", duplicate, err)
	}
	waitForGrokDelivery(t, host, "message-1", "delivered")
	final, err := requestControl(host.paths.ControlSocket, request, 2*time.Second)
	if err != nil || stringValue(final["delivery"]) != "delivered" {
		t.Fatalf("delivered duplicate = %#v, %v", final, err)
	}
	conflict := map[string]any{
		"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		"item": map[string]any{"id": "message-1", "message": "different"},
	}
	conflicted, err := requestControl(host.paths.ControlSocket, conflict, 2*time.Second)
	if err != nil || stringValue(conflicted["delivery"]) != "conflict" {
		t.Fatalf("conflicting reuse = %#v, %v", conflicted, err)
	}

	requests := readGrokFakeRecords(t, record)
	methods := []string{}
	prompts := 0
	var argvs [][]string
	var initialize, authenticate, load, prompt map[string]any
	for _, entry := range requests {
		if rawArgs, ok := entry["args"].([]any); ok {
			args := make([]string, 0, len(rawArgs))
			for _, value := range rawArgs {
				args = append(args, stringValue(value))
			}
			argvs = append(argvs, args)
		}
		request, _ := entry["request"].(map[string]any)
		method := stringValue(request["method"])
		if method != "" {
			methods = append(methods, method)
		}
		if method == "session/prompt" {
			prompts++
		}
		switch method {
		case "initialize":
			initialize, _ = request["params"].(map[string]any)
		case "authenticate":
			authenticate, _ = request["params"].(map[string]any)
		case "session/load":
			load, _ = request["params"].(map[string]any)
		case "session/prompt":
			prompt, _ = request["params"].(map[string]any)
		}
	}
	if prompts != 1 {
		t.Fatalf("session/prompt count = %d, methods %v", prompts, methods)
	}
	var durable grokWakeRecord
	body, err := os.ReadFile(grokWakeRecordPath(resolveNativePaths(), host.config.SessionID, "message-1"))
	if err != nil || json.Unmarshal(body, &durable) != nil || durable.Delivery != "delivered" ||
		durable.MessageID != "message-1" || durable.Fingerprint != wakeItemFingerprint(item) {
		t.Fatalf("durable Grok wake = %+v, read=%v", durable, err)
	}
	wantPrefix := []string{"initialize", "authenticate", "session/load", "_x.ai/sessions/list", "session/prompt"}
	next := 0
	for _, method := range methods {
		if next < len(wantPrefix) && method == wantPrefix[next] {
			next++
		}
	}
	if next != len(wantPrefix) {
		t.Fatalf("ACP method prefix = %v, want %v", methods, wantPrefix)
	}
	if intValue(initialize["protocolVersion"]) != 1 || stringValue(authenticate["methodId"]) != "cached_token" ||
		stringValue(load["sessionId"]) != host.config.SessionID || !samePath(stringValue(load["cwd"]), host.config.Cwd) {
		t.Fatalf("ACP bootstrap mismatch: initialize=%#v authenticate=%#v load=%#v", initialize, authenticate, load)
	}
	if _, ok := load["_meta"]; ok {
		t.Fatalf("session/load unexpectedly changes Grok policy: %#v", load)
	}
	if _, ok := prompt["_meta"]; ok {
		t.Fatalf("session/prompt unexpectedly changes Grok policy: %#v", prompt)
	}
	wantLeader := []string{"--permission-mode", "default", "agent", "leader", "--leader-socket", host.paths.LeaderSocket, "--no-exit-on-disconnect", "--relay-on-demand", "--no-auto-update"}
	wantBridge := []string{"--no-auto-update", "--permission-mode", "default", "--leader-socket", host.paths.LeaderSocket, "agent", "--leader", "stdio"}
	if !containsStringSlice(argvs, wantLeader) || !containsStringSlice(argvs, wantBridge) {
		t.Fatalf("Grok subprocess argv = %v, want leader %v and bridge %v", argvs, wantLeader, wantBridge)
	}
	for _, argv := range argvs {
		if strings.Contains(strings.Join(argv, "\x00"), host.config.LaunchToken) {
			t.Fatal("raw launch token leaked into Grok child argv")
		}
	}
	if live := liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID); live == nil {
		t.Fatal("loaded Grok host was not live-attested")
	}
}

func TestGrokHostRequiresTokenAndExactSession(t *testing.T) {
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-auth")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	base := map[string]any{"action": "status", "sessionId": host.config.SessionID}
	if _, err := requestControl(host.paths.ControlSocket, base, time.Second); err == nil {
		t.Fatal("tokenless control request was accepted")
	}
	base["launchToken"] = host.config.LaunchToken
	base["sessionId"] = "other-session"
	if _, err := requestControl(host.paths.ControlSocket, base, time.Second); err == nil {
		t.Fatal("wrong-session control request was accepted")
	}
}

func TestInferGrokParentRequiresLiveLaunchCapabilityAndLeaderAncestry(t *testing.T) {
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-lane-owner")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	t.Setenv(grokLaunchTokenEnv, host.config.LaunchToken)
	t.Setenv(grokSessionIDEnv, host.config.SessionID)

	owner, ok := inferGrokParent(resolveNativePaths(), host.leader.cmd.Process.Pid)
	if !ok || owner.PID != host.config.OwnerPID || owner.ProcStart != host.config.OwnerProcStart ||
		owner.SessionID != host.config.SessionID || owner.PermissionMode != "default" {
		t.Fatalf("Grok lane owner = %+v, %v", owner, ok)
	}
	if _, ok := inferGrokParent(resolveNativePaths(), os.Getpid()); ok {
		t.Fatal("process outside the private leader tree acquired Grok lane ownership")
	}
	t.Setenv(grokLaunchTokenEnv, strings.Repeat("b", 32))
	if _, ok := inferGrokParent(resolveNativePaths(), host.leader.cmd.Process.Pid); ok {
		t.Fatal("mismatched inherited launch token acquired Grok lane ownership")
	}
}

func TestGrokHostReconnectsACPBridgeBeforeNextWake(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "bridge-exited")
	t.Setenv("GROK_FAKE_EXIT_AFTER_LOAD_ONCE", marker)
	host, cancel, result, record := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-reconnect")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		host.acpMu.Lock()
		dead := host.acp != nil
		if dead {
			select {
			case <-host.acp.readDone:
			default:
				dead = false
			}
		}
		host.acpMu.Unlock()
		if dead {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	response, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		"item": map[string]any{"id": "after-reconnect", "message": "wake after bridge exit"},
	}, time.Second)
	if err != nil || stringValue(response["delivery"]) != "accepted" {
		t.Fatalf("wake after bridge exit = %#v, %v", response, err)
	}
	waitForGrokDelivery(t, host, "after-reconnect", "delivered")
	requests := readGrokFakeRecords(t, record)
	initializeCount, promptCount := 0, 0
	for _, entry := range requests {
		request, _ := entry["request"].(map[string]any)
		switch stringValue(request["method"]) {
		case "initialize":
			initializeCount++
		case "session/prompt":
			promptCount++
		}
	}
	if initializeCount != 2 || promptCount != 1 {
		t.Fatalf("reconnect request counts initialize=%d prompt=%d", initializeCount, promptCount)
	}
}

func TestGrokHostStatusDoesNotDeadlockInjectedTurn(t *testing.T) {
	t.Setenv("GROK_FAKE_PROMPT_DELAY_MS", "500")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-busy-status")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	request := map[string]any{
		"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		"item": map[string]any{"id": "busy-status", "message": "call an MCP tool"},
	}
	response, err := requestControl(host.paths.ControlSocket, request, time.Second)
	if err != nil || stringValue(response["delivery"]) != "accepted" {
		t.Fatalf("busy wake = %#v, %v", response, err)
	}
	waitForGrokDelivery(t, host, "busy-status", "in_flight")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, valid := host.activePromptPermissionSnapshot(); valid {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, valid := host.activePromptPermissionSnapshot(); !valid {
		t.Fatal("injected prompt did not publish its active permission snapshot")
	}
	started := time.Now()
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, 250*time.Millisecond)
	ready, _ := status["ready"].(bool)
	deferred, _ := status["refreshDeferred"].(bool)
	if err != nil || !deferred || !ready || stringValue(status["permissionAuthority"]) != "active_prompt_snapshot" {
		t.Fatalf("busy status = %#v, %v", status, err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("busy status blocked behind session/prompt for %s", elapsed)
	}
	waitForGrokDelivery(t, host, "busy-status", "delivered")
}

func TestGrokHostBusyStatusWithoutActivePromptSnapshotFailsClosed(t *testing.T) {
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-busy-no-snapshot")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	host.acpMu.Lock()
	started := time.Now()
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, 250*time.Millisecond)
	host.acpMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "without an authoritative active prompt permission snapshot") {
		t.Fatalf("busy status without prompt snapshot = %#v, %v", status, err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("busy status without prompt snapshot blocked for %s", elapsed)
	}
}

func TestGrokHostCannotRepublishAfterCleanupStarts(t *testing.T) {
	host := &grokHost{done: make(chan struct{})}
	close(host.done)
	if err := host.ensurePeerPublished(); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("publish after stop error = %v", err)
	}
}

func TestGrokHostDoesNotPublishBeforeSuccessfulAuthentication(t *testing.T) {
	t.Setenv("GROK_FAKE_AUTH_REJECT", "1")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-bad-auth")
	defer stopTestGrokHost(t, host, cancel, result)
	time.Sleep(400 * time.Millisecond)
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err == nil {
		t.Fatalf("authentication failure returned status %#v", status)
	}
	if live := liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID); live != nil {
		t.Fatalf("authentication failure published live peer: %#v", live)
	}
}

func TestGrokHostDoesNotPublishWithoutCachedAuthenticationMethod(t *testing.T) {
	t.Setenv("GROK_FAKE_NO_CACHED_TOKEN", "1")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-no-cached-auth")
	defer stopTestGrokHost(t, host, cancel, result)
	time.Sleep(300 * time.Millisecond)
	_, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err == nil || host.peer != nil {
		t.Fatalf("host accepted missing cached auth: err=%v peer=%v", err, host.peer)
	}
}

func TestGrokHostDoesNotPublishWithoutAuthoritativeLivePermission(t *testing.T) {
	t.Setenv("GROK_FAKE_BAD_ROSTER", "1")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-bad-roster")
	defer stopTestGrokHost(t, host, cancel, result)
	time.Sleep(300 * time.Millisecond)
	_, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err == nil || host.peer != nil {
		t.Fatalf("host accepted missing live permission: err=%v peer=%v", err, host.peer)
	}
}

func TestGrokHostRefreshesRuntimePermissionMode(t *testing.T) {
	yolo := filepath.Join(t.TempDir(), "yolo")
	if err := os.WriteFile(yolo, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_FAKE_YOLO_FILE", yolo)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-mode-refresh")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	if err := os.WriteFile(yolo, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err != nil || stringValue(status["permissionMode"]) != "bypassPermissions" {
		t.Fatalf("bypass status = %#v, %v", status, err)
	}
	record := readGrokLaunchRecord(grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID))
	state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(host.config.SessionID), "state.json"))
	registry := readJSONMap(stringValue(state["registryFile"]))
	if record == nil || record.PermissionMode != "bypassPermissions" ||
		stringValue(state["permissionMode"]) != "bypassPermissions" ||
		stringValue(registry["permissionMode"]) != "bypassPermissions" {
		t.Fatalf("bypass mode was not persisted and published: record=%#v state=%#v registry=%#v", record, state, registry)
	}
	if err := os.WriteFile(yolo, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err != nil || stringValue(status["permissionMode"]) != "default" {
		t.Fatalf("prompting status = %#v, %v", status, err)
	}
}

func TestGrokHostPermissionPublishFailureRemainsDirtyAndRetries(t *testing.T) {
	yolo := filepath.Join(t.TempDir(), "yolo")
	if err := os.WriteFile(yolo, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_FAKE_YOLO_FILE", yolo)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-mode-publish-retry")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	var reject atomic.Bool
	reject.Store(true)
	host.peerMu.Lock()
	host.publishPermission = func(peer *daemon) error {
		if reject.Load() {
			return errors.New("injected registry publication failure")
		}
		return peer.writeRecordsLocked()
	}
	host.peerMu.Unlock()
	if err := os.WriteFile(yolo, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "injected registry publication failure") {
		t.Fatalf("failed permission publication status = %#v, %v", status, err)
	}
	if mode := host.currentPermissionMode(); mode != "default" {
		t.Fatalf("failed publication committed host mode %q", mode)
	}
	if record := readGrokLaunchRecord(grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID)); record == nil || record.PermissionMode != "default" {
		t.Fatalf("failed publication committed launch record %#v", record)
	}

	reject.Store(false)
	status, err = requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err != nil || stringValue(status["permissionMode"]) != "bypassPermissions" {
		t.Fatalf("retried permission publication status = %#v, %v", status, err)
	}
	record := readGrokLaunchRecord(grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID))
	state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(host.config.SessionID), "state.json"))
	registry := readJSONMap(stringValue(state["registryFile"]))
	if record == nil || record.PermissionMode != "bypassPermissions" ||
		stringValue(state["permissionMode"]) != "bypassPermissions" ||
		stringValue(registry["permissionMode"]) != "bypassPermissions" {
		t.Fatalf("retried mode was not persisted and published: record=%#v state=%#v registry=%#v", record, state, registry)
	}
}

func TestGrokHostRejectsConcurrentOwnerForSameSession(t *testing.T) {
	first, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-lease")
	defer stopTestGrokHost(t, first, cancel, result)
	waitGrokHostReady(t, first)
	secondConfig := first.config
	secondConfig.LaunchToken = strings.Repeat("b", 32)
	second, err := newGrokHost(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := second.run(ctx); err == nil || !strings.Contains(err.Error(), "already has a launch host") {
		t.Fatalf("second host error = %v", err)
	}
}

func TestGrokHostOwnerDeathStopsProcessGroupAndUnpublishes(t *testing.T) {
	childFile := filepath.Join(t.TempDir(), "leader-child.pid")
	t.Setenv("GROK_FAKE_CHILD_FILE", childFile)
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerStart := readProcStart(owner.Process.Pid)
	if ownerStart == "" {
		_ = owner.Process.Kill()
		_ = owner.Wait()
		t.Fatal("owner process has no start token")
	}
	host, cancel, result, _ := startTestGrokHost(t, owner.Process.Pid, ownerStart, "session-owner-death")
	defer cancel()
	waitGrokHostReady(t, host)
	leaderPID := host.leader.cmd.Process.Pid
	childPID := waitGrokFakeChildPID(t, childFile)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("host exit after owner death: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not stop after exact owner died")
	}
	if processIdentityMayBeLive(leaderPID, "") {
		t.Fatalf("private leader %d survived host cleanup", leaderPID)
	}
	if processIdentityMayBeLive(childPID, "") {
		t.Fatalf("private leader descendant %d survived process-group cleanup", childPID)
	}
	if _, err := os.Lstat(host.paths.ControlSocket); !os.IsNotExist(err) {
		t.Fatalf("control socket survived cleanup: %v", err)
	}
	if live := liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID); live != nil {
		t.Fatalf("dead launch remains attested: %#v", live)
	}
}

func containsStringSlice(haystack [][]string, wanted []string) bool {
	for _, candidate := range haystack {
		if strings.Join(candidate, "\x00") == strings.Join(wanted, "\x00") {
			return true
		}
	}
	return false
}

func waitGrokFakeChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, _ := strconv.Atoi(string(body))
			if pid > 1 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake leader did not record its descendant pid")
	return 0
}

func TestGrokRuntimePathsStayCompactAndDoNotExposeToken(t *testing.T) {
	token := strings.Repeat("secret-token-", 4)
	paths := grokRuntimePaths(strings.Repeat("/long-runtime", 12), os.Getuid(), token)
	if len(paths.ControlSocket) > 92 || len(paths.LeaderSocket) > 92 {
		t.Fatalf("Grok sockets exceed compact budget: %#v", paths)
	}
	if strings.Contains(paths.ControlSocket, token) || strings.Contains(paths.LeaderSocket, token) {
		t.Fatal("raw launch token leaked into a socket path")
	}
}

func startTestGrokHost(t *testing.T, ownerPID int, ownerStart, sessionID string) (*grokHost, context.CancelFunc, <-chan error, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(grokFakeProcessEnv, "1")
	record := filepath.Join(root, "fake.jsonl")
	t.Setenv("GROK_FAKE_RECORD", record)
	t.Setenv("GROK_FAKE_PROMPT_DELAY_MS", "75")
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := grokHostConfig{
		GrokBin: os.Args[0], SessionID: sessionID, Cwd: root,
		OwnerPID: ownerPID, OwnerProcStart: ownerStart,
		LaunchToken: strings.Repeat("a", 32), RuntimeDir: filepath.Join(root, "run"),
		Name: "grok-test", PermissionMode: "default",
	}
	config.command = func(args ...string) *exec.Cmd {
		argv := append([]string{"-test.run=^TestGrokFakeProcess$", "--"}, args...)
		return exec.Command(os.Args[0], argv...)
	}
	host, err := newGrokHost(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- host.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probeUnixSocket(host.paths.ControlSocket, 100*time.Millisecond) {
			return host, cancel, result, record
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-result
	t.Fatal("Grok host control socket did not start")
	return nil, nil, nil, ""
}

func waitGrokHostReady(t *testing.T, host *grokHost) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		}, 500*time.Millisecond)
		if err == nil {
			if ready, _ := response["ready"].(bool); ready {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("Grok host did not load and publish its exact session")
}

func waitForGrokDelivery(t *testing.T, host *grokHost, messageID, wanted string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "wake_status", "sessionId": host.config.SessionID,
			"launchToken": host.config.LaunchToken, "messageId": messageID,
		}, 500*time.Millisecond)
		if err == nil && stringValue(response["delivery"]) == wanted {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Grok wake %s did not reach %s", messageID, wanted)
}

func stopTestGrokHost(t *testing.T, _ *grokHost, cancel context.CancelFunc, result <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("stop Grok host: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Grok host cleanup timed out")
	}
}

var grokFakeReadMu sync.Mutex

func readGrokFakeRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	grokFakeReadMu.Lock()
	defer grokFakeReadMu.Unlock()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}
	return records
}
