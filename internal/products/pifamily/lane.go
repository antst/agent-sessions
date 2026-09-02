package pifamily

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type PermissionMapper func(permissionmode.Mode) (PermissionPolicy, error)

type LaneConfig struct {
	Quirks        Quirks
	Executable    string
	Generation    uint64
	Processes     ProcessFactory
	MapPermission PermissionMapper
	Now           func() time.Time
}

type LaneDriver struct {
	config LaneConfig
	mu     sync.Mutex
	lanes  map[string]*laneSession
}

type laneSession struct {
	mu         sync.Mutex
	ref        productruntime.NativeSessionRef
	permission permissionmode.Mode
	process    RPCProcess
	client     *rpcClient
	cancel     context.CancelFunc
	active     *laneTurn
	closed     bool
}

type laneTurn struct {
	ref          productruntime.NativeTurnRef
	interrupted  bool
	terminal     *rpcTerminal
	terminalWait chan struct{}
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if err := config.Quirks.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Executable) == "" {
		config.Executable = config.Quirks.Executable
	}
	if config.Generation == 0 || config.Processes == nil || config.MapPermission == nil {
		return nil, errors.New("Pi-family lane requires generation, process factory, and permission mapper")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &LaneDriver{config: config, lanes: make(map[string]*laneSession)}, nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{Steer: true, DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if ctx == nil {
		return productruntime.NativeSessionRef{}, errors.New("Pi-family lane open requires context")
	}
	if err := ctx.Err(); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if request.ProductID != driver.config.Quirks.ProductID || strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(request.Cwd) == "" {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: lane product, id, and cwd must be exact", productruntime.ErrNativeRejected)
	}
	if request.ResumeNativeID == "" && strings.TrimSpace(request.Name) == "" {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: fresh Pi-family lane requires a product-native name", productruntime.ErrNativeRejected)
	}
	for _, argument := range request.Arguments {
		if reservedArgument(argument) {
			return productruntime.NativeSessionRef{}, fmt.Errorf("%w: native argument %q is owned by the Pi-family lane lifecycle", productruntime.ErrUnsupportedPolicy, argument)
		}
	}
	policy, err := driver.config.MapPermission(request.PermissionMode)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	driver.mu.Lock()
	if _, exists := driver.lanes[request.LaneID]; exists {
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: lane %q already has an ephemeral native client", productruntime.ErrAmbiguousSession, request.LaneID)
	}
	driver.mu.Unlock()

	arguments := append(driver.config.Quirks.modeArguments(), request.Arguments...)
	if request.ResumeNativeID != "" {
		arguments = append(arguments, driver.config.Quirks.resumeArguments(request.ResumeNativeID)...)
	} else {
		arguments = append(arguments, "--name", request.Name)
	}
	arguments = append(arguments, policy.Args...)
	command := productruntime.NativeCommand{Path: driver.config.Executable, Args: arguments, Cwd: request.Cwd}
	lifetime, cancel := context.WithCancel(context.Background())
	process, err := driver.config.Processes.StartRPC(lifetime, command)
	if err != nil {
		cancel()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: start %s RPC: %v", productruntime.ErrUnavailable, request.ProductID, err)
	}
	client, err := newRPCClient(process, driver.config.Quirks)
	if err != nil {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, err)
	}
	state, err := client.handshake(ctx)
	if err != nil {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, err)
	}
	if request.ResumeNativeID != "" && state.SessionID != request.ResumeNativeID {
		primary := fmt.Errorf("%w: resume returned native session %q, want %q", productruntime.ErrAmbiguousSession, state.SessionID, request.ResumeNativeID)
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, primary)
	}
	ref := productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: state.SessionID, Generation: driver.config.Generation}
	session := &laneSession{ref: ref, permission: request.PermissionMode, process: process, client: client, cancel: cancel}
	driver.mu.Lock()
	if _, exists := driver.lanes[request.LaneID]; exists || driver.nativeSessionInUseLocked(state.SessionID) {
		driver.mu.Unlock()
		primary := fmt.Errorf("%w: native session %q already has a lane owner", productruntime.ErrAmbiguousSession, state.SessionID)
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, primary)
	}
	driver.lanes[request.LaneID] = session
	driver.mu.Unlock()
	return ref, nil
}

func cleanupOpenFailure(cancel context.CancelFunc, process RPCProcess, primary error) error {
	cancel()
	if cleanupErr := process.Cleanup(context.Background()); cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("clean failed Pi-family RPC launch: %w", cleanupErr))
	}
	return primary
}

func (driver *LaneDriver) StartTurn(ctx context.Context, ref productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	session, err := driver.session(ref)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if request.PermissionMode != session.permission {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: per-turn policy differs from the exact native launch policy", productruntime.ErrUnsupportedPolicy)
	}
	prompt, err := lanePrompt(request.Prompt)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	state, err := driver.reconcileState(ctx, session)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if state.IsStreaming || state.IsCompacting {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: native session is not idle for a new turn", productruntime.ErrAmbiguousSession)
	}
	session.mu.Lock()
	if session.closed || session.active != nil {
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: lane already has an active native turn", productruntime.ErrAmbiguousSession)
	}
	// Reserve before native I/O so concurrent starts cannot both reach prompt.
	session.active = &laneTurn{}
	session.mu.Unlock()
	response, err := session.client.call(ctx, "prompt", map[string]any{"message": prompt})
	if err != nil {
		session.mu.Lock()
		session.active = nil
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, err
	}
	turnRef := productruntime.NativeTurnRef{NativeSessionRef: ref, NativeTurnID: response.ID}
	session.mu.Lock()
	session.active.ref = turnRef
	session.mu.Unlock()
	return turnRef, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, ref productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	session, turn, err := driver.turn(ref)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	terminalEvent, err := driver.waitTurnTerminal(ctx, session, turn)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	result, resultErr := session.client.lastAssistantText(ctx)
	if resultErr != nil {
		return productruntime.NativeTerminal{}, resultErr
	}
	session.mu.Lock()
	interrupted := turn.interrupted
	session.mu.Unlock()
	outcome, stopReason := terminalOutcome(terminalEvent, interrupted)
	terminal := productruntime.NativeTerminal{
		Outcome: outcome, Result: result, ResultDigest: sha256.Sum256([]byte(result)), NativeStopReason: stopReason,
	}
	if outcome == productruntime.TurnFailed {
		terminal.ExitLike = 1
	}
	session.mu.Lock()
	if session.active == turn {
		session.active = nil
	}
	session.mu.Unlock()
	return terminal, nil
}

func (*LaneDriver) waitTurnTerminal(ctx context.Context, session *laneSession, turn *laneTurn) (rpcTerminal, error) {
	for {
		session.mu.Lock()
		if session.active != turn {
			session.mu.Unlock()
			return rpcTerminal{}, fmt.Errorf("%w: native turn is no longer active", productruntime.ErrStale)
		}
		if turn.terminal != nil {
			terminal := *turn.terminal
			session.mu.Unlock()
			return terminal, nil
		}
		if turn.terminalWait == nil {
			wait := make(chan struct{})
			turn.terminalWait = wait
			session.mu.Unlock()

			terminal, err := session.client.waitTerminal(ctx)
			session.mu.Lock()
			if turn.terminalWait == wait {
				if err == nil && session.active == turn && turn.terminal == nil {
					observed := terminal
					turn.terminal = &observed
				}
				turn.terminalWait = nil
				close(wait)
			}
			cached := turn.terminal
			session.mu.Unlock()
			if err != nil {
				return rpcTerminal{}, err
			}
			if cached == nil {
				return rpcTerminal{}, fmt.Errorf("%w: observed terminal lost its exact turn", productruntime.ErrStale)
			}
			return *cached, nil
		}
		wait := turn.terminalWait
		session.mu.Unlock()
		select {
		case <-ctx.Done():
			return rpcTerminal{}, ctx.Err()
		case <-wait:
		}
	}
}

func (driver *LaneDriver) Steer(ctx context.Context, ref productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	session, _, err := driver.turn(ref)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if request.PermissionMode != session.permission {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: steer policy differs from the exact native launch policy", productruntime.ErrUnsupportedPolicy)
	}
	prompt, err := lanePrompt(request.Prompt)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	state, err := driver.reconcileState(ctx, session)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if !state.IsStreaming || state.IsCompacting {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: native session is not accepting a steer", productruntime.ErrNativeRejected)
	}
	// OMP's native steer command performs its own <system-notice> framing. The
	// adapter passes the original content exactly once and never pre-wraps it.
	response, err := session.client.call(ctx, "steer", map[string]any{"message": prompt})
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	return productruntime.NativeAcceptance{
		NativeSessionID: ref.NativeSessionID, NativeMessageID: response.ID, AcceptedAt: driver.config.Now().UTC(),
	}, nil
}

func (driver *LaneDriver) Interrupt(ctx context.Context, ref productruntime.NativeTurnRef) error {
	session, turn, err := driver.turn(ref)
	if err != nil {
		return err
	}
	state, err := driver.reconcileState(ctx, session)
	if err != nil {
		return err
	}
	if !state.IsStreaming {
		return fmt.Errorf("%w: native turn is no longer running", productruntime.ErrStale)
	}
	session.mu.Lock()
	turn.interrupted = true
	session.mu.Unlock()
	if _, err := session.client.call(ctx, "abort", nil); err != nil {
		session.mu.Lock()
		turn.interrupted = false
		session.mu.Unlock()
		return err
	}
	return nil
}

func (driver *LaneDriver) Archive(ctx context.Context, ref productruntime.NativeSessionRef) error {
	session, err := driver.session(ref)
	if err != nil {
		return err
	}
	if err := driver.reconcile(ctx, session); err != nil && !errors.Is(err, productruntime.ErrUnavailable) {
		return err
	}
	session.mu.Lock()
	if session.active != nil {
		session.mu.Unlock()
		return fmt.Errorf("%w: cannot archive a running native turn", productruntime.ErrNativeRejected)
	}
	session.mu.Unlock()
	// Archive retains the product JSONL transcript and stops only the exactly
	// owned RPC process group.
	if err := session.process.Cleanup(ctx); err != nil {
		return fmt.Errorf("archive exact RPC process: %w", err)
	}
	session.cancel()
	session.mu.Lock()
	session.closed = true
	session.mu.Unlock()
	driver.mu.Lock()
	if driver.lanes[ref.LaneID] == session {
		delete(driver.lanes, ref.LaneID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) session(ref productruntime.NativeSessionRef) (*laneSession, error) {
	driver.mu.Lock()
	session := driver.lanes[ref.LaneID]
	driver.mu.Unlock()
	if session == nil || session.ref != ref {
		return nil, fmt.Errorf("%w: native lane reference is stale", productruntime.ErrStale)
	}
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("%w: native lane is archived", productruntime.ErrStale)
	}
	return session, nil
}

func (driver *LaneDriver) turn(ref productruntime.NativeTurnRef) (*laneSession, *laneTurn, error) {
	session, err := driver.session(ref.NativeSessionRef)
	if err != nil {
		return nil, nil, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.active == nil || session.active.ref != ref {
		return nil, nil, fmt.Errorf("%w: native turn reference is stale", productruntime.ErrStale)
	}
	return session, session.active, nil
}

func (driver *LaneDriver) reconcile(ctx context.Context, session *laneSession) error {
	_, err := driver.reconcileState(ctx, session)
	return err
}

func (driver *LaneDriver) reconcileState(ctx context.Context, session *laneSession) (rpcSessionState, error) {
	state, err := session.client.getState(ctx)
	if err != nil {
		return rpcSessionState{}, err
	}
	if state.SessionID != session.ref.NativeSessionID {
		return rpcSessionState{}, fmt.Errorf("%w: live RPC session changed from %q to %q", productruntime.ErrAmbiguousSession, session.ref.NativeSessionID, state.SessionID)
	}
	return state, nil
}

func (driver *LaneDriver) nativeSessionInUseLocked(nativeSessionID string) bool {
	for _, session := range driver.lanes {
		if session.ref.NativeSessionID == nativeSessionID {
			return true
		}
	}
	return false
}

func lanePrompt(prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" || len(prompt) > MaxPromptBytes || !utf8.ValidString(prompt) || strings.IndexByte(prompt, 0) >= 0 {
		return "", fmt.Errorf("%w: prompt is not valid bounded native text", productruntime.ErrProtocol)
	}
	return prompt, nil
}

func terminalOutcome(event rpcTerminal, interrupted bool) (productruntime.TurnOutcome, string) {
	if interrupted {
		return productruntime.TurnInterrupted, "aborted"
	}
	stopReason := "settled"
	for index := len(event.Messages) - 1; index >= 0; index-- {
		var message map[string]any
		if jsonUnmarshal(event.Messages[index], &message) != nil {
			continue
		}
		for _, key := range []string{"stopReason", "stop_reason"} {
			if value, ok := message[key].(string); ok && value != "" {
				stopReason = value
				break
			}
		}
		if stopReason != "settled" {
			break
		}
	}
	switch strings.ToLower(stopReason) {
	case "aborted", "cancelled", "canceled", "interrupt", "interrupted":
		return productruntime.TurnInterrupted, stopReason
	case "error", "failed":
		return productruntime.TurnFailed, stopReason
	default:
		return productruntime.TurnCompleted, stopReason
	}
}

// Kept as a tiny seam so lane.go does not expose encoding/json types in its
// public API.
var jsonUnmarshal = func(body []byte, value any) error {
	return json.Unmarshal(body, value)
}
