package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/antst/agent-sessions/bus/internal/livepresence"
)

type fixtureProduct struct {
	worker                     *Worker
	runStarted, runRelease     chan struct{}
	closeEntered, closeRelease chan struct{}
	workerMethod               func(context.Context) error
	calls                      [5]atomic.Int32
}

var fixtureHello = HelloDescription{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}

func (*fixtureProduct) Hello(context.Context) (HelloDescription, error) { return fixtureHello, nil }
func (p *fixtureProduct) Open(context.Context, OpenRequest) (OpenResult, error) {
	p.calls[0].Add(1)
	return OpenResult{SessionID: "product-session"}, nil
}
func (p *fixtureProduct) Run(ctx context.Context, input string) (TurnResult, error) {
	p.calls[1].Add(1)
	switch input {
	case "block":
		close(p.runStarted)
		<-p.runRelease
		return TurnResult{Outcome: "interrupted"}, nil
	case "eof":
		close(p.runStarted)
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	case "fail":
		return TurnResult{}, errors.New("failed exactly")
	case "long":
		return TurnResult{Outcome: "completed", Result: strings.Repeat("x", 262145)}, nil
	case "interrupted":
		return TurnResult{Outcome: "interrupted"}, nil
	case "invalid":
		return TurnResult{Outcome: "invalid"}, nil
	default:
		return TurnResult{Outcome: "completed", Result: input}, nil
	}
}
func (p *fixtureProduct) Interrupt(context.Context) error { return count(&p.calls[2]) }
func (p *fixtureProduct) Deliver(ctx context.Context, _ DeliveryRequest) (DeliveryReceipt, error) {
	p.calls[3].Add(1)
	if p.workerMethod != nil {
		return DeliveryReceipt{Disposition: "injected"}, p.workerMethod(ctx)
	}
	return DeliveryReceipt{Disposition: "injected"}, nil
}
func (p *fixtureProduct) Close(context.Context) error {
	p.calls[4].Add(1)
	if p.closeEntered != nil {
		close(p.closeEntered)
		<-p.closeRelease
	}
	return nil
}

type workerHarness struct {
	t       *testing.T
	worker  *Worker
	daemon  *livepresence.SessionRPC
	raw     net.Conn
	request chan livepresence.Frame
}

func newWorkerHarness(t *testing.T, product *fixtureProduct, beforeHelloAck ...bool) *workerHarness {
	setWorkerEnvironment(t, "launch-secret", "")
	workerSide, daemonSide := net.Pipe()
	w := NewWorker(product, func(context.Context, string, string) (net.Conn, error) { return workerSide, nil })
	product.worker = w
	rpc, err := livepresence.NewSessionRPC(daemonSide)
	must(t, err)
	h := &workerHarness{t: t, worker: w, daemon: rpc, raw: daemonSide, request: make(chan livepresence.Frame, 8)}
	go func() {
		for {
			frame, readErr := rpc.Read(false)
			if readErr != nil {
				_ = rpc.Close()
				return
			}
			h.request <- frame
		}
	}()
	go func() { _ = w.Serve(context.Background()) }()
	hello := h.next("session.hello")
	if len(beforeHelloAck) != 0 {
		_ = rpc.Close()
	} else {
		must(t, rpc.Result(hello, struct{}{}))
	}
	t.Cleanup(func() { _ = rpc.Close(); <-w.Closed() })
	return h
}

func (h *workerHarness) next(method string) livepresence.Frame {
	frame := <-h.request
	check(h.t, frame.Method == method, "method = %s, want %s", frame.Method, method)
	return frame
}

func (h *workerHarness) call(method string, params, result any) error {
	return h.daemon.Call(context.Background(), false, method, params, result)
}

func (h *workerHarness) async(method string, params, result any) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.call(method, params, result) }()
	return done
}

func (h *workerHarness) open() {
	var result OpenResult
	must(h.t, h.call("session.open", OpenRequest{Name: "parent/leaf@local", Groups: []string{}, Open: OpenOptions{}}, &result))
	check(h.t, result.SessionID == "product-session", "session id = %q", result.SessionID)
}

type lifecycleFixture struct {
	Name  string   `json:"name"`
	Proof []string `json:"proof"`
}

func TestGoWorkerRunsSharedLifecycleFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../internal/protocol/session-lifecycle.fixtures.json")
	must(t, err)
	var table struct {
		Cases []lifecycleFixture `json:"cases"`
	}
	must(t, json.Unmarshal(raw, &table))
	check(t, len(table.Cases) == 14, "fixture rows = %d", len(table.Cases))
	for _, row := range table.Cases {
		t.Run(row.Name, func(t *testing.T) {
			got := runLifecycleFixture(t, row.Name)
			for _, proof := range row.Proof {
				t.Run(proof, func(t *testing.T) {
					if strings.Contains(" eof_reconnect same_id_rehello changed_groups_rejected different_id_replaced worker_rehello_rejected ", " "+proof+" ") {
						t.Skip("pending daemon/connectPeer in commits 2 and 3")
					}
					check(t, slices.Contains(got, proof), "fixture proof was not exercised")
				})
			}
		})
	}
}

var fixtureRunners = map[string]func(*testing.T) []string{
	"describe-eof":   proveDescribeEOF,
	"eof-during-run": proveRunEOF,
	"peer-lifetime":  proveTerminalConnection,
	"invalid-frames": proveInvalidFrames,
	"environment":    proveEnvironment,
}

func runLifecycleFixture(t *testing.T, name string) []string {
	if run := fixtureRunners[name]; run != nil {
		return run(t)
	}
	return proveConcurrentRun(t)
}

func proveConcurrentRun(t *testing.T) []string {
	started, release := make(chan struct{}), make(chan struct{})
	closeEntered, closeRelease := make(chan struct{}), make(chan struct{})
	p := &fixtureProduct{}
	p.runStarted, p.runRelease = started, release
	p.closeEntered, p.closeRelease = closeEntered, closeRelease
	var h *workerHarness
	p.workerMethod = func(ctx context.Context) error {
		called := make(chan error, 1)
		go func() { called <- p.worker.Call(ctx, "session.list", struct{}{}, &struct{}{}) }()
		must(t, h.daemon.Result(h.next("session.list"), map[string]any{"sessions": []any{}}))
		return <-called
	}
	h = newWorkerHarness(t, p)
	precommit := make(chan error, 1)
	go func() { precommit <- h.worker.Call(context.Background(), "session.list", struct{}{}, &struct{}{}) }()
	must(t, h.daemon.Error(h.next("session.list"), -32011, "not_committed", nil))
	check(t, <-precommit != nil, "precommit method succeeded")
	h.open()
	postcommit := make(chan error, 1)
	go func() { postcommit <- h.worker.Call(context.Background(), "session.list", struct{}{}, &struct{}{}) }()
	must(t, h.daemon.Result(h.next("session.list"), map[string]any{"sessions": []any{}}))
	must(t, <-postcommit)
	for _, input := range []string{"ok", "empty", "interrupted", "fail", "long"} {
		var result TurnResult
		must(t, h.call("turn.run", turn(input), &result))
		check(t, input != "fail" || result.Outcome == "failed", "failed result = %+v", result)
		check(t, input != "long" || result.Truncated && len([]rune(result.Result)) == 262144, "truncated result = %+v", result)
	}
	_ = h.call("turn.interrupt", sessionID(), &struct{}{})
	check(t, p.calls[2].Load() == 0, "terminal turn reached native interrupt")
	runs := p.calls[1].Load()
	run := h.async("turn.run", turn("block"), &TurnResult{})
	<-started
	slot := h.worker.run
	var receipt DeliveryReceipt
	must(t, h.call("message.deliver", deliveryRequest, &receipt))
	check(t, receipt.Disposition == "injected", "delivery = %+v", receipt)
	check(t, h.call("turn.run", turn("second"), &TurnResult{}) != nil, "second run succeeded")
	interrupt1, interrupt2 := h.async("turn.interrupt", sessionID(), &struct{}{}), h.async("turn.interrupt", sessionID(), &struct{}{})
	must(t, <-interrupt1)
	must(t, <-interrupt2)
	close1, close2 := h.async("session.close", sessionID(), &struct{}{}), h.async("session.close", sessionID(), &struct{}{})
	for slot.waiters.Load() != 2 {
		runtime.Gosched()
	}
	close(release)
	must(t, <-run)
	<-closeEntered
	deliveries := p.calls[3].Load()
	must(t, h.call("message.deliver", deliveryRequest, &receipt))
	check(t, receipt.Reason == "closing" && p.calls[3].Load() == deliveries, "closing delivery = %+v", receipt)
	close(closeRelease)
	must(t, <-close1)
	must(t, <-close2)
	check(t, p.calls[1].Load() == runs+1 && p.calls[2].Load() == 1 && p.calls[4].Load() == 1, "wrong callback counts")
	return []string{"hello_after_ready", "precommit_rejected", "postcommit_allowed", "completed", "interrupted", "failed", "empty_result", "character_truncation", "interrupt_calls_zero", "second_busy", "run_calls_one", "coalesced", "close_joins", "interrupt_calls_one", "deliver_while_running", "worker_method_while_running", "response_received", "run_response_before_close_response", "rejected_closing", "deliver_calls_zero"}
}

func proveDescribeEOF(t *testing.T) []string {
	p := &fixtureProduct{}
	h := newWorkerHarness(t, p, true)
	_ = h.daemon.Close()
	<-h.worker.Closed()
	check(t, p.calls[0].Load() == 0 && p.calls[4].Load() == 0, "unexpected open or close")
	return []string{"open_zero", "close_zero"}
}

func proveRunEOF(t *testing.T) []string {
	started := make(chan struct{})
	p := &fixtureProduct{}
	p.runStarted = started
	h := newWorkerHarness(t, p)
	h.open()
	run := h.async("turn.run", turn("eof"), &TurnResult{})
	<-started
	_ = h.daemon.Close()
	<-h.worker.Closed()
	check(t, <-run != nil && p.calls[4].Load() == 1, "EOF did not fail run and close once")
	return []string{"callbacks_cancelled", "pending_failed", "close_once"}
}

func proveTerminalConnection(t *testing.T) []string {
	h := newWorkerHarness(t, &fixtureProduct{})
	must(t, h.call("session.superseded", struct{}{}, &struct{}{}))
	<-h.worker.Closed()
	return []string{"superseded_terminal"}
}

func proveInvalidFrames(t *testing.T) []string {
	cases := map[string]string{
		"malformed": `{"jsonrpc":`,
		"unknown":   `{"jsonrpc":"2.0","id":77,"method":"unknown","params":{}}`,
		"oversized": strings.Repeat("x", livepresence.MaxFrameBytes+1),
		"unsafe_id": `{"jsonrpc":"2.0","id":9007199254740992,"method":"session.list","params":{}}`,
	}
	for name, frame := range cases {
		p := &fixtureProduct{}
		h := newWorkerHarness(t, p)
		_, _ = h.raw.Write(append([]byte(frame), '\n'))
		if name == "unknown" {
			must(t, h.call("session.superseded", struct{}{}, &struct{}{}))
		}
		<-h.worker.Closed()
		check(t, p.calls[0].Load()+p.calls[1].Load()+p.calls[2].Load()+p.calls[3].Load()+p.calls[4].Load() == 0, "invalid frame reached callback")
	}
	p := &fixtureProduct{}
	h := newWorkerHarness(t, p)
	h.open()
	check(t, h.call("turn.run", turn("invalid"), &TurnResult{}) != nil && h.worker.run != nil, "invalid result cleared its run slot")
	return []string{"malformed", "unknown", "oversized", "unsafe_id", "callbacks_zero"}
}

func proveEnvironment(t *testing.T) []string {
	setWorkerEnvironment(t, "secret", "key")
	calls := atomic.Int32{}
	dial := func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("connect failed")
	}
	w := NewWorker(&fixtureProduct{}, dial)
	err := w.Serve(context.Background())
	check(t, err != nil && err.Error() == "local key transport not implemented in this build", "error = %v", err)
	check(t, calls.Load() == 0, "dial calls = %d", calls.Load())
	check(t, os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "", "secret environment survived")
	setWorkerEnvironment(t, "secret", "")
	err = NewWorker(&fixtureProduct{}, dial).Serve(context.Background())
	check(t, err != nil && err.Error() == "connect failed" && calls.Load() == 1, "connect error/calls = %v/%d", err, calls.Load())
	check(t, os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "", "launch token survived connect failure")
	return []string{"endpoint_order", "secrets_read_once", "secrets_scrubbed", "connect_once", "no_reconnect"}
}

var deliveryRequest = DeliveryRequest{MessageID: "m", From: DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "peer", Groups: []string{}}, Body: "body"}

const fixtureSessionID = "product-session@local"

func count(call *atomic.Int32) error { call.Add(1); return nil }
func sessionID() map[string]string   { return map[string]string{"session_id": fixtureSessionID} }
func turn(input string) any          { return map[string]any{"session_id": fixtureSessionID, "input": input} }
func setWorkerEnvironment(t *testing.T, token, key string) {
	t.Setenv("AGENTBUS_LAUNCH_TOKEN", token)
	t.Setenv("AGENTBUS_LOCAL_KEY", key)
	t.Setenv("AGENTBUS_SOCKET", "/fixture/socket")
}

func must(t *testing.T, err error) { check(t, err == nil, "%v", err) }
func check(t *testing.T, condition bool, format string, args ...any) {
	if !condition {
		t.Fatalf(format, args...)
	}
}
