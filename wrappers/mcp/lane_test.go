package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

type crossingBackend struct {
	started chan struct{}
	release chan struct{}
	calls   chan string
}

func (b *crossingBackend) Call(_ context.Context, action string, arguments json.RawMessage) (json.RawMessage, error) {
	b.calls <- action + ":" + string(arguments)
	if action == "run" {
		close(b.started)
		<-b.release
	}
	if action == "close" {
		return nil, &failure{Code: -32004, Message: "offline"}
	}
	return json.RawMessage(`{"action":"` + action + `"}`), nil
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
		result, callErr := lane.Call(context.Background(), "run", json.RawMessage(`{"input":"work"}`))
		if string(result) != `{"action":"run"}` {
			callErr = errors.New("run result changed")
		}
		run <- callErr
	}()
	<-backend.started
	result, err := lane.Call(context.Background(), "interrupt", json.RawMessage(`{}`))
	check(t, err == nil && string(result) == `{"action":"interrupt"}`, "interrupt = %s / %v", result, err)
	close(backend.release)
	check(t, <-run == nil, "run failed")
	first, second := <-backend.calls, <-backend.calls
	check(t, first == `run:{"input":"work"}` && second == `interrupt:{}`, "calls = %q, %q", first, second)
	_, err = lane.Call(context.Background(), "close", json.RawMessage(`{}`))
	var failed *failure
	check(t, errors.As(err, &failed) && failed.Code == -32004 && failed.Message == "offline", "failure = %v", err)
}

func TestLaneBackendRequiresSocket(t *testing.T) {
	t.Setenv(LaneSocketEnv, "")
	_, err := NewLaneBackend()
	check(t, err != nil, "missing socket accepted")
}
