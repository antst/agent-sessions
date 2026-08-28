package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const qwenDaemonLaneACPTimeout = 45 * time.Second

// qwenDaemonLaneActor is an in-process owner for one vendor Qwen ACP worker.
// It has no Agent Sessions process, control listener, or private state store.
type qwenDaemonLaneActor struct {
	mu sync.Mutex

	laneSessionID string
	cwd           string
	profile       qwenprofile.Identity
	permission    string
	worker        *grokManagedProcess
	client        *qwenACPClient
	workerPID     int
	workerStart   string
	workerStrong  string
	nativeSession string
	nativeTurn    string
	eventCursor   uint64
	events        []any
	answer        strings.Builder
	response      map[string]any
	outcome       string
	terminalErr   error
	interrupted   bool
	done          chan struct{}
	doneOnce      sync.Once
	stopping      bool
}

// StartQwenTurn starts one daemon-owned Qwen ACP actor for an accepted turn.
func (coordinator *qwenNativeCoordinator) StartQwenTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	coordinator.ensureRecovered()
	prompt := qwenDaemonLanePrompt(turn)
	if prompt == "" {
		return nil, errors.New("qwen daemon lane turn has no native prompt reference")
	}
	coordinator.mu.Lock()
	prior := coordinator.lanes[lane.LaneSessionID]
	delete(coordinator.lanes, lane.LaneSessionID)
	coordinator.mu.Unlock()
	if prior != nil {
		prior.stop()
	}
	actor, err := startQwenDaemonLaneActor(ctx, lane, turn)
	if err != nil {
		return nil, err
	}
	coordinator.mu.Lock()
	if coordinator.lanes == nil {
		coordinator.lanes = make(map[string]*qwenDaemonLaneActor)
	}
	if existing := coordinator.lanes[lane.LaneSessionID]; existing != nil {
		coordinator.mu.Unlock()
		actor.stop()
		return nil, errors.New("qwen daemon lane already has an active ACP actor")
	}
	coordinator.lanes[lane.LaneSessionID] = actor
	coordinator.mu.Unlock()
	go actor.runPrompt(prompt) //nolint:gosec // G118: an accepted durable turn intentionally outlives its initiating RPC context.
	return actor.dispatchEvidence(), nil
}

// ReconnectQwenTurn reconnects exact surviving actor evidence or records the
// documented non-reattachable interruption after a daemon replacement.
func (coordinator *qwenNativeCoordinator) ReconnectQwenTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	actor := coordinator.lanes[lane.LaneSessionID]
	coordinator.mu.Unlock()
	if actor != nil {
		return actor.reconnectEvidence(lane, turn)
	}
	sessionID := qwenDaemonLaneSessionID(lane)
	turnID := stringValue(turn.NativeTurnIdentity["native_turn_id"])
	pid, start := intValue(lane.NativeActor["worker_pid"]), stringValue(lane.NativeActor["worker_proc_start"])
	if sessionID == "" || turnID == "" || pid <= 1 || start == "" ||
		exactProcessIdentityStatus(pid, start).Status != processIdentityStale {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return map[string]any{
		"reconnectable": false, "qwen_session_id": sessionID, "native_turn_id": turnID,
		"worker_status": "absent", "worker_pid": pid, "worker_proc_start": start,
		"worker_strong_start": stringValue(lane.NativeActor["worker_strong_start"]),
		"limitation":          "qwen_acp_stdio_is_not_reattachable",
		"native_transcript": map[string]any{
			"session_id": sessionID, "resume_supported": validSessionID(sessionID),
		},
	}, nil
}

// InterruptQwenTurn cancels the exact active Qwen ACP turn.
func (coordinator *qwenNativeCoordinator) InterruptQwenTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	actor, err := coordinator.exactQwenLaneActor(lane, turn)
	if err != nil {
		return err
	}
	return actor.interrupt()
}

// CollectQwenTurn waits for and returns exact terminal ACP evidence.
func (coordinator *qwenNativeCoordinator) CollectQwenTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	actor, err := coordinator.exactQwenLaneActor(lane, turn)
	if err != nil {
		return nil, err
	}
	select {
	case <-actor.done:
		return actor.terminalEvidence()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ArchiveQwenLane invokes Qwen's native transcript archive transaction.
func (coordinator *qwenNativeCoordinator) ArchiveQwenLane(
	_ context.Context,
	lane daemonpkg.LaneRecord,
) error {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	actor := coordinator.lanes[lane.LaneSessionID]
	coordinator.mu.Unlock()
	profile, err := qwenDaemonLaneProfile(lane)
	if err != nil {
		return err
	}
	if actor != nil {
		if err := actor.matchesLane(lane); err != nil {
			return err
		}
		actor.stop()
		profile = actor.profile
	} else if err := qwenDaemonLaneWorkerAbsent(lane); err != nil {
		return err
	}
	state := qwenLaneState{
		ThreadID: lane.LaneSessionID, QwenSessionID: qwenDaemonLaneSessionID(lane),
		Cwd: lane.Cwd, Profile: profile,
	}
	return executeQwenArchiveTransaction(state, "archive")
}

// CleanupQwenLane retires only the exact daemon-owned Qwen ACP actor.
func (coordinator *qwenNativeCoordinator) CleanupQwenLane(
	_ context.Context,
	lane daemonpkg.LaneRecord,
) error {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	actor := coordinator.lanes[lane.LaneSessionID]
	if actor != nil {
		if err := actor.matchesLane(lane); err != nil {
			coordinator.mu.Unlock()
			return err
		}
		delete(coordinator.lanes, lane.LaneSessionID)
	}
	coordinator.mu.Unlock()
	if actor != nil {
		actor.stop()
		return nil
	}
	return qwenDaemonLaneWorkerAbsent(lane)
}

func (coordinator *qwenNativeCoordinator) exactQwenLaneActor(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (*qwenDaemonLaneActor, error) {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	actor := coordinator.lanes[lane.LaneSessionID]
	coordinator.mu.Unlock()
	if actor == nil || actor.matchesTurn(lane, turn) != nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return actor, nil
}

func startQwenDaemonLaneActor(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (*qwenDaemonLaneActor, error) {
	profile, err := qwenDaemonLaneProfileForTurn(lane, turn)
	if err != nil {
		return nil, err
	}
	executable, err := qwenDaemonExecutable()
	if err != nil {
		return nil, err
	}
	report, err := qwenDaemonReadinessCheck(ctx, qwenreadiness.Request{
		Executable: executable, Workspace: lane.Cwd, Profile: profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(qwenprofile.ApplyEnvironment(os.Environ(), profile)),
	})
	if err != nil {
		return nil, fmt.Errorf("check Qwen lane readiness: %w", err)
	}
	if !report.Ready {
		return nil, qwenDaemonReadinessError(report)
	}
	mode, err := qwenDaemonLaneApprovalMode(lane.PermissionMode)
	if err != nil {
		return nil, err
	}
	arguments := []string{"--acp"}
	if mode != "" {
		arguments = append(arguments, "--approval-mode", mode)
	}
	command := exec.Command(executable, arguments...) //nolint:gosec // readiness-validated executable and structured native arguments.
	command.Dir = lane.Cwd
	command.Env = qwenDaemonLaneEnvironment(profile, lane.LaneSessionID)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	command.Stderr = os.Stderr
	worker, err := startGrokManagedProcess(command, nil)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start Qwen daemon ACP worker: %w", err)
	}
	identity := procinfo.Read(worker.cmd.Process.Pid)
	if identity.Status != procinfo.Known || identity.Start != worker.procStart || identity.StrongStart == "" {
		stopGrokManagedProcess(worker, 2*time.Second)
		return nil, errors.New("capture exact Qwen daemon ACP worker identity")
	}
	client := newQwenACPClient(stdin, stdout)
	actor := &qwenDaemonLaneActor{
		laneSessionID: lane.LaneSessionID, cwd: lane.Cwd, profile: profile, permission: mode,
		worker: worker, client: client, workerPID: worker.cmd.Process.Pid,
		workerStart: identity.Start, workerStrong: identity.StrongStart,
		nativeTurn: turn.TurnID, done: make(chan struct{}),
	}
	if err := actor.initialize(ctx, lane); err != nil {
		actor.stop()
		return nil, err
	}
	client.setNotificationHandler(actor.handleNotification)
	return actor, nil
}

func (actor *qwenDaemonLaneActor) initialize(ctx context.Context, lane daemonpkg.LaneRecord) error { //nolint:gocyclo // Bootstrap validates protocol, capabilities, native identity, mode, and MCP injection together.
	requestCtx, cancel := context.WithTimeout(ctx, qwenDaemonLaneACPTimeout)
	defer cancel()
	initialized, err := actor.client.request(requestCtx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize Qwen daemon ACP worker: %w", err)
	}
	agent, capabilities := mapValue(initialized["agentInfo"]), mapValue(initialized["agentCapabilities"])
	sessions := mapValue(capabilities["sessionCapabilities"])
	mcp := mapValue(capabilities["mcpCapabilities"])
	if intValue(initialized["protocolVersion"]) != 1 || stringValue(agent["name"]) != "qwen-code" ||
		!qwenreadiness.VersionAtLeast(stringValue(agent["version"]), qwenreadiness.MinimumVersion) ||
		!boolValue(capabilities["loadSession"]) || sessions["list"] == nil || sessions["resume"] == nil || len(mcp) == 0 {
		return errors.New("qwen daemon ACP initialize response lacks the admitted identity or capabilities")
	}
	servers, err := qwenDaemonLaneMCPServers(actor.laneSessionID)
	if err != nil {
		return err
	}
	params := map[string]any{"cwd": actor.cwd, "mcpServers": servers}
	method := "session/new"
	if sessionID := qwenDaemonLaneSessionID(lane); sessionID != "" {
		method, params["sessionId"] = "session/resume", sessionID
	}
	opened, err := actor.client.request(requestCtx, method, params)
	if err != nil {
		return fmt.Errorf("open Qwen daemon ACP session: %w", err)
	}
	returned := stringValue(opened["sessionId"])
	if method == "session/new" && !validSessionID(returned) {
		return errors.New("qwen daemon ACP returned no valid native session identity")
	}
	if method == "session/resume" {
		expected := stringValue(params["sessionId"])
		if returned != "" && returned != expected {
			return daemonpkg.ErrAttachmentEvidenceChanged
		}
		returned = expected
	}
	mode := qwenACPMode(opened)
	if mode == "" || actor.permission != "" && mode != actor.permission {
		return fmt.Errorf("qwen daemon ACP mode %q does not match requested %q", mode, actor.permission)
	}
	actor.nativeSession, actor.permission = returned, mode
	return nil
}

func (actor *qwenDaemonLaneActor) runPrompt(prompt string) {
	result, err := actor.client.request(context.Background(), "session/prompt", map[string]any{
		"sessionId": actor.nativeSession,
		"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
	})
	actor.mu.Lock()
	actor.response = cloneQwenDaemonLaneMap(result)
	actor.terminalErr = err
	switch {
	case actor.interrupted:
		actor.outcome = daemonpkg.LaneDispatchInterrupted
	case err != nil:
		actor.outcome = daemonpkg.LaneDispatchFailed
	default:
		actor.outcome = daemonpkg.LaneDispatchCompleted
	}
	actor.mu.Unlock()
	actor.doneOnce.Do(func() { close(actor.done) })
}

func (actor *qwenDaemonLaneActor) handleNotification(message map[string]any) {
	if stringValue(message["method"]) != "session/update" {
		return
	}
	params := mapValue(message["params"])
	if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != actor.nativeSession {
		return
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	actor.eventCursor++
	event := cloneQwenDaemonLaneMap(message)
	eventParams := cloneQwenDaemonLaneMap(params)
	if stringValue(eventParams["turnId"]) == "" {
		eventParams["turnId"] = actor.nativeTurn
	}
	event["params"] = eventParams
	actor.events = append(actor.events, event)
	update := mapValue(params["update"])
	if stringValue(update["sessionUpdate"]) == "agent_message_chunk" {
		actor.answer.WriteString(defaultString(stringValue(mapValue(update["content"])["text"]), stringValue(update["text"])))
	}
}

func (actor *qwenDaemonLaneActor) interrupt() error {
	actor.mu.Lock()
	if actor.stopping || actor.outcome != "" {
		actor.mu.Unlock()
		return errors.New("qwen daemon lane has no active native turn")
	}
	actor.interrupted = true
	client, sessionID := actor.client, actor.nativeSession
	actor.mu.Unlock()
	return client.notifyRequest("session/cancel", map[string]any{"sessionId": sessionID})
}

func (actor *qwenDaemonLaneActor) dispatchEvidence() map[string]any {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.evidenceLocked()
}

func (actor *qwenDaemonLaneActor) reconnectEvidence(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.matchesTurnLocked(lane, turn); err != nil {
		return nil, err
	}
	result := actor.evidenceLocked()
	result["reconnectable"] = true
	if actor.outcome != "" {
		result["terminal_outcome"] = actor.outcome
		result["result_reference"] = actor.resultReferenceLocked()
	}
	return result, nil
}

func (actor *qwenDaemonLaneActor) terminalEvidence() (map[string]any, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.outcome == "" {
		return nil, errors.New("qwen daemon lane turn is not terminal")
	}
	result := actor.evidenceLocked()
	result["terminal_outcome"] = actor.outcome
	result["result_reference"] = actor.resultReferenceLocked()
	result["content"] = actor.answer.String()
	result["events"] = append([]any(nil), actor.events...)
	response := cloneQwenDaemonLaneMap(actor.response)
	if response == nil {
		response = map[string]any{}
	}
	response["sessionId"], response["turnId"] = actor.nativeSession, actor.nativeTurn
	result["response"] = response
	if actor.terminalErr != nil && actor.outcome == daemonpkg.LaneDispatchFailed {
		result["result_reference"].(map[string]any)["error_code"] = "qwen_acp_prompt_failed"
	}
	return result, nil
}

func (actor *qwenDaemonLaneActor) resultReferenceLocked() map[string]any {
	result := map[string]any{"native_session_id": actor.nativeSession}
	if content := actor.answer.String(); content != "" {
		result["content"] = content
	}
	if stop := stringValue(actor.response["stopReason"]); stop != "" {
		result["stop_reason"] = stop
	}
	return result
}

func (actor *qwenDaemonLaneActor) evidenceLocked() map[string]any {
	return map[string]any{
		"qwen_session_id": actor.nativeSession, "native_turn_id": actor.nativeTurn,
		"event_cursor": fmt.Sprintf("event-%d", actor.eventCursor),
		"worker_pid":   actor.workerPID, "worker_proc_start": actor.workerStart,
		"worker_strong_start": actor.workerStrong, "profile": actor.profile.Fingerprint,
		"cwd": actor.cwd, "permission_mode": actor.permission,
		"qwen_home_set": actor.profile.QwenHomeSet, "qwen_home": actor.profile.QwenHome,
		"qwen_runtime_dir_set": actor.profile.QwenRuntimeSet, "qwen_runtime_dir": actor.profile.QwenRuntimeDir,
	}
}

func (actor *qwenDaemonLaneActor) matchesLane(lane daemonpkg.LaneRecord) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.matchesLaneLocked(lane)
}

func (actor *qwenDaemonLaneActor) matchesTurn(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.matchesTurnLocked(lane, turn)
}

func (actor *qwenDaemonLaneActor) matchesTurnLocked(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) error {
	if err := actor.matchesLaneLocked(lane); err != nil {
		return err
	}
	if stringValue(turn.NativeTurnIdentity["native_turn_id"]) != actor.nativeTurn ||
		stringValue(turn.NativeTurnIdentity["qwen_session_id"]) != actor.nativeSession {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return nil
}

func (actor *qwenDaemonLaneActor) matchesLaneLocked(lane daemonpkg.LaneRecord) error {
	if lane.LaneSessionID != actor.laneSessionID || lane.Cwd != actor.cwd ||
		qwenDaemonLaneSessionID(lane) != actor.nativeSession ||
		!matchesQwenDaemonOptionalInt(lane.NativeActor["worker_pid"], actor.workerPID) ||
		!matchesQwenDaemonOptionalString(lane.NativeActor["worker_proc_start"], actor.workerStart) ||
		!matchesQwenDaemonOptionalString(lane.NativeActor["worker_strong_start"], actor.workerStrong) {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return nil
}

func (actor *qwenDaemonLaneActor) stop() {
	actor.mu.Lock()
	if actor.stopping {
		actor.mu.Unlock()
		return
	}
	actor.stopping = true
	client, worker, active := actor.client, actor.worker, actor.outcome == ""
	if active {
		actor.interrupted = true
	}
	actor.mu.Unlock()
	if active && client != nil {
		_ = client.notifyRequest("session/cancel", map[string]any{"sessionId": actor.nativeSession})
	}
	if client != nil {
		_ = client.close()
	}
	stopGrokManagedProcess(worker, 3*time.Second)
	actor.mu.Lock()
	if actor.outcome == "" {
		actor.outcome = daemonpkg.LaneDispatchInterrupted
	}
	actor.mu.Unlock()
	actor.doneOnce.Do(func() { close(actor.done) })
}

func qwenDaemonLanePrompt(turn daemonpkg.LaneTurnRecord) string {
	return strings.TrimSpace(defaultString(stringValue(turn.InputReference["prompt"]), stringValue(turn.InputReference["content"])))
}

func qwenDaemonLaneApprovalMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "", "default":
		return "default", nil
	case "bypassPermissions":
		return "yolo", nil
	case "yolo", "plan", "auto", "accept_edits":
		return strings.TrimSpace(mode), nil
	default:
		return "", fmt.Errorf("unsupported Qwen approval mode %q", mode)
	}
}

func qwenDaemonLaneProfile(lane daemonpkg.LaneRecord) (qwenprofile.Identity, error) {
	if stringValue(lane.NativeActor["profile"]) != "" {
		return qwenDaemonProfile(lane.NativeActor)
	}
	return qwenprofile.Current()
}

func qwenDaemonLaneProfileForTurn(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) (qwenprofile.Identity, error) {
	native := daemonLaneNativeOptions(turn)
	if stringValue(native["profile"]) != "" {
		return qwenDaemonProfile(native)
	}
	return qwenDaemonLaneProfile(lane)
}

func qwenDaemonLaneEnvironment(profile qwenprofile.Identity, laneSessionID string) []string {
	blocked := map[string]bool{
		peerSessionIDEnvironment: true, "AGENT_SESSIONS_PRODUCT": true,
		"AGENT_SESSIONS_QWEN_CAPABILITY": true, qwenLaneLaunchTokenEnv: true,
	}
	filtered := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		if !blocked[name] {
			filtered = append(filtered, entry)
		}
	}
	filtered = qwenprofile.ApplyEnvironment(filtered, profile)
	return append(filtered, peerSessionIDEnvironment+"="+laneSessionID, "AGENT_SESSIONS_PRODUCT=qwen")
}

func qwenDaemonLaneMCPServers(laneSessionID string) ([]any, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Qwen daemon MCP relay: %w", err)
	}
	return []any{map[string]any{
		"name": "agent_sessions", "command": executable,
		"args": []any{"connector", "qwen", "mcp"},
		"env": []any{
			map[string]any{"name": peerSessionIDEnvironment, "value": laneSessionID},
			map[string]any{"name": "AGENT_SESSIONS_PRODUCT", "value": "qwen"},
		},
	}}, nil
}

func matchesQwenDaemonOptionalInt(expected any, actual int) bool {
	return expected == nil || intValue(expected) == actual
}

func matchesQwenDaemonOptionalString(expected any, actual string) bool {
	return expected == nil || stringValue(expected) == actual
}

func qwenDaemonLaneWorkerAbsent(lane daemonpkg.LaneRecord) error {
	pid, start := intValue(lane.NativeActor["worker_pid"]), stringValue(lane.NativeActor["worker_proc_start"])
	if pid <= 1 && start == "" {
		return nil
	}
	if pid <= 1 || start == "" || exactProcessIdentityStatus(pid, start).Status != processIdentityStale {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return nil
}
