package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrGrokUnsupportedPermissionMode marks a policy the Grok ACP lane cannot
// represent without widening authority. Runtime adapters map this category to
// productruntime.ErrUnsupportedPolicy when the registry boundary is composed.
var ErrGrokUnsupportedPermissionMode = errors.New("grok ACP lane permission mode is unsupported")

// GrokACPLaneRequest identifies one daemon-owned Grok ACP lane turn.
type GrokACPLaneRequest struct {
	LaneID, NativeSession, PermissionMode, Prompt string
}

// NativeACPLaneResult is the product-neutral result returned by an ACP driver.
type NativeACPLaneResult struct {
	NativeSessionID string
	Output          string
	Mode            string
}

// GrokLaneAdapter is the sole ACP driver for each daemon-owned Grok lane.
type GrokLaneAdapter struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// NewGrokLaneAdapter returns an empty Grok ACP lane registry.
func NewGrokLaneAdapter() *GrokLaneAdapter {
	return &GrokLaneAdapter{active: map[string]context.CancelFunc{}}
}

// Run validates and owns one Grok ACP turn until the native callback returns.
func (a *GrokLaneAdapter) Run(
	ctx context.Context,
	request GrokACPLaneRequest,
	run func(context.Context, GrokACPLaneRequest) (NativeACPLaneResult, error),
) (NativeACPLaneResult, error) {
	if request.LaneID == "" || strings.TrimSpace(request.Prompt) == "" || run == nil {
		return NativeACPLaneResult{}, errors.New("grok ACP lane request is incomplete")
	}
	if request.PermissionMode != "bypassPermissions" {
		return NativeACPLaneResult{}, fmt.Errorf("%w: %q", ErrGrokUnsupportedPermissionMode, request.PermissionMode)
	}
	turnCtx, cancel := context.WithCancel(ctx)
	if err := a.register(request.LaneID, cancel); err != nil {
		cancel()
		return NativeACPLaneResult{}, err
	}
	defer func() { cancel(); a.complete(request.LaneID) }()
	result, err := run(turnCtx, request)
	if err == nil && request.NativeSession != "" && result.NativeSessionID != request.NativeSession {
		return NativeACPLaneResult{}, errors.New("grok ACP resumed a different native session")
	}
	return result, err
}

// Interrupt cancels the active Grok ACP driver for laneID.
func (a *GrokLaneAdapter) Interrupt(laneID string) error { return a.interrupt(laneID, "grok") }

// Archive verifies that no active Grok ACP driver remains for laneID.
func (a *GrokLaneAdapter) Archive(laneID string) error { return a.archive(laneID, "grok") }

func (a *GrokLaneAdapter) register(laneID string, cancel context.CancelFunc) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.active[laneID]; exists {
		return errors.New("grok lane already has an active ACP driver")
	}
	a.active[laneID] = cancel
	return nil
}

func (a *GrokLaneAdapter) complete(laneID string) {
	a.mu.Lock()
	delete(a.active, laneID)
	a.mu.Unlock()
}

func (a *GrokLaneAdapter) interrupt(laneID, product string) error {
	a.mu.Lock()
	cancel, ok := a.active[laneID]
	a.mu.Unlock()
	if !ok {
		return errors.New(product + " lane has no active ACP driver")
	}
	cancel()
	return nil
}

func (a *GrokLaneAdapter) archive(laneID, product string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, active := a.active[laneID]; active {
		return errors.New("refuse to archive an active " + product + " ACP driver")
	}
	return nil
}
