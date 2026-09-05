package sessionkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

const unavailableReason = "result unavailable, lane resumable"

var ErrUnknownTurn = errors.New("unknown_turn")

type callFunc func(context.Context, string, any, any) error

type Caller struct {
	call          callFunc
	mu            sync.Mutex
	next          uint64
	runs, targets map[string]*localRun
}

type localRun struct {
	TurnStatus
	done chan struct{}
}

type StartResult struct {
	TurnID string `json:"turn_id"`
}

type StatusRequest = StartResult

type TurnStatus struct {
	TurnID    string      `json:"turn_id"`
	SessionID string      `json:"session_id"`
	State     string      `json:"state"`
	Result    *TurnResult `json:"result,omitempty"`
	Reason    string      `json:"reason,omitempty"`
}

type WaitRequest struct {
	TurnID    string `json:"turn_id"`
	TimeoutMS *int64 `json:"timeout_ms,omitempty"`
}

func newCaller(call callFunc) *Caller {
	return &Caller{call: call, runs: map[string]*localRun{}, targets: map[string]*localRun{}}
}

func callAs[T any](ctx context.Context, call callFunc, method string, params any) (T, error) {
	var result T
	err := call(ctx, method, params, &result)
	return result, err
}

func (c *Caller) List(ctx context.Context, request SessionListRequest) (SessionListResult, error) {
	return callAs[SessionListResult](ctx, c.call, "session.list", request)
}
func (c *Caller) Send(ctx context.Context, request MessageSendRequest) (MessageSendResult, error) {
	return callAs[MessageSendResult](ctx, c.call, "message.send", request)
}
func (c *Caller) Describe(ctx context.Context, request LaneDescribeRequest) (LaneDescribeResult, error) {
	return callAs[LaneDescribeResult](ctx, c.call, "lane.describe", request)
}
func (c *Caller) Spawn(ctx context.Context, request LaneSpawnRequest) (LaneSpawnResult, error) {
	return callAs[LaneSpawnResult](ctx, c.call, "lane.spawn", request)
}
func (c *Caller) Resume(ctx context.Context, sessionID string) (LaneSpawnResult, error) {
	return c.Spawn(ctx, LaneSpawnRequest{ResumeSessionID: sessionID})
}
func (c *Caller) Run(ctx context.Context, request TurnRunRequest) (TurnResult, error) {
	return callAs[TurnResult](ctx, c.call, "turn.run", request)
}
func (c *Caller) Interrupt(ctx context.Context, request SessionTarget) error {
	_, err := callAs[struct{}](ctx, c.call, "turn.interrupt", request)
	return err
}
func (c *Caller) Close(ctx context.Context, request SessionCloseRequest) error {
	_, err := callAs[struct{}](ctx, c.call, "session.close", request)
	return err
}

func (c *Caller) Start(request TurnRunRequest) (StartResult, error) {
	if _, err := protocol.EncodeParams("turn.run", request); err != nil {
		return StartResult{}, err
	}
	c.mu.Lock()
	if c.targets[request.SessionID] != nil {
		c.mu.Unlock()
		return StartResult{}, &ProtocolError{Code: protocol.Busy, Message: "busy"}
	}
	c.next++
	run := &localRun{TurnStatus: TurnStatus{TurnID: fmt.Sprintf("t-%d", c.next), SessionID: request.SessionID, State: "running"}, done: make(chan struct{})}
	c.runs[run.TurnID], c.targets[run.SessionID] = run, run
	c.mu.Unlock()
	go func() {
		result, err := c.Run(context.Background(), request)
		c.settle(run, result, err)
	}()
	return StartResult{TurnID: run.TurnID}, nil
}

func (c *Caller) Status(request StatusRequest) (TurnStatus, error) {
	if request.TurnID == "" {
		return TurnStatus{}, errors.New("invalid status request")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	run := c.runs[request.TurnID]
	if run == nil {
		return TurnStatus{}, ErrUnknownTurn
	}
	result := run.TurnStatus
	if run.State != "running" {
		delete(c.runs, request.TurnID)
	}
	return result, nil
}

func (c *Caller) Wait(request WaitRequest) (TurnStatus, error) {
	if request.TurnID == "" || request.TimeoutMS != nil && *request.TimeoutMS < 0 {
		return TurnStatus{}, errors.New("invalid wait request")
	}
	c.mu.Lock()
	run := c.runs[request.TurnID]
	c.mu.Unlock()
	if run == nil {
		return TurnStatus{}, ErrUnknownTurn
	}
	if request.TimeoutMS == nil {
		<-run.done
	} else {
		timer := time.NewTimer(time.Duration(*request.TimeoutMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-run.done:
		case <-timer.C:
		}
	}
	return c.Status(StatusRequest{TurnID: request.TurnID})
}

func (c *Caller) settle(run *localRun, result TurnResult, err error) {
	c.mu.Lock()
	if err == nil {
		run.State, run.Result = "done", &result
	} else {
		run.State, run.Reason = "unavailable", unavailableReason
		var value *ProtocolError
		if errors.As(err, &value) {
			run.Reason = fmt.Sprintf("%d %s", value.Code, value.Message)
		}
	}
	delete(c.targets, run.SessionID)
	close(run.done)
	c.mu.Unlock()
}
