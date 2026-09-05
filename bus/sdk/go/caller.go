package sessionkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

const unavailableReason = "result unavailable, lane resumable"

var ErrUnknownTurn = errors.New("unknown_turn")

type callFunc func(context.Context, string, any) (json.RawMessage, error)

type Caller struct {
	call          callFunc
	mu            sync.Mutex
	next          uint64
	runs, targets map[string]*localRun
}

type localRun struct {
	id, sessionID, state, reason string
	result                       *TurnResult
	done                         chan struct{}
}

type StartResult struct {
	TurnID string `json:"turn_id"`
}

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

type StatusRequest struct {
	TurnID string `json:"turn_id"`
}

func newCaller(call callFunc) *Caller {
	return &Caller{call: call, runs: map[string]*localRun{}, targets: map[string]*localRun{}}
}

func callAs[T any](ctx context.Context, call callFunc, method string, params any) (T, error) {
	var result T
	raw, err := call(ctx, method, params)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
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
	run := &localRun{id: fmt.Sprintf("t-%d", c.next), sessionID: request.SessionID, state: "running", done: make(chan struct{})}
	c.runs[run.id], c.targets[run.sessionID] = run, run
	c.mu.Unlock()
	go func() {
		result, err := c.Run(context.Background(), request)
		c.settle(run, result, err)
	}()
	return StartResult{TurnID: run.id}, nil
}

func (c *Caller) Status(request StatusRequest) (TurnStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run := c.runs[request.TurnID]
	if run == nil {
		return TurnStatus{}, ErrUnknownTurn
	}
	result := run.view()
	if run.state != "running" {
		delete(c.runs, request.TurnID)
	}
	return result, nil
}

func (c *Caller) Wait(request WaitRequest) (TurnStatus, error) {
	c.mu.Lock()
	run := c.runs[request.TurnID]
	c.mu.Unlock()
	if run == nil {
		return TurnStatus{}, ErrUnknownTurn
	}
	if request.TimeoutMS == nil {
		<-run.done
	} else if *request.TimeoutMS < 0 {
		return TurnStatus{}, errors.New("invalid wait request")
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
		run.state, run.result = "done", &result
	} else {
		run.state, run.reason = "unavailable", unavailableReason
		var value *ProtocolError
		if errors.As(err, &value) {
			run.reason = fmt.Sprintf("%d %s", value.Code, value.Message)
		}
	}
	delete(c.targets, run.sessionID)
	close(run.done)
	c.mu.Unlock()
}

func (r *localRun) view() TurnStatus {
	return TurnStatus{TurnID: r.id, SessionID: r.sessionID, State: r.state, Result: r.result, Reason: r.reason}
}
