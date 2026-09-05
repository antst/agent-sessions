// Package sessionkit is the product-agnostic Go implementation of the
// universal Agent Sessions worker contract.
package sessionkit

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/antst/agent-sessions/bus/internal/livepresence"
	"github.com/antst/agent-sessions/bus/internal/stateroot"
)

const maxTextCharacters = 262144

type (
	ExtraArgument   = livepresence.SessionExtraArgument
	Hello           = livepresence.SessionHello
	OpenRequest     = livepresence.SessionOpenRequest
	OpenResult      = livepresence.SessionOpenResult
	TurnResult      = livepresence.SessionTurnResult
	DeliveryRequest = livepresence.SessionDeliveryRequest
	DeliveryReceipt = livepresence.SessionDeliveryReceipt
)

// Product is the six-callback surface implemented by a native product.
type Product interface {
	Hello(context.Context) (Hello, error)
	Open(context.Context, OpenRequest) (OpenResult, error)
	Run(context.Context, string) (TurnResult, error)
	Interrupt(context.Context) error
	Deliver(context.Context, DeliveryRequest) (DeliveryReceipt, error)
	Close(context.Context) error
}

type DialFunc func(context.Context, string, string) (net.Conn, error)

type runSlot struct {
	done                  chan struct{}
	terminal, interrupted bool
}

type Worker struct {
	product Product
	dial    DialFunc

	mu                  sync.Mutex
	rpc                 *livepresence.SessionRPC
	root                context.Context
	cancel              context.CancelFunc
	run                 *runSlot
	openClaimed, opened atomic.Bool
	closing             atomic.Bool
	closeOnce, stopOnce sync.Once
	closeErr            error
	handlers            sync.WaitGroup
	closed              chan struct{}
}

func NewWorker(product Product, dial DialFunc) *Worker {
	if dial == nil {
		dial = plainDial
	}
	return &Worker{product: product, dial: dial, closed: make(chan struct{})}
}

func (w *Worker) Closed() <-chan struct{} { return w.closed }

func (w *Worker) Serve(ctx context.Context) error {
	defer w.stop()
	endpoint, token, key, err := workerEnvironment()
	if err != nil {
		return err
	}
	hello, err := w.product.Hello(ctx)
	if err != nil {
		return err
	}
	connection, err := w.dial(ctx, endpoint, key)
	if err != nil {
		return err
	}
	rpc, err := livepresence.NewSessionRPC(connection)
	if err != nil {
		_ = connection.Close()
		return err
	}
	w.rpc = rpc
	w.root, w.cancel = context.WithCancel(ctx)
	readErr := make(chan error, 1)
	go func() { readErr <- w.read() }()
	request := struct {
		Protocol    int    `json:"protocol"`
		LaunchToken string `json:"launch_token"`
		Hello
	}{1, token, hello}
	if err = rpc.Call(ctx, true, "session.hello", request, &struct{}{}); err == nil {
		err = <-readErr
	}
	w.cancel()
	w.handlers.Wait()
	_ = w.closeProduct()
	if w.closing.Load() {
		return nil
	}
	return err
}

func (w *Worker) Call(ctx context.Context, method string, params, result any) error {
	return w.rpc.Call(ctx, true, method, params, result)
}

func (w *Worker) read() error {
	for {
		frame, err := w.rpc.Read(true)
		if err != nil {
			return err
		}
		if frame.Method == "session.superseded" {
			_ = w.rpc.Result(frame, struct{}{})
			return errors.New("session superseded")
		}
		w.handlers.Add(1)
		go func() { defer w.handlers.Done(); w.handle(frame) }()
	}
}

func (w *Worker) handle(frame livepresence.Frame) {
	switch frame.Method {
	case "session.open":
		w.handleOpen(frame)
	case "turn.run":
		w.handleRun(frame)
	case "turn.interrupt":
		w.handleInterrupt(frame)
	case "message.deliver":
		w.handleDeliver(frame)
	case "session.close":
		w.handleClose(frame)
	}
}

func (w *Worker) handleOpen(frame livepresence.Frame) {
	var request OpenRequest
	_ = livepresence.DecodeStrict(frame.Params, &request)
	if !w.openClaimed.CompareAndSwap(false, true) || w.closing.Load() {
		_ = w.rpc.Error(frame, livepresence.SessionInvalidFrame, "invalid_frame", nil)
		return
	}
	result, err := w.product.Open(w.root, request)
	if err != nil {
		_ = w.rpc.Error(frame, livepresence.SessionSpawnFailed, "spawn_failed", map[string]any{"stderr_tail": []string{err.Error()}})
		return
	}
	w.opened.Store(true)
	_ = w.rpc.Result(frame, result)
}

func (w *Worker) handleRun(frame livepresence.Frame) {
	var request struct {
		SessionID string `json:"session_id"`
		Input     string `json:"input"`
	}
	_ = livepresence.DecodeStrict(frame.Params, &request)
	slot := &runSlot{done: make(chan struct{})}
	w.mu.Lock()
	busy := !w.opened.Load() || w.run != nil && !w.run.terminal || w.closing.Load()
	if !busy {
		w.run = slot
	}
	w.mu.Unlock()
	if busy {
		_ = w.rpc.Error(frame, livepresence.SessionBusy, "busy", nil)
		return
	}
	result, err := w.product.Run(w.root, request.Input)
	if err != nil {
		result = TurnResult{Outcome: "failed", Result: err.Error()}
	}
	result.Result, result.Truncated = truncate(result.Result)
	w.mu.Lock()
	slot.terminal = true
	w.mu.Unlock()
	_ = w.rpc.Result(frame, result)
	w.mu.Lock()
	if w.run == slot {
		w.run = nil
	}
	close(slot.done)
	w.mu.Unlock()
}

func (w *Worker) handleInterrupt(frame livepresence.Frame) {
	w.mu.Lock()
	slot := w.run
	if slot == nil {
		w.mu.Unlock()
		_ = w.rpc.Error(frame, livepresence.SessionNotRunning, "not_running", nil)
		return
	}
	call := !slot.terminal && !slot.interrupted
	if call {
		slot.interrupted = true
	}
	w.mu.Unlock()
	if call {
		if err := w.product.Interrupt(w.root); err != nil {
			_ = w.rpc.Error(frame, livepresence.SessionInternal, "internal", nil)
			return
		}
	}
	_ = w.rpc.Result(frame, struct{}{})
}

func (w *Worker) handleDeliver(frame livepresence.Frame) {
	if w.closing.Load() {
		_ = w.rpc.Result(frame, DeliveryReceipt{Disposition: "rejected", Reason: "closing"})
		return
	}
	var request DeliveryRequest
	_ = livepresence.DecodeStrict(frame.Params, &request)
	receipt, err := w.product.Deliver(w.root, request)
	if err != nil {
		receipt = DeliveryReceipt{Disposition: "rejected", Reason: err.Error()}
	}
	_ = w.rpc.Result(frame, receipt)
}

func (w *Worker) handleClose(frame livepresence.Frame) {
	if !w.closing.CompareAndSwap(false, true) {
		_ = w.rpc.Error(frame, livepresence.SessionBusy, "busy", nil)
		return
	}
	w.mu.Lock()
	slot := w.run
	interrupt := slot != nil && !slot.terminal && !slot.interrupted
	if interrupt {
		slot.interrupted = true
	}
	w.mu.Unlock()
	if interrupt {
		go w.product.Interrupt(w.root)
	}
	if slot != nil {
		<-slot.done
	}
	if err := w.closeProduct(); err != nil {
		_ = w.rpc.Error(frame, livepresence.SessionInternal, "internal", nil)
	} else {
		_ = w.rpc.Result(frame, struct{}{})
	}
	w.stop()
}

func (w *Worker) closeProduct() error {
	w.closeOnce.Do(func() {
		if w.opened.Load() {
			w.closeErr = w.product.Close(w.root)
		}
	})
	return w.closeErr
}

func (w *Worker) stop() {
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
		if w.rpc != nil {
			_ = w.rpc.Close()
		}
		close(w.closed)
	})
}

func workerEnvironment() (endpoint, token, key string, err error) {
	token, ok := os.LookupEnv("AGENT_SESSIONS_LAUNCH_TOKEN")
	key = os.Getenv("AGENT_SESSIONS_LOCAL_KEY")
	_ = os.Unsetenv("AGENT_SESSIONS_LAUNCH_TOKEN")
	_ = os.Unsetenv("AGENT_SESSIONS_LOCAL_KEY")
	if !ok || token == "" {
		return "", "", "", errors.New("launch token is required")
	}
	endpoint = os.Getenv("AGENT_SESSIONS_SOCKET")
	if endpoint == "" {
		endpoint, err = stateroot.SessionSocket()
	}
	return endpoint, token, key, nil
}

func plainDial(ctx context.Context, endpoint, key string) (net.Conn, error) {
	if key != "" {
		return nil, errors.New("daemon requires local TLS support")
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}

func truncate(text string) (string, bool) {
	characters := []rune(text)
	if len(characters) <= maxTextCharacters {
		return text, false
	}
	return string(characters[:maxTextCharacters]), true
}
