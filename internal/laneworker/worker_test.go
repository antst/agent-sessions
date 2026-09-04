package laneworker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type testWorkerBody struct {
	terminal    chan productruntime.NativeTerminal
	interrupted chan struct{}
	archived    chan struct{}
	closed      chan struct{}
	archiveOnce sync.Once
	closeOnce   sync.Once
}

func newTestWorkerBody() *testWorkerBody {
	return &testWorkerBody{
		terminal:    make(chan productruntime.NativeTerminal, 2),
		interrupted: make(chan struct{}, 1),
		archived:    make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (*testWorkerBody) Open(context.Context, productruntime.LaneOpenRequest) (string, error) {
	return "native-1", nil
}

func (*testWorkerBody) StartTurn(_ context.Context, request productruntime.LaneTurnStartRequest) (string, error) {
	return "native-" + request.InputID, nil
}

func (body *testWorkerBody) WaitTurn(ctx context.Context) (productruntime.NativeTerminal, error) {
	select {
	case terminal := <-body.terminal:
		return terminal, nil
	case <-ctx.Done():
		return productruntime.NativeTerminal{}, ctx.Err()
	}
}

func (body *testWorkerBody) Interrupt(context.Context) error {
	body.interrupted <- struct{}{}
	return nil
}

func (body *testWorkerBody) Archive(context.Context) error {
	body.archiveOnce.Do(func() { close(body.archived) })
	return nil
}

func (body *testWorkerBody) Close(context.Context) error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}

func TestWorkerServesOneCollectedTurnAndArchive(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body := newTestWorkerBody()
	config := WorkerConfig{
		Product: "example",
		Readiness: func(context.Context) productruntime.LaneReadiness {
			return productruntime.LaneReadiness{Available: true, NativePath: "/bin/example", NativeVersion: "1"}
		},
		Body:   body,
		Schema: testWorkerSchema(t),
		Dial:   func(context.Context, string, string) (net.Conn, error) { return client, nil },
	}
	result := make(chan error, 1)
	go func() { result <- run(context.Background(), testWorkerToken(t), config) }()
	daemon := livepresence.NewConnection(server)

	hello := readWorkerFrame(t, daemon)
	if hello.Method != "lane.worker.hello" || string(hello.ID) != "1" {
		t.Fatalf("hello = %#v", hello)
	}
	writeWorkerFrame(t, daemon, livepresence.Success(hello.ID, json.RawMessage(`{}`)))

	open := productruntime.LaneOpenRequest{
		Name: "lane", Cwd: "/work", PermissionMode: permissionmode.Default,
		Resume: false, AutoArchiveAfterSeconds: 0, Arguments: []string{},
	}
	writeWorkerRequest(t, daemon, 2, "lane.session.open", open)
	assertWorkerResult(t, readWorkerFrame(t, daemon), 2, `{"native_id":"native-1"}`)

	start := productruntime.LaneTurnStartRequest{InputID: "input-1", Body: "hello", Mode: "followup"}
	writeWorkerRequest(t, daemon, 3, "lane.turn.start", start)
	assertWorkerUpdate(t, daemon, "running", "native-input-1", "")
	assertWorkerResult(t, readWorkerFrame(t, daemon), 3, `{"native_message_id":"native-input-1"}`)

	writeWorkerRequest(t, daemon, 4, "lane.turn.wait", map[string]string{"native_message_id": "native-input-1"})
	body.terminal <- productruntime.NativeTerminal{
		Outcome: productruntime.TurnCompleted, Result: "done", NativeStopReason: `{"kind":"completed"}`,
	}
	assertWorkerUpdate(t, daemon, "terminal", "native-input-1", "completed")
	assertWorkerResult(t, readWorkerFrame(t, daemon), 4, `{"outcome":"completed","result":"done","reason":{"kind":"completed"}}`)
	assertWorkerUpdate(t, daemon, "idle", "native-input-1", "completed")

	start.InputID, start.Body = "input-2", "interrupt me"
	writeWorkerRequest(t, daemon, 5, "lane.turn.start", start)
	assertWorkerUpdate(t, daemon, "running", "native-input-2", "")
	assertWorkerResult(t, readWorkerFrame(t, daemon), 5, `{"native_message_id":"native-input-2"}`)
	writeWorkerRequest(t, daemon, 6, "lane.turn.wait", map[string]string{"native_message_id": "native-input-2"})
	writeWorkerRequest(t, daemon, 7, "lane.turn.interrupt", struct{}{})
	assertWorkerUpdate(t, daemon, "interrupting", "native-input-2", "")
	assertWorkerResult(t, readWorkerFrame(t, daemon), 7, `{}`)
	select {
	case <-body.interrupted:
	default:
		t.Fatal("native turn was not interrupted")
	}
	body.terminal <- productruntime.NativeTerminal{Outcome: productruntime.TurnInterrupted, Result: "stopped"}
	assertWorkerUpdate(t, daemon, "terminal", "native-input-2", "interrupted")
	assertWorkerResult(t, readWorkerFrame(t, daemon), 6, `{"outcome":"interrupted","result":"stopped","reason":null}`)
	assertWorkerUpdate(t, daemon, "idle", "native-input-2", "interrupted")

	writeWorkerRequest(t, daemon, 8, "lane.session.archive", struct{}{})
	assertWorkerUpdate(t, daemon, "archived", "native-input-2", "interrupted")
	assertWorkerResult(t, readWorkerFrame(t, daemon), 8, `{}`)
	if err := <-result; err != nil {
		t.Fatalf("worker exit = %v", err)
	}
	select {
	case <-body.archived:
	default:
		t.Fatal("native session was not archived")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("native session was not closed")
	}
}

func TestWorkerSessionRetainsTerminalUntilIdleUpdateAcknowledged(t *testing.T) {
	body := newTestWorkerBody()
	updates := make(chan productruntime.LaneStatusProjection, 8)
	rejectIdle := true
	session := productruntime.NewLaneWorkerSession(
		context.Background(), body, productruntime.LaneCapabilitySet{},
		func(update productruntime.LaneStatusProjection) error {
			updates <- update
			if update.State == "idle" && update.TurnID != "" && rejectIdle {
				rejectIdle = false
				return errors.New("collector disappeared")
			}
			return nil
		},
		func() {},
	)
	if _, err := session.Open(productruntime.LaneOpenRequest{Name: "lane"}); err != nil {
		t.Fatal(err)
	}
	started, err := session.Start(productruntime.LaneTurnStartRequest{InputID: "one", Body: "body", Mode: "followup"})
	if err != nil {
		t.Fatal(err)
	}
	<-updates // running
	body.terminal <- productruntime.NativeTerminal{Outcome: productruntime.TurnCompleted, Result: "kept"}
	<-updates // terminal
	waited, collected, err := session.Wait(context.Background(), started.NativeMessageID)
	if err != nil || waited.Result != "kept" {
		t.Fatalf("first wait = %#v, %v", waited, err)
	}
	if err := collected(); err == nil {
		t.Fatal("rejected idle update completed collection")
	}
	waited, collected, err = session.Wait(context.Background(), started.NativeMessageID)
	if err != nil || waited.Result != "kept" {
		t.Fatalf("second wait = %#v, %v", waited, err)
	}
	if err := collected(); err != nil {
		t.Fatal(err)
	}
}

func testWorkerSchema(t *testing.T) *productruntime.LaneWireSchema {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "integrations", "shared", "lane-worker.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := productruntime.ParseLaneWireSchema(body)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func testWorkerToken(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(tokenPayload{Endpoint: "/tmp/agent-sessions.sock", Nonce: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func writeWorkerRequest(t *testing.T, connection *livepresence.Connection, id int, method string, params any) {
	t.Helper()
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkerFrame(t, connection, livepresence.Frame{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: body})
}

func writeWorkerFrame(t *testing.T, connection *livepresence.Connection, frame livepresence.Frame) {
	t.Helper()
	if err := connection.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func readWorkerFrame(t *testing.T, connection *livepresence.Connection) livepresence.Frame {
	t.Helper()
	var frame livepresence.Frame
	if err := connection.DecodeWire(&frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func assertWorkerResult(t *testing.T, frame livepresence.Frame, id int, expected string) {
	t.Helper()
	if string(frame.ID) != strconv.Itoa(id) || string(frame.Result) != expected {
		t.Fatalf("response = %#v, want id=%d result=%s", frame, id, expected)
	}
}

func assertWorkerUpdate(t *testing.T, connection *livepresence.Connection, state, turnID, outcome string) {
	t.Helper()
	frame := readWorkerFrame(t, connection)
	var projection productruntime.LaneStatusProjection
	if frame.Method != "session.update" || productruntime.DecodeClosed(frame.Params, &projection) != nil ||
		projection.State != state || projection.TurnID != turnID || projection.Outcome != outcome {
		t.Fatalf("status update = %#v, projection=%#v", frame, projection)
	}
	writeWorkerFrame(t, connection, livepresence.Success(frame.ID, json.RawMessage(`{}`)))
}
