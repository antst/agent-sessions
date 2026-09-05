package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

const fixtureID = "00000000-0000-4000-8000-000000000123"

func TestMain(m *testing.M) {
	if mode := os.Getenv("CLAUDE_TEST_CHILD"); mode != "" {
		fakeChild(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeChild(mode string) {
	record := map[string]any{"args": os.Args[1:], "lane_socket": os.Getenv(LaneSocketEnv)}
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		record[name] = os.Getenv(name)
	}
	body, _ := json.Marshal(record)
	_ = os.WriteFile(os.Getenv("CLAUDE_TEST_RECORD"), body, 0o600)
	output := json.NewEncoder(os.Stdout)
	_ = output.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": fixtureID})
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var item map[string]any
		_ = json.Unmarshal(scanner.Bytes(), &item)
		if item["type"] == "user" {
			if mode == "die" {
				os.Exit(7)
			}
			_ = output.Encode(map[string]any{"type": "user", "isReplay": true})
			_ = output.Encode(map[string]any{"type": "result", "subtype": "success", "session_id": fixtureID, "result": "ok"})
		}
	}
}

func TestLaunchArgumentsTable(t *testing.T) {
	mcp := `{"mcpServers":{"agent_sessions":{"args":["mcp"],"command":"claude-peer","env":{"AGENTBUS_LANE_SOCKET":"/tmp/lane.sock"}}}}`
	base := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages"}
	for _, test := range []struct {
		name    string
		request sessionkit.OpenRequest
		want    []string
	}{
		{"fresh typed", sessionkit.OpenRequest{Name: "parent/leaf@local", Open: sessionkit.OpenOptions{Model: "sonnet", ReasoningEffort: "high", Arguments: []string{"--agent", "reviewer"}}}, append(append([]string{}, base...), "--session-id", fixtureID, "--name", "parent/leaf", "--permission-mode", "dontAsk", "--model", "sonnet", "--effort", "high", "--mcp-config", mcp, "--allowedTools", "mcp__agent_sessions__*", "--agent", "reviewer")},
		{"resume bypass", sessionkit.OpenRequest{Name: "ignored@local", ResumeSessionID: fixtureID, Open: sessionkit.OpenOptions{PermissionMode: "bypassPermissions"}}, append(append([]string{}, base...), "--resume", fixtureID, "--dangerously-skip-permissions", "--mcp-config", mcp, "--allowedTools", "mcp__agent_sessions__*")},
		{"native permission", sessionkit.OpenRequest{Name: "leaf@local", Open: sessionkit.OpenOptions{PermissionMode: "acceptEdits"}}, append(append([]string{}, base...), "--session-id", fixtureID, "--name", "leaf", "--permission-mode", "acceptEdits", "--mcp-config", mcp, "--allowedTools", "mcp__agent_sessions__*")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := launchArguments(test.request, fixtureID, "/tmp/lane.sock")
			must(t, err)
			check(t, reflect.DeepEqual(got, test.want), "arguments = %#v", got)
		})
	}
}

func TestArgumentConflicts(t *testing.T) {
	for _, test := range []struct{ argument, want string }{
		{"--model=x", "argument conflicts with typed field model"},
		{"--resume", "argument conflicts with typed field session_id"},
		{"--strict-mcp-config", "argument conflicts with typed field mcp"},
		{"--", "argument conflicts with typed field arguments"},
		{"prompt", "unsupported argument prompt"},
	} {
		_, err := launchArguments(sessionkit.OpenRequest{Name: "leaf@local", Open: sessionkit.OpenOptions{Arguments: []string{test.argument}}}, fixtureID, "/tmp/lane.sock")
		check(t, err != nil && err.Error() == test.want, "%s error = %v", test.argument, err)
	}
}

func TestHelloAndIdentity(t *testing.T) {
	p := New("")
	hello, err := p.Hello(context.Background())
	must(t, err)
	check(t, hello.Product == Product && reflect.DeepEqual(hello.SupportedOpenFields, []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"}) && len(hello.ExtraArguments) == 1 && hello.ExtraArguments[0].Name == "--agent", "hello = %#v", hello)
	id, err := sessionID("")
	must(t, err)
	check(t, len(id) == 36 && id[14] == '4' && strings.Contains("89ab", string(id[19])), "uuid = %q", id)
	p.expected, p.ready = fixtureID, make(chan error, 1)
	p.receive(frame{Type: "system", Subtype: "init", SessionID: "wrong"})
	check(t, strings.Contains((<-p.ready).Error(), `from "00000000-0000-4000-8000-000000000123" to "wrong"`), "identity mismatch changed")
}

type frameLog struct{ wrote chan []byte }

func (w *frameLog) Write(body []byte) (int, error) {
	copyOfBody := append([]byte(nil), bytes.TrimSpace(body)...)
	w.wrote <- copyOfBody
	return len(body), nil
}

func newStream() (*Wrapper, *frameLog) {
	writes := &frameLog{wrote: make(chan []byte, 16)}
	p := New("")
	p.expected, p.ready, p.init, p.encoder = fixtureID, make(chan error, 1), true, json.NewEncoder(writes)
	return p, writes
}

func TestStreamRunDeliveryAndInterrupt(t *testing.T) {
	p, writes := newStream()
	must(t, p.write("  preserved \n"))
	preserved := <-writes.wrote
	check(t, string(preserved) == `{"message":{"content":[{"text":"  preserved \n","type":"text"}],"role":"user"},"type":"user"}`, "user frame = %s", preserved)
	p.writes, p.replays = 0, 0
	receipt, err := p.Deliver(context.Background(), delivery("queued"))
	must(t, err)
	check(t, receipt.Disposition == "queued_for_next_turn", "idle receipt = %#v", receipt)
	done := make(chan sessionkit.TurnResult, 1)
	go func() {
		result, _ := p.Run(context.Background(), &sessionkit.Run{}, "caller")
		done <- result
	}()
	runFrame := prompt(<-writes.wrote)
	check(t, strings.Index(runFrame, "queued") < strings.Index(runFrame, "caller"), "queued prompt = %q", runFrame)
	p.receive(frame{Type: "user", IsReplay: true})
	p.receive(frame{Type: "result", Subtype: "success", SessionID: fixtureID, Result: "answer"})
	check(t, (<-done).Result == "answer", "run result changed")

	native, err := p.start(context.Background(), "next")
	must(t, err)
	<-writes.wrote
	receipt, err = p.Deliver(context.Background(), delivery("steer"))
	must(t, err)
	check(t, receipt.Disposition == "injected" && strings.Contains(prompt(<-writes.wrote), "steer"), "active receipt = %#v", receipt)
	p.receive(frame{Type: "user", IsReplay: true})
	p.receive(frame{Type: "user", IsReplay: true})
	p.receive(frame{Type: "result", Subtype: "success", SessionID: fixtureID})
	_, err = native.Wait(context.Background())
	must(t, err)
	receipt, err = p.Deliver(context.Background(), delivery("after"))
	must(t, err)
	check(t, receipt.Disposition == "queued_for_next_turn", "terminal receipt = %#v", receipt)

	native, err = p.start(context.Background(), "interrupt me")
	must(t, err)
	<-writes.wrote
	p.receive(frame{Type: "user", IsReplay: true})
	interrupted := make(chan error, 1)
	go func() { interrupted <- native.Interrupt(context.Background()) }()
	control := <-writes.wrote
	check(t, string(control) == `{"request":{"subtype":"interrupt"},"request_id":"interrupt-1","type":"control_request"}`, "control frame = %s", control)
	p.receive(frame{Type: "control_response", Response: map[string]string{"request_id": "interrupt-1", "subtype": "success"}})
	must(t, <-interrupted)
	p.receive(frame{Type: "result", Subtype: "interrupted", SessionID: fixtureID})
	result, err := native.Wait(context.Background())
	must(t, err)
	check(t, result.Outcome == "interrupted", "terminal = %#v", result)
}

func TestResultWaitsForInjectedReplay(t *testing.T) {
	p, writes := newStream()
	native, err := p.start(context.Background(), "caller")
	must(t, err)
	<-writes.wrote
	p.receive(frame{Type: "user", IsReplay: true})
	receipt, err := p.Deliver(context.Background(), delivery("injected"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "receipt = %#v", receipt)
	<-writes.wrote
	p.receive(frame{Type: "result", Subtype: "success", SessionID: fixtureID, Result: "includes delivery"})
	select {
	case <-native.(*turn).done:
		t.Fatal("result completed before the injected frame replay")
	default:
	}
	p.receive(frame{Type: "user", IsReplay: true})
	result, err := native.Wait(context.Background())
	must(t, err)
	check(t, result.Result == "includes delivery", "terminal = %#v", result)
}

func TestTerminalTable(t *testing.T) {
	for _, test := range []struct {
		name string
		item frame
		want sessionkit.TurnResult
	}{
		{"success", frame{Subtype: "success", Result: "ok"}, sessionkit.TurnResult{Outcome: "completed", Result: "ok"}},
		{"interrupted", frame{Subtype: "error", TerminalReason: "aborted_streaming"}, sessionkit.TurnResult{Outcome: "interrupted", NativeStopReason: "aborted_streaming"}},
		{"exact error", frame{Subtype: "error", IsError: true, Error: "denied", Result: "summary"}, sessionkit.TurnResult{Outcome: "failed", Result: "denied"}},
	} {
		t.Run(test.name, func(t *testing.T) { check(t, terminal(test.item) == test.want, "terminal = %#v", terminal(test.item)) })
	}
}

func TestWorkerChildDeathWritesTerminalBeforeEOF(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "cp")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket, record := filepath.Join(directory, "bus.sock"), filepath.Join(directory, "child.json")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Setenv(host.TokenEnv, "token")
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.SessionIDEnv, "secret")
	t.Setenv(host.NameEnv, "secret")
	t.Setenv(host.GroupsEnv, "secret")
	t.Setenv(LaneSocketEnv, "stale")
	t.Setenv("CLAUDE_TEST_CHILD", "die")
	t.Setenv("CLAUDE_TEST_RECORD", record)
	must(t, os.Symlink(os.Args[0], filepath.Join(directory, "claude")))
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := New(socket)
	worker := sessionkit.NewWorker(p)
	p.SetShutdown(worker.Shutdown)
	served := make(chan error, 1)
	go func() { served <- worker.Serve(context.Background()) }()
	connection, err := listener.Accept()
	must(t, err)
	reader := bufio.NewReader(connection)
	hello := readJSON(t, reader)
	writeJSON(t, connection, map[string]any{"jsonrpc": "2.0", "id": hello["id"], "result": map[string]any{}})
	writeJSON(t, connection, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session.open", "params": map[string]any{"name": "parent/leaf@local", "groups": []string{}, "resume_session_id": fixtureID, "open": map[string]any{}}})
	opened := readJSON(t, reader)
	check(t, opened["error"] == nil, "open = %#v", opened)
	var child map[string]any
	must(t, json.Unmarshal(mustRead(t, record), &child))
	check(t, strings.HasSuffix(child["lane_socket"].(string), "/lanes/"+fixtureID+".sock"), "child environment = %#v", child)
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		check(t, child[name] == "", "%s reached child: %#v", name, child)
	}
	writeJSON(t, connection, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "turn.run", "params": map[string]any{"session_id": fixtureID + "@local", "input": "die"}})
	terminal := readJSON(t, reader)
	result := terminal["result"].(map[string]any)
	check(t, result["outcome"] == "failed", "terminal = %#v", terminal)
	_, err = reader.ReadByte()
	check(t, errors.Is(err, io.EOF), "worker remained connected: %v", err)
	<-worker.Closed()
	_ = connection.Close()
	_ = listener.Close()
	_ = <-served
}

func delivery(body string) sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{MessageID: "message", Body: body, From: sessionkit.DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "example", Groups: []string{"project"}}}
}

func prompt(body []byte) string {
	var item struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(body, &item)
	return item.Message.Content[0].Text
}

func writeJSON(t *testing.T, output io.Writer, value any) {
	t.Helper()
	must(t, json.NewEncoder(output).Encode(value))
}

func readJSON(t *testing.T, input *bufio.Reader) map[string]any {
	t.Helper()
	var value map[string]any
	must(t, json.Unmarshal(bytes.TrimSpace(readLine(t, input)), &value))
	return value
}

func readLine(t *testing.T, input *bufio.Reader) []byte {
	t.Helper()
	body, err := input.ReadBytes('\n')
	must(t, err)
	return body
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	must(t, err)
	return body
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, condition bool, format string, values ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(format, values...)
	}
}
