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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type grokLaneManager struct {
	mu              sync.Mutex
	noticeMu        sync.Mutex
	paths           nativePaths
	state           grokLaneState
	launchToken     string
	hostPaths       grokHostPaths
	listener        net.Listener
	controlWG       sync.WaitGroup
	controlClosed   bool
	lifecycleLock   *os.File
	launchLease     *os.File
	diagnostics     *grokDiagnosticSink
	worker          *grokManagedProcess
	client          *grokACPClient
	peer            *daemon
	activeAnswer    strings.Builder
	activeTurnID    string
	interruptedID   string
	turnNotify      chan struct{}
	done            chan struct{}
	startupDone     chan struct{}
	closing         bool
	cleanupOnce     sync.Once
	persistOverride func(grokLaneState) error
}

func runGrokLaneManager(argv []string) int {
	args := parseArgs(argv)
	sessionID := args["session-id"]
	if !validSessionID(sessionID) {
		fmt.Fprintln(os.Stderr, "grok-lane-manager requires --session-id")
		return 2
	}
	paths := resolveNativePaths()
	state, err := readGrokLaneState(paths, sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grok-lane-manager: cannot read lane state")
		return 1
	}
	manager := &grokLaneManager{
		paths: paths, state: state,
		launchToken: strings.TrimSpace(os.Getenv(grokLaunchTokenEnv)),
		turnNotify:  make(chan struct{}, 1), done: make(chan struct{}), startupDone: make(chan struct{}),
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			manager.shutdown("manager signalled", true)
		case <-manager.done:
		}
	}()
	if err := manager.start(); err != nil {
		// The manager's stderr is a private 0600 per-lane log, never the calling
		// model/TUI stream. Retain the actionable startup cause there while the
		// public launcher continues to return a bounded generic failure.
		fmt.Fprintf(os.Stderr, "grok-lane-manager: startup failed: %v\n", err)
		manager.shutdown("manager startup failed", false)
		return 1
	}
	select {
	case <-manager.workerDone():
		manager.shutdown("Grok ACP worker exited", false)
	case <-manager.done:
	}
	return 0
}

func (m *grokLaneManager) workerDone() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worker == nil {
		return m.done
	}
	return m.worker.done
}

func (m *grokLaneManager) closingState() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closing
}

//nolint:gocyclo // Startup is one ownership transaction: state, socket, launch record, ACP, MCP, and publication.
func (m *grokLaneManager) start() error {
	if m.startupDone != nil {
		defer close(m.startupDone)
	}
	if !validGrokLaunchToken(m.launchToken) || subtle.ConstantTimeCompare([]byte(m.state.LaunchTokenHash), []byte(grokTokenHash(m.launchToken))) != 1 {
		return errors.New("grok lane launch capability is unavailable")
	}
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
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		_ = listener.Close()
		return errors.New("grok lane manager is closing during startup")
	}
	m.listener = listener
	m.state.ManagerPID, m.state.ManagerProcStart = os.Getpid(), managerProcStart
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	go m.acceptLoop(listener)
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
	ctx, cancel := context.WithTimeout(context.Background(), grokACPStartupTimeout)
	err = m.initializeACP(ctx)
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), grokLaneMCPReadyTimeout)
	err = m.waitForAgentSessionsMCP(ctx)
	cancel()
	if err != nil {
		return err
	}
	if m.closingState() {
		return errors.New("grok lane manager is closing before publication")
	}
	m.mu.Lock()
	persistent, ownerPID, ownerProcStart := m.state.Persistent, m.state.OwnerPID, m.state.OwnerProcStart
	m.mu.Unlock()
	if !persistent && !exactProcessIdentityMatch(ownerPID, ownerProcStart) {
		return errors.New("grok lane lifecycle owner exited during startup")
	}
	peer := newDaemon(map[string]string{
		"session-id": m.state.SessionID, "cwd": m.state.Cwd, "name": m.state.Name,
		"name-source": "lane", "entrypoint": "grok", "permission-mode": m.state.PermissionMode,
		"status": "idle", "supervisor-socket": m.state.ControlSocket, "supervisor-token": m.launchToken,
		"owner-pid": fmt.Sprintf("%d", os.Getpid()), "owner-proc-start": m.state.ManagerProcStart,
		"data-dir": m.paths.dataRoot, "claude-config-dir": m.paths.claudeRoot,
		"codex-home": m.paths.codexHome, "runtime-dir": m.paths.runtimeDir,
	})
	if err := peer.start(); err != nil {
		return fmt.Errorf("publish Grok lane peer: %w", err)
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		peer.shutdown()
		return errors.New("grok lane manager is closing before publication")
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
	go m.turnLoop()
	go m.maintenanceLoop()
	m.signalTurn()
	return nil
}

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
	command.Env = grokLaneManagerEnvironment(os.Environ(), m.launchToken, m.state.SessionID)
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
		return errors.New("grok lane manager is closing during worker startup")
	}
	m.worker = worker
	m.state.WorkerPID, m.state.WorkerProcStart = worker.cmd.Process.Pid, worker.procStart
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
	client := newGrokACPClient(worker, stdin, stdout, m.state.SessionID, 0, nil)
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		client.close()
		return errors.New("grok lane manager is closing during ACP startup")
	}
	m.client = client
	m.mu.Unlock()
	return nil
}

//nolint:gocyclo // ACP bootstrap validates authentication and exact create/load identity in one transaction.
func (m *grokLaneManager) initializeACP(ctx context.Context) error {
	m.mu.Lock()
	client, worker := m.client, m.worker
	m.mu.Unlock()
	if client == nil || worker == nil {
		return errors.New("headless Grok lane ACP worker is unavailable")
	}
	result, err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return worker.attributedError("initialize headless Grok lane ACP worker", err)
	}
	if !grokAuthMethodAdvertised(result, "cached_token") {
		return worker.attributedError("authenticate headless Grok lane ACP worker", errors.New("cached_token authentication was not advertised"))
	}
	if _, err := client.request(ctx, "authenticate", map[string]any{"methodId": "cached_token", "_meta": map[string]any{"headless": true}}); err != nil {
		return worker.attributedError("authenticate headless Grok lane ACP worker", err)
	}
	params := map[string]any{"cwd": m.state.Cwd, "mcpServers": []any{}, "_meta": map[string]any{"yoloMode": true}}
	method := "session/new"
	if m.state.SessionCreated {
		method = "session/load"
		if strings.TrimSpace(m.state.GrokSessionID) == "" {
			return errors.New("grok lane has no native ACP session identity to load")
		}
		params["sessionId"] = m.state.GrokSessionID
	}
	created, err := client.request(ctx, method, params)
	if err != nil {
		return worker.attributedError("open headless Grok lane session", err)
	}
	returned := stringValue(created["sessionId"])
	if method == "session/new" && returned == "" {
		return errors.New("grok ACP returned no native session identity")
	}
	if method == "session/load" && returned != "" && returned != m.state.GrokSessionID {
		return errors.New("grok ACP loaded a different native session identity")
	}
	m.mu.Lock()
	if returned != "" {
		m.state.GrokSessionID = returned
	}
	m.state.SessionCreated = true
	err = m.persistLocked()
	m.mu.Unlock()
	if err == nil {
		client.setNotificationHandler(m.handleACPNotification)
	}
	return err
}

func (m *grokLaneManager) waitForAgentSessionsMCP(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		client, worker, grokSessionID := m.client, m.worker, m.state.GrokSessionID
		m.mu.Unlock()
		if client == nil || worker == nil || grokSessionID == "" {
			return errors.New("headless Grok lane ACP session is unavailable")
		}
		roster, rosterErr := client.request(ctx, "_x.ai/sessions/list", map[string]any{})
		if rosterErr == nil {
			state, stateErr := grokRosterStateFromResponse(roster, grokSessionID)
			if stateErr == nil && state.permissionMode == "bypassPermissions" {
				result, callErr := client.request(ctx, "_x.ai/mcp/call", map[string]any{
					"sessionId": grokSessionID, "server": "agent_sessions", "tool": "list_peers", "arguments": map[string]any{},
				})
				if callErr == nil && grokAgentSessionsMCPCallReady(result) == nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for the Grok lane agent_sessions MCP")
		case <-worker.done:
			return worker.attributedError("headless Grok lane ACP worker exited during MCP startup", nil)
		case <-ticker.C:
		}
	}
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
		_ = m.client.notifyRequest("session/cancel", map[string]any{"sessionId": m.state.GrokSessionID})
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
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
				continue
			}
		}
		m.mu.Lock()
		if m.controlClosed {
			m.mu.Unlock()
			_ = conn.Close()
			continue
		}
		m.controlWG.Add(1)
		m.mu.Unlock()
		go func() {
			defer m.controlWG.Done()
			m.handleControlConn(conn)
		}()
	}
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
	writeClaudeLaneControlResponse(conn, response, err)
	if err == nil && stringValue(request["action"]) == "archive" {
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
		if err := client.notifyRequest("session/cancel", map[string]any{"sessionId": grokSessionID}); err != nil {
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
		turn := newGrokLaneTurn(trustedPeerTextForProduct(item, "grok"), 0)
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
	turn := newGrokLaneTurn(trustedPeerTextForProduct(item, "grok"), 0)
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
			if ownerDead || autoArchive {
				m.closing = true
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
	if state.NotifyTarget == "" {
		return
	}
	for _, notice := range state.Notices {
		if notice.TurnID == turn.ID {
			return
		}
	}
	noticeID := sessionKey("grok-lane-terminal\x00" + state.SessionID + "\x00" + turn.ID)
	message := fmt.Sprintf(
		"GROK_LANE_TERMINAL notice=%s name=%s session=%s turn=%s status=%s outcome=%s exit=%d collection=required\nCollect: grok-peer-lane wait %s",
		noticeID, state.Name, state.SessionID, turn.ID, turn.Status, turn.Outcome, turn.Exit, state.SessionID,
	)
	state.Notices = append(state.Notices, claudeLaneNotice{
		ID: noticeID, TurnID: turn.ID, Target: state.NotifyTarget, Message: message, CreatedAt: time.Now().UnixMilli(),
	})
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
	directory := filepath.Join(profileDataRoot(paths), "grok-lane-notice-locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionKey(sessionID)+".lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // session id is hashed.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
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
	peers, err := listNativePeerSessions(paths)
	if err != nil {
		return err
	}
	resolvedSocket, resolved, err := resolveNativePeerTarget(target, peers)
	if err != nil {
		return err
	}
	virtualSender := grokLaneVirtualSender(state)
	virtualSender, err = nativeSenderMatchingTargetMode(virtualSender, target, resolvedSocket, resolved, peers)
	if err != nil {
		return err
	}
	frame := createGrokLaneNoticeFrame(virtualSender, noticeID, message)
	return sendUnixJSON(resolvedSocket, frame, 5*time.Second)
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
	m.mu.Lock()
	m.closing = true
	startupDone := m.startupDone
	m.mu.Unlock()
	if startupDone != nil {
		<-startupDone
	}
	m.cleanupOnce.Do(func() {
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
		grokSessionID, workerSessionID, workerProcStart := m.state.GrokSessionID, m.state.WorkerSessionID, m.state.WorkerProcStart
		cleanupState := cloneGrokLaneState(m.state)
		m.mu.Unlock()
		if persistErr == nil && !explicitArchive {
			m.flushTerminalNotices()
		}
		if interrupt && client != nil {
			_ = client.notifyRequest("session/cancel", map[string]any{"sessionId": grokSessionID})
		}
		if client != nil {
			client.close()
		} else {
			stopGrokManagedProcess(worker, 2*time.Second)
		}
		sessionCleanupErr := stopGrokProcessSession(workerSessionID, workerProcStart, os.Getpid())
		taggedCleanupErr := stopGrokTaggedProcesses(cleanupState.LaunchTokenHash, os.Getpid())
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
		if cleanupErr == nil {
			cleanupErr = cleanupGrokLaneOwnedFiles(m.paths, cleanupState, os.Getpid())
		}
		if m.launchLease != nil {
			_ = syscall.Flock(int(m.launchLease.Fd()), syscall.LOCK_UN)
			_ = m.launchLease.Close()
		}
		if cleanupErr == nil {
			m.mu.Lock()
			previous := cloneGrokLaneState(m.state)
			m.state.ManagerPID, m.state.ManagerProcStart, m.state.WorkerPID, m.state.WorkerProcStart, m.state.WorkerSessionID = 0, "", 0, "", 0
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
