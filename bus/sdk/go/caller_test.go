package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

func TestCallerMapsWireMethods(t *testing.T) {
	var seen []struct {
		method string
		params any
	}
	call := func(_ context.Context, method string, params any) (json.RawMessage, error) {
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
		return json.Marshal(results[method])
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

func TestCallerSugarMatchesJavaScriptShapes(t *testing.T) {
	type pending struct {
		release chan struct{}
		result  TurnResult
		err     error
	}
	var mu sync.Mutex
	runs := map[string]*pending{}
	call := func(_ context.Context, method string, params any) (json.RawMessage, error) {
		request := params.(TurnRunRequest)
		mu.Lock()
		run := runs[request.SessionID]
		mu.Unlock()
		<-run.release
		return json.Marshal(run.result)
	}
	add := func(id string, result TurnResult, err error) {
		mu.Lock()
		runs[id] = &pending{release: make(chan struct{}), result: result, err: err}
		mu.Unlock()
	}
	add("one@local", TurnResult{Outcome: "completed", Result: "done"}, nil)
	add("two@local", TurnResult{Outcome: "completed", Result: "two"}, nil)
	c := newCaller(func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		raw, err := call(ctx, method, params)
		request := params.(TurnRunRequest)
		mu.Lock()
		configured := runs[request.SessionID].err
		mu.Unlock()
		if configured != nil {
			return nil, configured
		}
		return raw, err
	})
	first, _ := c.Start(TurnRunRequest{SessionID: "one@local", Input: "first"})
	second, _ := c.Start(TurnRunRequest{SessionID: "two@local", Input: "second"})
	assertJSON(t, first, `{"turn_id":"t-1"}`)
	assertJSON(t, second, `{"turn_id":"t-2"}`)
	if _, err := c.Start(TurnRunRequest{SessionID: "one@local", Input: "again"}); !isCode(err, protocol.Busy) {
		t.Fatalf("same-target start error = %v", err)
	}
	zero := int64(0)
	running, err := c.Wait(WaitRequest{TurnID: first.TurnID, TimeoutMS: &zero})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, running, `{"turn_id":"t-1","session_id":"one@local","state":"running"}`)
	close(runs["one@local"].release)
	done, err := c.Wait(WaitRequest{TurnID: first.TurnID})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, done, `{"turn_id":"t-1","session_id":"one@local","state":"done","result":{"outcome":"completed","result":"done"}}`)
	if _, err = c.Status(StatusRequest{TurnID: first.TurnID}); !errors.Is(err, ErrUnknownTurn) {
		t.Fatalf("collected status error = %v", err)
	}
	close(runs["two@local"].release)
	if _, err = c.Wait(WaitRequest{TurnID: second.TurnID}); err != nil {
		t.Fatal(err)
	}
	add("wire@local", TurnResult{}, &ProtocolError{Code: protocol.NotRunning, Message: "not_running"})
	wire, _ := c.Start(TurnRunRequest{SessionID: "wire@local", Input: "work"})
	close(runs["wire@local"].release)
	unavailable, _ := c.Wait(WaitRequest{TurnID: wire.TurnID})
	assertJSON(t, unavailable, `{"turn_id":"t-3","session_id":"wire@local","state":"unavailable","reason":"-32004 not_running"}`)
	add("lost@local", TurnResult{}, rpc.ErrClosed)
	lost, _ := c.Start(TurnRunRequest{SessionID: "lost@local", Input: "work"})
	close(runs["lost@local"].release)
	unavailable, _ = c.Wait(WaitRequest{TurnID: lost.TurnID})
	assertJSON(t, unavailable, `{"turn_id":"t-4","session_id":"lost@local","state":"unavailable","reason":"result unavailable, lane resumable"}`)
}

func TestDialIsOneShotFramedClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentbus.sock")
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
	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.Call(context.Background(), "session.list", SessionListRequest{})
	if err != nil || string(raw) != `{"sessions":[]}` {
		t.Fatalf("result = %s, err %v", raw, err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertJSON(t *testing.T, value any, want string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil || string(raw) != want {
		t.Fatalf("json = %s, want %s, err %v", raw, want, err)
	}
}

func isCode(err error, code int) bool {
	var value *ProtocolError
	return errors.As(err, &value) && value.Code == code
}
