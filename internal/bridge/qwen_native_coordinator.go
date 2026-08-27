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
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const qwenDaemonAdmissionTimeout = 20 * time.Second

var qwenDaemonReadinessCheck = qwenreadiness.Check

// qwenNativeCoordinator owns only Agent Sessions' dual-output files, cursors,
// and input descriptors. The Qwen TUI remains the direct vendor process; no
// qwen-host process or per-session listener exists.
type qwenNativeCoordinator struct {
	mu        sync.Mutex
	actors    map[string]*qwenDaemonActor
	sessions  map[string]string
	recovered bool
}

type qwenDaemonActor struct {
	mu sync.Mutex

	coordinatorID string
	sessionID     string
	name          string
	cwd           string
	profile       qwenprofile.Identity
	version       string
	actorRoot     string
	inputPath     string
	eventPath     string
	recordPath    string
	inputIdentity federator.QwenArtifactAttestation
	eventIdentity federator.QwenArtifactAttestation

	ownerPID       int
	ownerProcStart string
	parentPID      int
	status         string
	permissionMode string
	ready          bool
	cursor         *qwenEventCursor
	input          *qwenInputWriter
	eventCancel    context.CancelFunc
	suspended      bool
	retired        bool
}

type qwenDaemonActorRecord struct {
	CoordinatorID  string                            `json:"coordinator_id"`
	SessionID      string                            `json:"session_id"`
	Name           string                            `json:"name"`
	Cwd            string                            `json:"cwd"`
	Profile        qwenprofile.Identity              `json:"profile"`
	Version        string                            `json:"version"`
	ActorRoot      string                            `json:"actor_root"`
	Input          federator.QwenArtifactAttestation `json:"input"`
	Events         federator.QwenArtifactAttestation `json:"events"`
	OwnerPID       int                               `json:"owner_pid,omitempty"`
	OwnerStart     string                            `json:"owner_start,omitempty"`
	ParentPID      int                               `json:"parent_pid,omitempty"`
	Status         string                            `json:"status,omitempty"`
	PermissionMode string                            `json:"permission_mode,omitempty"`
	Ready          bool                              `json:"ready"`
	Active         bool                              `json:"active"`
	UpdatedAt      int64                             `json:"updated_at"`
}

func newQwenNativeCoordinator() *qwenNativeCoordinator {
	return &qwenNativeCoordinator{actors: make(map[string]*qwenDaemonActor), sessions: make(map[string]string)}
}

// PrepareInteractive creates exact dual-output artifacts and returns a direct Qwen handoff.
func (coordinator *qwenNativeCoordinator) PrepareInteractive(
	ctx context.Context,
	request daemonpkg.AttachmentPrepareRequest,
) (daemonpkg.NativeLaunchPlan, error) {
	coordinator.ensureRecovered()
	profile, err := qwenDaemonProfile(request.ProfileIdentity)
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	executable, err := qwenDaemonExecutable()
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	report, err := qwenDaemonReadinessCheck(ctx, qwenreadiness.Request{
		Executable: executable, Workspace: request.Cwd, Profile: profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(qwenprofile.ApplyEnvironment(os.Environ(), profile)),
	})
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, fmt.Errorf("check Qwen readiness: %w", err)
	}
	if !report.Ready {
		return daemonpkg.NativeLaunchPlan{}, qwenDaemonReadinessError(report)
	}
	sessionID, err := coordinator.resolveLaunchSession(request.Intent, profile.Fingerprint)
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	actor, err := newQwenDaemonActor(request, profile, report.Version, sessionID)
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	if err := actor.prepare(); err != nil {
		actor.rollbackPreparation()
		return daemonpkg.NativeLaunchPlan{}, err
	}
	coordinator.mu.Lock()
	if existing := coordinator.sessions[sessionID]; existing != "" && existing != actor.coordinatorID {
		coordinator.mu.Unlock()
		actor.retire()
		return daemonpkg.NativeLaunchPlan{}, errors.New("qwen session already has active daemon protocol artifacts")
	}
	coordinator.actors[actor.coordinatorID] = actor
	coordinator.sessions[sessionID] = actor.coordinatorID
	coordinator.mu.Unlock()
	return actor.launchPlan(request, executable), nil
}

// ResolveSession selects one exact or uniquely named active Qwen session.
func (coordinator *qwenNativeCoordinator) ResolveSession(_ context.Context, selector string) (qwenDaemonSession, bool, error) {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	matches := make([]qwenDaemonSession, 0, 1)
	for _, actor := range coordinator.actors {
		actor.mu.Lock()
		if !actor.retired && actor.ready && (actor.sessionID == selector || actor.name == selector) {
			matches = append(matches, actor.sessionLocked())
		}
		actor.mu.Unlock()
	}
	if len(matches) == 0 {
		return qwenDaemonSession{}, false, daemonpkg.ErrAttachmentNotFound
	}
	return matches[0], len(matches) > 1, nil
}

// ObserveSession admits Qwen's first event and exact vendor ancestry for one connector.
func (coordinator *qwenNativeCoordinator) ObserveSession(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	connectorPID int,
) (qwenDaemonSession, error) {
	coordinator.ensureRecovered()
	coordinatorID := strings.TrimSpace(stringValue(record.NativeActor["coordinator_id"]))
	coordinator.mu.Lock()
	actor := coordinator.actors[coordinatorID]
	coordinator.mu.Unlock()
	if actor == nil {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentSelecting
	}
	session, err := actor.observe(ctx, connectorPID)
	if err != nil {
		return qwenDaemonSession{}, err
	}
	go actor.watchOwner()
	return session, nil
}

// InspectSession re-attests one active Qwen process and its exact dual-output files.
func (coordinator *qwenNativeCoordinator) InspectSession(_ context.Context, sessionID string) (qwenDaemonSession, error) {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	actor := coordinator.actors[coordinator.sessions[sessionID]]
	coordinator.mu.Unlock()
	if actor == nil {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	return actor.inspect()
}

// WriteInput appends one complete AgentFrame carrier through the exact admitted descriptor.
func (coordinator *qwenNativeCoordinator) WriteInput(_ context.Context, path string, frame federation.AgentFrame) error {
	coordinator.ensureRecovered()
	coordinator.mu.Lock()
	var actor *qwenDaemonActor
	for _, candidate := range coordinator.actors {
		if candidate.inputPath == path {
			actor = candidate
			break
		}
	}
	coordinator.mu.Unlock()
	if actor == nil {
		return daemonpkg.ErrAttachmentNotFound
	}
	return actor.writeFrame(frame)
}

func (coordinator *qwenNativeCoordinator) resolveLaunchSession(intent daemonpkg.InteractiveLaunchIntent, profile string) (string, error) {
	selector := strings.TrimSpace(intent.Selector)
	if exactLaunchThreadIDRE.MatchString(selector) {
		return selector, nil
	}
	if intent.Mode != "resume" || selector == "" {
		return "", errors.New("qwen launch requires an exact daemon-selected session id")
	}
	return resolveHistoricalQwenName(selector, profile)
}

func (coordinator *qwenNativeCoordinator) ensureRecovered() {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.recovered {
		return
	}
	coordinator.recovered = true
	coordinator.recoverActorsLocked()
}

func (coordinator *qwenNativeCoordinator) recoverActorsLocked() {
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return
	}
	directory := filepath.Join(paths.StateRoot, "qwen", "actors")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		actor := recoverQwenDaemonActor(filepath.Join(directory, entry.Name()))
		if actor == nil {
			continue
		}
		coordinator.actors[actor.coordinatorID] = actor
		coordinator.sessions[actor.sessionID] = actor.coordinatorID
		if actor.ownerPID > 1 {
			go actor.watchOwner()
		}
	}
}

func (coordinator *qwenNativeCoordinator) close() {
	coordinator.mu.Lock()
	actors := coordinator.actors
	coordinator.actors = make(map[string]*qwenDaemonActor)
	coordinator.sessions = make(map[string]string)
	coordinator.mu.Unlock()
	for _, actor := range actors {
		actor.suspend()
	}
}

func newQwenDaemonActor(
	request daemonpkg.AttachmentPrepareRequest,
	profile qwenprofile.Identity,
	version, sessionID string,
) (*qwenDaemonActor, error) {
	coordinatorID, err := randomQwenCoordinatorID()
	if err != nil {
		return nil, err
	}
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return nil, err
	}
	actorRoot := filepath.Join(paths.StateRoot, "qwen", "runtime", coordinatorID)
	return &qwenDaemonActor{
		coordinatorID: coordinatorID, sessionID: sessionID, name: request.Name,
		cwd: request.Cwd, profile: profile, version: version, actorRoot: actorRoot,
		status: "idle", permissionMode: request.PermissionMode,
		inputPath: filepath.Join(actorRoot, "input.jsonl"), eventPath: filepath.Join(actorRoot, "events.jsonl"),
		recordPath: filepath.Join(paths.StateRoot, "qwen", "actors", coordinatorID+".json"),
	}, nil
}

func (actor *qwenDaemonActor) prepare() error {
	if err := os.MkdirAll(filepath.Dir(actor.actorRoot), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(actor.actorRoot, 0o700); err != nil {
		return err
	}
	for index, path := range []string{actor.inputPath, actor.eventPath} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // exact daemon-owned protocol artifact.
		if err != nil {
			return err
		}
		closeErr := file.Close()
		attestation, err := federator.QwenArtifactAttestationForPath(path)
		if err != nil {
			return err
		}
		if index == 0 {
			actor.inputIdentity = attestation
		} else {
			actor.eventIdentity = attestation
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return actor.persist(true)
}

func (actor *qwenDaemonActor) rollbackPreparation() {
	if federator.QwenArtifactIdentityMatches(actor.inputIdentity) {
		_ = os.Remove(actor.inputPath)
	}
	if federator.QwenArtifactIdentityMatches(actor.eventIdentity) {
		_ = os.Remove(actor.eventPath)
	}
	removeJSONIf(actor.recordPath, func(row map[string]any) bool {
		return stringValue(row["coordinator_id"]) == actor.coordinatorID && intValue(row["owner_pid"]) == 0
	})
	_ = os.Remove(actor.actorRoot)
}

func (actor *qwenDaemonActor) launchPlan(request daemonpkg.AttachmentPrepareRequest, executable string) daemonpkg.NativeLaunchPlan {
	arguments := insertQwenDaemonArguments(request.Intent.NativeArguments,
		"--chat-recording=true", "--input-file", actor.inputPath, "--json-file", actor.eventPath,
	)
	if request.Intent.Mode == "fresh" {
		arguments = insertQwenDaemonArguments(arguments, "--session-id", actor.sessionID)
	} else {
		arguments = replaceQwenDaemonResumeTarget(arguments, actor.sessionID)
	}
	expected := map[string]any{
		"coordinator_id": actor.coordinatorID, "event_path": actor.eventPath,
		"input_path": actor.inputPath, "readiness_path": actor.recordPath,
	}
	return daemonpkg.NativeLaunchPlan{
		Executable: executable, Arguments: arguments, Environment: qwenDaemonEnvironment(actor.profile),
		SessionID: "", Cwd: actor.cwd, ExpectedNativeActor: expected,
	}
}

func (actor *qwenDaemonActor) observe(ctx context.Context, connectorPID int) (qwenDaemonSession, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.retired {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	ownerPID, ownerStart, parentPID, err := observedQwenOwner(connectorPID)
	if err != nil {
		return qwenDaemonSession{}, err
	}
	expected := qwenAdmissionExpectation{
		SessionID: actor.sessionID, Cwd: actor.cwd, Version: actor.version,
		ProtocolVersion: 2, RequiredEvents: qwenRequiredDualOutputEvents(),
	}
	cursor, _, err := waitForQwenDaemonAdmission(ctx, actor.eventPath, expected)
	if err != nil {
		return qwenDaemonSession{}, err
	}
	input, err := openQwenInputWriter(actor.inputPath)
	if err != nil {
		return qwenDaemonSession{}, err
	}
	actor.ownerPID, actor.ownerProcStart, actor.parentPID = ownerPID, ownerStart, parentPID
	actor.cursor, actor.input, actor.ready, actor.suspended = cursor, input, true, false
	if err := actor.persist(true); err != nil {
		_ = input.Close()
		actor.input = nil
		return qwenDaemonSession{}, err
	}
	actor.startEventLoopLocked()
	return actor.sessionLocked(), nil
}

func (actor *qwenDaemonActor) inspect() (qwenDaemonSession, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.retired || !actor.ready || actor.suspended || actor.input == nil || actor.cursor == nil {
		return qwenDaemonSession{}, ErrQwenReadinessUnavailable
	}
	if exactProcessIdentityStatus(actor.ownerPID, actor.ownerProcStart).Status != processIdentityMatches ||
		!federator.QwenArtifactIdentityMatches(actor.inputIdentity) ||
		!federator.QwenArtifactIdentityMatches(actor.eventIdentity) {
		return qwenDaemonSession{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return actor.sessionLocked(), nil
}

func (actor *qwenDaemonActor) writeFrame(frame federation.AgentFrame) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.retired || !actor.ready || actor.input == nil {
		return ErrQwenReadinessUnavailable
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return actor.input.Submit(federation.AgentFrameCarrierPrefix + string(body))
}

func (actor *qwenDaemonActor) sessionLocked() qwenDaemonSession {
	return qwenDaemonSession{
		SessionID: actor.sessionID, Name: actor.name, Cwd: actor.cwd, Profile: actor.profile.Fingerprint,
		Status: actor.status, PermissionMode: actor.permissionMode,
		PID: actor.ownerPID, ProcStart: actor.ownerProcStart, ParentPID: actor.parentPID,
		EventPath: actor.eventPath, InputPath: actor.inputPath, ReadinessPath: actor.recordPath,
		Ready: actor.ready && !actor.suspended, DualOutput: actor.cursor != nil && actor.input != nil,
		CoordinatorID: actor.coordinatorID,
	}
}

func (actor *qwenDaemonActor) startEventLoopLocked() {
	if actor.eventCancel != nil {
		actor.eventCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	actor.eventCancel = cancel
	go actor.eventLoop(ctx)
}

func (actor *qwenDaemonActor) eventLoop(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			actor.mu.Lock()
			if err := actor.observeAvailableEventsLocked(); err != nil {
				actor.ready = false
				_ = actor.persist(true)
				actor.mu.Unlock()
				return
			}
			actor.mu.Unlock()
		}
	}
}

func (actor *qwenDaemonActor) observeAvailableEventsLocked() error {
	if actor.cursor == nil {
		return nil
	}
	events, err := actor.cursor.ReadAvailable()
	if err != nil {
		return err
	}
	changed := false
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		status := ""
		mode := ""
		switch stringValue(event["type"]) {
		case "user", "assistant", "stream_event":
			status = "busy"
		case "control_request":
			status = "waiting"
		case "control_response", "result":
			status = "idle"
		case "current_mode_update", "approval_mode_changed":
			mode = defaultString(stringValue(event["current_mode_id"]), stringValue(event["currentModeId"]))
		}
		if status != "" && status != actor.status {
			actor.status = status
			changed = true
		}
		if mode != "" && mode != actor.permissionMode {
			actor.permissionMode = mode
			changed = true
		}
	}
	if changed {
		return actor.persist(true)
	}
	return nil
}

func (actor *qwenDaemonActor) watchOwner() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		actor.mu.Lock()
		dead := actor.retired || (actor.ownerPID > 1 && exactProcessIdentityStatus(actor.ownerPID, actor.ownerProcStart).Status == processIdentityStale)
		actor.mu.Unlock()
		if dead {
			actor.retire()
			return
		}
	}
}

func (actor *qwenDaemonActor) suspend() {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.retired || actor.suspended {
		return
	}
	actor.suspended = true
	if actor.eventCancel != nil {
		actor.eventCancel()
		actor.eventCancel = nil
	}
	if actor.input != nil {
		_ = actor.input.Close()
		actor.input = nil
	}
}

func (actor *qwenDaemonActor) retire() {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.retired {
		return
	}
	actor.retired, actor.ready, actor.suspended = true, false, true
	if actor.eventCancel != nil {
		actor.eventCancel()
		actor.eventCancel = nil
	}
	if actor.input != nil {
		_ = actor.input.Close()
		actor.input = nil
	}
	_ = actor.persist(false)
	if federator.QwenArtifactIdentityMatches(actor.inputIdentity) {
		_ = os.Remove(actor.inputPath)
	}
	if federator.QwenArtifactIdentityMatches(actor.eventIdentity) {
		_ = os.Remove(actor.eventPath)
	}
	_ = os.Remove(actor.actorRoot)
}

func (actor *qwenDaemonActor) persist(active bool) error {
	record := qwenDaemonActorRecord{
		CoordinatorID: actor.coordinatorID, SessionID: actor.sessionID, Name: actor.name,
		Cwd: actor.cwd, Profile: actor.profile, Version: actor.version, ActorRoot: actor.actorRoot,
		Input: actor.inputIdentity, Events: actor.eventIdentity, OwnerPID: actor.ownerPID,
		OwnerStart: actor.ownerProcStart, ParentPID: actor.parentPID, Status: actor.status,
		PermissionMode: actor.permissionMode, Ready: actor.ready,
		Active: active, UpdatedAt: time.Now().UnixMilli(),
	}
	return writeJSONAtomic(actor.recordPath, record)
}

func recoverQwenDaemonActor(path string) *qwenDaemonActor {
	record, ok := readQwenDaemonActorRecord(path)
	if !ok || !record.Active || !validSessionID(record.SessionID) || record.CoordinatorID == "" ||
		!filepath.IsAbs(record.Cwd) || !filepath.IsAbs(record.ActorRoot) ||
		!federator.QwenArtifactIdentityMatches(record.Input) || !federator.QwenArtifactIdentityMatches(record.Events) {
		return nil
	}
	actor := &qwenDaemonActor{
		coordinatorID: record.CoordinatorID, sessionID: record.SessionID, name: record.Name,
		cwd: record.Cwd, profile: record.Profile, version: record.Version, actorRoot: record.ActorRoot,
		inputPath: record.Input.Path, eventPath: record.Events.Path, recordPath: path,
		inputIdentity: record.Input, eventIdentity: record.Events,
		ownerPID: record.OwnerPID, ownerProcStart: record.OwnerStart, parentPID: record.ParentPID,
		status: defaultString(record.Status, "idle"), permissionMode: record.PermissionMode,
	}
	if record.OwnerPID <= 1 {
		return actor
	}
	if exactProcessIdentityStatus(record.OwnerPID, record.OwnerStart).Status != processIdentityMatches {
		return nil
	}
	expected := qwenAdmissionExpectation{
		SessionID: record.SessionID, Cwd: record.Cwd, Version: record.Version,
		ProtocolVersion: 2, RequiredEvents: qwenRequiredDualOutputEvents(),
	}
	cursor, _, err := admitQwenDualOutput(record.Events.Path, expected)
	if err != nil {
		return nil
	}
	input, err := openQwenInputWriter(record.Input.Path)
	if err != nil {
		return nil
	}
	actor.cursor, actor.input, actor.ready = cursor, input, true
	actor.mu.Lock()
	actor.startEventLoopLocked()
	actor.mu.Unlock()
	return actor
}

func readQwenDaemonActorRecord(path string) (qwenDaemonActorRecord, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return qwenDaemonActorRecord{}, false
	}
	body, err := os.ReadFile(path) //nolint:gosec // bounded daemon-owned non-secret actor record.
	if err != nil {
		return qwenDaemonActorRecord{}, false
	}
	var record qwenDaemonActorRecord
	if json.Unmarshal(body, &record) != nil {
		return qwenDaemonActorRecord{}, false
	}
	return record, true
}

func resolveHistoricalQwenName(name, profile string) (string, error) {
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Join(paths.StateRoot, "qwen", "actors"))
	if err != nil {
		return "", daemonpkg.ErrAttachmentNotFound
	}
	matches := map[string]bool{}
	for _, entry := range entries {
		record, ok := readQwenDaemonActorRecord(filepath.Join(paths.StateRoot, "qwen", "actors", entry.Name()))
		if ok && record.Name == name && record.Profile.Fingerprint == profile {
			matches[record.SessionID] = true
		}
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			return "", daemonpkg.ErrAttachmentAmbiguous
		}
		return "", daemonpkg.ErrAttachmentNotFound
	}
	for sessionID := range matches {
		return sessionID, nil
	}
	return "", daemonpkg.ErrAttachmentNotFound
}

func waitForQwenDaemonAdmission(
	ctx context.Context,
	path string,
	expected qwenAdmissionExpectation,
) (*qwenEventCursor, qwenSessionStart, error) {
	timer := time.NewTimer(qwenDaemonAdmissionTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		cursor, start, err := admitQwenDualOutput(path, expected)
		if err == nil {
			return cursor, start, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, qwenSessionStart{}, ctx.Err()
		case <-timer.C:
			return nil, qwenSessionStart{}, fmt.Errorf("timed out admitting native Qwen session: %w", last)
		case <-ticker.C:
		}
	}
}

func observedQwenOwner(connectorPID int) (int, string, int, error) {
	pid := connectorPID
	for depth := 0; depth < 64 && pid > 1; depth++ {
		arguments, err := procinfo.Args(pid)
		if err == nil && looksLikeNativeQwen(arguments) {
			identity := procinfo.Read(pid)
			if identity.Status == procinfo.Known && identity.Start != "" {
				return pid, identity.Start, identity.Parent, nil
			}
			return 0, "", 0, daemonpkg.ErrAttachmentEvidenceChanged
		}
		identity := procinfo.Read(pid)
		if identity.Status != procinfo.Known || identity.Parent <= 1 || identity.Parent == pid {
			break
		}
		pid = identity.Parent
	}
	return 0, "", 0, daemonpkg.ErrAttachmentSelecting
}

func looksLikeNativeQwen(arguments []string) bool {
	for _, argument := range arguments {
		name := strings.ToLower(filepath.Base(argument))
		if name == "qwen" || name == "qwen.exe" || strings.Contains(name, "qwen-code") {
			return true
		}
	}
	return false
}

func qwenDaemonProfile(identity map[string]any) (qwenprofile.Identity, error) {
	profile := qwenprofile.Identity{
		QwenHomeSet: boolValue(identity["qwen_home_set"]), QwenHome: stringValue(identity["qwen_home"]),
		QwenRuntimeSet: boolValue(identity["qwen_runtime_dir_set"]), QwenRuntimeDir: stringValue(identity["qwen_runtime_dir"]),
		Fingerprint: stringValue(identity["profile"]),
	}
	if profile.Fingerprint == "" || (profile.QwenHomeSet && !filepath.IsAbs(profile.QwenHome)) ||
		(profile.QwenRuntimeSet && !filepath.IsAbs(profile.QwenRuntimeDir)) {
		return qwenprofile.Identity{}, errors.New("qwen daemon profile identity is incomplete")
	}
	return profile, nil
}

func qwenDaemonExecutable() (string, error) {
	configured := strings.TrimSpace(os.Getenv("QWEN_PEER_QWEN_BIN"))
	if configured == "" {
		configured = "qwen"
	}
	executable, err := exec.LookPath(configured)
	if err != nil {
		return "", fmt.Errorf("resolve Qwen executable %q: %w", configured, err)
	}
	return executable, nil
}

func qwenDaemonEnvironment(profile qwenprofile.Identity) map[string]string {
	environment := map[string]string{}
	if profile.QwenHomeSet {
		environment["QWEN_HOME"] = profile.QwenHome
	}
	if profile.QwenRuntimeSet {
		environment["QWEN_RUNTIME_DIR"] = profile.QwenRuntimeDir
	}
	return environment
}

func qwenDaemonReadinessError(report qwenreadiness.Report) error {
	issues := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		issues = append(issues, issue.Code+": "+issue.Message)
	}
	return fmt.Errorf("qwen readiness is not satisfied: %s", strings.Join(issues, "; "))
}

func insertQwenDaemonArguments(arguments []string, managed ...string) []string {
	for index, argument := range arguments {
		if argument == "--" {
			result := append([]string(nil), arguments[:index]...)
			result = append(result, managed...)
			return append(result, arguments[index:]...)
		}
	}
	return append(append([]string(nil), managed...), arguments...)
}

func replaceQwenDaemonResumeTarget(arguments []string, sessionID string) []string {
	result := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--resume" || argument == "-r":
			result = append(result, "--resume", sessionID)
			if index+1 < len(arguments) {
				index++
			}
		case strings.HasPrefix(argument, "--resume=") || strings.HasPrefix(argument, "-r="):
			result = append(result, "--resume", sessionID)
		default:
			result = append(result, argument)
		}
	}
	return result
}

func randomQwenCoordinatorID() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
