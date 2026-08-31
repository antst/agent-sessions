package daemon

import (
	"context"
	"errors"
	"strings"
)

// CodexLaneSession is the App Server thread identity and effective policy for
// one daemon-owned Codex lane.
type CodexLaneSession struct {
	ID             string
	Cwd            string
	ApprovalPolicy string
	Sandbox        string
}

// CodexLaneRequest selects thread start or exact transcript resume.
type CodexLaneRequest struct {
	LaneID         string
	NativeSession  string
	Cwd            string
	Name           string
	PermissionMode string
	ApprovalPolicy string
	Sandbox        string
	Resume         bool
	Unarchive      bool
}

// CodexLanePrompt is the exact App Server turn/start request.
type CodexLanePrompt struct {
	ThreadID       string
	Prompt         string
	Effort         string
	ApprovalPolicy string
	Sandbox        string
	SchemaPath     string
	Arguments      []string
}

// CodexLaneTerminal is product-owned terminal evidence.
type CodexLaneTerminal struct {
	ThreadID string
	TurnID   string
	Outcome  string
	Result   string
}

// CodexLaneAdapterConfig retains only App Server callbacks at the product
// boundary. The shared LaneEngine owns no Codex transcript content.
type CodexLaneAdapterConfig struct {
	Start     func(context.Context, CodexLaneRequest) (CodexLaneSession, error)
	Resume    func(context.Context, CodexLaneRequest) (CodexLaneSession, error)
	StartTurn func(context.Context, CodexLanePrompt) (string, error)
	Wait      func(context.Context, string, string) (CodexLaneTerminal, error)
	Interrupt func(context.Context, string, string) error
	Archive   func(context.Context, string) error
}

// CodexLaneAdapter connects the durable lane engine to Codex App Server.
type CodexLaneAdapter struct{ config CodexLaneAdapterConfig }

// NewCodexLaneAdapter validates the complete native callback surface.
func NewCodexLaneAdapter(config CodexLaneAdapterConfig) (*CodexLaneAdapter, error) {
	if config.Start == nil || config.Resume == nil || config.StartTurn == nil || config.Wait == nil ||
		config.Interrupt == nil || config.Archive == nil {
		return nil, errors.New("codex lane adapter requires every App Server callback")
	}
	return &CodexLaneAdapter{config: config}, nil
}

// Prepare starts or resumes one exact thread and normalizes bypass policy only
// at the Codex boundary.
func (a *CodexLaneAdapter) Prepare(ctx context.Context, request CodexLaneRequest) (CodexLaneSession, error) {
	if strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(request.Cwd) == "" {
		return CodexLaneSession{}, errors.New("codex lane request is incomplete")
	}
	if request.PermissionMode == "bypassPermissions" {
		request.ApprovalPolicy, request.Sandbox = "never", "danger-full-access"
	}
	var (
		session CodexLaneSession
		err     error
	)
	if request.Resume {
		if strings.TrimSpace(request.NativeSession) == "" {
			return CodexLaneSession{}, errors.New("codex lane resume requires native session identity")
		}
		session, err = a.config.Resume(ctx, request)
	} else {
		session, err = a.config.Start(ctx, request)
	}
	if err != nil {
		return CodexLaneSession{}, err
	}
	if strings.TrimSpace(session.ID) == "" || request.Resume && session.ID != request.NativeSession {
		return CodexLaneSession{}, errors.New("codex App Server selected a different thread")
	}
	session.ApprovalPolicy, session.Sandbox = request.ApprovalPolicy, request.Sandbox
	return session, nil
}

// StartTurn starts one validated turn on an exact Codex lane thread.
func (a *CodexLaneAdapter) StartTurn(ctx context.Context, prompt CodexLanePrompt) (string, error) {
	if prompt.ThreadID == "" || strings.TrimSpace(prompt.Prompt) == "" {
		return "", errors.New("codex lane turn request is incomplete")
	}
	prompt.Arguments = append([]string(nil), prompt.Arguments...)
	turnID, err := a.config.StartTurn(ctx, prompt)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(turnID) == "" {
		return "", errors.New("codex App Server returned no turn identity")
	}
	return turnID, nil
}

// Wait waits for terminal evidence that matches the requested thread and turn.
func (a *CodexLaneAdapter) Wait(ctx context.Context, threadID, turnID string) (CodexLaneTerminal, error) {
	result, err := a.config.Wait(ctx, threadID, turnID)
	if err == nil && (result.ThreadID != "" && result.ThreadID != threadID || result.TurnID != "" && result.TurnID != turnID) {
		return CodexLaneTerminal{}, errors.New("codex terminal evidence changed native identity")
	}
	return result, err
}

// Interrupt requests interruption of one exact Codex lane turn.
func (a *CodexLaneAdapter) Interrupt(ctx context.Context, threadID, turnID string) error {
	return a.config.Interrupt(ctx, threadID, turnID)
}

// Archive archives one exact Codex lane thread.
func (a *CodexLaneAdapter) Archive(ctx context.Context, threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("codex archive requires thread identity")
	}
	return a.config.Archive(ctx, threadID)
}
