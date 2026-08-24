package bridge

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
	"github.com/antst/agent-sessions/internal/socketpath"
)

// qwenACPClient is the single Agent Sessions client for one Qwen stdio ACP
// worker. Qwen may issue permission requests while a session/prompt request is
// outstanding, so inbound requests are answered by the reader goroutine while
// ordinary responses are correlated by their exact JSON-RPC id.
type qwenACPClient struct {
	stdin io.WriteCloser

	writeMu sync.Mutex
	stateMu sync.Mutex
	nextID  int64
	pending map[int64]chan qwenACPResponse
	readErr error
	done    chan struct{}

	notifyMu sync.RWMutex
	notify   func(map[string]any)

	permissionMu sync.RWMutex
	permission   func(map[string]any) (map[string]any, error)
}

type qwenACPResponse struct {
	result map[string]any
	err    error
}

type qwenRPCError struct {
	Code    int
	Message string
	Data    any
}

func (e *qwenRPCError) Error() string {
	if e.Code == 0 {
		return "Qwen ACP: " + e.Message
	}
	return fmt.Sprintf("Qwen ACP error %d: %s", e.Code, e.Message)
}

func newQwenACPClient(stdin io.WriteCloser, stdout io.ReadCloser) *qwenACPClient {
	client := &qwenACPClient{
		stdin: stdin, pending: map[int64]chan qwenACPResponse{}, done: make(chan struct{}),
	}
	client.permission = qwenAllowOncePermission
	go client.readLoop(stdout)
	return client
}

func (c *qwenACPClient) setNotificationHandler(handler func(map[string]any)) {
	c.notifyMu.Lock()
	c.notify = handler
	c.notifyMu.Unlock()
}

func (c *qwenACPClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.stateMu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.stateMu.Unlock()
		return nil, fmt.Errorf("qwen ACP %s is unavailable: %w", method, err)
	}
	c.nextID++
	id := c.nextID
	response := make(chan qwenACPResponse, 1)
	c.pending[id] = response
	c.stateMu.Unlock()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err == nil {
		err = c.writeFrame(body)
	}
	if err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("write Qwen ACP %s: %w", method, err)
	}

	select {
	case received := <-response:
		return received.result, received.err
	case <-c.done:
		c.removePending(id)
		return nil, fmt.Errorf("read Qwen ACP %s: %w", method, c.readError())
	case <-ctx.Done():
		c.removePending(id)
		return nil, fmt.Errorf("qwen ACP %s: %w", method, ctx.Err())
	}
}

func (c *qwenACPClient) notifyRequest(method string, params map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	if err := c.writeFrame(body); err != nil {
		return fmt.Errorf("write Qwen ACP %s notification: %w", method, err)
	}
	return nil
}

func (c *qwenACPClient) close() error {
	return c.stdin.Close()
}

func (c *qwenACPClient) readLoop(stdout io.ReadCloser) {
	defer func() { _ = stdout.Close() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 64*1024*1024)
	for scanner.Scan() {
		var message map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			c.fail(fmt.Errorf("malformed Qwen ACP frame: %w", err))
			return
		}
		method := stringValue(message["method"])
		id, hasID := qwenRPCID(message["id"])
		switch {
		case hasID && method != "":
			go c.handleInboundRequest(id, method, mapValue(message["params"]))
		case hasID:
			c.deliverResponse(id, message)
		case method != "":
			c.notifyMu.RLock()
			handler := c.notify
			c.notifyMu.RUnlock()
			if handler != nil {
				handler(message)
			}
		default:
			c.fail(errors.New("malformed Qwen ACP frame has neither id nor method"))
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(err)
}

func (c *qwenACPClient) handleInboundRequest(id int64, method string, params map[string]any) {
	var result map[string]any
	var err error
	if method == "session/request_permission" {
		c.permissionMu.RLock()
		handler := c.permission
		c.permissionMu.RUnlock()
		result, err = handler(params)
	} else {
		err = fmt.Errorf("unsupported Qwen ACP client request %q", method)
	}
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		response["error"] = map[string]any{"code": -32601, "message": err.Error()}
	} else {
		response["result"] = result
	}
	body, marshalErr := json.Marshal(response)
	if marshalErr == nil {
		marshalErr = c.writeFrame(body)
	}
	if marshalErr != nil {
		c.fail(fmt.Errorf("write Qwen ACP response: %w", marshalErr))
	}
}

func qwenAllowOncePermission(params map[string]any) (map[string]any, error) {
	options, ok := params["options"].([]any)
	if !ok || len(options) == 0 {
		return nil, errors.New("qwen permission request has no offered options")
	}
	for _, preferredKind := range []string{"allow_once", "reject_once"} {
		for _, raw := range options {
			option := mapValue(raw)
			if stringValue(option["kind"]) != preferredKind || stringValue(option["optionId"]) == "" {
				continue
			}
			return map[string]any{
				"outcome": map[string]any{"outcome": "selected", "optionId": stringValue(option["optionId"])},
			}, nil
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

func (c *qwenACPClient) deliverResponse(id int64, message map[string]any) {
	c.stateMu.Lock()
	response := c.pending[id]
	delete(c.pending, id)
	c.stateMu.Unlock()
	if response == nil {
		return
	}
	if raw := mapValue(message["error"]); len(raw) != 0 {
		response <- qwenACPResponse{err: &qwenRPCError{
			Code: intValue(raw["code"]), Message: defaultString(stringValue(raw["message"]), "request rejected"), Data: raw["data"],
		}}
		return
	}
	response <- qwenACPResponse{result: mapValue(message["result"])}
}

func (c *qwenACPClient) writeFrame(body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.stdin.Write(append(body, '\n'))
	return err
}

func (c *qwenACPClient) removePending(id int64) {
	c.stateMu.Lock()
	delete(c.pending, id)
	c.stateMu.Unlock()
}

func (c *qwenACPClient) fail(err error) {
	c.stateMu.Lock()
	if c.readErr != nil {
		c.stateMu.Unlock()
		return
	}
	c.readErr = err
	pending := c.pending
	c.pending = map[int64]chan qwenACPResponse{}
	close(c.done)
	c.stateMu.Unlock()
	for _, response := range pending {
		response <- qwenACPResponse{err: err}
	}
}

func (c *qwenACPClient) readError() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.readErr == nil {
		return io.EOF
	}
	return c.readErr
}

func qwenRPCID(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

const qwenACPStartupTimeout = 45 * time.Second

type qwenLaneManager struct {
	mu            sync.Mutex
	noticeMu      sync.Mutex
	paths         nativePaths
	state         qwenLaneState
	launchToken   string
	listener      net.Listener
	controlWG     sync.WaitGroup
	controlClosed bool
	lifecycleLock *os.File
	worker        *grokManagedProcess
	client        *qwenACPClient
	peer          *daemon
	activeAnswer  strings.Builder
	activeTurnID  string
	interruptedID string
	turnNotify    chan struct{}
	done          chan struct{}
	startupDone   chan struct{}
	closing       bool
	shutdownCause string
	shutdownAs130 bool
	cleanupOnce   sync.Once
}

func runQwenLaneManager(argv []string) int {
	args := parseArgs(argv)
	threadID := strings.TrimSpace(args["session-id"])
	if !validSessionID(threadID) {
		fmt.Fprintln(os.Stderr, "qwen-lane-manager requires --session-id")
		return 2
	}
	paths := resolveNativePaths()
	state, err := readQwenLaneState(paths, threadID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "qwen-lane-manager: cannot read lane state")
		return 1
	}
	manager := &qwenLaneManager{
		paths: paths, state: state, launchToken: strings.TrimSpace(os.Getenv(qwenLaneLaunchTokenEnv)),
		turnNotify: make(chan struct{}, 1), done: make(chan struct{}), startupDone: make(chan struct{}),
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		select {
		case caught := <-signals:
			manager.beginShutdown("manager signalled: "+caught.String(), true)
			manager.shutdown("manager signalled: "+caught.String(), true)
		case <-manager.done:
		}
	}()
	if err := manager.start(); err != nil {
		manager.beginShutdown("manager startup failed", false)
		fmt.Fprintf(os.Stderr, "qwen-lane-manager: startup failed: %v\n", err)
		manager.shutdown("manager startup failed", false)
		return 1
	}
	select {
	case <-manager.workerDone():
		manager.shutdown("Qwen ACP worker exited", false)
	case <-manager.done:
	}
	return 0
}

func (m *qwenLaneManager) workerDone() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worker == nil {
		return m.done
	}
	return m.worker.done
}

func (m *qwenLaneManager) beginShutdown(reason string, interrupted bool) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdownCause == "" {
		m.shutdownCause, m.shutdownAs130 = reason, interrupted
	}
	m.closing = true
	return m.startupDone
}

func (m *qwenLaneManager) start() error { //nolint:gocyclo // One transaction owns capability, worker, ACP identity, socket, and grouped publication.
	defer close(m.startupDone)
	if !validQwenLaneToken(m.launchToken) ||
		subtle.ConstantTimeCompare([]byte(m.state.LaunchTokenHash), []byte(qwenLaneTokenHash(m.launchToken))) != 1 {
		return errors.New("qwen lane launch capability is unavailable")
	}
	lifecycle, err := lockLaneLifecycle(m.paths, "qwen-"+m.state.ThreadID)
	if err != nil {
		return err
	}
	m.lifecycleLock = lifecycle
	latest, err := readQwenLaneState(m.paths, m.state.ThreadID)
	if err != nil || latest.Status != "starting" {
		return errors.New("refuse to start Qwen lane manager outside starting state")
	}
	m.state = latest
	if err := ensurePrivateRuntimeDir(filepath.Dir(m.state.ControlSocket)); err != nil {
		return err
	}
	if err := socketpath.Validate(m.state.ControlSocket); err != nil {
		return fmt.Errorf("validate Qwen lane control socket: %w", err)
	}
	_ = os.Remove(m.state.ControlSocket)
	listener, err := net.Listen("unix", m.state.ControlSocket)
	if err != nil {
		return fmt.Errorf("listen on Qwen lane control socket: %w", err)
	}
	if err := os.Chmod(m.state.ControlSocket, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("secure Qwen lane control socket: %w", err)
	}
	managerInfo := procinfo.Read(os.Getpid())
	if managerInfo.Status != procinfo.Known || managerInfo.Start == "" || managerInfo.StrongStart == "" {
		_ = listener.Close()
		return errors.New("capture strong Qwen lane manager identity")
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		_ = listener.Close()
		return errors.New("qwen lane manager is closing during startup")
	}
	m.listener = listener
	m.state.ManagerPID, m.state.ManagerProcStart, m.state.ManagerStrongStart = os.Getpid(), managerInfo.Start, managerInfo.StrongStart
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	go m.acceptLoop(listener)
	if err := m.prepareToolRegistry(); err != nil {
		return fmt.Errorf("prepare Qwen tool-root registry: %w", err)
	}
	if err := m.startWorker(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), qwenACPStartupTimeout)
	err = m.initializeACP(ctx)
	cancel()
	if err != nil {
		return err
	}
	m.mu.Lock()
	persistent, ownerPID, ownerStart := m.state.Persistent, m.state.OwnerPID, m.state.OwnerProcStart
	m.mu.Unlock()
	if !persistent && !exactProcessIdentityMatch(ownerPID, ownerStart) {
		return errors.New("qwen lane lifecycle owner exited during startup")
	}
	peerOptions := map[string]string{
		"session-id": m.state.ThreadID, "cwd": m.state.Cwd, "name": m.state.Name,
		"name-source": "lane", "entrypoint": "qwen", "permission-mode": defaultString(m.state.CurrentNativeMode, "unknown"),
		"status": "idle", "supervisor-socket": m.state.ControlSocket, "supervisor-token": m.launchToken,
		"owner-pid": fmt.Sprintf("%d", os.Getpid()), "owner-proc-start": m.state.ManagerProcStart,
		"data-dir": m.paths.dataRoot, "claude-config-dir": m.paths.claudeRoot,
		"codex-home": m.paths.codexHome, "runtime-dir": m.paths.runtimeDir,
	}
	if laneAgentConfigured() {
		peerOptions["agent-runtime-dir"] = laneAgentRuntimeDir()
	}
	peer := newDaemon(peerOptions)
	peer.registrationOverride = func(current federator.PeerRegistration) federator.PeerRegistration {
		current.QwenCapabilityDigest = "sha256:" + m.state.LaunchTokenHash
		return current
	}
	if err := peer.start(); err != nil {
		return fmt.Errorf("publish Qwen lane peer: %w", err)
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		peer.shutdown()
		return errors.New("qwen lane manager is closing before publication")
	}
	m.peer = peer
	m.state.MessagingSocket = peer.stableSocket
	m.state.Status, m.state.StartupID = "idle", ""
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		peer.shutdown()
		return err
	}
	go m.turnLoop()
	go m.maintenanceLoop()
	m.signalTurn()
	return nil
}

func (m *qwenLaneManager) startWorker() error {
	executable := strings.TrimSpace(os.Getenv(qwenLaneExecutableEnv))
	if executable == "" {
		return errors.New("validated Qwen executable is unavailable")
	}
	args := []string{"--acp"}
	if m.state.RequestedInitialMode != "" {
		args = append(args, "--approval-mode", m.state.RequestedInitialMode)
	}
	command := exec.Command(executable, args...) //nolint:gosec // launcher-selected executable and parsed native mode.
	command.Dir = m.state.Cwd
	command.Env = qwenLaneWorkerToolEnvironment(qwenLaneWorkerEnvironment(os.Environ(), m.state, m.launchToken), m.state)
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = os.Stderr
	worker, err := startGrokManagedProcess(command, nil)
	if err != nil {
		return fmt.Errorf("start Qwen ACP worker: %w", err)
	}
	info := procinfo.Read(worker.cmd.Process.Pid)
	if info.Status != procinfo.Known || info.Start != worker.procStart || info.StrongStart == "" {
		stopGrokManagedProcess(worker, 2*time.Second)
		return errors.New("capture strong Qwen ACP worker identity")
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		stopGrokManagedProcess(worker, 2*time.Second)
		return errors.New("qwen lane manager is closing during worker startup")
	}
	m.worker = worker
	m.state.WorkerPID, m.state.WorkerProcStart, m.state.WorkerStrongStart = worker.cmd.Process.Pid, info.Start, info.StrongStart
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		stopGrokManagedProcess(worker, 2*time.Second)
		return err
	}
	if err := m.prepareSharedToolRootLedger(); err != nil {
		stopGrokManagedProcess(worker, 2*time.Second)
		return fmt.Errorf("prepare Qwen detached-tool ledger: %w", err)
	}
	client := newQwenACPClient(stdin, stdout)
	m.mu.Lock()
	m.client = client
	m.mu.Unlock()
	return nil
}

func qwenLaneWorkerEnvironment(environment []string, state qwenLaneState, capability string) []string {
	blocked := map[string]bool{
		peerSessionIDEnvironment: true, "AGENT_SESSIONS_PRODUCT": true,
		"AGENT_SESSIONS_QWEN_CAPABILITY": true, agentRuntimeDirEnvironment: true,
		remoteParentEnvironment: true, qwenLaneLaunchTokenEnv: true,
	}
	filtered := make([]string, 0, len(environment)+4)
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if !blocked[name] {
			filtered = append(filtered, entry)
		}
	}
	filtered = qwenprofile.ApplyEnvironment(filtered, state.Profile)
	return append(filtered,
		peerSessionIDEnvironment+"="+state.ThreadID,
		"AGENT_SESSIONS_PRODUCT=qwen",
		"AGENT_SESSIONS_QWEN_CAPABILITY="+capability,
		agentRuntimeDirEnvironment+"="+laneAgentRuntimeDir(),
	)
}

func (m *qwenLaneManager) initializeACP(ctx context.Context) error { //nolint:gocyclo // Bootstrap validates the exact protocol, identity, native UUID, mode, and MCP injection.
	m.mu.Lock()
	client, worker := m.client, m.worker
	m.mu.Unlock()
	if client == nil || worker == nil {
		return errors.New("qwen ACP worker is unavailable")
	}
	initialized, err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize Qwen ACP worker: %w", err)
	}
	agent := mapValue(initialized["agentInfo"])
	capabilities := mapValue(initialized["agentCapabilities"])
	sessions := mapValue(capabilities["sessionCapabilities"])
	mcp := mapValue(capabilities["mcpCapabilities"])
	loadSession, _ := capabilities["loadSession"].(bool)
	if intValue(initialized["protocolVersion"]) != 1 || stringValue(agent["name"]) != "qwen-code" ||
		!qwenreadiness.VersionAtLeast(stringValue(agent["version"]), qwenreadiness.MinimumVersion) ||
		!loadSession || sessions["list"] == nil || sessions["resume"] == nil || len(mcp) == 0 {
		return errors.New("qwen ACP initialize response lacks the admitted identity or capabilities")
	}
	mcpServers, err := m.qwenMCPServers()
	if err != nil {
		return err
	}
	params := map[string]any{"cwd": m.state.Cwd, "mcpServers": mcpServers}
	method := "session/new"
	if m.state.QwenSessionID != "" {
		method = "session/resume"
		params["sessionId"] = m.state.QwenSessionID
	}
	opened, err := client.request(ctx, method, params)
	if err != nil {
		return fmt.Errorf("open Qwen ACP session: %w", err)
	}
	returned := stringValue(opened["sessionId"])
	if method == "session/new" && !validSessionID(returned) {
		return errors.New("qwen ACP returned no valid native session identity")
	}
	if method == "session/resume" && returned != "" && returned != m.state.QwenSessionID {
		return errors.New("qwen ACP resumed a different native session identity")
	}
	mode := qwenACPMode(opened)
	if mode == "" {
		return errors.New("qwen ACP did not corroborate its current native mode")
	}
	if m.state.RequestedInitialMode != "" && mode != m.state.RequestedInitialMode {
		return fmt.Errorf("qwen ACP initial mode %q does not match requested %q", mode, m.state.RequestedInitialMode)
	}
	m.mu.Lock()
	if returned != "" {
		m.state.QwenSessionID = returned
	}
	m.state.InitialNativeMode, m.state.CurrentNativeMode = mode, mode
	for index := range m.state.Turns {
		if m.state.Turns[index].QwenSessionID == "" {
			m.state.Turns[index].QwenSessionID = m.state.QwenSessionID
		}
	}
	err = m.persistLocked()
	m.mu.Unlock()
	if err == nil {
		client.setNotificationHandler(m.handleACPNotification)
	}
	return err
}

func (m *qwenLaneManager) qwenMCPServers() ([]any, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("resolve Qwen lane MCP runtime")
	}
	env := []any{
		map[string]any{"name": peerSessionIDEnvironment, "value": m.state.ThreadID},
		map[string]any{"name": "AGENT_SESSIONS_PRODUCT", "value": "qwen"},
		map[string]any{"name": "AGENT_SESSIONS_QWEN_CAPABILITY", "value": m.launchToken},
		map[string]any{"name": agentRuntimeDirEnvironment, "value": laneAgentRuntimeDir()},
	}
	return []any{map[string]any{
		"name": "agent_sessions", "command": executable, "args": []any{"mcp"}, "env": env,
	}}, nil
}

func qwenACPMode(result map[string]any) string {
	modes := mapValue(result["modes"])
	return stringValue(modes["currentModeId"])
}

func (m *qwenLaneManager) handleACPNotification(message map[string]any) {
	method := stringValue(message["method"])
	if method != "session/update" && method != "qwen/notify/session/mode-update" {
		return
	}
	params := mapValue(message["params"])
	m.mu.Lock()
	defer m.mu.Unlock()
	if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != m.state.QwenSessionID {
		return
	}
	if method == "qwen/notify/session/mode-update" {
		if mode := stringValue(params["currentModeId"]); mode != "" {
			m.state.CurrentNativeMode = mode
			_ = m.persistLocked()
		}
		return
	}
	update := mapValue(params["update"])
	switch stringValue(update["sessionUpdate"]) {
	case "current_mode_update":
		if mode := stringValue(update["currentModeId"]); mode != "" {
			m.state.CurrentNativeMode = mode
			_ = m.persistLocked()
		}
	case "agent_message_chunk":
		content := mapValue(update["content"])
		text := defaultString(stringValue(content["text"]), stringValue(update["text"]))
		if text != "" && m.activeTurnID != "" {
			m.activeAnswer.WriteString(text)
		}
	}
}

func (m *qwenLaneManager) signalTurn() {
	select {
	case m.turnNotify <- struct{}{}:
	default:
	}
}

func (m *qwenLaneManager) turnLoop() {
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

func (m *qwenLaneManager) executeNextTurn() bool {
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
	turn := &m.state.Turns[index]
	turn.Status, turn.StartedAt, turn.QwenSessionID = "active", now, m.state.QwenSessionID
	m.activeTurnID, m.state.ActiveTurnID = turn.ID, turn.ID
	m.activeAnswer.Reset()
	m.state.Status, m.state.AutoArchiveAt = "active", 0
	m.removePendingTurnLocked(turn.ID)
	if err := m.persistLocked(); err != nil {
		m.activeTurnID, m.state.ActiveTurnID = "", ""
		m.mu.Unlock()
		m.shutdown("persist active Qwen lane turn failed", false)
		return false
	}
	turnID, prompt, timeout, sessionID := turn.ID, turn.Prompt, time.Duration(turn.TimeoutMS)*time.Millisecond, m.state.QwenSessionID
	m.publishStatusLocked("busy")
	m.mu.Unlock()

	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	result, requestErr := m.client.request(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
	})
	cancel()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		_ = m.client.notifyRequest("session/cancel", map[string]any{"sessionId": sessionID})
	}

	m.mu.Lock()
	index = m.turnIndexLocked(turnID)
	if index < 0 || m.closing {
		m.activeTurnID, m.state.ActiveTurnID = "", ""
		m.mu.Unlock()
		return false
	}
	turn = &m.state.Turns[index]
	turn.Result, turn.CompletedAt = m.activeAnswer.String(), time.Now().UnixMilli()
	m.finishTurnLocked(turn, turnID, result, requestErr, ctx.Err())
	m.queueTerminalNoticeLocked(*turn)
	m.activeTurnID, m.state.ActiveTurnID = "", ""
	m.state.Status, m.state.TerminalOutcome, m.state.ExitCode = "idle", turn.Outcome, turn.Exit
	if !m.hasQueuedTurnLocked() && m.state.AutoArchive {
		m.state.AutoArchiveAt = time.Now().UnixMilli() + m.state.AutoArchiveDelayMS
	}
	if err := m.persistLocked(); err != nil {
		m.mu.Unlock()
		m.shutdown("persist terminal Qwen lane turn failed", false)
		return false
	}
	m.publishStatusLocked("idle")
	queued, notices := m.hasQueuedTurnLocked(), qwenLaneHasUnsentNotices(m.state)
	m.mu.Unlock()
	if notices {
		go m.flushTerminalNotices()
	}
	return queued
}

func (m *qwenLaneManager) finishTurnLocked(turn *qwenLaneTurn, turnID string, result map[string]any, requestErr, contextErr error) {
	switch {
	case m.interruptedID == turnID:
		turn.Status, turn.Outcome, turn.Exit = "interrupted", "interrupted", 130
		m.interruptedID = ""
	case errors.Is(contextErr, context.DeadlineExceeded):
		turn.Status, turn.Outcome, turn.Exit, turn.Error = "timed_out", "timed_out", 124, "turn deadline exceeded"
	case requestErr != nil:
		turn.Status, turn.Outcome, turn.Exit, turn.Error = "failed", "failed", 1, "Qwen ACP prompt failed"
	default:
		turn.Status, turn.Outcome, turn.Exit = "completed", "completed", 0
		turn.StopReason = stringValue(result["stopReason"])
		if turn.StopReason != "" && turn.StopReason != "end_turn" && turn.StopReason != "stop_sequence" {
			turn.Outcome = turn.StopReason
		}
	}
	turn.TerminalRevision = sessionKey(fmt.Sprintf("qwen-terminal\x00%s\x00%d\x00%s", turn.ID, turn.CompletedAt, turn.Outcome))
}

func (m *qwenLaneManager) turnIndexLocked(turnID string) int {
	for index := range m.state.Turns {
		if m.state.Turns[index].ID == turnID {
			return index
		}
	}
	return -1
}

func (m *qwenLaneManager) removePendingTurnLocked(turnID string) {
	remaining := m.state.PendingTurnIDs[:0]
	for _, pending := range m.state.PendingTurnIDs {
		if pending != turnID {
			remaining = append(remaining, pending)
		}
	}
	m.state.PendingTurnIDs = remaining
}

func (m *qwenLaneManager) hasQueuedTurnLocked() bool {
	for _, turn := range m.state.Turns {
		if turn.Status == "queued" {
			return true
		}
	}
	return false
}

func (m *qwenLaneManager) publishStatusLocked(status string) {
	if m.peer == nil {
		return
	}
	m.peer.mu.Lock()
	m.peer.permissionMode = defaultString(m.state.CurrentNativeMode, "unknown")
	m.peer.applyStatusLocked(status)
	_ = m.peer.writeRecordsLocked()
	m.peer.mu.Unlock()
}

func (m *qwenLaneManager) acceptLoop(listener net.Listener) {
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

func (m *qwenLaneManager) handleControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	if !scanner.Scan() {
		return
	}
	var request map[string]any
	if json.Unmarshal(scanner.Bytes(), &request) != nil {
		writeClaudeLaneControlResponse(conn, nil, errors.New("invalid Qwen lane control request"))
		return
	}
	response, err := m.handleControl(request)
	archive := err == nil && stringValue(request["action"]) == "archive"
	if archive {
		m.beginShutdown("explicit archive", true)
	}
	writeClaudeLaneControlResponse(conn, response, err)
	if archive {
		go m.shutdown("explicit archive", true)
	}
}

func (m *qwenLaneManager) handleControl(request map[string]any) (map[string]any, error) { //nolint:gocyclo // Each control verb has an explicit durable transition.
	if stringValue(request["sessionId"]) != m.state.ThreadID {
		return nil, errors.New("qwen lane control session mismatch")
	}
	action := stringValue(request["action"])
	if containsString([]string{"status", "wake", "wake_status"}, action) &&
		subtle.ConstantTimeCompare([]byte(stringValue(request["launchToken"])), []byte(m.launchToken)) != 1 {
		return nil, errors.New("qwen lane launch token mismatch")
	}
	switch action {
	case "status":
		m.mu.Lock()
		defer m.mu.Unlock()
		return map[string]any{
			"sessionId": m.state.ThreadID, "ready": m.peer != nil, "loaded": validSessionID(m.state.QwenSessionID),
			"permissionMode": defaultString(m.state.CurrentNativeMode, "unknown"), "permissionAuthority": "native_qwen",
		}, nil
	case "wake":
		return m.queueWake(mapValue(request["item"]))
	case "wake_status":
		return m.wakeStatus(stringValue(request["messageId"]))
	case "resume":
		body, _ := json.Marshal(request["turn"])
		var turn qwenLaneTurn
		if json.Unmarshal(body, &turn) != nil || !validSessionID(turn.ID) || strings.TrimSpace(turn.Prompt) == "" || turn.Status != "queued" || turn.Collected {
			return nil, errors.New("invalid Qwen lane resume turn")
		}
		persistent, _ := request["persistent"].(bool)
		if err := validateLaneOwner(persistent, intValue(request["ownerPid"]), stringValue(request["ownerProcStart"])); err != nil {
			return nil, err
		}
		groupsBody, _ := json.Marshal(request["groups"])
		explicitBody, _ := json.Marshal(request["explicitGroups"])
		var groups, explicit []string
		if json.Unmarshal(groupsBody, &groups) != nil || json.Unmarshal(explicitBody, &explicit) != nil {
			return nil, errors.New("invalid Qwen lane group state")
		}
		m.mu.Lock()
		if m.peer == nil || m.closing || m.state.Status != "idle" || m.activeTurnID != "" || m.hasQueuedTurnLocked() {
			m.mu.Unlock()
			return nil, errors.New("qwen lane is not idle")
		}
		if debt := firstQwenLaneDebt(m.state); debt != "" {
			m.mu.Unlock()
			return nil, fmt.Errorf("collect outstanding Qwen lane turn %s before resume", debt)
		}
		client, nativeSessionID := m.client, m.state.QwenSessionID
		m.mu.Unlock()
		requestedMode := stringValue(request["requestedInitialMode"])
		if requestedMode != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, modeErr := client.request(ctx, "session/set_mode", map[string]any{"sessionId": nativeSessionID, "modeId": requestedMode})
			cancel()
			if modeErr != nil {
				return nil, fmt.Errorf("set Qwen native mode for follow-up: %w", modeErr)
			}
		}
		m.mu.Lock()
		if m.peer == nil || m.closing || m.state.Status != "idle" || m.activeTurnID != "" || m.hasQueuedTurnLocked() {
			m.mu.Unlock()
			return nil, errors.New("qwen lane changed state during resume")
		}
		m.state.Persistent = persistent
		if persistent {
			m.state.OwnerPID, m.state.OwnerProcStart, m.state.OwnerSessionID = 0, "", ""
		} else {
			m.state.OwnerPID, m.state.OwnerProcStart = intValue(request["ownerPid"]), stringValue(request["ownerProcStart"])
			m.state.OwnerSessionID = stringValue(request["ownerSessionId"])
		}
		if notifySet, _ := request["notifySet"].(bool); notifySet {
			m.state.NotifyTarget = stringValue(request["notifyTarget"])
		}
		if autoArchive, ok := request["autoArchive"].(bool); ok {
			m.state.AutoArchive = autoArchive
			if !autoArchive {
				m.state.AutoArchiveAt = 0
			}
		}
		if delay := int64Value(request["autoArchiveDelayMs"]); delay > 0 {
			m.state.AutoArchiveDelayMS = delay
		}
		m.state.Groups, m.state.ExplicitGroups = groups, explicit
		m.state.ParentSessionID, m.state.ParentHostID = stringValue(request["parentSessionId"]), stringValue(request["parentHostId"])
		m.state.ParentAgentRuntimeDir = stringValue(request["parentAgentRuntimeDir"])
		m.state.InheritParentGroups, _ = request["inheritParentGroups"].(bool)
		if requestedMode != "" {
			m.state.RequestedInitialMode, m.state.CurrentNativeMode = requestedMode, requestedMode
			m.state.LaunchPreference = stringValue(request["launchPreference"])
		}
		m.state.Turns = append(m.state.Turns, turn)
		m.state.PendingTurnIDs = append(m.state.PendingTurnIDs, turn.ID)
		m.state.LatestTurnID, m.state.AutoArchiveAt = turn.ID, 0
		err := m.persistLocked()
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		m.signalTurn()
		return map[string]any{"accepted": true, "turnId": turn.ID}, nil
	case "ack":
		turnID := stringValue(request["turnId"])
		m.mu.Lock()
		defer m.mu.Unlock()
		index := m.turnIndexLocked(turnID)
		if index < 0 || !containsString([]string{"completed", "failed", "interrupted", "timed_out"}, m.state.Turns[index].Status) {
			return nil, errors.New("qwen lane can acknowledge only a terminal turn")
		}
		m.state.Turns[index].Collected, m.state.Turns[index].CollectedAt = true, time.Now().UnixMilli()
		m.state.CollectedTurnID = turnID
		m.cancelTerminalNoticeLocked(turnID)
		if err := m.persistLocked(); err != nil {
			return nil, err
		}
		return map[string]any{"acknowledged": true, "turnId": turnID}, nil
	case "interrupt":
		m.mu.Lock()
		turnID, client, sessionID := m.activeTurnID, m.client, m.state.QwenSessionID
		if turnID == "" || client == nil {
			m.mu.Unlock()
			return nil, errors.New("qwen lane has no active turn")
		}
		m.interruptedID = turnID
		m.mu.Unlock()
		if err := client.notifyRequest("session/cancel", map[string]any{"sessionId": sessionID}); err != nil {
			return nil, err
		}
		return map[string]any{"turnId": turnID}, nil
	case "archive":
		return map[string]any{"archiving": true}, nil
	default:
		return nil, fmt.Errorf("unknown Qwen lane control action %q", action)
	}
}

func (m *qwenLaneManager) queueWake(item map[string]any) (map[string]any, error) {
	messageID, message := stringValue(item["id"]), strings.TrimSpace(stringValue(item["message"]))
	if messageID == "" || message == "" {
		return nil, errors.New("qwen lane wake requires a message id and body")
	}
	fingerprint := wakeItemFingerprint(item)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, turn := range m.state.Turns {
		if turn.MessageID != messageID {
			continue
		}
		if turn.Fingerprint != fingerprint {
			return map[string]any{"delivery": "conflict", "messageId": messageID}, nil
		}
		return qwenLaneWakeResult(turn), nil
	}
	turn := newQwenLaneTurn(peerMessageText(item), 0)
	turn.MessageID, turn.Fingerprint = messageID, fingerprint
	if m.closing || m.state.Status == "archived" {
		turn.Status, turn.Outcome, turn.Exit, turn.Error, turn.CompletedAt = "interrupted", "interrupted", 130, "Qwen lane is closing", time.Now().UnixMilli()
	}
	m.state.Turns = append(m.state.Turns, turn)
	m.state.LatestTurnID = turn.ID
	if turn.Status == "queued" {
		m.state.PendingTurnIDs = append(m.state.PendingTurnIDs, turn.ID)
		m.state.AutoArchiveAt = 0
	}
	if err := m.persistLocked(); err != nil {
		return nil, err
	}
	if turn.Status == "queued" {
		m.signalTurn()
	}
	return qwenLaneWakeResult(turn), nil
}

func qwenLaneWakeResult(turn qwenLaneTurn) map[string]any {
	delivery := "queued"
	switch turn.Status {
	case "active":
		delivery = "started"
	case "completed":
		delivery = "delivered"
	case "failed", "interrupted", "timed_out":
		delivery = turn.Status
	}
	return map[string]any{
		"delivery": delivery, "messageId": turn.MessageID, "turnId": turn.ID,
		"status": turn.Status, "outcome": emptyStringAsNil(turn.Outcome), "exit": turn.Exit,
	}
}

func (m *qwenLaneManager) wakeStatus(messageID string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, turn := range m.state.Turns {
		if turn.MessageID == messageID {
			return qwenLaneWakeResult(turn), nil
		}
	}
	return nil, errors.New("qwen lane wake is not owned by this manager")
}

func (m *qwenLaneManager) maintenanceLoop() {
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
			notices := qwenLaneHasUnsentNotices(m.state)
			m.mu.Unlock()
			if ownerDead {
				m.shutdown("lifecycle owner exited", true)
				return
			}
			if autoArchive {
				m.shutdown("auto-archive delay elapsed", true)
				return
			}
			if notices {
				go m.tryFlushTerminalNotices()
			}
		}
	}
}

func cloneQwenLaneState(state qwenLaneState) qwenLaneState {
	state.Turns = append([]qwenLaneTurn(nil), state.Turns...)
	state.PendingTurnIDs = append([]string(nil), state.PendingTurnIDs...)
	state.Notices = append([]claudeLaneNotice(nil), state.Notices...)
	state.CleanupDebt = append([]qwenLaneDebt(nil), state.CleanupDebt...)
	return state
}

func (m *qwenLaneManager) queueTerminalNoticeLocked(turn qwenLaneTurn) {
	queueQwenLaneTerminalNotice(&m.state, turn)
}

func queueQwenLaneTerminalNotice(state *qwenLaneState, turn qwenLaneTurn) {
	state.Notices = appendLaneTerminalNotice(
		state.Notices, "qwen", state.Name, state.ThreadID, turn.ID, turn.Status, turn.Outcome, turn.Exit,
		state.NotifyTarget, state.ParentHostID, state.ParentAgentRuntimeDir, state.Groups,
	)
}

func cancelAllQwenLaneNotices(state *qwenLaneState) {
	now := time.Now().UnixMilli()
	for index := range state.Notices {
		if state.Notices[index].SentAt == 0 {
			state.Notices[index].SentAt = now
		}
	}
}

func (m *qwenLaneManager) cancelTerminalNoticeLocked(turnID string) {
	for index := range m.state.Notices {
		if m.state.Notices[index].TurnID == turnID && m.state.Notices[index].SentAt == 0 {
			m.state.Notices[index].SentAt = time.Now().UnixMilli()
		}
	}
}

func qwenLaneHasUnsentNotices(state qwenLaneState) bool {
	for _, notice := range state.Notices {
		if notice.SentAt == 0 {
			return true
		}
	}
	return false
}

func flushOrphanQwenLaneNotices(paths nativePaths, threadID string) {
	noticeLock, err := lockLaneStateFile(paths, "qwen-notices-"+threadID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(noticeLock)
	state, err := readQwenLaneState(paths, threadID)
	if err != nil {
		return
	}
	for _, notice := range state.Notices {
		if notice.SentAt != 0 {
			continue
		}
		if deliverGroupedLaneNotice(state.ThreadID, notice.Target, notice.ID, notice.Message) != nil {
			return
		}
		stateLock, lockErr := lockLaneStateFile(paths, "qwen-"+threadID)
		if lockErr != nil {
			return
		}
		latest, readErr := readQwenLaneState(paths, threadID)
		if readErr != nil {
			unlockLaneStateFile(stateLock)
			return
		}
		for index := range latest.Notices {
			if latest.Notices[index].ID == notice.ID && latest.Notices[index].SentAt == 0 {
				latest.Notices[index].SentAt = time.Now().UnixMilli()
			}
		}
		writeErr := writeQwenLaneStateUnlocked(paths, latest)
		unlockLaneStateFile(stateLock)
		if writeErr != nil {
			return
		}
		state = latest
	}
}

func (m *qwenLaneManager) flushTerminalNotices() {
	m.noticeMu.Lock()
	defer m.noticeMu.Unlock()
	m.flushTerminalNoticesLocked()
}

func (m *qwenLaneManager) tryFlushTerminalNotices() {
	if !m.noticeMu.TryLock() {
		return
	}
	defer m.noticeMu.Unlock()
	m.flushTerminalNoticesLocked()
}

func (m *qwenLaneManager) flushTerminalNoticesLocked() {
	for {
		m.mu.Lock()
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
		notice, snapshot := m.state.Notices[index], cloneQwenLaneState(m.state)
		m.state.Notices[index].Attempts++
		m.state.Notices[index].LastAttemptAt = time.Now().UnixMilli()
		if m.persistLocked() != nil {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		err := deliverGroupedLaneNotice(snapshot.ThreadID, notice.Target, notice.ID, notice.Message)
		m.mu.Lock()
		if err == nil {
			for current := range m.state.Notices {
				if m.state.Notices[current].ID == notice.ID {
					m.state.Notices[current].SentAt = time.Now().UnixMilli()
				}
			}
		}
		persistErr := m.persistLocked()
		m.mu.Unlock()
		if err != nil || persistErr != nil {
			return
		}
	}
}

func (m *qwenLaneManager) persistLocked() error {
	return writeQwenLaneState(m.paths, m.state)
}

func (m *qwenLaneManager) shutdown(reason string, interrupted bool) { //nolint:gocyclo // Shutdown closes admission, terminalizes debt, retires exact worker, and archives native state.
	startupDone := m.beginShutdown(reason, interrupted)
	if startupDone != nil {
		<-startupDone
	}
	m.cleanupOnce.Do(func() {
		m.mu.Lock()
		if m.shutdownCause != "" {
			reason, interrupted = m.shutdownCause, m.shutdownAs130
		}
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

		m.mu.Lock()
		now := time.Now().UnixMilli()
		status, outcome, exit := "failed", "failed", 1
		if interrupted {
			status, outcome, exit = "interrupted", "interrupted", 130
		}
		for index := range m.state.Turns {
			turn := &m.state.Turns[index]
			if containsString([]string{"queued", "active"}, turn.Status) {
				turn.Status, turn.Outcome, turn.Exit, turn.Error, turn.CompletedAt = status, outcome, exit, reason, now
				m.queueTerminalNoticeLocked(*turn)
			}
		}
		m.state.Status, m.state.ActiveTurnID, m.state.PendingTurnIDs, m.state.AutoArchiveAt = "retiring", "", nil, 0
		_ = m.persistLocked()
		client, worker, sessionID := m.client, m.worker, m.state.QwenSessionID
		cleanupSnapshot := cloneQwenLaneState(m.state)
		m.mu.Unlock()
		registry, registryErr := lockQwenToolRegistry(cleanupSnapshot)
		if registry != nil {
			defer registry.close()
		}
		if interrupted && client != nil && sessionID != "" {
			_ = client.notifyRequest("session/cancel", map[string]any{"sessionId": sessionID})
		}
		if client != nil {
			_ = client.close()
		}
		stopGrokManagedProcess(worker, 3*time.Second)
		if registryErr == nil && registry != nil && registry.ledger != nil {
			registryErr = registry.ledger.reconcileCleanup()
		}
		if registryErr == nil && registry != nil {
			registryErr = registry.removeArtifacts()
		}
		m.mu.Lock()
		m.state.WorkerPID, m.state.WorkerProcStart, m.state.WorkerStrongStart = 0, "", ""
		m.state.MessagingSocket = ""
		_ = m.persistLocked()
		archiveState := cloneQwenLaneState(m.state)
		m.mu.Unlock()
		if !qwenLaneHasUnsentNotices(archiveState) || reason == "explicit archive" {
			// Explicit collection controls notice debt; best-effort delivery is retried while live.
		} else {
			m.flushTerminalNotices()
		}
		archiveErr := registryErr
		if archiveErr == nil {
			archiveErr = completeQwenLaneArchive(m.paths, archiveState, reason)
		} else {
			archiveState.Status = "cleanup_debt"
			archiveState.CleanupDebt = []qwenLaneDebt{{Operation: "tool_root_cleanup", Error: archiveErr.Error(), Attempts: 1, UpdatedAt: time.Now().UnixMilli()}}
			_ = writeQwenLaneState(m.paths, archiveState)
		}
		if archiveErr != nil {
			fmt.Fprintf(os.Stderr, "qwen-lane-manager: archive failed: %v\n", archiveErr)
		}
		if m.lifecycleLock != nil {
			unlockLaneLifecycle(m.lifecycleLock)
			m.lifecycleLock = nil
		}
		_ = os.Remove(m.state.ControlSocket)
		close(m.done)
	})
}
