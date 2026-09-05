package host

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

type fakeTurn struct {
	done       chan struct{}
	waiting    chan struct{}
	injected   chan string
	interrupt  chan struct{}
	injecting  chan struct{}
	inject     chan struct{}
	active     atomic.Bool
	outcome    string
	interrupts atomic.Int32
}

func (t *fakeTurn) Wait(context.Context) (sessionkit.TurnResult, error) {
	if t.waiting != nil {
		close(t.waiting)
	}
	<-t.done
	return sessionkit.TurnResult{Outcome: value(t.outcome, "completed"), Result: "done"}, nil
}
func (t *fakeTurn) Inject(_ context.Context, message string) (bool, error) {
	if t.injecting != nil {
		close(t.injecting)
		<-t.inject
	}
	if t.active.Load() {
		t.injected <- message
	}
	return t.active.Load(), nil
}
func (t *fakeTurn) Interrupt(context.Context) error {
	t.interrupts.Add(1)
	close(t.interrupt)
	return nil
}

func TestHandoffDeliveryTable(t *testing.T) {
	for _, test := range []struct {
		name, phase, want string
	}{
		{"idle queues", "idle", "queued_for_next_turn"},
		{"creating queues", "creating", "queued_for_next_turn"},
		{"active confirms injection", "active", "injected"},
		{"terminal queues", "terminal", "queued_for_next_turn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &Handoff{}
			turn := newFakeTurn(true)
			started, release := make(chan struct{}), make(chan struct{})
			var run <-chan sessionkit.TurnResult
			if test.phase != "idle" {
				run = asyncRun(h, func(context.Context, string) (Turn, error) {
					close(started)
					<-release
					return turn, nil
				})
				<-started
			}
			if test.phase == "active" || test.phase == "terminal" {
				close(release)
				<-turn.waiting
				if test.phase == "terminal" {
					close(turn.done)
					<-run
				}
			}
			receipt, err := h.Deliver(context.Background(), delivery("body"))
			must(t, err)
			if receipt.Disposition != test.want {
				t.Fatalf("receipt = %#v", receipt)
			}
			if test.phase == "active" {
				if got := <-turn.injected; !strings.Contains(got, "body") {
					t.Fatalf("injected = %q", got)
				}
			}
			if test.phase == "creating" {
				close(release)
			}
			if test.phase != "idle" && test.phase != "terminal" {
				close(turn.done)
				<-run
			}
		})
	}
}

func TestHandoffQueueAndInterrupt(t *testing.T) {
	h := &Handoff{}
	_, _ = h.Deliver(context.Background(), delivery("queued"))
	turn := newFakeTurn(true)
	turn.outcome = "interrupted"
	started := make(chan string, 1)
	release := make(chan struct{})
	run := asyncRun(h, func(context.Context, string) (Turn, error) {
		started <- "started"
		<-release
		return turn, nil
	})
	<-started
	_, _ = h.Deliver(context.Background(), delivery("late"))
	interrupted := asyncInterrupt(h)
	select {
	case <-interrupted:
		t.Fatal("interrupt did not wait for creation")
	default:
	}
	waitStartingInterrupt(h)
	close(release)
	<-turn.interrupt
	must(t, <-interrupted)
	close(turn.done)
	if result := <-run; result.Outcome != "interrupted" || turn.interrupts.Load() != 1 {
		t.Fatalf("result = %#v, interrupts = %d", result, turn.interrupts.Load())
	}
	var nextPrompt string
	nextTurn := newFakeTurn(false)
	next := asyncRun(h, func(_ context.Context, prompt string) (Turn, error) {
		nextPrompt = prompt
		return nextTurn, nil
	})
	close(nextTurn.done)
	<-next
	if strings.Contains(nextPrompt, "queued") || !strings.Contains(nextPrompt, "late") {
		t.Fatalf("commit removed wrong queue entries: %q", nextPrompt)
	}
}

func TestHandoffFailedCreationKeepsFullQueue(t *testing.T) {
	h := &Handoff{queue: make([]string, MaxQueuedDeliveries), queueSize: MaxQueuedDeliveries - 1}
	started := make(chan struct{})
	run := asyncRun(h, func(context.Context, string) (Turn, error) {
		close(started)
		waitStartingInterrupt(h)
		return nil, errors.New("failed")
	})
	<-started
	receipt, err := h.Deliver(context.Background(), delivery("overflow"))
	must(t, err)
	if receipt.Reason != "queue_full" {
		t.Fatalf("receipt = %#v", receipt)
	}
	interrupted := asyncInterrupt(h)
	select {
	case <-interrupted:
		t.Fatal("interrupt did not wait for failed creation")
	default:
	}
	if result := <-run; result.Outcome != "interrupted" {
		t.Fatalf("result = %#v", result)
	}
	must(t, <-interrupted)
	if len(h.queue) != MaxQueuedDeliveries {
		t.Fatalf("queue length = %d", len(h.queue))
	}
}

func TestHandoffTerminalInjectionRace(t *testing.T) {
	h := &Handoff{}
	turn := newFakeTurn(true)
	turn.injecting, turn.inject = make(chan struct{}), make(chan struct{})
	run := asyncRun(h, func(context.Context, string) (Turn, error) { return turn, nil })
	<-turn.waiting
	delivered := asyncDeliver(h, "racing")
	<-turn.injecting
	turn.active.Store(false)
	close(turn.done)
	close(turn.inject)
	if receipt := <-delivered; receipt.Disposition != "queued_for_next_turn" {
		t.Fatalf("receipt = %#v", receipt)
	}
	<-run
	_, _ = h.Deliver(context.Background(), delivery("after"))
	var prompt string
	nextTurn := newFakeTurn(false)
	next := asyncRun(h, func(_ context.Context, input string) (Turn, error) { prompt = input; return nextTurn, nil })
	close(nextTurn.done)
	<-next
	if strings.Count(prompt, "racing") != 1 || strings.Index(prompt, "racing") > strings.Index(prompt, "after") {
		t.Fatalf("queued prompt = %q", prompt)
	}
}

func TestHandoffPendingInterrupt(t *testing.T) {
	h := &Handoff{}
	must(t, h.Interrupt(context.Background()))
	turn := newFakeTurn(true)
	turn.outcome = "interrupted"
	called := false
	run := asyncRun(h, func(ctx context.Context, prompt string) (Turn, error) {
		called = true
		return turn, nil
	})
	<-turn.interrupt
	close(turn.done)
	if result := <-run; !called || result.Outcome != "interrupted" {
		t.Fatalf("start called = %v, result = %#v", called, result)
	}
}

func TestHandoffQueueBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		items int
		bytes int
	}{
		{"items", MaxQueuedDeliveries, 0},
		{"bytes", 1, MaxQueuedBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &Handoff{queue: make([]string, test.items), queueSize: test.bytes}
			receipt, err := h.Deliver(context.Background(), delivery("overflow"))
			must(t, err)
			if receipt.Disposition != "rejected" || receipt.Reason != "queue_full" {
				t.Fatalf("receipt = %#v", receipt)
			}
		})
	}
}

func asyncRun(h *Handoff, start StartTurn) <-chan sessionkit.TurnResult {
	done := make(chan sessionkit.TurnResult, 1)
	go func() {
		result, _ := h.Run(context.Background(), "input", start)
		done <- result
	}()
	return done
}

func asyncInterrupt(h *Handoff) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.Interrupt(context.Background()) }()
	return done
}

func asyncDeliver(h *Handoff, body string) <-chan sessionkit.DeliveryReceipt {
	done := make(chan sessionkit.DeliveryReceipt, 1)
	go func() { receipt, _ := h.Deliver(context.Background(), delivery(body)); done <- receipt }()
	return done
}

func newFakeTurn(active bool) *fakeTurn {
	turn := &fakeTurn{done: make(chan struct{}), waiting: make(chan struct{}), injected: make(chan string, 1), interrupt: make(chan struct{})}
	turn.active.Store(active)
	return turn
}

func value(got, fallback string) string {
	if got != "" {
		return got
	}
	return fallback
}

func waitStartingInterrupt(h *Handoff) {
	for {
		h.mu.Lock()
		interrupted := h.starting != nil && h.starting.interrupted
		h.mu.Unlock()
		if interrupted {
			return
		}
		runtime.Gosched()
	}
}

func delivery(body string) sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{
		MessageID: "message", Body: body,
		From: sessionkit.DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "example", Groups: []string{"project"}},
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
