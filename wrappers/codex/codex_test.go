package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

type turnDone struct {
	result sessionkit.TurnResult
	err    error
}

func TestWrapperFreshOpenAndClose(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROCESS", "1")
	evidence := filepath.Join(t.TempDir(), "child.json")
	t.Setenv("CODEX_TEST_EVIDENCE", evidence)
	t.Setenv(host.TokenEnv, "must-not-reach-child")
	workspace := t.TempDir()
	t.Setenv("CODEX_TEST_CWD", workspace)
	original := laneCommand
	laneCommand = func(_ string, arguments ...string) *exec.Cmd {
		return exec.Command(os.Args[0], append([]string{"-test.run=TestCodexProcess", "--"}, arguments...)...)
	}
	t.Cleanup(func() { laneCommand = original })
	socket := filepath.Join(t.TempDir(), "agentbus.sock")
	p := New(socket, "provisional")
	p.backend = mcp.BackendFunc(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	result, err := p.Open(context.Background(), sessionkit.OpenRequest{
		Name: "parent/lane@host", Open: sessionkit.OpenOptions{Cwd: workspace, Model: "gpt-test", ReasoningEffort: "high", Arguments: []string{"--enable", "feature"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "thread-1" {
		t.Fatalf("session = %q", result.SessionID)
	}
	if err = p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Args, Env []string
		Calls     []appRequest
	}
	body, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(body, &observed) != nil {
		t.Fatalf("evidence = %s", body)
	}
	wantTail := []string{"--enable", "feature", "app-server", "--stdio"}
	if !slices.Equal(observed.Args[len(observed.Args)-len(wantTail):], wantTail) {
		t.Fatalf("args = %v", observed.Args)
	}
	if slices.Contains(observed.Env, host.TokenEnv) || !slices.Contains(observed.Env, mcp.LaneSocketEnv) {
		t.Fatalf("env names = %v", observed.Env)
	}
	if len(observed.Calls) < 7 || observed.Calls[2].Method != "thread/start" || !strings.Contains(string(observed.Calls[2].Params), `"agent_sessions"`) || !strings.Contains(string(observed.Calls[2].Params), `"code_mode_host":false`) {
		t.Fatalf("calls = %#v", observed.Calls)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(socket), "locks", "codex", "thread-1")); err != nil {
		t.Fatalf("renamed lock: %v", err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(socket), "lanes", "provisional.sock")); !os.IsNotExist(err) {
		t.Fatalf("lane socket remains: %v", err)
	}
}

func TestCodexProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_PROCESS") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	names := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		names = append(names, name)
	}
	calls := []appRequest{}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var request appRequest
		if decoder.Decode(&request) != nil {
			body, _ := json.Marshal(map[string]any{"Args": os.Args[separator+1:], "Env": names, "Calls": calls})
			_ = os.WriteFile(os.Getenv("CODEX_TEST_EVIDENCE"), body, 0o600)
			os.Exit(0)
		}
		calls = append(calls, request)
		result := any(map[string]any{})
		switch request.Method {
		case "thread/start":
			result = map[string]any{"thread": map[string]string{"id": "thread-1"}, "cwd": os.Getenv("CODEX_TEST_CWD"), "approvalPolicy": "never", "sandbox": map[string]string{"type": "readOnly"}}
		case "thread/resume":
			result = map[string]any{"thread": map[string]string{"id": "thread-1"}, "cwd": os.Getenv("CODEX_TEST_CWD"), "approvalPolicy": "never", "sandbox": map[string]string{"type": "readOnly"}}
		}
		if request.ID != 0 {
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
		}
	}
}

func TestLaneRunSteerAndTerminal(t *testing.T) {
	p, server := testLane(t)
	started := make(chan host.Turn, 1)
	go func() {
		turn, err := p.start(context.Background(), "original")
		if err != nil {
			t.Error(err)
		}
		started <- turn
	}()
	request := readAppRequest(t, server)
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
	select {
	case turn := <-started:
		t.Fatalf("turn published before turn/started: %v", turn)
	default:
	}
	writeRaw(t, server, `{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":1788680430,"completedAt":null,"durationMs":null}},"emittedAtMs":1788680430493}`)
	native := <-started
	injected := make(chan error, 1)
	go func() {
		ok, err := p.inject(context.Background(), "delivery")
		if !ok && err == nil {
			err = context.Canceled
		}
		injected <- err
	}()
	request = readAppRequest(t, server)
	if request.Method != "turn/steer" || !strings.Contains(string(request.Params), `"expectedTurnId":"turn-1"`) {
		t.Fatalf("steer = %#v", request)
	}
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]string{"turnId": "turn-1"}})
	if err := <-injected; err != nil {
		t.Fatal(err)
	}
	writeRaw(t, server, `{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}},"emittedAtMs":1788679081756}`)
	waited := make(chan turnDone, 1)
	go func() { result, waitErr := native.Wait(context.Background()); waited <- turnDone{result, waitErr} }()
	request = readAppRequest(t, server)
	if request.Method != "thread/turns/list" || !strings.Contains(string(request.Params), `"itemsView":"full"`) {
		t.Fatalf("read = %#v", request)
	}
	completed := int64(1788679081)
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]any{"data": []any{map[string]any{"id": "turn-1", "items": []any{map[string]string{"type": "agentMessage", "text": "STEER_PROBE_OK", "phase": "final_answer"}}, "status": "completed", "completedAt": completed}}}})
	done := <-waited
	result, err := done.result, done.err
	if err != nil || result.Outcome != "completed" || result.Result != "STEER_PROBE_OK" || result.NativeStopReason != "completed" {
		t.Fatalf("terminal = %#v, %v", result, err)
	}
}

func TestLaneInterruptAfterNativeStart(t *testing.T) {
	p, server := testLane(t)
	started := make(chan host.Turn, 1)
	go func() { turn, _ := p.start(context.Background(), "block"); started <- turn }()
	request := readAppRequest(t, server)
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-2"}}})
	writeRaw(t, server, `{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-2","status":"inProgress"}}}`)
	native := <-started
	interrupted := make(chan error, 1)
	go func() { interrupted <- native.Interrupt(context.Background()) }()
	request = readAppRequest(t, server)
	if request.Method != "turn/interrupt" {
		t.Fatalf("interrupt = %#v", request)
	}
	writeRaw(t, server, `{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-2","items":[],"itemsView":"notLoaded","status":"interrupted","error":null,"startedAt":1788680430,"completedAt":1788680430,"durationMs":22}},"emittedAtMs":1788680430509}`)
	waited := make(chan turnDone, 1)
	go func() { result, waitErr := native.Wait(context.Background()); waited <- turnDone{result, waitErr} }()
	read := readAppRequest(t, server)
	if read.Method != "thread/turns/list" {
		t.Fatalf("read = %#v", read)
	}
	completed := int64(1788680430)
	writeApp(t, server, map[string]any{"id": read.ID, "result": map[string]any{"data": []any{map[string]any{"id": "turn-2", "status": "interrupted", "completedAt": completed}}}})
	done := <-waited
	result, err := done.result, done.err
	if err != nil || result.Outcome != "interrupted" {
		t.Fatalf("terminal = %#v, %v", result, err)
	}
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]any{}})
	if err := <-interrupted; err != nil {
		t.Fatal(err)
	}
}

func TestLaneTerminalBeforeSteerResponseQueuesDelivery(t *testing.T) {
	p, server := testLane(t)
	started := make(chan host.Turn, 1)
	go func() { turn, _ := p.start(context.Background(), "original"); started <- turn }()
	request := readAppRequest(t, server)
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-3"}}})
	writeRaw(t, server, `{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-3","status":"inProgress"}}}`)
	native := <-started
	injected := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		ok, err := p.inject(context.Background(), "late")
		injected <- struct {
			ok  bool
			err error
		}{ok, err}
	}()
	request = readAppRequest(t, server)
	writeRaw(t, server, `{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-3","status":"completed"}}}`)
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]string{"turnId": "turn-3"}})
	if got := <-injected; got.ok || got.err != nil {
		t.Fatalf("inject = %v, %v", got.ok, got.err)
	}
	waited := make(chan error, 1)
	go func() { _, err := native.Wait(context.Background()); waited <- err }()
	request = readAppRequest(t, server)
	completed := int64(1)
	writeApp(t, server, map[string]any{"id": request.ID, "result": map[string]any{"data": []any{map[string]any{"id": "turn-3", "status": "completed", "completedAt": completed, "items": []any{map[string]string{"type": "agentMessage", "phase": "final_answer", "text": "done"}}}}}})
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
}

func TestCompletedTurnRequiresFinalAnswer(t *testing.T) {
	completed := int64(1)
	if _, err := terminal(nativeTurn{ID: "turn-4", Status: "completed", CompletedAt: &completed}); err == nil || !strings.Contains(err.Error(), "no final answer") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenEffectiveSettings(t *testing.T) {
	p := &Wrapper{approval: "never", sandbox: "danger-full-access"}
	reply := threadReply{Cwd: "/work", ApprovalPolicy: "never"}
	reply.Sandbox.Type = "dangerFullAccess"
	if err := p.checkEffective("thread/resume", "/work", reply); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*threadReply){
		func(r *threadReply) { r.Cwd = "/other" },
		func(r *threadReply) { r.ApprovalPolicy = "on-request" },
		func(r *threadReply) { r.Sandbox.Type = "readOnly" },
	} {
		changed := reply
		mutate(&changed)
		if err := p.checkEffective("thread/resume", "/work", changed); err == nil {
			t.Fatalf("accepted %#v", changed)
		}
	}
}

type appRequest struct {
	ID     int
	Method string
	Params json.RawMessage
}

func testLane(t *testing.T) (*Wrapper, net.Conn) {
	client, server := net.Pipe()
	p := &Wrapper{id: "thread-1", approval: "never"}
	p.app = newAppClient(client, client, p.receive, p.fail)
	t.Cleanup(func() { _ = server.Close() })
	return p, server
}

func readAppRequest(t *testing.T, connection net.Conn) appRequest {
	t.Helper()
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var request appRequest
	if json.Unmarshal(line, &request) != nil {
		t.Fatalf("request = %s", line)
	}
	return request
}

func writeApp(t *testing.T, connection net.Conn, value any) {
	t.Helper()
	if json.NewEncoder(connection).Encode(value) != nil {
		t.Fatal("write response")
	}
}
func writeRaw(t *testing.T, connection net.Conn, value string) {
	t.Helper()
	if _, err := connection.Write([]byte(value + "\n")); err != nil {
		t.Fatal(err)
	}
}
