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
	done        chan struct{}
	interrupted bool
	turn        Turn
}

// Handoff owns the only boundary between queued delivery and a native turn.
type Handoff struct {
	mu                 sync.Mutex
	queue              []string
	queueSize          int
	starting           *creation
	active             Turn
	interruptRequested bool
}

func (h *Handoff) Run(ctx context.Context, input string, start StartTurn) (sessionkit.TurnResult, error) {
	h.mu.Lock()
	if h.starting != nil || h.active != nil {
		h.mu.Unlock()
		return sessionkit.TurnResult{}, errors.New("turn already active")
	}
	if h.interruptRequested {
		h.interruptRequested = false
		h.mu.Unlock()
		return sessionkit.TurnResult{Outcome: "interrupted"}, nil
	}
	queued := append([]string(nil), h.queue...)
	starting := &creation{done: make(chan struct{})}
	h.starting = starting
	prompt := strings.Join(queued, "\n")
	if len(queued) > 0 && input != "" {
		prompt += "\n"
	}
	prompt += input
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
		starting.turn, h.active = turn, turn
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
	added := len(rendered)
	if len(h.queue) > 0 {
		added++
	}
	if len(h.queue) == MaxQueuedDeliveries || h.queueSize+added > MaxQueuedBytes {
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: ErrQueueFull.Error()}, nil
	}
	h.queueSize += added
	h.queue = append(h.queue, rendered)
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func (h *Handoff) Interrupt(ctx context.Context) error {
	h.mu.Lock()
	starting, turn := h.starting, h.active
	if starting != nil {
		starting.interrupted = true
	} else if turn == nil {
		h.interruptRequested = true
	}
	h.mu.Unlock()
	if starting != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-starting.done:
			turn = starting.turn
		}
	}
	if turn != nil {
		return turn.Interrupt(ctx)
	}
	return nil
}
