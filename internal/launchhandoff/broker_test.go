//go:build linux || darwin

package launchhandoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestBrokerTransfersOnceToExactWrapperAndExecUsesEnvelopeOnly(t *testing.T) {
	broker, cancel, done := startTestBroker(t, Config{})
	defer stopTestBroker(t, broker, cancel, done)
	owner := mustIdentity(t)
	wrapper := WrapperIdentity{UID: os.Geteuid(), Process: owner}
	var rollbacks atomic.Int32
	ticket, err := broker.Stage(StageRequest{
		AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(),
		Finalizers: testFinalizers(func(context.Context) error { rollbacks.Add(1); return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(ticket)
	if err != nil || bytes.Contains(wire, []byte(testSecret)) {
		t.Fatalf("ticket JSON leaked secret: %s, %v", wire, err)
	}
	if _, err := json.Marshal(testCommand()); err == nil {
		t.Fatal("native command unexpectedly serialized to control JSON")
	}
	ambientName := "AGENT_SESSIONS_HANDOFF_AMBIENT_SENTINEL"
	if _, present := os.LookupEnv(ambientName); present {
		t.Skip("test ambient sentinel is already set")
	}
	called := false
	err = consumeAndExec(context.Background(), broker.Endpoint(), ticket, DefaultLimits(), func(path string, args, env []string, cwd string) error {
		called = true
		if path != "native" || cwd != "/work" || strings.Join(args, " ") != "--mode peer" {
			t.Fatalf("exec command = %q %q cwd=%q", path, args, cwd)
		}
		if strings.Join(env, "\x00") != "VISIBLE=ordinary\x00BOOTSTRAP_SECRET="+testSecret {
			t.Fatalf("exec env = %q", env)
		}
		for _, argument := range args {
			if strings.Contains(argument, testSecret) {
				t.Fatal("secret entered argv")
			}
		}
		if _, present := os.LookupEnv("BOOTSTRAP_SECRET"); present {
			t.Fatal("wrapper ambient environment was mutated")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("ConsumeAndExec = %v, called=%t", err, called)
	}
	if rollbacks.Load() != 0 {
		t.Fatalf("successful handoff rolled back %d times", rollbacks.Load())
	}
	if _, err := Consume(context.Background(), broker.Endpoint(), ticket, DefaultLimits()); !errors.Is(err, ErrStale) && !errors.Is(err, ErrClaimed) {
		t.Fatalf("replayed ticket error = %v", err)
	}
	assertNoSecretFiles(t, filepath.Dir(filepath.Dir(broker.Endpoint())))
}

func TestForeignIdentityDoesNotConsumePendingTicket(t *testing.T) {
	owner := mustIdentity(t)
	wrapper := WrapperIdentity{UID: os.Geteuid(), Process: owner}
	var captures atomic.Int32
	broker, cancel, done := startTestBroker(t, Config{CaptureProcess: func(pid int) (procinfo.Identity, error) {
		if captures.Add(1) == 1 {
			wrong := owner
			wrong.StrongStart += "-recycled"
			return wrong, nil
		}
		return procinfo.CaptureIdentity(pid)
	}})
	defer stopTestBroker(t, broker, cancel, done)
	var rollbacks atomic.Int32
	ticket, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error {
		rollbacks.Add(1)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Consume(context.Background(), broker.Endpoint(), ticket, DefaultLimits()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign consume error = %v", err)
	}
	if _, err := Consume(context.Background(), broker.Endpoint(), ticket, DefaultLimits()); err != nil {
		t.Fatalf("exact wrapper could not consume retained ticket: %v", err)
	}
	if rollbacks.Load() != 0 {
		t.Fatalf("foreign attempt rolled back pending entry")
	}
}

func TestForeignUIDDoesNotConsumePendingTicket(t *testing.T) {
	broker, cancel, done := startTestBroker(t, Config{})
	defer stopTestBroker(t, broker, cancel, done)
	wrapper := mustWrapper(t)
	ticket, err := broker.Stage(StageRequest{
		AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(),
		Finalizers: testFinalizers(func(context.Context) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	peer := localtransport.PeerIdentity{PID: wrapper.Process.PID, UID: wrapper.UID + 1}
	if _, err := broker.claim(ticket, peer, wrapper.Process, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign UID claim error = %v", err)
	}
	if _, err := Consume(context.Background(), broker.Endpoint(), ticket, DefaultLimits()); err != nil {
		t.Fatalf("foreign UID consumed the pending ticket: %v", err)
	}
}

func TestClaimDisconnectNeverRependsAndRollsBackOnce(t *testing.T) {
	broker, cancel, done := startTestBroker(t, Config{})
	defer stopTestBroker(t, broker, cancel, done)
	var rollbacks atomic.Int32
	ticket, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: mustWrapper(t), Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error {
		rollbacks.Add(1)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := localtransport.DialBytes(broker.Endpoint(), uint32(DefaultLimits().MaxCommandBytes))
	if err != nil {
		t.Fatal(err)
	}
	claim, _ := encodeClaim(ticket)
	if err := connection.WriteFrame(claim); err != nil {
		t.Fatal(err)
	}
	body, err := connection.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	zero(body)
	_ = connection.Close() // no ACK: the one-shot is burned and rolled back.
	waitFor(t, func() bool { return rollbacks.Load() == 1 })
	if _, err := Consume(context.Background(), broker.Endpoint(), ticket, DefaultLimits()); !errors.Is(err, ErrStale) {
		t.Fatalf("claimed ticket was re-pended: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if rollbacks.Load() != 1 {
		t.Fatalf("rollback count = %d", rollbacks.Load())
	}
}

func TestExpiryShutdownAndCapacityAreBounded(t *testing.T) {
	root := testStateRoot(t)
	now := time.Now()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	limits := DefaultLimits()
	limits.MaxPending = 2
	limits.MaxAggregate = limits.MaxCommandBytes * 2
	broker, err := NewBroker(Config{StateRoot: root, Limits: limits, Now: func() time.Time { return time.Unix(0, clock.Load()) }, Random: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 16))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Broker.Run did not stop")
		}
	}()
	owner := mustIdentity(t)
	wrapper := WrapperIdentity{UID: os.Geteuid(), Process: owner}
	var rollbacks atomic.Int32
	rollback := func(context.Context) error { rollbacks.Add(1); return nil }
	first, err := broker.Stage(StageRequest{AttachmentID: "first", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(rollback)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Stage(StageRequest{AttachmentID: "collision", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(rollback)}); !errors.Is(err, ErrUnavailable) {
		// The deterministic reader intentionally returns the same ticket. A
		// selector collision cannot replace the existing record.
		t.Fatalf("ticket collision error = %v", err)
	}
	now = now.Add(limits.PendingTTL)
	clock.Store(now.UnixNano())
	broker.expire()
	if rollbacks.Load() != 1 {
		t.Fatalf("expiry rollback count = %d", rollbacks.Load())
	}
	if _, err := Consume(context.Background(), broker.Endpoint(), first, limits); !errors.Is(err, ErrStale) {
		t.Fatalf("expired ticket error = %v", err)
	}
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Stage(StageRequest{AttachmentID: "closed", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(rollback)}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stage after close error = %v", err)
	}
}

func TestCapacityAndShutdownDrainDistinctPendingEntriesOnce(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPending = 1
	broker, cancel, done := startTestBroker(t, Config{Limits: limits})
	wrapper := mustWrapper(t)
	var rollbacks atomic.Int32
	rollback := func(context.Context) error { rollbacks.Add(1); return nil }
	if _, err := broker.Stage(StageRequest{AttachmentID: "first", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(rollback)}); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Stage(StageRequest{AttachmentID: "second", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(rollback)}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second stage error = %v", err)
	}
	stopTestBroker(t, broker, cancel, done)
	if rollbacks.Load() != 1 {
		t.Fatalf("shutdown rollback count = %d", rollbacks.Load())
	}
}

func TestStageRefusesSecondTicketForSamePreparedAttachment(t *testing.T) {
	broker, cancel, done := startTestBroker(t, Config{})
	defer stopTestBroker(t, broker, cancel, done)
	request := StageRequest{
		AttachmentID: "attachment", Wrapper: mustWrapper(t), Command: testCommand(),
		Finalizers: testFinalizers(func(context.Context) error { return nil }),
	}
	if _, err := broker.Stage(request); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Stage(request); !errors.Is(err, ErrClaimed) {
		t.Fatalf("second ticket for one attachment error = %v", err)
	}
}

func TestPreRestartTicketIsStaleAtSuccessorBroker(t *testing.T) {
	root := testStateRoot(t)
	first, firstCancel, firstDone := startTestBroker(t, Config{StateRoot: root})
	ticket, err := first.Stage(StageRequest{
		AttachmentID: "attachment", Wrapper: mustWrapper(t), Command: testCommand(),
		Finalizers: testFinalizers(func(context.Context) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopTestBroker(t, first, firstCancel, firstDone)

	second, secondCancel, secondDone := startTestBroker(t, Config{StateRoot: root})
	defer stopTestBroker(t, second, secondCancel, secondDone)
	if _, err := Consume(context.Background(), second.Endpoint(), ticket, DefaultLimits()); !errors.Is(err, ErrStale) {
		t.Fatalf("pre-restart ticket error = %v", err)
	}
}

func TestNewBrokerRejectsPartiallyInvalidLimits(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPending = 0
	if _, err := NewBroker(Config{StateRoot: testStateRoot(t), Limits: limits}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid limits error = %v", err)
	}
}

func TestStageRejectsWrapperFromDifferentUID(t *testing.T) {
	broker, cancel, done := startTestBroker(t, Config{})
	defer stopTestBroker(t, broker, cancel, done)
	wrapper := mustWrapper(t)
	wrapper.UID++
	if _, err := broker.Stage(StageRequest{
		AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(),
		Finalizers: testFinalizers(func(context.Context) error { return nil }),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("different-UID stage error = %v", err)
	}
}

func TestExecFailureIsTypedAndEnvironmentSliceIsCleared(t *testing.T) {
	broker, cancel, done := startTestBroker(t, Config{})
	defer stopTestBroker(t, broker, cancel, done)
	ticket, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: mustWrapper(t), Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	returned := errors.New("exec failed without exposing command")
	var observedEnvironment []string
	if err := consumeAndExec(context.Background(), broker.Endpoint(), ticket, DefaultLimits(), func(_ string, _ []string, environment []string, _ string) error {
		observedEnvironment = environment
		return returned
	}); !errors.Is(err, returned) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("exec failure = %v", err)
	}
	for index, value := range observedEnvironment {
		if value != "" {
			t.Fatalf("exec environment entry %d retained after failed seam", index)
		}
	}
}

func TestClearCommandOverwritesEveryTransientSlice(t *testing.T) {
	command := testCommand()
	arguments := command.Args
	ordinary := command.Env
	sensitive := command.SensitiveEnv
	clearCommand(&command)
	if command.Path != "" || command.Cwd != "" || command.Args != nil || command.Env != nil || command.SensitiveEnv != nil {
		t.Fatalf("command not cleared: %v", command)
	}
	for index, value := range arguments {
		if value != "" {
			t.Fatalf("argument %d retained", index)
		}
	}
	for index, value := range ordinary {
		if value != (productruntime.EnvVar{}) {
			t.Fatalf("ordinary environment %d retained", index)
		}
	}
	for index, value := range sensitive {
		if value != (productruntime.SensitiveEnvVar{}) {
			t.Fatalf("sensitive environment %d retained", index)
		}
	}
}

func TestGOWriteClassificationKeepsReservationAndSeparatesFinalizers(t *testing.T) {
	for _, test := range []struct {
		name          string
		outcome       localtransport.FrameWriteOutcome
		writeErr      error
		wantRollback  int32
		wantAmbiguous int32
	}{
		{name: "zero", outcome: localtransport.FrameWriteOutcome{WrittenBytes: 0, TotalBytes: 11}, writeErr: errors.New("zero-byte GO failure"), wantRollback: 1},
		{name: "partial", outcome: localtransport.FrameWriteOutcome{WrittenBytes: 5, TotalBytes: 11}, writeErr: errors.New("partial GO failure"), wantAmbiguous: 1},
		{name: "full", outcome: localtransport.FrameWriteOutcome{WrittenBytes: 11, TotalBytes: 11}},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxPending = 1
			broker, err := NewBroker(Config{StateRoot: testStateRoot(t), Limits: limits})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			wrapper := mustWrapper(t)
			var rollbacks atomic.Int32
			var ambiguities atomic.Int32
			finalizerStarted := make(chan struct{}, 1)
			finalizerRelease := make(chan struct{})
			plan := mustFinalizers(
				func(context.Context) error {
					rollbacks.Add(1)
					finalizerStarted <- struct{}{}
					<-finalizerRelease
					return nil
				},
				func(_ context.Context, fact AmbiguousHandoff) error {
					if fact.AttachmentID != "attachment" || !validTicketID(fact.Ticket.ID) {
						t.Errorf("ambiguous fact = %+v", fact)
					}
					ambiguities.Add(1)
					finalizerStarted <- struct{}{}
					<-finalizerRelease
					return nil
				},
			)
			ticket, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: plan})
			if err != nil {
				t.Fatal(err)
			}
			connection := newScriptedConnection(ticket, test.outcome, test.writeErr)
			done := serveScripted(t, broker, connection, wrapper)
			<-connection.goStarted

			if _, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error { return nil })}); !errors.Is(err, ErrCapacity) && !errors.Is(err, ErrClaimed) {
				t.Fatalf("same attachment while GO pending = %v", err)
			}
			if _, err := broker.Stage(StageRequest{AttachmentID: "other", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error { return nil })}); !errors.Is(err, ErrCapacity) {
				t.Fatalf("capacity released before GO classification: %v", err)
			}
			close(connection.releaseGO)

			if test.name != "full" {
				<-finalizerStarted
				if _, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error { return nil })}); !errors.Is(err, ErrCapacity) && !errors.Is(err, ErrClaimed) {
					t.Fatalf("attachment reservation released during finalizer: %v", err)
				}
				close(finalizerRelease)
			}
			<-done
			if rollbacks.Load() != test.wantRollback || ambiguities.Load() != test.wantAmbiguous {
				t.Fatalf("callbacks rollback=%d ambiguous=%d", rollbacks.Load(), ambiguities.Load())
			}
			if _, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: testFinalizers(func(context.Context) error { return nil })}); err != nil {
				t.Fatalf("attachment not released after resolution: %v", err)
			}
		})
	}
}

func TestCloseAndExpireDuringGOWaitForSingleAmbiguousSettlement(t *testing.T) {
	for _, test := range []struct {
		name   string
		settle func(*Broker)
	}{
		{name: "close", settle: func(broker *Broker) { _ = broker.Close() }},
		{name: "expire", settle: func(broker *Broker) { broker.expire() }},
		{name: "close-and-expire", settle: func(broker *Broker) {
			var wait sync.WaitGroup
			wait.Add(2)
			go func() { defer wait.Done(); _ = broker.Close() }()
			go func() { defer wait.Done(); broker.expire() }()
			wait.Wait()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			limits := DefaultLimits()
			broker, err := NewBroker(Config{StateRoot: testStateRoot(t), Limits: limits, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			wrapper := mustWrapper(t)
			var rollbacks atomic.Int32
			var ambiguities atomic.Int32
			plan := mustFinalizers(
				func(context.Context) error { rollbacks.Add(1); return nil },
				func(context.Context, AmbiguousHandoff) error { ambiguities.Add(1); return nil },
			)
			ticket, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: plan})
			if err != nil {
				t.Fatal(err)
			}
			connection := newScriptedConnection(ticket, localtransport.FrameWriteOutcome{WrittenBytes: 5, TotalBytes: 11}, errors.New("partial close-race GO"))
			done := serveScripted(t, broker, connection, wrapper)
			<-connection.goStarted
			clock.Store(now.Add(limits.PendingTTL).UnixNano())
			settled := make(chan struct{})
			go func() {
				test.settle(broker)
				close(settled)
			}()
			<-settled
			<-done
			if rollbacks.Load() != 0 || ambiguities.Load() != 1 {
				t.Fatalf("race callbacks rollback=%d ambiguous=%d", rollbacks.Load(), ambiguities.Load())
			}
			broker.mu.Lock()
			remaining, total := len(broker.entries), broker.total
			broker.mu.Unlock()
			if remaining != 0 || total != 0 {
				t.Fatalf("public settlement returned with entries=%d total=%d", remaining, total)
			}
		})
	}
}

func TestCloseDoesNotReturnBeforeActiveAmbiguousFinalizer(t *testing.T) {
	broker, err := NewBroker(Config{StateRoot: testStateRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := mustWrapper(t)
	started := make(chan struct{})
	release := make(chan struct{})
	plan := mustFinalizers(
		func(context.Context) error { t.Error("partial GO reached rollback"); return nil },
		func(context.Context, AmbiguousHandoff) error {
			close(started)
			<-release
			return nil
		},
	)
	ticket, err := broker.Stage(StageRequest{AttachmentID: "attachment", Wrapper: wrapper, Command: testCommand(), Finalizers: plan})
	if err != nil {
		t.Fatal(err)
	}
	connection := newScriptedConnection(ticket, localtransport.FrameWriteOutcome{WrittenBytes: 5, TotalBytes: 11}, errors.New("partial shutdown GO"))
	done := serveScripted(t, broker, connection, wrapper)
	<-connection.goStarted
	closed := make(chan struct{})
	go func() { _ = broker.Close(); close(closed) }()
	<-started
	select {
	case <-closed:
		t.Fatal("Close returned before ambiguity finalizer settled")
	default:
	}
	close(release)
	<-closed
	<-done
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.entries) != 0 || broker.total != 0 {
		t.Fatalf("Close left entries=%d total=%d", len(broker.entries), broker.total)
	}
}

func TestFinalizationVariantsAreStructurallyDisjoint(t *testing.T) {
	var rollbacks atomic.Int32
	var ambiguities atomic.Int32
	plan := mustFinalizers(
		func(context.Context) error { rollbacks.Add(1); return nil },
		func(context.Context, AmbiguousHandoff) error { ambiguities.Add(1); return nil },
	)
	action := plan.take(handoffAmbiguous, AmbiguousHandoff{AttachmentID: "attachment"})
	variant, ok := action.(ambiguousResolution)
	if !ok {
		t.Fatalf("possible write selected %T", action)
	}
	variantType := reflect.TypeOf(variant)
	for index := 0; index < variantType.NumField(); index++ {
		field := variantType.Field(index)
		if field.Type == reflect.TypeOf(rollbackCapability{}) || field.Type == reflect.TypeOf((RollbackFunc)(nil)) {
			t.Fatalf("ambiguous variant carries destructive capability in %s", field.Name)
		}
	}
	action.finish(context.Background())
	if rollbacks.Load() != 0 || ambiguities.Load() != 1 {
		t.Fatalf("ambiguous action rollback=%d ambiguous=%d", rollbacks.Load(), ambiguities.Load())
	}
	if plan.valid() {
		t.Fatal("classification did not consume the pre-GO finalization plan")
	}
}

type scriptedConnection struct {
	mu          sync.Mutex
	ticket      Ticket
	readCount   int
	command     []byte
	outcome     localtransport.FrameWriteOutcome
	writeErr    error
	goStarted   chan struct{}
	releaseGO   chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
	startedOnce sync.Once
}

func newScriptedConnection(ticket Ticket, outcome localtransport.FrameWriteOutcome, writeErr error) *scriptedConnection {
	return &scriptedConnection{
		ticket: ticket, outcome: outcome, writeErr: writeErr,
		goStarted: make(chan struct{}), releaseGO: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (c *scriptedConnection) ReadFrame() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readCount++
	if c.readCount == 1 {
		return encodeClaim(c.ticket)
	}
	if c.readCount == 2 && len(c.command) != 0 {
		return encodeAck(protocolDigest(c.command)), nil
	}
	return nil, errors.New("unexpected scripted read")
}

func (c *scriptedConnection) WriteFrame(body []byte) error {
	c.mu.Lock()
	c.command = append(c.command[:0], body...)
	c.mu.Unlock()
	return nil
}

func (c *scriptedConnection) WriteFrameOutcome([]byte) (localtransport.FrameWriteOutcome, error) {
	c.startedOnce.Do(func() { close(c.goStarted) })
	select {
	case <-c.releaseGO:
	case <-c.closed:
	}
	return c.outcome, c.writeErr
}

func (*scriptedConnection) SetDeadline(time.Time) error { return nil }
func (c *scriptedConnection) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func serveScripted(t *testing.T, broker *Broker, connection *scriptedConnection, wrapper WrapperIdentity) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	broker.wg.Add(1)
	go func() {
		defer broker.wg.Done()
		defer close(done)
		broker.serve(context.Background(), connection, localtransport.PeerIdentity{PID: wrapper.Process.PID, UID: wrapper.UID})
	}()
	return done
}

func startTestBroker(t *testing.T, override Config) (*Broker, context.CancelFunc, <-chan error) {
	t.Helper()
	if override.StateRoot == "" {
		override.StateRoot = testStateRoot(t)
	}
	if !override.Limits.valid() {
		override.Limits = DefaultLimits()
	}
	broker, err := NewBroker(override)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Run(ctx) }()
	return broker, cancel, done
}

func stopTestBroker(t *testing.T, broker *Broker, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	_ = broker.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Broker.Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Broker.Run did not stop")
	}
}

func testStateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "run")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustIdentity(t *testing.T) procinfo.Identity {
	t.Helper()
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil || identity.PID <= 1 || identity.Start == "" || identity.StrongStart == "" {
		t.Fatalf("capture process identity: %+v, %v", identity, err)
	}
	return identity
}

func mustWrapper(t *testing.T) WrapperIdentity {
	t.Helper()
	return WrapperIdentity{UID: os.Geteuid(), Process: mustIdentity(t)}
}

func testFinalizers(rollback RollbackFunc) FinalizationPlan {
	return mustFinalizers(rollback, func(context.Context, AmbiguousHandoff) error { return nil })
}

func mustFinalizers(rollback RollbackFunc, ambiguous AmbiguousFinalizer) FinalizationPlan {
	plan, err := NewFinalizationPlan(rollback, ambiguous)
	if err != nil {
		panic(err)
	}
	return plan
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func assertNoSecretFiles(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Type()&os.ModeSocket != 0 {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(body, []byte(testSecret)) {
			t.Fatalf("secret persisted in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
