// Package sessionkit is the product-agnostic Go implementation of the
// universal Agentbus worker contract.
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

// WorkerCallbacks is the six-callback surface implemented by a native product.
type WorkerCallbacks interface {
	Hello(context.Context) (HelloDescription, error)
	Open(context.Context, OpenRequest) (OpenResult, error)
	Run(context.Context, string) (TurnResult, error)
	Interrupt(context.Context) error
	Deliver(context.Context, DeliveryRequest) (DeliveryReceipt, error)
	Close(context.Context) error
}

type DialFunc func(context.Context, string, string) (net.Conn, error)

type runSlot struct {
	runDone, closeDone    chan struct{}
	terminal, interrupted bool
	closeErr              error
	waiters               atomic.Int32
}

type Worker struct {
	product WorkerCallbacks
	dial    DialFunc

	mu                    sync.Mutex
	rpc                   *livepresence.SessionRPC
	root                  context.Context
	cancel                context.CancelFunc
	run                   *runSlot
	openClaimed           bool
	opened                atomic.Bool
	closeOnce, finishOnce sync.Once
	closeErr              error
	handlers              sync.WaitGroup
	fatal                 chan error
	closed                chan struct{}
}

func NewWorker(product WorkerCallbacks, dial DialFunc) *Worker {
	if dial == nil {
		dial = plainDial
	}
	return &Worker{product: product, dial: dial, fatal: make(chan error, 1), closed: make(chan struct{})}
}

func (w *Worker) Closed() <-chan struct{} { return w.closed }

func (w *Worker) Serve(ctx context.Context) error {
	defer close(w.closed)
	endpoint, token, key, err := workerEnvironment()
	if err != nil {
		return err
	}
	if key != "" {
		return errors.New("local key transport not implemented in this build")
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
	go func() { w.finish(w.read()) }()
	request := struct {
		Protocol    int    `json:"protocol"`
		LaunchToken string `json:"launch_token"`
		HelloDescription
	}{1, token, hello}
	if err = rpc.Call(ctx, true, "session.hello", request, &struct{}{}); err == nil {
		err = <-w.fatal
	}
	w.cancel()
	_ = rpc.Close()
	w.handlers.Wait()
	_ = w.closeProduct()
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
		w.mu.Lock()
		switch frame.Method {
		case "session.open":
			invalid := w.openClaimed || w.run != nil
			w.openClaimed = true
			w.dispatch(func() { w.handleOpen(frame, invalid) })
		case "turn.run":
			busy := !w.opened.Load() || w.run != nil
			var slot *runSlot
			if !busy {
				slot = &runSlot{runDone: make(chan struct{})}
				w.run = slot
			}
			w.dispatch(func() { w.handleRun(frame, slot, busy) })
		case "turn.interrupt":
			missing := w.run == nil || w.run.runDone == nil
			call := !missing && !w.run.terminal && !w.run.interrupted
			if call {
				w.run.interrupted = true
			}
			w.dispatch(func() { w.handleInterrupt(frame, call, missing) })
		case "message.deliver":
			closing := w.run != nil && w.run.closeDone != nil
			w.dispatch(func() { w.handleDeliver(frame, closing) })
		case "session.close":
			if w.run == nil {
				w.run = &runSlot{}
			}
			slot, owner := w.run, w.run.closeDone == nil
			if owner {
				slot.closeDone = make(chan struct{})
			}
			slot.waiters.Add(1)
			interrupt := owner && slot.runDone != nil && !slot.terminal && !slot.interrupted
			slot.interrupted = slot.interrupted || interrupt
			w.dispatch(func() { w.handleClose(frame, slot, owner, interrupt) })
		}
		w.mu.Unlock()
	}
}

func (w *Worker) dispatch(callback func()) {
	w.handlers.Add(1)
	go func() { defer w.handlers.Done(); callback() }()
}

func (w *Worker) handleOpen(frame livepresence.Frame, invalid bool) {
	var request OpenRequest
	_ = livepresence.DecodeStrict(frame.Params, &request)
	if invalid {
		w.reply(w.rpc.Error(frame, livepresence.SessionInvalidFrame, "invalid_frame", nil))
		return
	}
	result, err := w.product.Open(w.root, request)
	if err != nil {
		w.reply(w.rpc.Error(frame, livepresence.SessionSpawnFailed, "spawn_failed", map[string]any{"stderr_tail": []string{err.Error()}}))
		return
	}
	w.opened.Store(true)
	w.reply(w.rpc.Result(frame, result))
}

func (w *Worker) handleRun(frame livepresence.Frame, slot *runSlot, busy bool) {
	var request struct {
		SessionID string `json:"session_id"`
		Input     string `json:"input"`
	}
	_ = livepresence.DecodeStrict(frame.Params, &request)
	if busy {
		w.reply(w.rpc.Error(frame, livepresence.SessionBusy, "busy", nil))
		return
	}
	result, err := w.product.Run(w.root, request.Input)
	if err != nil {
		result = TurnResult{Outcome: "failed", Result: err.Error()}
	}
	result.Result, result.Truncated = truncate(result.Result)
	w.mu.Lock()
	slot.terminal = true
	err = w.rpc.Result(frame, result)
	close(slot.runDone)
	if err == nil && w.run == slot && slot.closeDone == nil {
		w.run = nil
	}
	w.mu.Unlock()
	if err != nil {
		w.finish(err)
	}
}

func (w *Worker) handleInterrupt(frame livepresence.Frame, call, missing bool) {
	if missing {
		w.reply(w.rpc.Error(frame, livepresence.SessionNotRunning, "not_running", nil))
		return
	}
	if call {
		if err := w.product.Interrupt(w.root); err != nil {
			w.reply(w.rpc.Error(frame, livepresence.SessionInternal, "internal", nil))
			return
		}
	}
	w.reply(w.rpc.Result(frame, struct{}{}))
}

func (w *Worker) handleDeliver(frame livepresence.Frame, closing bool) {
	if closing {
		w.reply(w.rpc.Result(frame, DeliveryReceipt{Disposition: "rejected", Reason: "closing"}))
		return
	}
	var request DeliveryRequest
	_ = livepresence.DecodeStrict(frame.Params, &request)
	receipt, err := w.product.Deliver(w.root, request)
	if err != nil {
		receipt = DeliveryReceipt{Disposition: "rejected", Reason: err.Error()}
	}
	w.reply(w.rpc.Result(frame, receipt))
}

func (w *Worker) handleClose(frame livepresence.Frame, slot *runSlot, owner, interrupt bool) {
	if owner {
		if interrupt {
			go w.product.Interrupt(w.root)
		}
		if slot.runDone != nil {
			<-slot.runDone
		}
		err := w.closeProduct()
		slot.closeErr = err
		close(slot.closeDone)
	}
	<-slot.closeDone
	var replyErr error
	if slot.closeErr != nil {
		replyErr = w.rpc.Error(frame, livepresence.SessionInternal, "internal", nil)
	} else {
		replyErr = w.rpc.Result(frame, struct{}{})
	}
	last := slot.waiters.Add(-1) == 0
	if replyErr != nil || last {
		w.finish(replyErr)
	}
}

func (w *Worker) finish(err error) {
	w.finishOnce.Do(func() {
		w.fatal <- err
		_ = w.rpc.Close()
	})
}

func (w *Worker) reply(err error) {
	if err != nil {
		w.finish(err)
	}
}

func (w *Worker) closeProduct() error {
	w.closeOnce.Do(func() {
		if w.opened.Load() {
			w.closeErr = w.product.Close(w.root)
		}
	})
	return w.closeErr
}

func workerEnvironment() (endpoint, token, key string, err error) {
	token, ok := os.LookupEnv("AGENTBUS_LAUNCH_TOKEN")
	key = os.Getenv("AGENTBUS_LOCAL_KEY")
	_ = os.Unsetenv("AGENTBUS_LAUNCH_TOKEN")
	_ = os.Unsetenv("AGENTBUS_LOCAL_KEY")
	if !ok || token == "" {
		return "", "", "", errors.New("launch token is required")
	}
	endpoint = os.Getenv("AGENTBUS_SOCKET")
	if endpoint == "" {
		endpoint, err = stateroot.SessionSocket()
	}
	return endpoint, token, key, nil
}

func plainDial(ctx context.Context, endpoint, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}

func truncate(text string) (string, bool) {
	characters := []rune(text)
	if len(characters) <= 262144 {
		return text, false
	}
	return string(characters[:262144]), true
}
