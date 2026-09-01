package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// ClaudeLaneRequest is the presence-sensitive worker and transcript request.
type ClaudeLaneRequest struct {
	LaneID, NativeSession, Name, Prompt, PermissionMode string
	Arguments                                           []string
	Resume                                              bool
}

// ClaudeLaneCommand is the exact supported native worker argv.
type ClaudeLaneCommand struct{ Arguments []string }

// ClaudeLaneAdapter owns one live worker per lane and the Claude transcript
// launch/resume shape. Process execution remains a host callback.
type ClaudeLaneAdapter struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// NewClaudeLaneAdapter returns an empty Claude worker registry.
func NewClaudeLaneAdapter() *ClaudeLaneAdapter {
	return &ClaudeLaneAdapter{active: map[string]context.CancelFunc{}}
}

// Command validates a Claude lane request and renders the exact native argv.
func (a *ClaudeLaneAdapter) Command(request ClaudeLaneRequest) (ClaudeLaneCommand, error) {
	if request.LaneID == "" || request.Name == "" || strings.TrimSpace(request.Prompt) == "" {
		return ClaudeLaneCommand{}, errors.New("claude lane request is incomplete")
	}
	args := []string{"-p", "--output-format", "json", "--name", request.Name}
	if request.Resume {
		if request.NativeSession == "" {
			return ClaudeLaneCommand{}, errors.New("claude resume requires transcript identity")
		}
		args = append(args, "--resume", request.NativeSession)
	} else {
		args = append(args, "--session-id", request.LaneID)
	}
	if request.PermissionMode != "" {
		args = append(args, "--permission-mode", request.PermissionMode)
	}
	args = append(args, request.Arguments...)
	args = append(args, request.Prompt)
	return ClaudeLaneCommand{Arguments: args}, nil
}

// Register records the cancellation function for one active Claude lane.
func (a *ClaudeLaneAdapter) Register(laneID string, cancel context.CancelFunc) error {
	if laneID == "" || cancel == nil {
		return errors.New("claude worker registration is incomplete")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.active[laneID]; exists {
		return errors.New("claude lane already has an active stream worker")
	}
	a.active[laneID] = cancel
	return nil
}

// Complete removes the active worker record after its native process exits.
func (a *ClaudeLaneAdapter) Complete(laneID string) {
	a.mu.Lock()
	delete(a.active, laneID)
	a.mu.Unlock()
}

// Interrupt cancels the active worker for laneID.
func (a *ClaudeLaneAdapter) Interrupt(laneID string) error {
	a.mu.Lock()
	cancel, ok := a.active[laneID]
	a.mu.Unlock()
	if !ok {
		return errors.New("claude lane has no active stream worker")
	}
	cancel()
	return nil
}

// Archive verifies that no active worker remains for laneID.
func (a *ClaudeLaneAdapter) Archive(laneID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, active := a.active[laneID]; active {
		return errors.New("refuse to archive an active claude stream worker")
	}
	return nil
}
