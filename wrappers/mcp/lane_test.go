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

func (*crossingBackend) Prepare(context.Context, json.RawMessage) error { return nil }

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
		result, callErr := lane.Action(context.Background(), "run", json.RawMessage(`{"session_id":"lane@local","input":"work"}`))
		if string(result) != `{"outcome":"completed","result":"done"}` {
			callErr = errors.New("run result changed")
		}
		run <- callErr
	}()
	<-backend.started
	result, err := lane.Action(context.Background(), "interrupt", json.RawMessage(`{"session_id":"lane@local"}`))
	check(t, err == nil && string(result) == `{}`, "interrupt = %s / %v", result, err)
	close(backend.release)
	check(t, <-run == nil, "run failed")
	first, second := <-backend.calls, <-backend.calls
	check(t, first == `turn.run:{"session_id":"lane@local","input":"work"}` && second == `turn.interrupt:{"session_id":"lane@local"}`, "calls = %q, %q", first, second)
	_, err = lane.Action(context.Background(), "close", json.RawMessage(`{"session_id":"lane@local"}`))
	var failed *sessionkit.ProtocolError
	check(t, errors.As(err, &failed) && failed.Code == -32004 && failed.Message == "not_running", "failure = %v", err)
}

func TestLaneBackendRequiresSocket(t *testing.T) {
	t.Setenv(LaneSocketEnv, "")
	_, err := NewLaneBackend()
	check(t, err != nil, "missing socket accepted")
}

func TestPrivateActionKeepsCallerInResidentServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lane.sock")
	listener, err := net.Listen("unix", path)
	check(t, err == nil, "listen: %v", err)
	defer listener.Close()
	backend := &crossingBackend{started: make(chan struct{}), release: make(chan struct{}), calls: make(chan string, 2)}
	go func() { _ = ServeLane(context.Background(), listener, backend) }()
	first := &LaneBackend{path: path}
	result, err := first.Action(context.Background(), "start", json.RawMessage(`{"session_id":"lane@local","input":"work"}`))
	check(t, err == nil && string(result) == `{"turn_id":"t-1"}`, "start = %s / %v", result, err)
	<-backend.started
	second := &LaneBackend{path: path}
	result, err = second.Action(context.Background(), "status", json.RawMessage(`{"turn_id":"t-1"}`))
	check(t, err == nil && string(result) == `{"turn_id":"t-1","session_id":"lane@local","state":"running"}`, "status = %s / %v", result, err)
	close(backend.release)
	result, err = second.Action(context.Background(), "wait", json.RawMessage(`{"turn_id":"t-1"}`))
	check(t, err == nil && string(result) == `{"turn_id":"t-1","session_id":"lane@local","state":"done","result":{"outcome":"completed","result":"done"}}`, "wait = %s / %v", result, err)
}
