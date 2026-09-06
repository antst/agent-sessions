package dsh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/productruntime"
)

const ManagedProfile = "agent-sessions"

const (
	resumeEnvironment     = "AGENT_SESSIONS_DSH_RESUME"
	modelProviderEnv      = "AGENT_SESSIONS_DSH_MODEL_PROVIDER"
	modelIDEnvironment    = "AGENT_SESSIONS_DSH_MODEL_ID"
	reasoningEffortEnv    = "AGENT_SESSIONS_DSH_REASONING_EFFORT"
	permissionPresetEnv   = "AGENT_SESSIONS_DSH_PERMISSION_PRESET"
	inspectSessionEnv     = "AGENT_SESSIONS_DSH_INSPECT_SESSION_ID"
	cwdEnvironment        = "AGENT_SESSIONS_DSH_CWD"
	defaultStartupTimeout = 15 * time.Second
)

type PresenceRPC interface {
	WaitLane(context.Context, string) error
	CallLane(context.Context, string, string, string, any) (json.RawMessage, error)
}

type LaneConfig struct {
	Executable     string
	Profile        string
	Generation     uint64
	Processes      productruntime.OwnedProcessSupervisor
	Presence       PresenceRPC
	StartupTimeout time.Duration
}

type laneSession struct {
	ref        productruntime.NativeSessionRef
	process    productruntime.OwnedProcessRef
	permission permissionmode.Mode
}

type LaneDriver struct {
	config LaneConfig

	mu       sync.Mutex
	sessions map[string]*laneSession
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if config.Executable == "" {
		config.Executable = "dsh"
	}
	if config.Profile == "" {
		config.Profile = ManagedProfile
	}
	if config.Generation == 0 {
		config.Generation = 1
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = defaultStartupTimeout
	}
	if filepath.Base(config.Executable) != "dsh" || config.Profile != ManagedProfile || config.Processes == nil || config.Presence == nil ||
		config.StartupTimeout < time.Millisecond || config.StartupTimeout > time.Minute {
		return nil, errors.New("DSH native lane driver configuration is incomplete")
	}
	return &LaneDriver{config: config, sessions: map[string]*laneSession{}}, nil
}

func (driver *LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{Steer: true, DurableResume: true, CallerSuppliedSessionID: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if ctx == nil || request.ProductID != ProductID || strings.TrimSpace(request.LaneID) == "" || !validCwd(request.Cwd) {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH lane open request is invalid", productruntime.ErrProtocol)
	}
	if request.ResumeNativeID != "" && request.ResumeNativeID != request.LaneID {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH resume identity differs from the lane identity", productruntime.ErrProtocol)
	}
	policy, err := MapPermission(request.PermissionMode)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	driver.mu.Lock()
	if current := driver.sessions[request.LaneID]; current != nil {
		if request.ResumeNativeID == request.LaneID && current.permission == request.PermissionMode {
			ref := current.ref
			driver.mu.Unlock()
			return ref, nil
		}
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH session %q is already open", productruntime.ErrAmbiguousSession, request.LaneID)
	}
	driver.mu.Unlock()

	provider, model, err := dshModelArgument(request.Arguments)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	environment, err := productruntime.ParseNativeEnvironment(request.Environment)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	environment = setEnvVar(environment, cwdEnvironment, request.Cwd)
	environment = setEnvVar(environment, permissionPresetEnv, policy.Preset)
	if request.ResumeNativeID != "" {
		environment = setEnvVar(environment, resumeEnvironment, "1")
	}
	if model != "" {
		environment = setEnvVar(environment, modelProviderEnv, provider)
		environment = setEnvVar(environment, modelIDEnvironment, model)
	}
	if request.Effort != "" {
		environment = setEnvVar(environment, reasoningEffortEnv, request.Effort)
	}
	process, err := driver.config.Processes.Start(ctx, productruntime.NativeCommand{
		Path: driver.config.Executable, Args: []string{"--profile", driver.config.Profile}, Env: environment, Cwd: request.Cwd,
	})
	if err != nil {
		return productruntime.NativeSessionRef{}, fmt.Errorf("start DSH native lane: %w", err)
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, driver.config.StartupTimeout)
	err = driver.config.Presence.WaitLane(readyCtx, request.LaneID)
	cancelReady()
	if err != nil {
		driver.stopProcess(process)
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH native lane did not publish presence: %v", productruntime.ErrUnavailable, err)
	}
	ref := productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: request.LaneID, Generation: driver.config.Generation}
	session := &laneSession{ref: ref, process: process, permission: request.PermissionMode}
	driver.mu.Lock()
	if driver.sessions[request.LaneID] != nil {
		driver.mu.Unlock()
		driver.stopProcess(process)
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH session %q opened twice", productruntime.ErrAmbiguousSession, request.LaneID)
	}
	driver.sessions[request.LaneID] = session
	driver.mu.Unlock()
	go driver.observeExit(session)
	return ref, nil
}

func (driver *LaneDriver) StartTurn(ctx context.Context, session productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: DSH lane prompt is empty", productruntime.ErrProtocol)
	}
	nativeMessageID, err := driver.startInput(ctx, session, request.Prompt, "followup")
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	return productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: nativeMessageID}, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, turn productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	if _, err := driver.exactSession(turn.NativeSessionRef); err != nil {
		return productruntime.NativeTerminal{}, err
	}
	var result struct {
		Outcome string          `json:"outcome"`
		Result  string          `json:"result"`
		Reason  json.RawMessage `json:"reason"`
	}
	if err := driver.call(ctx, turn.NativeSessionID, "wait", "lane.turn.wait", map[string]any{
		"native_message_id": turn.NativeTurnID,
	}, &result); err != nil {
		return productruntime.NativeTerminal{}, err
	}
	outcome, exitLike, err := dshTurnOutcome(result.Outcome)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	terminal := productruntime.NativeTerminal{
		Outcome: outcome, ExitLike: exitLike, Result: result.Result, NativeStopReason: strings.TrimSpace(string(result.Reason)),
	}
	if outcome == productruntime.TurnFailed {
		return terminal, errors.New(terminal.NativeStopReason)
	}
	return terminal, nil
}

func (driver *LaneDriver) Steer(ctx context.Context, turn productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: DSH steer input is empty", productruntime.ErrProtocol)
	}
	nativeMessageID, err := driver.startInput(ctx, turn.NativeSessionRef, request.Prompt, "steer")
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	return productruntime.NativeAcceptance{NativeSessionID: turn.NativeSessionID, NativeMessageID: nativeMessageID, AcceptedAt: time.Now()}, nil
}

func (driver *LaneDriver) Interrupt(ctx context.Context, turn productruntime.NativeTurnRef) error {
	if _, err := driver.exactSession(turn.NativeSessionRef); err != nil {
		return err
	}
	return driver.call(ctx, turn.NativeSessionID, "interrupt", "lane.turn.interrupt", map[string]any{}, &struct{}{})
}

func (driver *LaneDriver) Archive(ctx context.Context, session productruntime.NativeSessionRef) error {
	owned, err := driver.exactSession(session)
	if err != nil {
		return err
	}
	if err := driver.call(ctx, session.NativeSessionID, "archive", "lane.session.archive", map[string]any{}, &struct{}{}); err != nil {
		return err
	}
	if _, err := driver.config.Processes.Wait(ctx, owned.process); err != nil {
		return fmt.Errorf("wait for archived DSH lane: %w", err)
	}
	driver.mu.Lock()
	if driver.sessions[session.NativeSessionID] == owned {
		delete(driver.sessions, session.NativeSessionID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) startInput(ctx context.Context, session productruntime.NativeSessionRef, body, mode string) (string, error) {
	if _, err := driver.exactSession(session); err != nil {
		return "", err
	}
	inputID, err := newInputID()
	if err != nil {
		return "", err
	}
	var result struct {
		NativeMessageID string `json:"native_message_id"`
	}
	if err := driver.call(ctx, session.NativeSessionID, inputID, "lane.turn.start", map[string]any{
		"input_id": inputID, "body": body, "mode": mode,
	}, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.NativeMessageID) == "" {
		return "", fmt.Errorf("%w: DSH omitted its native message identity", productruntime.ErrProtocol)
	}
	return result.NativeMessageID, nil
}

func (driver *LaneDriver) call(ctx context.Context, sessionID, callID, method string, params any, output any) error {
	raw, err := driver.config.Presence.CallLane(ctx, sessionID, callID, method, params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("%w: decode DSH native %s result: %v", productruntime.ErrProtocol, method, err)
	}
	return nil
}

func (driver *LaneDriver) exactSession(ref productruntime.NativeSessionRef) (*laneSession, error) {
	if ref.LaneID == "" || ref.LaneID != ref.NativeSessionID || ref.Generation != driver.config.Generation {
		return nil, fmt.Errorf("%w: DSH lane reference is stale", productruntime.ErrStale)
	}
	driver.mu.Lock()
	session := driver.sessions[ref.NativeSessionID]
	driver.mu.Unlock()
	if session == nil || session.ref != ref {
		return nil, fmt.Errorf("%w: DSH lane %q is not open", productruntime.ErrStale, ref.NativeSessionID)
	}
	return session, nil
}

func (driver *LaneDriver) observeExit(session *laneSession) {
	_, _ = driver.config.Processes.Wait(context.Background(), session.process)
	driver.mu.Lock()
	if driver.sessions[session.ref.NativeSessionID] == session {
		delete(driver.sessions, session.ref.NativeSessionID)
	}
	driver.mu.Unlock()
}

func (driver *LaneDriver) stopProcess(process productruntime.OwnedProcessRef) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = driver.config.Processes.Signal(ctx, process, productruntime.ProcessTerminate)
	_, _ = driver.config.Processes.Wait(ctx, process)
}

func dshModelArgument(arguments []string) (string, string, error) {
	var selected string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if strings.HasPrefix(argument, "--model=") {
			selected = strings.TrimPrefix(argument, "--model=")
			continue
		}
		if argument == "--model" || argument == "-m" {
			index++
			if index >= len(arguments) {
				return "", "", fmt.Errorf("%w: %s requires provider/model", productruntime.ErrNativeRejected, argument)
			}
			selected = arguments[index]
			continue
		}
		return "", "", fmt.Errorf("%w: DSH lane option %q is unsupported", productruntime.ErrNativeRejected, argument)
	}
	if selected == "" {
		return "", "", nil
	}
	provider, model, ok := strings.Cut(selected, "/")
	if !ok || strings.TrimSpace(provider) != provider || strings.TrimSpace(model) != model || provider == "" || model == "" || strings.Contains(model, "/") {
		return "", "", fmt.Errorf("%w: DSH --model requires provider/model", productruntime.ErrNativeRejected)
	}
	return provider, model, nil
}

func dshTurnOutcome(value string) (productruntime.TurnOutcome, int, error) {
	switch value {
	case "completed":
		return productruntime.TurnCompleted, 0, nil
	case "interrupted":
		return productruntime.TurnInterrupted, 130, nil
	case "failed":
		return productruntime.TurnFailed, 1, nil
	default:
		return "", 0, fmt.Errorf("%w: DSH returned unknown turn outcome %q", productruntime.ErrProtocol, value)
	}
}

func newInputID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "input-" + hex.EncodeToString(bytes), nil
}

func validCwd(cwd string) bool {
	return cwd != "" && filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd
}

func setEnvVar(environment []productruntime.EnvVar, name, value string) []productruntime.EnvVar {
	result := make([]productruntime.EnvVar, 0, len(environment)+1)
	for _, variable := range environment {
		if variable.Name != name {
			result = append(result, variable)
		}
	}
	return append(result, productruntime.EnvVar{Name: name, Value: value})
}
