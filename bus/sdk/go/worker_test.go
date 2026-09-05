package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

type fakeProduct struct {
	worker                        *Worker
	started, release, interrupted chan struct{}
	closeStart, closeEnd          chan struct{}
	outbound                      bool
	calls                         [6]atomic.Int32
}

func (p *fakeProduct) Hello(context.Context) (HelloDescription, error) {
	p.calls[0].Add(1)
	return HelloDescription{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}, nil
}
func (p *fakeProduct) Open(context.Context, OpenRequest) (OpenResult, error) {
	p.calls[1].Add(1)
	return OpenResult{SessionID: "product-session"}, nil
}
func (p *fakeProduct) Run(ctx context.Context, input string) (TurnResult, error) {
	p.calls[2].Add(1)
	switch input {
	case "block":
		close(p.started)
		<-p.release
		return TurnResult{Outcome: "interrupted"}, nil
	case "eof":
		close(p.started)
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	case "fail":
		return TurnResult{}, errors.New("failed exactly")
	case "long":
		return TurnResult{Outcome: "completed", Result: strings.Repeat("x", protocol.MaxTextRunes+1)}, nil
	case "empty":
		return TurnResult{Outcome: "completed"}, nil
	case "interrupted":
		return TurnResult{Outcome: "interrupted"}, nil
	default:
		return TurnResult{Outcome: "completed", Result: input}, nil
	}
}
func (p *fakeProduct) Interrupt(context.Context) error {
	p.calls[3].Add(1)
	if p.interrupted != nil {
		close(p.interrupted)
	}
	return nil
}
func (p *fakeProduct) Deliver(ctx context.Context, _ DeliveryRequest) (DeliveryReceipt, error) {
	p.calls[4].Add(1)
	if p.outbound {
		return DeliveryReceipt{Disposition: "injected"}, p.worker.Call(ctx, "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{})
	}
	return DeliveryReceipt{Disposition: "injected"}, nil
}
func (p *fakeProduct) Close(context.Context) error {
	p.calls[5].Add(1)
	if p.closeStart != nil {
		close(p.closeStart)
		<-p.closeEnd
	}
	return nil
}
func (p *fakeProduct) observed() (out [6]int32) {
	for index := range out {
		out[index] = p.calls[index].Load()
	}
	return
}

type harness struct {
	w   *Worker
	d   *rpc.Conn
	raw net.Conn
}

type lifecycleCase struct {
	Name  string   `json:"name"`
	Calls [6]int32 `json:"calls"`
}

func newHarness(t *testing.T, p *fakeProduct) *harness {
	setEnvironment(t, "token", "")
	worker, daemon := net.Pipe()
	w := NewWorker(p)
	w.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		check(t, p.calls[0].Load() == 1 && network == "unix" && address == "/fixture/socket", "dial before hello or wrong endpoint: %d %s %s", p.calls[0].Load(), network, address)
		return worker, nil
	}
	h := &harness{w: w, raw: daemon}
	p.worker = w
	hello := make(chan struct{})
	h.d = rpc.New(daemon, false, func(_ context.Context, request *rpc.Request) {
		switch request.Method {
		case "session.hello":
			must(t, h.d.Result(request, struct{}{}))
			close(hello)
		case "session.list":
			if h.w.opened.Load() {
				must(t, h.d.Result(request, protocol.SessionListResult{Sessions: []protocol.SessionSummary{}}))
			} else {
				must(t, h.d.Error(request, protocol.NotCommitted, nil))
			}
		}
	})
	go w.Serve(context.Background())
	<-hello
	check(t, os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "" && os.Getenv("AGENTBUS_SOCKET") == "", "worker environment was not scrubbed")
	t.Cleanup(func() { _ = h.d.Close(); <-w.Closed() })
	return h
}

func (h *harness) call(method string, params, result any) error {
	return h.d.Call(context.Background(), method, params, result)
}
func (h *harness) async(method string, params, result any) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.call(method, params, result) }()
	return done
}
func (h *harness) open(t *testing.T) {
	var result OpenResult
	must(t, h.call("session.open", OpenRequest{Name: "parent/leaf@local", Groups: []string{}, Open: OpenOptions{}}, &result))
	check(t, result.SessionID == "product-session", "session id = %q", result.SessionID)
}

func TestWorkerLifecycleTable(t *testing.T) {
	var table struct {
		Cases []lifecycleCase `json:"cases"`
	}
	raw, err := os.ReadFile("../../internal/protocol/session-lifecycle.fixtures.json")
	must(t, err)
	must(t, json.Unmarshal(raw, &table))
	check(t, len(table.Cases) == 14, "cases = %d", len(table.Cases))
	for _, row := range table.Cases {
		t.Run(row.Name, func(t *testing.T) {
			got := runCase(t, row.Name)
			check(t, got == row.Calls, "callback calls = %v, want %v", got, row.Calls)
		})
	}
}

func runCase(t *testing.T, name string) [6]int32 {
	p := &fakeProduct{}
	switch name {
	case "ready-open-commit":
		h := newHarness(t, p)
		wantCode(t, p.worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}), protocol.NotCommitted)
		h.open(t)
		must(t, p.worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}))
	case "describe-eof":
		h := newHarness(t, p)
		_ = h.d.Close()
		<-h.w.Closed()
	case "terminal-results":
		checkResults(t, opened(t, p))
	case "one-run", "one-interrupt", "full-duplex", "close-during-run", "callback-originated-method":
		return blockingCase(t, name)
	case "eof-during-run":
		h := opened(t, p)
		p.started = make(chan struct{})
		run := h.async("turn.run", turn("eof"), &TurnResult{})
		<-p.started
		_ = h.d.Close()
		<-h.w.Closed()
		wantError(t, <-run)
	case "peer-lifetime":
		h := newHarness(t, p)
		must(t, h.call("session.superseded", struct{}{}, &struct{}{}))
		<-h.w.Closed()
		wantError(t, h.w.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}))
		for _, item := range []string{"eof reconnect", "same-id re-hello", "changed groups", "different-id re-hello"} {
			t.Run(item, func(t *testing.T) { t.Skip("pending connectPeer in commit 3") })
		}
	case "invalid-frames":
		invalidFrames(t, p)
	case "terminal-before-interrupt":
		h := opened(t, p)
		h.w.run = &runSlot{done: make(chan struct{}), terminal: true}
		must(t, h.call("turn.interrupt", target(), &struct{}{}))
		check(t, p.calls[3].Load() == 0, "terminal slot called Interrupt")
	case "idle-close-deliver":
		h := opened(t, p)
		p.closeStart, p.closeEnd = make(chan struct{}), make(chan struct{})
		closed := h.async("session.close", target(), &struct{}{})
		<-p.closeStart
		checkDelivery(t, h, "rejected")
		close(p.closeEnd)
		must(t, <-closed)
	case "environment":
		checkEnvironment(t, p)
	default:
		t.Fatalf("unknown lifecycle case %q", name)
	}
	return p.observed()
}

func blockingCase(t *testing.T, name string) [6]int32 {
	p := &fakeProduct{started: make(chan struct{}), release: make(chan struct{})}
	h := opened(t, p)
	run := h.async("turn.run", turn("block"), &TurnResult{})
	<-p.started
	switch name {
	case "one-run":
		wantCode(t, h.call("turn.run", turn("again"), &TurnResult{}), protocol.Busy)
	case "one-interrupt":
		p.interrupted = make(chan struct{})
		first, second := h.async("turn.interrupt", target(), &struct{}{}), h.async("turn.interrupt", target(), &struct{}{})
		<-p.interrupted
		must(t, <-first)
		must(t, <-second)
	case "full-duplex", "callback-originated-method":
		p.outbound = true
		checkDelivery(t, h, "injected")
	case "close-during-run":
		p.interrupted = make(chan struct{})
		closed := h.async("session.close", target(), &struct{}{})
		<-p.interrupted
		close(p.release)
		must(t, <-run)
		must(t, <-closed)
		return p.observed()
	}
	close(p.release)
	must(t, <-run)
	return p.observed()
}

func checkResults(t *testing.T, h *harness) {
	for _, test := range []struct {
		input, outcome, result string
		truncated              bool
	}{
		{"ok", "completed", "ok", false}, {"interrupted", "interrupted", "", false},
		{"fail", "failed", "failed exactly", false}, {"empty", "completed", "", false},
		{"long", "completed", strings.Repeat("x", protocol.MaxTextRunes), true},
	} {
		var result TurnResult
		must(t, h.call("turn.run", turn(test.input), &result))
		check(t, result.Outcome == test.outcome && result.Result == test.result && result.Truncated == test.truncated, "%s result = %#v", test.input, result)
	}
}

func checkDelivery(t *testing.T, h *harness, disposition string) {
	var receipt DeliveryReceipt
	must(t, h.call("message.deliver", delivery, &receipt))
	check(t, receipt.Disposition == disposition, "receipt = %#v", receipt)
}

func invalidFrames(t *testing.T, total *fakeProduct) {
	for _, frame := range []string{
		`{"jsonrpc":`, `{"jsonrpc":"2.0","id":77,"method":"unknown","params":{}}`,
		strings.Repeat("x", protocol.MaxFrameBytes+1), `{"jsonrpc":"2.0","id":9007199254740992,"method":"session.list","params":{}}`,
	} {
		p := &fakeProduct{}
		h := newHarness(t, p)
		_, _ = h.raw.Write(append([]byte(frame), '\n'))
		<-h.w.Closed()
		total.calls[0].Add(p.calls[0].Load())
	}
}

func checkEnvironment(t *testing.T, p *fakeProduct) {
	setEnvironment(t, "token", "key")
	w := NewWorker(p)
	w.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("keyed worker dialled")
		return nil, nil
	}
	err := w.Serve(context.Background())
	check(t, err != nil && err.Error() == "local key transport not implemented in this build", "key error = %v", err)
	check(t, os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "" && os.Getenv("AGENTBUS_SOCKET") == "", "keyed worker environment was not scrubbed")
}

func opened(t *testing.T, p *fakeProduct) *harness { h := newHarness(t, p); h.open(t); return h }
func turn(input string) protocol.TurnRunRequest {
	return protocol.TurnRunRequest{SessionID: "product-session@local", Input: input}
}
func target() protocol.SessionTarget {
	return protocol.SessionTarget{SessionID: "product-session@local"}
}
func setEnvironment(t *testing.T, token, key string) {
	t.Setenv("AGENTBUS_LAUNCH_TOKEN", token)
	t.Setenv("AGENTBUS_LOCAL_KEY", key)
	t.Setenv("AGENTBUS_SOCKET", "/fixture/socket")
}
func wantCode(t *testing.T, err error, code int) {
	var rpcErr *protocol.RPCError
	check(t, errors.As(err, &rpcErr) && rpcErr.Code == code, "error = %v, want code %d", err, code)
}
func wantError(t *testing.T, err error) { check(t, err != nil, "expected error") }
func must(t *testing.T, err error)      { check(t, err == nil, "%v", err) }
func check(t *testing.T, condition bool, format string, args ...any) {
	if !condition {
		t.Fatalf(format, args...)
	}
}

var delivery = DeliveryRequest{MessageID: "m", From: DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "peer", Groups: []string{}}, Body: "body"}
