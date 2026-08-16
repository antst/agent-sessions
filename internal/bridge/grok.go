package bridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	grokLaunchTokenEnv    = "AGENT_SESSIONS_GROK_LAUNCH_TOKEN"
	grokSessionIDEnv      = "AGENT_SESSIONS_GROK_SESSION_ID"
	grokACPStartupTimeout = 15 * time.Second
	// The outer control budget must exceed the inner ACP startup/refresh
	// budget so a cold reconnect can return its authoritative result.
	grokControlTimeout      = grokACPStartupTimeout + 5*time.Second
	grokACPInterjectTimeout = 30 * time.Second
	grokStatusRetryDelay    = 25 * time.Millisecond
	grokStatusRetryMax      = 250 * time.Millisecond
)

// grokHostConfig describes one process-attested interactive Grok launch.  A
// host never adopts Grok's default leader: every launch gets a new private
// directory, leader, ACP bridge, and control socket.
type grokHostConfig struct {
	GrokBin        string
	SessionID      string
	Cwd            string
	OwnerPID       int
	OwnerProcStart string
	LaunchToken    string
	RuntimeDir     string
	Name           string
	PermissionMode string
	readyWriter    io.Writer

	// command is overridden only by tests. Production always executes GrokBin.
	command func(args ...string) *exec.Cmd
}

type grokHostPaths struct {
	Root          string
	LaunchDir     string
	LeaderSocket  string
	ControlSocket string
}

// grokLaunchRecord is the durable attestation shared by the launcher host,
// Grok-owned MCP processes, and lane ownership inference. TokenHash is an
// identifier; the raw launch capability exists only in process environments.
type grokLaunchRecord struct {
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name"`
	PermissionMode  string `json:"permissionMode"`
	TokenHash       string `json:"tokenHash"`
	OwnerPID        int    `json:"ownerPid"`
	OwnerProcStart  string `json:"ownerProcStart"`
	HostPID         int    `json:"hostPid"`
	HostProcStart   string `json:"hostProcStart"`
	LeaderPID       int    `json:"leaderPid"`
	LeaderProcStart string `json:"leaderProcStart"`
	WakerPID        int    `json:"wakerPid,omitempty"`
	WakerProcStart  string `json:"wakerProcStart,omitempty"`
	RuntimeDir      string `json:"runtimeDir"`
	LeaderSocket    string `json:"leaderSocket"`
	ControlSocket   string `json:"controlSocket"`
	StartedAt       int64  `json:"startedAt"`
}

// GrokHostSocketPaths returns the compact, per-launch paths shared by the
// launcher and host. launchToken is never placed on disk verbatim.
func GrokHostSocketPaths(runtimeDir, launchToken string) (leader, control string, err error) {
	if !validGrokLaunchToken(launchToken) {
		return "", "", errors.New("invalid Grok launch token")
	}
	paths := grokRuntimePaths(runtimeDir, os.Getuid(), launchToken)
	return paths.LeaderSocket, paths.ControlSocket, nil
}

func grokRuntimePaths(runtimeDir string, uid int, launchToken string) grokHostPaths {
	return grokRuntimePathsForKey(runtimeDir, uid, sessionKey(launchToken))
}

func grokRuntimePathsForKey(runtimeDir string, uid int, launchKey string) grokHostPaths {
	root := filepath.Join(runtimeDir, fmt.Sprintf("agent-sessions-grok-%d", uid))
	// Darwin has a substantially shorter sockaddr_un budget. Prefer the caller's
	// runtime directory, but fall back to the literal /tmp spelling before any
	// socket is created. The root is still protected by ensurePrivateRuntimeDir.
	longest := filepath.Join(root, "g-"+strings.Repeat("0", 20), "control.sock")
	if len(longest) > 92 {
		root = filepath.Join("/tmp", fmt.Sprintf("asg-%d", uid))
	}
	launchDir := filepath.Join(root, "g-"+launchKey)
	return grokHostPaths{
		Root:          root,
		LaunchDir:     launchDir,
		LeaderSocket:  filepath.Join(launchDir, "leader.sock"),
		ControlSocket: filepath.Join(launchDir, "control.sock"),
	}
}

func validGrokLaunchToken(value string) bool {
	if len(value) < 32 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func grokTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func grokLaunchRecordPath(paths nativePaths, sessionID string) string {
	return filepath.Join(profileDataRoot(paths), "grok-launches", sessionKey(sessionID)+".json")
}

func grokLaunchLockPath(paths nativePaths, sessionID string) string {
	return filepath.Join(profileDataRoot(paths), "grok-launch-locks", sessionKey(sessionID)+".lock")
}

func acquireGrokLaunchLease(paths nativePaths, sessionID string) (*os.File, error) {
	directory := filepath.Dir(grokLaunchLockPath(paths, sessionID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(grokLaunchLockPath(paths, sessionID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("grok session %s already has a launch host", sessionID)
	}
	return lock, nil
}

func claimGrokLaunchRecord(paths nativePaths, record grokLaunchRecord) error {
	path := grokLaunchRecordPath(paths, record.SessionID)
	old := readGrokLaunchRecord(path)
	if _, err := os.Lstat(path); err == nil && old == nil {
		return fmt.Errorf("refuse malformed existing Grok launch record for %s", record.SessionID)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect existing Grok launch record: %w", err)
	}
	if old != nil {
		owner := cleanupProcessIdentityStatus(old.OwnerPID, old.OwnerProcStart).Status
		host := cleanupProcessIdentityStatus(old.HostPID, old.HostProcStart).Status
		if owner != processIdentityStale || host != processIdentityStale {
			return fmt.Errorf("grok session %s is still owned by a live or unverifiable launch", record.SessionID)
		}
		cleanupStaleGrokLaunchPaths(*old)
	}
	return writeJSONAtomic(path, record)
}

func cleanupStaleGrokLaunchPaths(record grokLaunchRecord) {
	if record.TokenHash == "" || record.RuntimeDir == "" {
		return
	}
	if len(record.TokenHash) != 64 {
		return
	}
	expected := grokRuntimePathsForKey(record.RuntimeDir, os.Getuid(), record.TokenHash[:20])
	if !samePath(record.ControlSocket, expected.ControlSocket) || !samePath(record.LeaderSocket, expected.LeaderSocket) {
		return
	}
	stopStaleGrokProcess(record.WakerPID, record.WakerProcStart)
	stopStaleGrokProcess(record.LeaderPID, record.LeaderProcStart)
	_ = os.Remove(expected.ControlSocket)
	_ = os.Remove(expected.LeaderSocket)
	_ = os.Remove(filepath.Join(expected.LaunchDir, "leader.lock"))
	_ = os.Remove(expected.LaunchDir)
}

func readGrokLaunchRecord(path string) *grokLaunchRecord {
	body, err := os.ReadFile(path) //nolint:gosec // path is a fixed bridge-owned state location.
	if err != nil {
		return nil
	}
	var record grokLaunchRecord
	if json.Unmarshal(body, &record) != nil || !validSessionID(record.SessionID) || record.TokenHash == "" {
		return nil
	}
	return &record
}

// runGrokSafetyCommand exposes the read-only process inventory needed by the
// installer. It never terminates a TUI or leader: the user must exit the
// owning grok-peer normally so its host can clean up the exact process group.
func runGrokSafetyCommand(argv []string) int {
	if len(argv) != 1 || argv[0] != "stopped" {
		fmt.Fprintln(os.Stderr, "usage: agent-session-runtime grok stopped")
		return 2
	}
	live, err := activeGrokLaunchSessions(resolveNativePaths())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok stopped: %v\n", err)
		return 1
	}
	encoded, _ := json.Marshal(map[string]any{"stopped": len(live) == 0, "liveSessionIds": live})
	fmt.Println(string(encoded))
	if len(live) > 0 {
		return 3
	}
	return 0
}

func activeGrokLaunchSessions(paths nativePaths) ([]string, error) {
	directory := filepath.Join(profileDataRoot(paths), "grok-launches")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Grok launch inventory: %w", err)
	}
	live := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record := readGrokLaunchRecord(path)
		if record == nil {
			return nil, fmt.Errorf("refuse malformed Grok launch record: %s", path)
		}
		identities := []processIdentityObservation{
			cleanupProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart),
			cleanupProcessIdentityStatus(record.HostPID, record.HostProcStart),
			cleanupProcessIdentityStatus(record.LeaderPID, record.LeaderProcStart),
			cleanupProcessIdentityStatus(record.WakerPID, record.WakerProcStart),
		}
		for _, identity := range identities {
			if identity.Status != processIdentityStale {
				live = append(live, record.SessionID)
				break
			}
		}
	}
	sort.Strings(live)
	return live, nil
}

// liveGrokLaunchForSession returns only a fully resident and published launch.
// Registry data alone is discovery, never authority: all three owning process
// identities, the private paths, and the bridge state must corroborate it.
func liveGrokLaunchForSession(paths nativePaths, sessionID string) *grokLaunchRecord {
	if !validSessionID(sessionID) {
		return nil
	}
	record := readGrokLaunchRecord(grokLaunchRecordPath(paths, sessionID))
	if record == nil || record.SessionID != sessionID || len(record.TokenHash) != 64 {
		return nil
	}
	if !liveGrokProcessTree(*record) {
		return nil
	}
	expected := grokRuntimePathsForKey(record.RuntimeDir, os.Getuid(), record.TokenHash[:20])
	if !liveGrokControlPaths(*record, expected) {
		return nil
	}
	state, err := readOwnNativeState(paths, sessionID)
	if err != nil || !grokBridgeStateMatchesLaunch(state, *record) {
		return nil
	}
	return record
}

func liveGrokProcessTree(record grokLaunchRecord) bool {
	return exactProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart).Status == processIdentityMatches &&
		exactProcessIdentityStatus(record.HostPID, record.HostProcStart).Status == processIdentityMatches &&
		exactProcessIdentityStatus(record.LeaderPID, record.LeaderProcStart).Status == processIdentityMatches &&
		processHasAncestor(record.HostPID, record.OwnerPID) && processHasAncestor(record.LeaderPID, record.HostPID)
}

func liveGrokControlPaths(record grokLaunchRecord, expected grokHostPaths) bool {
	return samePath(record.LeaderSocket, expected.LeaderSocket) &&
		samePath(record.ControlSocket, expected.ControlSocket) && probeUnixSocket(record.ControlSocket, 250*time.Millisecond)
}

func grokBridgeStateMatchesLaunch(state map[string]any, record grokLaunchRecord) bool {
	return intValue(state["pid"]) == record.HostPID && stringValue(state["procStart"]) == record.HostProcStart &&
		stringValue(state["entrypoint"]) == "grok" && intValue(state["ownerPid"]) == record.OwnerPID &&
		stringValue(state["ownerProcStart"]) == record.OwnerProcStart &&
		samePath(stringValue(state["supervisorSocket"]), record.ControlSocket)
}

// attestGrokMCPCaller authorizes an MCP process only when it inherited the raw
// per-launch capability from this launch's private leader and is a descendant
// of that exact live leader. Neither environment value grants authority alone.
func attestGrokMCPCaller(paths nativePaths) (string, error) {
	token := strings.TrimSpace(os.Getenv(grokLaunchTokenEnv))
	sessionID := strings.TrimSpace(os.Getenv(grokSessionIDEnv))
	if !validGrokLaunchToken(token) || !validSessionID(sessionID) {
		return "", errors.New("grok MCP launch context is unavailable")
	}
	record := liveGrokLaunchForSession(paths, sessionID)
	if record == nil || subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(grokTokenHash(token))) != 1 {
		return "", errors.New("grok MCP launch token is not attested by a live host")
	}
	if !processHasAncestor(os.Getpid(), record.LeaderPID) {
		return "", errors.New("grok MCP process is not descended from the private launch leader")
	}
	if err := refreshGrokLaunchPermission(record, token); err != nil {
		return "", fmt.Errorf("refresh live Grok permission mode: %w", err)
	}
	return sessionID, nil
}

func refreshGrokLaunchPermission(record *grokLaunchRecord, token string) error {
	deadline := time.Now().Add(grokControlTimeout)
	retryDelay := grokStatusRetryDelay
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("timed out waiting for authoritative Grok permission mode")
		}
		response, err := requestControl(record.ControlSocket, map[string]any{
			"action": "status", "sessionId": record.SessionID, "launchToken": token,
		}, remaining)
		if err != nil {
			return err
		}
		if busy, _ := response["refreshBusy"].(bool); busy {
			timer := time.NewTimer(min(retryDelay, remaining))
			<-timer.C
			retryDelay = min(retryDelay*2, grokStatusRetryMax)
			continue
		}
		mode := stringValue(response["permissionMode"])
		if mode != "default" && mode != "bypassPermissions" {
			return errors.New("grok host returned no authoritative permission mode")
		}
		if deferred, _ := response["refreshDeferred"].(bool); deferred &&
			stringValue(response["permissionAuthority"]) != "active_interjection_snapshot" {
			return errors.New("grok host deferred permission refresh without an active interjection snapshot")
		}
		return nil
	}
}

// inferGrokParent turns an inherited launch capability into lane lifecycle
// ownership only when the candidate process is inside the exact live private
// leader tree. Environment variables identify a candidate launch; they do not
// authorize it without the persisted identities, registry state, and ancestry.
func inferGrokParent(paths nativePaths, startPID int) (laneOwner, bool) {
	token := strings.TrimSpace(os.Getenv(grokLaunchTokenEnv))
	sessionID := strings.TrimSpace(os.Getenv(grokSessionIDEnv))
	if !validGrokLaunchToken(token) || !validSessionID(sessionID) {
		return laneOwner{}, false
	}
	record := liveGrokLaunchForSession(paths, sessionID)
	if record == nil || subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(grokTokenHash(token))) != 1 ||
		!processHasAncestor(startPID, record.LeaderPID) {
		return laneOwner{}, false
	}
	if refreshGrokLaunchPermission(record, token) != nil {
		return laneOwner{}, false
	}
	record = liveGrokLaunchForSession(paths, sessionID)
	if record == nil {
		return laneOwner{}, false
	}
	return laneOwner{
		PID: record.OwnerPID, ProcStart: record.OwnerProcStart,
		SessionID: record.SessionID, PermissionMode: defaultString(record.PermissionMode, "default"),
	}, true
}

type grokManagedProcess struct {
	cmd       *exec.Cmd
	procStart string
	done      chan struct{}
	mu        sync.Mutex
	err       error
}

func startGrokManagedProcess(command *exec.Cmd) (*grokManagedProcess, error) {
	configureGrokChildProcess(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	procStart, err := captureProcessStart(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("capture Grok child identity: %w", err)
	}
	managed := &grokManagedProcess{cmd: command, procStart: procStart, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		managed.mu.Lock()
		managed.err = err
		managed.mu.Unlock()
		close(managed.done)
	}()
	return managed, nil
}

func (p *grokManagedProcess) waitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

type grokACPClient struct {
	process   *grokManagedProcess
	stdin     io.WriteCloser
	responses chan map[string]any
	readDone  chan struct{}
	readMu    sync.Mutex
	readErr   error
	requestMu sync.Mutex
	nextID    int64
}

type grokRPCError struct {
	Code    int
	Message string
}

func (e *grokRPCError) Error() string {
	if e.Code == 0 {
		return "Grok ACP: " + e.Message
	}
	return fmt.Sprintf("Grok ACP error %d: %s", e.Code, e.Message)
}

func newGrokACPClient(process *grokManagedProcess, stdin io.WriteCloser, stdout io.ReadCloser) *grokACPClient {
	client := &grokACPClient{
		process: process, stdin: stdin, responses: make(chan map[string]any, 32),
		readDone: make(chan struct{}),
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxFrameBytes)
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) != nil || message["id"] == nil {
				// session/load replay and live session/update notifications have no
				// request id. They are intentionally ignored by the waker.
				continue
			}
			select {
			case client.responses <- message:
			case <-client.readDone:
				return
			}
		}
		client.readMu.Lock()
		client.readErr = scanner.Err()
		client.readMu.Unlock()
		close(client.readDone)
	}()
	return client
}

func (c *grokACPClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write Grok ACP %s: %w", method, err)
	}
	for {
		select {
		case response := <-c.responses:
			if int64Value(response["id"]) != id {
				// A prior timed-out request may finish later. Never confuse it with
				// the current request.
				continue
			}
			if raw, ok := response["error"].(map[string]any); ok {
				return nil, &grokRPCError{Code: intValue(raw["code"]), Message: defaultString(stringValue(raw["message"]), "request rejected")}
			}
			result, _ := response["result"].(map[string]any)
			return result, nil
		case <-c.readDone:
			c.readMu.Lock()
			readErr := c.readErr
			c.readMu.Unlock()
			if readErr == nil {
				readErr = io.EOF
			}
			return nil, fmt.Errorf("read Grok ACP %s: %w", method, readErr)
		case <-ctx.Done():
			return nil, fmt.Errorf("grok ACP %s: %w", method, ctx.Err())
		}
	}
}

func (c *grokACPClient) close() {
	_ = c.stdin.Close()
	stopGrokManagedProcess(c.process, 2*time.Second)
}

type grokWakeRecord struct {
	SessionID   string         `json:"sessionId"`
	MessageID   string         `json:"messageId"`
	Fingerprint string         `json:"fingerprint"`
	Delivery    string         `json:"delivery"`
	Error       string         `json:"error,omitempty"`
	Item        map[string]any `json:"item"`
	UpdatedAt   int64          `json:"updatedAt"`
}

func grokWakeRecordPath(paths nativePaths, sessionID, messageID string) string {
	return filepath.Join(profileDataRoot(paths), "grok-wakes", sessionKey(sessionID), sessionKey(messageID)+".json")
}

type grokHost struct {
	config grokHostConfig
	paths  grokHostPaths

	listener net.Listener
	leader   *grokManagedProcess
	peer     *daemon
	peerMu   sync.Mutex
	record   grokLaunchRecord
	lease    *os.File
	modeMu   sync.RWMutex
	mode     string
	// activeInterjectionMode is authoritative only while this host is submitting an
	// injected x.ai/interject whose immediately preceding roster refresh
	// produced the snapshot. It must never turn arbitrary acpMu contention into
	// permission authority.
	activeInterjectionMode  string
	activeInterjectionValid bool

	// publishPermission is a test seam. Production writes both daemon records
	// through writeRecordsLocked.
	publishPermission func(*daemon) error

	acpMu sync.Mutex
	acp   *grokACPClient

	wakeMu     sync.Mutex
	wakes      map[string]*grokWakeRecord
	wakeQueue  []string
	wakeNotify chan struct{}

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newGrokHost(config grokHostConfig) (*grokHost, error) {
	if config.GrokBin == "" || !validSessionID(config.SessionID) || strings.TrimSpace(config.Cwd) == "" {
		return nil, errors.New("grok host requires grok-bin, valid session-id, and cwd")
	}
	if !validGrokLaunchToken(config.LaunchToken) {
		return nil, errors.New("grok host requires a strong per-launch token")
	}
	if config.OwnerPID <= 1 || config.OwnerProcStart == "" ||
		exactProcessIdentityStatus(config.OwnerPID, config.OwnerProcStart).Status != processIdentityMatches {
		return nil, errors.New("grok host owner process identity does not match")
	}
	absoluteCwd, err := filepath.Abs(config.Cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve Grok cwd: %w", err)
	}
	info, err := os.Stat(absoluteCwd)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("grok cwd is unavailable: %s", absoluteCwd)
	}
	config.Cwd = absoluteCwd
	if strings.TrimSpace(config.Name) == "" {
		config.Name = sanitizeName("grok-" + filepath.Base(absoluteCwd) + "-" + first8(config.SessionID))
	} else {
		config.Name = sanitizeName(config.Name)
	}
	if config.PermissionMode != "bypassPermissions" {
		config.PermissionMode = "default"
	}
	if config.RuntimeDir == "" {
		config.RuntimeDir = os.TempDir()
	}
	host := &grokHost{
		config: config,
		paths:  grokRuntimePaths(config.RuntimeDir, os.Getuid(), config.LaunchToken),
		mode:   config.PermissionMode,
		wakes:  make(map[string]*grokWakeRecord), wakeNotify: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	host.restoreWakeRecords()
	return host, nil
}

func (h *grokHost) restoreWakeRecords() {
	directory := filepath.Dir(grokWakeRecordPath(resolveNativePaths(), h.config.SessionID, "message"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // entry is inside the bridge-owned wake directory.
		var record grokWakeRecord
		if readErr != nil || json.Unmarshal(body, &record) != nil || record.SessionID != h.config.SessionID ||
			record.MessageID == "" || record.Fingerprint != wakeItemFingerprint(record.Item) {
			continue
		}
		h.wakes[record.MessageID] = &record
		if record.Delivery == "queued" {
			h.wakeQueue = append(h.wakeQueue, record.MessageID)
		}
	}
}

func (h *grokHost) persistWakeRecord(record *grokWakeRecord) error {
	if record == nil || record.SessionID != h.config.SessionID || record.MessageID == "" {
		return errors.New("invalid Grok wake record")
	}
	return writeJSONAtomic(grokWakeRecordPath(resolveNativePaths(), record.SessionID, record.MessageID), record)
}

func (h *grokHost) grokCommand(args ...string) *exec.Cmd {
	var command *exec.Cmd
	if h.config.command != nil {
		command = h.config.command(args...)
	} else {
		command = exec.Command(h.config.GrokBin, args...) //nolint:gosec // GrokBin is the launcher-selected CLI and argv is fixed by this host.
	}
	command.Env = append(os.Environ(),
		grokLaunchTokenEnv+"="+h.config.LaunchToken,
		grokSessionIDEnv+"="+h.config.SessionID,
	)
	return command
}

func (h *grokHost) run(ctx context.Context) error {
	if err := h.start(); err != nil {
		h.cleanup()
		return err
	}
	defer h.cleanup()
	if h.config.readyWriter != nil {
		_ = json.NewEncoder(h.config.readyWriter).Encode(map[string]any{
			"ready": true, "session_id": h.config.SessionID, "cwd": h.config.Cwd,
			"leader_socket": h.paths.LeaderSocket, "control_socket": h.paths.ControlSocket,
		})
	}
	ownerTicker := time.NewTicker(250 * time.Millisecond)
	defer ownerTicker.Stop()
	permissionTicker := time.NewTicker(time.Second)
	defer permissionTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.done:
			return nil
		case <-h.leader.done:
			return fmt.Errorf("private Grok leader exited: %w", h.leader.waitError())
		case <-ownerTicker.C:
			if cleanupProcessIdentityStatus(h.config.OwnerPID, h.config.OwnerProcStart).Status == processIdentityStale {
				return nil
			}
		case <-permissionTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), grokACPStartupTimeout)
			_ = h.ensureACP(ctx)
			cancel()
		}
	}
}

func (h *grokHost) start() error {
	paths := resolveNativePaths()
	lease, err := acquireGrokLaunchLease(paths, h.config.SessionID)
	if err != nil {
		return err
	}
	h.lease = lease
	if err := ensurePrivateRuntimeDir(h.paths.Root); err != nil {
		return err
	}
	h.record = grokLaunchRecord{
		SessionID: h.config.SessionID, Cwd: h.config.Cwd, Name: sanitizeName(h.config.Name),
		PermissionMode: h.currentPermissionMode(), TokenHash: grokTokenHash(h.config.LaunchToken),
		OwnerPID: h.config.OwnerPID, OwnerProcStart: h.config.OwnerProcStart,
		HostPID: os.Getpid(), HostProcStart: readProcStart(os.Getpid()),
		RuntimeDir: h.config.RuntimeDir, LeaderSocket: h.paths.LeaderSocket,
		ControlSocket: h.paths.ControlSocket, StartedAt: time.Now().UnixMilli(),
	}
	// Persist owner + host + token hash before either child starts. The raw
	// capability is never written to disk.
	if err := claimGrokLaunchRecord(paths, h.record); err != nil {
		return fmt.Errorf("persist Grok launch ownership: %w", err)
	}
	if err := os.Mkdir(h.paths.LaunchDir, 0o700); err != nil {
		return fmt.Errorf("create private Grok launch directory: %w", err)
	}
	leaderCommand := h.grokCommand(
		"--permission-mode", "default",
		"agent", "leader", "--leader-socket", h.paths.LeaderSocket,
		"--no-exit-on-disconnect", "--relay-on-demand", "--no-auto-update",
	)
	leaderCommand.Stdout, leaderCommand.Stderr = os.Stderr, os.Stderr
	leader, err := startGrokManagedProcess(leaderCommand)
	if err != nil {
		return fmt.Errorf("start private Grok leader: %w", err)
	}
	h.leader = leader
	h.record.LeaderPID, h.record.LeaderProcStart = leader.cmd.Process.Pid, leader.procStart
	if err := writeJSONAtomic(grokLaunchRecordPath(paths, h.record.SessionID), h.record); err != nil {
		return fmt.Errorf("persist private Grok leader ownership: %w", err)
	}
	if err := h.waitForLeaderSocket(10 * time.Second); err != nil {
		return err
	}
	listener, err := net.Listen("unix", h.paths.ControlSocket)
	if err != nil {
		return fmt.Errorf("listen on Grok control socket: %w", err)
	}
	h.listener = listener
	if err := os.Chmod(h.paths.ControlSocket, 0o600); err != nil {
		return fmt.Errorf("secure Grok control socket: %w", err)
	}
	h.wg.Add(3)
	go func() { defer h.wg.Done(); h.acceptLoop() }()
	go func() { defer h.wg.Done(); h.wakeLoop() }()
	go func() { defer h.wg.Done(); h.prepareLoop() }()
	return nil
}

func (h *grokHost) waitForLeaderSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-h.leader.done:
			return fmt.Errorf("private Grok leader exited before its socket was ready: %w", h.leader.waitError())
		default:
		}
		if info, err := os.Lstat(h.paths.LeaderSocket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("timed out waiting for private Grok leader socket")
}

func (h *grokHost) prepareLoop() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), grokACPStartupTimeout)
		err := h.ensureACP(ctx)
		cancel()
		if err == nil && h.ensurePeerPublished() == nil {
			return
		}
		select {
		case <-h.done:
			return
		case <-ticker.C:
		}
	}
}

func (h *grokHost) ensureACP(ctx context.Context) error {
	h.acpMu.Lock()
	defer h.acpMu.Unlock()
	return h.ensureACPReadyLocked(ctx)
}

func (h *grokHost) ensureACPReadyLocked(ctx context.Context) error {
	if err := h.ensureACPConnectedLocked(ctx); err != nil {
		return err
	}
	if err := h.refreshPermissionModeLocked(ctx); err != nil {
		return err
	}
	return h.ensureAgentSessionsMCPReadyLocked(ctx)
}

func (h *grokHost) ensureACPConnectedLocked(ctx context.Context) error {
	// This client observes and interjects into the TUI-owned resident actor; it
	// must never attach with session/load. A concurrent load can replace the
	// actor while the TUI is still creating its MCP process scope. The live
	// roster is the readiness/permission observation, and x.ai/interject targets
	// that resident actor without becoming a second session owner.
	if h.acp != nil {
		select {
		case <-h.acp.readDone:
			h.closeACPLocked()
		default:
		}
	}
	if h.acp == nil {
		command := h.grokCommand(
			"--no-auto-update", "--permission-mode", "default",
			"--leader-socket", h.paths.LeaderSocket,
			"agent", "--leader", "stdio",
		)
		stdin, err := command.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := command.StdoutPipe()
		if err != nil {
			return err
		}
		command.Stderr = os.Stderr
		process, err := startGrokManagedProcess(command)
		if err != nil {
			return fmt.Errorf("start official Grok ACP leader bridge: %w", err)
		}
		h.record.WakerPID, h.record.WakerProcStart = process.cmd.Process.Pid, process.procStart
		if err := writeJSONAtomic(grokLaunchRecordPath(resolveNativePaths(), h.record.SessionID), h.record); err != nil {
			stopGrokManagedProcess(process, 2*time.Second)
			return fmt.Errorf("persist Grok ACP waker ownership: %w", err)
		}
		h.acp = newGrokACPClient(process, stdin, stdout)
		result, err := h.acp.request(ctx, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
				"terminal": false,
			},
		})
		if err != nil {
			h.closeACPLocked()
			return err
		}
		if !grokAuthMethodAdvertised(result, "cached_token") {
			h.closeACPLocked()
			return errors.New("grok ACP did not advertise cached_token authentication")
		}
		if _, err := h.acp.request(ctx, "authenticate", map[string]any{
			"methodId": "cached_token", "_meta": map[string]any{"headless": true},
		}); err != nil {
			h.closeACPLocked()
			return fmt.Errorf("authenticate Grok ACP from cached CLI credentials: %w", err)
		}
	}
	return nil
}

func (h *grokHost) refreshPermissionModeLocked(ctx context.Context) error {
	result, err := h.acp.request(ctx, "_x.ai/sessions/list", map[string]any{
		"method": "x.ai/sessions/list", "params": map[string]any{},
	})
	if err != nil {
		return fmt.Errorf("query live Grok session roster: %w", err)
	}
	mode, err := grokRosterPermissionMode(result, h.config.SessionID)
	if err != nil {
		return err
	}
	// Every path that needs both locks takes peerMu before modeMu. Keep the
	// candidate dirty until the daemon state and registry both contain it; a
	// failed publication must be retried by the next roster refresh.
	h.peerMu.Lock()
	defer h.peerMu.Unlock()
	h.modeMu.Lock()
	defer h.modeMu.Unlock()
	if mode == h.mode {
		return nil
	}
	if peer := h.peer; peer != nil {
		peer.mu.Lock()
		previous := peer.permissionMode
		peer.permissionMode = mode
		publisher := h.publishPermission
		if publisher == nil {
			publisher = func(peer *daemon) error { return peer.writeRecordsLocked() }
		}
		err = publisher(peer)
		if err != nil {
			peer.permissionMode = previous
			peer.mu.Unlock()
			return fmt.Errorf("publish live Grok permission mode: %w", err)
		}
		peer.mu.Unlock()
	}
	nextRecord := h.record
	nextRecord.PermissionMode = mode
	if err := writeJSONAtomic(grokLaunchRecordPath(resolveNativePaths(), nextRecord.SessionID), nextRecord); err != nil {
		return fmt.Errorf("persist live Grok permission mode: %w", err)
	}
	h.mode = mode
	h.record = nextRecord
	return nil
}

func (h *grokHost) ensureAgentSessionsMCPReadyLocked(ctx context.Context) error {
	result, err := h.acp.request(ctx, "_x.ai/mcp/list", map[string]any{
		"method": "x.ai/mcp/list",
		"params": map[string]any{"sessionId": h.config.SessionID, "cache": true},
	})
	if err != nil {
		return fmt.Errorf("query live Grok MCP readiness: %w", err)
	}
	return grokAgentSessionsMCPReady(result)
}

func grokAgentSessionsMCPReady(response map[string]any) error {
	result, _ := response["result"].(map[string]any)
	servers, ok := result["servers"].([]any)
	if !ok {
		return errors.New("grok MCP response has no servers")
	}
	matches := 0
	ready := false
	for _, raw := range servers {
		server, _ := raw.(map[string]any)
		if stringValue(server["name"]) != "agent_sessions" {
			continue
		}
		matches++
		if stringValue(server["source"]) != "local" || stringValue(server["type"]) != "stdio" {
			continue
		}
		session, _ := server["session"].(map[string]any)
		enabled, enabledOK := session["enabled"].(bool)
		if !enabledOK || !enabled || stringValue(session["status"]) != "ready" {
			continue
		}
		tools, _ := session["tools"].([]any)
		for _, toolRaw := range tools {
			tool, _ := toolRaw.(map[string]any)
			toolEnabled, toolEnabledOK := tool["enabled"].(bool)
			if stringValue(tool["name"]) == "send_message" && toolEnabledOK && toolEnabled {
				ready = true
			}
		}
	}
	if matches != 1 {
		return fmt.Errorf("grok MCP response returned %d agent_sessions rows", matches)
	}
	if !ready {
		return errors.New("grok agent_sessions MCP is not ready with send_message enabled")
	}
	return nil
}

func (h *grokHost) currentPermissionMode() string {
	h.modeMu.RLock()
	defer h.modeMu.RUnlock()
	return defaultString(h.mode, "default")
}

func (h *grokHost) beginActiveInterjectionPermissionSnapshot() {
	h.modeMu.Lock()
	h.activeInterjectionMode = defaultString(h.mode, "default")
	h.activeInterjectionValid = true
	h.modeMu.Unlock()
}

func (h *grokHost) clearActiveInterjectionPermissionSnapshot() {
	h.modeMu.Lock()
	h.activeInterjectionMode = ""
	h.activeInterjectionValid = false
	h.modeMu.Unlock()
}

func (h *grokHost) activeInterjectionPermissionSnapshot() (string, bool) {
	h.modeMu.RLock()
	defer h.modeMu.RUnlock()
	if !h.activeInterjectionValid {
		return "", false
	}
	mode := h.activeInterjectionMode
	return mode, mode == "default" || mode == "bypassPermissions"
}

func grokRosterPermissionMode(response map[string]any, sessionID string) (string, error) {
	result, _ := response["result"].(map[string]any)
	sessions, ok := result["sessions"].([]any)
	if !ok {
		return "", errors.New("grok session roster response has no sessions")
	}
	matches := 0
	mode := ""
	for _, raw := range sessions {
		row, _ := raw.(map[string]any)
		resident, residentOK := row["resident"].(bool)
		if stringValue(row["sessionId"]) != sessionID || !residentOK || !resident {
			continue
		}
		yolo, yoloOK := row["yolo"].(bool)
		if !yoloOK {
			return "", errors.New("live Grok session roster row has no yolo state")
		}
		matches++
		mode = map[bool]string{true: "bypassPermissions", false: "default"}[yolo]
	}
	if matches != 1 {
		return "", fmt.Errorf("grok session roster returned %d live rows for %s", matches, sessionID)
	}
	return mode, nil
}

func grokAuthMethodAdvertised(result map[string]any, wanted string) bool {
	methods, _ := result["authMethods"].([]any)
	for _, raw := range methods {
		method, _ := raw.(map[string]any)
		if stringValue(method["id"]) == wanted {
			return true
		}
	}
	return false
}

func (h *grokHost) closeACPLocked() {
	if h.acp != nil {
		h.acp.close()
	}
	h.acp = nil
}

func (h *grokHost) acceptLoop() {
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			select {
			case <-h.done:
				return
			default:
				continue
			}
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.handleControlConn(conn)
		}()
	}
}

func (h *grokHost) handleControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(grokControlTimeout))
	line, err := bufio.NewReader(ioLimitReader{r: conn, n: maxFrameBytes}).ReadBytes('\n')
	if err != nil {
		return
	}
	var request map[string]any
	if json.Unmarshal(line, &request) != nil {
		h.writeControlResponse(conn, nil, errors.New("invalid Grok control request"))
		return
	}
	response, err := h.handleControl(request)
	h.writeControlResponse(conn, response, err)
}

func (h *grokHost) writeControlResponse(conn net.Conn, response map[string]any, err error) {
	result := map[string]any{"ok": err == nil}
	if err != nil {
		result["error"] = err.Error()
	} else {
		for key, value := range response {
			result[key] = value
		}
	}
	body, _ := json.Marshal(result)
	_, _ = conn.Write(append(body, '\n'))
}

func (h *grokHost) handleControl(request map[string]any) (map[string]any, error) {
	provided := stringValue(request["launchToken"])
	if subtle.ConstantTimeCompare([]byte(provided), []byte(h.config.LaunchToken)) != 1 {
		return nil, errors.New("grok control launch token mismatch")
	}
	if stringValue(request["sessionId"]) != h.config.SessionID {
		return nil, errors.New("grok control session mismatch")
	}
	switch stringValue(request["action"]) {
	case "status":
		resident := false
		refreshDeferred := !h.acpMu.TryLock()
		permissionAuthority := "live_roster"
		if !refreshDeferred {
			ctx, cancel := context.WithTimeout(context.Background(), grokACPStartupTimeout)
			err := h.ensureACPReadyLocked(ctx)
			cancel()
			resident = err == nil
			h.acpMu.Unlock()
			if err != nil {
				return nil, err
			}
		}
		h.peerMu.Lock()
		published := h.peer != nil
		var permissionMode string
		if refreshDeferred {
			// The interject request briefly owns the ACP request stream. Its
			// permission mode was refreshed immediately before submission;
			// blocking here would deadlock an MCP call that starts before the
			// request returns. Human-started turns do not occupy this stream and
			// take the live refresh path above.
			var valid bool
			permissionMode, valid = h.activeInterjectionPermissionSnapshot()
			if !published || !valid {
				h.peerMu.Unlock()
				return map[string]any{
					"sessionId": h.config.SessionID, "leaderReady": h.leader != nil,
					"loaded": false, "ready": false, "refreshBusy": true,
					"refreshDeferred": true, "permissionAuthority": "none",
				}, nil
			}
			resident = true
			permissionAuthority = "active_interjection_snapshot"
		} else {
			permissionMode = h.currentPermissionMode()
		}
		h.peerMu.Unlock()
		return map[string]any{
			"sessionId": h.config.SessionID, "leaderReady": h.leader != nil,
			"loaded": resident, "ready": resident && published,
			"permissionMode": permissionMode, "refreshDeferred": refreshDeferred,
			"permissionAuthority": permissionAuthority,
		}, nil
	case "wake":
		h.peerMu.Lock()
		published := h.peer != nil
		h.peerMu.Unlock()
		if !published {
			return nil, errors.New("grok peer is not ready for wake delivery")
		}
		item, _ := request["item"].(map[string]any)
		return h.queueWake(item)
	case "wake_status":
		return h.wakeStatus(stringValue(request["messageId"]))
	default:
		return nil, fmt.Errorf("unknown Grok control action %q", stringValue(request["action"]))
	}
}

func (h *grokHost) queueWake(item map[string]any) (map[string]any, error) {
	messageID := stringValue(item["id"])
	if messageID == "" || strings.TrimSpace(stringValue(item["message"])) == "" {
		return nil, errors.New("grok wake requires a message id and body")
	}
	fingerprint := wakeItemFingerprint(item)
	h.wakeMu.Lock()
	if existing := h.wakes[messageID]; existing != nil {
		if existing.Fingerprint != fingerprint {
			h.wakeMu.Unlock()
			return map[string]any{"delivery": "conflict", "messageId": messageID}, nil
		}
		delivery, detail := existing.Delivery, existing.Error
		h.wakeMu.Unlock()
		response := map[string]any{"delivery": delivery, "messageId": messageID}
		if detail != "" {
			response["detail"] = detail
		}
		return response, nil
	}
	record := &grokWakeRecord{
		SessionID: h.config.SessionID, MessageID: messageID, Fingerprint: fingerprint,
		Delivery: "queued", Item: item, UpdatedAt: time.Now().UnixMilli(),
	}
	if err := h.persistWakeRecord(record); err != nil {
		h.wakeMu.Unlock()
		return nil, fmt.Errorf("persist accepted Grok wake: %w", err)
	}
	h.wakes[messageID] = record
	h.wakeQueue = append(h.wakeQueue, messageID)
	h.wakeMu.Unlock()
	select {
	case h.wakeNotify <- struct{}{}:
	default:
	}
	return map[string]any{"delivery": "accepted", "messageId": messageID}, nil
}

func (h *grokHost) wakeStatus(messageID string) (map[string]any, error) {
	if messageID == "" {
		return nil, errors.New("grok wake_status requires a message id")
	}
	h.wakeMu.Lock()
	record := h.wakes[messageID]
	if record == nil {
		h.wakeMu.Unlock()
		return nil, errors.New("grok wake is not owned by this launch")
	}
	delivery, detail := record.Delivery, record.Error
	h.wakeMu.Unlock()
	response := map[string]any{"delivery": delivery, "messageId": messageID}
	if detail != "" {
		response["detail"] = detail
	}
	return response, nil
}

func (h *grokHost) wakeLoop() {
	for {
		messageID := h.popWake()
		if messageID == "" {
			select {
			case <-h.done:
				return
			case <-h.wakeNotify:
				continue
			}
		}
		h.deliverWake(messageID)
	}
}

func (h *grokHost) popWake() string {
	h.wakeMu.Lock()
	defer h.wakeMu.Unlock()
	if len(h.wakeQueue) == 0 {
		return ""
	}
	id := h.wakeQueue[0]
	if record := h.wakes[id]; record != nil {
		record.Delivery, record.Error, record.UpdatedAt = "in_flight", "", time.Now().UnixMilli()
		if err := h.persistWakeRecord(record); err != nil {
			record.Delivery, record.Error = "queued", err.Error()
			time.AfterFunc(500*time.Millisecond, func() {
				select {
				case h.wakeNotify <- struct{}{}:
				default:
				}
			})
			return ""
		}
	}
	h.wakeQueue = h.wakeQueue[1:]
	return id
}

func (h *grokHost) deliverWake(messageID string) {
	h.wakeMu.Lock()
	record := h.wakes[messageID]
	if record == nil {
		h.wakeMu.Unlock()
		return
	}
	item := record.Item
	h.wakeMu.Unlock()

	h.acpMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), grokACPStartupTimeout)
	err := h.ensureACPReadyLocked(ctx)
	cancel()
	if err != nil {
		h.acpMu.Unlock()
		h.requeueWake(messageID, err)
		return
	}
	h.beginActiveInterjectionPermissionSnapshot()
	if err := h.ensurePeerPublished(); err != nil {
		h.clearActiveInterjectionPermissionSnapshot()
		h.acpMu.Unlock()
		h.requeueWake(messageID, err)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), grokACPInterjectTimeout)
	_, err = h.acp.request(ctx, "_x.ai/interject", map[string]any{
		"method": "x.ai/interject",
		"params": map[string]any{
			"sessionId": h.config.SessionID, "text": trustedPeerText(item), "interjectionId": messageID,
		},
	})
	h.clearActiveInterjectionPermissionSnapshot()
	cancel()
	if err != nil {
		var rpcErr *grokRPCError
		if errors.As(err, &rpcErr) {
			// A JSON-RPC error proves the interjection was rejected. It is safe to
			// reconnect and retry the same immutable message id.
			h.closeACPLocked()
			h.acpMu.Unlock()
			h.requeueWake(messageID, err)
			return
		}
		// EOF/timeout after the write is ambiguous. Keep durable ownership and
		// never issue a second interjection that could duplicate accepted work.
		h.closeACPLocked()
		h.acpMu.Unlock()
		h.setWakeResult(messageID, "in_flight", "delivery outcome is unknown: "+err.Error())
		return
	}
	h.acpMu.Unlock()
	h.setWakeResult(messageID, "delivered", "")
}

func (h *grokHost) requeueWake(messageID string, deliveryErr error) {
	h.setWakeResult(messageID, "queued", deliveryErr.Error())
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-h.done:
		return
	case <-timer.C:
	}
	h.wakeMu.Lock()
	if record := h.wakes[messageID]; record != nil && record.Delivery == "queued" {
		h.wakeQueue = append(h.wakeQueue, messageID)
	}
	h.wakeMu.Unlock()
	select {
	case h.wakeNotify <- struct{}{}:
	default:
	}
}

func (h *grokHost) setWakeResult(messageID, delivery, detail string) {
	h.wakeMu.Lock()
	if record := h.wakes[messageID]; record != nil {
		record.Delivery, record.Error, record.UpdatedAt = delivery, detail, time.Now().UnixMilli()
		_ = h.persistWakeRecord(record)
	}
	h.wakeMu.Unlock()
}

func (h *grokHost) cleanup() {
	h.closeOnce.Do(func() {
		close(h.done)
		if h.listener != nil {
			_ = h.listener.Close()
		}
		h.peerMu.Lock()
		if h.peer != nil {
			h.peer.shutdown()
			h.peer = nil
		}
		h.peerMu.Unlock()
		h.acpMu.Lock()
		h.closeACPLocked()
		h.acpMu.Unlock()
		stopGrokManagedProcess(h.leader, 2*time.Second)
		h.wg.Wait()
		_ = os.Remove(h.paths.ControlSocket)
		_ = os.Remove(h.paths.LeaderSocket)
		_ = os.Remove(filepath.Join(h.paths.LaunchDir, "leader.lock"))
		_ = os.Remove(h.paths.LaunchDir)
		paths := resolveNativePaths()
		removeJSONIf(grokLaunchRecordPath(paths, h.record.SessionID), func(row map[string]any) bool {
			return stringValue(row["tokenHash"]) == h.record.TokenHash &&
				intValue(row["hostPid"]) == os.Getpid() && stringValue(row["hostProcStart"]) == h.record.HostProcStart
		})
		if h.lease != nil {
			_ = syscall.Flock(int(h.lease.Fd()), syscall.LOCK_UN)
			_ = h.lease.Close()
			h.lease = nil
		}
	})
}

func (h *grokHost) ensurePeerPublished() error {
	h.peerMu.Lock()
	defer h.peerMu.Unlock()
	select {
	case <-h.done:
		return errors.New("grok host is stopping")
	default:
	}
	if h.peer != nil {
		return nil
	}
	paths := resolveNativePaths()
	args := map[string]string{
		"session-id": h.config.SessionID, "cwd": h.config.Cwd,
		"name": h.config.Name, "name-source": "launch", "entrypoint": "grok",
		"permission-mode":   h.currentPermissionMode(),
		"supervisor-socket": h.paths.ControlSocket,
		"supervisor-token":  h.config.LaunchToken,
		"owner-pid":         strconvItoa(h.config.OwnerPID), "owner-proc-start": h.config.OwnerProcStart,
		"data-dir": paths.dataRoot, "claude-config-dir": paths.claudeRoot,
		"codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
	}
	peer := newDaemon(args)
	if err := peer.start(); err != nil {
		return fmt.Errorf("publish live Grok peer: %w", err)
	}
	h.peer = peer
	return nil
}

func strconvItoa(value int) string { return fmt.Sprintf("%d", value) }

// runGrokHostCommand is the native runtime subcommand implementation. Main's
// dispatch is deliberately a separate, one-line integration hook.
func runGrokHostCommand(argv []string) int {
	flags := flag.NewFlagSet("grok-host", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var config grokHostConfig
	flags.StringVar(&config.GrokBin, "grok-bin", "", "headless-capable Grok CLI")
	flags.StringVar(&config.SessionID, "session-id", "", "exact Grok session id")
	flags.StringVar(&config.Cwd, "cwd", "", "session cwd")
	flags.IntVar(&config.OwnerPID, "owner-pid", 0, "TUI owner pid")
	flags.StringVar(&config.OwnerProcStart, "owner-proc-start", "", "TUI owner process-start token")
	flags.StringVar(&config.RuntimeDir, "runtime-dir", "", "private runtime parent")
	flags.StringVar(&config.Name, "name", "", "published peer name")
	flags.StringVar(&config.PermissionMode, "permission-mode", "default", "published permission class")
	if err := flags.Parse(argv); err != nil || flags.NArg() != 0 {
		return 2
	}
	config.LaunchToken = strings.TrimSpace(os.Getenv(grokLaunchTokenEnv))
	host, err := newGrokHost(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok-host: %v\n", err)
		return 1
	}
	host.config.readyWriter = os.Stdout
	ctx, stop := signalContext()
	defer stop()
	if err := host.run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok-host: %v\n", err)
		return 1
	}
	return 0
}

func signalContext() (context.Context, context.CancelFunc) {
	// signal.NotifyContext is isolated here so tests can drive hosts with their
	// own contexts without installing process-global handlers.
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
