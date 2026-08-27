package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const grokDaemonLeaderReadyTimeout = 10 * time.Second

// grokNativeCoordinator owns Grok's required private leader and headless ACP
// clients as goroutines and child processes of the one user daemon. It never
// creates a grok-host process or per-session Agent Sessions listener.
type grokNativeCoordinator struct {
	mu        sync.Mutex
	actors    map[string]*grokDaemonActor
	sessions  map[string]string
	recovered bool
}

type grokDaemonActor struct {
	mu sync.Mutex

	coordinatorID string
	sessionID     string
	selector      string
	lateBound     bool
	name          string
	cwd           string
	profile       string
	permission    string
	grokBin       string
	leaderSocket  string
	actorRoot     string
	recordPath    string

	ownerPID        int
	ownerProcStart  string
	leaderPID       int
	leaderProcStart string
	leader          *grokManagedProcess
	diagnostics     *grokDiagnosticSink
	acp             *grokACPClient
	rosterUpdates   chan grokRosterState
	commandOverride func(...string) *exec.Cmd
	closed          bool
}

type grokDaemonActorRecord struct {
	CoordinatorID  string `json:"coordinator_id"`
	SessionID      string `json:"session_id,omitempty"`
	Selector       string `json:"selector,omitempty"`
	LateBound      bool   `json:"late_bound"`
	Name           string `json:"name,omitempty"`
	Cwd            string `json:"cwd"`
	Profile        string `json:"profile"`
	PermissionMode string `json:"permission_mode"`
	LeaderPID      int    `json:"leader_pid"`
	LeaderStart    string `json:"leader_start"`
	LeaderSocket   string `json:"leader_socket"`
	OwnerPID       int    `json:"owner_pid,omitempty"`
	OwnerStart     string `json:"owner_start,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

func newGrokNativeCoordinator() *grokNativeCoordinator {
	return &grokNativeCoordinator{actors: make(map[string]*grokDaemonActor), sessions: make(map[string]string)}
}

// PrepareInteractive starts one daemon-owned native leader and returns the direct Grok TUI handoff.
func (coordinator *grokNativeCoordinator) PrepareInteractive(
	ctx context.Context,
	request daemonpkg.AttachmentPrepareRequest,
) (daemonpkg.NativeLaunchPlan, error) {
	coordinator.ensureRecovered()
	actor, err := newGrokDaemonActor(request)
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	if err := actor.start(ctx); err != nil {
		actor.close()
		return daemonpkg.NativeLaunchPlan{}, err
	}
	coordinator.mu.Lock()
	if existing := coordinator.sessions[actor.sessionID]; actor.sessionID != "" && existing != "" && existing != actor.coordinatorID {
		coordinator.mu.Unlock()
		actor.close()
		return daemonpkg.NativeLaunchPlan{}, errors.New("grok session already has a daemon-owned leader")
	}
	coordinator.actors[actor.coordinatorID] = actor
	if actor.sessionID != "" {
		coordinator.sessions[actor.sessionID] = actor.coordinatorID
	}
	coordinator.mu.Unlock()
	return actor.launchPlan(request), nil
}

// ResolveSession selects one exact or uniquely named live resident Grok session.
func (coordinator *grokNativeCoordinator) ResolveSession(
	ctx context.Context,
	selector string,
) (grokDaemonSession, bool, error) {
	coordinator.ensureRecovered()
	actors := coordinator.actorSnapshot()
	matches := make([]grokDaemonSession, 0, 1)
	for _, actor := range actors {
		session, err := actor.inspect(ctx)
		if err != nil {
			continue
		}
		if session.SessionID == selector || session.Name == selector {
			matches = append(matches, session)
		}
	}
	if len(matches) == 0 {
		return grokDaemonSession{}, false, daemonpkg.ErrAttachmentNotFound
	}
	return matches[0], len(matches) > 1, nil
}

// ObserveSession binds a late native selection using exact connector ancestry.
func (coordinator *grokNativeCoordinator) ObserveSession(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	connectorPID int,
) (grokDaemonSession, error) {
	coordinator.ensureRecovered()
	coordinatorID := strings.TrimSpace(stringValue(record.NativeActor["coordinator_id"]))
	actor := coordinator.actorByID(coordinatorID)
	if actor == nil {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentSelecting
	}
	session, err := actor.observe(ctx, connectorPID)
	if err != nil {
		return grokDaemonSession{}, err
	}
	coordinator.mu.Lock()
	if existing := coordinator.sessions[session.SessionID]; existing != "" && existing != coordinatorID {
		coordinator.mu.Unlock()
		actor.close()
		return grokDaemonSession{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	coordinator.sessions[session.SessionID] = coordinatorID
	coordinator.mu.Unlock()
	go actor.watchOwner()
	return session, nil
}

// InspectSession revalidates one exact native owner, leader, and ACP roster row.
func (coordinator *grokNativeCoordinator) InspectSession(ctx context.Context, sessionID string) (grokDaemonSession, error) {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	coordinatorID := coordinator.sessions[sessionID]
	actor := coordinator.actors[coordinatorID]
	coordinator.mu.Unlock()
	if actor == nil {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	return actor.inspect(ctx)
}

// InterjectFrame delivers one admitted AgentFrame through the resident actor's ACP channel.
func (coordinator *grokNativeCoordinator) InterjectFrame(ctx context.Context, sessionID string, frame federation.AgentFrame) error {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	actor := coordinator.actors[coordinator.sessions[sessionID]]
	coordinator.mu.Unlock()
	if actor == nil {
		return daemonpkg.ErrAttachmentNotFound
	}
	return actor.interject(ctx, frame)
}

func (coordinator *grokNativeCoordinator) actorByID(coordinatorID string) *grokDaemonActor {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.actors[coordinatorID]
}

func (coordinator *grokNativeCoordinator) actorSnapshot() []*grokDaemonActor {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	result := make([]*grokDaemonActor, 0, len(coordinator.actors))
	for _, actor := range coordinator.actors {
		result = append(result, actor)
	}
	return result
}

func (coordinator *grokNativeCoordinator) close() {
	coordinator.mu.Lock()
	actors := coordinator.actors
	coordinator.actors = make(map[string]*grokDaemonActor)
	coordinator.sessions = make(map[string]string)
	coordinator.mu.Unlock()
	for _, actor := range actors {
		actor.close()
	}
}

func (coordinator *grokNativeCoordinator) ensureRecovered() {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.recovered {
		return
	}
	coordinator.recovered = true
	coordinator.recoverActorsLocked()
}

func (coordinator *grokNativeCoordinator) recoverActorsLocked() {
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return
	}
	directory := filepath.Join(paths.StateRoot, "grok", "actors")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		actor := recoverGrokDaemonActor(filepath.Join(directory, entry.Name()))
		if actor == nil {
			continue
		}
		coordinator.actors[actor.coordinatorID] = actor
		coordinator.sessions[actor.sessionID] = actor.coordinatorID
		go actor.watchOwner()
	}
}

func recoverGrokDaemonActor(path string) *grokDaemonActor {
	record, ok := readGrokDaemonActorRecord(path)
	if !ok || !validRecoveredGrokActorRecord(record) {
		return nil
	}
	socketInfo, err := os.Lstat(record.LeaderSocket)
	if err != nil || socketInfo.Mode()&os.ModeSymlink != 0 || socketInfo.Mode()&os.ModeSocket == 0 {
		return nil
	}
	grokBin, err := grokDaemonExecutable()
	if err != nil {
		return nil
	}
	actorRoot := filepath.Dir(record.LeaderSocket)
	diagnostics, err := newGrokDiagnosticSink(filepath.Join(actorRoot, "diagnostics.log"))
	if err != nil {
		return nil
	}
	return &grokDaemonActor{
		coordinatorID: record.CoordinatorID, sessionID: record.SessionID, selector: record.Selector,
		lateBound: record.LateBound, name: record.Name, cwd: record.Cwd, profile: record.Profile,
		permission: record.PermissionMode, grokBin: grokBin, leaderSocket: record.LeaderSocket,
		actorRoot: actorRoot, recordPath: path, ownerPID: record.OwnerPID, ownerProcStart: record.OwnerStart,
		leaderPID: record.LeaderPID, leaderProcStart: record.LeaderStart, diagnostics: diagnostics,
		rosterUpdates: make(chan grokRosterState, 8),
	}
}

func readGrokDaemonActorRecord(path string) (grokDaemonActorRecord, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return grokDaemonActorRecord{}, false
	}
	body, err := os.ReadFile(path) //nolint:gosec // bounded daemon-owned actor record.
	if err != nil {
		return grokDaemonActorRecord{}, false
	}
	var record grokDaemonActorRecord
	if json.Unmarshal(body, &record) != nil {
		return grokDaemonActorRecord{}, false
	}
	return record, true
}

func validRecoveredGrokActorRecord(record grokDaemonActorRecord) bool {
	return record.CoordinatorID != "" && validSessionID(record.SessionID) && filepath.IsAbs(record.Cwd) &&
		filepath.IsAbs(record.Profile) && filepath.IsAbs(record.LeaderSocket) &&
		exactProcessIdentityStatus(record.LeaderPID, record.LeaderStart).Status == processIdentityMatches &&
		exactProcessIdentityStatus(record.OwnerPID, record.OwnerStart).Status == processIdentityMatches
}

func newGrokDaemonActor(request daemonpkg.AttachmentPrepareRequest) (*grokDaemonActor, error) {
	profile, err := canonicalGrokProfile(stringValue(request.ProfileIdentity["profile"]))
	if err != nil {
		return nil, err
	}
	grokBin, err := grokDaemonExecutable()
	if err != nil {
		return nil, err
	}
	coordinatorID, err := randomGrokCoordinatorID()
	if err != nil {
		return nil, err
	}
	sessionID := strings.TrimSpace(request.Intent.Selector)
	lateBound := request.Intent.Mode == "resume" && !exactLaunchThreadIDRE.MatchString(sessionID)
	if request.Intent.Mode != "resume" {
		sessionID, err = randomGrokSessionID()
		if err != nil {
			return nil, err
		}
	}
	if lateBound {
		sessionID = ""
	}
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return nil, err
	}
	actorRoot := filepath.Join(paths.RuntimeRoot, "grok-"+coordinatorID[:16])
	leaderSocket := filepath.Join(actorRoot, "leader.sock")
	if err := socketpath.Validate(leaderSocket); err != nil {
		return nil, fmt.Errorf("validate daemon-owned Grok leader socket: %w", err)
	}
	return &grokDaemonActor{
		coordinatorID: coordinatorID, sessionID: sessionID, selector: strings.TrimSpace(request.Intent.Selector),
		lateBound: lateBound, name: request.Name, cwd: request.Cwd, profile: profile,
		permission: defaultString(request.PermissionMode, "default"), grokBin: grokBin,
		leaderSocket: leaderSocket, actorRoot: actorRoot,
		recordPath:    filepath.Join(paths.StateRoot, "grok", "actors", coordinatorID+".json"),
		rosterUpdates: make(chan grokRosterState, 8),
	}, nil
}

func (actor *grokDaemonActor) start(ctx context.Context) error {
	if err := os.MkdirAll(actor.actorRoot, 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(actor.leaderSocket); err == nil || !os.IsNotExist(err) {
		return errors.New("daemon-owned Grok leader socket already exists")
	}
	diagnostics, err := newGrokDiagnosticSink(filepath.Join(actor.actorRoot, "diagnostics.log"))
	if err != nil {
		return err
	}
	actor.diagnostics = diagnostics
	processDiagnostics := diagnostics.process("daemon-owned Grok leader")
	command := actor.command(
		"--permission-mode", "default", "agent", "leader", "--leader-socket", actor.leaderSocket,
		"--no-exit-on-disconnect", "--relay-on-demand", "--no-auto-update",
	)
	command.Stdout, command.Stderr = processDiagnostics, processDiagnostics
	leader, err := startGrokManagedProcess(command, processDiagnostics)
	if err != nil {
		return (&grokManagedProcess{diagnostics: processDiagnostics}).attributedError("start daemon-owned Grok leader", err)
	}
	actor.leader = leader
	actor.leaderPID, actor.leaderProcStart = leader.cmd.Process.Pid, leader.procStart
	if err := actor.persist(); err != nil {
		return err
	}
	return actor.waitForLeader(ctx, grokDaemonLeaderReadyTimeout)
}

func (actor *grokDaemonActor) launchPlan(request daemonpkg.AttachmentPrepareRequest) daemonpkg.NativeLaunchPlan {
	arguments := []string{"--leader", "--leader-socket", actor.leaderSocket, "--sandbox", "off"}
	switch {
	case actor.lateBound:
		arguments = append(arguments, "--resume", actor.selector)
	case request.Intent.Mode == "resume":
		arguments = append(arguments, "--resume", actor.sessionID)
	default:
		arguments = append(arguments, "--session-id", actor.sessionID)
	}
	arguments = append(arguments, request.Intent.NativeArguments...)
	expected := map[string]any{
		"coordinator_id": actor.coordinatorID, "leader_session_id": actor.coordinatorID,
		"leader_pid": actor.leaderPID, "leader_proc_start": actor.leaderProcStart,
	}
	return daemonpkg.NativeLaunchPlan{
		Executable: actor.grokBin, Arguments: arguments, Environment: map[string]string{"HOME": actor.profile},
		// Grok owner identity is observable only after its MCP connector exists.
		// Keep the inherited connector session empty so the daemon adopts from
		// exact ancestry and the private ACP roster instead of trusting argv.
		SessionID: "", Cwd: actor.cwd, ExpectedNativeActor: expected,
	}
}

func (actor *grokDaemonActor) observe(ctx context.Context, connectorPID int) (grokDaemonSession, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.closed {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	ownerPID, ownerStart, err := observedGrokOwner(connectorPID)
	if err != nil {
		return grokDaemonSession{}, err
	}
	state, selectedID, err := actor.refreshRosterLocked(ctx)
	if err != nil {
		return grokDaemonSession{}, err
	}
	actor.ownerPID, actor.ownerProcStart = ownerPID, ownerStart
	actor.sessionID = selectedID
	if actor.name == "" && state.name != "" {
		actor.name = state.name
	}
	actor.permission = state.permissionMode
	actor.lateBound = false
	if err := actor.persist(); err != nil {
		return grokDaemonSession{}, err
	}
	return actor.sessionLocked(true), nil
}

func (actor *grokDaemonActor) inspect(ctx context.Context) (grokDaemonSession, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.closed || actor.sessionID == "" {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	if actor.ownerPID <= 1 || exactProcessIdentityStatus(actor.ownerPID, actor.ownerProcStart).Status != processIdentityMatches {
		return grokDaemonSession{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	state, selectedID, err := actor.refreshRosterLocked(ctx)
	if err != nil || selectedID != actor.sessionID {
		if err == nil {
			err = daemonpkg.ErrAttachmentEvidenceChanged
		}
		return grokDaemonSession{}, err
	}
	actor.name, actor.permission = defaultString(state.name, actor.name), state.permissionMode
	return actor.sessionLocked(true), nil
}

func (actor *grokDaemonActor) interject(ctx context.Context, frame federation.AgentFrame) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.closed || actor.sessionID == "" {
		return daemonpkg.ErrAttachmentNotFound
	}
	if _, _, err := actor.refreshRosterLocked(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	text := federation.AgentFrameCarrierPrefix + string(body)
	return actor.acp.requestInterjection(ctx, actor.sessionID, frame.MessageID, text)
}

func (actor *grokDaemonActor) refreshRosterLocked(ctx context.Context) (grokRosterState, string, error) {
	if err := actor.ensureACPLocked(ctx); err != nil {
		return grokRosterState{}, "", err
	}
	result, err := actor.acp.request(ctx, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return grokRosterState{}, "", fmt.Errorf("query daemon-owned Grok roster: %w", err)
	}
	if actor.lateBound || actor.sessionID == "" {
		selectedID, state, selectionErr := grokSelectedResidentSession(result)
		return state, selectedID, selectionErr
	}
	state, err := grokRosterStateFromResponse(result, actor.sessionID)
	return state, actor.sessionID, err
}

func (actor *grokDaemonActor) ensureACPLocked(ctx context.Context) error {
	if actor.acp != nil {
		select {
		case <-actor.acp.readDone:
			actor.acp.close()
			actor.acp = nil
		default:
			return nil
		}
	}
	processDiagnostics := actor.diagnostics.process("daemon-owned Grok ACP observer")
	command := actor.command(
		"--no-auto-update", "--permission-mode", "default", "--leader-socket", actor.leaderSocket,
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
	command.Stderr = processDiagnostics
	process, err := startGrokManagedProcess(command, processDiagnostics)
	if err != nil {
		return (&grokManagedProcess{diagnostics: processDiagnostics}).attributedError("start daemon-owned Grok ACP observer", err)
	}
	actor.acp = newGrokACPClient(process, stdin, stdout, actor.sessionID, 1, actor.rosterUpdates)
	initialized, err := actor.acp.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		actor.closeACPLocked()
		return err
	}
	if !grokAuthMethodAdvertised(initialized, "cached_token") {
		actor.closeACPLocked()
		return errors.New("cached_token authentication was not advertised by Grok")
	}
	if _, err := actor.acp.request(ctx, "authenticate", map[string]any{
		"methodId": "cached_token", "_meta": map[string]any{"headless": true},
	}); err != nil {
		actor.closeACPLocked()
		return err
	}
	return nil
}

func (actor *grokDaemonActor) command(arguments ...string) *exec.Cmd {
	var command *exec.Cmd
	if actor.commandOverride != nil {
		command = actor.commandOverride(arguments...)
	} else {
		command = exec.Command(actor.grokBin, arguments...) //nolint:gosec // validated local Grok CLI and fixed coordinator argv.
	}
	command.Env = envutil.Set(os.Environ(), "HOME", actor.profile)
	command.Env = envutil.Set(command.Env, grokSessionIDEnv, actor.sessionID)
	return command
}

func (actor *grokDaemonActor) waitForLeader(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("timed out waiting for daemon-owned Grok leader socket")
		case <-actor.leader.done:
			return actor.leader.attributedError("daemon-owned Grok leader exited", nil)
		case <-ticker.C:
			info, err := os.Lstat(actor.leaderSocket)
			if err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode()&os.ModeSocket != 0 {
				return nil
			}
		}
	}
}

func (actor *grokDaemonActor) sessionLocked(ready bool) grokDaemonSession {
	return grokDaemonSession{
		SessionID: actor.sessionID, Name: actor.name, Cwd: actor.cwd, Profile: actor.profile,
		OwnerPID: actor.ownerPID, OwnerProcStart: actor.ownerProcStart,
		LeaderSessionID: actor.coordinatorID, ACPReady: ready, CoordinatorID: actor.coordinatorID,
	}
}

func (actor *grokDaemonActor) persist() error {
	if actor.leaderPID <= 1 || actor.leaderProcStart == "" {
		return errors.New("cannot persist Grok actor without a leader")
	}
	record := grokDaemonActorRecord{
		CoordinatorID: actor.coordinatorID, SessionID: actor.sessionID, Selector: actor.selector,
		LateBound: actor.lateBound, Name: actor.name, Cwd: actor.cwd, Profile: actor.profile,
		PermissionMode: actor.permission, LeaderPID: actor.leaderPID,
		LeaderStart: actor.leaderProcStart, LeaderSocket: actor.leaderSocket,
		OwnerPID: actor.ownerPID, OwnerStart: actor.ownerProcStart, UpdatedAt: time.Now().UnixMilli(),
	}
	return writeJSONAtomic(actor.recordPath, record)
}

func (actor *grokDaemonActor) watchOwner() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		actor.mu.Lock()
		dead := actor.closed || (actor.ownerPID > 1 && exactProcessIdentityStatus(actor.ownerPID, actor.ownerProcStart).Status == processIdentityStale)
		actor.mu.Unlock()
		if dead {
			actor.close()
			return
		}
	}
}

func (actor *grokDaemonActor) close() {
	actor.mu.Lock()
	if actor.closed {
		actor.mu.Unlock()
		return
	}
	actor.closed = true
	actor.closeACPLocked()
	leaderPID, leaderStart := actor.leaderPID, actor.leaderProcStart
	stopGrokManagedProcess(actor.leader, 2*time.Second)
	if actor.leader == nil {
		stopStaleGrokProcess(leaderPID, leaderStart)
	}
	if actor.diagnostics != nil {
		_ = actor.diagnostics.close()
	}
	removeJSONIf(actor.recordPath, func(row map[string]any) bool {
		return stringValue(row["coordinator_id"]) == actor.coordinatorID &&
			leaderPID > 1 && intValue(row["leader_pid"]) == leaderPID &&
			stringValue(row["leader_start"]) == leaderStart
	})
	_ = os.Remove(actor.leaderSocket)
	_ = os.Remove(filepath.Join(actor.actorRoot, "diagnostics.log"))
	_ = os.Remove(actor.actorRoot)
	actor.mu.Unlock()
}

func (actor *grokDaemonActor) closeACPLocked() {
	if actor.acp != nil {
		actor.acp.close()
		actor.acp = nil
	}
}

func observedGrokOwner(connectorPID int) (int, string, error) {
	pid := connectorPID
	for depth := 0; depth < 64 && pid > 1; depth++ {
		arguments, err := procinfo.Args(pid)
		if err == nil && looksLikeNativeGrok(arguments) {
			identity := procinfo.Read(pid)
			if identity.Status == procinfo.Known && identity.Start != "" {
				return pid, identity.Start, nil
			}
			return 0, "", daemonpkg.ErrAttachmentEvidenceChanged
		}
		identity := procinfo.Read(pid)
		if identity.Status != procinfo.Known || identity.Parent <= 1 || identity.Parent == pid {
			break
		}
		pid = identity.Parent
	}
	return 0, "", daemonpkg.ErrAttachmentSelecting
}

func looksLikeNativeGrok(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	name := strings.ToLower(filepath.Base(arguments[0]))
	return name == "grok" || name == "grok.exe"
}

func canonicalGrokProfile(configured string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		configured = os.Getenv("HOME")
	}
	if !filepath.IsAbs(configured) {
		return "", errors.New("grok profile HOME must be absolute")
	}
	clean := filepath.Clean(configured)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	return clean, nil
}

func grokDaemonExecutable() (string, error) {
	configured := strings.TrimSpace(os.Getenv("GROK_PEER_GROK_BIN"))
	if configured == "" {
		configured = "grok"
	}
	executable, err := exec.LookPath(configured)
	if err == nil {
		return executable, nil
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		fallback := filepath.Join(home, ".grok", "bin", "grok")
		if info, statErr := os.Stat(fallback); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return fallback, nil
		}
	}
	return "", fmt.Errorf("resolve Grok executable %q: %w", configured, err)
}

func randomGrokCoordinatorID() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func randomGrokSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
