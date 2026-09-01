package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/fileutil"
	"github.com/antst/agent-sessions/internal/sessionkey"
	"github.com/antst/agent-sessions/internal/sessiontools"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const maxFrameBytes = 1024 * 1024

type daemon struct {
	mu               sync.Mutex
	sessionID        string
	attachmentID     string
	cwd              string
	name             string
	nameSource       string
	permissionMode   string
	agentRuntimeDir  string
	agentManaged     bool
	status           string
	entrypoint       string
	supervisorSocket string
	supervisorToken  string
	ownerPID         int
	ownerProcStart   string
	procStart        string
	startedAt        int64
	heartbeat        time.Duration
	backendSocket    string
	stableSocket     string
	registryFile     string
	stateFile        string
	inboxDir         string
	pendingDir       string
	sessionIndex     string
	qwenHome         string
	qwenTitlePath    string
	qwenTitleOffset  int64
	listener         net.Listener
	handlerWG        sync.WaitGroup
	admissionClosed  bool
	connections      map[net.Conn]struct{}
	seen             map[string]struct{}
	seenOrder        []string
	stopped          bool
	// maintenanceBeforeWrite is a test seam for terminal-write ordering.
	maintenanceBeforeWrite func()
	handleBeforeFrame      func()
	messageQueued          func(map[string]any)
	registrationOverride   func(federator.PeerRegistration) federator.PeerRegistration
	closeOnce              sync.Once
	done                   chan struct{}
}

type envelope struct {
	From, FromSession, FromName, FromProduct, FromMode, MessageID, SentAt, Message string
}

// Main dispatches one role of the agent-sessions executable.
//
//nolint:gocyclo // Runtime role dispatch is intentionally centralized.
func Main() {
	if isQwenToolWrapperInvocation() {
		os.Exit(runQwenToolWrapper(os.Args[1:]))
	}
	if isGrokToolWrapperInvocation() {
		os.Exit(runGrokToolWrapper(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "agent-sessions requires bootstrap, shim, supervisor, appserver, lane, claude-lane, claude-lane-manager, grok-lane, grok-lane-manager, qwen-lane, qwen-lane-manager, grok, grok-host, grok-plugin-verify, qwen-host, qwen-plugin-install, qwen-plugin-remove, release-package, release-evidence, hook, mcp, grok-mcp, or launch")
		os.Exit(2)
	}
	if code, handled := runNativeLaneRole(os.Args[1], os.Args[2:]); handled {
		os.Exit(code)
	}
	switch os.Args[1] {
	case "shim":
		runShimMain(os.Args[2:])
	case "supervisor":
		os.Exit(runSupervisorCommand(os.Args[2:]))
	case "appserver":
		os.Exit(runAppServerCommand(os.Args[2:]))
	case "claude-lane-manager":
		os.Exit(runClaudeLaneManager(os.Args[2:]))
	case "grok-lane-manager":
		os.Exit(runGrokLaneManager(os.Args[2:]))
	case "qwen-lane-manager":
		os.Exit(runQwenLaneManager(os.Args[2:]))
	case "grok":
		os.Exit(runGrokSafetyCommand(os.Args[2:]))
	case "grok-host":
		os.Exit(runGrokHostCommand(os.Args[2:]))
	case "grok-plugin-verify":
		os.Exit(runGrokPluginVerify(os.Args[2:]))
	case "qwen-plugin-install":
		os.Exit(runQwenPluginInstall(os.Args[2:]))
	case "qwen-plugin-remove":
		os.Exit(runQwenPluginRemove(os.Args[2:]))
	case "qwen-host":
		os.Exit(runQwenHostCommand(os.Args[2:]))
	case "release-package":
		os.Exit(runReleasePackage(os.Args[2:]))
	case "release-evidence":
		os.Exit(runReleaseEvidence(os.Args[2:]))
	case "hook":
		runHookCommand()
	case "mcp":
		os.Exit(runMCPCommand())
	case "grok-mcp":
		os.Exit(runGrokMCPCommand())
	case "launch":
		os.Exit(runLaunchCommand(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "agent-sessions: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runNativeLaneRole(role string, args []string) (int, bool) {
	product, known := bridgeProductByLaneRole(role)
	if !known {
		return 0, false
	}
	switch product.descriptor.ID {
	case "codex":
		return runLaneCommand(args), true
	case "claude":
		return runClaudeLaneCommand(args), true
	case "grok":
		return runGrokLaneCommand(args), true
	case "qwen":
		return runQwenLaneCommand(args), true
	default:
		return 0, false
	}
}

func runShimMain(argv []string) {
	args := parseArgs(argv)
	if args["session-id"] == "" || args["cwd"] == "" {
		fmt.Fprintln(os.Stderr, "agent-sessions shim requires --session-id and --cwd")
		os.Exit(2)
	}
	d := newDaemon(args)
	if err := d.start(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions native shim: %v\n", err)
		os.Exit(1)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	select {
	case <-signals:
	case <-d.done:
	}
	d.shutdown()
}

func parseArgs(argv []string) map[string]string {
	result := map[string]string{}
	for i := 0; i < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "--") || i+1 >= len(argv) {
			continue
		}
		result[strings.TrimPrefix(argv[i], "--")] = argv[i+1]
		i++
	}
	return result
}

func newDaemon(args map[string]string) *daemon {
	pid := os.Getpid()
	sessionID := args["session-id"]
	dataDir := args["data-dir"]
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "state", "agent-sessions")
	}
	claudeDir := args["claude-config-dir"]
	if claudeDir == "" {
		home, _ := os.UserHomeDir()
		claudeDir = filepath.Join(home, ".claude")
	}
	codexHome := args["codex-home"]
	if codexHome == "" {
		home, _ := os.UserHomeDir()
		codexHome = filepath.Join(home, ".codex")
	}
	runtimeDir := args["runtime-dir"]
	if runtimeDir == "" {
		runtimeDir = os.TempDir()
	}
	uid := os.Getuid()
	runtimeRoot := bridgeRuntimeRoot(runtimeDir, uid)
	key := sessionKey(sessionID)
	cwd := args["cwd"]
	heartbeat := 10 * time.Second
	if value, err := strconv.Atoi(args["heartbeat-ms"]); err == nil && value > 0 {
		heartbeat = time.Duration(max(value, 50)) * time.Millisecond
	}
	ownerPID, _ := strconv.Atoi(args["owner-pid"])
	entrypoint := defaultString(args["entrypoint"], "codex")
	name := args["name"]
	if name == "" {
		name = fmt.Sprintf("%s-%s-%s", entrypoint, sanitizeName(filepath.Base(cwd)), first8(sessionID))
	}
	nameSource := args["name-source"]
	if nameSource == "" {
		nameSource = "generated"
	}
	status := args["status"]
	switch status {
	case "busy", "shell", "waiting":
	default:
		status = "idle"
	}
	agentRuntimeDir := strings.TrimSpace(args["agent-runtime-dir"])
	if agentRuntimeDir == "" {
		agentRuntimeDir = strings.TrimSpace(os.Getenv("AGENT_SESSIONS_AGENT_RUNTIME_DIR"))
	}
	stableSocket := filepath.Join(runtimeRoot, fmt.Sprintf("session-%s.sock", key))
	return &daemon{
		sessionID: sessionID, attachmentID: strings.TrimSpace(args["attachment-id"]),
		cwd: cwd, name: sanitizeName(name), nameSource: nameSource,
		permissionMode: defaultString(args["permission-mode"], "default"), status: status,
		entrypoint:      entrypoint,
		agentRuntimeDir: agentRuntimeDir, agentManaged: agentRuntimeDir != "",
		supervisorSocket: args["supervisor-socket"], supervisorToken: args["supervisor-token"], ownerPID: ownerPID,
		ownerProcStart: args["owner-proc-start"], procStart: readProcStart(pid),
		startedAt: time.Now().UnixMilli(), heartbeat: heartbeat,
		// New adapters bind the stable session path directly. backendSocket is
		// retained in the state schema as the exact listener path so current
		// cleanup can also migrate the older PID-socket + symlink shape.
		backendSocket: stableSocket,
		stableSocket:  stableSocket,
		registryFile:  filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid)),
		stateFile:     filepath.Join(dataDir, "sessions", key, "state.json"),
		inboxDir:      filepath.Join(dataDir, "sessions", key, "inbox"),
		pendingDir:    filepath.Join(dataDir, "sessions", key, "inbox", "pending"),
		sessionIndex:  filepath.Join(codexHome, "session_index.jsonl"),
		qwenHome:      strings.TrimSpace(args["qwen-home"]),
		seen:          map[string]struct{}{}, done: make(chan struct{}),
		connections: map[net.Conn]struct{}{},
	}
}

func bridgeRuntimeRoot(runtimeDir string, uid int) string {
	runtimeRoot := filepath.Join(runtimeDir, fmt.Sprintf("agent-sessions-%d", uid))
	compactRoot := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_COMPACT_RUNTIME_DIR"))
	if compactRoot == "" {
		compactRoot = filepath.Join("/tmp", fmt.Sprintf("agent-sessions-runtime-%d", uid))
	}
	// The stable session address is longer than supervisor.sock, so it is the
	// representative budget for every socket below this root.
	return socketpath.PreferRoot(runtimeRoot, compactRoot, "session-"+strings.Repeat("0", 20)+".sock")
}

// ensurePrivateRuntimeDir establishes the trust boundary for every UDS and
// stable alias the bridge creates.  In particular, MkdirAll alone is unsafe
// for the predictable /tmp fallback: another user may have created the leaf
// first and retained authority to replace its contents.
func ensurePrivateRuntimeDir(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create private runtime directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect private runtime directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refuse unsafe runtime path %s: not a real directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("refuse unsafe runtime directory %s: owner is not uid %d", path, os.Geteuid())
	}
	if info.Mode().Perm() != 0700 {
		if err := os.Chmod(path, 0700); err != nil { //nolint:gosec // 0700 is required for a private Unix-socket directory.
			return fmt.Errorf("secure runtime directory %s: %w", path, err)
		}
		info, err = os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return fmt.Errorf("runtime directory %s did not retain mode 0700", path)
		}
	}
	return nil
}

func (d *daemon) start() error {
	if err := ensurePrivateRuntimeDir(filepath.Dir(d.backendSocket)); err != nil {
		return err
	}
	cleanupStaleStateFile(d.stateFile, filepath.Dir(d.backendSocket), filepath.Dir(d.registryFile))
	listener, err := listenPrivateSessionSocket(d.stableSocket)
	if err != nil {
		return err
	}
	d.listener = listener
	d.mu.Lock()
	d.refreshNameLocked()
	err = d.writeRecordsLocked()
	d.mu.Unlock()
	if err != nil {
		d.shutdown()
		return err
	}
	go d.acceptLoop()
	go d.maintenanceLoop()
	return nil
}

func listenPrivateSessionSocket(path string) (net.Listener, error) {
	if err := socketpath.Validate(path); err != nil {
		return nil, fmt.Errorf("validate session delivery socket: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("session delivery socket already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect session delivery socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("session delivery endpoint is not a real Unix socket")
	}
	return listener, nil
}

func (d *daemon) acceptLoop() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.done:
				return
			default:
				continue
			}
		}
		d.mu.Lock()
		if d.admissionClosed {
			d.mu.Unlock()
			_ = conn.Close()
			continue
		}
		d.handlerWG.Add(1)
		d.connections[conn] = struct{}{}
		d.mu.Unlock()
		go func() {
			defer func() {
				d.mu.Lock()
				delete(d.connections, conn)
				d.mu.Unlock()
				d.handlerWG.Done()
			}()
			d.handleConnection(conn)
		}()
	}
}

func (d *daemon) handleConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame map[string]any
		if json.Unmarshal(line, &frame) == nil {
			if stringValue(frame["type"]) == "control" && stringValue(frame["action"]) == "inspect" {
				d.writeInspection(conn)
				continue
			}
			if d.handleBeforeFrame != nil {
				d.handleBeforeFrame()
			}
			d.handleFrame(frame)
		}
	}
}

// writeInspection exposes the live daemon identity and lifecycle fields needed
// to acknowledge a mutation. Federators use it to corroborate a Grok registry
// row without trusting mutable registry content or stale TUI argv after an
// in-session mode change.
func (d *daemon) writeInspection(conn net.Conn) {
	d.mu.Lock()
	response := map[string]any{
		"type": "peer_inspection", "pid": os.Getpid(), "procStart": d.procStart,
		"sessionId": d.sessionID, "entrypoint": d.entrypoint, "permissionMode": d.permissionMode,
		"status": d.status, "cwd": d.cwd, "name": d.name, "nameSource": d.nameSource,
	}
	d.mu.Unlock()
	body, err := json.Marshal(response)
	if err == nil {
		_, _ = conn.Write(append(body, '\n'))
	}
}

func (d *daemon) handleFrame(frame map[string]any) {
	switch stringValue(frame["type"]) {
	case "user":
		d.handleUser(frame)
	case "control":
		d.handleControl(frame)
	}
}

//nolint:gocyclo // Native carrier decoding keeps ordered compatibility checks in one admission boundary.
func (d *daemon) handleUser(frame map[string]any) {
	if target := stringValue(frame["session_id"]); target != "" && target != d.sessionID {
		return
	}
	message, _ := frame["message"].(map[string]any)
	content := stringValue(message["content"])
	if content == "" {
		return
	}
	var grouped *federator.AgentFrame
	if decoded, err := federator.DecodeAgentFrameBody(content); err == nil &&
		decoded.Version == federator.AgentFrameVersion && decoded.Type == "delivery" &&
		decoded.MessageID != "" && decoded.Source != nil && decoded.Content != "" {
		grouped = &decoded
		content = decoded.Content
	}
	id := stringValue(frame["msg_id"])
	if id == "" {
		id = stringValue(frame["uuid"])
	}
	if grouped != nil {
		id = grouped.MessageID
	}
	if id == "" {
		// Retried transports without an explicit id must still converge on one
		// durable wake record across shim replacement.
		id = "derived-" + sessionKey(defaultString(stringValue(frame["from"]), "unknown")+"\x00"+content)
	}
	d.mu.Lock()
	if _, exists := d.seen[id]; exists {
		d.mu.Unlock()
		return
	}
	d.seen[id] = struct{}{}
	d.seenOrder = append(d.seenOrder, id)
	if len(d.seenOrder) > 500 {
		delete(d.seen, d.seenOrder[0])
		d.seenOrder = d.seenOrder[1:]
	}
	supervisor := d.supervisorSocket
	d.mu.Unlock()
	env := parsePeerMessage(content)
	item := map[string]any{
		"type": "message", "id": defaultString(env.MessageID, id), "transportMessageId": id,
		"receivedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"from":       defaultString(stringValue(frame["from"]), defaultString(env.From, "unknown")),
		"message":    defaultString(env.Message, content),
	}
	if grouped != nil {
		item["id"] = grouped.MessageID
		item["from"] = grouped.Source.ID
		item["message"] = grouped.Content
		item["sentAt"] = grouped.SentAt
		item["fromName"] = grouped.Source.Name
		item["fromSession"] = grouped.Source.SessionID
		item["fromProduct"] = grouped.Source.Entrypoint
		item["fromMode"] = grouped.Source.PermissionMode
		item["summary"] = grouped.Summary
	}
	putIf(item, "sentAt", env.SentAt)
	putIf(item, "fromName", env.FromName)
	putIf(item, "fromSession", env.FromSession)
	putIf(item, "fromProduct", env.FromProduct)
	putIf(item, "fromMode", env.FromMode)
	if supervisor != "" && d.supervisorOwnsWake(supervisor, item) {
		return
	}
	if err := d.enqueue(item); err == nil && d.messageQueued != nil {
		d.messageQueued(item)
	}
}

func (d *daemon) supervisorOwnsWake(supervisor string, item map[string]any) bool {
	response, err := requestControl(supervisor, map[string]any{
		"action": "wake", "sessionId": d.sessionID, "launchToken": d.supervisorToken, "item": item,
	}, 30*time.Second)
	ownedStates := []string{"accepted", "in_flight", "actor_accepted", "delivered", "queueing", "queued", "started", "steered", "observed", "conflict", "failed", "interrupted", "timed_out"}
	if err == nil && containsString(ownedStates, stringValue(response["delivery"])) {
		return true
	}
	var rejected *controlResponseError
	if errors.As(err, &rejected) {
		// The supervisor rejected the request before claiming durable ownership.
		return false
	}
	// A timed-out control call is ambiguous. Re-query and idempotently retry
	// the same message id until the supervisor proves durable ownership or
	// becomes unreachable; never create two delivery paths.
	deadline := time.Now().Add(5 * time.Second)
	for err != nil && time.Now().Before(deadline) && probeUnixSocket(supervisor, 300*time.Millisecond) {
		status, statusErr := requestControl(supervisor, map[string]any{
			"action": "wake_status", "sessionId": d.sessionID, "launchToken": d.supervisorToken,
			"messageId": stringValue(item["id"]),
		}, 2*time.Second)
		if statusErr == nil && containsString([]string{"in_flight", "actor_accepted", "delivered", "queueing", "queued", "fallback_delivered", "failed", "interrupted", "timed_out"}, stringValue(status["delivery"])) {
			return true
		}
		response, err = requestControl(supervisor, map[string]any{
			"action": "wake", "sessionId": d.sessionID, "launchToken": d.supervisorToken, "item": item,
		}, 2*time.Second)
		if err == nil && stringValue(response["delivery"]) != "" {
			return true
		}
		if errors.As(err, &rejected) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A live supervisor plus an unanswered mutating request is ambiguous, so
	// keep the durable path authoritative. Falling back here is the original
	// double-delivery race. If the supervisor actually died, the inbox is the
	// only remaining delivery path and is safe.
	return probeUnixSocket(supervisor, 300*time.Millisecond)
}

func (d *daemon) handleControl(frame map[string]any) {
	action := stringValue(frame["action"])
	if action == "permission_mode" {
		mode := strings.TrimSpace(stringValue(frame["permissionMode"]))
		if mode == "" {
			return
		}
		d.mu.Lock()
		d.permissionMode = mode
		_ = d.writeRecordsLocked()
		d.mu.Unlock()
		return
	}
	if action == "shutdown" {
		d.closeOnce.Do(func() { close(d.done) })
		return
	}
	if action == "peer_message_status" {
		_ = d.enqueue(map[string]any{
			"type": "delivery-status", "id": randomID(), "receivedAt": time.Now().UTC().Format(time.RFC3339Nano),
			"from": defaultString(stringValue(frame["from"]), "unknown"), "originalMessageId": frame["orig_msg_id"],
			"status": frame["status"], "reason": frame["reason"],
		})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch action {
	case "update":
		if value := stringValue(frame["cwd"]); value != "" {
			d.cwd = value
		}
		if value := stringValue(frame["supervisorSocket"]); value != "" {
			d.supervisorSocket = value
		}
		d.applyNameLocked(stringValue(frame["name"]), stringValue(frame["nameSource"]))
		if value := stringValue(frame["permissionMode"]); value != "" {
			d.permissionMode = value
		}
		d.applyStatusLocked(stringValue(frame["status"]))
	case "rename":
		if value := stringValue(frame["name"]); value != "" {
			d.name = sanitizeName(value)
			d.nameSource = "manual"
		}
	case "status":
		d.applyStatusLocked(stringValue(frame["status"]))
	default:
		return
	}
	_ = d.writeRecordsLocked()
}

func (d *daemon) applyNameLocked(value, source string) {
	if value == "" {
		return
	}
	var allowed bool
	switch source {
	case "canonical", "launch", "lane":
		allowed = true
	case "explicit":
		allowed = d.nameSource != "manual"
	case "codex":
		allowed = d.nameSource != "explicit" && d.nameSource != "launch" && d.nameSource != "lane" &&
			d.nameSource != "canonical" && d.nameSource != "manual"
	default:
		allowed = d.nameSource == "generated"
	}
	if allowed {
		d.name = sanitizeName(value)
		d.nameSource = defaultString(source, d.nameSource)
	}
}

func (d *daemon) applyStatusLocked(value string) {
	if value == "busy" || value == "shell" || value == "idle" || value == "waiting" {
		d.status = value
	}
}

func (d *daemon) maintenanceLoop() {
	ticker := time.NewTicker(d.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ownerStatus := cleanupProcessIdentityStatus(d.ownerPID, d.ownerProcStart).Status
			if ownerStatus == processIdentityStale {
				d.closeOnce.Do(func() { close(d.done) })
				return
			}
			if d.maintenanceBeforeWrite != nil {
				d.maintenanceBeforeWrite()
			}
			d.mu.Lock()
			d.refreshNameLocked()
			_ = d.writeRecordsLocked()
			d.mu.Unlock()
		case <-d.done:
			return
		}
	}
}

func (d *daemon) refreshNameLocked() {
	if d.entrypoint == "qwen" {
		d.refreshQwenNameLocked()
		return
	}
	if d.entrypoint != "codex" {
		return
	}
	if d.nameSource == "lane" || d.nameSource == "manual" {
		return
	}
	file, err := os.Open(d.sessionIndex)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	latest := ""
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil || stringValue(row["id"]) != d.sessionID {
			continue
		}
		for _, key := range []string{"thread_name", "title", "name"} {
			if value := stringValue(row[key]); value != "" {
				latest = value
				break
			}
		}
	}
	if latest != "" && sanitizeName(latest) != d.name {
		d.name = sanitizeName(latest)
		d.nameSource = "codex"
	}
}

//nolint:gocyclo // Incremental transcript identity and title parsing fail closed at each boundary.
func (d *daemon) refreshQwenNameLocked() {
	if d.qwenHome == "" || d.nameSource == "lane" || d.nameSource == "manual" {
		return
	}
	if d.qwenTitlePath == "" {
		path, ok := qwenTranscriptPath(d.qwenHome, d.sessionID)
		if !ok {
			return
		}
		d.qwenTitlePath = path
	}
	file, err := os.Open(d.qwenTitlePath)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if d.qwenTitleOffset < 0 || d.qwenTitleOffset > info.Size() {
		d.qwenTitleOffset = 0
	}
	if _, err := file.Seek(d.qwenTitleOffset, io.SeekStart); err != nil {
		return
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	offset := d.qwenTitleOffset
	latest := ""
	first := offset == 0
	for scanner.Scan() {
		line := scanner.Bytes()
		offset += int64(len(line) + 1)
		var event struct {
			SessionID     string `json:"sessionId"`
			Cwd           string `json:"cwd"`
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			SystemPayload struct {
				CustomTitle string `json:"customTitle"`
				TitleSource string `json:"titleSource"`
			} `json:"systemPayload"`
		}
		if json.Unmarshal(line, &event) != nil {
			return
		}
		if first {
			first = false
			if event.SessionID != d.sessionID || filepath.Clean(event.Cwd) != filepath.Clean(d.cwd) {
				return
			}
		}
		if event.SessionID != d.sessionID {
			return
		}
		if event.Type == "system" && event.Subtype == "custom_title" {
			value := strings.TrimSpace(event.SystemPayload.CustomTitle)
			source := strings.TrimSpace(event.SystemPayload.TitleSource)
			if value == "" || len(value) > 512 {
				return
			}
			if source != "" && source != "manual" {
				continue
			}
			latest = value
		}
	}
	if scanner.Err() != nil || offset > info.Size() {
		return
	}
	d.qwenTitleOffset = offset
	if latest != "" && sanitizeName(latest) != d.name {
		d.name = sanitizeName(latest)
		d.nameSource = "qwen"
	}
}

func qwenTranscriptPath(home, sessionID string) (string, bool) {
	if !filepath.IsAbs(home) || !validSessionID(sessionID) {
		return "", false
	}
	projects := filepath.Join(home, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return "", false
	}
	selected := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(projects, entry.Name(), "chats", sessionID+".jsonl")
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || selected != "" {
			return "", false
		}
		selected = path
	}
	return selected, selected != ""
}

// QwenNativeSessionTitle returns the latest manual native title for one exact
// transcript. Automatic model-generated titles deliberately do not rename a
// managed peer.
func QwenNativeSessionTitle(home, sessionID, cwd string) (string, bool) {
	path, ok := qwenTranscriptPath(home, sessionID)
	if !ok {
		return "", false
	}
	file, err := os.Open(path) //nolint:gosec // exact UUID below the selected Qwen profile.
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	first, latest := true, ""
	for scanner.Scan() {
		var event struct {
			SessionID     string `json:"sessionId"`
			Cwd           string `json:"cwd"`
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			SystemPayload struct {
				CustomTitle string `json:"customTitle"`
				TitleSource string `json:"titleSource"`
			} `json:"systemPayload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.SessionID != sessionID {
			return "", false
		}
		if first {
			first = false
			if filepath.Clean(event.Cwd) != filepath.Clean(cwd) {
				return "", false
			}
		}
		if event.Type == "system" && event.Subtype == "custom_title" &&
			strings.TrimSpace(event.SystemPayload.TitleSource) != "auto" {
			latest = strings.TrimSpace(event.SystemPayload.CustomTitle)
			if latest == "" || len(latest) > 512 {
				return "", false
			}
		}
	}
	return latest, scanner.Err() == nil
}

func (d *daemon) writeRecordsLocked() error {
	if d.stopped {
		return errors.New("peer daemon is stopping")
	}
	now := time.Now().UnixMilli()
	state := map[string]any{
		"pid": os.Getpid(), "procStart": d.procStart, "ownerPid": d.ownerPID, "ownerProcStart": d.ownerProcStart,
		"sessionId": d.sessionID, "cwd": d.cwd, "name": d.name, "nameSource": d.nameSource,
		"permissionMode": d.permissionMode, "socketPath": d.stableSocket, "backendSocketPath": d.backendSocket,
		"registryFile": d.registryFile, "inboxDir": d.inboxDir, "startedAt": d.startedAt,
		"status": d.status, "entrypoint": d.entrypoint, "supervisorSocket": d.supervisorSocket,
		"agentRuntimeDir": d.agentRuntimeDir, "groupProtocol": map[bool]int{true: federator.GroupProtocolVersion, false: 0}[d.agentManaged],
		"updatedAt": now,
	}
	registry := map[string]any{
		"pid": os.Getpid(), "sessionId": d.sessionID, "cwd": d.cwd, "startedAt": d.startedAt,
		"procStart": d.procStart, "version": "agent-sessions/0.1.0", "peerProtocol": 1,
		"kind": "interactive", "entrypoint": d.entrypoint, "name": d.name, "nameSource": d.nameSource,
		"status": d.status, "permissionMode": d.permissionMode, "updatedAt": now, "statusUpdatedAt": now,
		"messagingSocketPath": d.stableSocket, "bridgeSessionId": nil,
	}
	if d.attachmentID != "" && d.attachmentID != d.sessionID {
		state["attachmentId"] = d.attachmentID
		registry["attachmentId"] = d.attachmentID
	}
	if err := writeJSONAtomic(d.stateFile, state); err != nil {
		return err
	}
	if d.agentManaged {
		_, err := federator.RegisterPeer(d.agentRuntimeDir, d.agentRegistrationLocked())
		return err
	}
	return writeJSONAtomic(d.registryFile, registry)
}

func (d *daemon) agentRegistrationLocked() federator.PeerRegistration {
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: d.sessionID, AttachmentID: d.attachmentID, Product: d.entrypoint,
		Name: d.name, Status: d.status, PermissionMode: d.permissionMode, Cwd: d.cwd,
		PID: os.Getpid(), ProcStart: d.procStart, Socket: d.stableSocket,
		// The per-session shim is the communication parent's lifetime. Its
		// supervisor may host many unrelated sessions and must not keep children
		// alive after this one is retired.
		LifecyclePID: os.Getpid(), LifecycleProcStart: d.procStart, StartedAt: d.startedAt,
	}
	if d.registrationOverride != nil {
		registration = d.registrationOverride(registration)
	}
	return registration
}

func (d *daemon) enqueue(item map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return errors.New("peer daemon is stopping")
	}
	if err := os.MkdirAll(d.pendingDir, 0700); err != nil {
		return err
	}
	id := safeID(stringValue(item["id"]))
	if id == "" {
		id = randomID()
	}
	entries, _ := os.ReadDir(d.pendingDir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-"+id+".json") {
			return nil
		}
	}
	file := filepath.Join(d.pendingDir, fmt.Sprintf("%016d-%s.json", time.Now().UnixMilli(), id))
	if err := writeJSONAtomic(file, item); err != nil {
		return err
	}
	entries, _ = os.ReadDir(d.pendingDir)
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names[:max(0, len(names)-50)] {
		_ = os.Remove(filepath.Join(d.pendingDir, name))
	}
	return nil
}

func (d *daemon) shutdown() {
	d.closeOnce.Do(func() { close(d.done) })
	// Stop admitting frames, then drain every frame whose transport write may
	// already have succeeded. This gives each accepted user message exactly one
	// chance to reach its supervisor or durable inbox before teardown removes
	// either path.
	d.mu.Lock()
	d.admissionClosed = true
	listener := d.listener
	connections := make([]net.Conn, 0, len(d.connections))
	for conn := range d.connections {
		connections = append(connections, conn)
	}
	d.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	// Existing streams may already contain more newline-delimited frames. Give
	// their handlers a short bounded drain window; after it, closing the stream
	// makes any later sender write observably fail instead of being silently
	// accepted after teardown.
	for _, conn := range connections {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	}
	d.handlerWG.Wait()
	// Serialize the terminal state with every heartbeat and control update.
	// A heartbeat may already have won its ticker select when done closes; the
	// stopped latch prevents that writer from recreating state after removal.
	d.mu.Lock()
	d.stopped = true
	registration := d.agentRegistrationLocked()
	removeJSONIf(d.registryFile, func(row map[string]any) bool {
		return intValue(row["pid"]) == os.Getpid() && stringValue(row["sessionId"]) == d.sessionID
	})
	removeJSONIf(d.stateFile, func(row map[string]any) bool {
		return intValue(row["pid"]) == os.Getpid() && stringValue(row["sessionId"]) == d.sessionID
	})
	d.mu.Unlock()
	if d.agentManaged {
		_ = federator.UnregisterPeer(d.agentRuntimeDir, registration)
	}
	if target, err := os.Readlink(d.stableSocket); err == nil {
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(d.stableSocket), resolved)
		}
		resolved, _ = filepath.Abs(resolved)
		backend, _ := filepath.Abs(d.backendSocket)
		if resolved == backend {
			_ = os.Remove(d.stableSocket)
		}
	}
	_ = os.Remove(d.backendSocket)
}

type controlResponseError struct {
	message string
}

func (e *controlResponseError) Error() string { return e.message }

func requestControl(socket string, request map[string]any, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	body, _ := json.Marshal(request)
	if _, err = conn.Write(append(body, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(ioLimitReader{r: conn, n: maxFrameBytes}).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var response map[string]any
	if err = json.Unmarshal(line, &response); err != nil {
		return nil, err
	}
	if ok, _ := response["ok"].(bool); !ok {
		return nil, &controlResponseError{message: defaultString(stringValue(response["error"]), "local control failed")}
	}
	return response, nil
}

type ioLimitReader struct {
	r net.Conn
	n int
}

func (l ioLimitReader) Read(p []byte) (int, error) {
	if len(p) > l.n {
		p = p[:l.n]
	}
	return l.r.Read(p)
}

var envelopeRE = regexp.MustCompile(`(?s)^<cross-session-message([^>]*)>\n(.*)\n</cross-session-message>$`)
var attributeRE = regexp.MustCompile(`^\s+([a-z][a-z0-9-]*)="([^"<>\n\r]*)"`)
var escapedCloseRE = regexp.MustCompile(`(?i)<\\/cross-session-message`)

func parsePeerMessage(content string) envelope {
	match := envelopeRE.FindStringSubmatch(content)
	if match == nil {
		return envelope{Message: content}
	}
	attrs := map[string]string{}
	rest := match[1]
	for rest != "" {
		part := attributeRE.FindStringSubmatch(rest)
		if part == nil {
			return envelope{Message: content}
		}
		attrs[part[1]] = part[2]
		rest = rest[len(part[0]):]
	}
	body := match[2]
	metadata := map[string]any{}
	const prefix = "[codex-peer-metadata: "
	if strings.HasPrefix(body, prefix) {
		line, tail, found := strings.Cut(body, "\n")
		if !found {
			tail = ""
		}
		if strings.HasSuffix(line, "]") && json.Unmarshal([]byte(line[len(prefix):len(line)-1]), &metadata) == nil {
			body = tail
		}
	}
	product := defaultString(attrs["from-product"], stringValue(metadata["fromProduct"]))
	if _, ok := bridgeProductByID(product); !ok {
		product = ""
	}
	mode := attrs["from-mode"]
	if mode != "bypass" && mode != "prompting" {
		mode = ""
	}
	return envelope{
		From: attrs["from"], FromSession: attrs["from-session"], FromName: attrs["from-name"],
		FromProduct: product, FromMode: mode, MessageID: defaultString(attrs["message-id"], stringValue(metadata["messageId"])),
		SentAt: defaultString(attrs["sent-at"], stringValue(metadata["sentAt"])), Message: escapedCloseRE.ReplaceAllString(body, "</cross-session-message"),
	}
}

func writeJSONAtomic(file string, value any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return fileutil.WriteJSONAtomic(file, value)
}

func removeJSONIf(file string, predicate func(map[string]any) bool) {
	body, err := os.ReadFile(file) //nolint:gosec // callers supply fixed bridge-owned state and registry paths.
	if err != nil {
		return
	}
	var row map[string]any
	if json.Unmarshal(body, &row) == nil && predicate(row) {
		_ = os.Remove(file)
	}
}

func sanitizeName(value string) string {
	var out []rune
	separator := false
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '.' || r == '_' || r == '-' {
			out = append(out, r)
			separator = false
		} else if !separator {
			out = append(out, '-')
			separator = true
		}
		if len(out) >= 80 {
			break
		}
	}
	result := strings.Trim(string(out), "._-")
	if result == "" {
		return "codex"
	}
	return result
}

// NormalizePeerName applies the stable public peer-address normalization used
// by launchers and structured rename_session calls.
func NormalizePeerName(value string) string { return sessiontools.NormalizePeerName(value) }

func sessionKey(id string) string {
	return sessionkey.FromID(id)
}
func safeID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 80 {
			break
		}
	}
	return b.String()
}
func randomID() string {
	body := make([]byte, 16)
	_, _ = rand.Read(body)
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	s := hex.EncodeToString(body)
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[:8], s[8:12], s[12:16], s[16:20], s[20:])
}
func readProcStart(pid int) string {
	started, _ := captureProcessStart(pid)
	return started
}
func first8(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func stringValue(value any) string { text, _ := value.(string); return text }
func boolValue(value any) bool     { result, _ := value.(bool); return result }
func stringSlice(value any) []string {
	if values, ok := value.([]string); ok {
		return append([]string(nil), values...)
	}
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
func putIf(target map[string]any, key, value string) {
	if value != "" {
		target[key] = value
	}
}
