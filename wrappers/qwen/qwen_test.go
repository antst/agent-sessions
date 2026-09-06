package qwen

import (
	"bufio"
	"bytes"
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
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

const fixtureID = "11111111-2222-4333-8444-555555555555"

func TestMain(m *testing.M) {
	if os.Getenv("QWEN_TEST_CHILD") != "" {
		fakeChild()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeChild() {
	record := map[string]any{"args": os.Args[1:], "lane_socket": os.Getenv("AGENTBUS_LANE_SOCKET")}
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		record[name] = os.Getenv(name)
	}
	body, _ := json.Marshal(record)
	_ = os.WriteFile(os.Getenv("QWEN_TEST_RECORD"), body, 0o600)
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var request struct {
			ID     int64
			Method string
			Params struct {
				SessionID string            `json:"sessionId"`
				Cwd       string            `json:"cwd"`
				MCP       []any             `json:"mcpServers"`
				Meta      map[string]string `json:"_meta"`
				Title     string            `json:"title"`
			}
		}
		if decoder.Decode(&request) != nil {
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "qwen-code"}, "agentCapabilities": map[string]bool{"loadSession": true}}
		case "session/resume":
			result = map[string]any{"sessionId": request.Params.SessionID, "modes": map[string]string{"currentModeId": "yolo"}}
		case "session/new":
			if request.Params.Cwd == "" || len(request.Params.MCP) != 1 {
				return
			}
			result = map[string]any{"sessionId": request.Params.Meta["qwen-code/sessionId"], "modes": map[string]string{"currentModeId": "default"}}
		case "session/set_config_option":
			result = map[string]any{"configOptions": []any{map[string]string{"id": "reasoning_effort", "currentValue": "low"}}}
		case "renameSession":
			result = map[string]any{"success": request.Params.Title == "fresh"}
		default:
			result = map[string]any{}
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
}

func TestFreshOpenMintsV4AndRenames(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "qw")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket, record := filepath.Join(directory, "bus.sock"), filepath.Join(directory, "child.json")
	t.Setenv("QWEN_TEST_CHILD", "1")
	t.Setenv("QWEN_TEST_RECORD", record)
	oldCommand := laneCommand
	laneCommand = func(_ string, arguments ...string) *exec.Cmd { return exec.Command(os.Args[0], arguments...) }
	defer func() { laneCommand = oldCommand }()
	p := New(socket)
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	result, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "fresh@local", Open: sessionkit.OpenOptions{Cwd: directory}})
	must(t, err)
	check(t, len(result.SessionID) == 36 && result.SessionID[14] == '4' && strings.Contains("89ab", string(result.SessionID[19])), "session id = %q", result.SessionID)
	must(t, p.Close(context.Background()))
}

func TestOpenValueAndArgumentErrors(t *testing.T) {
	for _, test := range []struct {
		open sessionkit.OpenOptions
		want string
	}{
		{sessionkit.OpenOptions{PermissionMode: "ask"}, "unsupported value permission_mode=ask"},
		{sessionkit.OpenOptions{ReasoningEffort: "high"}, "unsupported value reasoning_effort=high"},
		{sessionkit.OpenOptions{Arguments: []string{"--resume=x"}}, "argument conflicts with typed field session_id"},
		{sessionkit.OpenOptions{Arguments: []string{"--"}}, "argument conflicts with typed field arguments"},
	} {
		_, err := launchArguments(test.open)
		check(t, err != nil && err.Error() == test.want, "error = %v", err)
	}
	arguments, err := launchArguments(sessionkit.OpenOptions{Arguments: []string{"--system-prompt", "--resume"}})
	must(t, err)
	check(t, reflect.DeepEqual(arguments, []string{"--acp", "--system-prompt", "--resume"}), "arguments = %#v", arguments)
	arguments, err = launchArguments(sessionkit.OpenOptions{Arguments: []string{"--telemetry"}})
	must(t, err)
	check(t, reflect.DeepEqual(arguments, []string{"--acp", "--telemetry"}), "telemetry arguments = %#v", arguments)
	_, err = launchArguments(sessionkit.OpenOptions{Arguments: []string{"--telemetry-enabled"}})
	check(t, err != nil && err.Error() == "unsupported argument --telemetry-enabled", "telemetry-enabled error = %v", err)
}

func TestOpenResumeUsesCapturedACPShapesAndScrubsBusEnv(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "qw")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket, record := filepath.Join(directory, "bus.sock"), filepath.Join(directory, "child.json")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	defer listener.Close()
	t.Setenv("QWEN_TEST_CHILD", "1")
	t.Setenv("QWEN_TEST_RECORD", record)
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		t.Setenv(name, "secret")
	}
	oldCommand := laneCommand
	laneCommand = func(_ string, arguments ...string) *exec.Cmd { return exec.Command(os.Args[0], arguments...) }
	defer func() { laneCommand = oldCommand }()
	p := New(socket)
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	result, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "leaf@local", ResumeSessionID: fixtureID, Open: sessionkit.OpenOptions{Cwd: directory, PermissionMode: "bypassPermissions", Model: "model", ReasoningEffort: "low", Arguments: []string{"--screen-reader"}}})
	must(t, err)
	check(t, result.SessionID == fixtureID, "open = %#v", result)
	var child map[string]any
	must(t, json.Unmarshal(mustRead(t, record), &child))
	check(t, reflect.DeepEqual(child["args"], []any{"--acp", "--yolo", "-m", "model", "--screen-reader"}), "args = %#v", child["args"])
	check(t, child["lane_socket"] == filepath.Join(directory, "lanes", fixtureID+".sock"), "lane socket = %#v", child["lane_socket"])
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		check(t, child[name] == "", "%s reached child: %#v", name, child)
	}
	must(t, p.Close(context.Background()))
}

func TestRunDrainsPendingAndRestoresUndrained(t *testing.T) {
	p, productIn, productOut := newFixtureClient(t)
	firstDone := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "first"); firstDone <- result }()
	first := readRequest(t, productIn)
	check(t, first.Method == "session/prompt", "request = %#v", first)
	receipt, err := p.Deliver(context.Background(), delivery("MID"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "receipt = %#v", receipt)
	writeFrame(t, productOut, `{"jsonrpc":"2.0","id":79,"method":"craft/drainMidTurnQueue","params":{"sessionId":"wrong"}}`)
	wrong := readRequest(t, productIn)
	check(t, bytes.Contains(wrong.Raw, []byte(`"code":-32602`)) && !bytes.Contains(wrong.Raw, []byte("MID")), "wrong-session drain = %s", wrong.Raw)
	writeFrame(t, productOut, `{"jsonrpc":"2.0","id":80,"method":"craft/drainMidTurnQueue","params":{"sessionId":"`+fixtureID+`"}}`)
	drain := readRequest(t, productIn)
	check(t, bytes.Contains(drain.Raw, []byte(`"hasQueuedPrompt":true`)) && bytes.Contains(drain.Raw, []byte("MID")), "drain = %s", drain.Raw)
	writeFrame(t, productOut, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"`+fixtureID+`","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"answer"}}}}`)
	writeResult(t, productOut, first.ID, `{"stopReason":"end_turn"}`)
	check(t, (<-firstDone).Result == "answer", "first result changed")

	secondDone := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "second"); secondDone <- result }()
	second := readRequest(t, productIn)
	check(t, !bytes.Contains(second.Raw, []byte("MID")), "claimed message replayed: %s", second.Raw)
	receipt, err = p.Deliver(context.Background(), delivery("UNDRAINED"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "receipt = %#v", receipt)
	writeResult(t, productOut, second.ID, `{"stopReason":"end_turn"}`)
	<-secondDone
	thirdDone := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "third"); thirdDone <- result }()
	third := readRequest(t, productIn)
	check(t, bytes.Contains(third.Raw, []byte("UNDRAINED")), "undrained message lost: %s", third.Raw)
	writeResult(t, productOut, third.ID, `{"stopReason":"end_turn"}`)
	<-thirdDone
}

func TestDrainRequestIsHandledBeforeFollowingTerminal(t *testing.T) {
	p, productIn, productOut := newFixtureClient(t)
	done := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "turn"); done <- result }()
	prompt := readRequest(t, productIn)
	receipt, err := p.Deliver(context.Background(), delivery("ORDERED"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "receipt = %#v", receipt)

	drain := p.client.drain
	entered, release := make(chan struct{}), make(chan struct{})
	p.client.drain = func(sessionID string) ([]string, error) {
		close(entered)
		<-release
		return drain(sessionID)
	}
	writeFrame(t, productOut, `{"jsonrpc":"2.0","id":90,"method":"craft/drainMidTurnQueue","params":{"sessionId":"`+fixtureID+`"}}`)
	writeResult(t, productOut, prompt.ID, `{"stopReason":"end_turn"}`)
	<-entered
	select {
	case result := <-done:
		t.Fatalf("terminal passed held drain: %#v", result)
	default:
	}
	close(release)
	response := readRequest(t, productIn)
	check(t, bytes.Count(response.Raw, []byte("ORDERED")) == 1, "drain response = %s", response.Raw)
	<-done

	next := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "next"); next <- result }()
	nextPrompt := readRequest(t, productIn)
	check(t, !bytes.Contains(nextPrompt.Raw, []byte("ORDERED")), "claimed delivery replayed: %s", nextPrompt.Raw)
	writeResult(t, productOut, nextPrompt.ID, `{"stopReason":"end_turn"}`)
	<-next
}

func TestFailedPromptWriteRestoresPendingDelivery(t *testing.T) {
	writer := &blockedWriteCloser{entered: make(chan struct{}), release: make(chan struct{})}
	clientOutput, productOut, err := os.Pipe()
	must(t, err)
	p := New("")
	p.id = fixtureID
	p.client = newACPClient(writer, clientOutput, p.receive, p.drain)
	t.Cleanup(func() { _ = productOut.Close(); <-p.client.done })
	failed := make(chan error, 1)
	go func() {
		_, err := p.Run(context.Background(), &sessionkit.Run{}, "turn")
		failed <- err
	}()
	<-writer.entered
	receipt, err := p.Deliver(context.Background(), delivery("RESTORED"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "receipt = %#v", receipt)
	close(writer.release)
	check(t, (<-failed).Error() == "prompt write failed", "prompt start did not fail")

	next, prompt := &immediateTurn{}, ""
	_, err = p.handoff.Run(context.Background(), &sessionkit.Run{}, "next", func(_ context.Context, input string) (host.Turn, error) {
		prompt = input
		return next, nil
	})
	must(t, err)
	check(t, strings.Count(prompt, "RESTORED") == 1 && len(p.handoff.Claim()) == 0, "prompt = %q", prompt)
	_, err = p.handoff.Run(context.Background(), &sessionkit.Run{}, "again", func(_ context.Context, input string) (host.Turn, error) {
		prompt = input
		return next, nil
	})
	must(t, err)
	check(t, !strings.Contains(prompt, "RESTORED"), "delivery replayed twice: %q", prompt)
}

func TestInterruptIgnoresCancelledBooleanAndUsesTerminal(t *testing.T) {
	p, productIn, productOut := newFixtureClient(t)
	done := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "block"); done <- result }()
	prompt := readRequest(t, productIn)
	p.mu.Lock()
	native := p.active
	p.mu.Unlock()
	interrupted := make(chan error, 1)
	go func() { interrupted <- native.Interrupt(context.Background()) }()
	cancel := readRequest(t, productIn)
	check(t, cancel.Method == "craft/cancelPendingPrompt", "cancel = %#v", cancel)
	writeResult(t, productOut, cancel.ID, `{"cancelled":false}`)
	must(t, <-interrupted)
	writeResult(t, productOut, prompt.ID, `{"stopReason":"end_turn"}`)
	check(t, (<-done).Outcome == "interrupted", "terminal was not authoritative")
}

func TestACPClientAnswersCapturedRequestsAndDrainsLargeExitFrame(t *testing.T) {
	p, productIn, productOut := newFixtureClient(t)
	writeFrame(t, productOut, `{"jsonrpc":"2.0","id":91,"method":"session/request_permission","params":{}}`)
	permission := readRequest(t, productIn)
	check(t, string(permission.Raw) == `{"id":91,"jsonrpc":"2.0","result":{"outcome":{"outcome":"cancelled"}}}`, "permission = %s", permission.Raw)
	done := make(chan sessionkit.TurnResult, 1)
	go func() { result, _ := p.Run(context.Background(), &sessionkit.Run{}, "large"); done <- result }()
	prompt := readRequest(t, productIn)
	large := strings.Repeat("x", 300000) + "tail"
	writeFrame(t, productOut, fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}}}}`, fixtureID, large))
	writeResult(t, productOut, prompt.ID, `{"stopReason":"end_turn"}`)
	must(t, productOut.Close())
	result := <-done
	check(t, len(result.Result) == 300004 && strings.HasSuffix(result.Result, "tail"), "result bytes = %d", len(result.Result))
}

type wireRequest struct {
	ID     int64
	Method string
	Raw    []byte
}

type blockedWriteCloser struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockedWriteCloser) Write([]byte) (int, error) {
	close(w.entered)
	<-w.release
	return 0, errors.New("prompt write failed")
}
func (*blockedWriteCloser) Close() error { return nil }

type immediateTurn struct{}

func (*immediateTurn) Wait(context.Context) (sessionkit.TurnResult, error) {
	return sessionkit.TurnResult{Outcome: "completed"}, nil
}
func (*immediateTurn) Interrupt(context.Context) error { return nil }

func newFixtureClient(t *testing.T) (*Wrapper, *bufio.Reader, *os.File) {
	productIn, clientInput, err := os.Pipe()
	must(t, err)
	clientOutput, productOut, err := os.Pipe()
	must(t, err)
	p := New("")
	p.id = fixtureID
	p.client = newACPClient(clientInput, clientOutput, p.receive, p.drain)
	t.Cleanup(func() { _ = productIn.Close(); _ = productOut.Close(); _ = p.client.close(); <-p.client.done })
	return p, bufio.NewReader(productIn), productOut
}

func readRequest(t *testing.T, input *bufio.Reader) wireRequest {
	t.Helper()
	line, err := input.ReadBytes('\n')
	must(t, err)
	line = bytes.TrimSpace(line)
	var request wireRequest
	must(t, json.Unmarshal(line, &request))
	request.Raw = append([]byte(nil), line...)
	return request
}

func writeFrame(t *testing.T, output io.Writer, frame string) {
	t.Helper()
	_, err := io.WriteString(output, frame+"\n")
	must(t, err)
}

func writeResult(t *testing.T, output io.Writer, id int64, result string) {
	t.Helper()
	writeFrame(t, output, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result))
}

func delivery(body string) sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{MessageID: "message", Body: body, From: sessionkit.DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "example", Groups: []string{"project"}}}
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

func check(t *testing.T, ok bool, format string, values ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, values...)
	}
}
