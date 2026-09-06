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
	cwd, _ := os.Getwd()
	record("START", map[string]any{"pid": os.Getpid(), "arguments": arguments, "cwd": cwd, "laneSocket": os.Getenv("AGENTBUS_LANE_SOCKET"), "environment": map[string]string{host.SocketEnv: os.Getenv(host.SocketEnv), host.GroupsEnv: os.Getenv(host.GroupsEnv)}})
	if path := os.Getenv("GROK_TEST_INTERACTIVE_STARTED"); path != "" && slices.Contains(arguments, "--leader") && !slices.Contains(arguments, "stdio") {
		publishTestFile(path, []byte("started"))
	}
	if slices.Contains(arguments, "leader") && !slices.Contains(arguments, "stdio") {
		if pidPath := os.Getenv("GROK_TEST_LEADER_PID"); pidPath != "" {
			publishTestFile(pidPath, []byte(strconv.Itoa(os.Getpid())))
		}
		path := option(arguments, "--leader-socket")
		listener, err := net.Listen("unix", path)
		if err != nil {
			os.Exit(2)
		}
		defer listener.Close()
		time.Sleep(24 * time.Hour)
	}
	if slices.Contains(arguments, "--leader") && !slices.Contains(arguments, "stdio") {
		if path := os.Getenv("GROK_TEST_INTERACTIVE_PID"); path != "" {
			publishTestFile(path, []byte(strconv.Itoa(os.Getpid())))
		}
		if code, _ := strconv.Atoi(os.Getenv("GROK_TEST_INTERACTIVE_EXIT")); code != 0 {
			if path := os.Getenv("GROK_TEST_INTERACTIVE_EXIT_BARRIER"); path != "" {
				<-fileReady(path)
			}
			os.Exit(code)
		}
		time.Sleep(24 * time.Hour)
	}
	if path := os.Getenv("GROK_TEST_OBSERVER_PID"); path != "" && slices.Contains(arguments, "--leader") && slices.Contains(arguments, "stdio") {
		publishTestFile(path, []byte(strconv.Itoa(os.Getpid())))
	}
	if path := os.Getenv("GROK_TEST_DESCENDANT_PID"); path != "" {
		child := exec.Command("sleep", "3600")
		if child.Start() == nil {
			publishTestFile(path, []byte(strconv.Itoa(child.Process.Pid)))
		}
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
		request["_testPID"] = os.Getpid()
		record("FRAME", request)
		method, id := request["method"], request["id"]
		params, _ := request["params"].(map[string]any)
		switch method {
		case "initialize":
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": 1, "authMethods": []map[string]string{{"id": "cached_token"}}}})
		case "authenticate":
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			if path := os.Getenv("GROK_TEST_HOLD_EXIT"); path != "" && slices.Contains(arguments, "stdio") {
				<-fileReady(path)
				return
			}
		case "session/new", "session/load":
			session := testSessionID
			if loaded, ok := params["sessionId"].(string); ok {
				session = loaded
			}
			if method == "session/load" {
				session = first(os.Getenv("GROK_TEST_LOAD_ID"), session)
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"models": map[string]any{}, "_meta": map[string]any{"sessionId": session, "x.ai/sessionDetail": map[string]string{"sessionId": session}}}})
			} else {
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"sessionId": session, "models": map[string]any{}}})
			}
		case "_x.ai/session/rename":
			title, _ = params["title"].(string)
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"success": true}})
		case "_x.ai/sessions/list":
			rosterCalls++
			if os.Getenv("GROK_TEST_ROSTER_DELAY") != "" && rosterCalls == 1 {
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"result": map[string]any{"sessions": []any{}}}})
				reply(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/sessions/changed", "params": map[string]any{}})
				continue
			}
			titleIndex := rosterCalls - 1
			if os.Getenv("GROK_TEST_ROSTER_DELAY") != "" {
				titleIndex--
			}
			if titleIndex >= 0 && titleIndex < len(titles) && titles[titleIndex] != "" {
				title = titles[titleIndex]
			}
			session := first(os.Getenv("GROK_TEST_SESSION_ID"), testSessionID)
			if ids := strings.Split(os.Getenv("GROK_TEST_SESSION_IDS"), ","); len(ids) > 0 && ids[0] != "" {
				session = ids[min(rosterCalls-1, len(ids)-1)]
			}
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"result": map[string]any{"sessions": []map[string]any{{"sessionId": session, "title": title, "cwd": first(os.Getenv("GROK_TEST_CWD"), os.TempDir()), "activity": first(os.Getenv("GROK_TEST_ACTIVITY"), "working"), "resident": true, "yolo": true}}}}})
			if path := os.Getenv("GROK_TEST_ROSTER_CHANGE"); path != "" && rosterCalls == 1 {
				go func() {
					<-fileReady(path)
					reply(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/sessions/changed", "params": map[string]any{}})
				}()
			}
			if path := os.Getenv("GROK_TEST_OBSERVER_EXIT"); path != "" && slices.Contains(arguments, "stdio") {
				<-fileReady(path)
				return
			}
		case "_x.ai/interject":
			if path := os.Getenv("GROK_TEST_INTERJECT_BLOCK"); path != "" {
				<-fileReady(path)
			}
			reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"result": map[string]string{"status": "queued"}}})
			reply(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/session/interjection", "params": params})
		case "session/prompt":
			go func(id any, params map[string]any) {
				session, _ := params["sessionId"].(string)
				promptID := fmt.Sprintf("prompt-%v", id)
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
				if os.Getenv("GROK_TEST_FOREIGN_CHUNK") != "" {
					reply(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "foreign"}}, "_meta": map[string]string{"promptId": "foreign-prompt"}}})
				}
				reply(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": answer}}, "_meta": map[string]string{"promptId": promptID}}})
				reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": stop, "_meta": map[string]string{"promptId": promptID}}})
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
	return fileReady(os.Getenv("GROK_TEST_RELEASE"))
}

func fileReady(path string) <-chan time.Time {
	ready := make(chan time.Time, 1)
	go func() {
		for {
			if _, err := os.Stat(path); err == nil {
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

func publishTestFile(path string, body []byte) {
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(temporary, body, 0o600) == nil {
		_ = os.Rename(temporary, path)
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
	check(t, allStartsContain(frames, "--no-auto-update"), "an ACP client omitted --no-auto-update")
	check(t, countFrames(frames, "initialize") == 3, "authenticated startup hold absent: %d handshakes", countFrames(frames, "initialize"))
	open := findFrame(frames, "session/new")
	check(t, !strings.Contains(string(open), "--session-id") && strings.Contains(string(open), `"agent_sessions"`) && strings.Contains(string(open), `"AGENTBUS_LANE_SOCKET"`) && strings.Contains(string(open), `"yoloMode":true`), "fresh open = %s", open)
	idle, err := p.Deliver(context.Background(), delivery("idle"))
	must(t, err)
	check(t, idle.Disposition == "queued_for_next_turn", "idle = %#v", idle)
	check(t, countFrames(records(t, recordPath), "_x.ai/interject") == 0, "idle delivery started native work")
	must(t, p.Close(context.Background()))
	check(t, !exists(filepath.Join(root, "lanes", p.key+".sock")), "lane socket remains")
}

func TestInterruptAndResume(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GROK_TEST_RECORD", filepath.Join(root, "record"))
	t.Setenv("GROK_TEST_RELEASE", filepath.Join(root, "never"))
	_, connection, reader := startGrokWorker(t, root)
	writeWorkerRequest(t, connection, 2, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "hold"})
	waitFrame(t, os.Getenv("GROK_TEST_RECORD"), "session/prompt", 1)
	writeWorkerRequest(t, connection, 3, "turn.interrupt", map[string]string{"session_id": testSessionID + "@local"})
	var terminal sessionkit.TurnResult
	must(t, json.Unmarshal(readWorkerResponse(t, reader, 2).Result, &terminal))
	check(t, terminal.Outcome == "interrupted", "interrupt terminal changed")
	check(t, readWorkerResponse(t, reader, 3).Result != nil, "interrupt ack absent")
	writeWorkerRequest(t, connection, 4, "session.close", map[string]string{"session_id": testSessionID + "@local"})
	check(t, readWorkerResponse(t, reader, 4).Result != nil, "close response absent")
	p := New(filepath.Join(root, "agentbus.sock"), "resume-token")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	_, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "lane@local", ResumeSessionID: testSessionID, Open: sessionkit.OpenOptions{Cwd: root}})
	must(t, err)
	frames := records(t, os.Getenv("GROK_TEST_RECORD"))
	load := findFrame(frames, "session/load")
	check(t, strings.Contains(string(load), `"sessionId":"`+testSessionID+`"`), "resume load = %s", load)
	check(t, !containsStart(frames, "--resume", testSessionID), "resume was selected in both argv and session/load")
	check(t, containsStart(frames, "--permission-mode", "default", "--allow", "MCPTool(agent_sessions__*)"), "default leader MCP allow absent")
	must(t, p.Close(context.Background()))
}

func TestResumeIdentityFailureRepliesBeforeCleanup(t *testing.T) {
	root := shortRoot(t)
	t.Setenv("GROK_TEST_RECORD", filepath.Join(root, "record"))
	t.Setenv("GROK_TEST_LOAD_ID", "different-product-id")
	socket := filepath.Join(root, "agentbus.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.TokenEnv, "resume-error-token")
	p := New(socket, "resume-error-token")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	worker := sessionkit.NewWorker(p)
	go func() { _ = worker.Serve(context.Background()) }()
	connection, err := listener.Accept()
	must(t, err)
	reader := &workerReader{reader: bufio.NewReader(connection), pending: map[int]workerResponse{}}
	readLine(t, reader.reader)
	_, err = connection.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	must(t, err)
	writeWorkerRequest(t, connection, 1, "session.open", map[string]any{"name": "lane@local", "groups": []string{"group"}, "resume_session_id": testSessionID, "open": map[string]any{"cwd": root}})
	response := readWorkerResponse(t, reader, 1)
	check(t, strings.Contains(string(response.Error), `"message":"spawn_failed"`) && strings.Contains(string(response.Error), `Grok returned session identity`), "open error = %s", response.Error)
	_ = connection.Close()
	<-worker.Closed()
	p.mu.Lock()
	primary, child, leader := p.primary, p.child, p.leader
	p.mu.Unlock()
	primary.close()
	_ = child.Close(context.Background(), func(context.Context) error { return nil })
	stopAux(leader)
	_ = listener.Close()
}

func TestOmittedCwdAndSocketReadiness(t *testing.T) {
	root, recordPath := t.TempDir(), filepath.Join(t.TempDir(), "record")
	regular := filepath.Join(root, "not-a-socket")
	must(t, os.WriteFile(regular, nil, 0o600))
	check(t, !grokSocketReady(regular), "regular file reported ready")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	p := New(filepath.Join(root, "agentbus.sock"), "cwd-token")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	_, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "lane@local"})
	must(t, err)
	want, err := os.Getwd()
	must(t, err)
	for _, frame := range records(t, recordPath) {
		if strings.Contains(string(frame), `"kind":"START"`) {
			check(t, strings.Contains(string(frame), `"cwd":`+strconv.Quote(want)), "child cwd = %s", frame)
		}
	}
	check(t, strings.Contains(string(findFrame(records(t, recordPath), "session/new")), `"cwd":`+strconv.Quote(want)), "ACP cwd was empty")
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

func TestCancelledCloseJoinsNativeProcesses(t *testing.T) {
	root := t.TempDir()
	p := New(filepath.Join(root, "agentbus.sock"), "cancelled-close")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	_, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "lane@local", Open: sessionkit.OpenOptions{Cwd: root}})
	must(t, err)
	p.mu.Lock()
	watcher, leader := p.watcher, p.leader
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = p.Close(ctx)
	for _, process := range []*nativeProcess{watcher, leader} {
		select {
		case <-process.done:
		default:
			t.Fatal("Close returned before a native process was reaped")
		}
	}
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

func TestDeliveryDoesNotHoldRunHandoff(t *testing.T) {
	root, recordPath := t.TempDir(), filepath.Join(t.TempDir(), "record")
	interjectRelease := filepath.Join(root, "interject-release")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_RELEASE", filepath.Join(root, "never"))
	t.Setenv("GROK_TEST_INTERJECT_BLOCK", interjectRelease)
	_, connection, reader := startGrokWorker(t, root)
	writeWorkerRequest(t, connection, 2, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "hold"})
	waitFrame(t, recordPath, "session/prompt", 1)
	writeWorkerRequest(t, connection, 3, "message.deliver", delivery("crossed delivery"))
	waitFrame(t, recordPath, "_x.ai/interject", 1)
	writeWorkerRequest(t, connection, 4, "turn.interrupt", map[string]string{"session_id": testSessionID + "@local"})
	check(t, readWorkerResponse(t, reader, 4).Result != nil, "interrupt did not progress")
	check(t, readWorkerResponse(t, reader, 2).Result != nil, "terminal did not progress")
	must(t, os.WriteFile(interjectRelease, nil, 0o600))
	response := readWorkerResponse(t, reader, 3)
	var receipt sessionkit.DeliveryReceipt
	must(t, json.Unmarshal(response.Result, &receipt))
	check(t, receipt.Disposition == "queued_for_next_turn", "stale native token reported %q", receipt.Disposition)
	writeWorkerRequest(t, connection, 5, "session.close", map[string]string{"session_id": testSessionID + "@local"})
	check(t, readWorkerResponse(t, reader, 5).Result != nil, "close response absent")
}

func TestIdleDeliveryJoinsOwnedPromptAndForeignChunksAreIgnored(t *testing.T) {
	root, recordPath := shortRoot(t), filepath.Join(t.TempDir(), "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_FOREIGN_CHUNK", "1")
	_, connection, reader := startGrokWorker(t, root)
	writeWorkerRequest(t, connection, 2, "message.deliver", delivery("idle-wire-token"))
	var receipt sessionkit.DeliveryReceipt
	must(t, json.Unmarshal(readWorkerResponse(t, reader, 2).Result, &receipt))
	check(t, receipt.Disposition == "queued_for_next_turn", "idle receipt = %#v", receipt)
	check(t, countFrames(records(t, recordPath), "_x.ai/interject") == 0, "idle delivery used native interject")
	writeWorkerRequest(t, connection, 3, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "caller-input"})
	var terminal sessionkit.TurnResult
	must(t, json.Unmarshal(readWorkerResponse(t, reader, 3).Result, &terminal))
	check(t, terminal.Outcome == "completed" && terminal.Result == "answer", "terminal = %#v", terminal)
	prompt := string(findFrame(records(t, recordPath), "session/prompt"))
	check(t, strings.Index(prompt, "idle-wire-token") >= 0 && strings.Index(prompt, "idle-wire-token") < strings.Index(prompt, "caller-input"), "owned prompt = %s", prompt)
	writeWorkerRequest(t, connection, 4, "session.close", map[string]string{"session_id": testSessionID + "@local"})
	check(t, readWorkerResponse(t, reader, 4).Result != nil, "close response absent")
}

func TestActiveDeliveryUsesInterject(t *testing.T) {
	root, recordPath := shortRoot(t), filepath.Join(t.TempDir(), "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_RELEASE", filepath.Join(root, "never"))
	p, connection, reader := startGrokWorker(t, root)
	writeWorkerRequest(t, connection, 2, "turn.run", map[string]any{"session_id": testSessionID + "@local", "input": "hold"})
	waitActive(t, p)
	writeWorkerRequest(t, connection, 3, "message.deliver", delivery("active-wire-token"))
	var receipt sessionkit.DeliveryReceipt
	must(t, json.Unmarshal(readWorkerResponse(t, reader, 3).Result, &receipt))
	check(t, receipt.Disposition == "injected" && countFrames(records(t, recordPath), "_x.ai/interject") == 1, "active receipt = %#v", receipt)
	writeWorkerRequest(t, connection, 4, "turn.interrupt", map[string]string{"session_id": testSessionID + "@local"})
	check(t, readWorkerResponse(t, reader, 4).Result != nil, "interrupt response absent")
	check(t, readWorkerResponse(t, reader, 2).Result != nil, "terminal absent")
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
	_, err := reader.reader.ReadBytes('\n')
	check(t, errors.Is(err, io.EOF), "worker did not shut down after terminal: %v", err)
}

type workerResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type workerReader struct {
	reader  *bufio.Reader
	pending map[int]workerResponse
}

func startGrokWorker(t *testing.T, root string) (*Wrapper, net.Conn, *workerReader) {
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
	reader := &workerReader{reader: bufio.NewReader(connection), pending: map[int]workerResponse{}}
	readLine(t, reader.reader)
	_, err = connection.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	must(t, err)
	writeWorkerRequest(t, connection, 1, "session.open", map[string]any{"name": "parent/grok@local", "groups": []string{"group"}, "open": map[string]any{"cwd": root}})
	opened := readWorkerResponse(t, reader, 1)
	check(t, opened.Result != nil, "open failed: %s", opened.Error)
	t.Cleanup(func() { _ = connection.Close(); <-worker.Closed(); _ = listener.Close() })
	return p, connection, reader
}

func shortRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "grok-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func writeWorkerRequest(t *testing.T, connection net.Conn, id int, method string, params any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	must(t, err)
	_, err = connection.Write(append(body, '\n'))
	must(t, err)
}

func readWorkerResponse(t *testing.T, reader *workerReader, id int) workerResponse {
	if response, ok := reader.pending[id]; ok {
		delete(reader.pending, id)
		return response
	}
	for {
		var response workerResponse
		must(t, json.Unmarshal(readLine(t, reader.reader), &response))
		if response.ID == id {
			return response
		}
		reader.pending[response.ID] = response
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

func waitActive(t *testing.T, p *Wrapper) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		active := p.active != nil
		p.mu.Unlock()
		if active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("native prompt did not become active")
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

func allStartsContain(records []json.RawMessage, value string) bool {
	found := false
	for _, raw := range records {
		if !strings.Contains(string(raw), `"kind":"START"`) {
			continue
		}
		found = true
		if !strings.Contains(string(raw), value) {
			return false
		}
	}
	return found
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
