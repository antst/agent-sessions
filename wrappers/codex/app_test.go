package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestAppClientCapturedFrames(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	notified := make(chan string, 1)
	client := newAppClient(clientSide, clientSide, func(method string, params json.RawMessage) {
		notified <- method + " " + string(params)
	}, func(error) {})
	requests := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(serverSide).ReadString('\n')
		requests <- line
		_, _ = serverSide.Write([]byte("{\"id\":1,\"result\":{\"userAgent\":\"agentbus-probe/0.153.4\"}}\n"))
		_, _ = serverSide.Write([]byte("{\"method\":\"turn/started\",\"params\":{\"threadId\":\"thread-1\",\"turn\":{\"id\":\"turn-1\"}}}\n"))
	}()
	var result struct {
		UserAgent string `json:"userAgent"`
	}
	if err := client.call(context.Background(), "initialize", map[string]any{"capabilities": map[string]bool{"experimentalApi": true}}, &result); err != nil {
		t.Fatal(err)
	}
	want := "{\"id\":1,\"jsonrpc\":\"2.0\",\"method\":\"initialize\",\"params\":{\"capabilities\":{\"experimentalApi\":true}}}\n"
	if got := <-requests; got != want {
		t.Fatalf("request = %s, want %s", got, want)
	}
	if result.UserAgent != "agentbus-probe/0.153.4" {
		t.Fatalf("result = %#v", result)
	}
	if got := <-notified; got != `turn/started {"threadId":"thread-1","turn":{"id":"turn-1"}}` {
		t.Fatalf("notification = %s", got)
	}
	_ = serverSide.Close()
	<-client.closed
}

func TestAppClientProtocolFailures(t *testing.T) {
	for _, test := range []struct{ name, response, want string }{
		{"native error", `{"id":1,"error":{"code":-32600,"message":"no active turn to interrupt"}}`, "no active turn to interrupt"},
		{"unknown id", `{"id":2,"result":{}}`, "unknown response id"},
		{"result and error", `{"id":1,"result":{},"error":{"code":-1,"message":"bad"}}`, "exactly one"},
		{"missing result", `{"id":1}`, "exactly one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientSide, serverSide := net.Pipe()
			failed := make(chan error, 1)
			client := newAppClient(clientSide, clientSide, nil, func(err error) { failed <- err })
			go func() {
				_, _ = bufio.NewReader(serverSide).ReadString('\n')
				_, _ = serverSide.Write([]byte(test.response + "\n"))
			}()
			err := client.call(context.Background(), "method", map[string]any{}, &struct{}{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if test.name != "native error" {
				if got := <-failed; !strings.Contains(got.Error(), test.want) {
					t.Fatalf("failure = %v", got)
				}
			}
			_ = serverSide.Close()
		})
	}
}

func TestAppClientServerRequest(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	client := newAppClient(clientSide, clientSide, nil, func(error) {})
	defer serverSide.Close()
	go func() {
		_, _ = serverSide.Write([]byte(`{"id":41,"method":"item/commandExecution/requestApproval","params":{}}` + "\n"))
	}()
	line, err := bufio.NewReader(serverSide).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID    int
		Error appError
	}
	if json.Unmarshal([]byte(line), &response) != nil || response.ID != 41 || response.Error.Code != -32601 {
		t.Fatalf("response = %s", line)
	}
	_ = serverSide.Close()
	<-client.closed
}

func TestAppClientPreCanceledCallWritesNothing(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	client := newAppClient(clientSide, clientSide, nil, func(error) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.call(ctx, "never", struct{}{}, &struct{}{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
	_ = serverSide.Close()
}

func TestAppClientCancelStopsBlockedWrite(t *testing.T) {
	input := &blockedAppInput{started: make(chan struct{}), closed: make(chan struct{})}
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	client := newAppClient(input, clientSide, nil, func(error) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.call(ctx, "blocked", map[string]string{"body": strings.Repeat("x", 4096)}, &struct{}{})
	}()
	<-input.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

type blockedAppInput struct {
	start, stop     sync.Once
	started, closed chan struct{}
}

func (w *blockedAppInput) Write([]byte) (int, error) {
	w.start.Do(func() { close(w.started) })
	<-w.closed
	return 0, context.Canceled
}
func (w *blockedAppInput) Close() error { w.stop.Do(func() { close(w.closed) }); return nil }
