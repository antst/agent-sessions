package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

func TestPeerUsesEnvironmentThenReplacesObservedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", path)
	must(t, err)
	defer listener.Close()
	t.Setenv(host.SocketEnv, path)
	t.Setenv(host.SessionIDEnv, "initial")
	t.Setenv(host.NameEnv, "launch-title")
	t.Setenv(host.GroupsEnv, `["team"]`)
	parent := os.Getppid()
	payload := `[{"sessionId":"replacement","name":"new-title","kind":"interactive","cwd":"/new","pid":` + fmt.Sprint(parent) + `}]`
	oldReadAgents := readAgents
	readAgents = func(context.Context) ([]byte, error) { return []byte(payload), nil }
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
	replaced, call := <-events, <-events
	check(t, replaced["method"] == "session.hello" && replaced["params"].(map[string]any)["session_id"] == "replacement", "replacement = %#v", replaced)
	check(t, call["method"] == "session.list", "call = %#v", call)
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
