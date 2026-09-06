package qwen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

func TestMCPInitializeDoesNotRequirePeerIdentityOrBus(t *testing.T) {
	for _, name := range []string{host.SessionIDEnv, host.NameEnv, host.GroupsEnv, InputFileEnv} {
		t.Setenv(name, "")
	}
	backend := NewPeerBackend()
	defer backend.Shutdown()
	input := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var output bytes.Buffer
	must(t, (&mcp.Server{Backend: backend}).Serve(context.Background(), input, &output))
	check(t, strings.Contains(output.String(), `"protocolVersion":"2025-06-18"`), "initialize = %s", output.String())
	check(t, backend.peer == nil, "initialize connected the peer")
	input = bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"agent_sessions","arguments":{"action":"list","arguments":{}}}}` + "\n")
	output.Reset()
	must(t, (&mcp.Server{Backend: backend}).Serve(context.Background(), input, &output))
	check(t, strings.Contains(output.String(), "Qwen peer identity is unavailable; start Qwen with qwen-peer") && backend.peer == nil, "tool error = %s", output.String())
}

func TestPeerPrepareRehellosAndDeliveryUsesInputFile(t *testing.T) {
	root, socket := t.TempDir(), filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	defer listener.Close()
	input := filepath.Join(root, "input.jsonl")
	must(t, os.WriteFile(input, nil, 0o600))
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.SessionIDEnv, fixtureID)
	t.Setenv(host.NameEnv, "initial")
	t.Setenv(host.GroupsEnv, `["team"]`)
	t.Setenv(InputFileEnv, input)
	t.Setenv("QWEN_HOME", root)
	transcript := filepath.Join(root, "projects", "p", "chats", fixtureID+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(transcript), 0o700))
	must(t, os.WriteFile(transcript, []byte(`{"sessionId":"`+fixtureID+`","type":"system","subtype":"custom_title","systemPayload":{"customTitle":"first title","titleSource":"manual"}}`+"\n"), 0o600))
	events := make(chan map[string]any, 4)
	go servePeer(t, listener, events)
	backend := NewPeerBackend()
	defer backend.Shutdown()
	must(t, backend.Prepare(context.Background(), nil))
	initial := <-events
	params := initial["params"].(map[string]any)
	check(t, params["session_id"] == fixtureID && params["name"] == "first title" && reflect.DeepEqual(params["groups"], []any{"team"}), "hello = %#v", initial)

	must(t, os.WriteFile(transcript, []byte(`{"sessionId":"`+fixtureID+`","type":"system","subtype":"custom_title","systemPayload":{"customTitle":"new title","titleSource":"manual"}}`+"\n"), 0o600))
	must(t, backend.Prepare(context.Background(), nil))
	rehello := <-events
	check(t, rehello["method"] == "session.hello" && rehello["params"].(map[string]any)["name"] == "new title", "rehello = %#v", rehello)
	receipt, err := backend.deliver(context.Background(), sessionkit.PeerIdentity{SessionID: fixtureID}, delivery("hello"))
	must(t, err)
	body := mustRead(t, input)
	check(t, receipt.Disposition == "injected" && bytes.Contains(body, []byte(`"type":"submit"`)) && bytes.Contains(body, []byte("[agentbus-metadata:")) && bytes.Contains(body, []byte("hello")), "receipt/body = %#v/%s", receipt, body)
}

func TestInteractivePlanFreshResumeAndPassthrough(t *testing.T) {
	plan, err := InteractivePlan([]string{"--model", "-g", "--group", "team", "-n", "chosen"}, []string{"PATH=/bin"})
	must(t, err)
	id, path := environmentValue(plan.Env, host.SessionIDEnv), environmentValue(plan.Env, InputFileEnv)
	defer os.Remove(path)
	check(t, len(id) == 36 && environmentValue(plan.Env, host.NameEnv) == "chosen" && environmentValue(plan.Env, host.GroupsEnv) == `["team"]`, "identity = %#v", plan.Env)
	check(t, reflect.DeepEqual(plan.Args, []string{"--model", "-g", "--chat-recording=true", "--input-file", path, "--session-id", id}), "args = %#v", plan.Args)
	check(t, string(bytes.TrimSpace(mustRead(t, path))) == `{"text":"/rename chosen","type":"submit"}`, "initial input = %s", mustRead(t, path))

	plan, err = InteractivePlan([]string{"--resume", fixtureID, "-g", "team"}, nil)
	must(t, err)
	path = environmentValue(plan.Env, InputFileEnv)
	defer os.Remove(path)
	check(t, environmentValue(plan.Env, host.SessionIDEnv) == fixtureID && !slicesContain(plan.Args, "--session-id"), "resume = %#v / %#v", plan.Args, plan.Env)
	for _, selector := range [][]string{{"--resume", "reviewer"}, {"--resume"}} {
		_, err = InteractivePlan(selector, nil)
		check(t, err != nil && strings.Contains(err.Error(), "requires an exact Qwen session UUID"), "selector %q error = %v", selector, err)
	}

	original := []string{"mcp", "list", "-g"}
	plan, err = InteractivePlan(original, []string{"ONLY=original", mcp.LaneSocketEnv + "=stale", InputFileEnv + "=stale"})
	must(t, err)
	check(t, reflect.DeepEqual(plan.Args, original) && reflect.DeepEqual(plan.Env, []string{"ONLY=original", mcp.LaneSocketEnv + "=stale", InputFileEnv + "=stale"}), "passthrough = %#v", plan)
}

func TestCancelledInitialPrepareKeepsResidentPeer(t *testing.T) {
	root, socket := t.TempDir(), filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	defer listener.Close()
	input := filepath.Join(root, "input.jsonl")
	must(t, os.WriteFile(input, nil, 0o600))
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.SessionIDEnv, fixtureID)
	t.Setenv(host.NameEnv, "name")
	t.Setenv(InputFileEnv, input)
	hellos, release := make(chan map[string]any, 1), make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewScanner(connection)
		if reader.Scan() {
			var request map[string]any
			_ = json.Unmarshal(reader.Bytes(), &request)
			hellos <- request
			<-release
			_, _ = fmt.Fprintf(connection, `{"jsonrpc":"2.0","id":%v,"result":{}}`+"\n", request["id"])
		}
		for reader.Scan() {
		}
	}()
	backend := NewPeerBackend()
	caller := backend.caller
	ctx, cancel := context.WithCancel(context.Background())
	prepared := make(chan error, 1)
	go func() { prepared <- backend.Prepare(ctx, nil) }()
	<-hellos
	backend.mu.Lock()
	peer := backend.peer
	backend.mu.Unlock()
	cancel()
	check(t, errors.Is(<-prepared, context.Canceled), "cancelled prepare did not fail")
	prepared = make(chan error, 1)
	go func() { prepared <- backend.Prepare(context.Background(), nil) }()
	close(release)
	must(t, <-prepared)
	check(t, backend.peer == peer && backend.caller == caller, "prepare replaced its resident peer or caller")
	backend.Shutdown()
	err = backend.Prepare(context.Background(), nil)
	check(t, err != nil && err.Error() == "Qwen peer is closed", "prepare after shutdown = %v", err)
}

func servePeer(t *testing.T, listener net.Listener, events chan<- map[string]any) {
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
		_, _ = fmt.Fprintf(connection, `{"jsonrpc":"2.0","id":%v,"result":{}}`+"\n", request["id"])
	}
}

func slicesContain(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
