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
	"reflect"
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
	var activeTurnUntil time.Time
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
		case "_x.ai/sessions/list":
			if delay := time.Until(activeTurnUntil); delay > 0 {
				time.Sleep(delay)
			}
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
			if marker := os.Getenv("GROK_FAKE_EXIT_AFTER_ROSTER_ONCE"); marker != "" {
				file, createErr := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if createErr == nil {
					_ = file.Close()
					writeGrokFakeResponse(request["id"], result, nil)
					return
				}
			}
		case "_x.ai/mcp/call":
			status := "ready"
			if path := os.Getenv("GROK_FAKE_MCP_STATUS_FILE"); path != "" {
				body, _ := os.ReadFile(path)
				status = strings.TrimSpace(string(body))
			}
			result["result"] = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "peer inventory ready"}},
				"isError": status != "ready",
			}
		case "_x.ai/interject":
			if delay, _ := strconv.Atoi(os.Getenv("GROK_FAKE_PROMPT_DELAY_MS")); delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			result["result"] = map[string]any{"status": "queued"}
			if delay, _ := strconv.Atoi(os.Getenv("GROK_FAKE_ACTIVE_TURN_MS")); delay > 0 {
				activeTurnUntil = time.Now().Add(time.Duration(delay) * time.Millisecond)
			}
			inner, _ := request["params"].(map[string]any)
			notification := map[string]any{
				"sessionId": stringValue(inner["sessionId"]), "interjectionId": stringValue(inner["interjectionId"]),
				"text": stringValue(inner["text"]),
			}
			if os.Getenv("GROK_FAKE_INTERJECT_ECHO_ORDER") == "after" {
				writeGrokFakeResponse(request["id"], result, nil)
				writeGrokFakeNotification("_x.ai/session/interjection", notification)
				continue
			}
			if os.Getenv("GROK_FAKE_NO_INTERJECT_ECHO") != "1" {
				writeGrokFakeNotification("_x.ai/session/interjection", notification)
			}
			if os.Getenv("GROK_FAKE_CLOSE_AFTER_INTERJECT") == "1" {
				writeGrokFakeResponse(request["id"], result, nil)
				return
			}
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
	if err != nil || !containsString([]string{"queued", "in_flight", "actor_accepted"}, stringValue(duplicate["delivery"])) {
		t.Fatalf("duplicate wake = %#v, %v", duplicate, err)
	}
	waitForGrokDelivery(t, host, "message-1", "actor_accepted")
	final, err := requestControl(host.paths.ControlSocket, request, 2*time.Second)
	if err != nil || stringValue(final["delivery"]) != "actor_accepted" {
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
	var argvs [][]string
	var initialize, authenticate, mcpCall, interject map[string]any
	loads, prompts := 0, 0
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
		switch method {
		case "initialize":
			initialize, _ = request["params"].(map[string]any)
		case "authenticate":
			authenticate, _ = request["params"].(map[string]any)
		case "session/load":
			loads++
		case "session/prompt":
			prompts++
		case "_x.ai/mcp/call":
			mcpCall, _ = request["params"].(map[string]any)
		case "_x.ai/interject":
			interject, _ = request["params"].(map[string]any)
		}
	}
	if loads != 0 || prompts != 0 || interject == nil {
		t.Fatalf("wake transport load=%d prompt=%d interject=%#v, methods %v", loads, prompts, interject, methods)
	}
	var durable grokWakeRecord
	body, err := os.ReadFile(grokWakeRecordPath(resolveNativePaths(), host.config.SessionID, "message-1"))
	if err != nil || json.Unmarshal(body, &durable) != nil || durable.Delivery != "actor_accepted" ||
		durable.MessageID != "message-1" || durable.Fingerprint != wakeItemFingerprint(item) {
		t.Fatalf("durable Grok wake = %+v, read=%v", durable, err)
	}
	wantPrefix := []string{"initialize", "authenticate", "_x.ai/sessions/list", "_x.ai/mcp/call", "_x.ai/interject"}
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
		stringValue(mcpCall["sessionId"]) != host.config.SessionID || stringValue(mcpCall["server"]) != "agent_sessions" ||
		stringValue(mcpCall["tool"]) != "list_peers" ||
		stringValue(interject["sessionId"]) != host.config.SessionID ||
		stringValue(interject["interjectionId"]) != "message-1" || !strings.Contains(stringValue(interject["text"]), "do exactly one turn") {
		t.Fatalf("ACP bootstrap mismatch: initialize=%#v authenticate=%#v mcp=%#v interject=%#v", initialize, authenticate, mcpCall, interject)
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

func TestGrokHostDoesNotPublishBeforeAgentSessionsMCPIsReady(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "mcp-status")
	if err := os.WriteFile(statusFile, []byte("initializing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_FAKE_MCP_STATUS_FILE", statusFile)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-mcp-readiness")
	defer stopTestGrokHost(t, host, cancel, result)
	time.Sleep(300 * time.Millisecond)
	host.peerMu.Lock()
	published := host.peer != nil
	host.peerMu.Unlock()
	if published || liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID) != nil {
		t.Fatal("Grok peer published before agent_sessions MCP was ready")
	}
	if err := os.WriteFile(statusFile, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitGrokHostReady(t, host)
}

func TestGrokMCPReadinessGatesPublicationNotLiveAttestation(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "mcp-status")
	if err := os.WriteFile(statusFile, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_FAKE_MCP_STATUS_FILE", statusFile)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-mcp-flap")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	if err := os.WriteFile(statusFile, []byte("initializing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	ready, _ := status["ready"].(bool)
	if err != nil || !ready || stringValue(status["permissionAuthority"]) != "live_roster" {
		t.Fatalf("published Grok status after MCP inventory flap = %#v, %v", status, err)
	}
}

func TestGrokAgentSessionsMCPReadinessRequiresSuccessfulToolResult(t *testing.T) {
	response := map[string]any{"result": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "ready"}},
	}}
	if err := grokAgentSessionsMCPCallReady(response); err != nil {
		t.Fatalf("successful agent_sessions readiness call was rejected: %v", err)
	}
	response["result"].(map[string]any)["isError"] = true
	if err := grokAgentSessionsMCPCallReady(response); err == nil {
		t.Fatal("failed agent_sessions readiness call was accepted")
	}
	delete(response["result"].(map[string]any), "isError")
	delete(response["result"].(map[string]any), "content")
	if err := grokAgentSessionsMCPCallReady(response); err == nil {
		t.Fatal("empty agent_sessions readiness call was accepted")
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
	t.Setenv("GROK_FAKE_EXIT_AFTER_ROSTER_ONCE", marker)
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
	waitForGrokDelivery(t, host, "after-reconnect", "actor_accepted")
	requests := readGrokFakeRecords(t, record)
	initializeCount, interjectCount := 0, 0
	for _, entry := range requests {
		request, _ := entry["request"].(map[string]any)
		switch stringValue(request["method"]) {
		case "initialize":
			initializeCount++
		case "_x.ai/interject":
			interjectCount++
		}
	}
	if initializeCount != 2 || interjectCount != 1 {
		t.Fatalf("reconnect request counts initialize=%d interject=%d", initializeCount, interjectCount)
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
		if _, valid := host.activeInterjectionPermissionSnapshot(); valid {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, valid := host.activeInterjectionPermissionSnapshot(); !valid {
		t.Fatal("interjection did not publish its active permission snapshot")
	}
	started := time.Now()
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, 250*time.Millisecond)
	ready, _ := status["ready"].(bool)
	deferred, _ := status["refreshDeferred"].(bool)
	if err != nil || !deferred || !ready || stringValue(status["permissionAuthority"]) != "active_interjection_snapshot" {
		t.Fatalf("busy status = %#v, %v", status, err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("busy status blocked behind x.ai/interject for %s", elapsed)
	}
	waitForGrokDelivery(t, host, "busy-status", "actor_accepted")
}

func TestGrokHostStatusUsesSnapshotUntilPostInterjectionRosterRefresh(t *testing.T) {
	t.Setenv("GROK_FAKE_ACTIVE_TURN_MS", "750")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-post-echo-status")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	response, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		"item": map[string]any{"id": "post-echo-status", "message": "call an MCP tool after actor acknowledgement"},
	}, time.Second)
	if err != nil || stringValue(response["delivery"]) != "accepted" {
		t.Fatalf("post-echo wake = %#v, %v", response, err)
	}
	waitForGrokDelivery(t, host, "post-echo-status", "actor_accepted")
	if _, valid := host.activeInterjectionPermissionSnapshot(); !valid {
		t.Fatal("actor acknowledgement cleared the permission snapshot before the generated turn")
	}
	host.modeMu.RLock()
	record := host.record
	host.modeMu.RUnlock()
	started := time.Now()
	if err := refreshGrokLaunchPermission(&record, host.config.LaunchToken); err != nil {
		t.Fatalf("MCP permission refresh before background roster poll: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("MCP permission refresh started a blocking roster request in %s", elapsed)
	}

	refreshDone := make(chan error, 1)
	go func() {
		ctx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer refreshCancel()
		refreshDone <- host.ensureACP(ctx)
	}()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !host.acpMu.TryLock() {
			break
		}
		host.acpMu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if host.acpMu.TryLock() {
		host.acpMu.Unlock()
		t.Fatal("post-interjection roster refresh did not hold the ACP stream")
	}

	started = time.Now()
	if err := refreshGrokLaunchPermission(&record, host.config.LaunchToken); err != nil {
		t.Fatalf("MCP permission refresh during generated turn: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("MCP permission refresh blocked behind generated turn for %s", elapsed)
	}
	if err := <-refreshDone; err != nil {
		t.Fatalf("post-interjection roster refresh: %v", err)
	}
	if _, valid := host.activeInterjectionPermissionSnapshot(); valid {
		t.Fatal("successful post-interjection roster refresh did not retire the snapshot")
	}
}

func TestGrokHostActiveInterjectionPermissionSnapshotExpires(t *testing.T) {
	host := &grokHost{mode: "bypassPermissions"}
	host.beginActiveInterjectionPermissionSnapshot()
	if mode, valid := host.activeInterjectionPermissionSnapshot(); !valid || mode != "bypassPermissions" {
		t.Fatalf("fresh snapshot = %q, %v", mode, valid)
	}
	host.modeMu.Lock()
	host.activeInterjectionAt = time.Now().Add(-grokInterjectionModeTTL - time.Second)
	host.modeMu.Unlock()
	if mode, valid := host.activeInterjectionPermissionSnapshot(); valid || mode != "" {
		t.Fatalf("expired snapshot remained authoritative: %q, %v", mode, valid)
	}
}

func TestGrokHostRequiresActorInterjectionEcho(t *testing.T) {
	t.Run("response before echo", func(t *testing.T) {
		t.Setenv("GROK_FAKE_INTERJECT_ECHO_ORDER", "after")
		host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-echo-after")
		defer stopTestGrokHost(t, host, cancel, result)
		waitGrokHostReady(t, host)
		response, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
			"item": map[string]any{"id": "echo-after", "message": "require actor echo"},
		}, time.Second)
		if err != nil || stringValue(response["delivery"]) != "accepted" {
			t.Fatalf("echo-after wake = %#v, %v", response, err)
		}
		waitForGrokDelivery(t, host, "echo-after", "actor_accepted")
	})

	t.Run("queued response without echo stays ambiguous", func(t *testing.T) {
		t.Setenv("GROK_FAKE_NO_INTERJECT_ECHO", "1")
		t.Setenv("GROK_FAKE_CLOSE_AFTER_INTERJECT", "1")
		host, cancel, result, record := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-no-echo")
		defer stopTestGrokHost(t, host, cancel, result)
		waitGrokHostReady(t, host)
		response, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
			"item": map[string]any{"id": "no-echo", "message": "do not replay this"},
		}, time.Second)
		if err != nil || stringValue(response["delivery"]) != "accepted" {
			t.Fatalf("no-echo wake = %#v, %v", response, err)
		}
		deadline := time.Now().Add(3 * time.Second)
		ambiguous := false
		for time.Now().Before(deadline) {
			status, statusErr := requestControl(host.paths.ControlSocket, map[string]any{
				"action": "wake_status", "sessionId": host.config.SessionID,
				"launchToken": host.config.LaunchToken, "messageId": "no-echo",
			}, time.Second)
			if statusErr == nil && stringValue(status["delivery"]) == "in_flight" &&
				strings.Contains(stringValue(status["detail"]), "unknown") {
				ambiguous = true
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !ambiguous {
			t.Fatal("queued response without an actor echo did not become visibly ambiguous")
		}
		time.Sleep(750 * time.Millisecond)
		interjects := 0
		for _, entry := range readGrokFakeRecords(t, record) {
			request, _ := entry["request"].(map[string]any)
			if stringValue(request["method"]) == "_x.ai/interject" {
				interjects++
			}
		}
		if interjects != 1 {
			t.Fatalf("ambiguous actor acknowledgement was replayed %d times", interjects)
		}
	})
}

func TestGrokHostBusyStatusWithoutActivePromptSnapshotIsRetryable(t *testing.T) {
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-busy-no-snapshot")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	host.acpMu.Lock()
	started := time.Now()
	status, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, 250*time.Millisecond)
	if busy, _ := status["refreshBusy"].(bool); err != nil || !busy || stringValue(status["permissionMode"]) != "" {
		t.Fatalf("busy status without interjection snapshot = %#v, %v", status, err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("busy status without interjection snapshot blocked for %s", elapsed)
	}
	host.modeMu.RLock()
	record := host.record
	host.modeMu.RUnlock()
	go func() {
		time.Sleep(75 * time.Millisecond)
		host.acpMu.Unlock()
	}()
	started = time.Now()
	if err := refreshGrokLaunchPermission(&record, host.config.LaunchToken); err != nil {
		t.Fatalf("retry transient busy permission refresh: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed >= time.Second {
		t.Fatalf("transient busy permission refresh completed in %s", elapsed)
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

func TestActiveGrokLaunchSessionsBlocksLiveOrUnverifiableInstallState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	record := grokLaunchRecord{
		SessionID: "install-live", TokenHash: strings.Repeat("a", 64),
		OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid()),
	}
	path := grokLaunchRecordPath(paths, record.SessionID)
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	live, err := activeGrokLaunchSessions(paths)
	if err != nil || !reflect.DeepEqual(live, []string{record.SessionID}) {
		t.Fatalf("live Grok inventory = %v, %v", live, err)
	}

	record.OwnerPID, record.OwnerProcStart = 0, ""
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	live, err = activeGrokLaunchSessions(paths)
	if err != nil || len(live) != 0 {
		t.Fatalf("stale Grok inventory = %v, %v", live, err)
	}

	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := activeGrokLaunchSessions(paths); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed Grok inventory error = %v", err)
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
	t.Fatal("Grok host did not observe and publish its exact resident session")
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
