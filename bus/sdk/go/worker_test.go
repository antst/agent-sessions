package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/livepresence"
)

type fixtureProduct struct {
	worker     *Worker
	run        func(context.Context, string) (TurnResult, error)
	interrupt  func(context.Context) error
	deliver    func(context.Context, DeliveryRequest) (DeliveryReceipt, error)
	close      func(context.Context) error
	opens      atomic.Int32
	runs       atomic.Int32
	interrupts atomic.Int32
	deliveries atomic.Int32
	closes     atomic.Int32
}

func (p *fixtureProduct) Hello(context.Context) (Hello, error) {
	return Hello{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}, nil
}
func (p *fixtureProduct) Open(context.Context, OpenRequest) (OpenResult, error) {
	p.opens.Add(1)
	return OpenResult{SessionID: "product-session"}, nil
}
func (p *fixtureProduct) Run(ctx context.Context, input string) (TurnResult, error) {
	p.runs.Add(1)
	return p.run(ctx, input)
}
func (p *fixtureProduct) Interrupt(ctx context.Context) error {
	p.interrupts.Add(1)
	return p.interrupt(ctx)
}
func (p *fixtureProduct) Deliver(ctx context.Context, request DeliveryRequest) (DeliveryReceipt, error) {
	p.deliveries.Add(1)
	return p.deliver(ctx, request)
}
func (p *fixtureProduct) Close(ctx context.Context) error { p.closes.Add(1); return p.close(ctx) }
func newFixtureProduct() *fixtureProduct {
	return &fixtureProduct{
		run: func(_ context.Context, input string) (TurnResult, error) {
			return TurnResult{Outcome: "completed", Result: input}, nil
		},
		interrupt: func(context.Context) error { return nil },
		deliver: func(context.Context, DeliveryRequest) (DeliveryReceipt, error) {
			return DeliveryReceipt{Disposition: "injected"}, nil
		},
		close: func(context.Context) error { return nil },
	}
}

type workerHarness struct {
	t       *testing.T
	worker  *Worker
	daemon  *livepresence.SessionRPC
	request chan livepresence.Frame
	serve   chan error
}

func newWorkerHarness(t *testing.T, product *fixtureProduct) *workerHarness {
	t.Setenv("AGENT_SESSIONS_LAUNCH_TOKEN", "launch-secret")
	t.Setenv("AGENT_SESSIONS_LOCAL_KEY", "")
	t.Setenv("AGENT_SESSIONS_SOCKET", "/fixture/socket")
	workerSide, daemonSide := net.Pipe()
	w := NewWorker(product, func(context.Context, string, string) (net.Conn, error) { return workerSide, nil })
	product.worker = w
	rpc, err := livepresence.NewSessionRPC(daemonSide)
	must(t, err)
	h := &workerHarness{t: t, worker: w, daemon: rpc, request: make(chan livepresence.Frame, 8), serve: make(chan error, 1)}
	go func() {
		for {
			frame, readErr := rpc.Read(false)
			if readErr != nil {
				return
			}
			h.request <- frame
		}
	}()
	go func() { h.serve <- w.Serve(context.Background()) }()
	hello := h.next("session.hello")
	must(t, rpc.Result(hello, struct{}{}))
	t.Cleanup(func() {
		_ = rpc.Close()
		select {
		case <-h.serve:
		case <-time.After(time.Second):
			t.Fatal("worker did not stop")
		}
	})
	return h
}

func (h *workerHarness) next(method string) livepresence.Frame {
	select {
	case frame := <-h.request:
		if frame.Method != method {
			h.t.Fatalf("method = %s, want %s", frame.Method, method)
		}
		return frame
	case <-time.After(time.Second):
		h.t.Fatalf("timed out waiting for %s", method)
		return livepresence.Frame{}
	}
}

func (h *workerHarness) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.daemon.Call(ctx, false, method, params, result)
}

func (h *workerHarness) async(method string, params, result any) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.call(method, params, result) }()
	return done
}

func (h *workerHarness) open() {
	var result OpenResult
	must(h.t, h.call("session.open", OpenRequest{Name: "parent/leaf@local", Groups: []string{}, Open: livepresence.SessionOpenOptions{}}, &result))
	check(h.t, result.SessionID == "product-session", "session id = %q", result.SessionID)
}

func must(t *testing.T, err error) { t.Helper(); check(t, err == nil, "%v", err) }

func check(t *testing.T, condition bool, format string, args ...any) {
	if !condition {
		t.Fatalf(format, args...)
	}
}

func TestGoWorkerRunsSharedLifecycleFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../protocol/session-lifecycle.fixtures.json")
	must(t, err)
	var table map[string][]json.RawMessage
	must(t, json.Unmarshal(raw, &table))
	check(t, len(table["cases"]) == 14, "fixture rows = %d", len(table["cases"]))

	t.Run("ready-open-and-terminal-results", func(t *testing.T) {
		product := newFixtureProduct()
		product.run = func(_ context.Context, input string) (TurnResult, error) {
			if input == "fail" {
				return TurnResult{}, errors.New("failed exactly")
			}
			if input == "long" {
				return TurnResult{Outcome: "completed", Result: strings.Repeat("x", maxTextCharacters+1)}, nil
			}
			if input == "empty" {
				return TurnResult{Outcome: "completed", Result: ""}, nil
			}
			return TurnResult{Outcome: "completed", Result: input}, nil
		}
		h := newWorkerHarness(t, product)
		precommit := make(chan error, 1)
		go func() { precommit <- h.worker.Call(context.Background(), "session.list", struct{}{}, &struct{}{}) }()
		request := h.next("session.list")
		_ = h.daemon.Error(request, -32011, "not_committed", nil)
		check(t, <-precommit != nil, "precommit method succeeded")
		h.open()
		for _, input := range []string{"ok", "empty", "fail", "long"} {
			var result TurnResult
			must(t, h.call("turn.run", map[string]any{"session_id": "product-session@local", "input": input}, &result))
			check(t, input != "fail" || result.Outcome == "failed", "failed result = %+v", result)
			check(t, input != "long" || result.Truncated && len([]rune(result.Result)) == maxTextCharacters, "truncated result = %+v", result)
		}
		_ = h.call("turn.interrupt", map[string]string{"session_id": "product-session@local"}, &struct{}{})
		check(t, product.interrupts.Load() == 0, "terminal turn reached native interrupt")
	})

	t.Run("one-run-interrupt-full-duplex-and-close", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		product := newFixtureProduct()
		product.run = func(context.Context, string) (TurnResult, error) {
			close(started)
			<-release
			return TurnResult{Outcome: "interrupted", Result: ""}, nil
		}
		product.interrupt = func(context.Context) error { return nil }
		var h *workerHarness
		product.deliver = func(ctx context.Context, _ DeliveryRequest) (DeliveryReceipt, error) {
			call := make(chan error, 1)
			go func() { call <- product.worker.Call(ctx, "session.list", struct{}{}, &struct{}{}) }()
			req := h.next("session.list")
			_ = h.daemon.Result(req, map[string]any{"sessions": []any{}})
			return DeliveryReceipt{Disposition: "injected"}, <-call
		}
		h = newWorkerHarness(t, product)
		h.open()
		runDone := h.async("turn.run", map[string]any{"session_id": "product-session@local", "input": "block"}, &TurnResult{})
		<-started
		var receipt DeliveryReceipt
		deliveryErr := h.call("message.deliver", DeliveryRequest{MessageID: "m", From: livepresence.SessionDeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "peer", Groups: []string{}}, Body: "body"}, &receipt)
		check(t, deliveryErr == nil && receipt.Disposition == "injected", "delivery = %+v, %v", receipt, deliveryErr)
		check(t, h.call("turn.run", map[string]any{"session_id": "product-session@local", "input": "second"}, &TurnResult{}) != nil, "second run succeeded")
		ints := make(chan error, 2)
		go func() {
			ints <- h.call("turn.interrupt", map[string]string{"session_id": "product-session@local"}, &struct{}{})
		}()
		go func() {
			ints <- h.call("turn.interrupt", map[string]string{"session_id": "product-session@local"}, &struct{}{})
		}()
		must(t, <-ints)
		must(t, <-ints)
		close(release)
		must(t, <-runDone)
		check(t, product.runs.Load() == 1 && product.interrupts.Load() == 1, "runs=%d interrupts=%d", product.runs.Load(), product.interrupts.Load())
	})

	t.Run("describe-eof", func(t *testing.T) {
		product := newFixtureProduct()
		h := newWorkerHarness(t, product)
		_ = h.daemon.Close()
		select {
		case <-h.worker.Closed():
		case <-time.After(time.Second):
			t.Fatal("worker stayed open")
		}
		check(t, product.opens.Load() == 0 && product.closes.Load() == 0, "open=%d close=%d", product.opens.Load(), product.closes.Load())
	})

	t.Run("close-during-run-orders-responses", func(t *testing.T) {
		started, interrupted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		product := newFixtureProduct()
		product.run = func(context.Context, string) (TurnResult, error) {
			close(started)
			<-release
			return TurnResult{Outcome: "interrupted", Result: ""}, nil
		}
		product.interrupt = func(context.Context) error { close(interrupted); return nil }
		h := newWorkerHarness(t, product)
		h.open()
		var methods sync.Map
		responses := make(chan string, 2)
		h.daemon.Observe(func(direction string, frame livepresence.Frame) {
			if direction == "send" && frame.Method != "" {
				methods.Store(string(frame.ID), frame.Method)
			}
			if direction == "receive" && frame.Method == "" {
				if method, ok := methods.Load(string(frame.ID)); ok {
					responses <- method.(string)
				}
			}
		})
		runDone := h.async("turn.run", map[string]any{"session_id": "product-session@local", "input": "block"}, &TurnResult{})
		<-started
		closeDone := h.async("session.close", map[string]any{"session_id": "product-session@local"}, &struct{}{})
		<-interrupted
		select {
		case err := <-closeDone:
			t.Fatalf("close returned before run: %v", err)
		default:
		}
		close(release)
		must(t, <-runDone)
		must(t, <-closeDone)
		first, second := <-responses, <-responses
		check(t, first == "turn.run" && second == "session.close", "response order = %s, %s", first, second)
		check(t, product.interrupts.Load() == 1 && product.closes.Load() == 1, "interrupts=%d closes=%d", product.interrupts.Load(), product.closes.Load())
	})

	t.Run("eof-during-run-cancels-and-closes", func(t *testing.T) {
		started := make(chan struct{})
		product := newFixtureProduct()
		product.run = func(ctx context.Context, _ string) (TurnResult, error) {
			close(started)
			<-ctx.Done()
			return TurnResult{}, ctx.Err()
		}
		h := newWorkerHarness(t, product)
		h.open()
		runDone := h.async("turn.run", map[string]any{"session_id": "product-session@local", "input": "block"}, &TurnResult{})
		<-started
		_ = h.daemon.Close()
		<-h.worker.Closed()
		check(t, <-runDone != nil, "run survived control EOF")
		check(t, product.closes.Load() == 1, "close calls = %d", product.closes.Load())
	})

	t.Run("idle-close-rejects-delivery", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		product := newFixtureProduct()
		product.close = func(context.Context) error { close(entered); <-release; return nil }
		h := newWorkerHarness(t, product)
		h.open()
		closed := h.async("session.close", map[string]any{"session_id": "product-session@local"}, &struct{}{})
		<-entered
		var receipt DeliveryReceipt
		err := h.call("message.deliver", DeliveryRequest{MessageID: "m", From: livepresence.SessionDeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "peer", Groups: []string{}}, Body: "body"}, &receipt)
		check(t, err == nil && receipt.Reason == "closing" && product.deliveries.Load() == 0, "delivery=%+v err=%v calls=%d", receipt, err, product.deliveries.Load())
		close(release)
		must(t, <-closed)
	})
}

func TestGoWorkerEnvironmentIsReadOnceAndScrubbed(t *testing.T) {
	t.Setenv("AGENT_SESSIONS_LAUNCH_TOKEN", "secret")
	t.Setenv("AGENT_SESSIONS_LOCAL_KEY", "key")
	t.Setenv("AGENT_SESSIONS_SOCKET", "/fixture/socket")
	product := newFixtureProduct()
	calls := atomic.Int32{}
	worker := NewWorker(product, func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("connect failed")
	})
	check(t, worker.Serve(context.Background()) != nil, "connect succeeded")
	check(t, calls.Load() == 1, "dial calls = %d", calls.Load())
	check(t, os.Getenv("AGENT_SESSIONS_LAUNCH_TOKEN") == "" && os.Getenv("AGENT_SESSIONS_LOCAL_KEY") == "", "secret environment survived")
}
