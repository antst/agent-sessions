package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/wrappers/host"
)

func TestInteractivePlan(t *testing.T) {
	plan, err := InteractivePlan([]string{"--model", "-g", "--group", "team", "prompt"}, []string{"PATH=/bin"})
	must(t, err)
	id := environment(plan.Env, host.SessionIDEnv)
	check(t, len(id) == 36 && id[14] == '4', "session id = %q", id)
	check(t, environment(plan.Env, host.NameEnv) == id, "name differs from id")
	check(t, environment(plan.Env, host.GroupsEnv) == `["team"]`, "groups = %q", environment(plan.Env, host.GroupsEnv))
	check(t, slices.Contains(plan.Args, "--leader") && slices.Contains(plan.Args, "--session-id"), "managed args = %#v", plan.Args)
	model := slices.Index(plan.Args, "--model")
	check(t, model >= 0 && plan.Args[model+1] == "-g", "native option value was consumed as a group: %#v", plan.Args)

	plan, err = InteractivePlan([]string{"--resume", testSessionID}, nil)
	must(t, err)
	check(t, environment(plan.Env, host.SessionIDEnv) == testSessionID && !slices.Contains(plan.Args, "--session-id"), "resume = %#v / %#v", plan.Env, plan.Args)
	_, err = InteractivePlan([]string{"--no-leader"}, nil)
	check(t, err != nil && err.Error() == "grok-peer requires the default leader", "no-leader error = %v", err)
	plan, err = InteractivePlan([]string{"--", "prompt"}, nil)
	must(t, err)
	separator := slices.Index(plan.Args, "--")
	check(t, separator > 0 && slices.Index(plan.Args, "--leader") < separator, "leader follows separator: %#v", plan.Args)
}

func TestPeerMCPUsesRosterTitleAndDefaultLeader(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "agentbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.SessionIDEnv, testSessionID)
	t.Setenv(host.NameEnv, testSessionID)
	t.Setenv(host.GroupsEnv, `["peer-group"]`)
	t.Setenv("GROK_TEST_RETITLE", "product-title")
	backend, err := NewPeerBackend(context.Background())
	must(t, err)
	select {
	case <-backend.peer.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not hello")
	}
	first := <-hellos
	check(t, first.SessionID == testSessionID && first.Name == testSessionID && slices.Equal(first.Groups, []string{"peer-group"}), "first hello = %#v", first)
	must(t, backend.Prepare(context.Background()))
	second := <-hellos
	check(t, second.SessionID == testSessionID && second.Name == "product-title" && slices.Equal(second.Groups, first.Groups), "re-hello = %#v", second)
	receipt, err := backend.deliver(context.Background(), delivery("peer message"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "peer delivery = %#v", receipt)
	check(t, backend.Caller() != nil, "peer caller absent")
	backend.Shutdown()
}

type hello struct {
	SessionID string   `json:"session_id"`
	Name      string   `json:"name"`
	Groups    []string `json:"groups"`
}

func fakeDaemon(t *testing.T, path string) (net.Listener, <-chan hello) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	must(t, err)
	hellos := make(chan hello, 4)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		var write sync.Mutex
		for scanner.Scan() {
			var frame struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &frame) != nil {
				return
			}
			if frame.Method == "session.hello" {
				var value hello
				_ = json.Unmarshal(frame.Params, &value)
				hellos <- value
			}
			write.Lock()
			_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
			write.Unlock()
		}
	}()
	return listener, hellos
}

func environment(values []string, name string) string {
	for _, value := range values {
		if key, body, found := strings.Cut(value, "="); found && key == name {
			return body
		}
	}
	return ""
}
