package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
)

type fakeBackend struct {
	method string
	params string
	result json.RawMessage
	err    error
	calls  int
}

func (*fakeBackend) Prepare(context.Context, json.RawMessage) error { return nil }

type preparedBackend struct {
	fakeBackend
	prepared int
	meta     []string
	caller   *sessionkit.Caller
}

type actionBackend struct {
	action    string
	arguments string
}

func (*actionBackend) Prepare(context.Context, json.RawMessage) error { return nil }
func (b *actionBackend) Action(_ context.Context, action string, arguments json.RawMessage) (json.RawMessage, error) {
	b.action, b.arguments = action, string(arguments)
	return json.RawMessage(`{"message_id":"message","deliveries":[]}`), nil
}

func (b *preparedBackend) Caller() *sessionkit.Caller { return b.caller }
func (b *preparedBackend) Prepare(_ context.Context, meta json.RawMessage) error {
	b.prepared++
	b.meta = append(b.meta, string(meta))
	return nil
}

func (b *fakeBackend) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	encoded, _ := json.Marshal(params)
	b.method, b.params, b.calls = method, string(encoded), b.calls+1
	return b.result, b.err
}

func TestServerMethods(t *testing.T) {
	tests := []struct {
		name, input string
		backend     *fakeBackend
		code        float64
		check       func(*testing.T, map[string]any, *fakeBackend)
	}{
		{"initialize version mismatch", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"fixture"}}`, &fakeBackend{}, 0, func(t *testing.T, response map[string]any, _ *fakeBackend) {
			result := response["result"].(map[string]any)
			check(t, result["protocolVersion"] == ProtocolVersion, "initialize = %#v", result)
		}},
		{"tools list", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, &fakeBackend{}, 0, func(t *testing.T, response map[string]any, _ *fakeBackend) {
			tool := response["result"].(map[string]any)["tools"].([]any)[0].(map[string]any)
			schema := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			check(t, tool["name"] == ToolName && reflect.DeepEqual(stringsOf(schema["action"].(map[string]any)["enum"]), sessionkit.Actions), "tool = %#v", tool)
		}},
		{"tools call", `{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"sessionbus","arguments":{"action":"send","arguments":{"target":"peer","message":"hello"}}}}`, &fakeBackend{result: json.RawMessage(`{"message_id":"message","deliveries":[]}`)}, 0, func(t *testing.T, response map[string]any, backend *fakeBackend) {
			result := response["result"].(map[string]any)
			check(t, backend.method == "message.send" && backend.params == `{"target":"peer","message":"hello"}` && result["structuredContent"].(map[string]any)["message_id"] == "message", "call = %#v / %#v", backend, result)
		}},
		{"protocol error", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sessionbus","arguments":{"action":"list"}}}`, &fakeBackend{err: &sessionkit.ProtocolError{Code: -32004, Message: "not_running"}}, -32004, nil},
		{"invalid action", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"sessionbus","arguments":{"action":"unknown"}}}`, &fakeBackend{}, -32602, nil},
		{"unknown method", `{"jsonrpc":"2.0","id":6,"method":"unknown"}`, &fakeBackend{}, -32601, nil},
		{"parse error", `{`, &fakeBackend{}, -32700, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := serveOne(&Server{Backend: test.backend}, test.input)
			check(t, err == nil, "serve: %v", err)
			if test.code != 0 {
				failure := response["error"].(map[string]any)
				check(t, failure["code"] == test.code, "error = %#v", failure)
			} else if test.check != nil {
				test.check(t, response, test.backend)
			}
		})
	}
}

func TestInvalidRequests(t *testing.T) {
	tests := []struct {
		name, input string
		id          any
	}{
		{"wrong version", `{"jsonrpc":"1.0","id":"kept","method":"initialize"}`, "kept"},
		{"missing method", `{"jsonrpc":"2.0","id":8}`, float64(8)},
		{"bad id kind", `{"jsonrpc":"2.0","id":{},"method":"initialize"}`, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := serveOne(&Server{Backend: &fakeBackend{}}, test.input)
			check(t, err == nil, "serve: %v", err)
			failed := response["error"].(map[string]any)
			check(t, failed["code"] == float64(-32600) && reflect.DeepEqual(response["id"], test.id), "response = %#v", response)
		})
	}
}

func TestToolArgumentsMustBeObject(t *testing.T) {
	for _, arguments := range []string{"null", `[]`, `"scalar"`} {
		t.Run(arguments, func(t *testing.T) {
			backend := &fakeBackend{}
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sessionbus","arguments":{"action":"send","arguments":` + arguments + `}}}`
			response, err := serveOne(&Server{Backend: backend}, input)
			check(t, err == nil, "serve: %v", err)
			failed := response["error"].(map[string]any)
			check(t, failed["code"] == float64(-32602) && backend.calls == 0, "response/backend = %#v/%#v", response, backend)
		})
	}
}

func TestServerUsesBackendCallerAfterPrepare(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	backend := &preparedBackend{caller: sessionkit.NewCaller(func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		check(t, method == "turn.run", "method = %q", method)
		close(started)
		<-release
		return json.RawMessage(`{"outcome":"completed","result":"done"}`), nil
	})}
	server := &Server{Backend: backend}
	response, _ := serveOne(server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sessionbus","_meta":{"threadId":"native"},"arguments":{"action":"start","arguments":{"session_id":"lane@local","input":"work"}}}}`)
	encoded, _ := json.Marshal(response)
	check(t, strings.Contains(string(encoded), `\"turn_id\":\"t-1\"`), "start = %s", encoded)
	<-started
	response, _ = serveOne(server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sessionbus","arguments":{"action":"status","arguments":{"turn_id":"t-1"}}}}`)
	encoded, _ = json.Marshal(response)
	check(t, strings.Contains(string(encoded), `\"state\":\"running\"`), "status = %s", encoded)
	close(release)
	response, _ = serveOne(server, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessionbus","arguments":{"action":"wait","arguments":{"turn_id":"t-1"}}}}`)
	encoded, _ = json.Marshal(response)
	check(t, strings.Contains(string(encoded), `\"state\":\"done\"`) && strings.Contains(string(encoded), `\"result\":\"done\"`) && reflect.DeepEqual(backend.meta, []string{`{"threadId":"native"}`, "", ""}), "wait = %s, meta = %#v", encoded, backend.meta)
}

func TestServerUsesStatelessActionBackend(t *testing.T) {
	backend := &actionBackend{}
	response, err := serveOne(&Server{Backend: backend}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_sessions","arguments":{"action":"send","arguments":{"target":"peer","message":"hello"}}}}`)
	check(t, err == nil, "serve: %v", err)
	result := response["result"].(map[string]any)
	check(t, backend.action == "send" && backend.arguments == `{"target":"peer","message":"hello"}` && result["structuredContent"].(map[string]any)["message_id"] == "message", "action/result = %#v / %#v", backend, result)
}

func TestServerReturnsOutputFailure(t *testing.T) {
	err := (&Server{Backend: &fakeBackend{}}).Serve(context.Background(), reader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`), failedWriter{})
	check(t, errors.Is(err, io.ErrClosedPipe), "error = %v", err)
}

func TestServerCancelWithStoppedOutput(t *testing.T) {
	input, writeInput := io.Pipe()
	output := &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&Server{Backend: &fakeBackend{}}).Serve(ctx, input, output) }()
	_, _ = io.WriteString(writeInput, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n")
	<-output.started
	cancel()
	check(t, errors.Is(<-done, context.Canceled), "server did not return on cancel")
	close(output.release)
}

func serveOne(server *Server, input string) (map[string]any, error) {
	in, writeInput := io.Pipe()
	readOutput, output := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), in, output) }()
	go func() { _, _ = io.WriteString(writeInput, input+"\n"); _ = writeInput.Close() }()
	var response map[string]any
	err := json.NewDecoder(readOutput).Decode(&response)
	_ = readOutput.Close()
	_ = output.Close()
	return response, errors.Join(err, <-done)
}

func stringsOf(value any) []string {
	values := value.([]any)
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].(string)
	}
	return result
}

func reader(value string) io.Reader { return &stringReader{value: []byte(value + "\n")} }

type stringReader struct{ value []byte }

func (r *stringReader) Read(body []byte) (int, error) {
	if len(r.value) == 0 {
		return 0, io.EOF
	}
	count := copy(body, r.value)
	r.value = r.value[count:]
	return count, nil
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type blockedWriter struct{ started, release chan struct{} }

func (w *blockedWriter) Write(body []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(body), nil
}

func check(t *testing.T, condition bool, format string, values ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(format, values...)
	}
}
