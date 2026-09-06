package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
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
			if rosterCalls > 1 && os.Getenv("GROK_TEST_RETITLE") != "" {
				title = os.Getenv("GROK_TEST_RETITLE")
			}
			session := first(os.Getenv("GROK_TEST_SESSION_ID"), testSessionID)
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"result": map[string]any{"sessions": []map[string]any{{"sessionId": session, "title": title, "cwd": os.TempDir(), "activity": "working", "resident": true}}}}})
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
				reply(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "answer"}}}})
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]string{"stopReason": stop}})
			}(id, params)
		case "session/cancel":
			select {
			case cancelled <- struct{}{}:
			default:
			}
		case "session/close":
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"_meta": map[string]string{"x.ai/closeOutcome": "closed"}}})
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
	request := sessionkit.OpenRequest{Name: "parent/grok@local", Groups: []string{"group"}, Open: sessionkit.OpenOptions{Cwd: root, PermissionMode: "dontAsk", Model: "grok-4.6", ReasoningEffort: "low", Arguments: []string{"--disable-web-search"}}}
	opened, err := p.Open(context.Background(), request)
	must(t, err)
	check(t, opened.SessionID == testSessionID, "session id = %q", opened.SessionID)
	check(t, exists(filepath.Join(root, "locks", "grok", testSessionID)), "renamed lock absent")
	frames := records(t, recordPath)
	check(t, containsStart(frames, "--permission-mode", "dontAsk", "--reasoning-effort", "low", "-m", "grok-4.6", "--disable-web-search"), "typed argv not preserved")
	open := findFrame(frames, "session/new")
	check(t, !strings.Contains(string(open), "--session-id") && strings.Contains(string(open), `"agent_sessions"`) && strings.Contains(string(open), `"AGENTBUS_LANE_SOCKET"`), "fresh open = %s", open)

	result, err := p.Run(context.Background(), &sessionkit.Run{}, "plain")
	must(t, err)
	check(t, result == (sessionkit.TurnResult{Outcome: "completed", Result: "answer", NativeStopReason: "end_turn"}), "turn = %#v", result)
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
	check(t, len(findFrame(records(t, os.Getenv("GROK_TEST_RECORD")), "session/load")) > 0, "resume did not load")
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
