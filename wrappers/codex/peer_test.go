package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

func TestPeerNativeAppStartsOnlyAfterIdentity(t *testing.T) {
	previous := peerAppStart
	started := 0
	peerAppStart = func() (*exec.Cmd, io.WriteCloser, io.Reader, error) {
		started++
		return nil, nil, nil, errors.New("native start observed")
	}
	t.Cleanup(func() { peerAppStart = previous })
	b, err := NewPeerBackend(context.Background())
	if err != nil || started != 0 {
		t.Fatalf("construct = %v, starts = %d", err, started)
	}
	if err = b.Prepare(context.Background(), nil); err == nil || started != 0 {
		t.Fatalf("missing identity = %v, starts = %d", err, started)
	}
	if err = b.Prepare(context.Background(), json.RawMessage(`{"threadId":"thread-1"}`)); err == nil || err.Error() != "native start observed" || started != 1 {
		t.Fatalf("first tool = %v, starts = %d", err, started)
	}
}

func TestPeerPrepareUsesMetadataAndRefreshesTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(host.SocketEnv, path)
	events := make(chan map[string]any, 4)
	go servePeerBus(listener, events)
	b, app := testPeer(t)
	titles := make(chan string, 2)
	titles <- "first"
	titles <- "second"
	go func() {
		decoder := json.NewDecoder(app)
		for {
			var request appRequest
			if decoder.Decode(&request) != nil {
				return
			}
			title := <-titles
			writeApp(t, app, map[string]any{"id": request.ID, "result": map[string]any{"thread": map[string]any{"id": "thread-1", "name": title, "cwd": "/work", "status": map[string]string{"type": "idle"}}}})
		}
	}()
	meta := json.RawMessage(`{"threadId":"thread-1"}`)
	if err = b.Prepare(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	hello := <-events
	if hello["method"] != "session.hello" {
		t.Fatalf("hello = %#v", hello)
	}
	params := hello["params"].(map[string]any)
	if params["session_id"] != "thread-1" || params["name"] != "first" || len(params["groups"].([]any)) != 0 {
		t.Fatalf("identity = %#v", params)
	}
	if err = b.Prepare(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	rehello := <-events
	if rehello["method"] != "session.hello" || rehello["params"].(map[string]any)["name"] != "second" {
		t.Fatalf("rehello = %#v", rehello)
	}
	if _, err = b.Call(context.Background(), "session.list", sessionkit.SessionListRequest{}); err != nil {
		t.Fatal(err)
	}
	if call := <-events; call["method"] != "session.list" {
		t.Fatalf("call = %#v", call)
	}
	b.Shutdown()
}

func TestPeerPrepareRejectsMissingOrFixedIdentityChange(t *testing.T) {
	b := &PeerBackend{groups: []string{}}
	if err := b.Prepare(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "identity is unavailable") {
		t.Fatalf("missing = %v", err)
	}
	b.identity.SessionID = "thread-1"
	b.fixedID = "thread-1"
	if err := b.Prepare(context.Background(), json.RawMessage(`{"threadId":"thread-2"}`)); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed = %v", err)
	}
}

func TestPeerPrepareReplacesAfterClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(host.SocketEnv, path)
	events := make(chan map[string]any, 2)
	go servePeerBus(listener, events)
	b, app := testPeer(t)
	go func() {
		decoder := json.NewDecoder(app)
		for {
			var request appRequest
			if decoder.Decode(&request) != nil {
				return
			}
			var params struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			writeApp(t, app, map[string]any{"id": request.ID, "result": map[string]any{"thread": map[string]any{"id": params.ThreadID, "name": params.ThreadID, "cwd": "/work"}}})
		}
	}()
	if err = b.Prepare(context.Background(), json.RawMessage(`{"threadId":"thread-1"}`)); err != nil {
		t.Fatal(err)
	}
	if hello := <-events; hello["params"].(map[string]any)["session_id"] != "thread-1" {
		t.Fatalf("hello = %#v", hello)
	}
	if err = b.Prepare(context.Background(), json.RawMessage(`{"threadId":"thread-2"}`)); err != nil {
		t.Fatal(err)
	}
	if replacement := <-events; replacement["params"].(map[string]any)["session_id"] != "thread-2" {
		t.Fatalf("replacement = %#v", replacement)
	}
	b.Shutdown()
}

func TestPeerDeliveryActiveAndIdle(t *testing.T) {
	for _, status := range []string{"active", "idle"} {
		t.Run(status, func(t *testing.T) {
			b, app := testPeer(t)
			b.identity.SessionID = "replacement-thread"
			done := make(chan struct{})
			go func() {
				defer close(done)
				decoder := json.NewDecoder(app)
				var request appRequest
				_ = decoder.Decode(&request)
				if !strings.Contains(string(request.Params), `"threadId":"thread-1"`) {
					t.Errorf("admitted identity = %s", request.Params)
					return
				}
				writeApp(t, app, map[string]any{"id": request.ID, "result": map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]string{"type": status}}}})
				_ = decoder.Decode(&request)
				if status == "active" {
					if request.Method != "thread/turns/list" {
						t.Errorf("method = %s", request.Method)
						return
					}
					writeApp(t, app, map[string]any{"id": request.ID, "result": map[string]any{"data": []any{map[string]string{"id": "turn-1", "status": "inProgress"}}}})
					_ = decoder.Decode(&request)
					if request.Method != "turn/steer" || !strings.Contains(string(request.Params), "[agentbus-metadata:") {
						t.Errorf("steer = %#v", request)
						return
					}
					writeApp(t, app, map[string]any{"id": request.ID, "result": map[string]string{"turnId": "turn-1"}})
				} else {
					if request.Method != "turn/start" || !strings.Contains(string(request.Params), "[agentbus-metadata:") {
						t.Errorf("start = %#v", request)
						return
					}
					writeApp(t, app, map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-2"}}})
				}
			}()
			receipt, err := b.deliver(context.Background(), sessionkit.PeerIdentity{SessionID: "thread-1"}, sessionkit.DeliveryRequest{MessageID: "message-1", From: sessionkit.DeliverySource{SessionID: "sender", Product: "example", Groups: []string{}}, Body: "hello"})
			if err != nil || receipt.Disposition != "injected" {
				t.Fatalf("receipt = %#v, %v", receipt, err)
			}
			<-done
		})
	}
}

func TestPeerDeliveryRejectsUnverifiedThreadState(t *testing.T) {
	for _, test := range []struct {
		name      string
		responses []map[string]any
		calls     int64
	}{
		{"wrong read id", []map[string]any{{"thread": map[string]any{"id": "thread-2", "status": map[string]string{"type": "idle"}}}}, 1},
		{"malformed status", []map[string]any{{"thread": map[string]any{"id": "thread-1", "status": map[string]string{"type": "mystery"}}}}, 1},
		{"wrong resume id", []map[string]any{
			{"thread": map[string]any{"id": "thread-1", "status": map[string]string{"type": "notLoaded"}}},
			{"thread": map[string]any{"id": "thread-2", "status": map[string]string{"type": "idle"}}},
		}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, app := testPeer(t)
			b.identity.SessionID = "thread-1"
			go func() {
				decoder := json.NewDecoder(app)
				for _, response := range test.responses {
					var request appRequest
					if decoder.Decode(&request) != nil {
						return
					}
					writeApp(t, app, map[string]any{"id": request.ID, "result": response})
				}
			}()
			_, err := b.deliver(context.Background(), sessionkit.PeerIdentity{SessionID: "thread-1"}, sessionkit.DeliveryRequest{MessageID: "message-1", From: sessionkit.DeliverySource{SessionID: "sender", Product: "example", Groups: []string{}}, Body: "hello"})
			if err == nil {
				t.Fatal("delivery succeeded")
			}
			b.app.mu.Lock()
			calls := b.app.next
			b.app.mu.Unlock()
			if calls != test.calls {
				t.Fatalf("calls = %d", calls)
			}
		})
	}
}

func TestPeerDeliveryRejectsCanceledIdentity(t *testing.T) {
	b, _ := testPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err := b.deliver(ctx, sessionkit.PeerIdentity{SessionID: "old-thread"}, sessionkit.DeliveryRequest{MessageID: "message-1", From: sessionkit.DeliverySource{SessionID: "sender", Product: "example", Groups: []string{}}, Body: "hello"})
	if err != nil || receipt.Disposition != "rejected" || receipt.Reason != "closing" || b.app.next != 0 {
		t.Fatalf("receipt = %#v, calls = %d, error = %v", receipt, b.app.next, err)
	}
}

func testPeer(t *testing.T) (*PeerBackend, net.Conn) {
	client, server := net.Pipe()
	b := &PeerBackend{groups: []string{}}
	b.app, b.caller = newAppClient(client, client, nil, func(error) {}), sessionkit.NewCaller(b.Call)
	t.Cleanup(func() { _ = server.Close() })
	return b, server
}

func servePeerBus(listener net.Listener, events chan<- map[string]any) {
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	defer connection.Close()
	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		events <- request
		result := `{}`
		if request["method"] == "session.list" {
			result = `{"sessions":[]}`
		}
		_, _ = fmt.Fprintf(connection, "{\"jsonrpc\":\"2.0\",\"id\":%v,\"result\":%s}\n", request["id"], result)
	}
}
