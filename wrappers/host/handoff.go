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
	Interrupt(context.Context) error
}

type StartTurn func(context.Context, string) (Turn, error)
type Inject func(context.Context, string) (bool, error)
type nativeTurn struct{ Turn }

// Handoff owns the FIFO and its boundary with native-turn creation.
type Handoff struct {
	mu        sync.Mutex
	queue     []string
	queueSize int
}

func (h *Handoff) Run(ctx context.Context, run *sessionkit.Run, input string, start StartTurn) (sessionkit.TurnResult, error) {
	h.mu.Lock()
	if run.Interrupted() {
		h.mu.Unlock()
		return sessionkit.TurnResult{Outcome: "interrupted"}, nil
	}
	queued := append([]string(nil), h.queue...)
	prompt := strings.Join(queued, "\n")
	if prompt != "" && input != "" {
		prompt += "\n"
	}
	h.mu.Unlock()

	turn, err := start(ctx, prompt+input)
	h.mu.Lock()
	if err == nil && turn == nil {
		err = errors.New("native turn was not returned")
	}
	if err != nil {
		interrupted := run.Interrupted()
		h.mu.Unlock()
		if interrupted {
			return sessionkit.TurnResult{Outcome: "interrupted"}, nil
		}
		return sessionkit.TurnResult{}, err
	}
	h.queue = append([]string(nil), h.queue[len(queued):]...)
	h.queueSize = len(strings.Join(h.queue, "\n"))
	slot := &nativeTurn{Turn: turn}
	run.Native = slot
	if run.Interrupted() {
		run.Native = nil
		go turn.Interrupt(ctx)
	}
	h.mu.Unlock()

	result, err := turn.Wait(ctx)
	h.mu.Lock()
	if run.Native == slot {
		run.Native = nil
	}
	h.mu.Unlock()
	return result, err
}

func (h *Handoff) Deliver(ctx context.Context, request sessionkit.DeliveryRequest, inject Inject) (sessionkit.DeliveryReceipt, error) {
	rendered, err := RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	if inject != nil {
		injected, injectErr := inject(ctx, rendered)
		if injectErr != nil {
			return sessionkit.DeliveryReceipt{}, injectErr
		}
		if injected {
			return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
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

func (h *Handoff) Interrupt(ctx context.Context, run *sessionkit.Run) error {
	h.mu.Lock()
	slot, _ := run.Native.(*nativeTurn)
	if slot != nil {
		run.Native = nil
	}
	h.mu.Unlock()
	if slot != nil {
		return slot.Interrupt(ctx)
	}
	return nil
}
