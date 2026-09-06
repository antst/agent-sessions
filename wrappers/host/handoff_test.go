package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

type fakeTurn struct {
	done, waiting, interrupted, interruptBlock chan struct{}
	interrupts                                 atomic.Int32
}

func (t *fakeTurn) Wait(context.Context) (sessionkit.TurnResult, error) {
	if t.waiting != nil {
		close(t.waiting)
	}
	<-t.done
	return sessionkit.TurnResult{Outcome: "completed"}, nil
}
func (t *fakeTurn) Interrupt(context.Context) error {
	t.interrupts.Add(1)
	close(t.interrupted)
	if t.interruptBlock != nil {
		<-t.interruptBlock
	}
	return nil
}

func TestHandoffDeliveryAndQueueCommit(t *testing.T) {
	h := &Handoff{}
	var injected string
	receipt, err := h.Deliver(context.Background(), delivery("active"), func(_ context.Context, message string) (Injection, error) { injected = message; return Injected, nil })
	must(t, err)
	check(t, receipt.Disposition == "injected" && strings.Contains(injected, "active"), "active receipt = %#v", receipt)
	check(t, len(h.Claim()) == 0 && h.queueSize == 0, "claimed injection stayed pending: size %d", h.queueSize)
	started, release := make(chan struct{}), make(chan struct{})
	delivered := make(chan sessionkit.DeliveryReceipt, 1)
	go func() {
		receipt, _ := h.Deliver(context.Background(), delivery("racing"), func(context.Context, string) (Injection, error) { close(started); <-release; return NotInjected, nil })
		delivered <- receipt
	}()
	<-started
	close(release)
	receipt = <-delivered
	check(t, receipt.Disposition == "queued_for_next_turn", "terminal receipt = %#v", receipt)
	_, _ = h.Deliver(context.Background(), delivery("after"), nil)
	next, prompt := newFakeTurn(), ""
	close(next.done)
	_, err = h.Run(context.Background(), &sessionkit.Run{}, "", func(_ context.Context, got string) (Turn, error) { prompt = got; return next, nil })
	must(t, err)
	check(t, !strings.Contains(prompt, "active") && strings.Count(prompt, "racing") == 1 && strings.Index(prompt, "racing") < strings.Index(prompt, "after") && h.queueSize == 0, "next prompt/size = %q/%d", prompt, h.queueSize)
}

func TestHandoffFailedCreationKeepsFullQueue(t *testing.T) {
	full := make([]*deliverySlot, MaxQueuedDeliveries)
	for index := range full {
		full[index] = &deliverySlot{}
	}
	h := &Handoff{queue: full, queueSize: MaxQueuedDeliveries - 1}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := h.Run(context.Background(), &sessionkit.Run{}, "input", func(context.Context, string) (Turn, error) {
			close(started)
			<-release
			return nil, errors.New("failed")
		})
		done <- err
	}()
	<-started
	receipt, err := h.Deliver(context.Background(), delivery("overflow"), nil)
	must(t, err)
	close(release)
	check(t, receipt.Reason == "queue_full" && (<-done).Error() == "failed" && len(h.queue) == MaxQueuedDeliveries, "receipt = %#v, queue = %d", receipt, len(h.queue))
	h = &Handoff{queue: []*deliverySlot{{body: "full"}}, queueSize: MaxQueuedBytes}
	receipt, err = h.Deliver(context.Background(), delivery("overflow"), nil)
	must(t, err)
	check(t, receipt.Reason == "queue_full", "byte-bound receipt = %#v", receipt)
}

func TestHandoffClaimAndFinish(t *testing.T) {
	h := &Handoff{}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan sessionkit.DeliveryReceipt, 1)
	go func() {
		receipt, _ := h.Deliver(context.Background(), delivery("active"), func(context.Context, string) (Injection, error) {
			close(started)
			<-release
			return Pending, nil
		})
		done <- receipt
	}()
	<-started
	check(t, len(h.Claim()) == 0, "pending delivery was claimed")
	close(release)
	check(t, (<-done).Disposition == "injected", "delivery was not injected")
	check(t, h.queueSize > 0, "pending delivery released its charge")
	claimed := h.Claim()
	check(t, len(claimed) == 1 && strings.Contains(string(claimed[0]), "active") && len(h.Claim()) == 0 && h.queueSize == 0, "claim/size = %#v/%d", claimed, h.queueSize)

	_, _ = h.Deliver(context.Background(), delivery("unclaimed"), func(context.Context, string) (Injection, error) { return Pending, nil })
	_, _ = h.Deliver(context.Background(), delivery("idle"), nil)
	h.Finish()
	crossed, finish := make(chan struct{}), make(chan struct{})
	go func() {
		receipt, _ := h.Deliver(context.Background(), delivery("crossed"), func(context.Context, string) (Injection, error) {
			close(crossed)
			<-finish
			return Injected, nil
		})
		done <- receipt
	}()
	<-crossed
	h.Finish()
	close(finish)
	check(t, (<-done).Disposition == "queued_for_next_turn", "crossed injection was acknowledged")
	next, prompt := newFakeTurn(), ""
	close(next.done)
	_, err := h.Run(context.Background(), &sessionkit.Run{}, "", func(_ context.Context, got string) (Turn, error) { prompt = got; return next, nil })
	must(t, err)
	check(t, strings.Count(prompt, "unclaimed") == 1 && strings.Count(prompt, "idle") == 1 && strings.Count(prompt, "crossed") == 1 && strings.Index(prompt, "unclaimed") < strings.Index(prompt, "idle") && h.queueSize == 0, "prompt/size = %q/%d", prompt, h.queueSize)
}

type workerProduct struct {
	h                          Handoff
	before, release, interrupt chan struct{}
	start                      StartTurn
}

func (*workerProduct) Hello(context.Context) (sessionkit.HelloDescription, error) {
	return sessionkit.HelloDescription{Product: "example-peer", SupportedOpenFields: []string{}, ExtraArguments: []sessionkit.ExtraArgument{}}, nil
}
func (*workerProduct) Open(context.Context, sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	return sessionkit.OpenResult{SessionID: "session"}, nil
}
func (p *workerProduct) Run(ctx context.Context, run *sessionkit.Run, input string) (sessionkit.TurnResult, error) {
	close(p.before)
	<-p.release
	return p.h.Run(ctx, run, input, p.start)
}
func (p *workerProduct) Interrupt(ctx context.Context, run *sessionkit.Run) error {
	close(p.interrupt)
	return p.h.Interrupt(ctx, run)
}
func (p *workerProduct) Deliver(context.Context, sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	return sessionkit.DeliveryReceipt{}, nil
}
func (*workerProduct) Close(context.Context) error { return nil }

func TestHandoffSDKInterruptCrossings(t *testing.T) {
	for _, phase := range []string{"before callback", "during creation", "active turn"} {
		t.Run(phase, func(t *testing.T) {
			p := &workerProduct{before: make(chan struct{}), release: make(chan struct{}), interrupt: make(chan struct{})}
			turn, creating, created := newFakeTurn(), make(chan struct{}), make(chan struct{})
			called := false
			switch phase {
			case "before callback":
				p.start = func(context.Context, string) (Turn, error) { called = true; return turn, nil }
			case "during creation":
				turn.interruptBlock = make(chan struct{})
				p.start = func(context.Context, string) (Turn, error) { close(creating); <-created; return turn, nil }
				close(p.release)
			case "active turn":
				turn.waiting, turn.interruptBlock = creating, make(chan struct{})
				p.start = func(context.Context, string) (Turn, error) { return turn, nil }
				close(p.release)
			}
			connection, reader := startWorker(t, p)
			writeRequest(t, connection, 2, "turn.run", map[string]any{"session_id": "session@local", "input": "input"})
			if phase == "before callback" {
				<-p.before
			} else {
				<-creating
			}
			writeRequest(t, connection, 3, "turn.interrupt", map[string]string{"session_id": "session@local"})
			<-p.interrupt
			if phase == "before callback" {
				close(p.release)
				frames := readFrames(t, reader, 2)
				check(t, !called && strings.Contains(frames, `"outcome":"interrupted"`), "created = %v, frames = %s", called, frames)
				return
			}
			if phase == "during creation" {
				close(created)
			}
			<-turn.interrupted
			close(turn.done)
			frames := readFrames(t, reader, map[bool]int{true: 1, false: 2}[phase == "active turn"])
			close(turn.interruptBlock)
			if phase == "active turn" {
				frames += readFrames(t, reader, 1)
			}
			check(t, turn.interrupts.Load() == 1 && strings.Contains(frames, `"outcome":"completed"`), "interrupts/frames = %d/%s", turn.interrupts.Load(), frames)
		})
	}
}

func startWorker(t *testing.T, product *workerProduct) (net.Conn, *bufio.Reader) {
	path := filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", path)
	must(t, err)
	t.Setenv(TokenEnv, "token")
	t.Setenv(SocketEnv, path)
	worker := sessionkit.NewWorker(product)
	go worker.Serve(context.Background())
	connection, err := listener.Accept()
	must(t, err)
	reader := bufio.NewReader(connection)
	readLine(t, reader)
	_, err = connection.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	must(t, err)
	writeRequest(t, connection, 1, "session.open", map[string]any{"name": "name@local", "groups": []string{}, "open": map[string]any{}})
	readFrames(t, reader, 1)
	t.Cleanup(func() { _ = connection.Close(); <-worker.Closed(); _ = listener.Close() })
	return connection, reader
}

func writeRequest(t *testing.T, connection net.Conn, id int, method string, params any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	must(t, err)
	_, err = connection.Write(append(body, '\n'))
	must(t, err)
}
func readFrames(t *testing.T, reader *bufio.Reader, count int) string {
	var frames strings.Builder
	for range count {
		frames.Write(readLine(t, reader))
	}
	return frames.String()
}
func readLine(t *testing.T, reader *bufio.Reader) []byte {
	body, err := reader.ReadBytes('\n')
	must(t, err)
	return body
}
func newFakeTurn() *fakeTurn {
	return &fakeTurn{done: make(chan struct{}), interrupted: make(chan struct{})}
}
func delivery(body string) sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{MessageID: "message", Body: body, From: sessionkit.DeliverySource{SessionID: "peer@local", Name: "peer@local", Product: "example", Groups: []string{"project"}}}
}
func must(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}
func check(t *testing.T, condition bool, format string, args ...any) {
	if !condition {
		t.Fatalf(format, args...)
	}
}
