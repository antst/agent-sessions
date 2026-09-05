// Package sessionkit is the product-agnostic Go implementation of Agentbus.
package sessionkit

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

type WorkerCallbacks interface {
	Hello(context.Context) (HelloDescription, error)
	Open(context.Context, OpenRequest) (OpenResult, error)
	Run(context.Context, string) (TurnResult, error)
	Interrupt(context.Context) error
	Deliver(context.Context, DeliveryRequest) (DeliveryReceipt, error)
	Close(context.Context) error
}

type runSlot struct {
	done      chan struct{}
	interrupt bool
}

type Worker struct {
	product WorkerCallbacks
	dial    func(context.Context, string, string) (net.Conn, error)
	mu      sync.Mutex
	conn    *rpc.Conn
	context context.Context
	cancel  context.CancelFunc
	run     *runSlot
	opened  atomic.Bool
	once    sync.Once
	closed  chan struct{}
}

func NewWorker(product WorkerCallbacks) *Worker {
	return &Worker{product: product, dial: (&net.Dialer{}).DialContext, closed: make(chan struct{})}
}

func (w *Worker) Closed() <-chan struct{} { return w.closed }

func (w *Worker) Serve(ctx context.Context) error {
	defer close(w.closed)
	endpoint, token, key, err := workerEnvironment()
	if err != nil || key != "" {
		if err == nil {
			err = errors.New("local key transport not implemented in this build")
		}
		return err
	}
	hello, err := w.product.Hello(ctx)
	if err != nil {
		return err
	}
	fd, err := w.dial(ctx, "unix", endpoint)
	if err != nil {
		return err
	}
	w.conn = rpc.New(fd, true, w.handle)
	w.context, w.cancel = context.WithCancel(w.conn.Context())
	request := protocol.WorkerHello{Protocol: 1, LaunchToken: token, HelloDescription: hello}
	if err = w.conn.Call(ctx, "session.hello", request, &struct{}{}); err == nil {
		<-w.conn.Done()
		err = rpc.ErrClosed
	}
	_ = w.conn.Close()
	w.closeProduct(w.conn.Context())
	return err
}

func (w *Worker) Call(ctx context.Context, method string, params, result any) error {
	return w.conn.Call(ctx, method, params, result)
}

func (w *Worker) handle(_ context.Context, request *rpc.Request) {
	ctx := w.context
	switch request.Method {
	case "session.superseded":
		w.reply(w.conn.Result(request, struct{}{}))
		_ = w.conn.Close()
	case "session.open":
		result, err := w.product.Open(ctx, *request.Params.(*OpenRequest))
		if err != nil {
			w.reply(w.conn.Error(request, protocol.SpawnFailed, map[string]any{"stderr_tail": []string{err.Error()}}))
			return
		}
		w.opened.Store(true)
		w.reply(w.conn.Result(request, result))
	case "turn.run":
		w.mu.Lock()
		if !w.opened.Load() || w.run != nil {
			w.mu.Unlock()
			w.reply(w.conn.Error(request, protocol.Busy, nil))
			return
		}
		slot := &runSlot{done: make(chan struct{})}
		w.run = slot
		w.mu.Unlock()
		result, err := w.product.Run(ctx, request.Params.(*protocol.TurnRunRequest).Input)
		if err != nil {
			result = TurnResult{Outcome: "failed", Result: err.Error()}
		}
		result.Result, result.Truncated = truncate(result.Result)
		w.mu.Lock()
		if _, err = protocol.EncodeResult("turn.run", result); err == nil {
			err = w.conn.Result(request, result)
		}
		if err == nil {
			if w.run == slot {
				w.run = nil
			}
		}
		close(slot.done)
		w.mu.Unlock()
		w.reply(err)
	case "turn.interrupt":
		w.mu.Lock()
		if w.run == nil {
			w.mu.Unlock()
			w.reply(w.conn.Error(request, protocol.NotRunning, nil))
			return
		}
		call := !w.run.interrupt
		w.run.interrupt = w.run.interrupt || call
		w.mu.Unlock()
		if call {
			if err := w.product.Interrupt(ctx); err != nil {
				w.reply(w.conn.Error(request, protocol.Internal, nil))
				return
			}
		}
		w.reply(w.conn.Result(request, struct{}{}))
	case "message.deliver":
		w.mu.Lock()
		closing := w.run != nil && w.run.done == nil
		w.mu.Unlock()
		if closing {
			w.reply(w.conn.Result(request, DeliveryReceipt{Disposition: "rejected", Reason: "closing"}))
			return
		}
		receipt, err := w.product.Deliver(ctx, *request.Params.(*DeliveryRequest))
		if err != nil {
			receipt = DeliveryReceipt{Disposition: "rejected", Reason: err.Error()}
		}
		w.reply(w.conn.Result(request, receipt))
	case "session.close":
		w.mu.Lock()
		slot := w.run
		if slot != nil && slot.done == nil {
			w.mu.Unlock()
			_ = w.conn.Close()
			return
		}
		w.run = &runSlot{interrupt: true}
		interrupt := slot != nil && !slot.interrupt
		if interrupt {
			slot.interrupt = true
		}
		w.mu.Unlock()
		if interrupt {
			go w.product.Interrupt(ctx)
		}
		if slot != nil {
			<-slot.done
		}
		w.cancel()
		if err := w.closeProduct(ctx); err != nil {
			w.reply(w.conn.Error(request, protocol.Internal, nil))
		} else {
			w.reply(w.conn.Result(request, struct{}{}))
		}
		_ = w.conn.Close()
	}
}

func (w *Worker) closeProduct(ctx context.Context) error {
	var err error
	w.once.Do(func() {
		if w.opened.Load() {
			err = w.product.Close(ctx)
		}
	})
	return err
}

func (w *Worker) reply(err error) {
	if err != nil {
		_ = w.conn.Close()
	}
}

func workerEnvironment() (string, string, string, error) {
	token, ok := os.LookupEnv("AGENTBUS_LAUNCH_TOKEN")
	key, endpoint := os.Getenv("AGENTBUS_LOCAL_KEY"), os.Getenv("AGENTBUS_SOCKET")
	for _, name := range []string{"AGENTBUS_LAUNCH_TOKEN", "AGENTBUS_LOCAL_KEY", "AGENTBUS_SOCKET"} {
		_ = os.Unsetenv(name)
	}
	if !ok || token == "" {
		return "", "", "", errors.New("launch token is required")
	}
	if endpoint == "" {
		return "", "", "", errors.New("agentbus socket is required")
	}
	return endpoint, token, key, nil
}

func truncate(text string) (string, bool) {
	characters := []rune(text)
	return string(characters[:min(len(characters), protocol.MaxTextRunes)]), len(characters) > protocol.MaxTextRunes
}
