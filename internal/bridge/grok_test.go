package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	federator "github.com/antst/agent-sessions/internal/attachmentcontrol"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const grokFakeProcessEnv = "AGENT_SESSIONS_GROK_FAKE_PROCESS"

const (
	grokFakeLeaderStderrEnv   = "AGENT_SESSIONS_GROK_FAKE_LEADER_STDERR"
	grokFakeLeaderFinalEnv    = "AGENT_SESSIONS_GROK_FAKE_LEADER_FINAL_STDERR"
	grokFakeLeaderExitEnv     = "AGENT_SESSIONS_GROK_FAKE_LEADER_EXIT"
	grokFakeObserverStderrEnv = "AGENT_SESSIONS_GROK_FAKE_OBSERVER_STDERR"
	grokFakeObserverExitEnv   = "AGENT_SESSIONS_GROK_FAKE_OBSERVER_EXIT"
	grokFakeObserverFinalEnv  = "AGENT_SESSIONS_GROK_FAKE_OBSERVER_FINAL_STDERR"
)

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
	if diagnostic := os.Getenv(grokFakeLeaderStderrEnv); diagnostic != "" {
		_, _ = fmt.Fprint(os.Stderr, diagnostic)
	}
	if final := os.Getenv(grokFakeLeaderFinalEnv); final != "" {
		_, _ = fmt.Fprint(os.Stderr, final)
	}
	if code, _ := strconv.Atoi(os.Getenv(grokFakeLeaderExitEnv)); code > 0 {
		os.Exit(code)
	}
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
	if diagnostic := os.Getenv(grokFakeObserverStderrEnv); diagnostic != "" {
		_, _ = fmt.Fprint(os.Stderr, diagnostic)
	}
	if final := os.Getenv(grokFakeObserverFinalEnv); final != "" {
		_ = os.Stdout.Close()
		time.Sleep(25 * time.Millisecond)
		_, _ = fmt.Fprint(os.Stderr, final)
	}
	if code, _ := strconv.Atoi(os.Getenv(grokFakeObserverExitEnv)); code > 0 {
		os.Exit(code)
	}
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
		case "session/new", "session/load":
			if os.Getenv("GROK_FAKE_REQUIRE_AGENT_SESSIONS_MCP") == "1" {
				params, _ := request["params"].(map[string]any)
				servers, _ := params["mcpServers"].([]any)
				server := map[string]any{}
				if len(servers) == 1 {
					server, _ = servers[0].(map[string]any)
				}
				args, _ := server["args"].([]any)
				if len(servers) != 1 || stringValue(server["name"]) != "agent_sessions" ||
					!filepath.IsAbs(stringValue(server["command"])) || len(args) != 1 || stringValue(args[0]) != "grok-mcp" ||
					!reflect.DeepEqual(server["env"], []any{}) {
					writeGrokFakeResponse(request["id"], nil, map[string]any{
						"code": -32602, "message": "missing injected agent_sessions MCP",
					})
					continue
				}
			}
			result["sessionId"] = defaultString(os.Getenv("GROK_FAKE_GENERATED_SESSION_ID"), os.Getenv(grokSessionIDEnv))
		case "session/prompt":
			if delay, _ := strconv.Atoi(os.Getenv("GROK_FAKE_PROMPT_DELAY_MS")); delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			params, _ := request["params"].(map[string]any)
			writeGrokFakeNotification("session/update", map[string]any{
				"sessionId": stringValue(params["sessionId"]),
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": defaultString(os.Getenv("GROK_FAKE_ANSWER"), "fake Grok lane answer")},
				},
			})
			result["stopReason"] = "end_turn"
		case "session/cancel":
			continue
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
			activity := defaultString(strings.TrimSpace(os.Getenv("GROK_FAKE_ACTIVITY")), "idle")
			if path := os.Getenv("GROK_FAKE_ACTIVITY_FILE"); path != "" {
				body, _ := os.ReadFile(path)
				activity = strings.TrimSpace(string(body))
			}
			result["result"] = map[string]any{"sessions": []any{map[string]any{
				"sessionId": defaultString(os.Getenv("GROK_FAKE_GENERATED_SESSION_ID"), os.Getenv(grokSessionIDEnv)),
				"title":     strings.TrimSpace(os.Getenv("GROK_FAKE_SESSION_TITLE")),
				"resident":  true, "yolo": yolo, "activity": activity,
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
			params, _ := request["params"].(map[string]any)
			arguments, _ := params["arguments"].(map[string]any)
			result["result"] = map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "peer inventory ready"}},
				"structuredContent": map[string]any{"sessionId": stringValue(arguments["session_id"]), "status": "starting"},
				"isError":           status != "ready",
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

func TestGrokManagedOutputUsesPrivateDiagnosticSink(t *testing.T) {
	leaderMarker := "FOREIGN-MCP-LEADER-STDERR\n"
	observerMarker := "ACP-OBSERVER-STDERR\n"
	t.Setenv(grokFakeLeaderStderrEnv, leaderMarker)
	t.Setenv(grokFakeObserverStderrEnv, observerMarker)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-private-diagnostics")
	stopped := false
	defer func() {
		if !stopped {
			stopTestGrokHost(t, host, cancel, result)
		}
	}()
	waitGrokHostReady(t, host)

	if host.leader.cmd.Stdout == os.Stderr || host.leader.cmd.Stderr == os.Stderr {
		t.Fatal("private Grok leader output still targets the interactive terminal")
	}
	host.acpMu.Lock()
	observer := host.acp
	host.acpMu.Unlock()
	if observer == nil || observer.process.cmd.Stderr == os.Stderr {
		t.Fatal("official Grok ACP observer stderr still targets the interactive terminal")
	}

	diagnosticPath := filepath.Join(host.paths.LaunchDir, "diagnostics.log")
	deadline := time.Now().Add(2 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		body, _ = os.ReadFile(diagnosticPath)
		if strings.Contains(string(body), leaderMarker) && strings.Contains(string(body), observerMarker) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(body), "[private Grok leader] "+leaderMarker) ||
		!strings.Contains(string(body), "[official Grok ACP observer] "+observerMarker) {
		t.Fatalf("private diagnostic log does not contain attributed child output: %q", body)
	}
	info, err := os.Stat(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() > grokDiagnosticFileLimit {
		t.Fatalf("private diagnostic log mode/size = %o/%d", info.Mode().Perm(), info.Size())
	}

	stopTestGrokHost(t, host, cancel, result)
	stopped = true
	if _, err := os.Lstat(diagnosticPath); !os.IsNotExist(err) {
		t.Fatalf("private diagnostic log survived cleanup: %v", err)
	}
}

func TestGrokManagedProcessFailuresKeepRawDiagnosticsPrivate(t *testing.T) {
	tests := []struct {
		name                         string
		role                         string
		exitEnv                      string
		stderrEnv                    string
		args                         []string
		closeStdoutBeforeFinalStderr bool
	}{
		{
			name: "leader", role: "private Grok leader exited", exitEnv: grokFakeLeaderExitEnv,
			stderrEnv: grokFakeLeaderStderrEnv,
			args:      []string{"--permission-mode", "default", "agent", "leader", "--leader-socket", filepath.Join(t.TempDir(), "leader.sock")},
		},
		{
			name: "observer", role: "official Grok ACP observer exited", exitEnv: grokFakeObserverExitEnv,
			stderrEnv: grokFakeObserverStderrEnv,
			args:      []string{"--no-auto-update", "agent", "--leader", "stdio"}, closeStdoutBeforeFinalStderr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			diagnosticPath := filepath.Join(root, "diagnostics.log")
			sink, err := newGrokDiagnosticSink(diagnosticPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sink.close() }()
			secretCanary := "SECRET-CANARY-" + strings.ToUpper(test.name)
			tailMarker := "TAIL-" + secretCanary
			diagnostic := "PREFIX-MUST-BE-EVICTED" + strings.Repeat("x", grokDiagnosticTailLimit+1024)
			finalDiagnostic := strings.Repeat("y", grokDiagnosticTailLimit+1024) + "\x1b[31m" + tailMarker + "\n"
			command := exec.Command(os.Args[0], append([]string{"-test.run=^TestGrokFakeProcess$", "--"}, test.args...)...)
			command.Env = append(os.Environ(), grokFakeProcessEnv+"=1", test.exitEnv+"=37")
			if test.closeStdoutBeforeFinalStderr {
				command.Env = append(command.Env, test.stderrEnv+"="+diagnostic, grokFakeObserverFinalEnv+"="+finalDiagnostic)
			} else {
				command.Env = append(command.Env, test.stderrEnv+"="+diagnostic, grokFakeLeaderFinalEnv+"="+finalDiagnostic)
			}
			processDiagnostics := sink.process(test.role)
			var stdout io.ReadCloser
			if test.closeStdoutBeforeFinalStderr {
				stdout, err = command.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				command.Stdout = processDiagnostics
			}
			command.Stderr = processDiagnostics
			managed, err := startGrokManagedProcess(command, processDiagnostics)
			if err != nil {
				t.Fatal(err)
			}
			if stdout != nil {
				if _, err := io.ReadAll(stdout); err != nil {
					t.Fatal(err)
				}
			}
			failure := managed.attributedError(test.role, nil)
			message := failure.Error()
			if !strings.Contains(message, test.role) || !strings.Contains(message, "private host diagnostics") ||
				strings.Contains(message, diagnosticPath) || strings.Contains(message, "bytes") ||
				strings.Contains(message, secretCanary) || strings.Contains(message, "\x1b") {
				t.Fatalf("managed-process error leaked raw diagnostics or lost safe attribution: %q", message)
			}
			body, err := os.ReadFile(diagnosticPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), tailMarker) || strings.Contains(string(body), "PREFIX-MUST-BE-EVICTED") {
				t.Fatalf("live bounded diagnostic file did not retain its newest tail: size=%d", len(body))
			}
			info, err := os.Stat(diagnosticPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 || info.Size() > grokDiagnosticFileLimit {
				t.Fatalf("bounded diagnostic file mode/size = %o/%d", info.Mode().Perm(), info.Size())
			}
		})
	}
}

func TestGrokDiagnosticSinkAmortizesSmallWritesAndKeepsNewestOutput(t *testing.T) {
	diagnosticPath := filepath.Join(t.TempDir(), "diagnostics.log")
	sink, err := newGrokDiagnosticSink(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.close() }()

	const writes = 12000
	for index := 0; index < writes; index++ {
		line := fmt.Sprintf("small-write-%05d-%s\n", index, strings.Repeat("x", 16))
		if _, err := sink.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), fmt.Sprintf("small-write-%05d", writes-1)) {
		t.Fatal("bounded diagnostic log lost its newest small write")
	}
	if strings.Contains(string(body), "small-write-00000") {
		t.Fatal("bounded diagnostic log retained output older than its compacted tail")
	}
	info, err := os.Stat(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > grokDiagnosticFileLimit {
		t.Fatalf("bounded diagnostic log size = %d, want <= %d", info.Size(), grokDiagnosticFileLimit)
	}
	sink.mu.Lock()
	compactions := sink.compactions
	sink.mu.Unlock()
	if compactions == 0 || compactions >= 16 {
		t.Fatalf("small writes caused %d compactions; want occasional amortized compaction", compactions)
	}
}

func TestGrokManagedProcessAttributionBoundsIncompleteJoin(t *testing.T) {
	diagnosticPath := filepath.Join(t.TempDir(), "diagnostics.log")
	sink, err := newGrokDiagnosticSink(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.close() }()
	role := "official Grok ACP observer exited"
	diagnostics := sink.process(role)
	process := &grokManagedProcess{done: make(chan struct{}), diagnostics: diagnostics}
	secretCanary := "SECRET-INCOMPLETE-JOIN-CANARY"

	started := time.Now()
	failure := process.attributedError(role, errors.New(secretCanary))
	elapsed := time.Since(started)
	if elapsed < grokDiagnosticJoinTimeout/2 || elapsed > 2*time.Second {
		t.Fatalf("incomplete managed-process join returned after %s", elapsed)
	}
	message := failure.Error()
	if !strings.Contains(message, role) || !strings.Contains(message, "join incomplete") ||
		!strings.Contains(message, "private host diagnostics") || strings.Contains(message, diagnosticPath) ||
		strings.Contains(message, "bytes") || strings.Contains(message, secretCanary) {
		t.Fatalf("incomplete-join error leaked raw diagnostics or lost fixed attribution: %q", message)
	}
	body, err := os.ReadFile(diagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secretCanary) {
		t.Fatal("unjoined managed-process cause was recorded before final join")
	}
}

func TestGrokObserverDiagnosticsNeverEnterWakeStatusOrDurableRecord(t *testing.T) {
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-private-wake-error")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	secretCanary := "SECRET-OBSERVER-CANARY-DO-NOT-PERSIST"
	t.Setenv(grokFakeObserverStderrEnv, "\x1b[31m"+secretCanary+"\n")
	t.Setenv(grokFakeObserverExitEnv, "41")
	host.acpMu.Lock()
	host.closeACPLocked()
	host.acpMu.Unlock()

	messageID := "private-observer-failure"
	response, err := requestControl(host.paths.ControlSocket, map[string]any{
		"action": "wake", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		"item": map[string]any{"id": messageID, "message": "exercise observer failure"},
	}, time.Second)
	if err != nil || stringValue(response["delivery"]) != "accepted" {
		t.Fatalf("queue wake for observer failure = %#v, %v", response, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var status map[string]any
	for time.Now().Before(deadline) {
		status, err = requestControl(host.paths.ControlSocket, map[string]any{
			"action": "wake_status", "sessionId": host.config.SessionID,
			"launchToken": host.config.LaunchToken, "messageId": messageID,
		}, time.Second)
		if err == nil && strings.Contains(stringValue(status["detail"]), "official Grok ACP observer") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	encodedStatus, _ := json.Marshal(status)
	recordPath := grokWakeRecordPath(resolveNativePaths(), host.config.SessionID, messageID)
	recordBody, readErr := os.ReadFile(recordPath)
	if err != nil || readErr != nil {
		t.Fatalf("observer failure wake status/read = %#v, %v / %v", status, err, readErr)
	}
	for label, body := range map[string][]byte{"wake_status": encodedStatus, "durable wake": recordBody} {
		if strings.Contains(string(body), secretCanary) || strings.Contains(string(body), "\x1b") ||
			strings.Contains(string(body), host.paths.LaunchDir) || strings.Contains(string(body), "bytes") {
			t.Fatalf("%s leaked raw observer diagnostics: %s", label, body)
		}
	}
	if detail := stringValue(status["detail"]); !strings.Contains(detail, "official Grok ACP observer") ||
		!strings.Contains(detail, "private host diagnostics") {
		t.Fatalf("wake status lost fixed role-only diagnostic metadata: %#v", status)
	}
	diagnosticBody, err := os.ReadFile(filepath.Join(host.paths.LaunchDir, "diagnostics.log"))
	if err != nil || !strings.Contains(string(diagnosticBody), secretCanary) {
		t.Fatalf("private diagnostic log did not retain observer canary: %v", err)
	}
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
		stringValue(mcpCall["tool"]) != "identity" ||
		stringValue(mcpCall["arguments"].(map[string]any)["session_id"]) != host.config.SessionID ||
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
		"content":           []any{map[string]any{"type": "text", "text": "ready"}},
		"structuredContent": map[string]any{"sessionId": "session-ready"},
	}}
	if err := grokAgentSessionsMCPCallReady(response); err != nil {
		t.Fatalf("successful agent_sessions readiness call was rejected: %v", err)
	}
	if err := grokAgentSessionsMCPIdentityReady(response, "session-ready"); err != nil {
		t.Fatalf("successful agent_sessions identity readiness call was rejected: %v", err)
	}
	delete(response["result"].(map[string]any), "structuredContent")
	if err := grokAgentSessionsMCPIdentityReady(response, "session-ready"); err != nil {
		t.Fatalf("successful text-only agent_sessions identity readiness call was rejected: %v", err)
	}
	response["result"].(map[string]any)["structuredContent"] = map[string]any{"sessionId": "session-ready"}
	if err := grokAgentSessionsMCPIdentityReady(response, "session-other"); err == nil {
		t.Fatal("wrong agent_sessions identity was accepted")
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
	live := liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID)
	if !ok || live == nil || owner.PID != live.HostPID || owner.ProcStart != live.HostProcStart ||
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
	host, cancel, result, record := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-reconnect")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	host.acpMu.Lock()
	oldPID := host.acp.process.cmd.Process.Pid
	stopGrokManagedProcess(host.acp.process, time.Second)
	select {
	case <-host.acp.readDone:
	case <-time.After(time.Second):
		host.acpMu.Unlock()
		t.Fatal("stopped observer did not close its ACP stream")
	}
	ctx, refreshCancel := context.WithTimeout(context.Background(), time.Second)
	err := host.ensureACPConnectedLocked(ctx)
	refreshCancel()
	newPID := 0
	if host.acp != nil {
		newPID = host.acp.process.cmd.Process.Pid
	}
	host.acpMu.Unlock()
	if err != nil || newPID <= 1 || newPID == oldPID {
		t.Fatalf("dead observer did not reconnect transparently in one call: old=%d new=%d err=%v", oldPID, newPID, err)
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
	host.requestStop()
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
	deadline := time.Now().Add(2 * time.Second)
	sawAuthFailure := false
	for time.Now().Before(deadline) {
		status, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		}, 500*time.Millisecond)
		if err != nil {
			if strings.Contains(err.Error(), "authenticate official Grok ACP observer") {
				sawAuthFailure = true
				break
			}
			t.Fatalf("missing cached auth returned an unrelated control error: %v", err)
		}
		if ready, _ := status["ready"].(bool); ready || stringValue(status["permissionMode"]) != "" {
			t.Fatalf("host reported readiness without cached auth: %#v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawAuthFailure {
		t.Fatal("host did not report the missing cached authentication method")
	}
	host.peerMu.Lock()
	peer := host.peer
	host.peerMu.Unlock()
	if peer != nil {
		t.Fatalf("host published without cached auth: %#v", peer)
	}
	if live := liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID); live != nil {
		t.Fatalf("missing cached auth published live peer: %#v", live)
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

func TestGrokHostPublishesLiveRosterActivity(t *testing.T) {
	activity := filepath.Join(t.TempDir(), "activity")
	if err := os.WriteFile(activity, []byte("idle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_FAKE_ACTIVITY_FILE", activity)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-activity")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	for _, test := range []struct {
		activity string
		status   string
	}{
		{activity: "working", status: "busy"},
		{activity: "needs_input", status: "waiting"},
		{activity: "idle", status: "idle"},
	} {
		if err := os.WriteFile(activity, []byte(test.activity+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
		}, time.Second); err != nil {
			t.Fatalf("refresh %s roster activity: %v", test.activity, err)
		}
		state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(host.config.SessionID), "state.json"))
		registry := readJSONMap(stringValue(state["registryFile"]))
		if host.currentStatus() != test.status || stringValue(state["status"]) != test.status || stringValue(registry["status"]) != test.status {
			t.Fatalf("%s activity publication = host %q state %#v registry %#v", test.activity, host.currentStatus(), state, registry)
		}
	}
}

func TestGrokHostInitialPublicationUsesAuthoritativeRosterStatus(t *testing.T) {
	for _, test := range []struct {
		activity string
		status   string
	}{
		{activity: "working", status: "busy"},
		{activity: "needs_input", status: "waiting"},
	} {
		t.Run(test.activity, func(t *testing.T) {
			t.Setenv("GROK_FAKE_ACTIVITY", test.activity)
			host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-initial-"+test.activity)
			defer stopTestGrokHost(t, host, cancel, result)
			waitGrokHostReady(t, host)
			state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(host.config.SessionID), "state.json"))
			registry := readJSONMap(stringValue(state["registryFile"]))
			if host.currentStatus() != test.status || stringValue(state["status"]) != test.status ||
				stringValue(registry["status"]) != test.status {
				t.Fatalf("initial %s publication = host %q state %#v registry %#v", test.activity, host.currentStatus(), state, registry)
			}
		})
	}
}

func TestGrokRosterChangedNotificationParsesExactResidentActivity(t *testing.T) {
	message := map[string]any{
		"method": "_x.ai/sessions/changed",
		"params": map[string]any{"method": "x.ai/sessions/changed", "params": map[string]any{
			"upserted": []any{
				map[string]any{"sessionId": "foreign", "resident": true, "yolo": true, "activity": "working"},
				map[string]any{"sessionId": "target", "title": "Native Review Session", "resident": true, "yolo": false, "activity": "needs_input"},
			},
		}},
	}
	state, ok := grokRosterNotificationState(message, "target")
	if !ok || state.name != "Native Review Session" || state.permissionMode != "default" || state.status != "waiting" {
		t.Fatalf("roster notification state = %+v, %v", state, ok)
	}
	if _, ok := grokRosterNotificationState(message, "absent"); ok {
		t.Fatal("foreign roster notification was accepted for absent session")
	}
	wrongWrapper := map[string]any{
		"method": "_x.ai/sessions/changed",
		"params": map[string]any{"method": "x.ai/something/else", "params": map[string]any{
			"upserted": []any{map[string]any{"sessionId": "target", "resident": true, "yolo": false, "activity": "working"}},
		}},
	}
	if _, ok := grokRosterNotificationState(wrongWrapper, "target"); ok {
		t.Fatal("notification with a different nested extension method was accepted")
	}
}

func TestGrokUUIDResumePublishesNativeRosterTitleUnlessNameIsExplicit(t *testing.T) {
	t.Setenv("GROK_FAKE_SESSION_TITLE", "Native Grok Session")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "019fe660-1c86-7700-b462-6ff16de00fc5")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	identity := host.identitySnapshot()
	state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(identity.sessionID), "state.json"))
	registry := readJSONMap(stringValue(state["registryFile"]))
	if identity.name != "Native-Grok-Session" || stringValue(state["name"]) != "Native-Grok-Session" ||
		stringValue(registry["name"]) != "Native-Grok-Session" || stringValue(registry["nameSource"]) != "canonical" {
		t.Fatalf("native title publication = identity %q state %#v registry %#v", identity.name, state, registry)
	}
}

func TestGrokExplicitNameOverridesNativeRosterTitle(t *testing.T) {
	t.Setenv("GROK_FAKE_SESSION_TITLE", "Native Grok Session")
	t.Setenv("GROK_FAKE_NAME_SPECIFIED", "1")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "019fe660-1c86-7700-b462-6ff16de00fc5")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	identity := host.identitySnapshot()
	state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(identity.sessionID), "state.json"))
	if identity.name != "grok-test" || stringValue(state["name"]) != "grok-test" {
		t.Fatalf("explicit peer name was overwritten by native title: %#v", state)
	}
}

func TestGrokNativeTitleResumeAdoptsSelectedUUIDAndTitle(t *testing.T) {
	attachmentID := "019fe660-1c86-7700-b462-6ff16de00fc5"
	selectedID := "3c0c0831-9bd7-40db-9ee3-e108f315ea57"
	t.Setenv("GROK_FAKE_GENERATED_SESSION_ID", selectedID)
	t.Setenv("GROK_FAKE_SESSION_TITLE", "Selected Native Title")
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), attachmentID)
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	identity := host.identitySnapshot()
	if identity.sessionID != selectedID || identity.name != "Selected-Native-Title" {
		t.Fatalf("adopted identity = %q / %q", identity.sessionID, identity.name)
	}
	paths := resolveNativePaths()
	if provisional := readGrokLaunchRecord(grokLaunchRecordPath(paths, attachmentID)); provisional != nil {
		t.Fatalf("provisional launch record survived adoption: %#v", provisional)
	}
	record := readGrokLaunchRecord(grokLaunchRecordPath(paths, selectedID))
	if record == nil || record.AttachmentID != attachmentID || record.Name != "Selected-Native-Title" {
		t.Fatalf("selected launch record = %#v", record)
	}
	t.Setenv(grokSessionIDEnv, attachmentID)
	t.Setenv(grokLaunchTokenEnv, host.config.LaunchToken)
	if launch := activeGrokLaunchForToken(paths, host.config.LaunchToken); launch == nil || launch.SessionID != selectedID {
		t.Fatalf("late-bound Grok launch capability resolved to %#v", launch)
	}
}

func TestGrokRosterTerminalAndRemovalLoseLiveAuthority(t *testing.T) {
	response := func(row map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{"sessions": []any{row}}}
	}
	for _, row := range []map[string]any{
		{"sessionId": "target", "resident": false, "yolo": false, "activity": "idle"},
		{"sessionId": "target", "resident": true, "yolo": false, "activity": "completed"},
		{"sessionId": "target", "resident": true, "yolo": false, "activity": "dormant"},
		{"sessionId": "target", "resident": true, "yolo": false, "activity": "dead"},
		{"sessionId": "target", "resident": true, "yolo": false, "activity": "future"},
	} {
		if _, err := grokRosterStateFromResponse(response(row), "target"); !errors.Is(err, errGrokRosterAuthorityLost) {
			t.Fatalf("unavailable roster row %#v returned %v", row, err)
		}
	}
	for _, sessions := range [][]any{
		{},
		{
			map[string]any{"sessionId": "target", "resident": true, "yolo": false, "activity": "idle"},
			map[string]any{"sessionId": "target", "resident": true, "yolo": false, "activity": "working"},
		},
	} {
		if _, err := grokRosterStateFromResponse(map[string]any{"result": map[string]any{"sessions": sessions}}, "target"); !errors.Is(err, errGrokRosterAuthorityLost) {
			t.Fatalf("ambiguous roster %#v returned %v", sessions, err)
		}
	}

	for _, message := range []map[string]any{
		{"method": "_x.ai/sessions/changed", "params": map[string]any{"removed": []any{"target"}}},
		{"method": "_x.ai/sessions/changed", "params": map[string]any{"upserted": []any{
			map[string]any{"sessionId": "target", "resident": false, "yolo": false, "activity": "idle"},
		}}},
	} {
		state, ok := grokRosterNotificationState(message, "target")
		if !ok || !state.fromPush || !state.authorityLost {
			t.Fatalf("authority-loss notification = %+v, %v", state, ok)
		}
	}
}

func TestGrokRosterPushWinsOneReconciliationWindowAndOldGenerationIsIgnored(t *testing.T) {
	host := &grokHost{
		mode: "default", status: "idle", acpGeneration: 2,
		done: make(chan struct{}),
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "busy", fromPush: true, generation: 1,
	}); err != nil || host.currentStatus() != "idle" {
		t.Fatalf("stale generation changed status to %q: %v", host.currentStatus(), err)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "busy", fromPush: true, generation: 2,
	}); err != nil || host.currentStatus() != "busy" {
		t.Fatalf("current push status = %q, %v", host.currentStatus(), err)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "idle", generation: 2,
	}); err != nil || host.currentStatus() != "busy" {
		t.Fatalf("immediately stale list overrode push: status %q, %v", host.currentStatus(), err)
	}
	host.modeMu.Lock()
	host.lastRosterPushAt = time.Now().Add(-2 * grokRosterPushGrace)
	host.modeMu.Unlock()
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "idle", generation: 2,
	}); err != nil || host.currentStatus() != "idle" {
		t.Fatalf("reconciled list status = %q, %v", host.currentStatus(), err)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "busy", generation: 2,
	}); err != nil || host.currentStatus() != "busy" {
		t.Fatalf("next working list status = %q, %v", host.currentStatus(), err)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "idle", fromPush: true, generation: 2,
	}); err != nil || host.currentStatus() != "idle" {
		t.Fatalf("idle push status = %q, %v", host.currentStatus(), err)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "busy", generation: 2,
	}); err != nil || host.currentStatus() != "idle" {
		t.Fatalf("immediately stale working list overrode idle push: status %q, %v", host.currentStatus(), err)
	}
}

func TestGrokFailedPushPublicationDoesNotAdvanceReconciliationWindow(t *testing.T) {
	host := &grokHost{
		mode: "default", status: "idle", acpGeneration: 1,
		peer:               &daemon{permissionMode: "default", status: "idle"},
		publishRosterState: func(*daemon) error { return errors.New("injected publication failure") },
		done:               make(chan struct{}),
	}
	err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "busy", fromPush: true, generation: 1,
	})
	if err == nil || host.currentStatus() != "idle" {
		t.Fatalf("failed push publication = status %q, err %v", host.currentStatus(), err)
	}
	host.modeMu.RLock()
	pushAt := host.lastRosterPushAt
	host.modeMu.RUnlock()
	if !pushAt.IsZero() {
		t.Fatalf("failed push advanced reconciliation window to %s", pushAt)
	}
}

func TestGrokReconnectClearsPushGraceBeforeNewRosterSnapshot(t *testing.T) {
	host := &grokHost{
		mode: "default", status: "busy", acpGeneration: 1, rosterValid: true,
		lastRosterPushAt: time.Now(), done: make(chan struct{}),
	}
	if generation := host.nextACPGeneration(); generation != 2 {
		t.Fatalf("next ACP generation = %d", generation)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "idle", generation: 1, fromPush: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.applyRosterState(grokRosterState{
		permissionMode: "default", status: "busy", generation: 2,
	}); err != nil || host.currentStatus() != "busy" {
		t.Fatalf("new-generation roster state = %q, %v", host.currentStatus(), err)
	}
	host.modeMu.RLock()
	valid := host.rosterValid
	pushAt := host.lastRosterPushAt
	host.modeMu.RUnlock()
	if !valid || !pushAt.IsZero() {
		t.Fatalf("new generation authority = valid %v pushAt %s", valid, pushAt)
	}
}

func TestGrokReconnectSerializesWithPeerPublication(t *testing.T) {
	host := &grokHost{
		mode: "default", status: "idle", acpGeneration: 1, rosterValid: true,
		done: make(chan struct{}),
	}
	host.peerMu.Lock()
	next := make(chan uint64, 1)
	go func() { next <- host.nextACPGeneration() }()
	select {
	case generation := <-next:
		host.peerMu.Unlock()
		t.Fatalf("generation %d advanced through the publication lock", generation)
	case <-time.After(50 * time.Millisecond):
	}
	host.peer = &daemon{}
	host.peerMu.Unlock()
	select {
	case generation := <-next:
		if generation != 2 {
			t.Fatalf("next generation = %d", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not resume after publication lock released")
	}
	host.modeMu.RLock()
	valid := host.rosterValid
	host.modeMu.RUnlock()
	if valid {
		t.Fatal("reconnect left the previous generation authoritative")
	}
}

func TestGrokReconnectInvalidatesRosterBeforeReplacementProcessStarts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	entered := make(chan struct{})
	release := make(chan struct{})
	config := grokHostConfig{
		GrokBin: os.Args[0], SessionID: "session-reconnect-before-spawn", Cwd: root,
		OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid()),
		LaunchToken: strings.Repeat("r", 32), RuntimeDir: filepath.Join(root, "run"),
		PermissionMode: "default",
	}
	config.command = func(args ...string) *exec.Cmd {
		close(entered)
		<-release
		return exec.Command(os.Args[0], append([]string{"-test.run=^TestGrokFakeProcess$", "--"}, args...)...)
	}
	host, err := newGrokHost(config)
	if err != nil {
		t.Fatal(err)
	}
	host.modeMu.Lock()
	host.acpGeneration = 1
	host.rosterValid = true
	host.modeMu.Unlock()
	host.record = grokLaunchRecord{SessionID: config.SessionID}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- host.ensureACPConnectedLocked(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("replacement process construction did not start")
	}
	if err := host.ensurePeerPublished(); err == nil || !strings.Contains(err.Error(), "no authoritative live roster state") {
		t.Fatalf("publication during replacement construction = %v", err)
	}
	cancel()
	close(release)
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement attempt did not finish")
	}
	host.acpMu.Lock()
	host.closeACPLocked()
	host.acpMu.Unlock()
}

func TestGrokRemovalBetweenReadinessAndPublicationBlocksPublication(t *testing.T) {
	host := &grokHost{
		mode: "default", status: "idle", acpGeneration: 1, rosterValid: true,
		done: make(chan struct{}),
	}
	if err := host.applyRosterState(grokRosterState{generation: 1, authorityLost: true}); !errors.Is(err, errGrokRosterAuthorityLost) {
		t.Fatalf("authority loss error = %v", err)
	}
	select {
	case <-host.done:
		t.Fatal("pre-publication authority loss stopped the host instead of allowing readiness retry")
	default:
	}
	if err := host.ensurePeerPublished(); err == nil || !strings.Contains(err.Error(), "no authoritative live roster state") {
		t.Fatalf("publication after authority loss = %v", err)
	}
}

func TestGrokOldGenerationRemovalCannotStopReconnectedPeer(t *testing.T) {
	host := &grokHost{
		mode: "default", status: "idle", acpGeneration: 2, rosterValid: true,
		peer: &daemon{}, done: make(chan struct{}),
	}
	host.stopForRosterAuthorityLoss(1)
	select {
	case <-host.done:
		t.Fatal("old observer generation stopped the reconnected Grok peer")
	default:
	}
	host.modeMu.RLock()
	valid := host.rosterValid
	host.modeMu.RUnlock()
	if !valid {
		t.Fatal("old observer generation cleared current roster authority")
	}
}

func TestGrokPublishedPeerStopsWhenRosterAuthorityIsRemoved(t *testing.T) {
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-roster-removed")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)
	if err := host.applyRosterState(grokRosterState{
		generation: host.currentACPGeneration(), authorityLost: true, fromPush: true,
	}); !errors.Is(err, errGrokRosterAuthorityLost) {
		t.Fatalf("authority loss error = %v", err)
	}
	select {
	case <-host.done:
	case <-time.After(time.Second):
		t.Fatal("published Grok peer did not stop after authoritative removal")
	}
}

func TestGrokHostPermissionPublishFailureRemainsDirtyAndRetries(t *testing.T) {
	yolo := filepath.Join(t.TempDir(), "yolo")
	if err := os.WriteFile(yolo, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	activity := filepath.Join(t.TempDir(), "activity")
	if err := os.WriteFile(activity, []byte("idle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_FAKE_YOLO_FILE", yolo)
	t.Setenv("GROK_FAKE_ACTIVITY_FILE", activity)
	host, cancel, result, _ := startTestGrokHost(t, os.Getpid(), readProcStart(os.Getpid()), "session-mode-publish-retry")
	defer stopTestGrokHost(t, host, cancel, result)
	waitGrokHostReady(t, host)

	var reject atomic.Bool
	reject.Store(true)
	host.peerMu.Lock()
	host.publishRosterState = func(peer *daemon) error {
		if reject.Load() {
			return errors.New("injected registry publication failure")
		}
		return peer.writeRecordsLocked()
	}
	host.peerMu.Unlock()
	if err := os.WriteFile(yolo, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activity, []byte("working\n"), 0o600); err != nil {
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
	if status := host.currentStatus(); status != "idle" {
		t.Fatalf("failed publication committed host status %q", status)
	}
	if record := readGrokLaunchRecord(grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID)); record == nil || record.PermissionMode != "default" {
		t.Fatalf("failed publication committed launch record %#v", record)
	}

	reject.Store(false)
	status, err = requestControl(host.paths.ControlSocket, map[string]any{
		"action": "status", "sessionId": host.config.SessionID, "launchToken": host.config.LaunchToken,
	}, time.Second)
	if err != nil || stringValue(status["permissionMode"]) != "bypassPermissions" || host.currentStatus() != "busy" {
		t.Fatalf("retried permission publication status = %#v, %v", status, err)
	}
	record := readGrokLaunchRecord(grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID))
	state := readJSONMap(filepath.Join(resolveNativePaths().dataRoot, "sessions", sessionKey(host.config.SessionID), "state.json"))
	registry := readJSONMap(stringValue(state["registryFile"]))
	if record == nil || record.PermissionMode != "bypassPermissions" ||
		stringValue(state["permissionMode"]) != "bypassPermissions" ||
		stringValue(registry["permissionMode"]) != "bypassPermissions" ||
		stringValue(state["status"]) != "busy" || stringValue(registry["status"]) != "busy" {
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

func TestActiveGrokLaunchSessionsIncludesLaneProcessSessionAndMalformedLaneState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	sessionID := randomID()
	worker := exec.Command("sleep", "30")
	worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	})
	processSessionID, err := grokProcessSessionID(worker.Process.Pid)
	if err != nil {
		t.Skipf("process-session inventory is unsupported: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !grokProcessSessionHasMembers(processSessionID, 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !grokProcessSessionHasMembers(processSessionID, 0) {
		t.Fatal("dedicated Grok lane process-session did not become observable")
	}
	state := grokLaneState{
		Type: "grok-peer-lane", Name: "install-lane", SessionID: sessionID, Status: "archived",
		WorkerSessionID: processSessionID, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeGrokLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	live, err := activeGrokLaunchSessions(paths)
	if err != nil || !reflect.DeepEqual(live, []string{sessionID}) {
		t.Fatalf("Grok lane process-session inventory = %v, %v", live, err)
	}
	if err := os.WriteFile(grokLaneStatePath(paths, sessionID), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := activeGrokLaunchSessions(paths); err == nil || !strings.Contains(err.Error(), "malformed Grok lane") {
		t.Fatalf("malformed Grok lane inventory error = %v", err)
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
	launchRecord := grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID)
	host.peerMu.Lock()
	peerStateDir := filepath.Dir(host.peer.stateFile)
	peerRegistry := host.peer.registryFile
	host.peerMu.Unlock()
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
	for _, path := range []string{launchRecord, host.paths.LeaderSocket, filepath.Join(host.paths.LaunchDir, "leader.lock"), host.paths.LaunchDir, peerStateDir, peerRegistry} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("Grok launch artifact survived cleanup at %s: %v", path, err)
		}
	}
	if live := liveGrokLaunchForSession(resolveNativePaths(), host.config.SessionID); live != nil {
		t.Fatalf("dead launch remains attested: %#v", live)
	}
}

func TestGrokHostRemovesDurableOwnershipBeforeACPShutdownCanBlock(t *testing.T) {
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Process.Kill()
		_ = owner.Wait()
	}()
	ownerStart := readProcStart(owner.Process.Pid)
	if ownerStart == "" {
		t.Fatal("owner process has no start token")
	}
	host, cancel, result, _ := startTestGrokHost(t, owner.Process.Pid, ownerStart, "session-cleanup-order")
	waitGrokHostReady(t, host)

	launchRecord := grokLaunchRecordPath(resolveNativePaths(), host.config.SessionID)
	host.peerMu.Lock()
	peerStateDir := filepath.Dir(host.peer.stateFile)
	peerRegistry := host.peer.registryFile
	host.peerMu.Unlock()
	host.acpMu.Lock()
	unlocked := false
	finished := false
	defer func() {
		if !unlocked {
			host.acpMu.Unlock()
		}
		cancel()
		if !finished {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Errorf("Grok host did not finish during test cleanup")
			}
		}
	}()
	cancel()

	paths := []string{
		launchRecord,
		host.paths.ControlSocket,
		host.paths.LeaderSocket,
		filepath.Join(host.paths.LaunchDir, "leader.lock"),
		host.paths.LaunchDir,
		peerStateDir,
		peerRegistry,
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		allAbsent := true
		for _, path := range paths {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				allAbsent = false
				break
			}
		}
		if allAbsent {
			break
		}
		if time.Now().After(deadline) {
			for _, path := range paths {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("Grok launch artifact remained before ACP shutdown at %s: %v", path, err)
				}
			}
			t.Fatal("Grok durable ownership was not removed before the ACP shutdown lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-result:
		t.Fatalf("Grok cleanup completed while ACP shutdown was blocked: %v", err)
	default:
	}

	host.acpMu.Unlock()
	unlocked = true
	select {
	case err := <-result:
		finished = true
		if err != nil {
			t.Fatalf("Grok host exit after releasing ACP shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Grok cleanup did not finish after releasing ACP shutdown")
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
	if err := socketpath.Validate(paths.ControlSocket); err != nil {
		t.Fatalf("Grok control socket exceeds platform budget: %#v: %v", paths, err)
	}
	if err := socketpath.Validate(paths.LeaderSocket); err != nil {
		t.Fatalf("Grok leader socket exceeds platform budget: %#v: %v", paths, err)
	}
	if strings.Contains(paths.ControlSocket, token) || strings.Contains(paths.LeaderSocket, token) {
		t.Fatal("raw launch token leaked into a socket path")
	}
}

func startTestGrokHost(t *testing.T, ownerPID int, ownerStart, sessionID string) (*grokHost, context.CancelFunc, <-chan error, string) {
	t.Helper()
	root := t.TempDir()
	// These tests exercise the private Grok host directly. A developer running
	// them from an attached peer may carry the production daemon runtime in the
	// environment; inheriting it would switch publication to the daemon adapter
	// without the prepared attachment that only the launcher integration owns.
	t.Setenv("AGENT_SESSIONS_AGENT_RUNTIME_DIR", "")
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
		LaunchToken: grokTokenHash(root)[:32], RuntimeDir: filepath.Join(root, "run"),
		Name: "grok-test", PermissionMode: "default",
	}
	config.NameSpecified = os.Getenv("GROK_FAKE_NAME_SPECIFIED") == "1"
	if selectedID := strings.TrimSpace(os.Getenv("GROK_FAKE_GENERATED_SESSION_ID")); selectedID != "" && selectedID != sessionID {
		config.LateBoundResume = true
		config.resolvePreferences = func(request federator.ResolvePreferencesRequest) (federator.ResolvedPreferences, error) {
			return federator.ResolvedPreferences{Preference: federation.SessionPreferences{
				SessionID: request.SessionID, Product: "grok", Kind: federator.SessionKindInteractive,
				AlwaysApprove: os.Getenv("GROK_FAKE_YOLO") == "1",
			}}, nil
		}
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
		identity := host.identitySnapshot()
		response, err := requestControl(host.paths.ControlSocket, map[string]any{
			"action": "status", "sessionId": identity.sessionID, "launchToken": host.config.LaunchToken,
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
