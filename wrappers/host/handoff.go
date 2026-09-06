package host

import (
	"context"
	"errors"
	"slices"
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
type Rendered string
type deliverySlot struct {
	body     Rendered
	accepted bool
}

// Handoff owns the FIFO and its boundary with native-turn creation.
type Handoff struct {
	mu        sync.Mutex
	queue     []*deliverySlot
	active    []*deliverySlot
	queueSize int
}

func (h *Handoff) Run(ctx context.Context, run *sessionkit.Run, input string, start StartTurn) (sessionkit.TurnResult, error) {
	h.mu.Lock()
	if run.Interrupted() {
		h.mu.Unlock()
		return sessionkit.TurnResult{Outcome: "interrupted"}, nil
	}
	queued := append([]*deliverySlot(nil), h.queue...)
	parts := make([]string, len(queued))
	for index := range queued {
		parts[index] = string(queued[index].body)
	}
	prompt := strings.Join(parts, "\n")
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
	h.queue = append([]*deliverySlot(nil), h.queue[len(queued):]...)
	h.recount()
	slot := &nativeTurn{Turn: turn}
	run.Native = slot
	if run.Interrupted() {
		run.Native = nil
		go turn.Interrupt(ctx)
	}
	h.mu.Unlock()

	result, err := turn.Wait(ctx)
	h.Finish()
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
	slot := &deliverySlot{body: Rendered(rendered)}
	h.mu.Lock()
	added := len(rendered)
	if len(h.queue)+len(h.active) > 0 {
		added++
	}
	if len(h.queue)+len(h.active) == MaxQueuedDeliveries || h.queueSize+added > MaxQueuedBytes {
		h.mu.Unlock()
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: ErrQueueFull.Error()}, nil
	}
	h.active = append(h.active, slot)
	h.queueSize += added
	h.mu.Unlock()

	injected := false
	if inject != nil {
		injected, err = inject(ctx, rendered)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	index := slices.Index(h.active, slot)
	if err != nil {
		if index < 0 {
			return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
		}
		h.active = slices.Delete(h.active, index, index+1)
		h.recount()
		return sessionkit.DeliveryReceipt{}, err
	}
	if injected {
		if index >= 0 {
			slot.accepted = true
		}
		return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
	}
	if index >= 0 {
		h.active = slices.Delete(h.active, index, index+1)
		h.queue = append(h.queue, slot)
	}
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func (h *Handoff) Claim() []Rendered {
	h.mu.Lock()
	defer h.mu.Unlock()
	claimed := []Rendered{}
	kept := h.active[:0]
	for _, slot := range h.active {
		if slot.accepted {
			claimed = append(claimed, slot.body)
		} else {
			kept = append(kept, slot)
		}
	}
	h.active = kept
	h.recount()
	return claimed
}

func (h *Handoff) Finish() {
	h.mu.Lock()
	h.queue = append(append([]*deliverySlot(nil), h.active...), h.queue...)
	h.active = nil
	h.recount()
	h.mu.Unlock()
}

func (h *Handoff) recount() {
	size, count := 0, 0
	for _, slots := range [][]*deliverySlot{h.active, h.queue} {
		for _, slot := range slots {
			size += len(slot.body)
			count++
		}
	}
	if count > 0 {
		size += count - 1
	}
	h.queueSize = size
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
