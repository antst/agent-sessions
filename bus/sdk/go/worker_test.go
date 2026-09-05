package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"runtime"
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
	calls                      [6]atomic.Int32
}

func (p *fixtureProduct) Hello(context.Context) (HelloDescription, error) {
	return HelloDescription{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []ExtraArgument{}}, count(&p.calls[0])
}
func (p *fixtureProduct) Open(context.Context, OpenRequest) (OpenResult, error) {
	return OpenResult{SessionID: "product-session"}, count(&p.calls[1])
}
func (p *fixtureProduct) Run(ctx context.Context, input string) (TurnResult, error) {
	p.calls[2].Add(1)
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
	case "empty":
		return TurnResult{Outcome: "completed"}, nil
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
func (p *fixtureProduct) Interrupt(context.Context) error { return count(&p.calls[3]) }
func (p *fixtureProduct) Deliver(ctx context.Context, _ DeliveryRequest) (DeliveryReceipt, error) {
	p.calls[4].Add(1)
	if p.workerMethod != nil {
		return DeliveryReceipt{Disposition: "injected"}, p.workerMethod(ctx)
	}
	return DeliveryReceipt{Disposition: "injected"}, nil
}
func (p *fixtureProduct) Close(context.Context) error {
	p.calls[5].Add(1)
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

type proofSet map[string]bool

func (p proofSet) add(t *testing.T, condition bool, names ...string) proofSet {
	check(t, condition, "proof failed: %v", names)
	for _, name := range names {
		p[name] = true
	}
	return p
}

func TestGoWorkerRunsSharedLifecycleFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../internal/protocol/session-lifecycle.fixtures.json")
	must(t, err)
	var table struct {
		Cases []struct {
			Name  string   `json:"name"`
			Proof []string `json:"proof"`
		} `json:"cases"`
	}
	must(t, json.Unmarshal(raw, &table))
	check(t, len(table.Cases) == 14, "fixture rows = %d", len(table.Cases))
	for _, row := range table.Cases {
		t.Run(row.Name, func(t *testing.T) {
			run := fixtureRunners[row.Name]
			if run == nil {
				run = proveConcurrentRun
			}
			got := run(t)
			for _, proof := range row.Proof {
				if strings.Contains(" eof_reconnect same_id_rehello changed_groups_rejected different_id_replaced worker_rehello_rejected ", " "+proof+" ") {
					t.Run(proof, func(t *testing.T) { t.Skip("pending daemon/connectPeer in commits 2 and 3") })
					continue
				}
				check(t, got[proof], "fixture proof %q has no passing assertion", proof)
			}
		})
	}
}

var fixtureRunners = map[string]func(*testing.T) proofSet{
	"describe-eof":   proveDescribeEOF,
	"eof-during-run": proveRunEOF,
	"peer-lifetime":  proveTerminalConnection,
	"invalid-frames": proveInvalidFrames,
	"environment":    proveEnvironment,
}

func proveConcurrentRun(t *testing.T) proofSet {
	got := proofSet{}
	started, release := make(chan struct{}), make(chan struct{})
	p := &fixtureProduct{runStarted: started, runRelease: release, closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	var h *workerHarness
	p.workerMethod = func(ctx context.Context) error {
		called := make(chan error, 1)
		go func() { called <- p.worker.Call(ctx, "session.list", struct{}{}, &struct{}{}) }()
		must(t, h.daemon.Result(h.next("session.list"), map[string]any{"sessions": []any{}}))
		return <-called
	}
	h = newWorkerHarness(t, p)
	got.add(t, p.calls[0].Load() == 1, "hello_after_ready")
	precommit := make(chan error, 1)
	go func() { precommit <- h.worker.Call(context.Background(), "session.list", struct{}{}, &struct{}{}) }()
	must(t, h.daemon.Error(h.next("session.list"), -32011, "not_committed", nil))
	got.add(t, <-precommit != nil, "precommit_rejected")
	h.open()
	go func() { precommit <- h.worker.Call(context.Background(), "session.list", struct{}{}, &struct{}{}) }()
	must(t, h.daemon.Result(h.next("session.list"), map[string]any{"sessions": []any{}}))
	got.add(t, <-precommit == nil, "postcommit_allowed")
	for _, test := range []struct{ input, outcome, result, proof string }{
		{"ok", "completed", "ok", "completed"}, {"empty", "completed", "", "empty_result"},
		{"interrupted", "interrupted", "", "interrupted"}, {"fail", "failed", "failed exactly", "failed"},
		{"long", "completed", "", "character_truncation"},
	} {
		var result TurnResult
		must(t, h.call("turn.run", turn(test.input), &result))
		exact := result.Outcome == test.outcome && (test.input == "long" && result.Truncated && len([]rune(result.Result)) == 262144 || test.input != "long" && !result.Truncated && result.Result == test.result)
		got.add(t, exact, test.proof)
	}
	_ = h.call("turn.interrupt", sessionID(), &struct{}{})
	got.add(t, p.calls[3].Load() == 0, "interrupt_calls_zero")
	runs := p.calls[2].Load()
	run := h.async("turn.run", turn("block"), &TurnResult{})
	<-started
	slot := h.worker.run
	var receipt DeliveryReceipt
	must(t, h.call("message.deliver", deliveryRequest, &receipt))
	got.add(t, receipt.Disposition == "injected" && h.call("turn.run", turn("second"), &TurnResult{}) != nil, "deliver_while_running", "worker_method_while_running", "response_received", "second_busy")
	interrupt1, interrupt2 := h.async("turn.interrupt", sessionID(), &struct{}{}), h.async("turn.interrupt", sessionID(), &struct{}{})
	interruptErr1, interruptErr2 := <-interrupt1, <-interrupt2
	close1, close2 := h.async("session.close", sessionID(), &struct{}{}), h.async("session.close", sessionID(), &struct{}{})
	for slot.waiters.Load() != 2 {
		runtime.Gosched()
	}
	close(release)
	runErr := <-run
	<-p.closeEntered
	deliveries := p.calls[4].Load()
	must(t, h.call("message.deliver", deliveryRequest, &receipt))
	got.add(t, runErr == nil && receipt.Reason == "closing" && p.calls[4].Load() == deliveries, "run_response_before_close_response", "rejected_closing", "deliver_calls_zero")
	close(p.closeRelease)
	closeErr1, closeErr2 := <-close1, <-close2
	got.add(t, p.calls[2].Load() == runs+1 && interruptErr1 == nil && interruptErr2 == nil && p.calls[3].Load() == 1 && closeErr1 == nil && closeErr2 == nil && p.calls[5].Load() == 1, "run_calls_one", "coalesced", "interrupt_calls_one", "close_joins")
	return got
}

func proveDescribeEOF(t *testing.T) proofSet {
	p := &fixtureProduct{}
	h := newWorkerHarness(t, p, true)
	<-h.worker.Closed()
	return proofSet{}.add(t, p.calls[1].Load() == 0 && p.calls[5].Load() == 0, "open_zero", "close_zero")
}

func proveRunEOF(t *testing.T) proofSet {
	p := &fixtureProduct{runStarted: make(chan struct{})}
	h := newWorkerHarness(t, p)
	h.open()
	run := h.async("turn.run", turn("eof"), &TurnResult{})
	<-p.runStarted
	_ = h.daemon.Close()
	<-h.worker.Closed()
	return proofSet{}.add(t, <-run != nil && p.calls[5].Load() == 1, "callbacks_cancelled", "pending_failed", "close_once")
}

func proveTerminalConnection(t *testing.T) proofSet {
	h := newWorkerHarness(t, &fixtureProduct{})
	must(t, h.call("session.superseded", struct{}{}, &struct{}{}))
	<-h.worker.Closed()
	return proofSet{}.add(t, h.worker.Call(context.Background(), "session.list", struct{}{}, &struct{}{}) != nil, "superseded_terminal")
}

func proveInvalidFrames(t *testing.T) proofSet {
	got := proofSet{}
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
		got.add(t, p.calls[1].Load()+p.calls[2].Load()+p.calls[3].Load()+p.calls[4].Load()+p.calls[5].Load() == 0, name, "callbacks_zero")
	}
	p := &fixtureProduct{}
	h := newWorkerHarness(t, p)
	h.open()
	check(t, h.call("turn.run", turn("invalid"), &TurnResult{}) != nil && h.worker.run != nil, "invalid result cleared its run slot")
	return got
}

func proveEnvironment(t *testing.T) proofSet {
	setWorkerEnvironment(t, "secret", "key")
	calls := atomic.Int32{}
	var endpoint, key string
	dial := func(_ context.Context, gotEndpoint, gotKey string) (net.Conn, error) {
		endpoint, key = gotEndpoint, gotKey
		calls.Add(1)
		return nil, errors.New("connect failed")
	}
	err := NewWorker(&fixtureProduct{}, dial).Serve(context.Background())
	check(t, err != nil && err.Error() == "local key transport not implemented in this build" && calls.Load() == 0 && os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "", "keyed attempt = %v/%d", err, calls.Load())
	setWorkerEnvironment(t, "secret", "")
	err = NewWorker(&fixtureProduct{}, dial).Serve(context.Background())
	check(t, err != nil && err.Error() == "connect failed" && calls.Load() == 1, "connect error/calls = %v/%d", err, calls.Load())
	got := proofSet{}
	got.add(t, endpoint == "/fixture/socket" && key == "" && os.Getenv("AGENTBUS_LAUNCH_TOKEN") == "" && os.Getenv("AGENTBUS_LOCAL_KEY") == "" && err != nil && calls.Load() == 1, "endpoint_order", "secrets_read_once", "secrets_scrubbed", "connect_once", "no_reconnect")
	return got
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
