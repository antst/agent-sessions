package laneworker

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestAuthorityConsumesOnlyFirstValidHello(t *testing.T) {
	authority := testAuthority(t)
	registration, err := authority.Register(PurposeLaunch, "codex", "lane-1", 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint, err := TokenEndpoint(registration.Token().Reveal()); err != nil || endpoint != "/tmp/agent-sessions.sock" {
		t.Fatalf("token endpoint = %q, %v", endpoint, err)
	}

	wrong := testHello(registration, "grok")
	server, client := net.Pipe()
	if _, err := authority.Accept(livepresence.NewConnection(server), wrong); err == nil {
		t.Fatal("wrong-product hello consumed token")
	}
	_ = server.Close()
	_ = client.Close()

	first := testHello(registration, "codex")
	binding, clientConnection := acceptHello(t, authority, first)
	defer binding.Close()
	defer clientConnection.Close()
	if binding.Purpose != PurposeLaunch || binding.LaneID != "lane-1" || binding.Generation != 4 {
		t.Fatalf("binding = %#v", binding)
	}
	if waited, err := registration.Wait(context.Background()); err != nil || waited != binding {
		t.Fatalf("wait = %p, %v", waited, err)
	}

	server, client = net.Pipe()
	if _, err := authority.Accept(livepresence.NewConnection(server), first); err == nil {
		t.Fatal("reused token was accepted")
	}
	_ = server.Close()
	_ = client.Close()
}

func TestAuthorityRejectsExpiredTokenWithoutBinding(t *testing.T) {
	authority := testAuthority(t)
	now := time.Unix(100, 0)
	authority.now = func() time.Time { return now }
	registration, err := authority.Register(PurposeDoctor, "example", "", 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if _, err := authority.Accept(livepresence.NewConnection(server), testHello(registration, "example")); err == nil {
		t.Fatal("expired doctor token was accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := registration.Wait(ctx); err == nil {
		t.Fatal("expired registration produced a binding")
	}
}

func TestBindingRoutesCallsUpdatesAndDisconnect(t *testing.T) {
	authority := testAuthority(t)
	registration, err := authority.Register(PurposeLaunch, "codex", "lane-1", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding, worker := acceptHello(t, authority, testHello(registration, "codex"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan productruntime.LaneStatusProjection, 1)
	served := make(chan error, 1)
	go func() {
		served <- binding.Serve(ctx, func(update productruntime.LaneStatusProjection) error { updates <- update; return nil })
	}()

	workerRequest := make(chan livepresence.Frame, 1)
	go func() {
		var frame livepresence.Frame
		if err := worker.DecodeWire(&frame); err == nil {
			workerRequest <- frame
			_ = worker.Write(livepresence.Success(frame.ID, json.RawMessage(`{"native_id":"native-1"}`)))
		}
	}()
	var result struct {
		NativeID string `json:"native_id"`
	}
	if err := binding.Call(context.Background(), "lane.session.open", map[string]any{"name": "lane"}, &result, func(raw json.RawMessage) bool { return json.Valid(raw) }); err != nil || result.NativeID != "native-1" {
		t.Fatalf("open result = %#v, %v", result, err)
	}
	if frame := <-workerRequest; frame.Method != "lane.session.open" || string(frame.ID) != "1" {
		t.Fatalf("worker request = %#v", frame)
	}

	projection := productruntime.LaneStatusProjection{Name: "lane", State: "terminal", TurnID: "turn-1", Outcome: "completed"}
	body, _ := json.Marshal(projection)
	if err := worker.Write(livepresence.Frame{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "session.update", Params: body}); err != nil {
		t.Fatal(err)
	}
	var response livepresence.Frame
	if err := worker.DecodeWire(&response); err != nil || string(response.Result) != `{}` {
		t.Fatalf("update response = %#v, %v", response, err)
	}
	if update := <-updates; update.TurnID != "turn-1" || update.State != "terminal" {
		t.Fatalf("update = %#v", update)
	}

	pending := make(chan error, 1)
	go func() {
		pending <- binding.Call(context.Background(), "lane.turn.wait", map[string]any{}, nil, func(raw json.RawMessage) bool { return json.Valid(raw) })
	}()
	if err := worker.DecodeWire(&response); err != nil {
		t.Fatal(err)
	}
	_ = worker.Close()
	if err := <-pending; err == nil {
		t.Fatal("pending call survived worker EOF")
	}
	if err := <-served; err == nil {
		t.Fatal("serve returned success after worker EOF")
	}
	select {
	case <-binding.Done():
	case <-time.After(time.Second):
		t.Fatal("binding did not close")
	}
}

func testAuthority(t *testing.T) *Authority {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "integrations", "shared", "lane-worker.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := productruntime.ParseLaneWireSchema(body)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAuthority("/tmp/agent-sessions.sock", schema)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func testHello(registration *Registration, product string) livepresence.Frame {
	hello := productruntime.LaneWorkerHello{
		Protocol: 1, LaunchToken: registration.Token().Reveal(), Product: product,
		Capabilities: productruntime.LaneCapabilitySet{}, ExtraArguments: []productruntime.LaneExtraArgument{},
		Readiness: productruntime.LaneReadiness{Available: true, NativePath: "/bin/native", NativeVersion: "1"},
	}
	params, _ := json.Marshal(hello)
	return livepresence.Frame{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "lane.worker.hello", Params: params}
}

func acceptHello(t *testing.T, authority *Authority, first livepresence.Frame) (*Binding, *livepresence.Connection) {
	t.Helper()
	server, client := net.Pipe()
	accepted := make(chan struct {
		binding *Binding
		err     error
	}, 1)
	go func() {
		binding, err := authority.Accept(livepresence.NewConnection(server), first)
		accepted <- struct {
			binding *Binding
			err     error
		}{binding, err}
	}()
	worker := livepresence.NewConnection(client)
	var acknowledgement livepresence.Frame
	if err := worker.DecodeWire(&acknowledgement); err != nil || string(acknowledgement.Result) != `{}` {
		t.Fatalf("hello acknowledgement = %#v, %v", acknowledgement, err)
	}
	result := <-accepted
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.binding, worker
}
