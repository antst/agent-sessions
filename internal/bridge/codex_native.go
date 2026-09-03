package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/pathidentity"
)

// CodexNativeConfig selects the user's real Codex profile and supported App
// Server lifecycle command. It never creates or copies a credential profile.
type CodexNativeConfig struct {
	CodexBinary string
	CodexHome   string
	SocketPath  string
	Environment []string
	Start       func(context.Context, string, []string, []string) error
	OnEvent     func(CodexNativeEvent)
}

// CodexNativeEvent is one metadata-only App Server lifecycle observation.
type CodexNativeEvent struct {
	Kind     string
	ThreadID string
	TurnID   string
	Name     string
	Status   string
}

// CodexNativeThread is the native identity needed by the daemon attachment.
type CodexNativeThread struct {
	ID             string
	Name           string
	Cwd            string
	UpdatedAt      int64
	Status         string
	ApprovalPolicy string
	Path           string
}

// CodexStartRequest preserves the legacy thread/start and thread/name/set inputs.
type CodexStartRequest struct {
	Cwd            string
	Name           string
	NameSource     string
	ApprovalPolicy string
	Sandbox        string
}

// CodexLaneTurnRequest carries the proven App Server turn/start surface used
// by the legacy Codex lane manager. The unified daemon owns lifecycle state;
// Codex still owns the native thread, turn, policy, and transcript.
type CodexLaneTurnRequest struct {
	ThreadID       string
	Prompt         string
	Model          string
	Effort         string
	ApprovalPolicy string
	Sandbox        string
	SchemaPath     string
	Arguments      []string
}

// CodexLaneTurnResult is the native terminal evidence needed by the unified
// lane actor without copying the vendor transcript into daemon state.
type CodexLaneTurnResult struct {
	ThreadID string
	TurnID   string
	Outcome  string
	Result   string
}

type codexLaneTurnKey struct {
	threadID string
	turnID   string
}

// CodexNative is the in-process native App Server surface transplanted from
// the working supervisor. It owns no user service or per-session process.
type CodexNative struct {
	config        CodexNativeConfig
	paths         nativePaths
	connectionMu  sync.Mutex
	client        *appServerClient
	eventCtx      context.Context
	cancel        context.CancelFunc
	eventWG       sync.WaitGroup
	closed        bool
	mu            sync.Mutex
	activeTurns   map[string]string
	turnWaiters   map[codexLaneTurnKey]chan struct{}
	loadedThreads map[string]bool
}

// OpenCodexNative connects to the selected App Server and lazily starts it
// through `codex app-server daemon start` only when the native socket is absent.
//
//nolint:gocyclo // Lazy native startup validates every profile, executable, socket, and timeout boundary.
func OpenCodexNative(ctx context.Context, config CodexNativeConfig) (*CodexNative, error) {
	if ctx == nil {
		return nil, errors.New("codex native context is nil")
	}
	binary, err := canonicalExecutable(config.CodexBinary)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex binary: %w", err)
	}
	home, err := pathidentity.ExistingDirectory(config.CodexHome)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex home: %w", err)
	}
	config.CodexBinary = binary
	config.CodexHome = home
	if len(config.Environment) == 0 {
		config.Environment = os.Environ()
	}
	if config.SocketPath == "" {
		config.SocketPath = filepath.Join(home, "app-server-control", "app-server-control.sock")
	}
	config.SocketPath, err = pathidentity.FuturePath(config.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex App Server socket: %w", err)
	}

	eventCtx, cancel := context.WithCancel(ctx)
	paths := resolveNativePaths()
	paths.codexHome = home
	native := &CodexNative{
		config: config, paths: paths, eventCtx: eventCtx, cancel: cancel,
		activeTurns: map[string]string{},
		turnWaiters: map[codexLaneTurnKey]chan struct{}{}, loadedThreads: map[string]bool{},
	}
	if _, err := native.clientForOperation(ctx); err != nil {
		cancel()
		return nil, err
	}
	return native, nil
}

// Close drops only this daemon's App Server client. It deliberately leaves the
// user-managed native App Server running for ordinary Codex clients.
func (native *CodexNative) Close() {
	if native == nil {
		return
	}
	native.connectionMu.Lock()
	if native.closed {
		native.connectionMu.Unlock()
		return
	}
	native.closed = true
	client := native.client
	native.connectionMu.Unlock()
	native.cancel()
	if client != nil {
		client.close()
	}
	native.eventWG.Wait()
}

// AppServerEvidence returns the exact peer identity corroborated at socket dial.
func (native *CodexNative) AppServerEvidence() (int, string, string) {
	client := native.clientSnapshot()
	if client == nil {
		return 0, "", native.config.SocketPath
	}
	return client.peerPID, client.peerProcStart, native.config.SocketPath
}

// RefreshAppServerEvidence opens one replacement connection when the previous
// App Server connection has closed. It never replays the operation that saw EOF.
func (native *CodexNative) RefreshAppServerEvidence(ctx context.Context) (int, string, string, error) {
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return 0, "", native.config.SocketPath, err
	}
	return client.peerPID, client.peerProcStart, native.config.SocketPath, nil
}

// ReloadMCPServers asks the already-running App Server to replace its MCP
// children from the current installed configuration. Agent Sessions upgrades
// must not restart the user-owned App Server, but preserving an old long-lived
// connector process would otherwise preserve the previous Agent Sessions image
// indefinitely. This supported App Server operation refreshes only MCPs.
func (native *CodexNative) ReloadMCPServers(ctx context.Context) error {
	if native == nil {
		return errors.New("codex App Server client is unavailable")
	}
	// Plugin installation is external to the long-lived App Server. Refresh its
	// home-scoped marketplace view first; otherwise mcpServer/reload sees the
	// cached pre-install plugin definition and correctly keeps the old child.
	if err := native.request(ctx, 30*time.Second, "plugin/list", map[string]any{
		"forceRefetch": true,
	}, nil); err != nil {
		return fmt.Errorf("refresh Codex plugin inventory: %w", err)
	}
	return native.request(ctx, 30*time.Second, "config/mcpServer/reload", nil, nil)
}

// StartThread preserves the legacy non-ephemeral start, effective-policy
// validation, native naming, and delete-on-failed-preparation transaction.
func (native *CodexNative) StartThread(ctx context.Context, request CodexStartRequest) (result CodexNativeThread, resultErr error) {
	cwd, err := canonicalLaunchDirectory(request.Cwd)
	if err != nil {
		return CodexNativeThread{}, err
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return CodexNativeThread{}, err
	}
	// Codex 0.151 defaults App Server starts to paginated history, but a fresh
	// zero-turn paginated thread has no source rollout and cannot be resumed by
	// the remote TUI. The established peer contract creates a durable legacy
	// rollout first; users may migrate it through Codex's supported migration.
	params := map[string]any{
		"cwd": cwd, "ephemeral": false, "serviceName": "codex-peer", "historyMode": "legacy",
	}
	putIf(params, "approvalPolicy", request.ApprovalPolicy)
	putIf(params, "sandbox", request.Sandbox)
	var started struct {
		Thread         appThread `json:"thread"`
		ApprovalPolicy any       `json:"approvalPolicy"`
	}
	if err := native.request(ctx, 60*time.Second, "thread/start", params, &started); err != nil {
		return CodexNativeThread{}, err
	}
	threadID := started.Thread.ID
	created := validSessionID(threadID)
	defer func() {
		if resultErr != nil && created {
			resultErr = errors.Join(resultErr, deletePreparedThread(client, threadID))
		}
	}()
	if !created {
		return CodexNativeThread{}, errors.New("invalid thread id returned by App Server")
	}
	approvalPolicy := stringValue(started.ApprovalPolicy)
	if approvalPolicy == "" {
		return CodexNativeThread{}, errors.New("thread/start did not report its effective approval policy")
	}
	if request.ApprovalPolicy != "" && approvalPolicy != request.ApprovalPolicy {
		return CodexNativeThread{}, fmt.Errorf("thread/start applied approval policy %q, expected %q", approvalPolicy, request.ApprovalPolicy)
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = defaultPeerName(cwd, threadID)
	} else {
		name = sanitizeName(name)
	}
	if err := native.request(ctx, 30*time.Second, "thread/name/set", map[string]any{
		"threadId": threadID, "name": name,
	}, nil); err != nil {
		return CodexNativeThread{}, err
	}
	// Resume the freshly named zero-turn thread before handing it to the TUI.
	// This materializes Codex's source rollout and applies the effective settings;
	// without it a remote TUI can
	// reject the otherwise valid UUID as a paginated lineage with no source
	// rollout.
	resumed, effectiveApproval, err := resumePreparedThread(client, threadID, map[string]any{
		"cwd": cwd, "approvalPolicy": request.ApprovalPolicy, "sandbox": request.Sandbox,
	})
	if err != nil {
		return CodexNativeThread{}, err
	}
	resumed.Name = name
	resumed.Cwd = cwd
	native.mu.Lock()
	native.loadedThreads[threadID] = true
	native.mu.Unlock()
	return exportedCodexThread(*resumed, effectiveApproval), nil
}

// ResolveThread asks the App Server for the exact UUID that native Codex has
// already created or selected and reported through its lifecycle stream.
func (native *CodexNative) ResolveThread(ctx context.Context, target string) (CodexNativeThread, error) {
	if err := ctx.Err(); err != nil {
		return CodexNativeThread{}, err
	}
	if !exactLaunchThreadIDRE.MatchString(target) {
		return CodexNativeThread{}, fmt.Errorf("codex resume requires an exact thread UUID, got %q", target)
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return CodexNativeThread{}, err
	}
	thread, err := readExactPreparedThread(client, target)
	if err != nil {
		return CodexNativeThread{}, err
	}
	if err := validatePreparedRootThread(thread); err != nil {
		return CodexNativeThread{}, err
	}
	return exportedCodexThread(thread, ""), nil
}

// NameThread writes the fresh peer's product-owned title after Codex reports
// the thread it created. Resume never renames an existing product session.
func (native *CodexNative) NameThread(ctx context.Context, threadID, requested, cwd string) (CodexNativeThread, error) {
	thread, err := native.ResolveThread(ctx, threadID)
	if err != nil {
		return CodexNativeThread{}, err
	}
	return native.NameResolvedThread(ctx, thread, requested, cwd)
}

// NameResolvedThread writes the product-owned title after an exact thread/read
// has already supplied the native identity and launch record.
func (native *CodexNative) NameResolvedThread(ctx context.Context, thread CodexNativeThread, requested, cwd string) (CodexNativeThread, error) {
	if !exactLaunchThreadIDRE.MatchString(thread.ID) {
		return CodexNativeThread{}, errors.New("cannot name a Codex thread without an exact native id")
	}
	name := strings.TrimSpace(requested)
	if name == "" {
		name = defaultPeerName(cwd, thread.ID)
	} else {
		name = sanitizeName(name)
	}
	if err := native.request(ctx, 30*time.Second, "thread/name/set", map[string]any{
		"threadId": thread.ID, "name": name,
	}, nil); err != nil {
		return CodexNativeThread{}, err
	}
	thread.Name = name
	thread.Cwd = cwd
	return thread, nil
}

// ResumeThread performs the same exclude-turns native subscription used by
// managed lanes and recovery, including effective cwd and settings checks.
func (native *CodexNative) ResumeThread(ctx context.Context, threadID, cwd, approvalPolicy, sandbox string) (CodexNativeThread, error) {
	if err := ctx.Err(); err != nil {
		return CodexNativeThread{}, err
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return CodexNativeThread{}, err
	}
	thread, effectiveApproval, err := resumePreparedThread(client, threadID, map[string]any{
		"cwd": cwd, "approvalPolicy": approvalPolicy, "sandbox": sandbox,
	})
	if err != nil {
		return CodexNativeThread{}, err
	}
	native.mu.Lock()
	native.loadedThreads[threadID] = true
	native.mu.Unlock()
	return exportedCodexThread(*thread, effectiveApproval), nil
}

// PrepareLaneThread resumes an unloaded lane transcript, but reuses a thread
// already loaded by this exact App Server client. Queued lane messages arrive
// immediately after the preceding turn completes; issuing thread/resume there
// attempts to acquire a second writer for our own loaded thread and Codex
// correctly rejects it. The working pre-unification supervisor kept one
// subscription and dispatched later turns directly with turn/start.
func (native *CodexNative) PrepareLaneThread(ctx context.Context, threadID, cwd, approvalPolicy, sandbox string) (CodexNativeThread, error) {
	if err := ctx.Err(); err != nil {
		return CodexNativeThread{}, err
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return CodexNativeThread{}, err
	}
	native.mu.Lock()
	loaded := native.loadedThreads[threadID]
	native.mu.Unlock()
	if !loaded {
		archived, err := listThreadMembership(client, true)
		if err != nil {
			return CodexNativeThread{}, err
		}
		if !archived[threadID] {
			return native.ResumeThread(ctx, threadID, cwd, approvalPolicy, sandbox)
		}
		if err := native.UnarchiveThread(ctx, threadID); err != nil {
			return CodexNativeThread{}, err
		}
		thread, err := native.ResumeThread(ctx, threadID, cwd, approvalPolicy, sandbox)
		if err != nil {
			rollbackErr := native.ArchiveThread(context.Background(), threadID)
			return CodexNativeThread{}, errors.Join(err, rollbackErr)
		}
		return thread, nil
	}
	thread, err := readExactPreparedThread(client, threadID)
	if err != nil {
		return CodexNativeThread{}, err
	}
	if statusType(thread.Status) == "notLoaded" {
		native.mu.Lock()
		delete(native.loadedThreads, threadID)
		native.mu.Unlock()
		return native.ResumeThread(ctx, threadID, cwd, approvalPolicy, sandbox)
	}
	if expected := strings.TrimSpace(cwd); expected != "" && strings.TrimSpace(thread.Cwd) != expected {
		return CodexNativeThread{}, fmt.Errorf("loaded Codex lane cwd %q, expected %q", thread.Cwd, expected)
	}
	if err := updatePreparedThreadSettings(client, threadID, approvalPolicy, sandbox); err != nil {
		return CodexNativeThread{}, err
	}
	return exportedCodexThread(thread, approvalPolicy), nil
}

// ReattachThread restores the notification subscription and active-turn
// observation that the coordinator rebuilds after its own restart. It
// deliberately applies no cwd or permission settings to a live TUI.
func (native *CodexNative) ReattachThread(ctx context.Context, threadID string) (CodexNativeThread, error) {
	if err := ctx.Err(); err != nil {
		return CodexNativeThread{}, err
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return CodexNativeThread{}, err
	}
	thread, err := resumeThreadForPeer(client, threadID)
	if err != nil {
		return CodexNativeThread{}, err
	}
	if statusType(thread.Status) == "active" {
		if _, err := native.recoverActiveTurn(ctx, threadID); err != nil {
			return CodexNativeThread{}, err
		}
	}
	native.mu.Lock()
	native.loadedThreads[threadID] = true
	native.mu.Unlock()
	return exportedCodexThread(*thread, ""), nil
}

// ResolveLaneTurnID binds durable daemon recovery to one App Server turn. A
// recorded dispatch ID must still exist; the newest native turn is used only
// for the narrow crash window after turn/start succeeded but before its ID was
// committed to daemon state.
func (native *CodexNative) ResolveLaneTurnID(ctx context.Context, threadID, preferred string) (string, error) {
	if !validSessionID(threadID) {
		return "", errors.New("codex lane recovery requires an exact thread id")
	}
	if preferred != "" {
		native.mu.Lock()
		active := native.activeTurns[threadID]
		native.mu.Unlock()
		if active != "" && active != preferred {
			return "", fmt.Errorf("active Codex lane turn %q does not match durable turn %q", active, preferred)
		}
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return "", err
	}
	turns, err := listLaneTurns(client, threadID, "notLoaded")
	if err != nil {
		return "", err
	}
	if preferred != "" {
		for _, turn := range turns {
			if turn.ID == preferred {
				native.mu.Lock()
				active := native.activeTurns[threadID]
				native.mu.Unlock()
				if active == "" && !statusTerminal(normalizeStatus(turn.Status)) {
					return "", fmt.Errorf("Codex lane turn %s is neither active nor terminal", preferred)
				}
				return preferred, nil
			}
		}
		return "", fmt.Errorf("codex lane turn %s is absent from thread %s", preferred, threadID)
	}
	if len(turns) == 0 || turns[0].ID == "" {
		return "", fmt.Errorf("codex lane thread %s has no recoverable turn", threadID)
	}
	return turns[0].ID, nil
}

// SendMessage preserves legacy active-turn steering and idle turn/start behavior.
func (native *CodexNative) SendMessage(ctx context.Context, threadID, message string) (string, error) {
	if !validSessionID(threadID) || strings.TrimSpace(message) == "" {
		return "", errors.New("codex message requires an exact thread and non-empty content")
	}
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return "", err
	}
	thread, err := readExactPreparedThread(client, threadID)
	if err != nil {
		return "", err
	}
	if statusType(thread.Status) == "notLoaded" {
		resumed, resumeErr := resumeThreadForPeer(client, threadID)
		if resumeErr != nil {
			return "", resumeErr
		}
		thread = *resumed
	}
	input := []map[string]any{{"type": "text", "text": message}}
	if statusType(thread.Status) == "active" {
		native.mu.Lock()
		turnID := native.activeTurns[threadID]
		native.mu.Unlock()
		if turnID == "" {
			turnID, err = native.recoverActiveTurn(ctx, threadID)
			if err != nil {
				return "", err
			}
		}
		if err := native.request(ctx, 30*time.Second, "turn/steer", map[string]any{
			"threadId": threadID, "input": input, "expectedTurnId": turnID,
		}, nil); err != nil {
			return "steered", classifyWakeMutationError(err)
		}
		return "steered", nil
	}
	var started struct {
		Turn appTurn `json:"turn"`
	}
	if err := native.request(ctx, 60*time.Second, "turn/start", map[string]any{
		"threadId": threadID, "input": input,
	}, &started); err != nil {
		return "started", classifyWakeMutationError(err)
	}
	if started.Turn.ID != "" {
		native.mu.Lock()
		native.activeTurns[threadID] = started.Turn.ID
		native.mu.Unlock()
	}
	return "started", nil
}

// StartLaneTurn dispatches the exact App Server turn/start used by the working
// pre-unification lane manager. Callers must publish the prepared thread as an
// attested lane before invoking this method so an immediate MCP call is bound.
func (native *CodexNative) StartLaneTurn(ctx context.Context, request CodexLaneTurnRequest) (string, error) {
	if !validSessionID(request.ThreadID) || strings.TrimSpace(request.Prompt) == "" {
		return "", errors.New("codex lane turn requires an exact thread and non-empty prompt")
	}
	params := map[string]any{
		"threadId": request.ThreadID,
		"input":    []map[string]any{{"type": "text", "text": request.Prompt}},
	}
	putIf(params, "model", request.Model)
	putIf(params, "effort", request.Effort)
	putIf(params, "approvalPolicy", request.ApprovalPolicy)
	if request.Sandbox != "" {
		typeName, ok := map[string]string{
			"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess",
		}[request.Sandbox]
		if !ok {
			return "", fmt.Errorf("unsupported Codex lane sandbox %q", request.Sandbox)
		}
		params["sandboxPolicy"] = map[string]any{"type": typeName}
	}
	if request.SchemaPath != "" {
		schema, err := readLaneOutputSchema(request.SchemaPath)
		if err != nil {
			return "", err
		}
		var decoded any
		if json.Unmarshal(schema, &decoded) != nil {
			return "", errors.New("codex lane output schema is invalid")
		}
		params["outputSchema"] = decoded
	}
	if err := applyCodexLaneArguments(params, request.Arguments); err != nil {
		return "", err
	}
	var started struct {
		Turn appTurn `json:"turn"`
	}
	if err := native.request(ctx, 60*time.Second, "turn/start", params, &started); err != nil {
		return "", err
	}
	if started.Turn.ID == "" {
		return "", errors.New("codex App Server did not return a lane turn id")
	}
	native.mu.Lock()
	native.activeTurns[request.ThreadID] = started.Turn.ID
	native.mu.Unlock()
	return started.Turn.ID, nil
}

// WaitLaneTurn waits for Codex's exact turn/completed notification, then reads
// the closed native turn once to collect its product-owned result. Cancellation
// abandons only this process-local waiter; the daemon owns the separate decision
// to interrupt an exact native turn. A successor daemon whose turn completed
// while disconnected proceeds directly to the same exact product read.
func (native *CodexNative) WaitLaneTurn(
	ctx context.Context,
	threadID, turnID string,
) (CodexLaneTurnResult, error) {
	if !validSessionID(threadID) || turnID == "" {
		return CodexLaneTurnResult{}, errors.New("codex lane wait requires an exact thread and turn")
	}
	key := codexLaneTurnKey{threadID: threadID, turnID: turnID}
	native.mu.Lock()
	active := native.activeTurns[threadID]
	if active != "" && active != turnID {
		native.mu.Unlock()
		return CodexLaneTurnResult{}, fmt.Errorf("active Codex lane turn %q does not match wait turn %q", active, turnID)
	}
	var completed <-chan struct{}
	if active != "" {
		if _, exists := native.turnWaiters[key]; exists {
			native.mu.Unlock()
			return CodexLaneTurnResult{}, fmt.Errorf("Codex lane turn %s already has a waiter", turnID)
		}
		waiter := make(chan struct{})
		native.turnWaiters[key] = waiter
		completed = waiter
	}
	native.mu.Unlock()

	if completed != nil {
		select {
		case <-completed:
		case <-ctx.Done():
			native.mu.Lock()
			if native.turnWaiters[key] == completed {
				delete(native.turnWaiters, key)
			}
			native.mu.Unlock()
			return CodexLaneTurnResult{}, ctx.Err()
		}
	}
	turn, err := native.readExactLaneTurn(ctx, threadID, turnID)
	if err != nil {
		return CodexLaneTurnResult{}, err
	}
	outcome := normalizeStatus(turn.Status)
	if !statusTerminal(outcome) || turn.CompletedAt == nil {
		return CodexLaneTurnResult{}, fmt.Errorf("Codex lane turn %s is not product-closed", turnID)
	}
	result := codexLaneFinalAnswer(turn)
	if outcome == "completed" && strings.TrimSpace(result) == "" {
		return CodexLaneTurnResult{}, fmt.Errorf("completed Codex lane turn %s has no final answer", turnID)
	}
	return CodexLaneTurnResult{
		ThreadID: threadID, TurnID: turnID, Outcome: outcome, Result: result,
	}, nil
}

func (native *CodexNative) readExactLaneTurn(ctx context.Context, threadID, turnID string) (appTurn, error) {
	var page struct {
		Data []appTurn `json:"data"`
	}
	if err := native.request(ctx, 30*time.Second, "thread/turns/list", map[string]any{
		"threadId": threadID, "limit": 100, "sortDirection": "desc", "itemsView": "full",
	}, &page); err != nil {
		return appTurn{}, err
	}
	for _, turn := range page.Data {
		if turn.ID == turnID {
			return turn, nil
		}
	}
	return appTurn{}, fmt.Errorf("codex lane turn %s is absent from thread %s", turnID, threadID)
}

// InterruptLaneTurn interrupts only the selected App Server turn.
func (native *CodexNative) InterruptLaneTurn(ctx context.Context, threadID, turnID string) error {
	if !validSessionID(threadID) || turnID == "" {
		return errors.New("codex lane interrupt requires an exact thread and turn")
	}
	return native.request(ctx, 10*time.Second, "turn/interrupt", map[string]any{
		"threadId": threadID, "turnId": turnID,
	}, nil)
}

func applyCodexLaneArguments(params map[string]any, arguments []string) error {
	config := map[string]any{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		value := ""
		if strings.Contains(argument, "=") {
			parts := strings.SplitN(argument, "=", 2)
			argument, value = parts[0], parts[1]
		}
		switch argument {
		case "-m", "--model":
			if value == "" {
				index++
				if index >= len(arguments) {
					return errors.New("codex lane --model requires a value")
				}
				value = arguments[index]
			}
			params["model"] = value
		case "-c", "--config":
			if value == "" {
				index++
				if index >= len(arguments) {
					return errors.New("codex lane --config requires a value")
				}
				value = arguments[index]
			}
			separator := strings.IndexByte(value, '=')
			if separator <= 0 {
				return errors.New("codex lane config must use KEY=VALUE")
			}
			var decoded any
			if json.Unmarshal([]byte(value[separator+1:]), &decoded) != nil {
				decoded = strings.Trim(value[separator+1:], "\"'")
			}
			if err := assignDottedNative(config, value[:separator], decoded); err != nil {
				return err
			}
		case "--web":
			_ = assignDottedNative(config, "tools.web_search", true)
		case "--no-web":
			_ = assignDottedNative(config, "tools.web_search", false)
		default:
			return fmt.Errorf("unsupported Codex lane native argument %q", argument)
		}
	}
	// The external code-mode host delegates nested tools to a UI client which
	// headless lanes do not have. Preserve the legacy in-server tool path.
	_ = assignDottedNative(config, "features.code_mode_host", false)
	if len(config) > 0 {
		params["config"] = config
	}
	return nil
}

func codexLaneFinalAnswer(turn appTurn) string {
	answer := ""
	for _, raw := range turn.Items {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil || snakeCaseNative(stringValue(item["type"])) != "agent_message" {
			continue
		}
		text := stringValue(item["text"])
		if stringValue(item["phase"]) == "final_answer" {
			answer = text
		} else if answer == "" && text != "" {
			answer = text
		}
	}
	return answer
}

func (native *CodexNative) recoverActiveTurn(ctx context.Context, threadID string) (string, error) {
	var page struct {
		Data []appTurn `json:"data"`
	}
	if err := native.request(ctx, 30*time.Second, "thread/turns/list", map[string]any{
		"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
	}, &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 || statusType(page.Data[0].Status) != "active" || page.Data[0].ID == "" {
		return "", fmt.Errorf("active Codex thread %s did not expose its active turn", threadID)
	}
	turnID := page.Data[0].ID
	native.mu.Lock()
	native.activeTurns[threadID] = turnID
	native.mu.Unlock()
	return turnID, nil
}

// ArchiveThread withdraws native membership; daemon addressability is withdrawn separately first.
func (native *CodexNative) ArchiveThread(ctx context.Context, threadID string) error {
	if err := native.request(ctx, 30*time.Second, "thread/archive", map[string]any{"threadId": threadID}, nil); err != nil {
		return err
	}
	// Preserve the working supervisor's archive boundary: withdrawing native
	// membership is followed by releasing this App Server client's subscription.
	// Without the unsubscribe, Codex keeps the thread's stdio MCP child alive
	// after the lane is no longer addressable.
	if err := native.UnsubscribeThread(ctx, threadID); err != nil {
		return err
	}
	return nil
}

// UnsubscribeThread releases this daemon generation's App Server subscription
// without altering the vendor-owned transcript or archive membership.
func (native *CodexNative) UnsubscribeThread(ctx context.Context, threadID string) error {
	if !validSessionID(threadID) {
		return errors.New("codex unsubscribe requires an exact thread id")
	}
	if err := native.request(ctx, 10*time.Second, "thread/unsubscribe", map[string]any{"threadId": threadID}, nil); err != nil {
		return err
	}
	native.mu.Lock()
	delete(native.loadedThreads, threadID)
	delete(native.activeTurns, threadID)
	native.mu.Unlock()
	return nil
}

// UnarchiveThread restores native membership without publishing daemon authority.
func (native *CodexNative) UnarchiveThread(ctx context.Context, threadID string) error {
	return native.request(ctx, 30*time.Second, "thread/unarchive", map[string]any{"threadId": threadID}, nil)
}

// DeleteThread removes only the exact native thread selected by a failed fresh preparation.
func (native *CodexNative) DeleteThread(ctx context.Context, threadID string) error {
	if err := native.request(ctx, 15*time.Second, "thread/delete", map[string]any{"threadId": threadID}, nil); err != nil {
		return err
	}
	native.mu.Lock()
	delete(native.loadedThreads, threadID)
	delete(native.activeTurns, threadID)
	native.mu.Unlock()
	return nil
}

func (native *CodexNative) request(ctx context.Context, timeout time.Duration, method string, params, output any) error {
	client, err := native.clientForOperation(ctx)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.request(requestCtx, method, params, output)
}

func (native *CodexNative) clientSnapshot() *appServerClient {
	if native == nil {
		return nil
	}
	native.connectionMu.Lock()
	defer native.connectionMu.Unlock()
	return native.client
}

func (native *CodexNative) clientForOperation(ctx context.Context) (*appServerClient, error) {
	if native == nil {
		return nil, errors.New("codex App Server client is unavailable")
	}
	native.connectionMu.Lock()
	defer native.connectionMu.Unlock()
	if native.closed {
		return nil, errors.New("codex App Server client is closed")
	}
	if native.client != nil {
		select {
		case <-native.client.done:
		default:
			if exactProcessIdentityMatch(native.client.peerPID, native.client.peerProcStart) {
				return native.client, nil
			}
			native.client.close()
		}
	}
	client, err := openCodexNativeClient(ctx, native.config)
	if err != nil {
		return nil, err
	}
	native.client = client
	native.mu.Lock()
	native.loadedThreads = map[string]bool{}
	native.activeTurns = map[string]string{}
	for key, waiter := range native.turnWaiters {
		delete(native.turnWaiters, key)
		close(waiter)
	}
	native.mu.Unlock()
	native.eventWG.Add(1)
	go native.observe(native.eventCtx, client)
	return client, nil
}

func openCodexNativeClient(ctx context.Context, config CodexNativeConfig) (*appServerClient, error) {
	client, dialErr := dialCodexNative(ctx, config.SocketPath, 500*time.Millisecond)
	if dialErr == nil {
		return client, nil
	}
	if !errors.Is(dialErr, os.ErrNotExist) && !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("connect Codex App Server: %w", dialErr)
	}
	start := config.Start
	if start == nil {
		start = startCodexAppServer
	}
	if err := start(ctx, config.CodexBinary, []string{"app-server", "daemon", "start"}, config.Environment); err != nil {
		return nil, fmt.Errorf("start Codex App Server: %w", err)
	}
	client, dialErr = dialCodexNative(ctx, config.SocketPath, 2*time.Second)
	if dialErr != nil {
		return nil, fmt.Errorf("connect lazily started Codex App Server: %w", dialErr)
	}
	return client, nil
}

func (native *CodexNative) observe(ctx context.Context, client *appServerClient) {
	defer native.eventWG.Done()
	for {
		select {
		case notification := <-client.notifications:
			native.connectionMu.Lock()
			current := !native.closed && native.client == client
			native.connectionMu.Unlock()
			if current {
				native.observeNotification(notification)
			}
		case <-client.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (native *CodexNative) observeNotification(notification rpcNotification) {
	var params map[string]any
	if jsonUnmarshalMetadata(notification.Params, &params) != nil {
		return
	}
	event := CodexNativeEvent{Kind: notification.Method, ThreadID: stringValue(params["threadId"])}
	switch notification.Method {
	case "thread/started":
		thread, _ := params["thread"].(map[string]any)
		event.ThreadID = stringValue(thread["id"])
	case "turn/started":
		turn, _ := params["turn"].(map[string]any)
		event.TurnID = stringValue(turn["id"])
		if event.ThreadID != "" && event.TurnID != "" {
			native.mu.Lock()
			native.activeTurns[event.ThreadID] = event.TurnID
			native.mu.Unlock()
		}
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		event.TurnID = stringValue(turn["id"])
		event.Status = normalizeStatus(turn["status"])
		if event.ThreadID != "" && event.TurnID != "" {
			key := codexLaneTurnKey{threadID: event.ThreadID, turnID: event.TurnID}
			native.mu.Lock()
			if native.activeTurns[event.ThreadID] == event.TurnID {
				delete(native.activeTurns, event.ThreadID)
			}
			if waiter := native.turnWaiters[key]; waiter != nil {
				delete(native.turnWaiters, key)
				close(waiter)
			}
			native.mu.Unlock()
		}
	case "thread/status/changed":
		event.Status = statusType(params["status"])
	case "thread/name/updated":
		event.Name = stringValue(params["threadName"])
	}
	if native.config.OnEvent != nil {
		native.config.OnEvent(event)
	}
}

func exportedCodexThread(thread appThread, approvalPolicy string) CodexNativeThread {
	return CodexNativeThread{
		ID: thread.ID, Name: thread.Name, Cwd: thread.Cwd, UpdatedAt: thread.UpdatedAt, Status: statusType(thread.Status),
		ApprovalPolicy: approvalPolicy, Path: thread.Path,
	}
}

func dialCodexNative(ctx context.Context, socket string, timeout time.Duration) (*appServerClient, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return dialAppServer(dialCtx, socket)
}

func startCodexAppServer(ctx context.Context, binary string, args, environment []string) error {
	command := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary is canonical and args are fixed above.
	command.Env = environment
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	return command.Run()
}

func canonicalExecutable(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("executable path is empty")
	}
	if !filepath.IsAbs(value) {
		resolved, err := exec.LookPath(value)
		if err != nil {
			return "", err
		}
		value = resolved
	}
	canonical, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("path is not an executable regular file")
	}
	return canonical, nil
}

func jsonUnmarshalMetadata(body []byte, output any) error {
	return json.Unmarshal(body, output)
}
