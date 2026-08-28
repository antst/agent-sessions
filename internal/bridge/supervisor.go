package bridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/socketpath"
)

type nativePaths struct {
	dataRoot        string
	profileRoot     string
	profileKey      string
	claudeRoot      string
	codexHome       string
	runtimeDir      string
	supervisorSock  string
	supervisorState string
	appServerSock   string
}

func resolveNativePaths() nativePaths {
	home, _ := os.UserHomeDir()
	data := firstEnv("CLAUDE_PEER_DATA_DIR", filepath.Join(firstEnv("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "claude-code-peer"))
	claude := firstEnv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", firstEnv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")))
	codex := firstEnv("CODEX_HOME", filepath.Join(home, ".codex"))
	uid := strconv.Itoa(os.Getuid())
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" && runtime.GOOS == "linux" {
		candidate := filepath.Join("/run/user", uid)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			runtimeDir = candidate
		}
	}
	if runtimeDir == "" {
		runtimeDir = os.TempDir()
	}
	runtimeRoot := bridgeRuntimeRoot(runtimeDir, os.Getuid())
	profileKey := nativeProfileKey(codex)
	profileRoot := filepath.Join(data, "profiles", profileKey)
	return nativePaths{
		dataRoot: data, profileRoot: profileRoot, profileKey: profileKey,
		claudeRoot: claude, codexHome: codex, runtimeDir: runtimeDir,
		supervisorSock:  firstEnv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(runtimeRoot, "supervisor-"+profileKey+".sock")),
		supervisorState: filepath.Join(profileRoot, "supervisor.json"),
		appServerSock:   firstEnv("CLAUDE_PEER_APP_SERVER_SOCKET", filepath.Join(codex, "app-server-control", "app-server-control.sock")),
	}
}

func nativeProfileKey(codexHome string) string {
	canonical, err := filepath.Abs(codexHome)
	if err != nil {
		canonical = filepath.Clean(codexHome)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
		canonical = resolved
	}
	return sessionKey(canonical)
}

func profileDataRoot(paths nativePaths) string {
	if paths.profileRoot != "" {
		return paths.profileRoot
	}
	return paths.dataRoot
}

func firstEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func retiredThreadPath(paths nativePaths, threadID string) string {
	return filepath.Join(profileDataRoot(paths), "retired", sessionKey(threadID)+".json")
}

func markRetiredThread(paths nativePaths, threadID string) error {
	if !validSessionID(threadID) {
		return errors.New("invalid retired Codex thread id")
	}
	return writeJSONAtomic(retiredThreadPath(paths, threadID), map[string]any{
		"threadId": threadID, "retiredAt": time.Now().UnixMilli(),
	})
}

func readRetiredThreads(paths nativePaths) map[string]bool {
	result := map[string]bool{}
	directory := filepath.Join(profileDataRoot(paths), "retired")
	entries, _ := os.ReadDir(directory)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		row := readJSONMap(filepath.Join(directory, entry.Name()))
		threadID := stringValue(row["threadId"])
		if validSessionID(threadID) && entry.Name() == sessionKey(threadID)+".json" {
			result[threadID] = true
		}
	}
	return result
}

func isRetiredThreadNative(paths nativePaths, threadID string) bool {
	if !validSessionID(threadID) {
		return false
	}
	row := readJSONMap(retiredThreadPath(paths, threadID))
	return stringValue(row["threadId"]) == threadID
}

func clearRetiredThread(paths nativePaths, threadID string) {
	file := retiredThreadPath(paths, threadID)
	removeJSONIf(file, func(row map[string]any) bool {
		return stringValue(row["threadId"]) == threadID
	})
}

func (s *nativeSupervisor) markRetired(threadID string) error {
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	if err := markRetiredThread(s.paths, threadID); err != nil {
		return err
	}
	s.mu.Lock()
	if s.retired == nil {
		s.retired = map[string]bool{}
	}
	s.retired[threadID] = true
	s.mu.Unlock()
	return nil
}

func (s *nativeSupervisor) clearRetired(threadID string) {
	if threadID == "" {
		return
	}
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	clearRetiredThread(s.paths, threadID)
	s.mu.Lock()
	delete(s.retired, threadID)
	s.mu.Unlock()
}

func (s *nativeSupervisor) isRetired(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := readJSONMap(retiredThreadPath(s.paths, threadID))
	if stringValue(row["threadId"]) == threadID {
		if s.retired == nil {
			s.retired = map[string]bool{}
		}
		s.retired[threadID] = true
		return true
	}
	if s.retired != nil {
		delete(s.retired, threadID)
	}
	return false
}

type appTurn struct {
	ID          string            `json:"id"`
	Status      any               `json:"status"`
	Items       []json.RawMessage `json:"items"`
	Error       any               `json:"error"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	DurationMS  *int64            `json:"durationMs"`
}

type appThread struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"sessionId"`
	CreatedAt      int64     `json:"createdAt"`
	UpdatedAt      int64     `json:"updatedAt"`
	Cwd            string    `json:"cwd"`
	Name           string    `json:"name"`
	Path           string    `json:"path"`
	Source         any       `json:"source"`
	ParentThreadID string    `json:"parentThreadId"`
	Status         any       `json:"status"`
	Turns          []appTurn `json:"turns"`
}

type interactiveOwnerRecord struct {
	ThreadID       string `json:"threadId"`
	RequestID      string `json:"requestId"`
	OwnerPID       int    `json:"ownerPid"`
	OwnerProcStart string `json:"ownerProcStart"`
	Pending        bool   `json:"pending,omitempty"`
	Prepared       bool   `json:"prepared,omitempty"`
	DeleteOnAbort  bool   `json:"deleteOnAbort,omitempty"`
	ParkOnAbort    bool   `json:"parkOnAbort,omitempty"`
	ResumeLoaded   bool   `json:"resumeLoaded,omitempty"`
	Aborting       bool   `json:"aborting,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	Name           string `json:"name,omitempty"`
	NameSource     string `json:"nameSource,omitempty"`
	UpdatedAt      int64  `json:"updatedAt"`
}

type nativeSupervisor struct {
	paths           nativePaths
	pluginVersion   string
	executable      string
	runtimeIdentity string
	shimExecutable  string
	procStart       string
	startedAt       int64

	listener net.Listener
	done     chan struct{}
	stopOnce sync.Once
	ready    bool

	clientMu sync.Mutex
	client   *appServerClient

	mu                sync.Mutex
	shims             map[string]map[string]any
	activeTurns       map[string]string
	subscribed        map[string]bool
	releasing         map[string]int64
	retired           map[string]bool
	closedCandidates  map[string]uint64
	retirementAudited bool
	reconcileWake     chan struct{}
	ensureMu          sync.Mutex
	ownerMu           sync.Mutex
	subscribeMu       sync.Mutex
	noticeMu          sync.Mutex
	wakeMu            sync.Mutex
	schemaMu          sync.Mutex
}

type wakeRecord struct {
	SessionID           string         `json:"sessionId"`
	MessageID           string         `json:"messageId"`
	Fingerprint         string         `json:"fingerprint"`
	DeliveryFingerprint string         `json:"deliveryFingerprint"`
	State               string         `json:"state"`
	Delivery            string         `json:"delivery,omitempty"`
	TurnID              string         `json:"turnId,omitempty"`
	Item                map[string]any `json:"item"`
	OwnerPID            int            `json:"ownerPid"`
	OwnerProcStart      string         `json:"ownerProcStart"`
	PriorLatestTurnID   string         `json:"priorLatestTurnId,omitempty"`
	PriorAutoArchiveAt  int64          `json:"priorAutoArchiveAt,omitempty"`
	CreatedAt           int64          `json:"createdAt"`
	UpdatedAt           int64          `json:"updatedAt"`
}

// wakeDeliveryUncertainError means a mutating App Server request was written,
// but its terminal response was not observed.  Falling back immediately in
// that state can deliver the same message both through the turn and the inbox.
type wakeDeliveryUncertainError struct {
	err error
}

func (e *wakeDeliveryUncertainError) Error() string { return e.err.Error() }
func (e *wakeDeliveryUncertainError) Unwrap() error { return e.err }

func newNativeSupervisor() (*nativeSupervisor, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	runtimeIdentity, err := nativeExecutableIdentity(executable)
	if err != nil {
		return nil, err
	}
	paths := resolveNativePaths()
	return &nativeSupervisor{
		paths: paths, pluginVersion: "test",
		executable: executable, runtimeIdentity: runtimeIdentity, shimExecutable: executable,
		procStart: readProcStart(os.Getpid()), startedAt: time.Now().UnixMilli(),
		done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, releasing: map[string]int64{},
		retired: readRetiredThreads(paths), closedCandidates: map[string]uint64{}, reconcileWake: make(chan struct{}, 1),
	}, nil
}

func (s *nativeSupervisor) start() error {
	if s.runtimeIdentity == "" {
		identity, err := nativeExecutableIdentity(s.executable)
		if err != nil {
			return err
		}
		s.runtimeIdentity = identity
	}
	if err := ensurePrivateRuntimeDir(filepath.Dir(s.paths.supervisorSock)); err != nil {
		return err
	}
	if err := stopLegacyNativeSupervisor(s.paths); err != nil {
		return err
	}
	cleanupStaleBridgeArtifacts(s.paths)
	if probeUnixSocket(s.paths.supervisorSock, 250*time.Millisecond) {
		return fmt.Errorf("refuse to replace live supervisor socket %s", s.paths.supervisorSock)
	}
	if err := socketpath.Validate(s.paths.supervisorSock); err != nil {
		return fmt.Errorf("validate Codex supervisor socket: %w", err)
	}
	_ = os.Remove(s.paths.supervisorSock)
	listener, err := net.Listen("unix", s.paths.supervisorSock)
	if err != nil {
		return err
	}
	s.listener = listener
	_ = os.Chmod(s.paths.supervisorSock, 0600)
	if err := writeJSONAtomic(s.paths.supervisorState, map[string]any{
		"pid": os.Getpid(), "procStart": s.procStart, "pluginVersion": s.pluginVersion,
		"runtimeIdentity": s.runtimeIdentity,
		"controlSocket":   s.paths.supervisorSock, "appServerSocket": s.paths.appServerSock,
		"implementation": "go", "startedAt": s.startedAt,
	}); err != nil {
		_ = listener.Close()
		return err
	}
	if _, err := s.ensureClient(); err != nil {
		_ = listener.Close()
		return err
	}
	if err := s.reconcile(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: initial reconciliation failed: %v\n", err)
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	go s.acceptLoop()
	go s.reconcileLoop()
	return nil
}

func (s *nativeSupervisor) ensureClient() (*appServerClient, error) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client != nil {
		select {
		case <-s.client.done:
			s.client = nil
		default:
			return s.client, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, s.paths.appServerSock)
	if err != nil {
		return nil, err
	}
	s.client = client
	go s.notificationLoop(client)
	return client, nil
}

func (s *nativeSupervisor) notificationLoop(client *appServerClient) {
	for {
		select {
		case notification := <-client.notifications:
			s.handleNotification(notification)
		case <-client.done:
			s.mu.Lock()
			s.subscribed = map[string]bool{}
			s.retirementAudited = false
			s.mu.Unlock()
			return
		case <-s.done:
			return
		}
	}
}

//nolint:gocyclo // Cases intentionally mirror the App Server notification protocol.
func (s *nativeSupervisor) handleNotification(notification rpcNotification) {
	var params map[string]any
	if json.Unmarshal(notification.Params, &params) != nil {
		return
	}
	threadID := stringValue(params["threadId"])
	switch notification.Method {
	case "thread/started":
		body, _ := json.Marshal(params["thread"])
		var thread appThread
		if json.Unmarshal(body, &thread) == nil && thread.ID != "" {
			go s.handleThreadStarted(thread)
		}
	case "thread/status/changed":
		status := statusType(params["status"])
		if threadID != "" && s.authorizedPeerThread(threadID) {
			go s.updateShimStatus(threadID, map[bool]string{true: "busy", false: "idle"}[status == "active"])
		}
	case "turn/started":
		turn, _ := params["turn"].(map[string]any)
		if threadID != "" && s.authorizedPeerThread(threadID) {
			if turnID := stringValue(turn["id"]); turnID != "" {
				s.mu.Lock()
				s.activeTurns[threadID] = turnID
				s.mu.Unlock()
				if s.activeCodexLaneThread(threadID) {
					_ = s.recordLaneTurnStarted(threadID, turnID)
				}
			}
			go s.updateShimStatus(threadID, "busy")
		}
	case "thread/tokenUsage/updated":
		turnID := stringValue(params["turnId"])
		if threadID != "" && turnID != "" && s.activeCodexLaneThread(threadID) {
			_ = writeJSONAtomic(laneMetricPath(s.paths, threadID, turnID), map[string]any{
				"threadId": threadID, "turnId": turnID, "tokenUsage": params["tokenUsage"],
				"updatedAt": time.Now().UnixMilli(),
			})
		}
	case "item/completed":
		if threadID != "" && s.activeCodexLaneThread(threadID) {
			turnID := laneNotificationTurnID(params)
			item, _ := params["item"].(map[string]any)
			if turnID == "" {
				s.mu.Lock()
				turnID = s.activeTurns[threadID]
				s.mu.Unlock()
			}
			if turnID != "" && item != nil {
				if err := persistLaneItem(s.paths, threadID, turnID, item); err != nil {
					fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: persist lane item: %v\n", err)
				}
			}
		}
	case "turn/completed":
		if threadID != "" && s.activeCodexLaneThread(threadID) {
			turn, _ := params["turn"].(map[string]any)
			go s.handleLaneTurnCompleted(threadID, turn)
		} else if threadID != "" && s.authorizedPeerThread(threadID) {
			s.mu.Lock()
			delete(s.activeTurns, threadID)
			s.mu.Unlock()
			go s.updateShimStatus(threadID, "idle")
		}
	case "thread/name/updated":
		if threadID != "" && s.authorizedPeerThread(threadID) {
			go s.updateShimName(threadID, stringValue(params["threadName"]))
		}
	case "thread/archived":
		s.handleDestructiveThreadNotification(threadID, "archived")
	case "thread/deleted":
		s.handleDestructiveThreadNotification(threadID, "deleted")
	case "thread/unarchived":
		s.handleDestructiveThreadNotification(threadID, "unarchived")
	case "thread/closed":
		// This notification identifies only a thread, not the client attachment
		// that closed. Queue one bounded reconciliation pass and remove authority
		// only after App Server confirms that no attachment still has the thread
		// loaded. The periodic pass retains failed candidates for retry.
		s.queueClosedInteractiveThread(threadID)
	}
}

func (s *nativeSupervisor) queueClosedInteractiveThread(threadID string) {
	if !validSessionID(threadID) {
		return
	}
	s.mu.Lock()
	if s.closedCandidates == nil {
		s.closedCandidates = map[string]uint64{}
	}
	s.closedCandidates[threadID]++
	s.mu.Unlock()
	s.requestReconcile()
}

func (s *nativeSupervisor) requestReconcile() {
	wake := s.reconcileWake
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (s *nativeSupervisor) handleDestructiveThreadNotification(threadID, event string) {
	if !validSessionID(threadID) {
		return
	}
	// Owner-exit parking holds the lifecycle lock while archive/unarchive RPCs
	// run. Their notifications are dispatched on a different goroutine and must
	// be ignored before trying that same lock; otherwise they can wait past the
	// release guard and retire a successfully restored replacement.
	if s.interactiveReleasePending(threadID) {
		switch event {
		case "archived":
			return
		case "unarchived":
			// The release transaction owns ensureMu while App Server performs
			// archive/unarchive. Clear any older retirement marker without
			// waiting on that same mutex, then reconcile after the transaction.
			clearRetiredThread(s.paths, threadID)
			s.mu.Lock()
			delete(s.retired, threadID)
			s.mu.Unlock()
			s.requestReconcile()
			return
		}
	}
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return
	}
	defer unlockLaneLifecycle(lifecycleLock)
	if !s.knownPeerLineage(threadID) {
		return
	}
	switch event {
	case "archived":
		// Exact owner-exit cleanup briefly archives and immediately unarchives
		// an interactive root because App Server has no public unload RPC.
		// The old shim was removed synchronously before archive. Ignore its
		// delayed notification completely while the release guard is live: an
		// exact prepared replacement may already have published a new shim.
		if s.interactiveReleasePending(threadID) {
			return
		}
		_ = s.markRetired(threadID)
		s.clearInteractiveOwner(threadID)
		s.removeShim(threadID)
	case "deleted":
		owner := readInteractiveOwner(s.paths, threadID)
		if owner == nil || !owner.Pending {
			_ = s.markRetired(threadID)
		} else {
			s.clearRetired(threadID)
		}
		s.clearInteractiveOwner(threadID)
		s.removeShim(threadID)
	case "unarchived":
		s.clearRetired(threadID)
		s.requestReconcile()
	}
}

// reconcileClosedInteractiveThreadWithLoaded returns true once this close
// candidate no longer needs retry. Unknown owner identity is retained until a
// later pass can prove whether authority is still safe to remove.
func (s *nativeSupervisor) reconcileClosedInteractiveThreadWithLoaded(threadID string, loaded bool) bool {
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return false
	}
	defer unlockLaneLifecycle(lifecycleLock)
	if s.activeCodexLaneThread(threadID) {
		return true
	}
	owner := readInteractiveOwner(s.paths, threadID)
	if owner == nil || owner.Pending {
		return true
	}
	switch exactProcessIdentityStatus(owner.OwnerPID, owner.OwnerProcStart).Status {
	case processIdentityUnknown:
		return false
	case processIdentityStale:
		return true
	case processIdentityMatches:
	}
	if loaded {
		return true
	}
	removeInteractiveOwnerIfMatching(s.paths, threadID, owner)
	s.removeShim(threadID)
	return true
}

func (s *nativeSupervisor) handleThreadStarted(thread appThread) {
	if thread.Path != "" && s.authorizedPeerThread(thread.ID) {
		_, _ = s.subscribeThread(thread.ID)
	}
}

// authorizedPeerThread is the supervisor-side capability boundary. App Server
// discovery and daemon-wide hooks may observe ordinary threads, but only an
// exact attached owner, an exact fresh prepared owner awaiting SessionStart,
// or a durable unarchived lane may publish or receive peer work.
func (s *nativeSupervisor) authorizedPeerThread(threadID string) bool {
	return publishablePeerThreadNative(s.paths, threadID)
}

func (s *nativeSupervisor) activeCodexLaneThread(threadID string) bool {
	return activeCodexLaneThreadNative(s.paths, threadID)
}

type peerThreadCapability uint8

const (
	peerCapabilityNone peerThreadCapability = iota
	peerCapabilityPrepared
	peerCapabilityInteractive
	peerCapabilityLane
)

func peerCapabilityNative(paths nativePaths, threadID string) peerThreadCapability {
	if !validSessionID(threadID) || isRetiredThreadNative(paths, threadID) {
		return peerCapabilityNone
	}
	if owner := liveAuthorizedInteractiveOwnerRecord(paths, threadID); owner != nil {
		if owner.Pending {
			return peerCapabilityPrepared
		}
		return peerCapabilityInteractive
	}
	state, err := readLaneStateFile(paths, threadID)
	if err == nil && state.Type == "codex-peer-lane" && state.Status != "archived" {
		return peerCapabilityLane
	}
	return peerCapabilityNone
}

func authorizedPeerThreadNative(paths nativePaths, threadID string) bool {
	capability := peerCapabilityNative(paths, threadID)
	return capability == peerCapabilityInteractive || capability == peerCapabilityLane
}

// publishablePeerThreadNative additionally recognizes a bridge-prepared fresh
// thread while the exact launcher process is alive. That narrow pre-attachment
// capability may publish and receive an idle wake, but it does not authorize
// MCP/dynamic tool calls until SessionStart promotes the owner to attached.
func publishablePeerThreadNative(paths nativePaths, threadID string) bool {
	return peerCapabilityNative(paths, threadID) != peerCapabilityNone
}

func activeCodexLaneThreadNative(paths nativePaths, threadID string) bool {
	if isRetiredThreadNative(paths, threadID) {
		return false
	}
	state, err := readLaneStateFile(paths, threadID)
	return err == nil && state.Type == "codex-peer-lane" && state.Status != "archived"
}

func liveInteractiveOwnerRecord(paths nativePaths, threadID string) *interactiveOwnerRecord {
	owner := liveAuthorizedInteractiveOwnerRecord(paths, threadID)
	if owner == nil || owner.Pending {
		return nil
	}
	return owner
}

func liveAuthorizedInteractiveOwnerRecord(paths nativePaths, threadID string) *interactiveOwnerRecord {
	if isRetiredThreadNative(paths, threadID) {
		return nil
	}
	owner := readInteractiveOwner(paths, threadID)
	if owner == nil || owner.Aborting || (owner.Pending && !owner.Prepared) ||
		!exactProcessIdentityMatch(owner.OwnerPID, owner.OwnerProcStart) {
		return nil
	}
	return owner
}

func interactiveOwnerMatchesRequest(record *interactiveOwnerRecord, request map[string]any) bool {
	return record != nil &&
		record.ThreadID == stringValue(request["sessionId"]) &&
		record.RequestID == stringValue(request["requestId"]) &&
		record.OwnerPID == intValue(request["ownerPid"]) &&
		record.OwnerProcStart == stringValue(request["ownerProcStart"])
}

func preparedOwnerMatchesRequest(record *interactiveOwnerRecord, request map[string]any) bool {
	return interactiveOwnerMatchesRequest(record, request) && record.Pending && record.Prepared && !record.Aborting
}

func (s *nativeSupervisor) registerPreparedLaunch(request map[string]any) (map[string]any, error) {
	threadID := stringValue(request["sessionId"])
	replacingReleasedOwner := s.interactiveReleasePending(threadID)
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return nil, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	record := readInteractiveOwner(s.paths, threadID)
	if !preparedOwnerMatchesRequest(record, request) {
		return nil, errors.New("prepared Codex peer owner changed before publication")
	}
	client, err := s.ensureClient()
	if err != nil {
		return nil, err
	}
	if !record.DeleteOnAbort {
		archived, listErr := listThreadMembership(client, true)
		if listErr != nil {
			return nil, listErr
		}
		if archived[threadID] || isRetiredThreadNative(s.paths, threadID) {
			return nil, fmt.Errorf("codex thread %s became archived before peer publication", threadID)
		}
		loaded, listErr := loadedPreparedThreads(client)
		if listErr != nil {
			return nil, listErr
		}
		if loaded[threadID] && !record.ResumeLoaded {
			return nil, fmt.Errorf("codex thread %s became loaded before peer publication", threadID)
		}
	}
	// Starting publication is the irreversible durability boundary. A shim can
	// become externally connectable before subscribe/response completes, so any
	// failure from this point must park and preserve the thread, never delete it.
	record.DeleteOnAbort = false
	record.ParkOnAbort = true
	record.UpdatedAt = time.Now().UnixMilli()
	if err := writeInteractiveOwnerRecord(s.paths, *record); err != nil {
		return nil, err
	}
	thread, approvalPolicy, err := resumePreparedThread(client, threadID, request)
	if err != nil {
		return nil, err
	}
	publication := make(map[string]any, len(request)+1)
	for key, value := range request {
		publication[key] = value
	}
	publication["allowInteractiveRelease"] = replacingReleasedOwner
	publication["permissionMode"] = permissionModeForApprovalPolicy(approvalPolicy)
	state, err := s.ensureShim(publication)
	if err == nil {
		s.mu.Lock()
		s.subscribed[threadID] = true
		s.mu.Unlock()
		return map[string]any{
			"state": state, "thread": thread, "approvalPolicy": approvalPolicy,
		}, nil
	}
	s.removeShim(threadID)
	return nil, err
}

func (s *nativeSupervisor) abortPreparedLaunch(request map[string]any) (map[string]any, error) {
	threadID := stringValue(request["sessionId"])
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return nil, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	record := readInteractiveOwner(s.paths, threadID)
	if !preparedOwnerMatchesRequest(record, request) {
		return map[string]any{"sessionId": threadID, "preserved": true}, nil
	}
	switch exactProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart).Status {
	case processIdentityUnknown:
		return map[string]any{"sessionId": threadID, "preserved": true}, errors.New("cannot corroborate the prepared Codex peer owner during abort")
	case processIdentityStale:
		// Definite owner death belongs to the normal stale-owner reaper. Keeping
		// this transaction untouched makes the destructive proof and retry path
		// identical whether the launcher vanished before or during rollback.
		return map[string]any{"sessionId": threadID, "preserved": true}, nil
	case processIdentityMatches:
	}
	record.Aborting = true
	record.UpdatedAt = time.Now().UnixMilli()
	if err := writeInteractiveOwnerRecord(s.paths, *record); err != nil {
		return nil, err
	}
	s.removeShim(threadID)
	if record.ParkOnAbort {
		// A committed zero-turn launch may remain loaded after its TUI exits.
		// Preserve this exact stale owner as takeover proof instead of using
		// archive/unarchive as an unload surrogate: archiving races immediate
		// resume and moves the rollout out of the live sessions tree.
		return map[string]any{"sessionId": threadID, "aborted": true, "preserved": true}, nil
	}
	if !record.DeleteOnAbort {
		removeInteractiveOwnerIfMatching(s.paths, threadID, record)
		return map[string]any{"sessionId": threadID, "aborted": true, "preserved": true}, nil
	}
	client, err := s.ensureClient()
	if err != nil {
		return nil, err
	}
	if err := deletePreparedThread(client, threadID); err != nil {
		return nil, err
	}
	removeInteractiveOwnerIfMatching(s.paths, threadID, record)
	return map[string]any{"sessionId": threadID, "aborted": true}, nil
}

func (s *nativeSupervisor) releaseStaleInteractiveOwner(threadID string) error {
	if !validSessionID(threadID) {
		return errors.New("release requires a valid Codex thread id")
	}
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	record := readInteractiveOwner(s.paths, threadID)
	if record == nil || record.Pending {
		return errors.New("loaded Codex thread has no releasable attached owner")
	}
	switch exactProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart).Status {
	case processIdentityMatches:
		return errors.New("refuse to release a live Codex peer owner")
	case processIdentityUnknown:
		return errors.New("cannot corroborate the loaded Codex peer owner")
	case processIdentityStale:
	}
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return s.releaseInteractiveThreadLocked(threadID, record)
}

func (s *nativeSupervisor) detachStalePreparedOwner(request map[string]any) error {
	threadID := stringValue(request["sessionId"])
	if !validSessionID(threadID) {
		return errors.New("detach requires a valid Codex thread id")
	}
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	record := readInteractiveOwner(s.paths, threadID)
	if !interactiveOwnerMatchesRequest(record, request) || !record.Pending || !record.Prepared || !record.ParkOnAbort {
		return errors.New("loaded Codex thread has no matching stale prepared owner")
	}
	switch exactProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart).Status {
	case processIdentityMatches:
		return errors.New("refuse to detach a live prepared Codex peer owner")
	case processIdentityUnknown:
		return errors.New("cannot corroborate the stale prepared Codex peer owner")
	case processIdentityStale:
	}
	s.subscribeMu.Lock()
	defer s.subscribeMu.Unlock()
	client, err := s.ensureClient()
	if err != nil {
		return err
	}
	if err := requestWithTimeout(client, 5*time.Second, "thread/unsubscribe", map[string]any{"threadId": threadID}, nil); err != nil {
		return fmt.Errorf("unsubscribe stale prepared Codex peer %s: %w", threadID, err)
	}
	s.mu.Lock()
	delete(s.subscribed, threadID)
	delete(s.activeTurns, threadID)
	s.mu.Unlock()
	return nil
}

func (s *nativeSupervisor) knownPeerLineage(threadID string) bool {
	if !validSessionID(threadID) {
		return false
	}
	if readInteractiveOwner(s.paths, threadID) != nil {
		return true
	}
	if state, err := readLaneStateFile(s.paths, threadID); err == nil && state.Type == "codex-peer-lane" {
		return true
	}
	return isRetiredThreadNative(s.paths, threadID)
}

func (s *nativeSupervisor) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}
		go s.handleControlConn(conn)
	}
}

func (s *nativeSupervisor) handleControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(preparedPublicationTimeout))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	if !scanner.Scan() {
		return
	}
	var request map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		s.writeControlResponse(conn, nil, err)
		return
	}
	result, err := s.handleControl(request)
	s.writeControlResponse(conn, result, err)
}

func (s *nativeSupervisor) writeControlResponse(conn net.Conn, result map[string]any, err error) {
	response := map[string]any{"ok": err == nil}
	if err != nil {
		response["error"] = err.Error()
	} else {
		for key, value := range result {
			response[key] = value
		}
	}
	body, _ := json.Marshal(response)
	_, _ = conn.Write(append(body, '\n'))
}

//nolint:gocyclo // Control actions are intentionally explicit at the socket security boundary.
func (s *nativeSupervisor) handleControl(request map[string]any) (map[string]any, error) {
	switch stringValue(request["action"]) {
	case "ping", "status":
		return s.status(), nil
	case "register":
		delete(request, "allowInteractiveRelease")
		owner := readInteractiveOwner(s.paths, stringValue(request["sessionId"]))
		if owner != nil && owner.Pending {
			return nil, errors.New("prepared Codex peer registration requires its exact launch transaction")
		}
		if !s.authorizedPeerThread(stringValue(request["sessionId"])) {
			return nil, errors.New("refuse to publish an unauthorized Codex thread")
		}
		if owner != nil && owner.Prepared && owner.ParkOnAbort && exactProcessIdentityMatch(owner.OwnerPID, owner.OwnerProcStart) {
			request["allowInteractiveRelease"] = true
		}
		state, err := s.ensureShim(request)
		allowInteractiveRelease, _ := request["allowInteractiveRelease"].(bool)
		if errors.Is(err, errInteractiveSessionEnding) && !allowInteractiveRelease {
			// A new client may attach while owner-exit reconciliation parks an older one.
			// ensureShim waited on ensureMu, so the park is complete here; cancel
			// its grace marker and publish the replacement attachment.
			s.cancelInteractiveRelease(stringValue(request["sessionId"]))
			state, err = s.ensureShim(request)
		}
		if err != nil {
			return nil, err
		}
		thread, err := s.subscribeThread(stringValue(request["sessionId"]))
		return map[string]any{"state": state, "thread": thread}, err
	case "register_prepared":
		return s.registerPreparedLaunch(request)
	case "abort_prepared":
		return s.abortPreparedLaunch(request)
	case "release_stale_interactive":
		sessionID := stringValue(request["sessionId"])
		if err := s.releaseStaleInteractiveOwner(sessionID); err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": sessionID, "released": true}, nil
	case "detach_stale_prepared":
		sessionID := stringValue(request["sessionId"])
		if err := s.detachStalePreparedOwner(request); err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": sessionID, "detached": true}, nil
	case "wake":
		item, _ := request["item"].(map[string]any)
		delivery, err := s.queueWake(stringValue(request["sessionId"]), item)
		return map[string]any{"delivery": delivery}, err
	case "wake_status":
		record := readWakeRecord(s.paths, stringValue(request["sessionId"]), stringValue(request["messageId"]))
		if record == nil {
			return map[string]any{"delivery": "absent"}, nil
		}
		return map[string]any{"delivery": record.State, "turnId": record.TurnID}, nil
	case "retire":
		sessionID := stringValue(request["sessionId"])
		if !validSessionID(sessionID) {
			return nil, errors.New("retire requires a valid Codex thread id")
		}
		if err := s.markRetired(sessionID); err != nil {
			return nil, err
		}
		s.removeShim(sessionID)
		if client, err := s.ensureClient(); err == nil {
			_ = requestWithTimeout(client, 5*time.Second, "thread/unsubscribe", map[string]any{"threadId": sessionID}, nil)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			state := readJSONMap(filepath.Join(s.paths.dataRoot, "sessions", sessionKey(sessionID), "state.json"))
			if state == nil || !probeUnixSocket(stringValue(state["socketPath"]), 100*time.Millisecond) {
				return map[string]any{"sessionId": sessionID, "retired": true}, nil
			}
			time.Sleep(25 * time.Millisecond)
		}
		return nil, fmt.Errorf("timed out retiring Claude peer shim for %s", sessionID)
	case "flush_notices":
		sessionID := stringValue(request["sessionId"])
		pending, err := s.flushLaneNotices(sessionID)
		return map[string]any{"sessionId": sessionID, "pending": pending}, err
	case "timeout_turn":
		sessionID := stringValue(request["sessionId"])
		turnID := stringValue(request["turnId"])
		if err := s.timeoutLaneTurn(sessionID, turnID); err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": sessionID, "turnId": turnID, "outcome": "timed_out"}, nil
	case "schema_terminal":
		sessionID := stringValue(request["sessionId"])
		turnID := stringValue(request["turnId"])
		action, attempt, err := s.processLaneSchemaTerminal(sessionID, turnID)
		return map[string]any{
			"sessionId": sessionID, "turnId": turnID,
			"retried": action == laneSchemaRetried, "attempt": attempt,
		}, err
	case "stop":
		go func() {
			time.Sleep(20 * time.Millisecond)
			s.shutdown()
		}()
		return map[string]any{"stopping": true}, nil
	default:
		return nil, fmt.Errorf("unknown supervisor action: %s", stringValue(request["action"]))
	}
}

func (s *nativeSupervisor) handleLaneTurnCompleted(threadID string, turn map[string]any) {
	turnID := stringValue(turn["id"])
	if err := persistLaneTerminalObservation(s.paths, threadID, turn); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: persist lane terminal: %v\n", err)
	}
	if !laneTurnIsCollectionHead(s.paths, threadID, turnID) {
		// A newer ordinary (or non-completed) turn is already final even while an
		// older result is owed. Schema-constrained completed turns must wait until
		// they reach the queue head so validation/correction cannot be skipped.
		if state, err := readLaneStateFile(s.paths, threadID); err == nil &&
			(normalizeStatus(turn["status"]) != "completed" || len(state.OutputSchema) == 0) {
			s.recordLaneAutoArchiveTerminal(threadID, turnID)
		}
		s.mu.Lock()
		if s.activeTurns[threadID] == turnID {
			delete(s.activeTurns, threadID)
		}
		stillActive := s.activeTurns[threadID] != ""
		s.mu.Unlock()
		if !stillActive {
			go s.updateShimStatus(threadID, "idle")
		}
		return
	}
	action := laneSchemaNone
	var err error
	if normalizeStatus(turn["status"]) == "completed" {
		action, _, err = s.processLaneSchemaTerminal(threadID, turnID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: lane schema validation failed: %v\n", err)
		turn = map[string]any{"id": turnID, "status": "failed", "error": err.Error()}
	}
	if action == laneSchemaRetried {
		go s.updateShimStatus(threadID, "busy")
		return
	}
	if !s.commitLaneTerminal(threadID, turnID, normalizeStatus(turn["status"]), turn) {
		return
	}
	s.mu.Lock()
	delete(s.activeTurns, threadID)
	s.mu.Unlock()
	go s.updateShimStatus(threadID, "idle")
}

func (s *nativeSupervisor) commitLaneTerminal(threadID, turnID, status string, turn map[string]any) bool {
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return false
	}
	defer unlockLaneLifecycle(lifecycleLock)
	state, err := readLaneStateFile(s.paths, threadID)
	if err != nil || state.Status == "archived" || isRetiredThreadNative(s.paths, threadID) {
		return false
	}
	s.recordLaneAutoArchiveTerminal(threadID, turnID)
	s.recordLaneTurnTerminal(threadID, turnID, status)
	if state.NotifyTarget != "" {
		s.queueLaneTerminalNotice(threadID, turn)
	}
	return true
}

func (s *nativeSupervisor) recordLaneAutoArchiveTerminal(threadID, turnID string) {
	lock, err := lockLaneStateFile(s.paths, threadID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(lock)
	state, err := readLaneStateFile(s.paths, threadID)
	if err != nil || turnID == "" || state.LatestTurnID != turnID || state.Status == "archived" || isRetiredThreadNative(s.paths, threadID) {
		return
	}
	if state.AutoArchive && state.AutoArchiveAt == 0 {
		state.AutoArchiveAt = time.Now().Add(laneAutoArchiveDelay(state)).UnixMilli()
	} else if !state.AutoArchive {
		state.AutoArchiveAt = 0
	}
	_ = writeLaneStateUnlocked(s.paths, state)
}

// recordLaneTurnStarted records the newest turn without overwriting a terminal
// turn still owed to the collector. TurnID is the collection cursor;
// LatestTurnID is current App Server activity.
func (s *nativeSupervisor) recordLaneTurnStarted(threadID, turnID string) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	lock, lockErr := lockLaneStateFile(s.paths, threadID)
	if lockErr != nil {
		return lockErr
	}
	defer unlockLaneStateFile(lock)
	state, err := readLaneStateFile(s.paths, threadID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if turnID == "" {
		return errors.New("cannot record an empty lane turn id")
	}
	if state.Status == "archived" || isRetiredThreadNative(s.paths, threadID) {
		return fmt.Errorf("codex lane %s is archived", threadID)
	}
	if state.LatestTurnID == turnID {
		return nil
	}
	wasPending := laneTurnPending(state, turnID)
	appendLanePendingTurn(&state, turnID)
	state.LatestTurnID = turnID
	state.AutoArchiveAt = 0
	if !wasPending {
		state.SchemaAttempts = 0
	}
	if state.TurnID == turnID {
		state.Status = "in_progress"
		state.DeadlineAt = 0
		state.TimedOutTurnID = ""
		state.TerminalOutcome = ""
		state.TerminalTurnID = ""
	}
	return writeLaneStateUnlocked(s.paths, state)
}

func (s *nativeSupervisor) recordLaneTurnTerminal(threadID, turnID, status string) {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	lock, lockErr := lockLaneStateFile(s.paths, threadID)
	if lockErr != nil {
		return
	}
	defer unlockLaneStateFile(lock)
	state, err := readLaneStateFile(s.paths, threadID)
	if err != nil || turnID == "" || state.TurnID != turnID {
		return
	}
	state.Status = defaultString(status, "failed")
	state.DeadlineAt = 0
	if state.TimedOutTurnID == turnID && state.TerminalOutcome == "timed_out" {
		// App Server represents a client-enforced deadline as an interrupted
		// turn. Preserve the more precise durable outcome.
		state.TerminalTurnID = turnID
	} else {
		state.TerminalOutcome = state.Status
		state.TerminalTurnID = turnID
		state.TimedOutTurnID = ""
	}
	_ = writeLaneStateUnlocked(s.paths, state)
}

func (s *nativeSupervisor) timeoutLaneTurn(threadID, turnID string) error {
	if !validSessionID(threadID) || turnID == "" {
		return errors.New("timeout_turn requires a lane thread and turn")
	}
	s.schemaMu.Lock()
	stateLock, lockErr := lockLaneStateFile(s.paths, threadID)
	if lockErr != nil {
		s.schemaMu.Unlock()
		return lockErr
	}
	state, err := readLaneStateFile(s.paths, threadID)
	if err == nil && state.TurnID != turnID {
		err = fmt.Errorf("lane %s current turn is %s, not %s", threadID, state.TurnID, turnID)
	}
	terminal := err == nil && statusTerminal(state.Status)
	if err == nil && !terminal {
		state.TimedOutTurnID = turnID
		state.TerminalOutcome = "timed_out"
		state.TerminalTurnID = turnID
		err = writeLaneStateUnlocked(s.paths, state)
	}
	unlockLaneStateFile(stateLock)
	s.schemaMu.Unlock()
	if err != nil {
		return err
	}
	if terminal {
		return nil
	}
	client, err := s.ensureClient()
	if err != nil {
		return err
	}
	return requestWithTimeout(client, 10*time.Second, "turn/interrupt", map[string]any{
		"threadId": threadID, "turnId": turnID,
	}, nil)
}

type laneSchemaAction uint8

const (
	laneSchemaNone laneSchemaAction = iota
	laneSchemaRetried
)

func (s *nativeSupervisor) processLaneSchemaTerminal(threadID, turnID string) (laneSchemaAction, int, error) {
	if !validSessionID(threadID) || turnID == "" {
		return laneSchemaNone, 0, errors.New("schema terminal check requires a lane thread and turn")
	}
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return laneSchemaNone, 0, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	state, err := readLaneStateFile(s.paths, threadID)
	if err != nil {
		return laneSchemaNone, 0, nil
	}
	if state.Status == "archived" || isRetiredThreadNative(s.paths, threadID) {
		return laneSchemaNone, 0, nil
	}
	if len(state.OutputSchema) == 0 {
		s.recordLaneAutoArchiveTerminal(threadID, turnID)
		return laneSchemaNone, 0, nil
	}
	normalizeLanePendingTurns(&state)
	if action, ready := alignLaneSchemaTurn(&state, turnID); !ready {
		return action, state.SchemaAttempts, nil
	}
	client, err := s.ensureClient()
	if err != nil {
		return laneSchemaNone, 0, err
	}
	validationErr := validateLaneTurnOutput(client, s.paths, state, turnID)
	if validationErr == nil {
		s.recordLaneAutoArchiveTerminal(threadID, turnID)
		return laneSchemaNone, 0, nil
	}
	attempts := state.SchemaRetryByID[turnID]
	if attempts >= 2 {
		return laneSchemaNone, attempts, fmt.Errorf("lane %s did not satisfy its output schema after %d retries: %w", state.Name, attempts, validationErr)
	}
	if err := s.startLaneSchemaCorrection(client, state, turnID, attempts+1, validationErr); err != nil {
		return laneSchemaNone, attempts, err
	}
	return laneSchemaRetried, attempts + 1, nil
}

func (s *nativeSupervisor) startLaneSchemaCorrection(
	client *appServerClient,
	state laneState,
	draftTurnID string,
	attempt int,
	validationErr error,
) error {
	message := validationErr.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	var schema any
	if err := json.Unmarshal(state.OutputSchema, &schema); err != nil {
		return err
	}
	var started struct {
		Turn appTurn `json:"turn"`
	}
	if err := requestWithTimeout(client, 60*time.Second, "turn/start", map[string]any{
		"threadId":     state.ThreadID,
		"input":        []map[string]any{{"type": "text", "text": "Your prior final answer failed the caller's JSON Schema: " + message + "\nReturn only a corrected JSON value satisfying the schema."}},
		"outputSchema": schema,
	}, &started); err != nil {
		return err
	}
	if err := s.recordLaneSchemaCorrection(state.ThreadID, draftTurnID, started.Turn, attempt); err != nil {
		return err
	}
	s.mu.Lock()
	s.activeTurns[state.ThreadID] = started.Turn.ID
	s.mu.Unlock()
	return nil
}

// recordLaneSchemaCorrection re-reads state after the potentially long
// turn/start RPC and merges only schema-owned fields. A collector runs in a
// different process and may acknowledge an older queue entry while that RPC
// is in flight; writing the pre-RPC snapshot would resurrect acknowledged
// work and regress CollectedTurnID.
func (s *nativeSupervisor) recordLaneSchemaCorrection(
	threadID string,
	draftTurnID string,
	correction appTurn,
	attempt int,
) error {
	lock, err := lockLaneStateFile(s.paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	state, err := readLaneStateFile(s.paths, threadID)
	if err != nil {
		return err
	}
	normalizeLanePendingTurns(&state)
	if state.SchemaRetryByID == nil {
		state.SchemaRetryByID = map[string]int{}
	}
	delete(state.SchemaRetryByID, draftTurnID)
	state.SchemaRetryByID[correction.ID] = attempt
	state.SchemaAttempts = attempt
	wasHead := state.TurnID == draftTurnID
	if !replaceLanePendingTurn(&state, draftTurnID, correction.ID) {
		appendLanePendingTurn(&state, correction.ID)
	}
	state.LatestTurnID = correction.ID
	state.AutoArchiveAt = 0
	if wasHead {
		state.Status = normalizeStatus(correction.Status)
		state.TimedOutTurnID = ""
		state.TerminalOutcome = ""
		state.TerminalTurnID = ""
	}
	return writeLaneStateUnlocked(s.paths, state)
}

func alignLaneSchemaTurn(state *laneState, turnID string) (laneSchemaAction, bool) {
	if laneTurnPending(*state, turnID) {
		return laneSchemaNone, true
	}
	// The pending queue no longer contains this draft, so an earlier schema
	// pass already replaced it with its correction turn.
	return laneSchemaRetried, false
}

func (s *nativeSupervisor) status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientMu.Lock()
	connected := s.client != nil
	appServerPID := 0
	appServerProcStart := ""
	if connected {
		select {
		case <-s.client.done:
			connected = false
		default:
			appServerPID = s.client.peerPID
			appServerProcStart = s.client.peerProcStart
		}
	}
	s.clientMu.Unlock()
	return map[string]any{
		"pid": os.Getpid(), "pluginVersion": s.pluginVersion, "runtimeIdentity": s.runtimeIdentity, "implementation": "go",
		"controlSocket": s.paths.supervisorSock, "appServerSocket": s.paths.appServerSock,
		"appServerConnected": connected, "appServerPid": appServerPID, "appServerProcStart": appServerProcStart,
		"shimCount": len(s.shims), "ready": s.ready,
	}
}

//nolint:gocyclo // Publication reconciles durable state, live transport, and a new shim explicitly.
func (s *nativeSupervisor) ensureShim(input map[string]any) (map[string]any, error) {
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	sessionID, err := s.shimSessionID(input)
	if err != nil {
		return nil, err
	}
	if s.isRetired(sessionID) {
		return nil, fmt.Errorf("codex thread %s is retired", sessionID)
	}
	if !s.authorizedPeerThread(sessionID) {
		return nil, fmt.Errorf("refuse to publish unauthorized Codex thread %s", sessionID)
	}
	cwd := defaultString(stringValue(input["cwd"]), mustGetwd())
	configuredName := strings.TrimSpace(stringValue(input["name"]))
	name := configuredName
	if name == "" {
		name = defaultPeerName(cwd, sessionID)
	}
	name = sanitizeName(name)
	nameSource := defaultString(stringValue(input["nameSource"]), map[bool]string{true: "explicit", false: "generated"}[configuredName != ""])
	status := defaultString(stringValue(input["status"]), "idle")
	statePath := filepath.Join(s.paths.dataRoot, "sessions", sessionKey(sessionID), "state.json")
	previousState := readJSONMap(statePath)
	agentRuntimeDir := defaultString(stringValue(input["agentRuntimeDir"]), stringValue(previousState["agentRuntimeDir"]))
	// Reconciliation has no fresh permission attestation. Preserve the mode
	// last supplied by a SessionStart or lane registration, and fail toward the
	// ordinary prompting mode when no authoritative state exists.
	permission := defaultString(stringValue(input["permissionMode"]), defaultString(stringValue(previousState["permissionMode"]), "default"))
	if state := previousState; state != nil {
		if socket := stringValue(state["socketPath"]); socket != "" && probeUnixSocket(socket, 250*time.Millisecond) {
			update := map[string]any{
				"type": "control", "action": "update", "cwd": cwd, "permissionMode": permission,
				"status": status, "supervisorSocket": s.paths.supervisorSock,
			}
			if configuredName != "" {
				update["name"] = name
				update["nameSource"] = nameSource
			}
			if err := sendUnixJSON(socket, update, time.Second); err == nil {
				state = readJSONMap(statePath)
				if !s.authorizedPeerThread(sessionID) {
					_ = sendUnixJSON(socket, map[string]any{"type": "control", "action": "shutdown"}, time.Second)
					return nil, fmt.Errorf("codex thread %s lost authorization while its shim was updated", sessionID)
				}
				s.mu.Lock()
				s.shims[sessionID] = state
				s.mu.Unlock()
				return state, nil
			}
		}
	}
	args := []string{
		"shim",
		"--session-id", sessionID, "--cwd", cwd, "--permission-mode", permission,
		"--name", name, "--name-source", nameSource, "--data-dir", s.paths.dataRoot,
		"--claude-config-dir", s.paths.claudeRoot, "--codex-home", s.paths.codexHome,
		"--supervisor-socket", s.paths.supervisorSock, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", s.procStart, "--runtime-dir", s.paths.runtimeDir,
	}
	if agentRuntimeDir != "" {
		args = append(args, "--agent-runtime-dir", agentRuntimeDir)
	}
	command := exec.Command(s.shimExecutable, args...) //nolint:gosec // executable is os.Executable and args are built from validated thread state.
	command.Env = overrideNativeEnv(os.Environ(), "GOMAXPROCS", "1")
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	pid := command.Process.Pid
	// The supervisor outlives its shims and must reap them. Process.Release
	// leaves a killed shim as the supervisor's zombie child on Unix, and both
	// Claude discovery and kill(2) can otherwise mistake that zombie for live.
	go func() { _ = command.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := readJSONMap(statePath)
		if intValue(state["pid"]) == pid {
			if socket := stringValue(state["socketPath"]); socket != "" && probeUnixSocket(socket, 250*time.Millisecond) {
				if s.isRetired(sessionID) {
					_ = sendUnixJSON(socket, map[string]any{"type": "control", "action": "shutdown"}, time.Second)
					return nil, fmt.Errorf("codex thread %s was retired while its shim started", sessionID)
				}
				if !s.authorizedPeerThread(sessionID) {
					_ = sendUnixJSON(socket, map[string]any{"type": "control", "action": "shutdown"}, time.Second)
					return nil, fmt.Errorf("codex thread %s lost authorization while its shim started", sessionID)
				}
				s.mu.Lock()
				s.shims[sessionID] = state
				s.mu.Unlock()
				return state, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out starting Claude peer shim for %s", sessionID)
}

func (s *nativeSupervisor) shimSessionID(input map[string]any) (string, error) {
	sessionID := stringValue(input["sessionId"])
	if !validSessionID(sessionID) {
		return "", errors.New("invalid Codex thread id")
	}
	allowInteractiveRelease, _ := input["allowInteractiveRelease"].(bool)
	if s.interactiveReleasePending(sessionID) && !allowInteractiveRelease {
		return "", errInteractiveSessionEnding
	}
	return sessionID, nil
}

const interactiveReleaseGrace = 30 * time.Second

var errInteractiveSessionEnding = errors.New("interactive Codex session is ending")

func (s *nativeSupervisor) cancelInteractiveRelease(threadID string) {
	if threadID == "" {
		return
	}
	s.mu.Lock()
	delete(s.releasing, threadID)
	s.mu.Unlock()
}

func (s *nativeSupervisor) interactiveReleasePending(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releasing[threadID] > time.Now().UnixMilli()
}

func (s *nativeSupervisor) releaseInteractiveThread(threadID string) error {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return s.releaseInteractiveThreadLocked(threadID, nil)
}

func (s *nativeSupervisor) releaseInteractiveThreadLocked(threadID string, expected *interactiveOwnerRecord) error {
	if !validSessionID(threadID) {
		return errors.New("release requires a valid Codex thread id")
	}
	if expected != nil {
		current := readInteractiveOwner(s.paths, threadID)
		if !sameInteractiveOwner(current, expected) ||
			cleanupProcessIdentityStatus(current.OwnerPID, current.OwnerProcStart).Status != processIdentityStale {
			return nil
		}
	}
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	if lane, err := readLaneStateFile(s.paths, threadID); err == nil && lane.Type == "codex-peer-lane" {
		removeInteractiveOwnerIfMatching(s.paths, threadID, expected)
		return nil
	}
	s.mu.Lock()
	if s.releasing == nil {
		s.releasing = map[string]int64{}
	}
	s.releasing[threadID] = time.Now().Add(interactiveReleaseGrace).UnixMilli()
	s.mu.Unlock()
	s.removeShim(threadID)
	client, err := s.ensureClient()
	if err == nil {
		err = parkInteractiveThread(client, threadID)
	}
	if err != nil {
		// Keep the release guard and exact stale owner for attached-session
		// reconciliation. Zero-turn prepared owners never enter this path.
		return err
	}
	removeInteractiveOwnerIfMatching(s.paths, threadID, expected)
	// App Server has no public unload operation. Archiving unloads the runtime
	// and its MCP children; immediately unarchiving leaves the durable thread
	// resumable but not loaded. The release guard prevents the transient archive
	// notifications from retiring or republishing the peer shim.
	return nil
}

func interactiveOwnerPath(paths nativePaths, threadID string) string {
	return filepath.Join(profileDataRoot(paths), "interactive-owners", sessionKey(threadID)+".json")
}

func writeInteractiveOwnerRecord(paths nativePaths, record interactiveOwnerRecord) error {
	if !validSessionID(record.ThreadID) || record.RequestID == "" ||
		!exactProcessIdentityMatch(record.OwnerPID, record.OwnerProcStart) {
		return errors.New("interactive launch owner is not live")
	}
	return writeJSONAtomic(interactiveOwnerPath(paths, record.ThreadID), record)
}

func markPreparedLaunchAttached(paths nativePaths, threadID string) (*interactiveOwnerRecord, error) {
	lifecycleLock, err := lockLaneLifecycle(paths, threadID)
	if err != nil {
		return nil, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	record := readInteractiveOwner(paths, threadID)
	if record == nil || !record.Pending {
		return record, nil
	}
	if record.Aborting {
		return record, errors.New("prepared launch is already aborting")
	}
	if !exactProcessIdentityMatch(record.OwnerPID, record.OwnerProcStart) {
		return nil, errors.New("cannot corroborate the prepared launch owner")
	}
	record.Pending = false
	record.UpdatedAt = time.Now().UnixMilli()
	if err := writeInteractiveOwnerRecord(paths, *record); err != nil {
		return nil, err
	}
	return record, nil
}

func readInteractiveOwner(paths nativePaths, threadID string) *interactiveOwnerRecord {
	body, err := os.ReadFile(interactiveOwnerPath(paths, threadID))
	if err != nil {
		return nil
	}
	var record interactiveOwnerRecord
	if json.Unmarshal(body, &record) != nil || record.ThreadID != threadID || record.OwnerPID <= 1 {
		return nil
	}
	return &record
}

func readInteractiveOwners(paths nativePaths) []interactiveOwnerRecord {
	directory := filepath.Join(profileDataRoot(paths), "interactive-owners")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	records := make([]interactiveOwnerRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // entries are constrained to the bridge-owned directory.
		if readErr != nil {
			continue
		}
		var record interactiveOwnerRecord
		if json.Unmarshal(body, &record) != nil || !validSessionID(record.ThreadID) || record.OwnerPID <= 1 ||
			entry.Name() != sessionKey(record.ThreadID)+".json" {
			continue
		}
		records = append(records, record)
	}
	return records
}

func sameInteractiveOwner(left, right *interactiveOwnerRecord) bool {
	return left != nil && right != nil && left.ThreadID == right.ThreadID && left.RequestID == right.RequestID &&
		left.OwnerPID == right.OwnerPID && left.OwnerProcStart == right.OwnerProcStart
}

func removeInteractiveOwnerIfMatching(paths nativePaths, threadID string, expected *interactiveOwnerRecord) {
	removeJSONIf(interactiveOwnerPath(paths, threadID), func(current map[string]any) bool {
		if stringValue(current["threadId"]) != threadID {
			return false
		}
		return expected == nil || (stringValue(current["requestId"]) == expected.RequestID &&
			intValue(current["ownerPid"]) == expected.OwnerPID &&
			stringValue(current["ownerProcStart"]) == expected.OwnerProcStart)
	})
}

func (s *nativeSupervisor) clearInteractiveOwner(threadID string) {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	removeInteractiveOwnerIfMatching(s.paths, threadID, nil)
}

func (s *nativeSupervisor) reconcileExitedInteractiveOwners() {
	records := readInteractiveOwners(s.paths)
	for index := range records {
		candidate := &records[index]
		lifecycleLock, err := lockLaneLifecycle(s.paths, candidate.ThreadID)
		if err != nil {
			continue
		}
		record := readInteractiveOwner(s.paths, candidate.ThreadID)
		if record == nil || !sameInteractiveOwner(record, candidate) {
			unlockLaneLifecycle(lifecycleLock)
			continue
		}
		observation := cleanupProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart)
		if observation.Status == processIdentityMatches || observation.Status == processIdentityUnknown {
			unlockLaneLifecycle(lifecycleLock)
			continue
		}
		if record.Pending && record.ParkOnAbort {
			// A zero-turn TUI has exited after publication. Remove its advertised
			// transport, but retain the exact stale record as the authorization
			// proof for an immediate resume of the still-loaded Codex thread.
			s.removeShim(record.ThreadID)
			unlockLaneLifecycle(lifecycleLock)
			continue
		}
		if record.Pending {
			if record.DeleteOnAbort {
				s.removeShim(record.ThreadID)
				var deleteErr error
				client, clientErr := s.ensureClient()
				deleteErr = clientErr
				if deleteErr == nil {
					deleteErr = deletePreparedThread(client, record.ThreadID)
				}
				if deleteErr == nil {
					removeInteractiveOwnerIfMatching(s.paths, record.ThreadID, record)
				}
				unlockLaneLifecycle(lifecycleLock)
				if deleteErr != nil {
					fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: delete uncommitted prepared thread %s: %v\n", record.ThreadID, deleteErr)
				}
				continue
			}
			removeInteractiveOwnerIfMatching(s.paths, record.ThreadID, record)
			unlockLaneLifecycle(lifecycleLock)
			continue
		}
		s.ownerMu.Lock()
		releaseErr := s.releaseInteractiveThreadLocked(record.ThreadID, record)
		s.ownerMu.Unlock()
		unlockLaneLifecycle(lifecycleLock)
		if releaseErr != nil {
			fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: release exited interactive owner %s: %v\n", record.ThreadID, releaseErr)
		}
	}
}

func parkInteractiveThread(client *appServerClient, threadID string) error {
	if err := requestWithTimeout(client, 4*time.Second, "thread/archive", map[string]any{"threadId": threadID}, nil); err != nil {
		return fmt.Errorf("unload interactive Codex thread: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lastErr = requestWithTimeout(client, 4*time.Second, "thread/unarchive", map[string]any{"threadId": threadID}, nil)
		if lastErr == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("restore interactive Codex thread after unload: %w", lastErr)
}

func (s *nativeSupervisor) updateShimStatus(threadID, status string) {
	s.mu.Lock()
	state := s.shims[threadID]
	s.mu.Unlock()
	if socket := stringValue(state["socketPath"]); socket != "" {
		_ = sendUnixJSON(socket, map[string]any{"type": "control", "action": "status", "status": status}, time.Second)
	}
}

func (s *nativeSupervisor) updateShimName(threadID, name string) {
	s.mu.Lock()
	state := s.shims[threadID]
	s.mu.Unlock()
	if socket := stringValue(state["socketPath"]); socket != "" && name != "" {
		_ = sendUnixJSON(socket, map[string]any{
			"type": "control", "action": "update", "name": sanitizeName(name), "nameSource": "codex",
		}, time.Second)
	}
}

func (s *nativeSupervisor) removeShim(threadID string) {
	s.mu.Lock()
	state := s.shims[threadID]
	delete(s.shims, threadID)
	delete(s.activeTurns, threadID)
	delete(s.subscribed, threadID)
	s.mu.Unlock()
	if state == nil {
		state = readJSONMap(filepath.Join(s.paths.dataRoot, "sessions", sessionKey(threadID), "state.json"))
	}
	if socket := stringValue(state["socketPath"]); socket != "" {
		_ = sendUnixJSON(socket, map[string]any{"type": "control", "action": "shutdown"}, time.Second)
	}
}

func (s *nativeSupervisor) subscribeThread(threadID string) (*appThread, error) {
	if !validSessionID(threadID) {
		return nil, errors.New("invalid Codex thread id")
	}
	if !s.authorizedPeerThread(threadID) {
		return nil, fmt.Errorf("refuse to subscribe unauthorized Codex thread %s", threadID)
	}
	s.subscribeMu.Lock()
	defer s.subscribeMu.Unlock()
	s.mu.Lock()
	already := s.subscribed[threadID]
	s.mu.Unlock()
	if already {
		return nil, nil
	}
	client, err := s.ensureClient()
	if err != nil {
		return nil, err
	}
	resumed, err := resumeThreadForPeer(client, threadID)
	if err != nil {
		return nil, err
	}
	if statusType(resumed.Status) == "active" {
		var page struct {
			Data []appTurn `json:"data"`
		}
		if requestWithTimeout(client, 30*time.Second, "thread/turns/list", map[string]any{
			"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
		}, &page) == nil && len(page.Data) > 0 && statusType(page.Data[0].Status) == "active" {
			s.mu.Lock()
			s.activeTurns[threadID] = page.Data[0].ID
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	s.subscribed[threadID] = true
	s.mu.Unlock()
	return resumed, nil
}

// resumeThreadForPeer subscribes without hydrating transcript history. Codex's
// projection is a derived cache, but public read/resume calls do not repair a
// cursor gap left by an App Server interrupted mid-turn; installation prevents
// that state by refusing to restart while a turn is active.
func resumeThreadForPeer(client *appServerClient, threadID string) (*appThread, error) {
	var resumed struct {
		Thread appThread `json:"thread"`
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/resume", map[string]any{
		"threadId": threadID, "excludeTurns": true,
	}, &resumed); err != nil {
		return nil, err
	}
	return &resumed.Thread, nil
}

func resumePreparedThread(client *appServerClient, threadID string, request map[string]any) (*appThread, string, error) {
	params := map[string]any{"threadId": threadID, "excludeTurns": true}
	if cwd := strings.TrimSpace(stringValue(request["cwd"])); cwd != "" {
		params["cwd"] = cwd
	}
	var resumed struct {
		Thread         appThread `json:"thread"`
		Cwd            string    `json:"cwd"`
		ApprovalPolicy any       `json:"approvalPolicy"`
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/resume", params, &resumed); err != nil {
		return nil, "", err
	}
	approvalPolicy := stringValue(resumed.ApprovalPolicy)
	if approvalPolicy == "" {
		return nil, "", errors.New("thread/resume did not report its effective approval policy")
	}
	effectiveCwd := strings.TrimSpace(resumed.Cwd)
	if effectiveCwd == "" {
		return nil, "", errors.New("thread/resume did not report its effective cwd")
	}
	if expected := strings.TrimSpace(stringValue(request["cwd"])); expected != "" && effectiveCwd != expected {
		return nil, "", fmt.Errorf("thread/resume applied cwd %q, expected %q", effectiveCwd, expected)
	}
	resumed.Thread.Cwd = effectiveCwd
	requestedApproval := strings.TrimSpace(stringValue(request["approvalPolicy"]))
	requestedSandbox := strings.TrimSpace(stringValue(request["sandbox"]))
	if requestedApproval != "" || requestedSandbox != "" {
		settings := map[string]any{"threadId": threadID}
		putIf(settings, "approvalPolicy", requestedApproval)
		if requestedSandbox != "" {
			sandboxPolicy, err := sandboxPolicyForMode(requestedSandbox)
			if err != nil {
				return nil, "", err
			}
			settings["sandboxPolicy"] = sandboxPolicy
		}
		if err := requestWithTimeout(client, 30*time.Second, "thread/settings/update", settings, nil); err != nil {
			return nil, "", fmt.Errorf("apply resumed Codex thread settings: %w", err)
		}
		if requestedApproval != "" {
			approvalPolicy = requestedApproval
		}
	}
	return &resumed.Thread, approvalPolicy, nil
}

func sandboxPolicyForMode(mode string) (map[string]any, error) {
	switch mode {
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}, nil
	case "read-only":
		return map[string]any{"type": "readOnly"}, nil
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite"}, nil
	default:
		return nil, fmt.Errorf("unsupported resumed Codex sandbox %q", mode)
	}
}

func permissionModeForApprovalPolicy(approvalPolicy string) string {
	if approvalPolicy == "never" {
		return "bypassPermissions"
	}
	return "default"
}

//nolint:gocyclo // Wake handles active steer, idle start, and uncertain transport outcomes explicitly.
func (s *nativeSupervisor) wakeThread(threadID string, item map[string]any) (string, error) {
	if threadID == "" || stringValue(item["message"]) == "" {
		return "", errors.New("wake request requires a thread and message")
	}
	if !s.authorizedPeerThread(threadID) {
		return "", fmt.Errorf("refuse to wake unauthorized Codex thread %s", threadID)
	}
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return "", err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	if !s.authorizedPeerThread(threadID) {
		return "", fmt.Errorf("refuse to wake unauthorized Codex thread %s", threadID)
	}
	if state, stateErr := readLaneStateFile(s.paths, threadID); stateErr == nil && state.Status == "archived" {
		return "", fmt.Errorf("codex lane %s is archived", threadID)
	}
	if isRetiredThreadNative(s.paths, threadID) {
		return "", fmt.Errorf("codex thread %s is retired", threadID)
	}
	client, err := s.ensureClient()
	if err != nil {
		return "", err
	}
	read, err := readExactPreparedThread(client, threadID)
	if err != nil {
		return "", err
	}
	thread := &read
	if statusType(thread.Status) == "notLoaded" {
		thread, err = s.subscribeThread(threadID)
		if err != nil {
			return "", err
		}
	}
	input := []map[string]any{{"type": "text", "text": peerMessageText(item)}}
	if thread != nil && statusType(thread.Status) == "active" {
		s.mu.Lock()
		turnID := s.activeTurns[threadID]
		s.mu.Unlock()
		if turnID == "" {
			return "", fmt.Errorf("active Codex thread %s did not expose its active turn", threadID)
		}
		err = requestWithTimeout(client, 30*time.Second, "turn/steer", map[string]any{
			"threadId": threadID, "input": input, "expectedTurnId": turnID,
		}, nil)
		if err != nil {
			return "steered", classifyWakeMutationError(err)
		}
		return "steered", err
	}
	var started struct {
		Turn appTurn `json:"turn"`
	}
	// Turn policy overrides are sticky in App Server.  A peer wake must inherit
	// the thread's configured approval/sandbox policy instead of permanently
	// converting a normal or read-only interactive thread to yolo mode.
	err = requestWithTimeout(client, 60*time.Second, "turn/start", map[string]any{
		"threadId": threadID, "input": input,
	}, &started)
	if err != nil {
		return "started", classifyWakeMutationError(err)
	}
	if err == nil && started.Turn.ID != "" {
		s.mu.Lock()
		s.activeTurns[threadID] = started.Turn.ID
		s.mu.Unlock()
		if recordErr := s.recordLaneTurnStarted(threadID, started.Turn.ID); recordErr != nil {
			return "started", &wakeDeliveryUncertainError{err: recordErr}
		}
	}
	return "started", err
}

func classifyWakeMutationError(err error) error {
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) {
		// An App Server JSON-RPC error is an authoritative rejection. Transport
		// errors and timeouts are not: the request may already have taken effect.
		return err
	}
	return &wakeDeliveryUncertainError{err: err}
}

func wakeRecordPath(paths nativePaths, threadID, messageID string) string {
	return filepath.Join(profileDataRoot(paths), "wakes", sessionKey(threadID), sessionKey(messageID)+".json")
}

func readWakeRecord(paths nativePaths, threadID, messageID string) *wakeRecord {
	if threadID == "" || messageID == "" {
		return nil
	}
	body, err := os.ReadFile(wakeRecordPath(paths, threadID, messageID))
	if err != nil {
		return nil
	}
	var record wakeRecord
	if json.Unmarshal(body, &record) != nil || record.SessionID != threadID || record.MessageID != messageID {
		return nil
	}
	return &record
}

func writeWakeRecord(paths nativePaths, record wakeRecord) error {
	record.UpdatedAt = time.Now().UnixMilli()
	return writeJSONAtomic(wakeRecordPath(paths, record.SessionID, record.MessageID), record)
}

func (s *nativeSupervisor) queueWake(threadID string, item map[string]any) (string, error) {
	messageID := stringValue(item["id"])
	if !validSessionID(threadID) || messageID == "" || stringValue(item["message"]) == "" {
		return "", errors.New("wake request requires a valid thread, message id, and message")
	}
	if !s.authorizedPeerThread(threadID) {
		return "", fmt.Errorf("refuse to queue wake for unauthorized Codex thread %s", threadID)
	}
	fingerprint := wakeItemFingerprint(item)
	s.wakeMu.Lock()
	existing := readWakeRecord(s.paths, threadID, messageID)
	if existing != nil {
		if existing.Fingerprint != fingerprint {
			s.wakeMu.Unlock()
			// A transport id names one immutable message. Treat conflicting reuse
			// as already owned instead of opening the inbox fallback as a second,
			// attacker-controlled delivery path.
			return "conflict", nil
		}
		delivery := defaultString(existing.Delivery, existing.State)
		s.wakeMu.Unlock()
		// The first owner already cancelled (and recorded) the prior grace
		// deadline. A retry must not clear a deadline that its fallback restored.
		return delivery, nil
	}
	now := time.Now().UnixMilli()
	record := wakeRecord{
		SessionID: threadID, MessageID: messageID, Fingerprint: fingerprint,
		DeliveryFingerprint: sessionKey(peerMessageText(item)),
		State:               "in_flight", Delivery: "accepted", Item: item,
		OwnerPID: os.Getpid(), OwnerProcStart: s.procStart, CreatedAt: now,
	}
	if err := s.reserveLaneWake(&record); err != nil {
		s.wakeMu.Unlock()
		return "", err
	}
	s.wakeMu.Unlock()
	go s.finishWake(record)
	return "accepted", nil
}

// reserveLaneWake makes the delivery intent durable before it clears a lane's
// grace deadline. A crash can therefore leave either the old deadline or a
// recoverable wake record, but never a zero deadline with no owner.
// The caller holds wakeMu while this function writes the journal.
func (s *nativeSupervisor) reserveLaneWake(record *wakeRecord) error {
	lifecycleLock, err := lockLaneLifecycle(s.paths, record.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	if !s.authorizedPeerThread(record.SessionID) {
		return fmt.Errorf("refuse to queue wake for unauthorized Codex thread %s", record.SessionID)
	}
	stateLock, err := lockLaneStateFile(s.paths, record.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(stateLock)
	state, stateErr := readLaneStateFile(s.paths, record.SessionID)
	if stateErr != nil && !os.IsNotExist(stateErr) {
		return stateErr
	}
	if stateErr == nil {
		if state.Status == "archived" || isRetiredThreadNative(s.paths, record.SessionID) {
			return fmt.Errorf("codex lane %s is archived", record.SessionID)
		}
		record.PriorLatestTurnID = state.LatestTurnID
		record.PriorAutoArchiveAt = state.AutoArchiveAt
	}
	if err := writeWakeRecord(s.paths, *record); err != nil {
		return err
	}
	if stateErr != nil || state.AutoArchiveAt == 0 {
		return nil
	}
	state.AutoArchiveAt = 0
	if err := writeLaneStateUnlocked(s.paths, state); err == nil {
		return nil
	} else {
		// A normal error response lets the shim use its inbox fallback, so first
		// relinquish the journal. If removal itself fails, durable ownership is
		// safer than opening a second delivery path; recovery will finish it.
		removeJSONIf(wakeRecordPath(s.paths, record.SessionID, record.MessageID), func(current map[string]any) bool {
			return stringValue(current["messageId"]) == record.MessageID
		})
		if readWakeRecord(s.paths, record.SessionID, record.MessageID) == nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: preserve wake %s after lane timer update failed: %v\n", record.MessageID, err)
		return nil
	}
}

// prepareLaneWake cancels a terminal grace timer before the asynchronous wake
// can race lifecycle cleanup. Interactive threads have no lane record and are
// unaffected.
func (s *nativeSupervisor) prepareLaneWake(threadID string) (string, int64, error) {
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return "", 0, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	stateLock, err := lockLaneStateFile(s.paths, threadID)
	if err != nil {
		return "", 0, err
	}
	defer unlockLaneStateFile(stateLock)
	state, err := readLaneStateFile(s.paths, threadID)
	if os.IsNotExist(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if state.Status == "archived" || isRetiredThreadNative(s.paths, threadID) {
		return "", 0, fmt.Errorf("codex lane %s is archived", threadID)
	}
	latestTurnID, deadline := state.LatestTurnID, state.AutoArchiveAt
	if state.AutoArchiveAt == 0 {
		return latestTurnID, deadline, nil
	}
	state.AutoArchiveAt = 0
	return latestTurnID, deadline, writeLaneStateUnlocked(s.paths, state)
}

func (s *nativeSupervisor) restoreLaneAutoArchive(record wakeRecord, renew bool) {
	if record.PriorLatestTurnID == "" {
		return
	}
	lifecycleLock, err := lockLaneLifecycle(s.paths, record.SessionID)
	if err != nil {
		return
	}
	defer unlockLaneLifecycle(lifecycleLock)
	stateLock, err := lockLaneStateFile(s.paths, record.SessionID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(stateLock)
	state, err := readLaneStateFile(s.paths, record.SessionID)
	if err != nil || state.Status == "archived" || !state.AutoArchive || state.AutoArchiveAt != 0 || state.LatestTurnID != record.PriorLatestTurnID {
		return
	}
	deadline := record.PriorAutoArchiveAt
	if renew || deadline == 0 {
		deadline = time.Now().Add(laneAutoArchiveDelay(state)).UnixMilli()
	}
	state.AutoArchiveAt = deadline
	_ = writeLaneStateUnlocked(s.paths, state)
}

func wakeItemFingerprint(item map[string]any) string {
	stable := make(map[string]any, len(item))
	for key, value := range item {
		if key != "receivedAt" {
			stable[key] = value
		}
	}
	body, _ := json.Marshal(stable)
	return sessionKey(string(body))
}

func (s *nativeSupervisor) finishWake(record wakeRecord) {
	delivery, err := s.wakeThread(record.SessionID, record.Item)
	if err != nil {
		var uncertain *wakeDeliveryUncertainError
		var turnID string
		var found bool
		if errors.As(err, &uncertain) {
			turnID, found = s.awaitWakeObservation(record, 65*time.Second)
		} else {
			turnID, found = s.findWakeMessage(record.SessionID, record)
		}
		if found {
			if adoptErr := s.adoptObservedWakeTurn(record.SessionID, turnID); adoptErr != nil {
				record.OwnerPID, record.OwnerProcStart = 0, ""
				s.wakeMu.Lock()
				_ = writeWakeRecord(s.paths, record)
				s.wakeMu.Unlock()
				return
			}
			record.State, record.Delivery, record.TurnID = "delivered", "observed", turnID
		} else {
			enqueueErr := s.queueWakeFallback(&record)
			if enqueueErr != nil {
				fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: preserve failed wake %s: %v (wake error: %v)\n", record.MessageID, enqueueErr, err)
				record.OwnerPID, record.OwnerProcStart = 0, ""
				s.wakeMu.Lock()
				_ = writeWakeRecord(s.paths, record)
				s.wakeMu.Unlock()
			}
			s.restoreLaneAutoArchive(record, true)
			return
		}
	} else {
		record.State, record.Delivery = "delivered", delivery
		s.mu.Lock()
		record.TurnID = s.activeTurns[record.SessionID]
		s.mu.Unlock()
	}
	s.wakeMu.Lock()
	if writeErr := writeWakeRecord(s.paths, record); writeErr != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: persist wake outcome %s: %v\n", record.MessageID, writeErr)
	}
	s.wakeMu.Unlock()
}

func (s *nativeSupervisor) awaitWakeObservation(record wakeRecord, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if turnID, found := s.findWakeMessage(record.SessionID, record); found {
			return turnID, true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (s *nativeSupervisor) queueWakeFallback(record *wakeRecord) error {
	record.State, record.Delivery = "queueing", "queued"
	s.wakeMu.Lock()
	if err := writeWakeRecord(s.paths, *record); err != nil {
		s.wakeMu.Unlock()
		return err
	}
	s.wakeMu.Unlock()
	if err := enqueueNativeInboxItem(s.paths, record.SessionID, record.Item, record.CreatedAt); err != nil {
		record.State, record.Delivery = "in_flight", "accepted"
		s.wakeMu.Lock()
		_ = writeWakeRecord(s.paths, *record)
		s.wakeMu.Unlock()
		return err
	}
	record.State, record.Delivery = "queued", "queued"
	s.wakeMu.Lock()
	err := writeWakeRecord(s.paths, *record)
	s.wakeMu.Unlock()
	return err
}

func (s *nativeSupervisor) findWakeMessage(threadID string, record wakeRecord) (string, bool) {
	client, err := s.ensureClient()
	if err != nil {
		return "", false
	}
	turns, err := listLaneTurns(client, threadID, "full")
	if err != nil {
		return "", false
	}
	for _, turn := range turns {
		for _, raw := range turn.Items {
			var item any
			fingerprint := defaultString(record.DeliveryFingerprint, sessionKey(peerMessageText(record.Item)))
			if json.Unmarshal(raw, &item) == nil && wakeItemContainsFingerprint(item, fingerprint) {
				return turn.ID, true
			}
		}
	}
	return "", false
}

func wakeItemContainsFingerprint(value any, fingerprint string) bool {
	switch current := value.(type) {
	case string:
		return sessionKey(current) == fingerprint
	case []any:
		for _, entry := range current {
			if wakeItemContainsFingerprint(entry, fingerprint) {
				return true
			}
		}
	case map[string]any:
		for _, entry := range current {
			if wakeItemContainsFingerprint(entry, fingerprint) {
				return true
			}
		}
	}
	return false
}

func enqueueNativeInboxItem(paths nativePaths, threadID string, item map[string]any, createdAt int64) error {
	pending := filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "inbox", "pending")
	if err := os.MkdirAll(pending, 0700); err != nil {
		return err
	}
	messageID := stringValue(item["id"])
	if messageID == "" {
		return errors.New("cannot queue wake fallback without a message id")
	}
	suffix := "-" + safeID(messageID) + ".json"
	entries, _ := os.ReadDir(pending)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), suffix) {
			return nil
		}
	}
	return writeJSONAtomic(filepath.Join(pending, fmt.Sprintf("%016d%s", createdAt, suffix)), item)
}

//nolint:gocyclo // Recovery distinguishes durable wake journal states and ownership explicitly.
func (s *nativeSupervisor) recoverWakeRecords() {
	root := filepath.Join(profileDataRoot(s.paths), "wakes")
	threads, _ := os.ReadDir(root)
	for _, thread := range threads {
		if !thread.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(root, thread.Name()))
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(root, thread.Name(), entry.Name())) //nolint:gosec // entries come from the bridge-owned wake ledger.
			var record wakeRecord
			if err != nil || json.Unmarshal(body, &record) != nil {
				continue
			}
			// A durable wake journal is not itself a peer capability. Older
			// versions could leave records for ordinary threads, so recovery must
			// re-attest the target before creating inbox state or starting work.
			if !s.authorizedPeerThread(record.SessionID) {
				continue
			}
			if record.State == "queueing" {
				if err := s.recoverQueueingWake(record); err != nil {
					fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: recover queued wake %s: %v\n", record.MessageID, err)
				}
				continue
			}
			if record.State == "queued" {
				// The inbox write can become durable immediately before the worker
				// restores the lane grace timer. Reconciliation completes that second
				// half after a supervisor crash; the helper is idempotent once armed.
				s.restoreLaneAutoArchive(record, true)
				continue
			}
			if record.State != "in_flight" ||
				cleanupProcessIdentityStatus(record.OwnerPID, record.OwnerProcStart).Status != processIdentityStale {
				continue
			}
			s.wakeMu.Lock()
			current := readWakeRecord(s.paths, record.SessionID, record.MessageID)
			if current == nil || current.State != "in_flight" ||
				cleanupProcessIdentityStatus(current.OwnerPID, current.OwnerProcStart).Status != processIdentityStale {
				s.wakeMu.Unlock()
				continue
			}
			record = *current
			record.OwnerPID, record.OwnerProcStart = os.Getpid(), s.procStart
			if writeWakeRecord(s.paths, record) != nil {
				s.wakeMu.Unlock()
				continue
			}
			s.wakeMu.Unlock()
			priorLatestTurnID, priorAutoArchiveAt, err := s.prepareLaneWake(record.SessionID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: preserve recovered wake %s: %v\n", record.MessageID, err)
				record.OwnerPID, record.OwnerProcStart = 0, ""
				s.wakeMu.Lock()
				_ = writeWakeRecord(s.paths, record)
				s.wakeMu.Unlock()
				continue
			}
			if record.PriorLatestTurnID == "" {
				record.PriorLatestTurnID = priorLatestTurnID
				record.PriorAutoArchiveAt = priorAutoArchiveAt
				s.wakeMu.Lock()
				_ = writeWakeRecord(s.paths, record)
				s.wakeMu.Unlock()
			}
			go s.finishWakeRecovery(record)
		}
	}
}

func (s *nativeSupervisor) recoverQueueingWake(record wakeRecord) error {
	defer func() { s.restoreLaneAutoArchive(record, true) }()
	return func() error {
		s.wakeMu.Lock()
		defer s.wakeMu.Unlock()
		current := readWakeRecord(s.paths, record.SessionID, record.MessageID)
		if current == nil || current.State != "queueing" {
			return nil
		}
		record = *current
		lifecycleLock, err := lockLaneLifecycle(s.paths, record.SessionID)
		if err != nil {
			return err
		}
		defer unlockLaneLifecycle(lifecycleLock)
		if !s.authorizedPeerThread(record.SessionID) {
			return errors.New("refuse to recover a wake for an unauthorized Codex thread")
		}
		// The filename is deterministic by message id and creation time, so this is
		// idempotent when the inbox write completed before the supervisor died.
		if err := enqueueNativeInboxItem(s.paths, record.SessionID, record.Item, record.CreatedAt); err != nil {
			return err
		}
		record.State, record.Delivery = "queued", "queued"
		record.OwnerPID, record.OwnerProcStart = os.Getpid(), s.procStart
		err = writeWakeRecord(s.paths, record)
		return err
	}()
}

func (s *nativeSupervisor) finishWakeRecovery(record wakeRecord) {
	if turnID, found := s.awaitWakeObservation(record, 65*time.Second); found {
		if err := s.adoptObservedWakeTurn(record.SessionID, turnID); err != nil {
			record.OwnerPID, record.OwnerProcStart = 0, ""
			s.wakeMu.Lock()
			_ = writeWakeRecord(s.paths, record)
			s.wakeMu.Unlock()
			return
		}
		record.State, record.Delivery, record.TurnID = "delivered", "observed", turnID
	} else {
		if err := s.queueWakeFallback(&record); err != nil {
			record.OwnerPID, record.OwnerProcStart = 0, ""
			s.wakeMu.Lock()
			_ = writeWakeRecord(s.paths, record)
			s.wakeMu.Unlock()
		}
		s.restoreLaneAutoArchive(record, true)
		return
	}
	record.OwnerPID, record.OwnerProcStart = os.Getpid(), s.procStart
	s.wakeMu.Lock()
	_ = writeWakeRecord(s.paths, record)
	s.wakeMu.Unlock()
}

func (s *nativeSupervisor) adoptObservedWakeTurn(threadID, turnID string) error {
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	return s.recordLaneTurnStarted(threadID, turnID)
}

func (s *nativeSupervisor) reconcileLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.reconcileWake:
		case <-s.done:
			return
		}
		if err := s.reconcile(); err != nil {
			fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: reconciliation failed: %v\n", err)
		}
	}
}

func (s *nativeSupervisor) reconcile() error {
	var reconcileErrors []error
	cleanupStaleBridgeArtifacts(s.paths)
	reconcileClaudeLaneManagers(s.paths)
	if err := reconcileGrokLaneManagers(s.paths); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	client, err := s.ensureClient()
	if err != nil {
		return errors.Join(append(reconcileErrors, err)...)
	}
	s.reconcileExitedInteractiveOwners()
	loadedSet, err := loadedPreparedThreads(client)
	if err != nil {
		return errors.Join(append(reconcileErrors, err)...)
	}
	loaded := make([]string, 0, len(loadedSet))
	for threadID := range loadedSet {
		loaded = append(loaded, threadID)
	}
	releasing := s.reconcileInteractiveReleases()
	if err := s.auditLoadedRetirement(client, loaded); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: archived-thread audit failed: %v\n", err)
	}
	for _, threadID := range loaded {
		if err := s.reconcileLoadedThread(client, threadID, releasing); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile loaded thread %s: %w", threadID, err))
		}
	}
	// Re-read membership after loaded-thread reconciliation. That work can take
	// seconds; using its initial snapshot here could remove a peer that attached
	// while the pass was in progress.
	if err := s.reconcileClosedInteractiveCandidatesFromAppServer(client); err != nil {
		reconcileErrors = append(reconcileErrors, fmt.Errorf("refresh closed-thread membership: %w", err))
	}
	s.enforceLaneDeadlines()
	s.recoverWakeRecords()
	s.reconcileLaneTerminalNotices(client)
	_, _ = s.flushLaneNotices("")
	s.reconcileLaneLifecycles(client)
	return errors.Join(reconcileErrors...)
}

func (s *nativeSupervisor) reconcileClosedInteractiveCandidatesFromAppServer(client *appServerClient) error {
	s.mu.Lock()
	hasCandidates := len(s.closedCandidates) != 0
	s.mu.Unlock()
	if !hasCandidates {
		return nil
	}
	loaded, err := loadedPreparedThreads(client)
	if err != nil {
		return err
	}
	s.reconcileClosedInteractiveCandidates(loaded)
	return nil
}

func (s *nativeSupervisor) reconcileClosedInteractiveCandidates(loaded map[string]bool) {
	s.mu.Lock()
	candidates := make(map[string]uint64, len(s.closedCandidates))
	for threadID, generation := range s.closedCandidates {
		candidates[threadID] = generation
	}
	s.mu.Unlock()
	for threadID, generation := range candidates {
		if !s.reconcileClosedInteractiveThreadWithLoaded(threadID, loaded[threadID]) {
			continue
		}
		s.mu.Lock()
		if s.closedCandidates[threadID] == generation {
			delete(s.closedCandidates, threadID)
		}
		s.mu.Unlock()
	}
}

func (s *nativeSupervisor) reconcileLoadedThread(client *appServerClient, threadID string, releasing map[string]bool) error {
	lifecycleLock, err := lockLaneLifecycle(s.paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	if s.skipLoadedThreadReconciliation(threadID, releasing) {
		return nil
	}
	thread, err := readExactPreparedThread(client, threadID)
	if err != nil {
		return nil
	}
	if thread.ID == "" || thread.ParentThreadID != "" || !rootThreadSource(thread.Source) {
		return nil
	}
	_, err = s.ensureShim(map[string]any{
		"sessionId": thread.ID, "cwd": thread.Cwd, "name": thread.Name,
		"nameSource": map[bool]string{true: "codex", false: "generated"}[thread.Name != ""],
		"status":     map[bool]string{true: "busy", false: "idle"}[statusType(thread.Status) == "active"],
	})
	if errors.Is(err, errInteractiveSessionEnding) {
		return nil
	}
	if err != nil {
		return err
	}
	if thread.Path != "" {
		_, _ = s.subscribeThread(thread.ID)
	}
	return nil
}

func (s *nativeSupervisor) skipLoadedThreadReconciliation(threadID string, releasing map[string]bool) bool {
	if releasing[threadID] || s.interactiveReleasePending(threadID) {
		return true
	}
	if s.isRetired(threadID) {
		s.removeShim(threadID)
		return true
	}
	if s.activeCodexLaneThread(threadID) {
		return false
	}
	if owner := readInteractiveOwner(s.paths, threadID); owner != nil {
		if owner.Pending {
			switch cleanupProcessIdentityStatus(owner.OwnerPID, owner.OwnerProcStart).Status {
			case processIdentityMatches:
				if owner.Prepared {
					return false
				}
			case processIdentityUnknown:
				return true
			case processIdentityStale:
			}
			s.removeShim(threadID)
			return true
		}
		switch cleanupProcessIdentityStatus(owner.OwnerPID, owner.OwnerProcStart).Status {
		case processIdentityMatches:
			return false
		case processIdentityUnknown:
			// Unknown is neither authorization nor proof that an existing
			// transport should be destroyed. Preserve it for a later retry.
			return true
		case processIdentityStale:
		}
	}
	s.removeShim(threadID)
	return true
}

func (s *nativeSupervisor) reconcileInteractiveReleases() map[string]bool {
	now := time.Now().UnixMilli()
	pending := map[string]bool{}
	s.mu.Lock()
	for threadID, deadline := range s.releasing {
		if deadline <= now {
			delete(s.releasing, threadID)
			continue
		}
		pending[threadID] = true
	}
	s.mu.Unlock()
	return pending
}

func (s *nativeSupervisor) reconcileLaneLifecycles(client *appServerClient) {
	retired := readRetiredThreads(s.paths)
	now := time.Now().UnixMilli()
	for _, state := range readLaneStates(s.paths) {
		if state.Type != "codex-peer-lane" || state.ThreadID == "" || state.Status == "archived" || retired[state.ThreadID] {
			continue
		}
		ownerExited := !state.Persistent && state.OwnerPID > 1 &&
			cleanupProcessIdentityStatus(state.OwnerPID, state.OwnerProcStart).Status == processIdentityStale
		autoArchiveDue := state.AutoArchive && state.AutoArchiveAt > 0 && state.AutoArchiveAt <= now
		if !ownerExited && !autoArchiveDue {
			continue
		}
		reason := "lane auto-archive delay elapsed"
		if ownerExited {
			reason = "lane owner exited"
		}
		if err := s.archiveLifecycleLane(client, state, reason, ownerExited); err != nil {
			fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: archive lifecycle lane %s: %v\n", state.ThreadID, err)
		}
	}
}

func (s *nativeSupervisor) archiveLifecycleLane(client *appServerClient, state laneState, reason string, interruptActive bool) error {
	lifecycleLock, err := lockLaneLifecycle(s.paths, state.ThreadID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	latest, err := readLaneStateFile(s.paths, state.ThreadID)
	if err != nil {
		return err
	}
	if !laneLifecycleArchiveDue(latest, interruptActive, time.Now().UnixMilli()) {
		return nil
	}
	state = latest
	var current struct {
		Thread appThread `json:"thread"`
	}
	if err := requestWithTimeout(client, 15*time.Second, "thread/read", map[string]any{
		"threadId": state.ThreadID, "includeTurns": false,
	}, &current); err != nil {
		return err
	}
	latest, err = readLaneStateFile(s.paths, state.ThreadID)
	if err != nil {
		return err
	}
	if !laneLifecycleArchiveDue(latest, interruptActive, time.Now().UnixMilli()) {
		return nil
	}
	state = latest
	if statusType(current.Thread.Status) == "active" {
		turnID := defaultString(state.LatestTurnID, state.TurnID)
		if interruptActive && turnID != "" {
			_ = requestWithTimeout(client, 10*time.Second, "turn/interrupt", map[string]any{
				"threadId": state.ThreadID, "turnId": turnID,
			}, nil)
		}
		return nil
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/archive", map[string]any{"threadId": state.ThreadID}, nil); err != nil {
		return err
	}
	if err := s.markRetired(state.ThreadID); err != nil {
		return err
	}
	_, _ = cancelLaneNotices(s.paths, state.ThreadID, reason)
	s.removeShim(state.ThreadID)
	_ = requestWithTimeout(client, 5*time.Second, "thread/unsubscribe", map[string]any{"threadId": state.ThreadID}, nil)
	stateLock, err := lockLaneStateFile(s.paths, state.ThreadID)
	if err != nil {
		return err
	}
	latest, err = readLaneStateFile(s.paths, state.ThreadID)
	if err == nil {
		latest.Status = "archived"
		latest.DeadlineAt = 0
		latest.AutoArchiveAt = 0
		err = writeLaneStateUnlocked(s.paths, latest)
	}
	unlockLaneStateFile(stateLock)
	if err != nil {
		return err
	}
	_ = clearLaneThreadItemSpool(s.paths, state.ThreadID)
	return nil
}

func laneLifecycleArchiveDue(state laneState, ownerExit bool, now int64) bool {
	if state.Type != "codex-peer-lane" || state.ThreadID == "" || state.Status == "archived" {
		return false
	}
	if ownerExit {
		return !state.Persistent && state.OwnerPID > 1 &&
			cleanupProcessIdentityStatus(state.OwnerPID, state.OwnerProcStart).Status == processIdentityStale
	}
	return state.AutoArchive && state.AutoArchiveAt > 0 && state.AutoArchiveAt <= now
}

func (s *nativeSupervisor) enforceLaneDeadlines() {
	now := time.Now().UnixMilli()
	for _, state := range readLaneStates(s.paths) {
		if state.DeadlineAt <= 0 || state.DeadlineAt > now || state.TurnID == "" || statusTerminal(state.Status) {
			continue
		}
		if err := s.timeoutLaneTurn(state.ThreadID, state.TurnID); err != nil {
			fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: enforce lane deadline for %s: %v\n", state.ThreadID, err)
		}
	}
}

func (s *nativeSupervisor) queueLaneTerminalNotice(threadID string, turn map[string]any) {
	turnID := stringValue(turn["id"])
	if threadID == "" || turnID == "" {
		return
	}
	var state laneState
	body, err := os.ReadFile(laneStatePath(s.paths, threadID))
	if err != nil || json.Unmarshal(body, &state) != nil || state.NotifyTarget == "" {
		return
	}
	if isRetiredThreadNative(s.paths, threadID) {
		return
	}
	status := normalizeStatus(turn["status"])
	if !statusTerminal(status) {
		return
	}
	outcome := laneTerminalOutcome(state, turnID, status)
	noticeID := sessionKey("lane-terminal\x00" + threadID + "\x00" + turnID)
	if _, err := os.Stat(filepath.Join(profileDataRoot(s.paths), "notices-sent", noticeID+".json")); err == nil {
		return
	}
	job := map[string]any{
		"noticeId": noticeID, "threadId": threadID, "turnId": turnID,
		"name": state.Name, "target": state.NotifyTarget, "status": status,
		"outcome": outcome, "exit": laneExitCode(outcome),
		"createdAt": time.Now().UnixMilli(), "parentHostId": state.ParentHostID,
		"parentAgentRuntimeDir": state.ParentAgentRuntimeDir, "groups": state.Groups,
	}
	directory := filepath.Join(profileDataRoot(s.paths), "notices")
	if err := writeJSONAtomic(filepath.Join(directory, noticeID+".json"), job); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native supervisor: persist terminal notice: %v\n", err)
		return
	}
	// Archive publishes its retirement marker before its final cancellation
	// sweep. Close the queue-vs-archive race even if retirement landed between
	// the preflight above and this atomic write.
	if isRetiredThreadNative(s.paths, threadID) {
		_, _ = cancelLaneNotices(s.paths, threadID, "lane archived while terminal notice was queued")
		return
	}
	_, _ = s.flushLaneNotices(threadID)
}

func (s *nativeSupervisor) flushLaneNotices(threadFilter string) (int, error) {
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	directory := filepath.Join(profileDataRoot(s.paths), "notices")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(directory, entry.Name())
		job := readJSONMap(file)
		threadID := stringValue(job["threadId"])
		if threadFilter != "" && threadID != threadFilter {
			continue
		}
		if !validSessionID(threadID) || stringValue(job["noticeId"])+".json" != entry.Name() {
			continue
		}
		groups := stringSlice(job["groups"])
		collect := laneCollectionPointer(
			"codex", threadID, stringValue(job["parentHostId"]), stringValue(job["parentAgentRuntimeDir"]), groups,
		)
		message := fmt.Sprintf(
			"CODEX_LANE_TERMINAL notice=%s name=%s thread=%s turn=%s status=%s outcome=%s exit=%d collection=required\nCollect: %s",
			stringValue(job["noticeId"]), stringValue(job["name"]), threadID,
			stringValue(job["turnId"]), stringValue(job["status"]), stringValue(job["outcome"]), intValue(job["exit"]), collect,
		)
		sendErr := deliverGroupedLaneNotice(threadID, stringValue(job["target"]), stringValue(job["noticeId"]), message)
		if sendErr != nil {
			pending++
			continue
		}
		if err := writeJSONAtomic(filepath.Join(profileDataRoot(s.paths), "notices-sent", stringValue(job["noticeId"])+".json"), map[string]any{
			"noticeId": stringValue(job["noticeId"]), "sentAt": time.Now().UnixMilli(),
		}); err != nil {
			pending++
			continue
		}
		removeJSONIf(file, func(current map[string]any) bool {
			return stringValue(current["noticeId"]) == stringValue(job["noticeId"])
		})
	}
	return pending, nil
}

func cancelLaneNotices(paths nativePaths, threadID, reason string) (int, error) {
	directory := filepath.Join(profileDataRoot(paths), "notices")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cancelled := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(directory, entry.Name())
		job := readJSONMap(file)
		if stringValue(job["threadId"]) != threadID {
			continue
		}
		noticeID := stringValue(job["noticeId"])
		if noticeID == "" || noticeID+".json" != entry.Name() {
			continue
		}
		if err := writeJSONAtomic(filepath.Join(profileDataRoot(paths), "notices-cancelled", noticeID+".json"), map[string]any{
			"noticeId": noticeID, "threadId": threadID, "cancelledAt": time.Now().UnixMilli(), "reason": reason,
		}); err != nil {
			return cancelled, err
		}
		removeJSONIf(file, func(current map[string]any) bool {
			return stringValue(current["noticeId"]) == noticeID
		})
		cancelled++
	}
	return cancelled, nil
}

func (s *nativeSupervisor) reconcileLaneTerminalNotices(client *appServerClient) {
	retired := readRetiredThreads(s.paths)
	for _, state := range readLaneStates(s.paths) {
		normalizeLanePendingTurns(&state)
		if state.TurnID == "" || state.Status == "archived" || retired[state.ThreadID] {
			continue
		}
		targets := map[string]bool{state.TurnID: true}
		turns, err := listLaneTurns(client, state.ThreadID, "notLoaded")
		if err != nil {
			continue
		}
		for _, turn := range turns {
			if !targets[turn.ID] || !statusTerminal(normalizeStatus(turn.Status)) {
				continue
			}
			turn.Items = append(turn.Items, readLaneItemSpool(s.paths, state.ThreadID, turn.ID)...)
			confirmedStatus, confirmed := laneCollectableTerminal(s.paths, state, turn)
			if !confirmed {
				continue
			}
			body, _ := json.Marshal(turn)
			var row map[string]any
			_ = json.Unmarshal(body, &row)
			row["status"] = confirmedStatus
			_ = persistLaneTerminalObservation(s.paths, state.ThreadID, row)
			terminalStatus := confirmedStatus
			if confirmedStatus == "completed" {
				action, _, schemaErr := s.processLaneSchemaTerminal(state.ThreadID, turn.ID)
				if schemaErr != nil {
					row = map[string]any{"id": turn.ID, "status": "failed", "error": schemaErr.Error()}
					terminalStatus = "failed"
				} else if action != laneSchemaNone {
					continue
				}
			}
			s.commitLaneTerminal(state.ThreadID, turn.ID, terminalStatus, row)
		}
	}
}

func (s *nativeSupervisor) auditLoadedRetirement(client *appServerClient, loaded []string) error {
	s.mu.Lock()
	if s.retirementAudited {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	active, err := listThreadMembership(client, false)
	if err != nil {
		return err
	}
	archived, err := listThreadMembership(client, true)
	if err != nil {
		return err
	}
	candidates := map[string]bool{}
	for _, threadID := range loaded {
		candidates[threadID] = true
	}
	for threadID := range active {
		candidates[threadID] = true
	}
	for threadID := range archived {
		candidates[threadID] = true
	}
	for threadID := range candidates {
		if !s.knownPeerLineage(threadID) {
			continue
		}
		if active[threadID] {
			s.clearRetired(threadID)
		} else if archived[threadID] {
			if err := s.markRetired(threadID); err != nil {
				return err
			}
			s.removeShim(threadID)
		}
	}
	s.mu.Lock()
	s.retirementAudited = true
	s.mu.Unlock()
	return nil
}

func listThreadMembership(client *appServerClient, archived bool) (map[string]bool, error) {
	threads := map[string]bool{}
	cursor := ""
	seenCursors := map[string]bool{}
	for len(threads) < 10000 {
		params := map[string]any{
			"archived": archived, "limit": 100, "sortDirection": "desc", "sortKey": "updated_at",
			"sourceKinds": []string{
				"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview",
				"subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown",
			},
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appThread `json:"data"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := requestWithTimeout(client, 30*time.Second, "thread/list", params, &page); err != nil {
			return nil, err
		}
		for _, thread := range page.Data {
			if validSessionID(thread.ID) {
				threads[thread.ID] = true
			}
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			return threads, nil
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return threads, nil
}

func (s *nativeSupervisor) shutdown() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		s.ready = false
		ids := make([]string, 0, len(s.shims))
		for id := range s.shims {
			ids = append(ids, id)
		}
		s.mu.Unlock()
		sort.Strings(ids)
		for _, id := range ids {
			s.removeShim(id)
		}
		s.clientMu.Lock()
		if s.client != nil {
			s.client.close()
		}
		s.clientMu.Unlock()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.paths.supervisorSock)
		state := readJSONMap(s.paths.supervisorState)
		if intValue(state["pid"]) == os.Getpid() {
			_ = os.Remove(s.paths.supervisorState)
		}
	})
}

func stopNativeSupervisor(paths nativePaths) (map[string]any, error) {
	state := readJSONMap(paths.supervisorState)
	pid, procStart := intValue(state["pid"]), stringValue(state["procStart"])
	if err := stopExistingNativeSupervisor(paths.supervisorSock, 10*time.Second); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for pid > 1 && procStart != "" && processIdentityMayBeLive(pid, procStart) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pid > 1 && procStart != "" && processIdentityMayBeLive(pid, procStart) {
		return nil, fmt.Errorf("supervisor process %d did not stop", pid)
	}
	return map[string]any{"stopped": true}, nil
}

func nativeExecutableIdentity(executable string) (string, error) {
	body, err := os.ReadFile(executable) //nolint:gosec // executable is resolved from the running bridge process.
	if err != nil {
		return "", fmt.Errorf("read native runtime identity: %w", err)
	}
	digest := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func nativeSupervisorMatches(status map[string]any, paths nativePaths, version, runtimeIdentity string) bool {
	ready, _ := status["ready"].(bool)
	return ready && stringValue(status["implementation"]) == "go" &&
		stringValue(status["pluginVersion"]) == version &&
		stringValue(status["runtimeIdentity"]) == runtimeIdentity &&
		samePath(stringValue(status["appServerSocket"]), paths.appServerSock)
}

// stopLegacyNativeSupervisor removes the single global supervisor used before
// CODEX_HOME namespacing. Leaving it alive would keep a second App Server
// subscriber and a second shim owner after an otherwise successful upgrade.
func stopLegacyNativeSupervisor(paths nativePaths) error {
	legacySocket := filepath.Join(bridgeRuntimeRoot(paths.runtimeDir, os.Getuid()), "supervisor.sock")
	if samePath(legacySocket, paths.supervisorSock) {
		return nil
	}
	status, err := requestControl(legacySocket, map[string]any{"action": "status"}, 500*time.Millisecond)
	if err != nil {
		if probeUnixSocket(legacySocket, 200*time.Millisecond) {
			return fmt.Errorf("refuse supervisor migration while legacy socket %s is live but unresponsive", legacySocket)
		}
		_ = os.Remove(legacySocket)
		return nil
	}
	if stringValue(status["implementation"]) != "go" {
		return fmt.Errorf("refuse to stop unknown legacy supervisor at %s", legacySocket)
	}
	if appServerSocket := stringValue(status["appServerSocket"]); !samePath(appServerSocket, paths.appServerSock) {
		return fmt.Errorf("legacy supervisor at %s belongs to App Server %s, not %s", legacySocket, appServerSocket, paths.appServerSock)
	}
	if err := stopExistingNativeSupervisor(legacySocket, 10*time.Second); err != nil {
		return fmt.Errorf("retire legacy supervisor: %w", err)
	}
	return nil
}

func stopExistingNativeSupervisor(socket string, timeout time.Duration) error {
	if _, err := requestControl(socket, map[string]any{"action": "stop"}, min(timeout, 2*time.Second)); err != nil {
		return fmt.Errorf("stop existing native supervisor: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !probeUnixSocket(socket, min(timeout, 200*time.Millisecond)) {
			return nil
		}
		time.Sleep(min(timeout, 50*time.Millisecond))
	}
	if probeUnixSocket(socket, min(timeout, 200*time.Millisecond)) {
		return fmt.Errorf("timed out waiting for existing supervisor %s to stop", socket)
	}
	return nil
}

//nolint:unparam // Keeping the timeout explicit makes the socket boundary reusable in focused tests and adapters.
func sendUnixJSON(socket string, frame map[string]any, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	body, _ := json.Marshal(frame)
	if len(body)+1 > maxFrameBytes {
		return errors.New("peer message exceeds Claude Code's 1 MiB frame limit")
	}
	_, err = conn.Write(append(body, '\n'))
	return err
}

func probeUnixSocket(socket string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", socket, timeout) // #nosec G704 -- The network is fixed to a local Unix socket; callers validate bridge-owned or peer-advertised paths.
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func readJSONMap(file string) map[string]any {
	body, err := os.ReadFile(file) //nolint:gosec // callers supply fixed bridge-owned JSON state paths.
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return nil
	}
	return value
}

func peerMessageText(item map[string]any) string {
	sender := stringValue(item["from"])
	if name := stringValue(item["fromName"]); name != "" {
		sender = fmt.Sprintf("%s (%s)", name, defaultString(sender, "unknown address"))
	}
	if sender == "" {
		sender = "an unidentified peer"
	}
	metadata := []string{}
	for _, pair := range [][2]string{
		{"sender-type", stringValue(item["fromProduct"])}, {"message-id", stringValue(item["id"])},
		{"sent-at", stringValue(item["sentAt"])}, {"received-at", stringValue(item["receivedAt"])},
	} {
		if pair[1] != "" {
			metadata = append(metadata, pair[0]+"="+pair[1])
		}
	}
	parts := []string{"Message from " + sender + ":"}
	if len(metadata) > 0 {
		parts = append(parts, "Message metadata: "+strings.Join(metadata, ", "))
	}
	parts = append(parts, stringValue(item["message"]))
	return strings.Join(parts, "\n\n")
}

func statusType(value any) string {
	switch current := value.(type) {
	case string:
		if current == "inProgress" {
			return "active"
		}
		return current
	case map[string]any:
		return stringValue(current["type"])
	}
	return ""
}

func rootThreadSource(source any) bool {
	value, ok := source.(string)
	return ok && (value == "cli" || value == "vscode" || value == "appServer")
}

func validSessionID(value string) bool {
	if len(value) < 1 || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func defaultPeerName(cwd, sessionID string) string {
	project := sanitizeName(filepath.Base(cwd))
	return sanitizeName("codex-" + project + "-" + first8(sessionID))
}

func mustGetwd() string {
	cwd, _ := os.Getwd()
	return cwd
}

func overrideNativeEnv(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
