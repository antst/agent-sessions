package sessionkit

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestCallerWireActions(t *testing.T) {
	tests := []struct {
		action, method, args, response string
		want                           any
	}{
		{"list", "session.list", `{}`, `{"sessions":[]}`, SessionListRequest{}},
		{"send", "message.send", `{"target":"lane","message":"hi"}`, `{"message_id":"m","deliveries":[]}`, MessageSendRequest{Target: "lane", Message: "hi"}},
		{"spawn", "lane.spawn", `{"name":"child","product":"example-peer","open":{}}`, `{"session_id":"child@local"}`, LaneSpawnRequest{Name: "child", Product: "example-peer", Open: &OpenOptions{}}},
		{"describe", "lane.describe", `{"product":"example-peer"}`, `{"product":"example-peer","supported_open_fields":[],"extra_arguments":[]}`, LaneDescribeRequest{Product: "example-peer"}},
		{"run", "turn.run", `{"session_id":"child@local","input":"hi"}`, `{"outcome":"completed","result":"hi"}`, TurnRunRequest{SessionID: "child@local", Input: "hi"}},
		{"interrupt", "turn.interrupt", `{"session_id":"child@local"}`, `{}`, SessionTarget{SessionID: "child@local"}},
		{"close", "session.close", `{"session_id":"child@local"}`, `{}`, SessionCloseRequest{SessionID: "child@local"}},
		{"forget", "session.close", `{"session_id":"child@local"}`, `{}`, SessionCloseRequest{SessionID: "child@local", Forget: true}},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			var method string
			var params any
			caller := newCaller(func(_ context.Context, gotMethod string, gotParams, result any) error {
				method, params = gotMethod, gotParams
				return json.Unmarshal([]byte(test.response), result)
			})
			got, err := caller.Action(context.Background(), test.action, json.RawMessage(test.args))
			if err != nil || method != test.method || string(got) != test.response {
				t.Fatalf("method %q params %#v result %s, err %v", method, params, got, err)
			}
			assertJSON(t, params, test.want)
		})
	}
}

func TestCallerLocalActions(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	caller := newCaller(func(_ context.Context, method string, _ any, result any) error {
		if method == "turn.run" {
			close(started)
			<-release
			*result.(*TurnResult) = TurnResult{Outcome: "completed", Result: "done"}
		}
		return nil
	})
	tests := []struct {
		action, args, want string
		before             func()
	}{
		{"start", `{"session_id":"child@local","input":"hi"}`, `{"turn_id":"t-1"}`, nil},
		{"status", `{"turn_id":"t-1"}`, `{"turn_id":"t-1","session_id":"child@local","state":"running"}`, func() { <-started }},
		{"wait", `{"turn_id":"t-1"}`, `{"turn_id":"t-1","session_id":"child@local","state":"done","result":{"outcome":"completed","result":"done"}}`, func() { close(release) }},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			if test.before != nil {
				test.before()
			}
			got, err := caller.Action(context.Background(), test.action, json.RawMessage(test.args))
			if err != nil || string(got) != test.want {
				t.Fatalf("result %s, err %v", got, err)
			}
		})
	}
}

func TestCallerActionRejectsInvalidInput(t *testing.T) {
	caller := newCaller(func(context.Context, string, any, any) error { return nil })
	for _, test := range []struct {
		name, action, args string
	}{
		{"unknown action", "unknown", `{}`},
		{"wire shape", "list", `{"extra":true}`},
		{"local shape", "status", `{"turn_id":"t-1","extra":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := caller.Action(context.Background(), test.action, json.RawMessage(test.args)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if !reflect.DeepEqual(Actions, []string{"list", "send", "spawn", "describe", "run", "start", "wait", "status", "interrupt", "close", "forget"}) {
		t.Fatalf("actions = %v", Actions)
	}
}

func TestNewCallerRejectsInvalidResult(t *testing.T) {
	for _, test := range []struct {
		name, result string
	}{
		{"missing field", `{}`},
		{"extra field", `{"sessions":[],"extra":true}`},
		{"wrong field type", `{"sessions":"wrong"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := NewCaller(func(context.Context, string, any) (json.RawMessage, error) {
				return json.RawMessage(test.result), nil
			})
			listed, err := caller.List(context.Background(), SessionListRequest{})
			if err == nil || listed.Sessions != nil || listed.Hosts != nil {
				t.Fatalf("typed result = %#v, err %v", listed, err)
			}
			raw, err := caller.Action(context.Background(), "list", json.RawMessage(`{}`))
			if err == nil || raw != nil {
				t.Fatalf("action result = %s, err %v", raw, err)
			}
		})
	}
}
