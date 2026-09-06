package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

type crossingBackend struct {
	started chan struct{}
	release chan struct{}
	calls   chan string
}

func (b *crossingBackend) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	arguments, _ := json.Marshal(params)
	b.calls <- method + ":" + string(arguments)
	if method == "turn.run" {
		close(b.started)
		<-b.release
	}
	if method == "session.close" {
		return nil, &sessionkit.ProtocolError{Code: -32004, Message: "not_running"}
	}
	if method == "turn.run" {
		return json.RawMessage(`{"outcome":"completed","result":"done"}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func TestLaneBackendCrossingCallsAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lane.sock")
	listener, err := net.Listen("unix", path)
	check(t, err == nil, "listen: %v", err)
	t.Cleanup(func() { _ = listener.Close() })
	backend := &crossingBackend{started: make(chan struct{}), release: make(chan struct{}), calls: make(chan string, 2)}
	go func() { _ = ServeLane(context.Background(), listener, backend) }()
	t.Setenv(LaneSocketEnv, path)
	lane, err := NewLaneBackend()
	check(t, err == nil, "backend: %v", err)
	run := make(chan error, 1)
	go func() {
		result, callErr := lane.Call(context.Background(), "turn.run", sessionkit.TurnRunRequest{SessionID: "lane@local", Input: "work"})
		if string(result) != `{"outcome":"completed","result":"done"}` {
			callErr = errors.New("run result changed")
		}
		run <- callErr
	}()
	<-backend.started
	result, err := lane.Call(context.Background(), "turn.interrupt", sessionkit.SessionTarget{SessionID: "lane@local"})
	check(t, err == nil && string(result) == `{}`, "interrupt = %s / %v", result, err)
	close(backend.release)
	check(t, <-run == nil, "run failed")
	first, second := <-backend.calls, <-backend.calls
	check(t, first == `turn.run:{"session_id":"lane@local","input":"work"}` && second == `turn.interrupt:{"session_id":"lane@local"}`, "calls = %q, %q", first, second)
	_, err = lane.Call(context.Background(), "session.close", sessionkit.SessionCloseRequest{SessionID: "lane@local"})
	var failed *sessionkit.ProtocolError
	check(t, errors.As(err, &failed) && failed.Code == -32004 && failed.Message == "not_running", "failure = %v", err)
}

func TestLaneBackendRequiresSocket(t *testing.T) {
	t.Setenv(LaneSocketEnv, "")
	_, err := NewLaneBackend()
	check(t, err != nil, "missing socket accepted")
}
