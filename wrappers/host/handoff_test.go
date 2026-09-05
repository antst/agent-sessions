package host

import (
	"context"
	"strings"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

type fakeTurn struct {
	done      chan struct{}
	waiting   chan struct{}
	injected  chan string
	interrupt chan struct{}
	active    bool
}

func (t *fakeTurn) Wait(context.Context) (sessionkit.TurnResult, error) {
	if t.waiting != nil {
		close(t.waiting)
	}
	<-t.done
	return sessionkit.TurnResult{Outcome: "completed", Result: "done"}, nil
}
func (t *fakeTurn) Inject(_ context.Context, message string) (bool, error) {
	if t.active {
		t.injected <- message
	}
	return t.active, nil
}
func (t *fakeTurn) Interrupt(context.Context) error { close(t.interrupt); return nil }

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
			turn := &fakeTurn{done: make(chan struct{}), waiting: make(chan struct{}), injected: make(chan string, 1), interrupt: make(chan struct{}), active: true}
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
	receipt, err := h.Deliver(context.Background(), delivery("queued"))
	must(t, err)
	if receipt.Disposition != "queued_for_next_turn" {
		t.Fatalf("receipt = %#v", receipt)
	}
	started := make(chan string, 1)
	release := make(chan struct{})
	run := asyncRun(h, func(ctx context.Context, prompt string) (Turn, error) {
		started <- prompt
		<-ctx.Done()
		close(release)
		return nil, ctx.Err()
	})
	prompt := <-started
	if !strings.Contains(prompt, "queued") || !strings.HasSuffix(prompt, "\ninput") {
		t.Fatalf("prompt = %q", prompt)
	}
	must(t, h.Interrupt(context.Background()))
	<-release
	if result := <-run; result.Outcome != "interrupted" {
		t.Fatalf("result = %#v", result)
	}
	var nextPrompt string
	turn := &fakeTurn{done: make(chan struct{}), injected: make(chan string, 1), interrupt: make(chan struct{})}
	next := asyncRun(h, func(_ context.Context, prompt string) (Turn, error) { nextPrompt = prompt; return turn, nil })
	close(turn.done)
	<-next
	if !strings.Contains(nextPrompt, "queued") {
		t.Fatalf("aborted creation lost queue: %q", nextPrompt)
	}
}

func TestHandoffPendingInterrupt(t *testing.T) {
	h := &Handoff{}
	must(t, h.Interrupt(context.Background()))
	called := false
	result, err := h.Run(context.Background(), "input", func(context.Context, string) (Turn, error) {
		called = true
		return nil, nil
	})
	must(t, err)
	if called || result.Outcome != "interrupted" {
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

func delivery(body string) sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{
		MessageID: "message", Body: body,
		From: sessionkit.DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "codex", Groups: []string{"project"}},
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
