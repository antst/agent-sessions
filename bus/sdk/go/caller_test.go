package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/antst/sessionbus/bus/internal/protocol"
	"github.com/antst/sessionbus/bus/internal/rpc"
)

func TestCallerMapsWireMethods(t *testing.T) {
	var seen []struct {
		method string
		params any
	}
	call := func(_ context.Context, method string, params, result any) error {
		seen = append(seen, struct {
			method string
			params any
		}{method, params})
		results := map[string]any{
			"session.list":   SessionListResult{Sessions: []SessionSummary{}},
			"message.send":   MessageSendResult{MessageID: "message", Deliveries: []protocol.MessageSendDelivery{}},
			"lane.describe":  LaneDescribeResult{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}},
			"lane.spawn":     LaneSpawnResult{SessionID: "lane@local"},
			"turn.run":       TurnResult{Outcome: "completed", Result: "done"},
			"turn.interrupt": struct{}{},
			"session.close":  struct{}{},
		}
		raw, _ := json.Marshal(results[method])
		return json.Unmarshal(raw, result)
	}
	c := newCaller(call)
	ctx := context.Background()
	table := []struct {
		method string
		params any
		call   func() error
	}{
		{"session.list", SessionListRequest{}, func() error { _, err := c.List(ctx, SessionListRequest{}); return err }},
		{"message.send", MessageSendRequest{Target: "lane", Message: "hello"}, func() error { _, err := c.Send(ctx, MessageSendRequest{Target: "lane", Message: "hello"}); return err }},
		{"lane.describe", LaneDescribeRequest{Product: "example-peer"}, func() error { _, err := c.Describe(ctx, LaneDescribeRequest{Product: "example-peer"}); return err }},
		{"lane.spawn", LaneSpawnRequest{Name: "child", Product: "example-peer", Open: &OpenOptions{}}, func() error {
			_, err := c.Spawn(ctx, LaneSpawnRequest{Name: "child", Product: "example-peer", Open: &OpenOptions{}})
			return err
		}},
		{"lane.spawn", LaneSpawnRequest{ResumeSessionID: "lane@local"}, func() error { _, err := c.Resume(ctx, "lane@local"); return err }},
		{"turn.run", TurnRunRequest{SessionID: "lane@local", Input: "work"}, func() error { _, err := c.Run(ctx, TurnRunRequest{SessionID: "lane@local", Input: "work"}); return err }},
		{"turn.interrupt", SessionTarget{SessionID: "lane@local"}, func() error { return c.Interrupt(ctx, SessionTarget{SessionID: "lane@local"}) }},
		{"session.close", SessionCloseRequest{SessionID: "lane@local", Forget: true}, func() error { return c.Close(ctx, SessionCloseRequest{SessionID: "lane@local", Forget: true}) }},
	}
	for index, test := range table {
		if err := test.call(); err != nil || seen[index].method != test.method || !reflect.DeepEqual(seen[index].params, test.params) {
			t.Fatalf("call %d: got %#v, err %v", index, seen[index], err)
		}
	}
}

func TestCallerSchemaErrorNamesPathAndConstraint(t *testing.T) {
	called := false
	caller := NewCaller(func(context.Context, string, any) (json.RawMessage, error) {
		called = true
		return nil, nil
	})
	_, err := caller.Spawn(context.Background(), LaneSpawnRequest{Name: "child", ResumeSessionID: "lane@local"})
	want := `LaneSpawnRequest: "name" is not allowed with "resume_session_id"`
	if err == nil || err.Error() != want || called {
		t.Fatalf("error = %v, called = %t", err, called)
	}
}

func TestCallerSugarMatchesJavaScriptShapes(t *testing.T) {
	fixtures := loadCallerFixtures(t)
	type terminal struct {
		result TurnResult
		err    error
	}
	terminals := make(chan terminal)
	c := newCaller(func(_ context.Context, _ string, _ any, result any) error {
		terminal := <-terminals
		if terminal.err == nil {
			*result.(*TurnResult) = terminal.result
		}
		return terminal.err
	})
	sequence := fixtures.Sequences.TargetRelease
	request := TurnRunRequest{SessionID: sequence.SessionID, Input: sequence.FirstInput}
	first, _ := c.Start(request)
	assertJSON(t, first, fixtures.Shapes.Start.Result)
	if _, err := c.Start(request); !isCode(err, protocol.Busy) {
		t.Fatalf("same-target start error = %v", err)
	}
	zero := int64(0)
	running, err := c.Wait(WaitRequest{TurnID: first.TurnID, TimeoutMS: &zero})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, running, fixtures.Shapes.Running.Result)
	c.mu.Lock()
	firstDone := c.runs[first.TurnID].done
	c.mu.Unlock()
	terminals <- terminal{result: fixtures.Shapes.Done.Terminal}
	<-firstDone
	second, err := c.Start(TurnRunRequest{SessionID: sequence.SessionID, Input: sequence.SecondInput})
	if err != nil || first.TurnID != sequence.FirstTurnID || second.TurnID != sequence.SecondTurnID {
		t.Fatalf("target release = %#v %#v, %v", first, second, err)
	}
	done, err := c.Status(StatusRequest{TurnID: first.TurnID})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, done, fixtures.Shapes.Done.Result)
	if _, err = c.Status(StatusRequest{TurnID: first.TurnID}); !errors.Is(err, ErrUnknownTurn) {
		t.Fatalf("collected status error = %v", err)
	}
	terminals <- terminal{result: fixtures.Shapes.Done.Terminal}
	_, _ = c.Wait(WaitRequest{TurnID: second.TurnID})

	for _, failure := range []struct {
		err  error
		want TurnStatus
	}{{rpc.ErrClosed, fixtures.Shapes.EOF.Result}, {&ProtocolError{Code: fixtures.Shapes.WireError.Code, Message: fixtures.Shapes.WireError.Message}, fixtures.Shapes.WireError.Result}} {
		caller := newCaller(func(context.Context, string, any, any) error { return failure.err })
		started, _ := caller.Start(fixtures.Shapes.Start.Request)
		unavailable, waitErr := caller.Wait(WaitRequest{TurnID: started.TurnID})
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		assertJSON(t, unavailable, failure.want)
	}
	for _, invalid := range fixtures.Sequences.InvalidLocalRequests {
		if err := runInvalidLocal(c, invalid.Operation, invalid.Request); err == nil || err.Error() != invalid.Error {
			t.Fatalf("%s error = %v", invalid.Operation, err)
		}
	}
}

func TestDialIsOneShotFramedClient(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("SESSIONBUS_SOCKET", "")
	path := filepath.Join(root, "sessionbus", "run", "presence.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if got := Socket(); got != path {
		t.Fatalf("socket = %q, want %q", got, path)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		fd, _ := listener.Accept()
		requests := make(chan *rpc.Request, 1)
		server := rpc.New(fd, false, func(_ context.Context, request *rpc.Request) { requests <- request })
		_ = server.Result(<-requests, SessionListResult{Sessions: []SessionSummary{}})
	}()
	client, err := Dial("")
	if err != nil {
		t.Fatal(err)
	}
	caller := NewCaller(client.Call)
	listed, err := caller.List(context.Background(), SessionListRequest{})
	if err != nil || len(listed.Sessions) != 0 {
		t.Fatalf("result = %#v, err %v", listed, err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertJSON(t *testing.T, value, want any) {
	t.Helper()
	raw, err := json.Marshal(value)
	expected, wantErr := json.Marshal(want)
	if err != nil || wantErr != nil || string(raw) != string(expected) {
		t.Fatalf("json = %s, want %s, errors %v / %v", raw, expected, err, wantErr)
	}
}

func isCode(err error, code int) bool {
	var value *ProtocolError
	return errors.As(err, &value) && value.Code == code
}

type callerFixtures struct {
	Shapes struct {
		Start     startFixture
		Running   resultFixture[TurnStatus]
		Done      doneFixture
		EOF       resultFixture[TurnStatus]
		WireError wireErrorFixture `json:"wire_error"`
	}
	Sequences struct {
		TargetRelease        targetReleaseFixture  `json:"target_release_before_collection"`
		InvalidLocalRequests []invalidLocalFixture `json:"invalid_local_requests"`
		CrossedRehello       crossedRehelloFixture `json:"crossed_rehello"`
	}
}

type startFixture struct {
	Request TurnRunRequest
	Result  StartResult
}

type resultFixture[T any] struct {
	Result T
}

type doneFixture struct {
	Terminal TurnResult
	Result   TurnStatus
}

type wireErrorFixture struct {
	Code    int
	Message string
	Result  TurnStatus
}

type targetReleaseFixture struct {
	SessionID    string `json:"session_id"`
	FirstInput   string `json:"first_input"`
	SecondInput  string `json:"second_input"`
	FirstTurnID  string `json:"first_turn_id"`
	SecondTurnID string `json:"second_turn_id"`
}

type invalidLocalFixture struct {
	Operation string
	Request   json.RawMessage
	Error     string
}

func loadCallerFixtures(t *testing.T) callerFixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "protocol", "caller-sugar.fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures callerFixtures
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func runInvalidLocal(c *Caller, operation string, raw json.RawMessage) error {
	switch operation {
	case "start":
		var request TurnRunRequest
		_ = json.Unmarshal(raw, &request)
		_, err := c.Start(request)
		return err
	case "status":
		var request StatusRequest
		_ = json.Unmarshal(raw, &request)
		_, err := c.Status(request)
		return err
	default:
		var request WaitRequest
		_ = json.Unmarshal(raw, &request)
		_, err := c.Wait(request)
		return err
	}
}
