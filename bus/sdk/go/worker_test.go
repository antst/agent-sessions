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
	deliverStart                  chan struct{}
	closeStart, closeEnd          chan struct{}
	outbound, hangInterrupt       bool
	calls                         [6]atomic.Int32
}

func (p *fakeProduct) Hello(context.Context) (HelloDescription, error) {
	p.calls[0].Add(1)
	if os.Getenv("AGENTBUS_LAUNCH_TOKEN") != "" || os.Getenv("AGENTBUS_LOCAL_KEY") != "" || os.Getenv("AGENTBUS_SOCKET") != "" {
		panic("worker environment reached Hello")
	}
	return HelloDescription{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}, nil
}
func (p *fakeProduct) Open(_ context.Context, request OpenRequest) (OpenResult, error) {
	p.calls[1].Add(1)
	if request.Name != "parent/leaf@local" || len(request.Groups) != 1 || request.Groups[0] != "session:parent" || request.ResumeSessionID != "product-session" || request.Open.Cwd != "/tmp" || request.Open.PermissionMode != "ask" || request.Open.Model != "model" || request.Open.ReasoningEffort != "high" || len(request.Open.Arguments) != 1 || request.Open.Arguments[0] != "--flag" {
		return OpenResult{}, errors.New("open request changed")
	}
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
func (p *fakeProduct) Interrupt(ctx context.Context) error {
	p.calls[3].Add(1)
	if p.interrupted != nil {
		close(p.interrupted)
	}
	if p.hangInterrupt {
		<-ctx.Done()
	}
	return nil
}
func (p *fakeProduct) Deliver(ctx context.Context, _ DeliveryRequest) (DeliveryReceipt, error) {
	p.calls[4].Add(1)
	if p.deliverStart != nil {
		close(p.deliverStart)
		<-ctx.Done()
		return DeliveryReceipt{}, ctx.Err()
	}
	if p.outbound {
		return DeliveryReceipt{Disposition: "injected"}, p.worker.Call(ctx, "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{})
	}
	return DeliveryReceipt{Disposition: "injected"}, nil
}
func (p *fakeProduct) Close(ctx context.Context) error {
	p.calls[5].Add(1)
	if ctx.Err() == nil {
		return errors.New("close context was not cancelled")
	}
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
	d *rpc.Conn
}
type lifecycleCase struct {
	Name  string   `json:"name"`
	Calls [6]int32 `json:"calls"`
}

func newHarness(t *testing.T, p *fakeProduct) *harness { return startHarness(t, p, true) }
func startHarness(t *testing.T, p *fakeProduct, acknowledge bool) *harness {
	setEnvironment(t, "token", "")
	worker, daemon := net.Pipe()
	w := NewWorker(p)
	w.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		check(t, p.calls[0].Load() == 1 && network == "unix" && address == "/fixture/socket", "dial before hello or wrong endpoint")
		return worker, nil
	}
	h := &harness{}
	p.worker = w
	hello := make(chan struct{})
	h.d = rpc.New(daemon, false, func(_ context.Context, request *rpc.Request) {
		switch request.Method {
		case "session.hello":
			if acknowledge {
				must(t, h.d.Result(request, struct{}{}))
			} else {
				_ = h.d.Close()
			}
			close(hello)
		case "session.list":
			if p.worker.opened.Load() {
				must(t, h.d.Result(request, protocol.SessionListResult{Sessions: []protocol.SessionSummary{}}))
			} else {
				must(t, h.d.Error(request, protocol.NotCommitted, nil))
			}
		}
	})
	go w.Serve(context.Background())
	<-hello
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
	check(t, h.call("session.open", OpenRequest{Name: "parent/leaf@local", Groups: []string{"session:parent"}, ResumeSessionID: "product-session", Open: OpenOptions{Cwd: "/tmp", PermissionMode: "ask", Model: "model", ReasoningEffort: "high", Arguments: []string{"--flag"}}}, &result) == nil && result.SessionID == "product-session", "open result = %#v", result)
}

func TestWorkerLifecycleTable(t *testing.T) {
	var table struct {
		Cases []lifecycleCase `json:"cases"`
	}
	raw, err := os.ReadFile("../../internal/protocol/session-lifecycle.fixtures.json")
	must(t, err)
	must(t, json.Unmarshal(raw, &table))
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
		startHarness(t, p, false)
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
		<-p.worker.Closed()
		check(t, <-run != nil, "expected error")
	case "peer-lifetime":
		h := newHarness(t, p)
		must(t, h.call("session.superseded", struct{}{}, &struct{}{}))
		check(t, p.worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}) != nil, "expected error")
		for _, item := range []string{"eof reconnect", "same-id re-hello", "changed groups", "different-id re-hello"} {
			t.Run(item, func(t *testing.T) { t.Skip("pending connectPeer in commit 3") })
		}
		t.Run("worker re-hello", func(t *testing.T) { t.Skip("pending daemon admission (commit 2)") })
	case "wrong-direction-request":
		h := newHarness(t, p)
		check(t, h.call("session.hello", protocol.WorkerHello{Protocol: 1, LaunchToken: "again", HelloDescription: HelloDescription{Product: "worker", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}}, &struct{}{}) != nil, "expected error")
	case "terminal-before-interrupt":
		h := opened(t, p)
		must(t, h.call("turn.run", turn("ok"), &TurnResult{}))
		wantCode(t, h.call("turn.interrupt", target, &struct{}{}), protocol.NotRunning)
	case "idle-close-deliver":
		h := opened(t, p)
		p.deliverStart, p.closeStart, p.closeEnd = make(chan struct{}), make(chan struct{}), make(chan struct{})
		delivered := h.async("message.deliver", delivery, &DeliveryReceipt{})
		<-p.deliverStart
		closed := h.async("session.close", target, &struct{}{})
		<-p.closeStart
		checkDelivery(t, h, "rejected")
		must(t, <-delivered)
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
		first, second := h.async("turn.interrupt", target, &struct{}{}), h.async("turn.interrupt", target, &struct{}{})
		<-p.interrupted
		must(t, <-first)
		must(t, <-second)
	case "full-duplex", "callback-originated-method":
		p.outbound = true
		checkDelivery(t, h, "injected")
	case "close-during-run":
		p.interrupted, p.closeStart, p.closeEnd, p.hangInterrupt = make(chan struct{}), make(chan struct{}), make(chan struct{}), true
		closed := h.async("session.close", target, &struct{}{})
		<-p.interrupted
		must(t, h.call("turn.interrupt", target, &struct{}{}))
		close(p.release)
		must(t, <-run)
		<-p.closeStart
		close(p.closeEnd)
		must(t, <-closed)
		return p.observed()
	}
	close(p.release)
	must(t, <-run)
	return p.observed()
}

func checkResults(t *testing.T, h *harness) {
	for _, test := range []struct {
		input string
		want  TurnResult
	}{
		{"ok", TurnResult{Outcome: "completed", Result: "ok"}}, {"interrupted", TurnResult{Outcome: "interrupted"}},
		{"fail", TurnResult{Outcome: "failed", Result: "failed exactly"}}, {"empty", TurnResult{Outcome: "completed"}},
		{"long", TurnResult{Outcome: "completed", Result: strings.Repeat("x", protocol.MaxTextRunes), Truncated: true}},
	} {
		var result TurnResult
		must(t, h.call("turn.run", turn(test.input), &result))
		check(t, result == test.want, "%s result = %#v", test.input, result)
	}
}

func checkDelivery(t *testing.T, h *harness, disposition string) {
	var receipt DeliveryReceipt
	must(t, h.call("message.deliver", delivery, &receipt))
	check(t, receipt.Disposition == disposition, "receipt = %#v", receipt)
}

func checkEnvironment(t *testing.T, p *fakeProduct) {
	setEnvironment(t, "token", "key")
	w := NewWorker(p)
	err := w.Serve(context.Background())
	check(t, err != nil && err.Error() == "local key transport not implemented in this build", "key error = %v", err)
	check(t, os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "" && os.Getenv("AGENTBUS_SOCKET") == "", "keyed worker environment was not scrubbed")
}

func opened(t *testing.T, p *fakeProduct) *harness { h := newHarness(t, p); h.open(t); return h }
func turn(input string) protocol.TurnRunRequest {
	return protocol.TurnRunRequest{SessionID: "product-session@local", Input: input}
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
func must(t *testing.T, err error) { check(t, err == nil, "%v", err) }
func check(t *testing.T, condition bool, format string, args ...any) {
	if !condition {
		t.Fatalf(format, args...)
	}
}

var delivery = DeliveryRequest{MessageID: "m", From: DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "peer", Groups: []string{}}, Body: "body"}
var target = protocol.SessionTarget{SessionID: "product-session@local"}
