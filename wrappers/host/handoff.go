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

// Handoff owns the only boundary between queued delivery and a native turn.
type Handoff struct {
	mu        sync.Mutex
	queue     []string
	queueSize int
	starting  context.CancelFunc
	active    Turn
	pending   bool
}

func (h *Handoff) Run(ctx context.Context, input string, start StartTurn) (sessionkit.TurnResult, error) {
	h.mu.Lock()
	if h.starting != nil || h.active != nil {
		h.mu.Unlock()
		return sessionkit.TurnResult{}, errors.New("turn already active")
	}
	if h.pending {
		h.pending = false
		h.mu.Unlock()
		return sessionkit.TurnResult{Outcome: "interrupted"}, nil
	}
	queued := h.queue
	h.queue, h.queueSize = nil, 0
	startCtx, cancel := context.WithCancel(ctx)
	h.starting = cancel
	prompt := prepend(queued, input)
	h.mu.Unlock()

	turn, err := start(startCtx, prompt)
	h.mu.Lock()
	h.starting = nil
	if err != nil || startCtx.Err() != nil {
		h.requeueFront(queued)
		h.mu.Unlock()
		cancel()
		if startCtx.Err() != nil {
			return sessionkit.TurnResult{Outcome: "interrupted"}, nil
		}
		return sessionkit.TurnResult{}, err
	}
	h.active = turn
	h.mu.Unlock()
	cancel()

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
	defer h.mu.Unlock()
	if h.starting != nil {
		h.starting()
		return nil
	}
	if h.active != nil {
		return h.active.Interrupt(ctx)
	}
	// The worker run slot may be installed before its Run callback is scheduled.
	h.pending = true
	return nil
}

func (h *Handoff) requeueFront(messages []string) {
	if len(messages) == 0 {
		return
	}
	h.queue = append(messages, h.queue...)
	h.queueSize = len(strings.Join(h.queue, "\n"))
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
