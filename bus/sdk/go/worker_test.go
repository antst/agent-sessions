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
	worker                  *Worker
	started                 chan *Run
	release, interrupted    chan struct{}
	deliverStart            chan struct{}
	closeStart, closeEnd    chan struct{}
	outbound, hangInterrupt bool
	calls                   [6]int32
}

func (p *fakeProduct) Hello(context.Context) (HelloDescription, error) {
	atomic.AddInt32(&p.calls[0], 1)
	if os.Getenv("AGENTBUS_LAUNCH_TOKEN") != "" || os.Getenv("AGENTBUS_LOCAL_KEY") != "" || os.Getenv("AGENTBUS_SOCKET") != "" {
		panic("worker environment reached Hello")
	}
	return HelloDescription{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}, nil
}
func (p *fakeProduct) Open(_ context.Context, request OpenRequest) (OpenResult, error) {
	atomic.AddInt32(&p.calls[1], 1)
	if request.Name != "parent/leaf@local" || len(request.Groups) != 1 || request.Groups[0] != "session:parent" || request.ResumeSessionID != "product-session" || request.Open.Cwd != "/tmp" || request.Open.PermissionMode != "ask" || request.Open.Model != "model" || request.Open.ReasoningEffort != "high" || len(request.Open.Arguments) != 1 || request.Open.Arguments[0] != "--flag" {
		return OpenResult{}, errors.New("open request changed")
	}
	return OpenResult{SessionID: "product-session"}, nil
}
func (p *fakeProduct) Run(ctx context.Context, run *Run, input string) (TurnResult, error) {
	atomic.AddInt32(&p.calls[2], 1)
	switch input {
	case "block":
		p.started <- run
		<-p.release
		if run.Interrupted() {
			return TurnResult{Outcome: "interrupted"}, nil
		}
		run.Native = "native"
		return TurnResult{Outcome: "completed"}, nil
	case "eof":
		p.started <- run
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	case "fail":
		return TurnResult{}, p
	case "long":
		return TurnResult{Outcome: "completed", Result: strings.Repeat("x", protocol.MaxTextRunes+1)}, nil
	case "empty", "interrupted":
		return TurnResult{Outcome: map[string]string{"empty": "completed", "interrupted": "interrupted"}[input]}, nil
	default:
		return TurnResult{Outcome: "completed", Result: input}, nil
	}
}
func (p *fakeProduct) Interrupt(ctx context.Context, run *Run) error {
	atomic.AddInt32(&p.calls[3], 1)
	if !run.Interrupted() {
		return errors.New("run was not marked interrupted")
	}
	if p.interrupted != nil {
		close(p.interrupted)
	}
	if p.hangInterrupt {
		<-ctx.Done()
	}
	return nil
}
func (p *fakeProduct) Deliver(ctx context.Context, _ DeliveryRequest) (DeliveryReceipt, error) {
	atomic.AddInt32(&p.calls[4], 1)
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
	atomic.AddInt32(&p.calls[5], 1)
	if ctx.Err() == nil {
		return errors.New("close context was not cancelled")
	}
	if p.closeStart != nil {
		close(p.closeStart)
		<-p.closeEnd
	}
	return nil
}
func (p *fakeProduct) Error() string {
	if p.deliverStart != nil {
		close(p.deliverStart)
		<-p.release
	}
	return "failed exactly"
}
func startHarness(t *testing.T, p *fakeProduct, acknowledge, openNow bool) *rpc.Conn {
	setEnvironment(t, "token", "")
	worker, daemon := net.Pipe()
	w := NewWorker(p)
	w.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		check(t, atomic.LoadInt32(&p.calls[0]) == 1 && network == "unix" && address == "/fixture/socket", "dial before hello or wrong endpoint")
		return worker, nil
	}
	p.worker = w
	hello := make(chan struct{})
	var h *rpc.Conn
	h = rpc.New(daemon, false, func(_ context.Context, request *rpc.Request) {
		switch request.Method {
		case "session.hello":
			if acknowledge {
				must(t, h.Result(request, struct{}{}))
			} else {
				_ = h.Close()
			}
			close(hello)
		case "session.list":
			if p.worker.opened.Load() {
				must(t, h.Result(request, protocol.SessionListResult{Sessions: []protocol.SessionSummary{}}))
			} else {
				must(t, h.Error(request, protocol.NotCommitted, nil))
			}
		}
	})
	go w.Serve(context.Background())
	<-hello
	t.Cleanup(func() { _ = h.Close(); <-w.Closed() })
	if openNow {
		open(t, h)
	}
	return h
}

func async(h *rpc.Conn, method string, params, result any) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.Call(context.Background(), method, params, result) }()
	return done
}
func open(t *testing.T, h *rpc.Conn) {
	var result OpenResult
	check(t, h.Call(context.Background(), "session.open", OpenRequest{Name: "parent/leaf@local", Groups: []string{"session:parent"}, ResumeSessionID: "product-session", Open: OpenOptions{Cwd: "/tmp", PermissionMode: "ask", Model: "model", ReasoningEffort: "high", Arguments: []string{"--flag"}}}, &result) == nil && result.SessionID == "product-session", "open result = %#v", result)
}

func TestWorkerLifecycleTable(t *testing.T) {
	var table struct {
		Cases []struct {
			Name  string   `json:"name"`
			Calls [6]int32 `json:"calls"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile("../../internal/protocol/session-lifecycle.fixtures.json")
	check(t, err == nil && json.Unmarshal(raw, &table) == nil, "read lifecycle table: %v", err)
	for _, row := range table.Cases {
		t.Run(row.Name, func(t *testing.T) {
			check(t, runCase(t, row.Name) == row.Calls, "callback calls did not match %v", row.Calls)
		})
	}
}

func runCase(t *testing.T, name string) [6]int32 {
	p := &fakeProduct{}
	switch name {
	case "ready-open-commit":
		h := startHarness(t, p, true, false)
		wantCode(t, p.worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}), protocol.NotCommitted)
		open(t, h)
		must(t, p.worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}))
	case "describe-eof":
		startHarness(t, p, false, false)
	case "terminal-results":
		h := startHarness(t, p, true, true)
		for input, want := range map[string]TurnResult{"ok": {Outcome: "completed", Result: "ok"}, "interrupted": {Outcome: "interrupted"}, "fail": {Outcome: "failed", Result: "failed exactly"}, "empty": {Outcome: "completed"}, "long": {Outcome: "completed", Result: strings.Repeat("x", protocol.MaxTextRunes), Truncated: true}} {
			var result TurnResult
			check(t, h.Call(context.Background(), "turn.run", protocol.TurnRunRequest{SessionID: "product-session@local", Input: input}, &result) == nil && result == want, "%s result = %#v", input, result)
		}
	case "one-run", "one-interrupt", "full-duplex", "close-during-run", "callback-originated-method", "run-done":
		return blockingCase(t, name)
	case "eof-during-run":
		h := startHarness(t, p, true, true)
		p.started = make(chan *Run)
		run := async(h, "turn.run", protocol.TurnRunRequest{SessionID: "product-session@local", Input: "eof"}, &TurnResult{})
		<-p.started
		_ = h.Close()
		<-p.worker.Closed()
		check(t, <-run != nil, "expected error")
	case "peer-lifetime":
		h := startHarness(t, p, true, false)
		must(t, h.Call(context.Background(), "session.superseded", struct{}{}, &struct{}{}))
		check(t, p.worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}) != nil, "expected error")
		t.Run("connectPeer proofs", func(t *testing.T) { t.Skip("pending connectPeer in commit 3") })
	case "wrong-direction-request":
		h := startHarness(t, p, true, false)
		check(t, h.Call(context.Background(), "session.hello", protocol.WorkerHello{Protocol: 1, LaunchToken: "again", HelloDescription: HelloDescription{Product: "worker", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}}, &struct{}{}) != nil, "expected error")
	case "terminal-before-interrupt":
		p.release, p.deliverStart = make(chan struct{}), make(chan struct{})
		h := startHarness(t, p, true, true)
		terminal := async(h, "turn.run", protocol.TurnRunRequest{SessionID: "product-session@local", Input: "fail"}, &TurnResult{})
		<-p.deliverStart
		wantCode(t, h.Call(context.Background(), "turn.interrupt", target, &struct{}{}), protocol.NotRunning)
		close(p.release)
		must(t, <-terminal)
	case "idle-close-deliver":
		h := startHarness(t, p, true, true)
		p.deliverStart, p.closeStart, p.closeEnd = make(chan struct{}), make(chan struct{}), make(chan struct{})
		delivered := async(h, "message.deliver", delivery, &DeliveryReceipt{})
		<-p.deliverStart
		closed := async(h, "session.close", target, &struct{}{})
		<-p.closeStart
		checkDelivery(t, h, "rejected")
		must(t, <-delivered)
		close(p.closeEnd)
		must(t, <-closed)
	case "environment":
		setEnvironment(t, "token", "key")
		err := NewWorker(p).Serve(context.Background())
		check(t, err != nil && err.Error() == "local key transport not implemented in this build" && os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "" && os.Getenv("AGENTBUS_SOCKET") == "", "key error or uncleared environment: %v", err)
	case "shutdown":
		startHarness(t, p, true, false)
		p.worker.Shutdown()
		p.worker.Shutdown()
		<-p.worker.Closed()
	default:
		t.Fatalf("unknown lifecycle case %q", name)
	}
	return p.calls
}

func blockingCase(t *testing.T, name string) [6]int32 {
	p := &fakeProduct{started: make(chan *Run), release: make(chan struct{})}
	h := startHarness(t, p, true, true)
	run := async(h, "turn.run", protocol.TurnRunRequest{SessionID: "product-session@local", Input: "block"}, &TurnResult{})
	runToken := <-p.started
	switch name {
	case "one-run":
		wantCode(t, h.Call(context.Background(), "turn.run", protocol.TurnRunRequest{SessionID: "product-session@local", Input: "again"}, &TurnResult{}), protocol.Busy)
	case "one-interrupt":
		p.interrupted = make(chan struct{})
		first, second := async(h, "turn.interrupt", target, &struct{}{}), async(h, "turn.interrupt", target, &struct{}{})
		<-p.interrupted
		must(t, errors.Join(<-first, <-second))
	case "full-duplex", "callback-originated-method":
		p.outbound = true
		checkDelivery(t, h, "injected")
	case "close-during-run":
		p.interrupted, p.closeStart, p.closeEnd, p.hangInterrupt = make(chan struct{}), make(chan struct{}), make(chan struct{}), true
		closed := async(h, "session.close", target, &struct{}{})
		<-p.interrupted
		must(t, h.Call(context.Background(), "turn.interrupt", target, &struct{}{}))
		close(p.release)
		must(t, <-run)
		<-p.closeStart
		close(p.closeEnd)
		must(t, <-closed)
		return p.calls
	case "run-done":
		select {
		case <-runToken.Done():
			t.Fatal("Run.Done closed before the terminal response")
		default:
		}
	}
	close(p.release)
	must(t, <-run)
	<-runToken.Done()
	if name == "one-interrupt" {
		check(t, runToken.Native == nil, "native work started after pre-handoff interrupt")
	}
	return p.calls
}

func checkDelivery(t *testing.T, h *rpc.Conn, disposition string) {
	var receipt DeliveryReceipt
	check(t, h.Call(context.Background(), "message.deliver", delivery, &receipt) == nil && receipt.Disposition == disposition, "receipt = %#v", receipt)
}

func setEnvironment(t *testing.T, token, key string) {
	for name, value := range map[string]string{"AGENTBUS_LAUNCH_TOKEN": token, "AGENTBUS_LOCAL_KEY": key, "AGENTBUS_SOCKET": "/fixture/socket"} {
		t.Setenv(name, value)
	}
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
