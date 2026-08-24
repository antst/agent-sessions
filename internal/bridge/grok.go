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

	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const (
	grokLaunchTokenEnv    = "AGENT_SESSIONS_GROK_LAUNCH_TOKEN"
	grokSessionIDEnv      = "AGENT_SESSIONS_GROK_SESSION_ID"
	grokACPStartupTimeout = 15 * time.Second
	// The outer control budget must exceed the inner ACP startup/refresh
	// budget so a cold reconnect can return its authoritative result.
	grokControlTimeout      = grokACPStartupTimeout + 5*time.Second
	grokACPInterjectTimeout = 30 * time.Second
	grokInterjectionModeTTL = 30 * time.Minute
	grokRosterPushGrace     = 1500 * time.Millisecond
	grokCleanupWaitTimeout  = 5 * time.Second
	grokStatusRetryDelay    = 25 * time.Millisecond
	grokStatusRetryMax      = 250 * time.Millisecond
)

// grokHostConfig describes one process-attested interactive Grok launch.  A
// host never adopts Grok's default leader: every launch gets a new private
// directory, leader, ACP bridge, and control socket.
type grokHostConfig struct {
	GrokBin         string
	SessionID       string
	Cwd             string
	OwnerPID        int
	OwnerProcStart  string
	LaunchToken     string
	RuntimeDir      string
	Name            string
	NameSpecified   bool
	PermissionMode  string
	AgentRuntimeDir string
	AttachmentID    string
	LateBoundResume bool
	Groups          []string
	GroupsSpecified bool
	ParentSession   string
	ParentSpecified bool
	InheritGroups   bool
	InheritSet      bool
	AlwaysApprove   bool
	AlwaysSet       bool
	readyWriter     io.Writer

	// command is overridden only by tests. Production always executes GrokBin.
	command            func(args ...string) *exec.Cmd
	resolvePreferences func(federator.ResolvePreferencesRequest) (federator.ResolvedPreferences, error)
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
	AttachmentID    string `json:"attachmentId,omitempty"`
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
	// Prefer the caller's runtime directory, but compact before any socket is
	// created when the longest per-launch address would exceed sun_path.
	root = socketpath.PreferRoot(
		root,
		filepath.Join("/tmp", fmt.Sprintf("asg-%d", uid)),
		filepath.Join("g-"+strings.Repeat("0", 20), "control.sock"),
	)
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
	_ = os.Remove(filepath.Join(expected.LaunchDir, "diagnostics.log"))
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

//nolint:gocyclo // Installer inventory deliberately validates every durable ownership source fail-closed.
func activeGrokLaunchSessions(paths nativePaths) ([]string, error) {
	liveSet := map[string]bool{}
	directory := filepath.Join(profileDataRoot(paths), "grok-launches")
	entries, err := os.ReadDir(directory)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read Grok launch inventory: %w", err)
	}
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
				liveSet[record.SessionID] = true
				break
			}
		}
	}
	laneDirectory := filepath.Join(profileDataRoot(paths), "grok-lanes")
	laneEntries, laneErr := os.ReadDir(laneDirectory)
	if laneErr != nil && !os.IsNotExist(laneErr) {
		return nil, fmt.Errorf("read Grok lane inventory: %w", laneErr)
	}
	for _, entry := range laneEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(laneDirectory, entry.Name())
		body, readErr := os.ReadFile(path) //nolint:gosec // path is a bridge-owned inventory entry under a fixed private directory.
		if readErr != nil {
			return nil, fmt.Errorf("read Grok lane inventory record: %w", readErr)
		}
		var state grokLaneState
		if json.Unmarshal(body, &state) != nil || state.Type != "grok-peer-lane" || !validSessionID(state.SessionID) {
			return nil, fmt.Errorf("refuse malformed Grok lane record: %s", path)
		}
		manager := cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart)
		worker := cleanupProcessIdentityStatus(state.WorkerPID, state.WorkerProcStart)
		registryGuard, cleanupRoots, registryErr := grokLaneCleanupRoots(state, false)
		if registryErr != nil {
			return nil, fmt.Errorf("read Grok lane tool ownership: %w", registryErr)
		}
		taggedRemain := grokTaggedProcessesRemain(state.LaunchTokenHash, 0, cleanupRoots...)
		if registryGuard != nil {
			registryGuard.close()
		}
		if state.Status != "archived" || manager.Status != processIdentityStale || worker.Status != processIdentityStale ||
			grokProcessSessionHasMembers(state.WorkerSessionID, 0) ||
			taggedRemain {
			liveSet[state.SessionID] = true
		}
	}
	live := make([]string, 0, len(liveSet))
	for sessionID := range liveSet {
		live = append(live, sessionID)
	}
	sort.Strings(live)
	return live, nil
}

// activeGrokLaunchForSession returns an owner-attested launch whose private
// leader and control plane are live. Publication is deliberately not required:
// the Grok MCP process must prove it can answer one harmless tool call before
// the peer is advertised, and requiring the advertisement here would make that
// readiness check circular.
func activeGrokLaunchForSession(paths nativePaths, sessionID string) *grokLaunchRecord {
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
	return record
}

// activeGrokLaunchForToken recovers the canonical session selected by a
// native title resume. The private leader and TUI inherit the provisional
// attachment ID before Grok resolves the title, but the strong launch token,
// exact process tree, and private sockets remain stable across that selection.
func activeGrokLaunchForToken(paths nativePaths, token string) *grokLaunchRecord {
	if !validGrokLaunchToken(token) {
		return nil
	}
	directory := filepath.Join(profileDataRoot(paths), "grok-launches")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	wanted := grokTokenHash(token)
	var matched *grokLaunchRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record := readGrokLaunchRecord(filepath.Join(directory, entry.Name()))
		if record == nil || record.TokenHash != wanted ||
			(record.AttachmentID != "" && record.AttachmentID == record.SessionID) ||
			entry.Name() != sessionKey(record.SessionID)+".json" || !liveGrokProcessTree(*record) {
			continue
		}
		expected := grokRuntimePathsForKey(record.RuntimeDir, os.Getuid(), record.TokenHash[:20])
		if !liveGrokControlPaths(*record, expected) || matched != nil {
			return nil
		}
		recordCopy := *record
		matched = &recordCopy
	}
	return matched
}

// liveGrokLaunchForSession returns only a fully resident and published launch.
// Registry data alone is discovery, never authority: the owning process tree,
// private paths, and published bridge state must all corroborate it.
func liveGrokLaunchForSession(paths nativePaths, sessionID string) *grokLaunchRecord {
	record := activeGrokLaunchForSession(paths, sessionID)
	if record == nil {
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
	record := activeGrokLaunchForSession(paths, sessionID)
	if record == nil {
		record = activeGrokLaunchForToken(paths, token)
	}
	if record == nil || subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(grokTokenHash(token))) != 1 {
		return "", errors.New("grok MCP launch token is not attested by a live host")
	}
	if !processHasAncestor(os.Getpid(), record.LeaderPID) {
		return "", errors.New("grok MCP process is not descended from the private launch leader")
	}
	// The host's pre-publication MCP probe runs while it owns the ACP request
	// stream. Calling status from that probe would wait on the same stream and
	// deadlock. The raw capability, exact ancestry, owner/host/leader identities,
	// and private control paths already authorize this narrow bootstrap phase.
	// Once the daemon is published, every model-visible call refreshes the live
	// permission class normally before it can act as the peer.
	if liveGrokLaunchForSession(paths, record.SessionID) != nil {
		if err := refreshGrokLaunchPermission(record, token); err != nil {
			return "", fmt.Errorf("refresh live Grok permission mode: %w", err)
		}
	}
	return record.SessionID, nil
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
	if record == nil {
		record = activeGrokLaunchForToken(paths, token)
		if record != nil {
			record = liveGrokLaunchForSession(paths, record.SessionID)
		}
	}
	if record == nil || subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(grokTokenHash(token))) != 1 ||
		!processHasAncestor(startPID, record.LeaderPID) {
		return laneOwner{}, false
	}
	if refreshGrokLaunchPermission(record, token) != nil {
		return laneOwner{}, false
	}
	record = liveGrokLaunchForSession(paths, record.SessionID)
	if record == nil {
		return laneOwner{}, false
	}
	return laneOwner{
		PID: record.HostPID, ProcStart: record.HostProcStart,
		SessionID: record.SessionID, PermissionMode: defaultString(record.PermissionMode, "default"),
	}, true
}

type grokManagedProcess struct {
	cmd         *exec.Cmd
	procStart   string
	done        chan struct{}
	mu          sync.Mutex
	err         error
	diagnostics *grokProcessDiagnostics
}

func startGrokManagedProcess(command *exec.Cmd, diagnostics *grokProcessDiagnostics) (*grokManagedProcess, error) {
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
	managed := &grokManagedProcess{cmd: command, procStart: procStart, done: make(chan struct{}), diagnostics: diagnostics}
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

func (p *grokManagedProcess) attributedError(role string, cause error) error {
	// All callers with a started process stop or observe it first. Use a bounded
	// final join because stdout EOF can precede a last stderr write, while an
	// unverifiable process identity must never hang host shutdown indefinitely.
	joined := p == nil || p.done == nil
	if p != nil && p.done != nil {
		timer := time.NewTimer(grokDiagnosticJoinTimeout)
		select {
		case <-p.done:
			joined = true
			if cause == nil {
				cause = p.waitError()
			}
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	if !joined {
		if p == nil || p.diagnostics == nil {
			return fmt.Errorf("%s; managed process join incomplete; private diagnostics unavailable", role)
		}
		return p.diagnostics.safeError(role, false)
	}
	if cause == nil {
		cause = errors.New("process exited unexpectedly")
	}
	if p == nil || p.diagnostics == nil {
		return fmt.Errorf("%s; private diagnostics unavailable", role)
	}
	p.diagnostics.recordFailure(cause)
	return p.diagnostics.safeError(role, true)
}

type grokACPClient struct {
	process   *grokManagedProcess
	stdin     io.WriteCloser
	responses chan map[string]any
	interject chan map[string]any
	notifyMu  sync.RWMutex
	notify    func(map[string]any)
	readDone  chan struct{}
	readMu    sync.Mutex
	readErr   error
	requestMu sync.Mutex
	writeMu   sync.Mutex
	nextID    int64
}

func (c *grokACPClient) readError() error {
	if c == nil {
		return io.EOF
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readErr == nil {
		return io.EOF
	}
	return c.readErr
}

type grokRosterState struct {
	name           string
	permissionMode string
	status         string
	fromPush       bool
	generation     uint64
	authorityLost  bool
}

var errGrokRosterAuthorityLost = errors.New("live Grok roster authority lost")

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

func newGrokACPClient(
	process *grokManagedProcess,
	stdin io.WriteCloser,
	stdout io.ReadCloser,
	sessionID string,
	generation uint64,
	rosterUpdates chan grokRosterState,
) *grokACPClient {
	client := &grokACPClient{
		process: process, stdin: stdin, responses: make(chan map[string]any, 32),
		interject: make(chan map[string]any, 32), readDone: make(chan struct{}),
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxFrameBytes)
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				continue
			}
			if message["id"] == nil {
				client.notifyMu.RLock()
				notify := client.notify
				client.notifyMu.RUnlock()
				if notify != nil {
					notify(message)
				}
				switch stringValue(message["method"]) {
				case "_x.ai/session/interjection":
					select {
					case client.interject <- message:
					case <-client.readDone:
						return
					}
				case "_x.ai/sessions/changed", "x.ai/sessions/changed":
					if state, ok := grokRosterNotificationState(message, sessionID); ok {
						state.generation = generation
						publishLatestGrokRosterState(rosterUpdates, state)
					}
				}
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

func (c *grokACPClient) setNotificationHandler(handler func(map[string]any)) {
	c.notifyMu.Lock()
	c.notify = handler
	c.notifyMu.Unlock()
}

func publishLatestGrokRosterState(updates chan grokRosterState, state grokRosterState) {
	select {
	case updates <- state:
		return
	default:
	}
	select {
	case <-updates:
	default:
	}
	select {
	case updates <- state:
	default:
	}
}

func (c *grokACPClient) requestInterjection(ctx context.Context, sessionID, messageID, text string) error {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "_x.ai/interject",
		"params": map[string]any{"sessionId": sessionID, "text": text, "interjectionId": messageID},
	})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write Grok ACP _x.ai/interject: %w", err)
	}
	for {
		select {
		case notification := <-c.interject:
			params, _ := notification["params"].(map[string]any)
			if stringValue(params["sessionId"]) == sessionID && stringValue(params["interjectionId"]) == messageID {
				return nil
			}
		case response := <-c.responses:
			if int64Value(response["id"]) != id {
				continue
			}
			if raw, ok := response["error"].(map[string]any); ok {
				return &grokRPCError{Code: intValue(raw["code"]), Message: defaultString(stringValue(raw["message"]), "request rejected")}
			}
			result, _ := response["result"].(map[string]any)
			inner, _ := result["result"].(map[string]any)
			if stringValue(inner["status"]) != "queued" {
				return errors.New("grok ACP interjection returned no queued acknowledgement")
			}
			// Grok 1.0.4 returns queued even when the resident actor's mailbox
			// is closed. Only the actor's matching notification proves that it
			// began handling this immutable interjection id.
		case <-c.readDone:
			c.readMu.Lock()
			readErr := c.readErr
			c.readMu.Unlock()
			if readErr == nil {
				readErr = io.EOF
			}
			return fmt.Errorf("read Grok ACP interjection acknowledgement: %w", readErr)
		case <-ctx.Done():
			return fmt.Errorf("wait for Grok actor interjection acknowledgement: %w", ctx.Err())
		}
	}
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
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
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

func (c *grokACPClient) notifyRequest(method string, params map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write Grok ACP %s notification: %w", method, err)
	}
	return nil
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
	// Native title/picker resume replaces the provisional attachment with the
	// selected UUID after the host has already started its worker goroutines.
	// Keep every mutable identity field behind one independent lock.
	identityMu sync.RWMutex
	identity   grokHostIdentity

	listener    net.Listener
	leader      *grokManagedProcess
	diagnostics *grokDiagnosticSink
	peer        *daemon
	peerMu      sync.Mutex
	record      grokLaunchRecord
	lease       *os.File
	modeMu      sync.RWMutex
	mode        string
	status      string
	// activeInterjectionMode is authoritative from the roster refresh immediately
	// preceding x.ai/interject until the first successful roster refresh after the
	// actor accepts it. Grok can hold roster requests for the whole generated turn,
	// so the snapshot must outlive the brief actor-ack RPC. It must never turn
	// arbitrary acpMu contention into permission authority.
	activeInterjectionMode  string
	activeInterjectionValid bool
	activeInterjectionAt    time.Time
	lastRosterPushAt        time.Time
	acpGeneration           uint64
	rosterValid             bool

	// publishRosterState is a test seam. Production writes both daemon records
	// through writeRecordsLocked.
	publishRosterState func(*daemon) error

	acpMu sync.Mutex
	acp   *grokACPClient

	wakeMu        sync.Mutex
	wakes         map[string]*grokWakeRecord
	wakeQueue     []string
	wakeNotify    chan struct{}
	rosterUpdates chan grokRosterState

	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type grokHostIdentity struct {
	sessionID       string
	name            string
	lateBoundResume bool
}

func (h *grokHost) identitySnapshot() grokHostIdentity {
	h.identityMu.RLock()
	defer h.identityMu.RUnlock()
	return h.identity
}

func (h *grokHost) setIdentity(identity grokHostIdentity) {
	h.identityMu.Lock()
	h.identity = identity
	h.identityMu.Unlock()
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
	if config.LateBoundResume {
		config.AttachmentID = config.SessionID
	}
	if config.RuntimeDir == "" {
		config.RuntimeDir = os.TempDir()
	}
	host := &grokHost{
		config: config,
		paths:  grokRuntimePaths(config.RuntimeDir, os.Getuid(), config.LaunchToken),
		identity: grokHostIdentity{
			sessionID: config.SessionID, name: config.Name, lateBoundResume: config.LateBoundResume,
		},
		mode: config.PermissionMode, status: "idle",
		wakes: make(map[string]*grokWakeRecord), wakeNotify: make(chan struct{}, 1),
		rosterUpdates: make(chan grokRosterState, 8),
		done:          make(chan struct{}),
	}
	host.restoreWakeRecords()
	return host, nil
}

func (h *grokHost) restoreWakeRecords() {
	h.wakeMu.Lock()
	defer h.wakeMu.Unlock()
	sessionID := h.identitySnapshot().sessionID
	directory := filepath.Dir(grokWakeRecordPath(resolveNativePaths(), sessionID, "message"))
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
		if readErr != nil || json.Unmarshal(body, &record) != nil || record.SessionID != sessionID ||
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
	if record == nil || record.SessionID != h.identitySnapshot().sessionID || record.MessageID == "" {
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
		grokSessionIDEnv+"="+h.identitySnapshot().sessionID,
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
		identity := h.identitySnapshot()
		_ = json.NewEncoder(h.config.readyWriter).Encode(map[string]any{
			"ready": true, "session_id": identity.sessionID, "cwd": h.config.Cwd,
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
			return h.leader.attributedError("private Grok leader exited", nil)
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
	identity := h.identitySnapshot()
	lease, err := acquireGrokLaunchLease(paths, identity.sessionID)
	if err != nil {
		return err
	}
	h.lease = lease
	if err := ensurePrivateRuntimeDir(h.paths.Root); err != nil {
		return err
	}
	h.record = grokLaunchRecord{
		SessionID: identity.sessionID, AttachmentID: h.config.AttachmentID,
		Cwd: h.config.Cwd, Name: sanitizeName(identity.name),
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
	diagnostics, err := newGrokDiagnosticSink(filepath.Join(h.paths.LaunchDir, "diagnostics.log"))
	if err != nil {
		return err
	}
	h.diagnostics = diagnostics
	leaderDiagnostics := diagnostics.process("private Grok leader")
	leaderCommand := h.grokCommand(
		"--permission-mode", "default",
		"agent", "leader", "--leader-socket", h.paths.LeaderSocket,
		"--no-exit-on-disconnect", "--relay-on-demand", "--no-auto-update",
	)
	leaderCommand.Stdout, leaderCommand.Stderr = leaderDiagnostics, leaderDiagnostics
	leader, err := startGrokManagedProcess(leaderCommand, leaderDiagnostics)
	if err != nil {
		return (&grokManagedProcess{diagnostics: leaderDiagnostics}).attributedError("start private Grok leader", err)
	}
	h.leader = leader
	h.record.LeaderPID, h.record.LeaderProcStart = leader.cmd.Process.Pid, leader.procStart
	if err := writeJSONAtomic(grokLaunchRecordPath(paths, h.record.SessionID), h.record); err != nil {
		return fmt.Errorf("persist private Grok leader ownership: %w", err)
	}
	if err := h.waitForLeaderSocket(10 * time.Second); err != nil {
		return err
	}
	if err := socketpath.Validate(h.paths.ControlSocket); err != nil {
		return fmt.Errorf("validate Grok control socket: %w", err)
	}
	listener, err := net.Listen("unix", h.paths.ControlSocket)
	if err != nil {
		return fmt.Errorf("listen on Grok control socket: %w", err)
	}
	h.listener = listener
	if err := os.Chmod(h.paths.ControlSocket, 0o600); err != nil {
		return fmt.Errorf("secure Grok control socket: %w", err)
	}
	h.wg.Add(4)
	go func() { defer h.wg.Done(); h.acceptLoop() }()
	go func() { defer h.wg.Done(); h.wakeLoop() }()
	go func() { defer h.wg.Done(); h.prepareLoop() }()
	go func() { defer h.wg.Done(); h.rosterLoop() }()
	return nil
}

func (h *grokHost) rosterLoop() {
	for {
		select {
		case state := <-h.rosterUpdates:
			_ = h.applyRosterState(state)
		case <-h.done:
			return
		}
	}
}

func (h *grokHost) waitForLeaderSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-h.leader.done:
			return h.leader.attributedError("private Grok leader exited before its socket was ready", nil)
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
		err := h.ensureACPForPublication(ctx)
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

func (h *grokHost) ensureACPForPublication(ctx context.Context) error {
	h.acpMu.Lock()
	defer h.acpMu.Unlock()
	if err := h.ensureACPReadyLocked(ctx); err != nil {
		return err
	}
	return h.ensureAgentSessionsMCPReadyLocked(ctx)
}

func (h *grokHost) ensureACPReadyLocked(ctx context.Context) error {
	if err := h.ensureACPConnectedLocked(ctx); err != nil {
		return err
	}
	return h.refreshRosterStateLocked(ctx)
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
			process := h.acp.process
			readErr := h.acp.readError()
			h.closeACPLocked()
			// Wait boundedly for the completed observer's final stderr before
			// recording its failure. A healthy replacement remains transparent to
			// the caller, so discard the fixed role-only outward error.
			_ = process.attributedError("official Grok ACP observer exited", readErr)
		default:
		}
	}
	if h.acp == nil {
		// Invalidate the old observer before any replacement spawn/persist work.
		// Otherwise first publication can race through the dead generation while
		// this function is still constructing the replacement client.
		generation := h.nextACPGeneration()
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
		observerDiagnostics := h.diagnostics.process("official Grok ACP observer")
		command.Stderr = observerDiagnostics
		process, err := startGrokManagedProcess(command, observerDiagnostics)
		if err != nil {
			return (&grokManagedProcess{diagnostics: observerDiagnostics}).attributedError("start official Grok ACP observer", err)
		}
		h.record.WakerPID, h.record.WakerProcStart = process.cmd.Process.Pid, process.procStart
		if err := writeJSONAtomic(grokLaunchRecordPath(resolveNativePaths(), h.record.SessionID), h.record); err != nil {
			stopGrokManagedProcess(process, 2*time.Second)
			return process.attributedError("persist official Grok ACP observer ownership", err)
		}
		h.acp = newGrokACPClient(process, stdin, stdout, h.identitySnapshot().sessionID, generation, h.rosterUpdates)
		result, err := h.acp.request(ctx, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
				"terminal": false,
			},
		})
		if err != nil {
			process := h.acp.process
			h.closeACPLocked()
			return process.attributedError("initialize official Grok ACP observer", err)
		}
		if !grokAuthMethodAdvertised(result, "cached_token") {
			process := h.acp.process
			h.closeACPLocked()
			return process.attributedError("authenticate official Grok ACP observer", errors.New("cached_token authentication was not advertised"))
		}
		if _, err := h.acp.request(ctx, "authenticate", map[string]any{
			"methodId": "cached_token", "_meta": map[string]any{"headless": true},
		}); err != nil {
			process := h.acp.process
			h.closeACPLocked()
			return process.attributedError("authenticate official Grok ACP observer", err)
		}
	}
	return nil
}

func (h *grokHost) refreshRosterStateLocked(ctx context.Context) error {
	result, err := h.acp.request(ctx, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return fmt.Errorf("query live Grok session roster: %w", err)
	}
	identity := h.identitySnapshot()
	if identity.lateBoundResume {
		selectedID, state, selectionErr := grokSelectedResidentSession(result)
		if selectionErr != nil {
			return selectionErr
		}
		if adoptErr := h.adoptNativeGrokSelection(selectedID, state); adoptErr != nil {
			return adoptErr
		}
		// Reconnect so notification filtering and every subsequent request use
		// the selected native UUID rather than the provisional attachment ID.
		h.closeACPLocked()
		return errors.New("native Grok resume selection adopted; reconnecting observer")
	}
	state, err := grokRosterStateFromResponse(result, identity.sessionID)
	if err != nil {
		if errors.Is(err, errGrokRosterAuthorityLost) {
			h.stopForRosterAuthorityLoss(h.currentACPGeneration())
		}
		return err
	}
	state.generation = h.currentACPGeneration()
	return h.applyRosterState(state)
}

var errGrokSelectionPending = errors.New("native Grok resume selection is not resident yet")

func grokSelectedResidentSession(response map[string]any) (string, grokRosterState, error) {
	result, _ := response["result"].(map[string]any)
	sessions, ok := result["sessions"].([]any)
	if !ok {
		return "", grokRosterState{}, errors.New("native Grok resume roster has no sessions")
	}
	selected := ""
	for _, raw := range sessions {
		row, _ := raw.(map[string]any)
		id := stringValue(row["sessionId"])
		resident, residentOK := row["resident"].(bool)
		activity := stringValue(row["activity"])
		if !validSessionID(id) || !residentOK || !resident ||
			activity == "completed" || activity == "dormant" || activity == "dead" {
			continue
		}
		if selected != "" && selected != id {
			return "", grokRosterState{}, errors.New("private Grok leader reported multiple resident resume selections")
		}
		selected = id
	}
	if selected == "" {
		return "", grokRosterState{}, errGrokSelectionPending
	}
	state, err := grokRosterStateFromResponse(response, selected)
	return selected, state, err
}

func (h *grokHost) adoptNativeGrokSelection(selectedID string, state grokRosterState) error {
	identity := h.identitySnapshot()
	if !identity.lateBoundResume || !validSessionID(selectedID) || selectedID == h.config.AttachmentID {
		return errors.New("invalid native Grok resume selection")
	}
	paths := resolveNativePaths()
	selectedLease, err := acquireGrokLaunchLease(paths, selectedID)
	if err != nil {
		return err
	}
	resolve := h.config.resolvePreferences
	if resolve == nil {
		resolve = func(request federator.ResolvePreferencesRequest) (federator.ResolvedPreferences, error) {
			return federator.ResolveSessionPreferences(h.config.AgentRuntimeDir, request)
		}
	}
	resolved, err := resolve(federator.ResolvePreferencesRequest{
		SessionID: selectedID, Product: "grok", Kind: federator.SessionKindInteractive,
		Groups: h.config.Groups, GroupsSpecified: h.config.GroupsSpecified,
		ParentSessionID: h.config.ParentSession, ParentSpecified: h.config.ParentSpecified,
		InheritParentGroups: h.config.InheritGroups, InheritGroupsSpecified: h.config.InheritSet,
		AlwaysApprove: h.config.AlwaysApprove, AlwaysApproveSpecified: h.config.AlwaysSet,
	})
	if err != nil {
		_ = selectedLease.Close()
		return fmt.Errorf("resolve selected Grok peer preferences: %w", err)
	}
	liveYolo := state.permissionMode == "bypassPermissions"
	if resolved.Preference.AlwaysApprove != liveYolo {
		_ = selectedLease.Close()
		return errors.New("grok selected a session whose durable yolo preference differs from the native launch; pass --yolo or --no-yolo explicitly")
	}
	previousID := identity.sessionID
	promoted := h.record
	promoted.SessionID = selectedID
	promoted.AttachmentID = h.config.AttachmentID
	promoted.PermissionMode = state.permissionMode
	if !h.config.NameSpecified && state.name != "" {
		promoted.Name = sanitizeName(state.name)
	}
	if err := claimGrokLaunchRecord(paths, promoted); err != nil {
		_ = selectedLease.Close()
		return fmt.Errorf("persist selected Grok launch ownership: %w", err)
	}
	removeJSONIf(grokLaunchRecordPath(paths, previousID), func(row map[string]any) bool {
		return stringValue(row["tokenHash"]) == promoted.TokenHash && intValue(row["hostPid"]) == os.Getpid()
	})
	if h.lease != nil {
		_ = h.lease.Close()
	}
	h.lease = selectedLease
	identity.sessionID = selectedID
	identity.lateBoundResume = false
	if !h.config.NameSpecified && state.name != "" {
		identity.name = promoted.Name
	}
	h.setIdentity(identity)
	h.record = promoted
	h.modeMu.Lock()
	h.mode, h.status = state.permissionMode, state.status
	h.modeMu.Unlock()
	h.restoreWakeRecords()
	return nil
}

//nolint:gocyclo // Generation, identity, permission, status, and durable publication are independent fail-closed gates.
func (h *grokHost) applyRosterState(state grokRosterState) error {
	if state.authorityLost {
		h.stopForRosterAuthorityLoss(state.generation)
		return errGrokRosterAuthorityLost
	}
	// Every path that needs both locks takes peerMu before modeMu. Keep the
	// candidate dirty until the daemon state and registry both contain it; a
	// failed publication must be retried by the next roster refresh.
	h.peerMu.Lock()
	defer h.peerMu.Unlock()
	h.modeMu.Lock()
	defer h.modeMu.Unlock()
	if state.generation != 0 && state.generation != h.acpGeneration {
		return nil
	}
	pushObservedAt := h.reconcileRosterPushLocked(&state)
	identity := h.identitySnapshot()
	desiredName := identity.name
	if !h.config.NameSpecified && state.name != "" {
		desiredName = sanitizeName(state.name)
	}
	if state.permissionMode == h.mode && state.status == h.status && desiredName == identity.name {
		h.rosterValid = true
		if !pushObservedAt.IsZero() {
			h.lastRosterPushAt = pushObservedAt
		}
		h.activeInterjectionMode = ""
		h.activeInterjectionValid = false
		h.activeInterjectionAt = time.Time{}
		return nil
	}
	if err := h.publishRosterStateLocked(state); err != nil {
		return err
	}
	nextRecord := h.record
	if desiredName != identity.name {
		nextRecord.Name = desiredName
	}
	if state.permissionMode != h.mode {
		nextRecord.PermissionMode = state.permissionMode
	}
	if nextRecord.Name != h.record.Name || nextRecord.PermissionMode != h.record.PermissionMode {
		if err := writeJSONAtomic(grokLaunchRecordPath(resolveNativePaths(), nextRecord.SessionID), nextRecord); err != nil {
			return fmt.Errorf("persist live Grok roster identity: %w", err)
		}
	}
	identity.name = desiredName
	h.setIdentity(identity)
	h.mode = state.permissionMode
	h.status = state.status
	h.record = nextRecord
	h.rosterValid = true
	if !pushObservedAt.IsZero() {
		h.lastRosterPushAt = pushObservedAt
	}
	h.activeInterjectionMode = ""
	h.activeInterjectionValid = false
	h.activeInterjectionAt = time.Time{}
	return nil
}

// reconcileRosterPushLocked preserves Grok's forced turn-boundary push over
// the one list snapshot that can race before currentPromptId changes. Callers
// hold modeMu. The returned timestamp is committed only after daemon and
// launch-record publication succeeds.
func (h *grokHost) reconcileRosterPushLocked(state *grokRosterState) time.Time {
	if state.fromPush {
		return time.Now()
	}
	if state.status != h.status && !h.lastRosterPushAt.IsZero() && time.Since(h.lastRosterPushAt) < grokRosterPushGrace {
		state.status = h.status
	}
	return time.Time{}
}

// publishRosterStateLocked updates both native daemon records atomically from
// the host's perspective and restores the previous in-memory values on error.
// Callers hold peerMu and modeMu.
func (h *grokHost) publishRosterStateLocked(state grokRosterState) error {
	peer := h.peer
	if peer == nil {
		return nil
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	previousMode := peer.permissionMode
	previousStatus := peer.status
	previousName := peer.name
	previousNameSource := peer.nameSource
	peer.permissionMode = state.permissionMode
	peer.status = state.status
	if !h.config.NameSpecified && state.name != "" {
		peer.name = sanitizeName(state.name)
		peer.nameSource = "canonical"
	}
	publisher := h.publishRosterState
	if publisher == nil {
		publisher = func(peer *daemon) error { return peer.writeRecordsLocked() }
	}
	if err := publisher(peer); err != nil {
		peer.permissionMode = previousMode
		peer.status = previousStatus
		peer.name = previousName
		peer.nameSource = previousNameSource
		return fmt.Errorf("publish live Grok roster state: %w", err)
	}
	return nil
}

func (h *grokHost) currentACPGeneration() uint64 {
	h.modeMu.RLock()
	defer h.modeMu.RUnlock()
	return h.acpGeneration
}

func (h *grokHost) nextACPGeneration() uint64 {
	// A reconnect and first publication must serialize on peerMu. Otherwise an
	// old generation can pass the publication check immediately before this
	// reset and publish an actor whose authority has just been invalidated.
	h.peerMu.Lock()
	defer h.peerMu.Unlock()
	h.modeMu.Lock()
	defer h.modeMu.Unlock()
	h.acpGeneration++
	h.rosterValid = false
	h.lastRosterPushAt = time.Time{}
	return h.acpGeneration
}

func (h *grokHost) stopForRosterAuthorityLoss(generation uint64) {
	// An absent actor is expected while the TUI is still starting. Once the
	// peer is published, a complete roster snapshot or global removal event is
	// authoritative: withdraw the adapter rather than leave stale state visible.
	// The peerMu -> modeMu order matches every state publication path and makes
	// the reconnect-generation check atomic with the publication check.
	h.peerMu.Lock()
	h.modeMu.Lock()
	current := generation == 0 || generation == h.acpGeneration
	published := h.peer != nil
	if current {
		h.rosterValid = false
		h.lastRosterPushAt = time.Time{}
	}
	if current && published {
		h.requestStop()
	}
	h.modeMu.Unlock()
	h.peerMu.Unlock()
}

func (h *grokHost) requestStop() {
	h.doneOnce.Do(func() { close(h.done) })
}

func (h *grokHost) ensureAgentSessionsMCPReadyLocked(ctx context.Context) error {
	// Grok 1.0.4 starts trusted plugin MCPs in the resident session, but its
	// x.ai/mcp/list catalog omits those plugin-only clients. Exercise the exact
	// server and its identity tool instead; the exact process-attested launch may
	// report a starting identity before publication. Group discovery cannot run
	// until that same publication has registered the source with the host agent.
	sessionID := h.identitySnapshot().sessionID
	result, err := h.acp.request(ctx, "_x.ai/mcp/call", map[string]any{
		"sessionId": sessionID,
		"server":    "agent_sessions",
		"tool":      "identity",
		"arguments": map[string]any{"session_id": sessionID},
	})
	if err != nil {
		return fmt.Errorf("probe live Grok agent_sessions MCP: %w", err)
	}
	return grokAgentSessionsMCPIdentityReady(result, sessionID)
}

func grokAgentSessionsMCPCallReady(response map[string]any) error {
	result, _ := response["result"].(map[string]any)
	if isError, _ := result["isError"].(bool); isError {
		return errors.New("grok agent_sessions MCP readiness tool returned an error")
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return errors.New("grok agent_sessions MCP readiness tool returned no content")
	}
	return nil
}

func grokAgentSessionsMCPIdentityReady(response map[string]any, sessionID string) error {
	if err := grokAgentSessionsMCPCallReady(response); err != nil {
		return err
	}
	result, _ := response["result"].(map[string]any)
	identity, _ := result["structuredContent"].(map[string]any)
	// Grok 1.0.4 can omit MCP structuredContent while preserving the successful
	// text result. The ACP session, launch token, and MCP process ancestry have
	// already attested the caller; validate the structured identity when the
	// client forwards it, but do not make that optional transport field a gate.
	if len(identity) == 0 {
		return nil
	}
	if stringValue(identity["sessionId"]) != sessionID {
		return errors.New("grok agent_sessions MCP readiness tool returned the wrong session identity")
	}
	return nil
}

func (h *grokHost) currentPermissionMode() string {
	h.modeMu.RLock()
	defer h.modeMu.RUnlock()
	return defaultString(h.mode, "default")
}

func (h *grokHost) currentStatus() string {
	h.modeMu.RLock()
	defer h.modeMu.RUnlock()
	return defaultString(h.status, "idle")
}

func (h *grokHost) beginActiveInterjectionPermissionSnapshot() {
	h.modeMu.Lock()
	h.activeInterjectionMode = defaultString(h.mode, "default")
	h.activeInterjectionValid = true
	h.activeInterjectionAt = time.Now()
	h.modeMu.Unlock()
}

func (h *grokHost) clearActiveInterjectionPermissionSnapshot() {
	h.modeMu.Lock()
	h.activeInterjectionMode = ""
	h.activeInterjectionValid = false
	h.activeInterjectionAt = time.Time{}
	h.modeMu.Unlock()
}

func (h *grokHost) activeInterjectionPermissionSnapshot() (string, bool) {
	h.modeMu.RLock()
	defer h.modeMu.RUnlock()
	if !h.activeInterjectionValid || h.activeInterjectionAt.IsZero() ||
		time.Since(h.activeInterjectionAt) > grokInterjectionModeTTL {
		return "", false
	}
	mode := h.activeInterjectionMode
	return mode, mode == "default" || mode == "bypassPermissions"
}

func grokRosterStateFromResponse(response map[string]any, sessionID string) (grokRosterState, error) {
	result, _ := response["result"].(map[string]any)
	sessions, ok := result["sessions"].([]any)
	if !ok {
		return grokRosterState{}, fmt.Errorf("%w: response has no sessions", errGrokRosterAuthorityLost)
	}
	state, matches, err := grokRosterStateFromRows(sessions, sessionID)
	if err != nil {
		return grokRosterState{}, fmt.Errorf("%w: %w", errGrokRosterAuthorityLost, err)
	}
	if matches != 1 {
		return grokRosterState{}, fmt.Errorf("%w: roster returned %d exact rows for %s", errGrokRosterAuthorityLost, matches, sessionID)
	}
	if state.authorityLost {
		return grokRosterState{}, fmt.Errorf("%w: actor %s is not live", errGrokRosterAuthorityLost, sessionID)
	}
	return state, nil
}

func grokRosterNotificationState(message map[string]any, sessionID string) (grokRosterState, bool) {
	params, _ := message["params"].(map[string]any)
	if nested, ok := params["params"].(map[string]any); ok {
		if method := stringValue(params["method"]); method != "" && method != "x.ai/sessions/changed" {
			return grokRosterState{}, false
		}
		params = nested
	}
	if removed, ok := params["removed"].([]any); ok {
		for _, raw := range removed {
			if stringValue(raw) == sessionID {
				return grokRosterState{fromPush: true, authorityLost: true}, true
			}
		}
	}
	rows, ok := params["upserted"].([]any)
	if !ok {
		return grokRosterState{}, false
	}
	state, matches, err := grokRosterStateFromRows(rows, sessionID)
	state.fromPush = true
	if matches == 0 {
		return grokRosterState{}, false
	}
	if err != nil || matches != 1 {
		return grokRosterState{fromPush: true, authorityLost: true}, true
	}
	return state, true
}

func grokRosterStateFromRows(rows []any, sessionID string) (grokRosterState, int, error) {
	matches := 0
	state := grokRosterState{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if stringValue(row["sessionId"]) != sessionID {
			continue
		}
		matches++
		resident, residentOK := row["resident"].(bool)
		if !residentOK {
			return grokRosterState{}, matches, errors.New("exact Grok session roster row has no resident state")
		}
		activity := stringValue(row["activity"])
		if !resident || activity == "completed" || activity == "dormant" || activity == "dead" {
			state.authorityLost = true
			continue
		}
		yolo, yoloOK := row["yolo"].(bool)
		if !yoloOK {
			return grokRosterState{}, matches, errors.New("live Grok session roster row has no yolo state")
		}
		status, err := grokRosterActivityStatus(activity)
		if err != nil {
			return grokRosterState{}, matches, err
		}
		state = grokRosterState{
			name:           strings.TrimSpace(stringValue(row["title"])),
			permissionMode: map[bool]string{true: "bypassPermissions", false: "default"}[yolo],
			status:         status,
		}
	}
	return state, matches, nil
}

func grokRosterActivityStatus(activity string) (string, error) {
	switch activity {
	case "working":
		return "busy", nil
	case "needs_input":
		return "waiting", nil
	case "idle":
		return "idle", nil
	default:
		return "", fmt.Errorf("live Grok session roster row has unsupported activity %q", activity)
	}
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
	sessionID := h.identitySnapshot().sessionID
	if stringValue(request["sessionId"]) != sessionID {
		return nil, errors.New("grok control session mismatch")
	}
	switch stringValue(request["action"]) {
	case "status":
		// Grok may defer roster responses until an interjected model turn ends.
		// Serve the immediately-preceding authoritative roster snapshot throughout
		// that interval instead of letting this MCP call become the request that
		// blocks on the same actor. The next successful background roster refresh
		// retires the snapshot.
		if permissionMode, valid := h.activeInterjectionPermissionSnapshot(); valid {
			h.peerMu.Lock()
			published := h.peer != nil
			leaderReady := h.leader != nil
			h.peerMu.Unlock()
			return map[string]any{
				"sessionId": sessionID, "leaderReady": leaderReady,
				"loaded": true, "ready": published, "permissionMode": permissionMode,
				"refreshDeferred": true, "permissionAuthority": "active_interjection_snapshot",
			}, nil
		}
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
					"sessionId": sessionID, "leaderReady": h.leader != nil,
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
			"sessionId": sessionID, "leaderReady": h.leader != nil,
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
		SessionID: h.identitySnapshot().sessionID, MessageID: messageID, Fingerprint: fingerprint,
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
	err = h.acp.requestInterjection(ctx, h.identitySnapshot().sessionID, messageID, peerMessageText(item))
	cancel()
	if err != nil {
		h.clearActiveInterjectionPermissionSnapshot()
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
		detail := "delivery outcome is unknown: " + err.Error()
		h.setWakeResult(messageID, "in_flight", detail)
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok-host: wake %s remains ambiguous and will not be replayed: %s\n", messageID, detail)
		return
	}
	h.acpMu.Unlock()
	h.setWakeResult(messageID, "actor_accepted", "")
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

//nolint:gocyclo // Every process, record, socket, wake, and provisional-identity cleanup has its own ownership predicate.
func (h *grokHost) cleanup() {
	h.requestStop()
	h.closeOnce.Do(func() {
		if h.listener != nil {
			_ = h.listener.Close()
		}
		paths := resolveNativePaths()
		hostProcStart := readProcStart(os.Getpid())
		identity := h.identitySnapshot()
		launchRecordPath := grokLaunchRecordPath(paths, identity.sessionID)
		removeDurableOwnership := func() {
			removeJSONIf(launchRecordPath, func(row map[string]any) bool {
				return stringValue(row["tokenHash"]) == grokTokenHash(h.config.LaunchToken) &&
					intValue(row["hostPid"]) == os.Getpid() && stringValue(row["hostProcStart"]) == hostProcStart
			})
		}
		removeProvisionalOwnership := func() {
			if h.config.AttachmentID == "" || h.config.AttachmentID == identity.sessionID {
				return
			}
			removeJSONIf(grokLaunchRecordPath(paths, h.config.AttachmentID), func(row map[string]any) bool {
				return stringValue(row["tokenHash"]) == grokTokenHash(h.config.LaunchToken) &&
					intValue(row["hostPid"]) == os.Getpid() && stringValue(row["hostProcStart"]) == hostProcStart
			})
		}
		h.peerMu.Lock()
		if h.peer != nil {
			h.peer.shutdown()
			h.peer = nil
		}
		h.peerMu.Unlock()
		peerStateDir := filepath.Join(paths.dataRoot, "sessions", sessionKey(identity.sessionID))
		_ = os.Remove(peerStateDir) // Only succeeds when the session left no durable inbox content.

		// Stop both private process groups before discarding their durable
		// identities. The waker is stopped from the persisted record rather than
		// through acpMu: a control request may still hold that mutex while Grok's
		// /quit process-scope teardown is already reaping this host.
		stopGrokManagedProcess(h.leader, 2*time.Second)
		if record := readGrokLaunchRecord(launchRecordPath); record != nil {
			stopStaleGrokProcess(record.WakerPID, record.WakerProcStart)
			if cleanupProcessIdentityStatus(record.LeaderPID, record.LeaderProcStart).Status == processIdentityStale &&
				cleanupProcessIdentityStatus(record.WakerPID, record.WakerProcStart).Status == processIdentityStale {
				removeDurableOwnership()
			}
		}
		removeProvisionalOwnership()
		if h.diagnostics != nil {
			if err := h.diagnostics.close(); err != nil {
				fmt.Fprintln(os.Stderr, "agent-session-runtime grok-host: close private diagnostic log failed")
			}
		}
		diagnosticPath := filepath.Join(h.paths.LaunchDir, "diagnostics.log")
		if err := os.Remove(diagnosticPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "agent-session-runtime grok-host: remove private diagnostic log failed")
		}
		// Unlink private endpoints before acquiring acpMu. The live failure this
		// ordering guards against had already reaped both child groups but killed
		// the host while a final ACP control request was unwinding.
		h.removePrivateRuntimeArtifacts(false)
		h.acpMu.Lock()
		h.closeACPLocked()
		h.acpMu.Unlock()
		// closeACPLocked provides the final process join. Retry the exact-predicate
		// ownership and endpoint cleanup now that no private child can still use it.
		removeDurableOwnership()
		h.removePrivateRuntimeArtifacts(true)
		if h.lease != nil {
			_ = syscall.Flock(int(h.lease.Fd()), syscall.LOCK_UN)
			_ = h.lease.Close()
			h.lease = nil
		}
		workersDone := make(chan struct{})
		go func() {
			h.wg.Wait()
			close(workersDone)
		}()
		timer := time.NewTimer(grokCleanupWaitTimeout)
		defer timer.Stop()
		select {
		case <-workersDone:
		case <-timer.C:
			fmt.Fprintln(os.Stderr, "agent-session-runtime grok-host: cleanup workers did not stop before the bounded shutdown deadline")
		}
	})
}

func (h *grokHost) removePrivateRuntimeArtifacts(report bool) {
	for _, path := range []string{
		h.paths.ControlSocket,
		h.paths.LeaderSocket,
		filepath.Join(h.paths.LaunchDir, "leader.lock"),
	} {
		if err := os.Remove(path); report && err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "agent-session-runtime grok-host: remove private runtime artifact %s: %v\n", path, err)
		}
	}
	if err := os.Remove(h.paths.LaunchDir); report && err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok-host: remove private launch directory %s: %v\n", h.paths.LaunchDir, err)
	}
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
	h.modeMu.RLock()
	rosterValid := h.rosterValid
	permissionMode := defaultString(h.mode, "default")
	status := defaultString(h.status, "idle")
	h.modeMu.RUnlock()
	if !rosterValid {
		return errors.New("grok host has no authoritative live roster state")
	}
	paths := resolveNativePaths()
	identity := h.identitySnapshot()
	args := map[string]string{
		"session-id": identity.sessionID, "cwd": h.config.Cwd,
		"name": identity.name, "name-source": map[bool]string{true: "explicit", false: "canonical"}[h.config.NameSpecified], "entrypoint": "grok",
		"permission-mode":   permissionMode,
		"status":            status,
		"supervisor-socket": h.paths.ControlSocket,
		"supervisor-token":  h.config.LaunchToken,
		"owner-pid":         strconvItoa(h.config.OwnerPID), "owner-proc-start": h.config.OwnerProcStart,
		"data-dir": paths.dataRoot, "claude-config-dir": paths.claudeRoot,
		"codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
		"agent-runtime-dir": h.config.AgentRuntimeDir,
	}
	if h.config.AttachmentID != "" && h.config.AttachmentID != identity.sessionID {
		args["attachment-id"] = h.config.AttachmentID
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
	flags.BoolVar(&config.NameSpecified, "name-specified", false, "published peer name was explicit")
	flags.StringVar(&config.PermissionMode, "permission-mode", "default", "published permission class")
	flags.StringVar(&config.AgentRuntimeDir, "agent-runtime-dir", "", "Agent Sessions host-agent runtime directory")
	flags.BoolVar(&config.LateBoundResume, "late-bound-resume", false, "adopt the native Grok title selection")
	groupsJSON := "[]"
	flags.StringVar(&groupsJSON, "groups-json", groupsJSON, "explicit peer groups")
	flags.BoolVar(&config.GroupsSpecified, "groups-specified", false, "explicit groups were supplied")
	flags.StringVar(&config.ParentSession, "parent-session", "", "attested parent session")
	flags.BoolVar(&config.ParentSpecified, "parent-specified", false, "parent session was supplied")
	flags.BoolVar(&config.InheritGroups, "inherit-parent-groups", false, "inherit parent groups")
	flags.BoolVar(&config.InheritSet, "inherit-groups-specified", false, "group inheritance was supplied")
	flags.BoolVar(&config.AlwaysApprove, "always-approve", false, "requested durable yolo policy")
	flags.BoolVar(&config.AlwaysSet, "always-approve-specified", false, "yolo policy was supplied")
	if err := flags.Parse(argv); err != nil || flags.NArg() != 0 {
		return 2
	}
	if json.Unmarshal([]byte(groupsJSON), &config.Groups) != nil {
		fmt.Fprintln(os.Stderr, "agent-session-runtime grok-host: invalid groups JSON")
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
