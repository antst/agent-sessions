package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

func TestActiveSessionRequiresOneParentMatch(t *testing.T) {
	pid := os.Getpid()
	row := func(id, name string, candidate int) string {
		return `{"sessionId":"` + id + `","name":"` + name + `","kind":"interactive","cwd":"/work","pid":` + fmt.Sprint(candidate) + `}`
	}
	for _, test := range []struct {
		name, payload, want, error string
	}{
		{"match", `[` + row("id", "title", pid) + `]`, "title", ""},
		{"fallback name", `[` + row("id", "", pid) + `]`, "id", ""},
		{"wrong parent", `[` + row("id", "title", pid+1) + `]`, "", "unavailable"},
		{"ambiguous", `[` + row("a", "one", pid) + `,` + row("b", "two", pid) + `]`, "", "ambiguous"},
		{"invalid", `{`, "", "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity, err := activeSession([]byte(test.payload), pid, []string{"team"})
			check(t, identity.Name == test.want, "identity = %#v", identity)
			check(t, test.error == "" && err == nil || err != nil && strings.Contains(err.Error(), test.error), "error = %v", err)
			if err == nil {
				check(t, identity.Product == "claude" && identity.Groups[0] == "team" && identity.Info["cwd"] == "/work", "identity = %#v", identity)
			}
		})
	}
}

func TestPeerDeliveryUsesCanonicalEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.sock")
	listener, err := net.Listen("unix", path)
	must(t, err)
	defer listener.Close()
	t.Setenv(messagingSocketEnv, path)
	received := make(chan map[string]any, 1)
	go func() {
		connection, _ := listener.Accept()
		defer connection.Close()
		var frame map[string]any
		_ = json.NewDecoder(connection).Decode(&frame)
		received <- frame
	}()
	receipt, err := (&PeerBackend{}).deliver(context.Background(), delivery("hello"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "receipt = %#v", receipt)
	frame := <-received
	message := frame["message"].(map[string]any)["content"].(string)
	check(t, frame["from"] == "agentbus" && strings.Contains(message, "[agentbus-metadata:") && strings.Contains(message, "hello"), "frame = %#v", frame)
}

func TestPeerDeliveryCancelStopsBlockedWrite(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	started := make(chan struct{})
	oldDial := dialMessaging
	dialMessaging = func(context.Context, string, string) (net.Conn, error) {
		return &writeSignal{Conn: client, started: started}, nil
	}
	defer func() { dialMessaging = oldDial }()
	t.Setenv(messagingSocketEnv, "fixture")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := (&PeerBackend{}).deliver(ctx, delivery("blocked")); done <- err }()
	<-started
	cancel()
	check(t, <-done != nil, "blocked delivery succeeded after cancellation")
}

type writeSignal struct {
	net.Conn
	started chan struct{}
}

func (c *writeSignal) Write(body []byte) (int, error) { close(c.started); return c.Conn.Write(body) }

func TestPeerRehellosReplacesAndStopsOnObservationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", path)
	must(t, err)
	defer listener.Close()
	t.Setenv(host.SocketEnv, path)
	t.Setenv(host.SessionIDEnv, "initial")
	t.Setenv(host.NameEnv, "launch-title")
	t.Setenv(host.GroupsEnv, `["team"]`)
	parent := os.Getppid()
	payload := `[{"sessionId":"initial","name":"same-title","kind":"interactive","cwd":"/same","pid":` + fmt.Sprint(parent) + `}]`
	var observationError error
	oldReadAgents := readAgents
	readAgents = func(context.Context) ([]byte, error) { return []byte(payload), observationError }
	defer func() { readAgents = oldReadAgents }()
	events := make(chan map[string]any, 4)
	go servePeerFixture(t, listener, events)
	backend, err := NewPeerBackend(context.Background())
	must(t, err)
	defer backend.Shutdown()
	<-backend.peer.Ready()
	initial := <-events
	check(t, initial["method"] == "session.hello" && initial["params"].(map[string]any)["session_id"] == "initial", "initial = %#v", initial)
	must(t, backend.Prepare(context.Background()))
	result, err := backend.Call(context.Background(), "session.list", sessionkit.SessionListRequest{})
	must(t, err)
	check(t, string(result) == `{"sessions":[]}`, "result = %s", result)
	rehello, call := <-events, <-events
	params := rehello["params"].(map[string]any)
	check(t, params["session_id"] == "initial" && params["name"] == "same-title" && reflect.DeepEqual(params["groups"], []any{"team"}) && params["info"].(map[string]any)["cwd"] == "/same", "rehello = %#v", rehello)
	check(t, call["method"] == "session.list", "call = %#v", call)
	payload = `[{"sessionId":"replacement","name":"new-title","kind":"interactive","cwd":"/new","pid":` + fmt.Sprint(parent) + `}]`
	must(t, backend.Prepare(context.Background()))
	_, err = backend.Call(context.Background(), "session.list", sessionkit.SessionListRequest{})
	must(t, err)
	replaced, call := <-events, <-events
	check(t, replaced["method"] == "session.hello" && replaced["params"].(map[string]any)["session_id"] == "replacement", "replacement = %#v", replaced)
	check(t, call["method"] == "session.list", "call = %#v", call)
	observationError = os.ErrNotExist
	err = backend.Prepare(context.Background())
	check(t, err != nil && strings.Contains(err.Error(), "start Claude with claude-peer"), "observation error = %v", err)
	select {
	case event := <-events:
		t.Fatalf("caller frame after observation failure: %#v", event)
	default:
	}
}

func TestHandStartedPeerHelloUsesEmptyGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", path)
	must(t, err)
	defer listener.Close()
	for _, name := range []string{host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		t.Setenv(name, "")
	}
	t.Setenv(host.SocketEnv, path)
	parent := os.Getppid()
	oldReadAgents := readAgents
	readAgents = func(context.Context) ([]byte, error) {
		return []byte(`[{"sessionId":"observed","name":"title","kind":"interactive","cwd":"/work","pid":` + fmt.Sprint(parent) + `}]`), nil
	}
	defer func() { readAgents = oldReadAgents }()
	events := make(chan map[string]any, 1)
	go servePeerFixture(t, listener, events)
	backend, err := NewPeerBackend(context.Background())
	must(t, err)
	defer backend.Shutdown()
	<-backend.peer.Ready()
	groups := (<-events)["params"].(map[string]any)["groups"].([]any)
	check(t, groups != nil && len(groups) == 0, "groups = %#v", groups)
}

func TestUnresolvedPeerNeverConnects(t *testing.T) {
	t.Setenv(host.SessionIDEnv, "")
	oldReadAgents := readAgents
	readAgents = func(context.Context) ([]byte, error) { return nil, os.ErrNotExist }
	defer func() { readAgents = oldReadAgents }()
	backend, err := NewPeerBackend(context.Background())
	must(t, err)
	defer backend.Shutdown()
	err = backend.Prepare(context.Background())
	check(t, backend.peer == nil && err != nil && strings.Contains(err.Error(), "start Claude with claude-peer"), "backend = %#v / %v", backend, err)
}

func servePeerFixture(t *testing.T, listener net.Listener, events chan<- map[string]any) {
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
		_, _ = connection.Write([]byte(`{"jsonrpc":"2.0","id":` + fmt.Sprint(request["id"]) + `,"result":` + result + `}` + "\n"))
	}
}
