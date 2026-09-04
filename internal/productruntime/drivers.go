package productruntime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type RuntimeProduct struct {
	Descriptor productcatalog.Descriptor
	Lane       LaneDriver
	Doctor     DoctorProbe
}

type LaneDriver interface {
	Capabilities() LaneCapabilitySet
	Open(context.Context, LaneOpenRequest) (NativeSessionRef, error)
	StartTurn(context.Context, NativeSessionRef, TurnStartRequest) (NativeTurnRef, error)
	WaitTurn(context.Context, NativeTurnRef) (NativeTerminal, error)
	Steer(context.Context, NativeTurnRef, TurnStartRequest) (NativeAcceptance, error)
	Interrupt(context.Context, NativeTurnRef) error
	Archive(context.Context, NativeSessionRef) error
}

// LaneWorkerBody translates one model-agnostic worker session to a native
// product. The shared session below owns lifecycle state and timing.
type LaneWorkerBody interface {
	Open(context.Context, LaneOpenRequest) (string, error)
	StartTurn(context.Context, LaneTurnStartRequest) (string, error)
	WaitTurn(context.Context) (NativeTerminal, error)
	Interrupt(context.Context) error
	Archive(context.Context) error
	Close(context.Context) error
}

type LaneOpenResult struct {
	NativeID string `json:"native_id"`
}
type LaneTurnStartResult struct {
	NativeMessageID string `json:"native_message_id"`
}
type LaneTurnWaitResult struct {
	Outcome TurnOutcome     `json:"outcome"`
	Result  string          `json:"result"`
	Reason  json.RawMessage `json:"reason"`
}

// LaneWorkerSession is the single common lifecycle used by every worker body.
// It owns turn timing and collection; the body owns only native translation.
type LaneWorkerSession struct {
	ctx          context.Context
	body         LaneWorkerBody
	capabilities LaneCapabilitySet
	update       func(LaneStatusProjection) error
	exit         func()
	mu           sync.Mutex
	name         string
	state        string
	turnID       string
	outcome      string
	opening      bool
	collecting   bool
	settling     chan struct{}
	terminal     *NativeTerminal
	ready        chan struct{}
	auto         time.Duration
	timer        *time.Timer
	timerEpoch   uint64
	closeOnce    sync.Once
	closeErr     error
}

func NewLaneWorkerSession(
	ctx context.Context,
	body LaneWorkerBody,
	capabilities LaneCapabilitySet,
	update func(LaneStatusProjection) error,
	exit func(),
) *LaneWorkerSession {
	return &LaneWorkerSession{ctx: ctx, body: body, capabilities: capabilities, update: update, exit: exit}
}

func (s *LaneWorkerSession) Open(request LaneOpenRequest) (LaneOpenResult, error) {
	auto, err := laneDuration(request.AutoArchiveAfterSeconds)
	if err != nil {
		return LaneOpenResult{}, err
	}
	s.mu.Lock()
	if s.state != "" || s.opening {
		s.mu.Unlock()
		return LaneOpenResult{}, ErrAmbiguousSession
	}
	s.opening = true
	s.mu.Unlock()
	nativeID, err := s.body.Open(s.ctx, request)
	s.mu.Lock()
	s.opening = false
	if err == nil {
		s.name, s.state = request.Name, "idle"
		s.auto = auto
	}
	s.mu.Unlock()
	return LaneOpenResult{NativeID: nativeID}, err
}

func (s *LaneWorkerSession) Start(request LaneTurnStartRequest) (LaneTurnStartResult, error) {
	if request.TimeoutSeconds != nil {
		if _, err := laneDuration(*request.TimeoutSeconds); err != nil {
			return LaneTurnStartResult{}, err
		}
	}
	s.awaitCollection()
	s.mu.Lock()
	steer := request.Mode == "steer"
	if steer && (!s.capabilities.Steer || s.state != "running" && s.state != "interrupting") ||
		!steer && s.state != "idle" {
		s.mu.Unlock()
		return LaneTurnStartResult{}, ErrUnauthorized
	}
	if !steer {
		s.state = "starting"
		s.stopTimer()
	}
	s.mu.Unlock()
	messageID, err := s.body.StartTurn(s.ctx, request)
	if err == nil && messageID == "" {
		err = ErrProtocol
	}
	if err != nil {
		if !steer {
			s.mu.Lock()
			s.state = "idle"
			s.armTimer()
			s.mu.Unlock()
		}
		return LaneTurnStartResult{}, err
	}
	if steer {
		return LaneTurnStartResult{NativeMessageID: messageID}, nil
	}
	s.mu.Lock()
	s.state, s.turnID, s.outcome, s.ready = "running", messageID, "", make(chan struct{})
	s.mu.Unlock()
	go s.observe(request.TimeoutSeconds)
	if err := s.report(); err != nil {
		s.exit()
		return LaneTurnStartResult{}, err
	}
	return LaneTurnStartResult{NativeMessageID: messageID}, nil
}

func (s *LaneWorkerSession) observe(timeout *float64) {
	ctx, cancel := context.WithCancel(s.ctx)
	if timeout != nil {
		duration, err := laneDuration(*timeout)
		if err != nil {
			s.finish(NativeTerminal{Outcome: TurnFailed, NativeStopReason: err.Error()})
			return
		}
		ctx, cancel = context.WithTimeout(s.ctx, duration)
	}
	terminal, err := s.body.WaitTurn(ctx)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	if timedOut {
		interruptCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.body.Interrupt(interruptCtx)
		stop()
		terminal, err = s.body.WaitTurn(s.ctx)
		terminal.Outcome, terminal.ExitLike = TurnTimedOut, 124
	}
	if terminal.Outcome == "" {
		terminal.Outcome = TurnFailed
	}
	if terminal.Outcome != TurnCompleted && terminal.Outcome != TurnInterrupted &&
		terminal.Outcome != TurnFailed && terminal.Outcome != TurnTimedOut {
		terminal.Outcome = TurnFailed
	}
	if err != nil && terminal.NativeStopReason == "" {
		terminal.NativeStopReason = err.Error()
	}
	s.finish(terminal)
}

func (s *LaneWorkerSession) finish(terminal NativeTerminal) {
	s.mu.Lock()
	s.terminal, s.state, s.outcome = &terminal, "terminal", string(terminal.Outcome)
	projection := s.projection()
	s.mu.Unlock()
	if s.update(projection) != nil {
		s.exit()
		return
	}
	s.mu.Lock()
	close(s.ready)
	s.mu.Unlock()
}

func (s *LaneWorkerSession) Wait(ctx context.Context, messageID string) (LaneTurnWaitResult, func() error, error) {
	s.mu.Lock()
	if s.ready == nil || s.collecting || messageID == "" || messageID != s.turnID {
		s.mu.Unlock()
		return LaneTurnWaitResult{}, nil, ErrAmbiguousSession
	}
	s.collecting = true
	ready := s.ready
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.mu.Lock()
		s.collecting = false
		s.mu.Unlock()
		return LaneTurnWaitResult{}, nil, ctx.Err()
	case <-ready:
	}
	s.mu.Lock()
	terminal := *s.terminal
	s.settling = make(chan struct{})
	s.mu.Unlock()
	reason := json.RawMessage(`null`)
	if json.Valid([]byte(terminal.NativeStopReason)) {
		reason = json.RawMessage(terminal.NativeStopReason)
	} else if terminal.NativeStopReason != "" {
		reason, _ = json.Marshal(terminal.NativeStopReason)
	}
	return LaneTurnWaitResult{Outcome: terminal.Outcome, Result: terminal.Result, Reason: reason}, s.collected, nil
}

func (s *LaneWorkerSession) collected() error {
	s.mu.Lock()
	settled := s.settling
	projection := s.projection()
	projection.State = "idle"
	if s.auto > 0 {
		projection.AutoArchiveAt = WireInteger(time.Now().Add(s.auto).UnixMilli())
	}
	s.mu.Unlock()
	if err := s.update(projection); err != nil {
		s.mu.Lock()
		s.collecting, s.settling = false, nil
		s.mu.Unlock()
		close(settled)
		return err
	}
	s.mu.Lock()
	s.state, s.terminal, s.ready, s.collecting, s.settling = "idle", nil, nil, false, nil
	s.armTimer()
	s.mu.Unlock()
	close(settled)
	return nil
}

func (s *LaneWorkerSession) Interrupt() error {
	s.mu.Lock()
	if s.state != "running" {
		s.mu.Unlock()
		return ErrUnauthorized
	}
	s.state = "interrupting"
	s.mu.Unlock()
	if err := s.body.Interrupt(s.ctx); err != nil {
		s.mu.Lock()
		if s.state == "interrupting" {
			s.state = "running"
		}
		s.mu.Unlock()
		return err
	}
	if err := s.report(); err != nil {
		s.exit()
		return err
	}
	return nil
}

func (s *LaneWorkerSession) Archive() (func() error, error) {
	s.awaitCollection()
	s.mu.Lock()
	if s.state != "idle" {
		s.mu.Unlock()
		return nil, ErrUnauthorized
	}
	s.state = "archiving"
	s.stopTimer()
	s.mu.Unlock()
	if err := s.body.Archive(s.ctx); err != nil {
		s.mu.Lock()
		s.state = "idle"
		s.armTimer()
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	s.state = "archived"
	s.mu.Unlock()
	if err := s.report(); err != nil {
		s.exit()
		return nil, err
	}
	return func() error {
		s.exit()
		return nil
	}, nil
}

func (s *LaneWorkerSession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.stopTimer()
		s.mu.Unlock()
		s.closeErr = s.body.Close(ctx)
	})
	return s.closeErr
}

func (s *LaneWorkerSession) report() error {
	s.mu.Lock()
	projection := s.projection()
	s.mu.Unlock()
	return s.update(projection)
}

func (s *LaneWorkerSession) projection() LaneStatusProjection {
	return LaneStatusProjection{Name: s.name, State: s.state, TurnID: s.turnID, Outcome: s.outcome}
}

func (s *LaneWorkerSession) awaitCollection() {
	s.mu.Lock()
	settling := s.settling
	s.mu.Unlock()
	if settling != nil {
		select {
		case <-settling:
		case <-s.ctx.Done():
		}
	}
}

func (s *LaneWorkerSession) stopTimer() {
	s.timerEpoch++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *LaneWorkerSession) armTimer() {
	if s.auto > 0 {
		s.timerEpoch++
		epoch := s.timerEpoch
		s.timer = time.AfterFunc(s.auto, func() {
			s.mu.Lock()
			if s.timerEpoch != epoch || s.state != "idle" {
				s.mu.Unlock()
				return
			}
			s.timer = nil
			s.mu.Unlock()
			after, err := s.Archive()
			if err != nil {
				s.exit()
			} else {
				_ = after()
			}
		})
	}
}

func laneDuration(seconds float64) (time.Duration, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 ||
		seconds > float64(time.Duration(1<<63-1))/float64(time.Second) {
		return 0, ErrProtocol
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// LaneMessageDriver is implemented when a daemon-owned product session has a
// native inbound message path. Other lane drivers receive messages through
// their held presence connection.
type LaneMessageDriver interface {
	SendMessage(context.Context, NativeSessionRef, NativeMessage) error
}

type NativeMessageSource struct {
	UUID    string   `json:"uuid"`
	Name    string   `json:"name"`
	Product string   `json:"product"`
	Groups  []string `json:"groups"`
}

type NativeMessage struct {
	ID   string              `json:"message_id"`
	From NativeMessageSource `json:"from"`
	Body string              `json:"body"`
}

type DoctorProbe interface {
	Probe(context.Context, ProbeRequest) (ProbeReport, error)
}
