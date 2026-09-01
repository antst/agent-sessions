package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// QwenACPLaneRequest identifies one daemon-owned Qwen ACP lane turn.
type QwenACPLaneRequest struct {
	LaneID, NativeSession, PermissionMode, Prompt string
}

// QwenLaneAdapter is the sole ACP client for each daemon-owned Qwen lane.
type QwenLaneAdapter struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// NewQwenLaneAdapter returns an empty Qwen ACP lane registry.
func NewQwenLaneAdapter() *QwenLaneAdapter {
	return &QwenLaneAdapter{active: map[string]context.CancelFunc{}}
}

// Run validates and owns one Qwen ACP turn until the native callback returns.
func (a *QwenLaneAdapter) Run(
	ctx context.Context,
	request QwenACPLaneRequest,
	run func(context.Context, QwenACPLaneRequest) (NativeACPLaneResult, error),
) (NativeACPLaneResult, error) {
	if request.LaneID == "" || strings.TrimSpace(request.Prompt) == "" || run == nil {
		return NativeACPLaneResult{}, errors.New("qwen ACP lane request is incomplete")
	}
	if request.PermissionMode == "yolo" {
		request.PermissionMode = "bypassPermissions"
	}
	turnCtx, cancel := context.WithCancel(ctx)
	if err := a.register(request.LaneID, cancel); err != nil {
		cancel()
		return NativeACPLaneResult{}, err
	}
	defer func() { cancel(); a.complete(request.LaneID) }()
	result, err := run(turnCtx, request)
	if err == nil && request.NativeSession != "" && result.NativeSessionID != request.NativeSession {
		return NativeACPLaneResult{}, errors.New("qwen ACP resumed a different native session")
	}
	return result, err
}

// Interrupt cancels the active Qwen ACP client for laneID.
func (a *QwenLaneAdapter) Interrupt(laneID string) error {
	a.mu.Lock()
	cancel, ok := a.active[laneID]
	a.mu.Unlock()
	if !ok {
		return errors.New("qwen lane has no active ACP client")
	}
	cancel()
	return nil
}

// Archive verifies that no active Qwen ACP client remains for laneID.
func (a *QwenLaneAdapter) Archive(laneID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, active := a.active[laneID]; active {
		return errors.New("refuse to archive an active qwen ACP client")
	}
	return nil
}

func (a *QwenLaneAdapter) register(laneID string, cancel context.CancelFunc) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.active[laneID]; exists {
		return errors.New("qwen lane already has an active ACP client")
	}
	a.active[laneID] = cancel
	return nil
}

func (a *QwenLaneAdapter) complete(laneID string) {
	a.mu.Lock()
	delete(a.active, laneID)
	a.mu.Unlock()
}
