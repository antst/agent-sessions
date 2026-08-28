package bridge

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/socketpath"
)

type grokLaneManager struct {
	mu                sync.Mutex
	noticeMu          sync.Mutex
	paths             nativePaths
	state             grokLaneState
	launchToken       string
	hostPaths         grokHostPaths
	listener          net.Listener
	controlWG         sync.WaitGroup
	controlClosed     bool
	lifecycleLock     *os.File
	launchLease       *os.File
	diagnostics       *grokDiagnosticSink
	toolShellPath     string
	toolRealShell     string
	worker            *grokManagedProcess
	client            *grokACPClient
	peer              *daemon
	activeAnswer      strings.Builder
	activeTurnID      string
	interruptedID     string
	turnNotify        chan struct{}
	done              chan struct{}
	startupDone       chan struct{}
	closing           bool
	shutdownReason    string
	shutdownInterrupt bool
	startupPhase      string
	cleanupOnce       sync.Once
	persistOverride   func(grokLaneState) error
}

// grokDaemonLaneActor is the in-process successor to grokLaneManager. It owns
// one vendor Grok ACP worker and its anonymous stdio channel, but no Agent
// Sessions listener, manager process, peer daemon, or durable catalog.
type grokDaemonLaneActor struct {
	mu sync.Mutex

	laneSessionID string
	cwd           string
	permission    string
	model         string
	reasoning     string
	profile       string
	grokBin       string
	actorRoot     string

	diagnostics *grokDiagnosticSink
	worker      *grokManagedProcess
	client      *grokACPClient

	sessionID         string
	workerPID         int
	workerProcStart   string
	workerStrongStart string
	activeTurnID      string
	activeAnswer      strings.Builder
	interrupted       map[string]bool
	dispatches        map[string]map[string]any
	terminals         map[string]map[string]any
	terminalReady     map[string]chan struct{}
	closed            bool
}

func newGrokDaemonLaneActor(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) (*grokDaemonLaneActor, error) {
	if lane.Product != "grok" || strings.TrimSpace(lane.LaneSessionID) == "" ||
		!filepath.IsAbs(lane.Cwd) || lane.PermissionMode != "bypassPermissions" {
		return nil, errors.New("invalid daemon-owned Grok lane")
	}
	profile, err := canonicalGrokProfile("")
	if err != nil {
		return nil, err
	}
	grokBin, err := grokDaemonExecutable()
	if err != nil {
		return nil, err
	}
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return nil, err
	}
	actorRoot := filepath.Join(paths.RuntimeRoot, "grok-lanes", safeID(lane.LaneSessionID))
	if !filepath.IsAbs(actorRoot) || safeID(lane.LaneSessionID) == "" {
		return nil, errors.New("invalid daemon-owned Grok lane runtime root")
	}
	native := daemonLaneNativeOptions(turn)
	return &grokDaemonLaneActor{
		laneSessionID: lane.LaneSessionID, cwd: lane.Cwd, permission: lane.PermissionMode,
		model: stringValue(native["model"]), reasoning: stringValue(native["reasoning_effort"]),
		profile: profile, grokBin: grokBin, actorRoot: actorRoot,
		interrupted: make(map[string]bool), dispatches: make(map[string]map[string]any),
		terminals: make(map[string]map[string]any), terminalReady: make(map[string]chan struct{}),
	}, nil
}

func (actor *grokDaemonLaneActor) startTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	if turn.LaneSessionID != lane.LaneSessionID || strings.TrimSpace(turn.TurnID) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.verifyLocked(lane); err != nil {
		return nil, err
	}
	if prior := actor.dispatches[turn.TurnID]; prior != nil {
		return cloneGrokLaneEvidence(prior), nil
	}
	if actor.activeTurnID != "" {
		return nil, errors.New("grok daemon lane already has an active turn")
	}
	if err := actor.ensureStartedLocked(ctx, lane); err != nil {
		return nil, err
	}
	dispatch := actor.dispatchEvidenceLocked(turn.TurnID)
	actor.dispatches[turn.TurnID] = dispatch
	actor.terminalReady[turn.TurnID] = make(chan struct{})
	if stringValue(turn.InputReference["kind"]) == "peer_message" {
		messageID := strings.TrimSpace(stringValue(turn.InputReference["message_id"]))
		message := strings.TrimSpace(stringValue(turn.InputReference["content"]))
		if messageID == "" || message == "" {
			delete(actor.dispatches, turn.TurnID)
			return nil, errors.New("grok lane interjection requires message identity and content")
		}
		if err := actor.client.requestInterjection(ctx, actor.sessionID, messageID, message); err != nil {
			delete(actor.dispatches, turn.TurnID)
			return nil, err
		}
		actor.setTerminalLocked(turn.TurnID, map[string]any{
			"session_id": actor.sessionID, "native_turn_id": messageID,
			"terminal_outcome": "completed", "result_reference": map[string]any{
				"kind": "native_interjection", "message_id": messageID,
			},
		})
		dispatch["native_turn_id"] = messageID
		actor.dispatches[turn.TurnID] = dispatch
		return cloneGrokLaneEvidence(dispatch), nil
	}
	prompt := strings.TrimSpace(defaultString(stringValue(turn.InputReference["content"]), stringValue(turn.InputReference["prompt"])))
	if prompt == "" {
		delete(actor.dispatches, turn.TurnID)
		return nil, errors.New("grok lane turn requires prompt content")
	}
	actor.activeTurnID = turn.TurnID
	actor.activeAnswer.Reset()
	// An accepted turn outlives the request that dispatched it while retaining
	// request-scoped values needed by the native coordinator.
	executionContext := context.WithoutCancel(ctx)
	go actor.executePrompt(executionContext, actor.client, actor.sessionID, turn.TurnID, prompt)
	return cloneGrokLaneEvidence(dispatch), nil
}

func (actor *grokDaemonLaneActor) ensureStartedLocked(ctx context.Context, lane daemonpkg.LaneRecord) error {
	if actor.client != nil {
		return nil
	}
	if err := os.MkdirAll(actor.actorRoot, 0o700); err != nil {
		return err
	}
	diagnostics, err := newGrokDiagnosticSink(filepath.Join(actor.actorRoot, "diagnostics.log"))
	if err != nil {
		return err
	}
	actor.diagnostics = diagnostics
	processDiagnostics := diagnostics.process("daemon-owned Grok lane ACP worker")
	args := []string{"--no-auto-update"}
	if actor.model != "" {
		args = append(args, "--model", actor.model)
	}
	if actor.reasoning != "" {
		args = append(args, "--reasoning-effort", actor.reasoning)
	}
	args = append(args, "agent", "--no-leader", "--always-approve", "stdio")
	command := exec.Command(actor.grokBin, args...) //nolint:gosec // validated Grok executable and fixed lane argv.
	command.Dir = actor.cwd
	command.Env = envutil.Set(grokLaneManagerEnvironment(os.Environ(), "", actor.laneSessionID), "HOME", actor.profile)
	command.Env = envutil.Set(command.Env, "AGENT_SESSIONS_PRODUCT", "grok")
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = processDiagnostics
	worker, err := startGrokManagedProcess(command, processDiagnostics)
	if err != nil {
		return (&grokManagedProcess{diagnostics: processDiagnostics}).attributedError("start daemon-owned Grok lane ACP worker", err)
	}
	identity := procinfo.Read(worker.cmd.Process.Pid)
	if identity.Status != procinfo.Known || identity.Start != worker.procStart || identity.StrongStart == "" {
		stopGrokManagedProcess(worker, 2*time.Second)
		return errors.New("capture strong daemon-owned Grok lane worker identity")
	}
	actor.worker, actor.workerPID = worker, worker.cmd.Process.Pid
	actor.workerProcStart, actor.workerStrongStart = worker.procStart, identity.StrongStart
	actor.client = newGrokACPClient(worker, stdin, stdout, actor.laneSessionID, 0, nil)
	if err := actor.initializeACPLocked(ctx, lane); err != nil {
		actor.closeClientLocked()
		return err
	}
	actor.client.setNotificationHandler(actor.handleNotification)
	return nil
}

func (actor *grokDaemonLaneActor) initializeACPLocked(ctx context.Context, lane daemonpkg.LaneRecord) error {
	sessionID, err := initializeGrokLaneACP(ctx, actor.client, actor.cwd, stringValue(lane.NativeActor["session_id"]))
	if err != nil {
		return err
	}
	actor.sessionID = sessionID
	return nil
}

func (actor *grokDaemonLaneActor) executePrompt(ctx context.Context, client *grokACPClient, sessionID, turnID, prompt string) {
	result, requestErr := client.request(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
	})
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.closed || actor.activeTurnID != turnID {
		return
	}
	outcome := "completed"
	reference := map[string]any{"kind": "native_result", "text": actor.activeAnswer.String()}
	if stopReason := stringValue(result["stopReason"]); stopReason != "" {
		reference["stop_reason"] = stopReason
	}
	switch {
	case actor.interrupted[turnID]:
		outcome = "interrupted"
	case requestErr != nil:
		outcome = "failed"
		reference = map[string]any{"kind": "native_error", "error": "Grok ACP prompt failed"}
	}
	actor.setTerminalLocked(turnID, map[string]any{
		"session_id": actor.sessionID, "native_turn_id": turnID,
		"terminal_outcome": outcome, "result_reference": reference,
	})
	actor.activeTurnID = ""
	actor.activeAnswer.Reset()
}

func (actor *grokDaemonLaneActor) handleNotification(message map[string]any) {
	method := stringValue(message["method"])
	if method != "session/update" && method != "x.ai/session/update" && method != "_x.ai/session/update" {
		return
	}
	params, _ := message["params"].(map[string]any)
	if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != actor.sessionID {
		return
	}
	update, _ := params["update"].(map[string]any)
	if update == nil {
		update, _ = params["sessionUpdate"].(map[string]any)
	}
	if stringValue(update["sessionUpdate"]) != "agent_message_chunk" {
		return
	}
	content, _ := update["content"].(map[string]any)
	text := defaultString(stringValue(content["text"]), stringValue(update["text"]))
	if text == "" {
		return
	}
	actor.mu.Lock()
	if actor.activeTurnID != "" {
		actor.activeAnswer.WriteString(text)
	}
	actor.mu.Unlock()
}

func (actor *grokDaemonLaneActor) reconnectTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.verifyLocked(lane); err != nil {
		return nil, err
	}
	if actor.activeTurnID != turn.TurnID || stringValue(turn.NativeTurnIdentity["native_turn_id"]) != turn.TurnID {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	result := actor.dispatchEvidenceLocked(turn.TurnID)
	result["reconnectable"] = true
	return result, nil
}

func (actor *grokDaemonLaneActor) interruptTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.verifyLocked(lane); err != nil {
		return err
	}
	if actor.activeTurnID != turn.TurnID ||
		stringValue(turn.NativeTurnIdentity["native_turn_id"]) != turn.TurnID || actor.client == nil {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	if err := actor.client.notifyRequest(map[string]any{"sessionId": actor.sessionID}); err != nil {
		return err
	}
	actor.interrupted[turn.TurnID] = true
	return nil
}

func (actor *grokDaemonLaneActor) collectTurn(
	_ context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.verifyLocked(lane); err != nil {
		return nil, err
	}
	if stringValue(turn.NativeTurnIdentity["native_turn_id"]) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	terminal := actor.terminals[turn.TurnID]
	if terminal == nil {
		return nil, daemonpkg.ErrLaneNotTerminal
	}
	return cloneGrokLaneEvidence(terminal), nil
}

func (actor *grokDaemonLaneActor) waitTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	actor.mu.Lock()
	if err := actor.verifyLocked(lane); err != nil {
		actor.mu.Unlock()
		return nil, err
	}
	if stringValue(turn.NativeTurnIdentity["native_turn_id"]) == "" {
		actor.mu.Unlock()
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if terminal := actor.terminals[turn.TurnID]; terminal != nil {
		result := cloneGrokLaneEvidence(terminal)
		actor.mu.Unlock()
		return result, nil
	}
	ready := actor.terminalReady[turn.TurnID]
	if ready == nil {
		actor.mu.Unlock()
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	actor.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ready:
	}

	actor.mu.Lock()
	defer actor.mu.Unlock()
	if terminal := actor.terminals[turn.TurnID]; terminal != nil {
		return cloneGrokLaneEvidence(terminal), nil
	}
	return nil, daemonpkg.ErrAttachmentEvidenceChanged
}

func (actor *grokDaemonLaneActor) setTerminalLocked(turnID string, terminal map[string]any) {
	actor.terminals[turnID] = terminal
	if ready := actor.terminalReady[turnID]; ready != nil {
		close(ready)
		delete(actor.terminalReady, turnID)
	}
}

func (actor *grokDaemonLaneActor) verify(_ context.Context, lane daemonpkg.LaneRecord) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.verifyLocked(lane)
}

func (actor *grokDaemonLaneActor) verifyLocked(lane daemonpkg.LaneRecord) error {
	if actor.closed || lane.LaneSessionID != actor.laneSessionID || lane.Cwd != actor.cwd ||
		lane.PermissionMode != actor.permission {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	// A newly constructed actor is allowed to replace the prior worker only as
	// part of an accepted resume, while loading the exact durable session ID.
	// Once its ACP channel exists every subsequent operation requires exact
	// worker identity below.
	if actor.client == nil {
		return nil
	}
	if expected := stringValue(lane.NativeActor["session_id"]); expected != "" && expected != actor.sessionID {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	if expected := lane.NativeActor["worker_pid"]; expected != nil && intValue(expected) != actor.workerPID {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	for key, actual := range map[string]string{
		"worker_proc_start": actor.workerProcStart, "worker_strong_start": actor.workerStrongStart,
	} {
		if expected := stringValue(lane.NativeActor[key]); expected != "" && expected != actual {
			return daemonpkg.ErrAttachmentEvidenceChanged
		}
	}
	return nil
}

func (actor *grokDaemonLaneActor) dispatchEvidenceLocked(turnID string) map[string]any {
	return map[string]any{
		"session_id": actor.sessionID, "native_turn_id": turnID,
		"worker_pid": actor.workerPID, "worker_proc_start": actor.workerProcStart,
		"worker_strong_start": actor.workerStrongStart,
	}
}

func (actor *grokDaemonLaneActor) unstarted() bool {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.client == nil && actor.activeTurnID == ""
}

func (actor *grokDaemonLaneActor) close() {
	actor.mu.Lock()
	if actor.closed {
		actor.mu.Unlock()
		return
	}
	actor.closed = true
	for turnID, ready := range actor.terminalReady {
		close(ready)
		delete(actor.terminalReady, turnID)
	}
	if actor.client != nil && actor.activeTurnID != "" {
		_ = actor.client.notifyRequest(map[string]any{"sessionId": actor.sessionID})
	}
	actor.closeClientLocked()
	if actor.diagnostics != nil {
		_ = actor.diagnostics.close()
	}
	_ = os.Remove(filepath.Join(actor.actorRoot, "diagnostics.log"))
	_ = os.Remove(actor.actorRoot)
	actor.mu.Unlock()
}

func (actor *grokDaemonLaneActor) closeClientLocked() {
	if actor.client != nil {
		actor.client.close()
		actor.client = nil
	} else {
		stopGrokManagedProcess(actor.worker, 2*time.Second)
	}
	actor.worker = nil
}

func inspectUnattachableGrokLaneTurn(
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (map[string]any, error) {
	sessionID := stringValue(lane.NativeActor["session_id"])
	nativeTurnID := stringValue(turn.NativeTurnIdentity["native_turn_id"])
	pid := intValue(lane.NativeActor["worker_pid"])
	procStart := stringValue(lane.NativeActor["worker_proc_start"])
	strongStart := stringValue(lane.NativeActor["worker_strong_start"])
	if sessionID == "" || nativeTurnID == "" || pid <= 1 || procStart == "" || strongStart == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	identity := procinfo.Read(pid)
	workerStatus := "absent"
	switch identity.Status {
	case procinfo.Known:
		if identity.Start != procStart || identity.StrongStart != strongStart {
			return nil, daemonpkg.ErrAttachmentEvidenceChanged
		}
		workerStatus = "live_unattachable"
	case procinfo.Absent:
		// Exact absence is sufficient native evidence for the bounded restart outcome.
	case procinfo.Unknown:
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return map[string]any{
		"reconnectable": false, "session_id": sessionID, "native_turn_id": nativeTurnID,
		"worker_status": workerStatus, "worker_pid": pid, "worker_proc_start": procStart,
		"worker_strong_start": strongStart, "limitation": "grok_acp_stdio_is_not_reattachable",
		"native_transcript": map[string]any{"session_id": sessionID, "resume_supported": true},
	}, nil
}

func (m *grokLaneManager) closingState() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closing
}

func (m *grokLaneManager) setStartupPhase(phase string) {
	m.mu.Lock()
	m.startupPhase = phase
	m.mu.Unlock()
}

// beginShutdown records the first accepted shutdown cause before waiting for
// startup. The startup goroutine can then report an error and race cleanup
// without replacing a signal/archive terminal disposition with a generic
// infrastructure failure.
func (m *grokLaneManager) beginShutdown(reason string, interrupt bool) <-chan struct{} {
	m.mu.Lock()
	startupDone := m.beginShutdownLocked(reason, interrupt)
	m.mu.Unlock()
	return startupDone
}

func (m *grokLaneManager) beginShutdownLocked(reason string, interrupt bool) <-chan struct{} {
	if m.shutdownReason == "" {
		m.shutdownReason, m.shutdownInterrupt = reason, interrupt
	}
	m.closing = true
	return m.startupDone
}

func (m *grokLaneManager) recordedShutdown(fallbackReason string, fallbackInterrupt bool) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdownReason == "" {
		m.shutdownReason, m.shutdownInterrupt = fallbackReason, fallbackInterrupt
	}
	return m.shutdownReason, m.shutdownInterrupt
}

func (m *grokLaneManager) closingError(message string) error {
	m.mu.Lock()
	reason := m.shutdownReason
	m.mu.Unlock()
	if reason == "" {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %s", message, reason)
}

func (m *grokLaneManager) logShutdownTrigger(trigger string) {
	m.mu.Lock()
	phase, startupID, sessionID := m.startupPhase, m.state.StartupID, m.state.SessionID
	m.mu.Unlock()
	if phase == "" {
		phase = "unknown"
	}
	fmt.Fprintf(os.Stderr, "grok-lane-manager: shutdown trigger %s pid=%d session=%s startup=%s phase=%s\n",
		trigger, os.Getpid(), sessionID, startupID, phase)
}

//nolint:gocyclo // Startup is one ownership transaction: state, socket, launch record, ACP, MCP, and publication.
func (m *grokLaneManager) start() error {
	if m.startupDone != nil {
		defer close(m.startupDone)
	}
	if !validGrokLaunchToken(m.launchToken) || subtle.ConstantTimeCompare([]byte(m.state.LaunchTokenHash), []byte(grokTokenHash(m.launchToken))) != 1 {
		return errors.New("grok lane launch capability is unavailable")
	}
	m.setStartupPhase("lifecycle")
	lifecycle, err := lockLaneLifecycle(m.paths, "grok-"+m.state.SessionID)
	if err != nil {
		return err
	}
	m.lifecycleLock = lifecycle
	latest, err := readGrokLaneState(m.paths, m.state.SessionID)
	if err != nil || latest.Status != "starting" {
		return errors.New("refuse to start Grok lane manager outside starting state")
	}
	m.state = latest
	m.hostPaths = grokRuntimePaths(m.state.RuntimeDir, os.Getuid(), m.launchToken)
	if !samePath(m.state.ControlSocket, m.hostPaths.ControlSocket) {
		return errors.New("grok lane control path does not match its launch capability")
	}
	if err := ensurePrivateRuntimeDir(m.hostPaths.Root); err != nil {
		return err
	}
	if err := os.Mkdir(m.hostPaths.LaunchDir, 0o700); err != nil {
		return fmt.Errorf("create private Grok lane directory: %w", err)
	}
	diagnostics, err := newGrokDiagnosticSink(filepath.Join(m.hostPaths.LaunchDir, "diagnostics.log"))
	if err != nil {
		return err
	}
	m.diagnostics = diagnostics
	if err := m.prepareToolRegistry(); err != nil {
		return err
	}
	if err := socketpath.Validate(m.state.ControlSocket); err != nil {
		return fmt.Errorf("validate Grok lane control socket: %w", err)
	}
	_ = os.Remove(m.state.ControlSocket)
	listener, err := net.Listen("unix", m.state.ControlSocket)
	if err != nil {
		return fmt.Errorf("listen on Grok lane control socket: %w", err)
	}
	if err := os.Chmod(m.state.ControlSocket, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("secure Grok lane control socket: %w", err)
	}
	managerProcStart, err := captureProcessStart(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture Grok lane manager identity: %w", err)
	}
	managerInfo := procinfo.Read(os.Getpid())
	if managerInfo.Status != procinfo.Known || managerInfo.Start != managerProcStart || managerInfo.StrongStart == "" {
		return errors.New("capture strong Grok lane manager identity")
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		_ = listener.Close()
		return m.closingError("grok lane manager is closing during startup")
	}
	m.listener = listener
	m.state.ManagerPID, m.state.ManagerProcStart, m.state.ManagerStrongStart = os.Getpid(), managerProcStart, managerInfo.StrongStart
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	go m.acceptLoop(listener)
	m.setStartupPhase("worker")
	lease, err := acquireGrokLaunchLease(m.paths, m.state.SessionID)
	if err != nil {
		return err
	}
	m.launchLease = lease
	record := grokLaunchRecord{
		SessionID: m.state.SessionID, Cwd: m.state.Cwd, Name: m.state.Name,
		PermissionMode: m.state.PermissionMode, TokenHash: m.state.LaunchTokenHash,
		OwnerPID: os.Getpid(), OwnerProcStart: m.state.ManagerProcStart,
		HostPID: os.Getpid(), HostProcStart: m.state.ManagerProcStart,
		RuntimeDir: m.state.RuntimeDir, LeaderSocket: m.hostPaths.LeaderSocket,
		ControlSocket: m.hostPaths.ControlSocket, StartedAt: time.Now().UnixMilli(),
	}
	if err := claimGrokLaunchRecord(m.paths, record); err != nil {
		return fmt.Errorf("persist Grok lane ownership: %w", err)
	}
	if err := m.startWorker(&record); err != nil {
		return err
	}
	if err := writeJSONAtomic(grokLaunchRecordPath(m.paths, m.state.SessionID), record); err != nil {
		return fmt.Errorf("persist Grok lane worker ownership: %w", err)
	}
	m.setStartupPhase("acp")
	ctx, cancel := context.WithTimeout(context.Background(), grokACPStartupTimeout)
	err = m.initializeACP(ctx)
	cancel()
	if err != nil {
		return err
	}
	m.setStartupPhase("mcp")
	ctx, cancel = context.WithTimeout(context.Background(), grokLaneMCPReadyTimeout)
	err = m.waitForAgentSessionsMCP(ctx)
	cancel()
	if err != nil {
		return err
	}
	if m.closingState() {
		return m.closingError("grok lane manager is closing before publication")
	}
	m.mu.Lock()
	persistent, ownerPID, ownerProcStart := m.state.Persistent, m.state.OwnerPID, m.state.OwnerProcStart
	m.mu.Unlock()
	if !persistent && !exactProcessIdentityMatch(ownerPID, ownerProcStart) {
		return errors.New("grok lane lifecycle owner exited during startup")
	}
	peerOptions := map[string]string{
		"session-id": m.state.SessionID, "cwd": m.state.Cwd, "name": m.state.Name,
		"name-source": "lane", "entrypoint": "grok", "permission-mode": m.state.PermissionMode,
		"status": "idle", "supervisor-socket": m.state.ControlSocket, "supervisor-token": m.launchToken,
		"owner-pid": fmt.Sprintf("%d", os.Getpid()), "owner-proc-start": m.state.ManagerProcStart,
		"data-dir": m.paths.dataRoot, "claude-config-dir": m.paths.claudeRoot,
		"codex-home": m.paths.codexHome, "runtime-dir": m.paths.runtimeDir,
	}
	if laneAgentConfigured() {
		peerOptions["agent-runtime-dir"] = laneAgentRuntimeDir()
	}
	m.setStartupPhase("publication")
	peer := newDaemon(peerOptions)
	if err := peer.start(); err != nil {
		return fmt.Errorf("publish Grok lane peer: %w", err)
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		peer.shutdown()
		return m.closingError("grok lane manager is closing before publication")
	}
	m.peer = peer
	m.state.MessagingSocket = peer.stableSocket
	m.state.Status, m.state.StartupID = "idle", ""
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		m.mu.Lock()
		m.peer = nil
		m.mu.Unlock()
		peer.shutdown()
		return err
	}
	m.setStartupPhase("published")
	go m.turnLoop()
	go m.maintenanceLoop()
	m.signalTurn()
	return nil
}

//nolint:gocyclo // Worker startup keeps process, ledger, ACP, and publication state in one fail-closed transaction.
func (m *grokLaneManager) startWorker(record *grokLaunchRecord) error {
	grokBin := strings.TrimSpace(os.Getenv("GROK_PEER_GROK_BIN"))
	if grokBin == "" {
		return errors.New("validated Grok Build executable is unavailable")
	}
	args := []string{"--no-auto-update", "agent", "--no-leader", "--always-approve"}
	if m.state.Model != "" {
		args = append(args, "--model", m.state.Model)
	}
	if m.state.ReasoningEffort != "" {
		args = append(args, "--reasoning-effort", m.state.ReasoningEffort)
	}
	args = append(args, "stdio")
	command := exec.Command(grokBin, args...) //nolint:gosec // launcher-selected Grok Build executable and validated structured options.
	command.Dir = m.state.Cwd
	command.Env = grokLaneWorkerEnvironment(os.Environ(), m.launchToken, m.state, m.toolShellPath, m.toolRealShell)
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	workerDiagnostics := m.diagnostics.process("headless Grok lane ACP worker")
	command.Stderr = workerDiagnostics
	worker, err := startGrokManagedProcess(command, workerDiagnostics)
	if err != nil {
		return (&grokManagedProcess{diagnostics: workerDiagnostics}).attributedError("start headless Grok lane ACP worker", err)
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		stopGrokManagedProcess(worker, 2*time.Second)
		return m.closingError("grok lane manager is closing during worker startup")
	}
	m.worker = worker
	m.state.WorkerPID, m.state.WorkerProcStart = worker.cmd.Process.Pid, worker.procStart
	workerInfo := procinfo.Read(worker.cmd.Process.Pid)
	if workerInfo.Status != procinfo.Known || workerInfo.Start != worker.procStart || workerInfo.StrongStart == "" {
		m.mu.Unlock()
		stopGrokManagedProcess(worker, 2*time.Second)
		return errors.New("capture strong headless Grok lane worker identity")
	}
	m.state.WorkerStrongStart = workerInfo.StrongStart
	workerSessionID, sessionErr := grokProcessSessionID(worker.cmd.Process.Pid)
	if sessionErr != nil || workerSessionID != worker.cmd.Process.Pid {
		m.mu.Unlock()
		stopGrokManagedProcess(worker, 2*time.Second)
		return errors.New("headless Grok lane worker did not establish an isolated process session")
	}
	m.state.WorkerSessionID = workerSessionID
	record.LeaderPID, record.LeaderProcStart = m.state.WorkerPID, m.state.WorkerProcStart
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		stopGrokManagedProcess(worker, 2*time.Second)
		return err
	}
	if err := m.prepareSharedToolRootLedger(); err != nil {
		stopGrokManagedProcess(worker, 2*time.Second)
		return err
	}
	client := newGrokACPClient(worker, stdin, stdout, m.state.SessionID, 0, nil)
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		client.close()
		return m.closingError("grok lane manager is closing during ACP startup")
	}
	m.client = client
	m.mu.Unlock()
	return nil
}

func (m *grokLaneManager) initializeACP(ctx context.Context) error {
	m.mu.Lock()
	client, worker := m.client, m.worker
	cwd, expectedSessionID := m.state.Cwd, ""
	if m.state.SessionCreated {
		expectedSessionID = m.state.GrokSessionID
	}
	m.mu.Unlock()
	if client == nil || worker == nil {
		return errors.New("headless Grok lane ACP worker is unavailable")
	}
	returned, err := initializeGrokLaneACP(ctx, client, cwd, expectedSessionID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.state.GrokSessionID, m.state.SessionCreated = returned, true
	err = m.persistLocked()
	m.mu.Unlock()
	if err == nil {
		client.setNotificationHandler(m.handleACPNotification)
	}
	return err
}

// initializeGrokLaneACP is the one native create/load transaction shared by
// the legacy migration manager and the daemon-owned actor. The successor path
// therefore preserves Grok authentication, MCP injection, and exact native
// session validation without carrying the manager listener or catalog.
func initializeGrokLaneACP(ctx context.Context, client *grokACPClient, cwd, expectedSessionID string) (string, error) {
	result, err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return "", grokLanePrivateACPError("initialize headless Grok lane ACP worker", err)
	}
	if !grokAuthMethodAdvertised(result, "cached_token") {
		return "", errors.New("authenticate headless Grok lane ACP worker: cached_token authentication was not advertised")
	}
	if _, err := client.request(ctx, "authenticate", map[string]any{"methodId": "cached_token", "_meta": map[string]any{"headless": true}}); err != nil {
		return "", grokLanePrivateACPError("authenticate headless Grok lane ACP worker", err)
	}
	mcpServer, err := nativeRuntimeAgentSessionsMCPServer("grok-mcp", nil)
	if err != nil {
		return "", err
	}
	params := map[string]any{
		"cwd": cwd, "mcpServers": []any{mcpServer}, "_meta": map[string]any{"yoloMode": true},
	}
	method := "session/new"
	if strings.TrimSpace(expectedSessionID) != "" {
		method, params["sessionId"] = "session/load", expectedSessionID
	}
	created, err := client.request(ctx, method, params)
	if err != nil {
		return "", grokLanePrivateACPError("open headless Grok lane session", err)
	}
	returned := stringValue(created["sessionId"])
	if method == "session/new" && returned == "" {
		return "", errors.New("grok ACP returned no native session identity")
	}
	if method == "session/load" && returned != "" && returned != expectedSessionID {
		return "", daemonpkg.ErrAttachmentEvidenceChanged
	}
	return defaultString(returned, expectedSessionID), nil
}

// grokLanePrivateACPError preserves a bounded protocol cause for the lane
// manager's private 0600 log. The public launcher reports only that the manager
// failed startup, while shutdown separately stops and joins the still-live ACP
// worker; treating a request rejection as a process-exit error would mask the
// useful cause behind a spurious "managed process join incomplete" message.
func grokLanePrivateACPError(role string, err error) error {
	return fmt.Errorf("%s: %w", role, grokMCPReadinessDiagnostic(err))
}

func (m *grokLaneManager) waitForAgentSessionsMCP(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	failures := grokMCPReadinessFailures{}
	for {
		m.mu.Lock()
		client, worker := m.client, m.worker
		grokSessionID, bridgeSessionID := m.state.GrokSessionID, m.state.SessionID
		m.mu.Unlock()
		if client == nil || worker == nil || grokSessionID == "" {
			return errors.New("headless Grok lane ACP session is unavailable")
		}
		roster, rosterErr := client.request(ctx, "_x.ai/sessions/list", map[string]any{})
		switch {
		case rosterErr != nil:
			failures.record(rosterErr)
		default:
			state, stateErr := grokRosterStateFromResponse(roster, grokSessionID)
			switch {
			case stateErr != nil:
				failures.record(stateErr)
			case state.permissionMode != "bypassPermissions":
				failures.record(errors.New("grok session is not in bypassPermissions mode"))
			default:
				result, callErr := client.request(ctx, "_x.ai/mcp/call", map[string]any{
					"sessionId": grokSessionID, "server": "agent_sessions", "tool": "identity",
					"arguments": map[string]any{"session_id": bridgeSessionID},
				})
				var readyErr error
				if callErr == nil {
					readyErr = grokAgentSessionsMCPIdentityReady(result, bridgeSessionID)
				}
				switch {
				case callErr != nil:
					failures.record(grokMCPReadinessDiagnostic(callErr))
				case readyErr != nil:
					failures.record(readyErr)
				default:
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for the Grok lane agent_sessions MCP: %s", failures.summary(ctx.Err()))
		case <-worker.done:
			return worker.attributedError("headless Grok lane ACP worker exited during MCP startup", nil)
		case <-ticker.C:
		}
	}
}

func grokMCPReadinessDiagnostic(err error) error {
	var rpcErr *grokRPCError
	if !errors.As(err, &rpcErr) {
		return err
	}
	detail := rpcErr.diagnosticDetail()
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w; detail: %s", err, detail)
}

// grokMCPReadinessFailures retains the last substantive protocol failure even
// when the final bounded probe ends with its parent context. Without this, a
// deterministic, promptly returned ACP/MCP error is misreported as a hung
// request merely because the retry window eventually closes.
type grokMCPReadinessFailures struct {
	lastFailure          string
	lastSubstantive      string
	lastSubstantiveCount int
}

func (f *grokMCPReadinessFailures) record(err error) {
	if err == nil {
		return
	}
	f.lastFailure = err.Error()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return
	}
	if f.lastSubstantive == f.lastFailure {
		f.lastSubstantiveCount++
		return
	}
	f.lastSubstantive = f.lastFailure
	f.lastSubstantiveCount = 1
}

func (f *grokMCPReadinessFailures) summary(ctxErr error) string {
	if f.lastSubstantive != "" {
		if f.lastSubstantiveCount > 1 {
			return fmt.Sprintf("%s (repeated %d times before %v)", f.lastSubstantive, f.lastSubstantiveCount, ctxErr)
		}
		return fmt.Sprintf("%s (before %v)", f.lastSubstantive, ctxErr)
	}
	if f.lastFailure != "" {
		return f.lastFailure
	}
	return ctxErr.Error()
}

func (m *grokLaneManager) handleACPNotification(message map[string]any) {
	method := stringValue(message["method"])
	if method != "session/update" && method != "x.ai/session/update" && method != "_x.ai/session/update" {
		return
	}
	params, _ := message["params"].(map[string]any)
	m.mu.Lock()
	grokSessionID := m.state.GrokSessionID
	m.mu.Unlock()
	if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != grokSessionID {
		return
	}
	update, _ := params["update"].(map[string]any)
	if update == nil {
		update, _ = params["sessionUpdate"].(map[string]any)
	}
	if stringValue(update["sessionUpdate"]) != "agent_message_chunk" {
		return
	}
	content, _ := update["content"].(map[string]any)
	text := stringValue(content["text"])
	if text == "" {
		text = stringValue(update["text"])
	}
	if text == "" {
		return
	}
	m.mu.Lock()
	if m.activeTurnID != "" {
		m.activeAnswer.WriteString(text)
	}
	m.mu.Unlock()
}

func (m *grokLaneManager) signalTurn() {
	select {
	case m.turnNotify <- struct{}{}:
	default:
	}
}

func (m *grokLaneManager) turnLoop() {
	for {
		select {
		case <-m.done:
			return
		case <-m.turnNotify:
			for m.executeNextTurn() {
			}
		}
	}
}

func (m *grokLaneManager) executeNextTurn() bool {
	m.mu.Lock()
	if m.closing || m.activeTurnID != "" {
		m.mu.Unlock()
		return false
	}
	index := -1
	for candidate := range m.state.Turns {
		if m.state.Turns[candidate].Status == "queued" {
			index = candidate
			break
		}
	}
	if index < 0 {
		m.mu.Unlock()
		return false
	}
	now := time.Now().UnixMilli()
	previous := cloneGrokLaneState(m.state)
	turn := &m.state.Turns[index]
	turn.Status, turn.StartedAt = "active", now
	m.activeTurnID = turn.ID
	m.activeAnswer.Reset()
	m.state.Status, m.state.AutoArchiveAt = "active", 0
	if err := m.persistLocked(); err != nil {
		m.state = previous
		m.activeTurnID = ""
		m.mu.Unlock()
		m.shutdown("persist active Grok lane turn failed", false)
		return false
	}
	turnID, prompt, timeout := turn.ID, turn.Prompt, time.Duration(turn.TimeoutMS)*time.Millisecond
	m.publishStatusLocked("busy")
	m.mu.Unlock()

	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	result, requestErr := m.client.request(ctx, "session/prompt", map[string]any{
		"sessionId": m.state.GrokSessionID, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
	})
	cancel()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		_ = m.client.notifyRequest(map[string]any{"sessionId": m.state.GrokSessionID})
	}

	m.mu.Lock()
	if m.closing {
		m.activeTurnID = ""
		m.mu.Unlock()
		return false
	}
	index = m.turnIndexLocked(turnID)
	if index < 0 {
		m.activeTurnID = ""
		m.mu.Unlock()
		return true
	}
	turn = &m.state.Turns[index]
	turn.Result = m.activeAnswer.String()
	turn.CompletedAt = time.Now().UnixMilli()
	m.finishTurnLocked(turn, turnID, result, requestErr, ctx.Err())
	m.queueTerminalNoticeLocked(*turn)
	m.activeTurnID = ""
	m.state.Status = "idle"
	if !m.hasQueuedTurnLocked() && m.state.AutoArchive {
		m.state.AutoArchiveAt = time.Now().UnixMilli() + m.state.AutoArchiveDelayMS
	}
	if err := m.persistLocked(); err != nil {
		m.mu.Unlock()
		m.shutdown("persist terminal Grok lane turn failed", false)
		return false
	}
	m.publishStatusLocked("idle")
	queued := m.hasQueuedTurnLocked()
	flushNotices := grokLaneHasUnsentNotices(m.state)
	m.mu.Unlock()
	if flushNotices {
		go m.flushTerminalNotices()
	}
	return queued
}

func (m *grokLaneManager) finishTurnLocked(turn *grokLaneTurn, turnID string, result map[string]any, requestErr, contextErr error) {
	switch {
	case m.interruptedID == turnID:
		turn.Status, turn.Outcome, turn.Exit = "interrupted", "interrupted", 130
		m.interruptedID = ""
	case errors.Is(contextErr, context.DeadlineExceeded):
		turn.Status, turn.Outcome, turn.Exit, turn.Error = "timed_out", "timed_out", 124, "turn deadline exceeded"
	case requestErr != nil:
		turn.Status, turn.Outcome, turn.Exit, turn.Error = "failed", "failed", 1, "Grok ACP prompt failed"
	default:
		turn.Status, turn.Outcome, turn.Exit = "completed", "completed", 0
		if stopReason := stringValue(result["stopReason"]); stopReason != "" && stopReason != "end_turn" && stopReason != "stop_sequence" {
			turn.Outcome = stopReason
		}
	}
}

func (m *grokLaneManager) turnIndexLocked(turnID string) int {
	for index := range m.state.Turns {
		if m.state.Turns[index].ID == turnID {
			return index
		}
	}
	return -1
}

func (m *grokLaneManager) hasQueuedTurnLocked() bool {
	for _, turn := range m.state.Turns {
		if turn.Status == "queued" {
			return true
		}
	}
	return false
}

func (m *grokLaneManager) publishStatusLocked(status string) {
	if m.peer == nil {
		return
	}
	m.peer.mu.Lock()
	m.peer.applyStatusLocked(status)
	_ = m.peer.writeRecordsLocked()
	m.peer.mu.Unlock()
}

func (m *grokLaneManager) acceptLoop(listener net.Listener) {
	acceptLaneControlLoop(listener, m.done, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.controlClosed {
			return false
		}
		m.controlWG.Add(1)
		return true
	}, m.controlWG.Done, m.handleControlConn)
}

func (m *grokLaneManager) handleControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	if !scanner.Scan() {
		return
	}
	var request map[string]any
	if json.Unmarshal(scanner.Bytes(), &request) != nil {
		writeClaudeLaneControlResponse(conn, nil, errors.New("invalid Grok lane control request"))
		return
	}
	response, err := m.handleControl(request)
	archiveAccepted := err == nil && stringValue(request["action"]) == "archive"
	if archiveAccepted {
		m.beginShutdown("explicit archive", true)
		m.logShutdownTrigger("control=archive")
	}
	writeClaudeLaneControlResponse(conn, response, err)
	if archiveAccepted {
		go m.shutdown("explicit archive", true)
	}
}

//nolint:gocyclo // Control actions are explicit and separately attest model-visible wake operations.
func (m *grokLaneManager) handleControl(request map[string]any) (map[string]any, error) {
	if sessionID := stringValue(request["sessionId"]); sessionID == "" || sessionID != m.state.SessionID {
		return nil, errors.New("grok lane control session mismatch")
	}
	action := stringValue(request["action"])
	if containsString([]string{"status", "wake", "wake_status"}, action) {
		provided := stringValue(request["launchToken"])
		if subtle.ConstantTimeCompare([]byte(provided), []byte(m.launchToken)) != 1 {
			return nil, errors.New("grok lane launch token mismatch")
		}
	}
	switch action {
	case "status":
		m.mu.Lock()
		defer m.mu.Unlock()
		return map[string]any{
			"sessionId": m.state.SessionID, "leaderReady": m.worker != nil, "loaded": m.state.SessionCreated,
			"ready": m.peer != nil, "permissionMode": m.state.PermissionMode,
			"refreshDeferred": false, "permissionAuthority": "headless_lane_policy",
		}, nil
	case "wake":
		item, _ := request["item"].(map[string]any)
		return m.queueWake(item)
	case "wake_status":
		return m.wakeStatus(stringValue(request["messageId"]))
	case "ack":
		turnID := stringValue(request["turnId"])
		if turnID == "" {
			return nil, errors.New("grok lane ack requires a turn id")
		}
		m.mu.Lock()
		previous := cloneGrokLaneState(m.state)
		found := false
		for index := range m.state.Turns {
			if m.state.Turns[index].ID == turnID {
				if !containsString([]string{"completed", "failed", "interrupted", "timed_out"}, m.state.Turns[index].Status) {
					m.mu.Unlock()
					return nil, errors.New("grok lane can acknowledge only a terminal turn")
				}
				m.state.Turns[index].Collected = true
				m.state.CollectedTurnID = turnID
				m.cancelTerminalNoticeLocked(turnID)
				found = true
				break
			}
		}
		if !found {
			m.mu.Unlock()
			return nil, fmt.Errorf("grok lane turn %s was not found", turnID)
		}
		if m.peer == nil || m.state.Status == "starting" {
			m.state = previous
			m.mu.Unlock()
			return nil, errors.New("grok lane is not ready")
		}
		m.state.TurnID = firstGrokLaneDebt(m.state)
		err := m.persistLocked()
		if err != nil {
			m.state = previous
		}
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return map[string]any{"acknowledged": true, "turnId": turnID}, nil
	case "resume":
		body, _ := json.Marshal(request["turn"])
		var turn grokLaneTurn
		if json.Unmarshal(body, &turn) != nil || turn.ID == "" || strings.TrimSpace(turn.Prompt) == "" ||
			turn.Status != "queued" || turn.Collected || turn.MessageID != "" || turn.Fingerprint != "" ||
			turn.Result != "" || turn.Error != "" || turn.Outcome != "" || turn.Exit != 0 ||
			turn.CreatedAt <= 0 || turn.StartedAt != 0 || turn.CompletedAt != 0 || turn.TimeoutMS < 0 {
			return nil, errors.New("invalid Grok lane resume turn")
		}
		m.mu.Lock()
		if m.peer == nil || m.closing || m.state.Status == "starting" || m.state.Status == "archived" || m.activeTurnID != "" || m.hasQueuedTurnLocked() {
			m.mu.Unlock()
			return nil, errors.New("grok lane is not idle")
		}
		if debt := firstGrokLaneDebt(m.state); debt != "" {
			m.mu.Unlock()
			return nil, fmt.Errorf("collect outstanding Grok lane turn %s before resume", debt)
		}
		previous := cloneGrokLaneState(m.state)
		if persistent, _ := request["persistent"].(bool); persistent {
			m.state.Persistent = true
			m.state.OwnerPID, m.state.OwnerProcStart, m.state.OwnerSessionID = 0, "", ""
		} else {
			if err := validateLaneOwner(false, intValue(request["ownerPid"]), stringValue(request["ownerProcStart"])); err != nil {
				m.mu.Unlock()
				return nil, err
			}
			m.state.OwnerPID, m.state.OwnerProcStart = intValue(request["ownerPid"]), stringValue(request["ownerProcStart"])
			m.state.OwnerSessionID = stringValue(request["ownerSessionId"])
		}
		if auto, ok := request["autoArchive"].(bool); ok {
			m.state.AutoArchive = auto
			if !auto {
				m.state.AutoArchiveAt = 0
			}
		}
		if delay := int64Value(request["autoArchiveDelayMs"]); delay > 0 {
			m.state.AutoArchiveDelayMS = delay
		}
		if notifySet, _ := request["notifySet"].(bool); notifySet {
			m.state.NotifyTarget = stringValue(request["notifyTarget"])
		}
		groupsBody, _ := json.Marshal(request["groups"])
		explicitBody, _ := json.Marshal(request["explicitGroups"])
		var groups, explicit []string
		if json.Unmarshal(groupsBody, &groups) != nil || json.Unmarshal(explicitBody, &explicit) != nil {
			m.state = previous
			m.mu.Unlock()
			return nil, errors.New("invalid Grok lane group state")
		}
		m.state.Groups, m.state.ExplicitGroups = groups, explicit
		m.state.ParentSessionID = stringValue(request["parentSessionId"])
		m.state.ParentHostID = stringValue(request["parentHostId"])
		m.state.ParentAgentRuntimeDir = stringValue(request["parentAgentRuntimeDir"])
		m.state.InheritParentGroups, _ = request["inheritParentGroups"].(bool)
		m.state.Turns = append(m.state.Turns, turn)
		m.state.TurnID, m.state.LatestTurnID, m.state.AutoArchiveAt = turn.ID, turn.ID, 0
		err := m.persistLocked()
		if err != nil {
			m.state = previous
		}
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		m.signalTurn()
		return map[string]any{"accepted": true, "turnId": turn.ID}, nil
	case "interrupt":
		m.mu.Lock()
		if m.peer == nil || m.client == nil || m.state.Status == "starting" {
			m.mu.Unlock()
			return nil, errors.New("grok lane is not ready")
		}
		turnID := m.activeTurnID
		if turnID == "" {
			m.mu.Unlock()
			return nil, errors.New("grok lane has no active turn")
		}
		m.interruptedID = turnID
		client, grokSessionID := m.client, m.state.GrokSessionID
		m.mu.Unlock()
		if client == nil {
			return nil, errors.New("grok lane ACP worker is unavailable")
		}
		if err := client.notifyRequest(map[string]any{"sessionId": grokSessionID}); err != nil {
			m.mu.Lock()
			if m.interruptedID == turnID {
				m.interruptedID = ""
			}
			m.mu.Unlock()
			return nil, err
		}
		return map[string]any{"turnId": turnID}, nil
	case "archive":
		return map[string]any{"archiving": true}, nil
	default:
		return nil, fmt.Errorf("unknown Grok lane control action %q", action)
	}
}

func (m *grokLaneManager) queueWake(item map[string]any) (map[string]any, error) {
	messageID := stringValue(item["id"])
	if messageID == "" || strings.TrimSpace(stringValue(item["message"])) == "" {
		return nil, errors.New("grok lane wake requires a message id and body")
	}
	fingerprint := wakeItemFingerprint(item)
	m.mu.Lock()
	for _, turn := range m.state.Turns {
		if turn.MessageID != messageID {
			continue
		}
		if turn.Fingerprint != fingerprint {
			m.mu.Unlock()
			return map[string]any{"delivery": "conflict", "messageId": messageID}, nil
		}
		delivery := grokLaneTurnDelivery(turn)
		m.mu.Unlock()
		return grokLaneWakeResult(turn, delivery), nil
	}
	if m.closing || m.state.Status == "archived" {
		previous := cloneGrokLaneState(m.state)
		turn := newGrokLaneTurn(peerMessageText(item))
		turn.MessageID, turn.Fingerprint = messageID, fingerprint
		turn.Status, turn.Outcome, turn.Exit, turn.Error, turn.CompletedAt = "interrupted", "interrupted", 130, "Grok lane is closing", time.Now().UnixMilli()
		m.state.Turns = append(m.state.Turns, turn)
		if m.state.TurnID == "" {
			m.state.TurnID = turn.ID
		}
		m.state.LatestTurnID = turn.ID
		m.queueTerminalNoticeLocked(turn)
		if err := m.persistLocked(); err != nil {
			m.state = previous
			m.mu.Unlock()
			return nil, fmt.Errorf("persist rejected Grok lane wake: %w", err)
		}
		m.mu.Unlock()
		return grokLaneWakeResult(turn, "interrupted"), nil
	}
	previous := cloneGrokLaneState(m.state)
	turn := newGrokLaneTurn(peerMessageText(item))
	turn.MessageID, turn.Fingerprint = messageID, fingerprint
	m.state.Turns = append(m.state.Turns, turn)
	if m.state.TurnID == "" {
		m.state.TurnID = turn.ID
	}
	m.state.LatestTurnID, m.state.AutoArchiveAt = turn.ID, 0
	err := m.persistLocked()
	if err != nil {
		m.state = previous
	}
	m.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("persist accepted Grok lane wake: %w", err)
	}
	m.signalTurn()
	return map[string]any{"delivery": "accepted", "messageId": messageID}, nil
}

func (m *grokLaneManager) wakeStatus(messageID string) (map[string]any, error) {
	if messageID == "" {
		return nil, errors.New("grok lane wake_status requires a message id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, turn := range m.state.Turns {
		if turn.MessageID == messageID {
			return grokLaneWakeResult(turn, grokLaneTurnDelivery(turn)), nil
		}
	}
	return nil, errors.New("grok lane wake is not owned by this manager")
}

func grokLaneTurnDelivery(turn grokLaneTurn) string {
	switch turn.Status {
	case "queued":
		return "queued"
	case "active":
		return "started"
	case "completed":
		return "delivered"
	case "failed", "interrupted", "timed_out":
		return turn.Status
	default:
		return "failed"
	}
}

func grokLaneWakeResult(turn grokLaneTurn, delivery string) map[string]any {
	return map[string]any{
		"delivery": delivery, "messageId": turn.MessageID, "turnId": turn.ID,
		"status": turn.Status, "outcome": emptyStringAsNil(turn.Outcome), "exit": turn.Exit,
	}
}

func (m *grokLaneManager) maintenanceLoop() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.mu.Lock()
			ownerDead := !m.state.Persistent && cleanupProcessIdentityStatus(m.state.OwnerPID, m.state.OwnerProcStart).Status == processIdentityStale
			autoArchive := m.state.AutoArchiveAt > 0 && time.Now().UnixMilli() >= m.state.AutoArchiveAt
			pendingNotices := grokLaneHasUnsentNotices(m.state)
			switch {
			case ownerDead:
				m.beginShutdownLocked("lifecycle owner exited", true)
			case autoArchive:
				m.beginShutdownLocked("auto-archive delay elapsed", true)
			}
			m.mu.Unlock()
			if ownerDead {
				m.shutdown("lifecycle owner exited", true)
				return
			}
			if autoArchive {
				m.shutdown("auto-archive delay elapsed", true)
				return
			}
			if pendingNotices {
				go m.tryFlushTerminalNotices()
			}
		}
	}
}

func cloneGrokLaneState(state grokLaneState) grokLaneState {
	state.Turns = append([]grokLaneTurn(nil), state.Turns...)
	state.Notices = append([]claudeLaneNotice(nil), state.Notices...)
	return state
}

func (m *grokLaneManager) queueTerminalNoticeLocked(turn grokLaneTurn) {
	queueGrokLaneTerminalNotice(&m.state, turn)
}

func (m *grokLaneManager) cancelTerminalNoticeLocked(turnID string) {
	for index := range m.state.Notices {
		if m.state.Notices[index].TurnID == turnID && m.state.Notices[index].SentAt == 0 {
			m.state.Notices[index].SentAt = time.Now().UnixMilli()
		}
	}
}

func cancelAllGrokLaneNotices(state *grokLaneState) int {
	now, dropped := time.Now().UnixMilli(), 0
	for index := range state.Notices {
		if state.Notices[index].SentAt == 0 {
			state.Notices[index].SentAt = now
			dropped++
		}
	}
	return dropped
}

func queueGrokLaneTerminalNotice(state *grokLaneState, turn grokLaneTurn) {
	state.Notices = appendLaneTerminalNotice(
		state.Notices, "grok", state.Name, state.SessionID, turn.ID, turn.Status, turn.Outcome, turn.Exit,
		state.NotifyTarget, state.ParentHostID, state.ParentAgentRuntimeDir, state.Groups,
	)
}

func grokLaneHasUnsentNotices(state grokLaneState) bool {
	for _, notice := range state.Notices {
		if notice.SentAt == 0 {
			return true
		}
	}
	return false
}

func (m *grokLaneManager) flushTerminalNotices() {
	m.noticeMu.Lock()
	defer m.noticeMu.Unlock()
	m.flushTerminalNoticesLocked()
}

func (m *grokLaneManager) tryFlushTerminalNotices() {
	if !m.noticeMu.TryLock() {
		return
	}
	defer m.noticeMu.Unlock()
	m.flushTerminalNoticesLocked()
}

func (m *grokLaneManager) flushTerminalNoticesLocked() {
	noticeLock, err := lockGrokLaneNotices(m.paths, m.state.SessionID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(noticeLock)
	for {
		m.mu.Lock()
		if latest, readErr := readGrokLaneState(m.paths, m.state.SessionID); readErr == nil {
			m.state.Notices = latest.Notices
		}
		index := -1
		for current := range m.state.Notices {
			if m.state.Notices[current].SentAt == 0 {
				index = current
				break
			}
		}
		if index < 0 {
			m.mu.Unlock()
			return
		}
		notice, state := m.state.Notices[index], cloneGrokLaneState(m.state)
		if notice.LastAttemptAt > 0 && time.Now().UnixMilli()-notice.LastAttemptAt < 250 {
			m.mu.Unlock()
			return
		}
		m.state.Notices[index].Attempts++
		m.state.Notices[index].LastAttemptAt = time.Now().UnixMilli()
		if m.persistLocked() != nil {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()

		target := currentGrokLaneNotifyTarget(m.paths, state, notice.Target)
		deliveryErr := deliverGrokLaneNotice(m.paths, state, target, notice.ID, notice.Message)
		m.mu.Lock()
		for current := range m.state.Notices {
			if m.state.Notices[current].ID == notice.ID && deliveryErr == nil {
				m.state.Notices[current].SentAt = time.Now().UnixMilli()
				break
			}
		}
		persistErr := m.persistLocked()
		m.mu.Unlock()
		if deliveryErr != nil || persistErr != nil {
			return
		}
	}
}

func lockGrokLaneNotices(paths nativePaths, sessionID string) (*os.File, error) {
	return lockLaneFile(paths, "grok-lane-notice-locks", sessionID, true)
}

func currentGrokLaneNotifyTarget(paths nativePaths, state grokLaneState, fallback string) string {
	if fallback == "" {
		return ""
	}
	if exactProcessIdentityMatch(state.OwnerPID, state.OwnerProcStart) {
		row := readJSONMap(filepath.Join(paths.claudeRoot, "sessions", fmt.Sprintf("%d.json", state.OwnerPID)))
		if intValue(row["pid"]) == state.OwnerPID && stringValue(row["procStart"]) == state.OwnerProcStart && validSessionID(stringValue(row["sessionId"])) {
			return "session:" + stringValue(row["sessionId"])
		}
	}
	return fallback
}

func deliverGrokLaneNotice(paths nativePaths, state grokLaneState, target, noticeID, message string) error {
	_ = paths
	return deliverGroupedLaneNotice(state.SessionID, target, noticeID, message)
}

func createGrokLaneNoticeFrame(sender map[string]any, noticeID, message string) map[string]any {
	from := encodeNativeAddress(stringValue(sender["socketPath"]))
	mode := "prompting"
	if stringValue(sender["permissionMode"]) == "bypassPermissions" {
		mode = "bypass"
	}
	sentAt := time.Now().UTC().Format(time.RFC3339Nano)
	content := wrapNativePeerMessageForProduct("grok", from, stringValue(sender["sessionId"]),
		stringValue(sender["name"]), mode, noticeID, sentAt, message)
	return map[string]any{
		"msgV": 1, "msg_id": noticeID, "type": "user", "priority": "next", "from": from,
		"message": map[string]any{"role": "user", "content": content},
	}
}

func grokLaneVirtualSender(state grokLaneState) map[string]any {
	return map[string]any{
		"socketPath": state.MessagingSocket, "sessionId": state.SessionID,
		"name": state.Name, "permissionMode": state.PermissionMode, "entrypoint": "grok",
	}
}

func flushOrphanGrokLaneNotices(paths nativePaths, sessionID string) {
	noticeLock, err := lockGrokLaneNotices(paths, sessionID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(noticeLock)
	state, err := readGrokLaneState(paths, sessionID)
	if err != nil {
		return
	}
	for _, notice := range state.Notices {
		if notice.SentAt != 0 {
			continue
		}
		target := currentGrokLaneNotifyTarget(paths, state, notice.Target)
		if deliverGrokLaneNotice(paths, state, target, notice.ID, notice.Message) != nil {
			return
		}
		lock, lockErr := lockLaneStateFile(paths, "grok-"+sessionID)
		if lockErr != nil {
			return
		}
		latest, readErr := readGrokLaneState(paths, sessionID)
		if readErr == nil {
			for index := range latest.Notices {
				if latest.Notices[index].ID == notice.ID && latest.Notices[index].SentAt == 0 {
					latest.Notices[index].SentAt = time.Now().UnixMilli()
				}
			}
			if writeErr := writeGrokLaneStateUnlocked(paths, latest); writeErr == nil {
				state = latest
			} else {
				readErr = writeErr
			}
		}
		unlockLaneStateFile(lock)
		if readErr != nil {
			return
		}
	}
}

func (m *grokLaneManager) persistLocked() error {
	if m.persistOverride != nil {
		return m.persistOverride(m.state)
	}
	return writeGrokLaneState(m.paths, m.state)
}

//nolint:gocyclo // Shutdown quiesces admission, terminalizes debt, stops the process session, and proves cleanup.
func (m *grokLaneManager) shutdown(reason string, interrupt bool) {
	startupDone := m.beginShutdown(reason, interrupt)
	if startupDone != nil {
		<-startupDone
	}
	m.cleanupOnce.Do(func() {
		reason, interrupt = m.recordedShutdown(reason, interrupt)
		// Withdraw and drain peer admission while the manager control socket is
		// still available. Every user frame whose transport write succeeded can
		// therefore reach queueWake and acquire a durable terminal disposition.
		m.mu.Lock()
		peer := m.peer
		m.mu.Unlock()
		if peer != nil {
			peer.shutdown()
		}
		m.mu.Lock()
		m.controlClosed = true
		listener := m.listener
		m.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
		m.controlWG.Wait()

		explicitArchive := reason == "explicit archive"
		if explicitArchive {
			m.noticeMu.Lock()
			defer m.noticeMu.Unlock()
		}
		m.mu.Lock()
		now := time.Now().UnixMilli()
		terminalStatus, terminalOutcome, terminalExit := "failed", "failed", 1
		if interrupt {
			terminalStatus, terminalOutcome, terminalExit = "interrupted", "interrupted", 130
		}
		for index := range m.state.Turns {
			turn := &m.state.Turns[index]
			if containsString([]string{"queued", "active"}, turn.Status) {
				turn.Status, turn.Outcome, turn.Exit, turn.Error, turn.CompletedAt = terminalStatus, terminalOutcome, terminalExit, reason, now
				m.queueTerminalNoticeLocked(*turn)
			}
		}
		m.state.ArchiveDroppedNotices = 0
		if explicitArchive {
			m.state.ArchiveDroppedNotices = cancelAllGrokLaneNotices(&m.state)
		}
		m.state.Status, m.state.AutoArchiveAt = "archived", 0
		persistErr := m.persistLocked()
		client, worker := m.client, m.worker
		grokSessionID, workerSessionID, workerProcStart, workerStrongStart := m.state.GrokSessionID, m.state.WorkerSessionID, m.state.WorkerProcStart, m.state.WorkerStrongStart
		cleanupState := cloneGrokLaneState(m.state)
		m.mu.Unlock()
		registryGuard, cleanupRoots, registryErr := grokLaneCleanupRoots(cleanupState, true)
		if registryGuard != nil {
			defer registryGuard.close()
		}
		if persistErr == nil && !explicitArchive {
			m.flushTerminalNotices()
		}
		if interrupt && client != nil {
			_ = client.notifyRequest(map[string]any{"sessionId": grokSessionID})
		}
		// Snapshot and stop the exact worker plus every registered tool-shell
		// root before ACP shutdown changes ancestry. Registered roots remain
		// authoritative after manager death even when Darwin hides their env.
		taggedCleanupErr := registryErr
		if taggedCleanupErr == nil && registryGuard != nil && registryGuard.ledger != nil {
			taggedCleanupErr = registryGuard.ledger.reconcileCleanup()
		}
		if client != nil {
			client.close()
		} else {
			stopGrokManagedProcess(worker, 2*time.Second)
		}
		sessionCleanupErr := stopGrokProcessSessionStrong(workerSessionID, workerProcStart, workerStrongStart, os.Getpid())
		if m.diagnostics != nil {
			_ = m.diagnostics.close()
		}
		cleanupErr := persistErr
		if cleanupErr == nil {
			cleanupErr = sessionCleanupErr
		}
		if cleanupErr == nil {
			cleanupErr = taggedCleanupErr
		}
		if cleanupErr == nil && registryGuard != nil {
			cleanupErr = registryGuard.removeArtifacts()
		}
		if cleanupErr == nil {
			cleanupErr = cleanupGrokLaneOwnedFiles(m.paths, cleanupState, os.Getpid(), cleanupRoots...)
		}
		if m.launchLease != nil {
			_ = syscall.Flock(int(m.launchLease.Fd()), syscall.LOCK_UN)
			_ = m.launchLease.Close()
		}
		if cleanupErr == nil {
			m.mu.Lock()
			previous := cloneGrokLaneState(m.state)
			m.state.ManagerPID, m.state.ManagerProcStart, m.state.ManagerStrongStart = 0, "", ""
			m.state.WorkerPID, m.state.WorkerProcStart, m.state.WorkerStrongStart, m.state.WorkerSessionID = 0, "", "", 0
			m.state.ControlSocket, m.state.MessagingSocket, m.state.StartupID = "", "", ""
			if err := m.persistLocked(); err != nil {
				m.state = previous
				fmt.Fprintln(os.Stderr, "grok-lane-manager: persist final archived ownership state failed")
			}
			m.mu.Unlock()
		}
		if m.lifecycleLock != nil {
			unlockLaneLifecycle(m.lifecycleLock)
			m.lifecycleLock = nil
		}
		close(m.done)
	})
}
