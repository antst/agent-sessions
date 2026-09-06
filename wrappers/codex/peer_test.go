package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

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
			b.identity.SessionID = "thread-1"
			done := make(chan struct{})
			go func() {
				defer close(done)
				decoder := json.NewDecoder(app)
				var request appRequest
				_ = decoder.Decode(&request)
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
			receipt, err := b.deliver(context.Background(), sessionkit.DeliveryRequest{MessageID: "message-1", From: sessionkit.DeliverySource{SessionID: "sender", Product: "example", Groups: []string{}}, Body: "hello"})
			if err != nil || receipt.Disposition != "injected" {
				t.Fatalf("receipt = %#v, %v", receipt, err)
			}
			<-done
		})
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
