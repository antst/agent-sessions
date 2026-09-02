package qwen

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const ProductID = "qwen"

type ProcessFactory interface {
	StartRPC(context.Context, productruntime.NativeCommand) (rpcProcess, error)
}

type LaneConfig struct {
	Executable     string
	HostExecutable string
	Generation     uint64
	Processes      ProcessFactory
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
	process    rpcProcess
	client     *rpcClient
	cancel     context.CancelFunc
	active     *laneTurn
	closed     bool
}

type laneTurn struct {
	ref         productruntime.NativeTurnRef
	future      *rpcFuture
	output      strings.Builder
	interrupted bool
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if strings.TrimSpace(config.Executable) == "" || strings.TrimSpace(config.HostExecutable) == "" || config.Generation == 0 || config.Processes == nil {
		return nil, errors.New("Qwen lane requires executable, host executable, generation, and process factory")
	}
	return &LaneDriver{config: config, lanes: make(map[string]*laneSession)}, nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if ctx == nil {
		return productruntime.NativeSessionRef{}, errors.New("Qwen lane open requires context")
	}
	if err := ctx.Err(); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if request.ProductID != ProductID || strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(request.Cwd) == "" || !request.PermissionMode.Valid() {
		return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
	}
	if request.ResumeNativeID == "" && strings.TrimSpace(request.Name) == "" {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: fresh Qwen lane requires a product-native name", productruntime.ErrNativeRejected)
	}
	for _, argument := range request.Arguments {
		if reservedArgument(argument) {
			return productruntime.NativeSessionRef{}, fmt.Errorf("%w: native argument %q is owned by the Qwen lane lifecycle", productruntime.ErrUnsupportedPolicy, argument)
		}
	}
	driver.mu.Lock()
	if existing := driver.lanes[request.LaneID]; existing != nil {
		existing.mu.Lock()
		live := !existing.closed && existing.ref.NativeSessionID == request.ResumeNativeID && existing.permission == request.PermissionMode
		ref := existing.ref
		existing.mu.Unlock()
		driver.mu.Unlock()
		if live && request.ResumeNativeID != "" {
			return ref, nil
		}
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: lane %q already has an ephemeral Qwen client", productruntime.ErrAmbiguousSession, request.LaneID)
	}
	driver.mu.Unlock()

	arguments := []string{"--acp"}
	if request.PermissionMode == permissionmode.BypassPermissions {
		arguments = append(arguments, "--yolo")
	}
	arguments = append(arguments, request.Arguments...)
	lifetime, cancel := context.WithCancel(context.Background())
	process, err := driver.config.Processes.StartRPC(lifetime, productruntime.NativeCommand{
		Path: driver.config.Executable, Args: arguments, Cwd: request.Cwd,
	})
	if err != nil {
		cancel()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: start Qwen ACP: %v", productruntime.ErrUnavailable, err)
	}
	client, err := newRPCClient(process)
	if err != nil {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, err)
	}
	initialized, err := client.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, err)
	}
	agent := mapValue(initialized["agentInfo"])
	protocol, _ := initialized["protocolVersion"].(float64)
	name, _ := agent["name"].(string)
	if int(protocol) != 1 || name != "qwen-code" {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, fmt.Errorf("%w: Qwen ACP initialize returned the wrong product", productruntime.ErrProtocol))
	}
	params := map[string]any{"cwd": request.Cwd, "mcpServers": []any{driver.mcpServer(request.LaneID)}}
	method := "session/new"
	if request.ResumeNativeID != "" {
		capabilities := mapValue(initialized["agentCapabilities"])
		load, _ := capabilities["loadSession"].(bool)
		if !load {
			return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, fmt.Errorf("%w: Qwen ACP does not support session resume", productruntime.ErrIncompatible))
		}
		method, params["sessionId"] = "session/resume", request.ResumeNativeID
	}
	opened, err := client.call(ctx, method, params)
	if err != nil {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, err)
	}
	nativeID, _ := opened["sessionId"].(string)
	if nativeID == "" {
		nativeID = request.ResumeNativeID
	}
	if nativeID == "" || request.ResumeNativeID != "" && nativeID != request.ResumeNativeID {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, fmt.Errorf("%w: Qwen ACP selected a different native session", productruntime.ErrAmbiguousSession))
	}
	mode, _ := mapValue(opened["modes"])["currentModeId"].(string)
	if request.PermissionMode == permissionmode.BypassPermissions && mode != "yolo" {
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, fmt.Errorf("%w: Qwen ACP mode %q is not yolo", productruntime.ErrUnsupportedPolicy, mode))
	}
	if request.ResumeNativeID == "" {
		renamed, renameErr := client.call(ctx, "renameSession", map[string]any{"sessionId": nativeID, "title": request.Name})
		success, _ := renamed["success"].(bool)
		if renameErr != nil || !success {
			if renameErr == nil {
				renameErr = errors.New("Qwen ACP did not accept the session name")
			}
			return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, renameErr)
		}
	}
	ref := productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: nativeID, Generation: driver.config.Generation}
	session := &laneSession{ref: ref, permission: request.PermissionMode, process: process, client: client, cancel: cancel}
	client.setNotificationHandler(session.handleNotification)
	driver.mu.Lock()
	if _, exists := driver.lanes[request.LaneID]; exists || driver.nativeSessionInUseLocked(nativeID) {
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, fmt.Errorf("%w: Qwen native session %q already has a lane owner", productruntime.ErrAmbiguousSession, nativeID))
	}
	driver.lanes[request.LaneID] = session
	driver.mu.Unlock()
	return ref, nil
}

func cleanupOpenFailure(cancel context.CancelFunc, process rpcProcess, primary error) error {
	cancel()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cleanupCancel()
	if cleanupErr := process.Cleanup(cleanupCtx); cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("clean failed Qwen ACP launch: %w", cleanupErr))
	}
	return primary
}

func (driver *LaneDriver) StartTurn(ctx context.Context, ref productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	session, err := driver.session(ref)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if request.PermissionMode != session.permission {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: per-turn policy differs from the Qwen launch policy", productruntime.ErrUnsupportedPolicy)
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: Qwen lane prompt is empty", productruntime.ErrNativeRejected)
	}
	session.mu.Lock()
	if session.closed || session.active != nil {
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: Qwen lane already has an active turn", productruntime.ErrAmbiguousSession)
	}
	turn := &laneTurn{}
	session.active = turn
	future, err := session.client.start(ctx, "session/prompt", map[string]any{
		"sessionId": ref.NativeSessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": request.Prompt}},
	})
	if err != nil {
		session.active = nil
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, err
	}
	turn.future = future
	turn.ref = productruntime.NativeTurnRef{NativeSessionRef: ref, NativeTurnID: strconv.FormatInt(future.id, 10)}
	session.mu.Unlock()
	return turn.ref, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, ref productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	session, turn, err := driver.turn(ref)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	result, waitErr := turn.future.wait(ctx)
	if waitErr != nil {
		if ctx != nil && ctx.Err() != nil {
			return productruntime.NativeTerminal{}, ctx.Err()
		}
		driver.clearTurn(session, turn)
		return productruntime.NativeTerminal{Outcome: productruntime.TurnFailed, ExitLike: 1, NativeStopReason: "native-error"}, waitErr
	}
	session.mu.Lock()
	output := turn.output.String()
	interrupted := turn.interrupted
	if session.active == turn {
		session.active = nil
	}
	session.mu.Unlock()
	stopReason, _ := result["stopReason"].(string)
	terminal := productruntime.NativeTerminal{
		Outcome: productruntime.TurnCompleted, Result: output, ResultDigest: sha256.Sum256([]byte(output)), NativeStopReason: stopReason,
	}
	if stopReason == "cancelled" || interrupted {
		terminal.Outcome, terminal.ExitLike = productruntime.TurnInterrupted, 130
	}
	return terminal, nil
}

func (*LaneDriver) Steer(context.Context, productruntime.NativeTurnRef, productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
}

func (driver *LaneDriver) Interrupt(ctx context.Context, ref productruntime.NativeTurnRef) error {
	session, turn, err := driver.turn(ref)
	if err != nil {
		return err
	}
	result, err := session.client.call(ctx, "craft/cancelPendingPrompt", map[string]any{"sessionId": ref.NativeSessionID})
	if err != nil {
		return err
	}
	cancelled, _ := result["cancelled"].(bool)
	if !cancelled {
		return fmt.Errorf("%w: Qwen reported no prompt to cancel", productruntime.ErrNativeRejected)
	}
	session.mu.Lock()
	if session.active == turn {
		turn.interrupted = true
	}
	session.mu.Unlock()
	return nil
}

func (driver *LaneDriver) Archive(ctx context.Context, ref productruntime.NativeSessionRef) error {
	session, err := driver.session(ref)
	if err != nil {
		return err
	}
	driver.mu.Lock()
	if driver.lanes[ref.LaneID] != session {
		driver.mu.Unlock()
		return productruntime.ErrStale
	}
	delete(driver.lanes, ref.LaneID)
	driver.mu.Unlock()
	session.mu.Lock()
	session.closed = true
	session.mu.Unlock()
	session.cancel()
	return session.process.Cleanup(ctx)
}

func (driver *LaneDriver) session(ref productruntime.NativeSessionRef) (*laneSession, error) {
	if ref.Generation != driver.config.Generation || strings.TrimSpace(ref.LaneID) == "" || strings.TrimSpace(ref.NativeSessionID) == "" {
		return nil, productruntime.ErrStale
	}
	driver.mu.Lock()
	session := driver.lanes[ref.LaneID]
	driver.mu.Unlock()
	if session == nil || session.ref != ref {
		return nil, productruntime.ErrStale
	}
	return session, nil
}

func (driver *LaneDriver) turn(ref productruntime.NativeTurnRef) (*laneSession, *laneTurn, error) {
	session, err := driver.session(ref.NativeSessionRef)
	if err != nil {
		return nil, nil, err
	}
	session.mu.Lock()
	turn := session.active
	valid := turn != nil && turn.ref == ref
	session.mu.Unlock()
	if !valid {
		return nil, nil, productruntime.ErrStale
	}
	return session, turn, nil
}

func (driver *LaneDriver) clearTurn(session *laneSession, turn *laneTurn) {
	session.mu.Lock()
	if session.active == turn {
		session.active = nil
	}
	session.mu.Unlock()
}

func (driver *LaneDriver) nativeSessionInUseLocked(nativeID string) bool {
	for _, session := range driver.lanes {
		if session.ref.NativeSessionID == nativeID {
			return true
		}
	}
	return false
}

func (session *laneSession) handleNotification(message map[string]any) {
	method, _ := message["method"].(string)
	if method != "session/update" {
		return
	}
	params := mapValue(message["params"])
	nativeID, _ := params["sessionId"].(string)
	if nativeID != "" && nativeID != session.ref.NativeSessionID {
		return
	}
	update := mapValue(params["update"])
	kind, _ := update["sessionUpdate"].(string)
	if kind != "agent_message_chunk" {
		return
	}
	content := mapValue(update["content"])
	text, _ := content["text"].(string)
	if text == "" {
		text, _ = update["text"].(string)
	}
	if text == "" {
		return
	}
	session.mu.Lock()
	if session.active != nil {
		session.active.output.WriteString(text)
	}
	session.mu.Unlock()
}

func (driver *LaneDriver) mcpServer(laneID string) map[string]any {
	environment := map[string]string{
		"AGENT_SESSIONS_HOST_BINARY": driver.config.HostExecutable,
		"AGENT_SESSIONS_PRODUCT":     ProductID,
		"AGENT_SESSIONS_SESSION_ID":  laneID,
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]any, 0, len(names))
	for _, name := range names {
		values = append(values, map[string]any{"name": name, "value": environment[name]})
	}
	return map[string]any{
		"name": "agent_sessions", "command": driver.config.HostExecutable,
		"args": []any{"connector", ProductID}, "env": values,
	}
}

func reservedArgument(argument string) bool {
	name := argument
	if index := strings.IndexByte(name, '='); index >= 0 {
		name = name[:index]
	}
	switch name {
	case "--acp", "--approval-mode", "--yolo", "-r", "--resume", "-c", "--continue",
		"--session-id", "-p", "--prompt", "-i", "--prompt-interactive", "-o", "--output-format", "-n", "--name":
		return true
	default:
		return argument == "--"
	}
}
