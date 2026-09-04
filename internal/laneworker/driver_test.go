package laneworker

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
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
	authority.mu.Lock()
	remaining := len(authority.tokens)
	authority.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired token table size = %d", remaining)
	}
}

func TestAuthorityLinearizesWaitCancellationAgainstHelloAck(t *testing.T) {
	authority := testAuthority(t)
	registration, err := authority.Register(PurposeLaunch, "codex", "lane-1", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close()
	accepted := make(chan error, 1)
	go func() {
		_, err := authority.Accept(livepresence.NewConnection(server), testHello(registration, "codex"))
		accepted <- err
	}()
	for {
		authority.mu.Lock()
		accepting := registration.state == registrationAccepting && authority.tokens[registration.token] == nil
		authority.mu.Unlock()
		if accepting {
			break
		}
		runtime.Gosched()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if binding, err := registration.Wait(ctx); err == nil || binding != nil {
		t.Fatalf("crossed wait = %p, %v", binding, err)
	}
	if err := <-accepted; err == nil {
		t.Fatal("accept published a binding after wait cancellation")
	}
	select {
	case <-registration.binding.Done():
	default:
		t.Fatal("canceled provisional binding remained live")
	}
	server, replay := net.Pipe()
	if _, err := authority.Accept(livepresence.NewConnection(server), testHello(registration, "codex")); err == nil {
		t.Fatal("canceled token was replayed")
	}
	_ = server.Close()
	_ = replay.Close()
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

func TestBindingRejectsMalformedFramesBeforeRouting(t *testing.T) {
	t.Run("malformed response", func(t *testing.T) {
		binding, worker, served := openServedBinding(t, nil)
		pending := make(chan error, 1)
		go func() {
			pending <- binding.Call(context.Background(), "lane.turn.wait", map[string]any{}, nil, func(json.RawMessage) bool { return true })
		}()
		var request livepresence.Frame
		if err := worker.DecodeWire(&request); err != nil {
			t.Fatal(err)
		}
		if err := worker.Write(livepresence.Frame{JSONRPC: "2.0", ID: request.ID, Method: "invalid", Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := <-served; err == nil {
			t.Fatal("malformed response did not terminate binding")
		}
		if err := <-pending; err == nil {
			t.Fatal("malformed response satisfied pending call")
		}
	})

	t.Run("string update id", func(t *testing.T) {
		updates := make(chan productruntime.LaneStatusProjection, 1)
		binding, worker, served := openServedBinding(t, func(update productruntime.LaneStatusProjection) error { updates <- update; return nil })
		projection, _ := json.Marshal(productruntime.LaneStatusProjection{Name: "lane", State: "idle"})
		if err := worker.Write(livepresence.Frame{JSONRPC: "2.0", ID: json.RawMessage(`"update"`), Method: "session.update", Params: projection}); err != nil {
			t.Fatal(err)
		}
		if err := <-served; err == nil {
			t.Fatal("string-id update did not terminate binding")
		}
		select {
		case update := <-updates:
			t.Fatalf("string-id update reached callback: %#v", update)
		default:
		}
		_ = binding.Close()
	})
}

func TestBindingCloseFailsPendingCallAndClosesDone(t *testing.T) {
	binding, worker, served := openServedBinding(t, nil)
	pending := make(chan error, 1)
	go func() {
		pending <- binding.Call(context.Background(), "lane.turn.wait", map[string]any{}, nil, func(json.RawMessage) bool { return true })
	}()
	var request livepresence.Frame
	if err := worker.DecodeWire(&request); err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-pending; err == nil {
		t.Fatal("close did not fail pending call")
	}
	if err := <-served; err == nil {
		t.Fatal("serve returned success after close")
	}
	select {
	case <-binding.Done():
	default:
		t.Fatal("close left Done open")
	}
	if err := binding.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestLaneStatusProjectionRelations(t *testing.T) {
	valid := []productruntime.LaneStatusProjection{
		{Name: "lane", State: "idle"},
		{Name: "lane", State: "idle", TurnID: "turn", Outcome: "completed", AutoArchiveAt: 1},
		{Name: "lane", State: "running", TurnID: "turn"},
		{Name: "lane", State: "interrupting", TurnID: "turn"},
		{Name: "lane", State: "terminal", TurnID: "turn", Outcome: "timed_out"},
		{Name: "lane", State: "archived"},
		{Name: "lane", State: "archived", TurnID: "turn", Outcome: "failed"},
	}
	invalid := []productruntime.LaneStatusProjection{
		{State: "idle"},
		{Name: "lane", State: "unknown"},
		{Name: "lane", State: "running"},
		{Name: "lane", State: "running", TurnID: "turn", Outcome: "completed"},
		{Name: "lane", State: "interrupting", TurnID: "turn", AutoArchiveAt: 1},
		{Name: "lane", State: "terminal", TurnID: "turn"},
		{Name: "lane", State: "terminal", TurnID: "turn", Outcome: "completed", AutoArchiveAt: 1},
		{Name: "lane", State: "idle", TurnID: "turn"},
		{Name: "lane", State: "idle", Outcome: "completed"},
		{Name: "lane", State: "archived", Outcome: "failed"},
		{Name: "lane", State: "archived", AutoArchiveAt: 1},
	}
	for _, projection := range valid {
		if !projection.Valid() {
			t.Fatalf("valid projection rejected: %#v", projection)
		}
	}
	for _, projection := range invalid {
		if projection.Valid() {
			t.Fatalf("invalid projection accepted: %#v", projection)
		}
	}
}

func openServedBinding(t *testing.T, update func(productruntime.LaneStatusProjection) error) (*Binding, *livepresence.Connection, <-chan error) {
	t.Helper()
	authority := testAuthority(t)
	registration, err := authority.Register(PurposeLaunch, "codex", "lane-1", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding, worker := acceptHello(t, authority, testHello(registration, "codex"))
	served := make(chan error, 1)
	go func() { served <- binding.Serve(context.Background(), update) }()
	return binding, worker, served
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
