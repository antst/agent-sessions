package grok

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

const testSessionID = "01a075aa-7c7e-7f21-87dd-28c7b43f5bbc"

func TestMain(m *testing.M) {
	if os.Getenv("GROK_TEST_CHILD") != "" {
		fakeGrok()
		os.Exit(0)
	}
	_ = os.Setenv("GROK_TEST_CHILD", "1")
	command = func(_ string, arguments ...string) *exec.Cmd {
		args := append([]string{"-test.run=^$", "--"}, arguments...)
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), "GROK_TEST_CHILD=1")
		return cmd
	}
	os.Exit(m.Run())
}

func fakeGrok() {
	index := slices.Index(os.Args, "--")
	arguments := os.Args[index+1:]
	record("START", arguments)
	if slices.Contains(arguments, "leader") && !slices.Contains(arguments, "stdio") {
		path := option(arguments, "--leader-socket")
		_ = os.WriteFile(path, nil, 0o600)
		time.Sleep(24 * time.Hour)
	}
	title, cancelled, rosterCalls := "", make(chan struct{}, 1), 0
	titles := strings.Split(os.Getenv("GROK_TEST_TITLES"), ",")
	var write sync.Mutex
	encoder := json.NewEncoder(os.Stdout)
	reply := func(value any) { write.Lock(); _ = encoder.Encode(value); write.Unlock() }
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request map[string]any
		_ = json.Unmarshal(scanner.Bytes(), &request)
		record("FRAME", request)
		method, id := request["method"], request["id"]
		params, _ := request["params"].(map[string]any)
		switch method {
		case "initialize":
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": 1, "authMethods": []map[string]string{{"id": "cached_token"}}}})
		case "authenticate":
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "session/new", "session/load":
			session := testSessionID
			if loaded, ok := params["sessionId"].(string); ok {
				session = loaded
			}
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"sessionId": session, "models": map[string]any{}}})
		case "_x.ai/session/rename":
			title, _ = params["title"].(string)
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"success": true}})
		case "_x.ai/sessions/list":
			rosterCalls++
			if rosterCalls > 1 && rosterCalls-2 < len(titles) && titles[rosterCalls-2] != "" {
				title = titles[rosterCalls-2]
			}
			session := first(os.Getenv("GROK_TEST_SESSION_ID"), testSessionID)
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"result": map[string]any{"sessions": []map[string]any{{"sessionId": session, "title": title, "cwd": os.TempDir(), "activity": "working", "resident": true, "yolo": true}}}}})
		case "_x.ai/interject":
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"result": map[string]string{"status": "queued"}}})
			reply(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/session/interjection", "params": params})
		case "session/prompt":
			go func(id any, params map[string]any) {
				session, _ := params["sessionId"].(string)
				prompt := fmt.Sprint(params["prompt"])
				stop := "end_turn"
				if strings.Contains(prompt, "hold") {
					select {
					case <-cancelled:
						stop = "cancelled"
					case <-released():
					}
				}
				answer := "answer"
				if size, _ := strconv.Atoi(os.Getenv("GROK_TEST_OUTPUT_SIZE")); size > 0 {
					answer = strings.Repeat("x", size)
				}
				reply(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": answer}}}})
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]string{"stopReason": stop}})
				if os.Getenv("GROK_TEST_EXIT_AFTER_PROMPT") != "" {
					os.Exit(0)
				}
			}(id, params)
		case "session/cancel":
			select {
			case cancelled <- struct{}{}:
			default:
			}
		case "session/close":
			if os.Getenv("GROK_TEST_CLOSE_ERROR") != "" {
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "close failed"}})
			} else {
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"_meta": map[string]string{"x.ai/closeOutcome": "closed"}}})
			}
		}
	}
}

func released() <-chan time.Time {
	ready := make(chan time.Time, 1)
	go func() {
		for {
			if _, err := os.Stat(os.Getenv("GROK_TEST_RELEASE")); err == nil {
				ready <- time.Now()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return ready
}

func record(kind string, value any) {
	path := os.Getenv("GROK_TEST_RECORD")
	if path == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"kind": kind, "value": value})
	file, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if file != nil {
		_, _ = file.Write(append(body, '\n'))
		_ = file.Close()
	}
}

func TestFreshLaneNativeLifecycle(t *testing.T) {
	root, recordPath := t.TempDir(), filepath.Join(t.TempDir(), "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_RELEASE", filepath.Join(root, "release"))
	p := New(filepath.Join(root, "agentbus.sock"), "single-use-token")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	request := sessionkit.OpenRequest{Name: "parent/grok@local", Groups: []string{"group"}, Open: sessionkit.OpenOptions{Cwd: root, PermissionMode: "bypassPermissions", Model: "grok-4.6", ReasoningEffort: "low", Arguments: []string{"--disable-web-search"}}}
	opened, err := p.Open(context.Background(), request)
	must(t, err)
	check(t, opened.SessionID == testSessionID, "session id = %q", opened.SessionID)
	check(t, exists(filepath.Join(root, "locks", "grok", testSessionID)), "renamed lock absent")
	frames := records(t, recordPath)
	check(t, containsStart(frames, "--permission-mode", "bypassPermissions", "--reasoning-effort", "low", "-m", "grok-4.6", "--disable-web-search"), "typed argv not preserved")
	check(t, containsStart(frames, "--relay-on-demand") && !containsStart(frames, "--no-exit-on-disconnect"), "leader argv did not preserve relay-on-demand")
	check(t, countFrames(frames, "initialize") == 3, "authenticated startup hold absent: %d handshakes", countFrames(frames, "initialize"))
	open := findFrame(frames, "session/new")
	check(t, !strings.Contains(string(open), "--session-id") && strings.Contains(string(open), `"agent_sessions"`) && strings.Contains(string(open), `"AGENTBUS_LANE_SOCKET"`) && strings.Contains(string(open), `"yoloMode":true`), "fresh open = %s", open)

	result, err := p.Run(context.Background(), &sessionkit.Run{}, "plain")
	must(t, err)
	check(t, result == (sessionkit.TurnResult{Outcome: "completed", Result: "answer", NativeStopReason: "end_turn"}), "turn = %#v", result)
	p.mu.Lock()
	p.run = nil // Direct callback invocation has no kit-owned Done signal.
	p.mu.Unlock()
	idle, err := p.Deliver(context.Background(), delivery("idle"))
	must(t, err)
	check(t, idle.Disposition == "queued_for_next_turn", "idle = %#v", idle)

	done := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "hold"); done <- result }()
	waitFrame(t, recordPath, "session/prompt", 2)
	active, err := p.Deliver(context.Background(), delivery("active"))
	must(t, err)
	check(t, active.Disposition == "injected", "active = %#v", active)
	must(t, os.WriteFile(os.Getenv("GROK_TEST_RELEASE"), nil, 0o600))
	check(t, (<-done).Outcome == "completed", "held run failed")
	must(t, p.Close(context.Background()))
	check(t, !exists(filepath.Join(root, "lanes", p.key+".sock")), "lane socket remains")
}

func TestInterruptAndResume(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GROK_TEST_RECORD", filepath.Join(root, "record"))
	t.Setenv("GROK_TEST_RELEASE", filepath.Join(root, "never"))
	open := func(resume string) *Wrapper {
		p := New(filepath.Join(root, "agentbus.sock"), "token-"+resume)
		p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
		_, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "lane@local", ResumeSessionID: resume, Open: sessionkit.OpenOptions{Cwd: root}})
		must(t, err)
		return p
	}
	p := open("")
	done := make(chan sessionkit.TurnResult, 1)
	run := &sessionkit.Run{}
	go func() { result, _ := p.Run(context.Background(), run, "hold"); done <- result }()
	waitFrame(t, os.Getenv("GROK_TEST_RECORD"), "session/prompt", 1)
	must(t, p.Interrupt(context.Background(), run))
	check(t, (<-done).Outcome == "interrupted", "interrupt terminal changed")
	must(t, p.Close(context.Background()))
	p = open(testSessionID)
	frames := records(t, os.Getenv("GROK_TEST_RECORD"))
	check(t, len(findFrame(frames, "session/load")) > 0, "resume did not load")
	check(t, containsStart(frames, "--permission-mode", "default", "--allow", "MCPTool(agent_sessions__*)"), "default leader MCP allow absent")
	must(t, p.Close(context.Background()))
}

func TestArgumentsAndHello(t *testing.T) {
	hello, err := (&Wrapper{}).Hello(context.Background())
	must(t, err)
	check(t, hello.Product == Product && reflect.DeepEqual(hello.SupportedOpenFields, []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"}), "hello = %#v", hello)
	for _, test := range []struct{ argument, want string }{{"--model=x", "model"}, {"--resume=x", "session_id"}, {"--leader", "leader"}, {"text", "unsupported argument"}} {
		_, err := extraArguments([]string{test.argument})
		check(t, err != nil && strings.Contains(err.Error(), test.want), "%s error = %v", test.argument, err)
	}
}

func TestCloseReturnsNativeErrorAfterCleanup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GROK_TEST_CLOSE_ERROR", "1")
	p := New(filepath.Join(root, "agentbus.sock"), "close-error")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	_, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "lane@local", Open: sessionkit.OpenOptions{Cwd: root}})
	must(t, err)
	err = p.Close(context.Background())
	check(t, err != nil && strings.Contains(err.Error(), "close failed"), "close error = %v", err)
	check(t, !exists(filepath.Join(root, "lanes", p.key+".sock")), "endpoint survived failed native close")
}

func TestKitRunTokenCrossings(t *testing.T) {
	root, recordPath := t.TempDir(), filepath.Join(t.TempDir(), "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	p, connection, reader := startGrokWorker(t, root)
	p.mu.Lock()
	writeWorkerRequest(t, connection, 2, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "plain"})
	writeWorkerRequest(t, connection, 3, "turn.interrupt", map[string]string{"session_id": testSessionID + "@local"})
	writeWorkerRequest(t, connection, 4, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "second"})
	check(t, readWorkerResponse(t, reader, 4).Error != nil, "second run was not busy")
	p.mu.Unlock()
	check(t, readWorkerResponse(t, reader, 2).Result != nil, "interrupted terminal absent")
	check(t, readWorkerResponse(t, reader, 3).Result != nil, "interrupt ack absent")
	check(t, countFrames(records(t, recordPath), "session/prompt") == 0, "pre-write interrupt reached Grok")
	writeWorkerRequest(t, connection, 5, "session.close", map[string]string{"session_id": testSessionID + "@local"})
	check(t, readWorkerResponse(t, reader, 5).Result != nil, "close response absent")
}

func TestChildExitWaitsForRunDoneBeforeShutdown(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GROK_TEST_OUTPUT_SIZE", "300000")
	t.Setenv("GROK_TEST_EXIT_AFTER_PROMPT", "1")
	p, connection, reader := startGrokWorker(t, root)
	writeWorkerRequest(t, connection, 2, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "plain"})
	<-p.child.Done()
	response := readWorkerResponse(t, reader, 2)
	var terminal sessionkit.TurnResult
	must(t, json.Unmarshal(response.Result, &terminal))
	check(t, terminal.Outcome == "completed" && len(terminal.Result) == 262144, "terminal before EOF = %s/%d", terminal.Outcome, len(terminal.Result))
	_, err := reader.ReadBytes('\n')
	check(t, errors.Is(err, io.EOF), "worker did not shut down after terminal: %v", err)
}

type workerResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func startGrokWorker(t *testing.T, root string) (*Wrapper, net.Conn, *bufio.Reader) {
	socket := filepath.Join(root, "agentbus.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.TokenEnv, "worker-token")
	p := New(socket, "worker-token")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	worker := sessionkit.NewWorker(p)
	p.SetShutdown(worker.Shutdown)
	go func() { _ = worker.Serve(context.Background()) }()
	connection, err := listener.Accept()
	must(t, err)
	reader := bufio.NewReader(connection)
	readLine(t, reader)
	_, err = connection.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	must(t, err)
	writeWorkerRequest(t, connection, 1, "session.open", map[string]any{"name": "parent/grok@local", "groups": []string{"group"}, "open": map[string]any{"cwd": root}})
	opened := readWorkerResponse(t, reader, 1)
	check(t, opened.Result != nil, "open failed: %s", opened.Error)
	t.Cleanup(func() { _ = connection.Close(); <-worker.Closed(); _ = listener.Close() })
	return p, connection, reader
}

func writeWorkerRequest(t *testing.T, connection net.Conn, id int, method string, params any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	must(t, err)
	_, err = connection.Write(append(body, '\n'))
	must(t, err)
}

func readWorkerResponse(t *testing.T, reader *bufio.Reader, id int) workerResponse {
	for {
		var response workerResponse
		must(t, json.Unmarshal(readLine(t, reader), &response))
		if response.ID == id {
			return response
		}
	}
}

func readLine(t *testing.T, reader *bufio.Reader) []byte {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	must(t, err)
	return line
}

func delivery(body string) sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{MessageID: "message-1", From: sessionkit.DeliverySource{SessionID: "source@local", Name: "source@local", Product: "example", Groups: []string{"group"}}, Body: body}
}

func waitFrame(t *testing.T, path, method string, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		found := 0
		for _, raw := range records(t, path) {
			if strings.Contains(string(raw), `"method":"`+method+`"`) {
				found++
			}
		}
		if found >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not observe %s", method)
}

func records(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			result = append(result, json.RawMessage(line))
		}
	}
	return result
}

func findFrame(records []json.RawMessage, method string) json.RawMessage {
	for _, raw := range records {
		if strings.Contains(string(raw), `"method":"`+method+`"`) {
			return raw
		}
	}
	return nil
}

func countFrames(records []json.RawMessage, method string) int {
	count := 0
	for _, raw := range records {
		if strings.Contains(string(raw), `"method":"`+method+`"`) {
			count++
		}
	}
	return count
}

func containsStart(records []json.RawMessage, values ...string) bool {
	for _, raw := range records {
		if !strings.Contains(string(raw), `"kind":"START"`) {
			continue
		}
		body := string(raw)
		if slices.ContainsFunc(values, func(value string) bool { return !strings.Contains(body, value) }) {
			continue
		}
		return true
	}
	return false
}

func option(arguments []string, name string) string {
	for index, argument := range arguments {
		if argument == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func check(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

var _ = syscall.SIGTERM
