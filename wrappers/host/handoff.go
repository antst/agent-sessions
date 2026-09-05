package host

import (
	"context"
	"errors"
	"strings"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

const (
	MaxQueuedDeliveries = 64
	MaxQueuedBytes      = 1 << 20
)

var ErrQueueFull = errors.New("queue_full")

// Turn is returned only after the product confirms that a native turn exists.
type Turn interface {
	Wait(context.Context) (sessionkit.TurnResult, error)
	Inject(context.Context, string) (bool, error)
	Interrupt(context.Context) error
}

type StartTurn func(context.Context, string) (Turn, error)

type creation struct {
	done         chan struct{}
	interrupted  bool
	interruptErr error
}

// Handoff owns the only boundary between queued delivery and a native turn.
type Handoff struct {
	mu        sync.Mutex
	queue     []string
	queueSize int
	starting  *creation
	active    Turn
	pending   bool
}

func (h *Handoff) Run(ctx context.Context, input string, start StartTurn) (sessionkit.TurnResult, error) {
	h.mu.Lock()
	if h.starting != nil || h.active != nil {
		h.mu.Unlock()
		return sessionkit.TurnResult{}, errors.New("turn already active")
	}
	queued := append([]string(nil), h.queue...)
	starting := &creation{done: make(chan struct{}), interrupted: h.pending}
	h.pending = false
	h.starting = starting
	prompt := prepend(queued, input)
	h.mu.Unlock()

	turn, err := start(ctx, prompt)
	h.mu.Lock()
	h.starting = nil
	if err == nil && turn == nil {
		err = errors.New("native turn was not returned")
	}
	if err == nil {
		h.queue = append([]string(nil), h.queue[len(queued):]...)
		h.queueSize = len(strings.Join(h.queue, "\n"))
		h.active = turn
		if starting.interrupted {
			starting.interruptErr = turn.Interrupt(ctx)
		}
	}
	close(starting.done)
	h.mu.Unlock()
	if err != nil {
		if starting.interrupted {
			return sessionkit.TurnResult{Outcome: "interrupted"}, nil
		}
		return sessionkit.TurnResult{}, err
	}

	result, err := turn.Wait(ctx)
	h.mu.Lock()
	if h.active == turn {
		h.active = nil
	}
	h.mu.Unlock()
	return result, err
}

func (h *Handoff) Deliver(ctx context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	rendered, err := render(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active != nil {
		injected, injectErr := h.active.Inject(ctx, rendered)
		if injectErr != nil {
			return sessionkit.DeliveryReceipt{}, injectErr
		}
		if injected {
			return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
		}
	}
	if len(h.queue) == MaxQueuedDeliveries || h.queueSize+separator(h.queue)+len(rendered) > MaxQueuedBytes {
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: ErrQueueFull.Error()}, nil
	}
	h.queueSize += separator(h.queue) + len(rendered)
	h.queue = append(h.queue, rendered)
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func (h *Handoff) Interrupt(ctx context.Context) error {
	h.mu.Lock()
	if h.starting != nil {
		starting := h.starting
		starting.interrupted = true
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-starting.done:
			return starting.interruptErr
		}
	}
	if h.active != nil {
		err := h.active.Interrupt(ctx)
		h.mu.Unlock()
		return err
	}
	// The worker run slot may be installed before its Run callback is scheduled.
	h.pending = true
	h.mu.Unlock()
	return nil
}

func prepend(messages []string, input string) string {
	if len(messages) == 0 {
		return input
	}
	prefix := strings.Join(messages, "\n")
	if input == "" {
		return prefix
	}
	return prefix + "\n" + input
}

func separator(messages []string) int {
	if len(messages) > 0 {
		return 1
	}
	return 0
}
