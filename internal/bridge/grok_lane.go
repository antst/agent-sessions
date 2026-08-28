package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	grokLaneContractVersion     = 1
	grokLaneMCPReadyTimeout     = 35 * time.Second
	grokLaneManagerReadyTimeout = grokACPStartupTimeout + grokLaneMCPReadyTimeout + 10*time.Second
)

type grokLaneOptions struct {
	laneCommonOptions
	model              string
	modelSet           bool
	reasoningEffort    string
	reasoningEffortSet bool
	permissionMode     string
	permissionModeSet  bool
}

type grokLaneTurn struct {
	ID          string `json:"id"`
	Prompt      string `json:"prompt"`
	MessageID   string `json:"messageId,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Status      string `json:"status"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Exit        int    `json:"exit"`
	CreatedAt   int64  `json:"createdAt"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	CompletedAt int64  `json:"completedAt,omitempty"`
	TimeoutMS   int64  `json:"timeoutMs,omitempty"`
	Collected   bool   `json:"collected,omitempty"`
}

type grokLaneState struct {
	Type                  string             `json:"type"`
	Name                  string             `json:"name"`
	SessionID             string             `json:"sessionId"`
	GrokSessionID         string             `json:"grokSessionId,omitempty"`
	Cwd                   string             `json:"cwd"`
	Status                string             `json:"status"`
	ManagerPID            int                `json:"managerPid,omitempty"`
	ManagerProcStart      string             `json:"managerProcStart,omitempty"`
	ManagerStrongStart    string             `json:"managerStrongStart,omitempty"`
	ControlSocket         string             `json:"controlSocket,omitempty"`
	ManagerLog            string             `json:"managerLog,omitempty"`
	WorkerPID             int                `json:"workerPid,omitempty"`
	WorkerProcStart       string             `json:"workerProcStart,omitempty"`
	WorkerStrongStart     string             `json:"workerStrongStart,omitempty"`
	WorkerSessionID       int                `json:"workerSessionId,omitempty"`
	ToolRegistryVersion   int                `json:"toolRegistryVersion,omitempty"`
	ToolShellName         string             `json:"toolShellName,omitempty"`
	ToolRealShell         string             `json:"toolRealShell,omitempty"`
	MessagingSocket       string             `json:"messagingSocket,omitempty"`
	LaunchTokenHash       string             `json:"launchTokenHash,omitempty"`
	RuntimeDir            string             `json:"runtimeDir,omitempty"`
	OwnerPID              int                `json:"ownerPid,omitempty"`
	OwnerProcStart        string             `json:"ownerProcStart,omitempty"`
	OwnerSessionID        string             `json:"ownerSessionId,omitempty"`
	NotifyTarget          string             `json:"notifyTarget,omitempty"`
	Persistent            bool               `json:"persistent,omitempty"`
	AutoArchive           bool               `json:"autoArchive,omitempty"`
	AutoArchiveDelayMS    int64              `json:"autoArchiveDelayMs,omitempty"`
	AutoArchiveAt         int64              `json:"autoArchiveAt,omitempty"`
	PermissionMode        string             `json:"permissionMode"`
	Model                 string             `json:"model,omitempty"`
	ReasoningEffort       string             `json:"reasoningEffort,omitempty"`
	SessionCreated        bool               `json:"sessionCreated,omitempty"`
	StartupID             string             `json:"startupId,omitempty"`
	Turns                 []grokLaneTurn     `json:"turns,omitempty"`
	TurnID                string             `json:"turnId,omitempty"`
	LatestTurnID          string             `json:"latestTurnId,omitempty"`
	CollectedTurnID       string             `json:"collectedTurnId,omitempty"`
	Notices               []claudeLaneNotice `json:"notices,omitempty"`
	ArchiveDroppedNotices int                `json:"archiveDroppedNotices,omitempty"`
	Groups                []string           `json:"groups,omitempty"`
	ExplicitGroups        []string           `json:"explicitGroups,omitempty"`
	ParentSessionID       string             `json:"parentSessionId,omitempty"`
	ParentHostID          string             `json:"parentHostId,omitempty"`
	ParentAgentRuntimeDir string             `json:"parentAgentRuntimeDir,omitempty"`
	InheritParentGroups   bool               `json:"inheritParentGroups,omitempty"`
	CreatedAt             int64              `json:"createdAt"`
	UpdatedAt             int64              `json:"updatedAt"`
}

func grokLaneUsage() string {
	return `grok-peer-lane — named, messageable Grok Build lanes

Usage:
  grok-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  grok-peer-lane status SESSION_OR_NAME
  grok-peer-lane interrupt SESSION_OR_NAME
  grok-peer-lane archive SESSION_OR_NAME
  grok-peer-lane list [--all] [--mine]
  grok-peer-lane doctor [--json]

Options:
  -n, --name NAME
  -C, --cd DIR
  -m, --model MODEL
      --reasoning-effort LEVEL
      --permission-mode bypassPermissions
      --timeout SECONDS
	  --notify PEER            persistent lanes: send terminal pointers here
	  --no-notify              suppress owner terminal pointers
      --persistent
      --auto-archive-after SECONDS
      --no-auto-archive
      --prompt-file FILE
	  -g, --group GROUP       add a child group; repeatable
	  --inherit-groups        also inherit the parent's non-private groups
	  --no-inherit-groups     retain only the mandatory parent anchor
      --all
      --mine

Headless Grok Build lanes use explicit always-approve mode. They own separate
ACP sessions and never attach to an interactive grok-peer conversation.
`
}

func parseGrokLaneArgs(argv []string) (grokLaneOptions, error) {
	o := grokLaneOptions{
		laneCommonOptions: newLaneCommonOptions("SESSION_OR_NAME"), permissionMode: "bypassPermissions",
	}
	start, done, err := beginLaneOptionParse(argv, &o.laneCommonOptions)
	if done || err != nil {
		return o, err
	}
	parser := newLaneFlagParser("grok-peer-lane", &o.laneCommonOptions)
	parser.set.StringVarP(&o.model, "model", "m", o.model, "model")
	parser.set.StringVar(&o.reasoningEffort, "reasoning-effort", o.reasoningEffort, "reasoning effort")
	parser.set.StringVar(&o.reasoningEffort, "effort", o.reasoningEffort, "reasoning effort alias")
	parser.set.StringVar(&o.permissionMode, "permission-mode", o.permissionMode, "permission mode")
	parser.set.StringVar(&o.permissionMode, "always-approve", o.permissionMode, "always approve")
	parser.set.Lookup("always-approve").NoOptDefVal = "bypassPermissions"
	parser.set.StringVar(&o.permissionMode, "yolo", o.permissionMode, "always approve alias")
	parser.set.Lookup("yolo").NoOptDefVal = "bypassPermissions"
	positionals, err := parser.parse(argv[start:])
	if err != nil {
		return o, err
	}
	o.modelSet = parser.set.Changed("model")
	o.reasoningEffortSet = parser.set.Changed("reasoning-effort") || parser.set.Changed("effort")
	o.permissionModeSet = parser.set.Changed("permission-mode") || parser.set.Changed("always-approve") || parser.set.Changed("yolo")
	if o.permissionMode != "bypassPermissions" {
		return o, fmt.Errorf("unsupported headless Grok permission mode %q; use bypassPermissions", o.permissionMode)
	}
	if err := validateGrokLaneCommandOptions(o); err != nil {
		return o, err
	}
	if err := validateLaneCommonOptions(&o.laneCommonOptions, positionals); err != nil {
		return o, err
	}
	return o, nil
}

func (o grokLaneOptions) hasLaunchOptions() bool {
	return o.nameSet || o.cwdSet || o.modelSet || o.reasoningEffortSet || o.permissionModeSet ||
		o.promptFile != "" || o.notifyExplicit || o.disableNotify || o.persistentSet || o.noAutoArchiveSet || o.autoArchiveCustom ||
		o.allowDuplicateName || o.stdinMarker
}

func validateGrokLaneCommandOptions(o grokLaneOptions) error {
	checks := []laneOptionCheck{}
	switch o.command {
	case "run", "start":
		checks = append(checks, laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	case "resume":
		checks = append(checks, laneOption("--name", o.nameSet), laneOption("--cd", o.cwdSet), laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	case "wait":
		checks = append(checks, laneOption("launch options", o.hasLaunchOptions()), laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	case "list":
		checks = append(checks, laneOption("launch options", o.hasLaunchOptions()), laneOption("--timeout", o.timeoutSet), laneOption("--json", o.json))
	case "doctor":
		checks = append(checks, laneOption("launch options", o.hasLaunchOptions()), laneOption("--timeout", o.timeoutSet), laneOption("--all", o.all), laneOption("--mine", o.mine))
	default:
		checks = append(checks, laneOption("launch options", o.hasLaunchOptions()), laneOption("--timeout", o.timeoutSet), laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	}
	return validateLaneCommandOptions(o.command, checks)
}

func grokLaneStatePath(paths nativePaths, sessionID string) string {
	return filepath.Join(profileDataRoot(paths), "grok-lanes", sessionKey(sessionID)+".json")
}

func readGrokLaneState(paths nativePaths, sessionID string) (grokLaneState, error) {
	body, err := os.ReadFile(grokLaneStatePath(paths, sessionID))
	if err != nil {
		return grokLaneState{}, err
	}
	var state grokLaneState
	if json.Unmarshal(body, &state) != nil || state.Type != "grok-peer-lane" || state.SessionID != sessionID {
		return grokLaneState{}, errors.New("grok lane state/session mismatch")
	}
	return state, nil
}

func writeGrokLaneState(paths nativePaths, state grokLaneState) error {
	lock, err := lockLaneStateFile(paths, "grok-"+state.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	return writeGrokLaneStateUnlocked(paths, state)
}

func writeGrokLaneStateUnlocked(paths nativePaths, state grokLaneState) error {
	state.UpdatedAt = time.Now().UnixMilli()
	return writeJSONAtomic(grokLaneStatePath(paths, state.SessionID), state)
}

func readGrokLaneStates(paths nativePaths) []grokLaneState {
	directory := filepath.Join(profileDataRoot(paths), "grok-lanes")
	return readProductLaneStates(directory, func(entryName string, state *grokLaneState) bool {
		return state.Type == "grok-peer-lane" && entryName == sessionKey(state.SessionID)+".json"
	}, func(state *grokLaneState) int64 { return state.CreatedAt })
}

func resolveGrokLaneState(paths nativePaths, target string) (grokLaneState, error) {
	return resolveProductLaneState(
		target, readGrokLaneStates(paths),
		func(state *grokLaneState, candidate string) bool { return state.SessionID == candidate },
		func(state *grokLaneState) string { return state.Name },
		func(state *grokLaneState) string { return state.Status },
		"Grok", "session ID",
	)
}

func newGrokLaneTurn(prompt string) grokLaneTurn {
	now := time.Now().UnixMilli()
	return grokLaneTurn{ID: randomID(), Prompt: prompt, Status: "queued", CreatedAt: now}
}

func canonicalGrokLaneDirectory(value string) (string, error) {
	cwd, err := canonicalLaunchDirectory(value)
	if err != nil {
		return "", fmt.Errorf("resolve Grok lane cwd: %w", err)
	}
	return cwd, nil
}

func grokLaneManagerEnvironment(environment []string, launchToken, sessionID string) []string {
	blocked := map[string]bool{grokLaunchTokenEnv: true, grokSessionIDEnv: true}
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if !blocked[name] {
			result = append(result, entry)
		}
	}
	return append(result, grokLaunchTokenEnv+"="+launchToken, grokSessionIDEnv+"="+sessionID)
}

func waitGrokLaneReady(paths nativePaths, sessionID string, managerPID int, managerProcStart string, timeout time.Duration) (grokLaneState, error) {
	return waitProductLaneReady(
		managerPID, managerProcStart, timeout,
		func() (grokLaneState, error) { return readGrokLaneState(paths, sessionID) },
		func(state *grokLaneState) bool {
			return state.ManagerPID == managerPID && state.Status != "starting" && state.MessagingSocket != "" &&
				probeUnixSocket(state.MessagingSocket, 200*time.Millisecond)
		},
		"grok lane manager exited during startup; inspect its private manager log",
		"timed out starting Grok lane manager; inspect its private manager log",
	)
}

func reconcileGrokLaneManagers(paths nativePaths) error {
	now := time.Now().UnixMilli()
	for _, state := range readGrokLaneStates(paths) {
		if err := reconcileGrokLaneManager(paths, state, now); err != nil {
			return fmt.Errorf("reconcile Grok lane %s: %w", state.SessionID, err)
		}
	}
	return nil
}

//nolint:gocyclo // Reconciliation enumerates mutually exclusive durable lifecycle residue states.
func reconcileGrokLaneManager(paths nativePaths, state grokLaneState, now int64) error {
	managerIdentity := cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart)
	managerUnavailable := state.ManagerPID <= 1 || managerIdentity.Status == processIdentityStale
	if managerUnavailable && grokLaneHasUnsentNotices(state) {
		flushOrphanGrokLaneNotices(paths, state.SessionID)
		if latest, err := readGrokLaneState(paths, state.SessionID); err == nil {
			state = latest
		}
	}
	startingOrphan := state.Status == "starting" && state.ManagerPID <= 1 && state.UpdatedAt+20_000 <= now
	managerExited := state.ManagerPID > 1 && managerIdentity.Status == processIdentityStale
	missingManager := state.Status != "starting" && state.Status != "archived" && state.ManagerPID <= 1
	// The manager persists archived before it tears down its worker and clears
	// durable ownership. That is an in-progress shutdown, not a crashed lane.
	// Reconciliation may take over only after the exact manager identity is
	// absent or stale; killing a matching/unknown manager here can interrupt its
	// cleanup transaction and strand the very residue we are trying to remove.
	archivedResidue := state.Status == "archived" && managerUnavailable && grokLaneHasOwnedResidue(paths, state)
	archivedUnnormalized := state.Status == "archived" && managerUnavailable && !grokLaneStateNormalized(state)
	if startingOrphan || managerExited || missingManager || archivedResidue || archivedUnnormalized {
		return forceArchiveGrokLane(paths, state.SessionID, "Grok lane manager exited")
	}
	return nil
}

func grokLaneHasOwnedResidue(paths nativePaths, state grokLaneState) bool {
	registryGuard, cleanupRoots, registryErr := grokLaneCleanupRoots(state, false)
	if registryGuard != nil {
		defer registryGuard.close()
	}
	if registryErr != nil {
		return true
	}
	if processIdentityMayBeLive(state.ManagerPID, state.ManagerProcStart) ||
		processIdentityMayBeLive(state.WorkerPID, state.WorkerProcStart) ||
		grokProcessSessionHasMembers(state.WorkerSessionID, 0) ||
		grokTaggedProcessesRemain(state.LaunchTokenHash, 0, cleanupRoots...) ||
		state.ControlSocket != "" || state.MessagingSocket != "" {
		return true
	}
	if grokLanePathMayExist(grokLaunchRecordPath(paths, state.SessionID)) {
		return true
	}
	runtimeDir := defaultString(state.RuntimeDir, paths.runtimeDir)
	runtimeRoot := bridgeRuntimeRoot(runtimeDir, os.Getuid())
	stable := filepath.Join(runtimeRoot, "session-"+sessionKey(state.SessionID)+".sock")
	if grokLanePathMayExist(stable) {
		return true
	}
	if grokLanePathMayExist(filepath.Join(paths.dataRoot, "sessions", sessionKey(state.SessionID))) {
		return true
	}
	if len(state.LaunchTokenHash) == 64 {
		hostPaths := grokRuntimePathsForKey(runtimeDir, os.Getuid(), state.LaunchTokenHash[:20])
		if grokLanePathMayExist(hostPaths.LaunchDir) {
			return true
		}
	}
	return grokLaneRegistryResidue(paths, state.SessionID)
}

func grokLanePathMayExist(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !os.IsNotExist(err)
}

func grokLaneRegistryResidue(paths nativePaths, sessionID string) bool {
	entries, _ := os.ReadDir(filepath.Join(paths.claudeRoot, "sessions"))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		row := readJSONMap(filepath.Join(paths.claudeRoot, "sessions", entry.Name()))
		if stringValue(row["sessionId"]) == sessionID && stringValue(row["entrypoint"]) == "grok" {
			return true
		}
	}
	return false
}

func firstGrokLaneDebt(state grokLaneState) string {
	for _, turn := range state.Turns {
		if !turn.Collected {
			return turn.ID
		}
	}
	return ""
}

//nolint:gocyclo // Collection interleaves terminal emission, acknowledgement, crash reconciliation, and timeout.
func waitGrokLane(o grokLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveGrokLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	lock, err := lockLaneStateFile(paths, "grok-collect-"+state.SessionID)
	if err != nil {
		return 1, err
	}
	defer unlockLaneStateFile(lock)
	deadline := time.Time{}
	if o.timeout > 0 {
		deadline = time.Now().Add(o.timeout)
	}
	for {
		state, err = readGrokLaneState(paths, state.SessionID)
		if err != nil {
			return 1, err
		}
		for _, turn := range state.Turns {
			if turn.Collected || !containsString([]string{"completed", "failed", "interrupted", "timed_out"}, turn.Status) {
				continue
			}
			if err := emitGrokLaneTurn(state, turn); err != nil {
				return 1, err
			}
			if err := acknowledgeGrokLaneTurn(paths, state.SessionID, turn.ID); err != nil {
				return 1, err
			}
			return turn.Exit, nil
		}
		if state.Status == "archived" {
			return 1, fmt.Errorf("grok lane %s is archived and has no collectable turn", state.SessionID)
		}
		if state.ManagerPID <= 1 || cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart).Status == processIdentityStale {
			if reconcileErr := reconcileGrokLaneManager(paths, state, time.Now().UnixMilli()); reconcileErr != nil {
				return 1, reconcileErr
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return 124, context.DeadlineExceeded
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func emitGrokLaneTurn(state grokLaneState, turn grokLaneTurn) error {
	if err := emitLane(map[string]any{
		"type": "item.completed", "thread_id": state.SessionID, "turn_id": turn.ID,
		"item": map[string]any{"id": "user-" + first8(turn.ID), "type": "user_message", "text": turn.Prompt},
	}); err != nil {
		return err
	}
	if turn.Result != "" {
		if err := emitLane(map[string]any{
			"type": "item.completed", "thread_id": state.SessionID, "turn_id": turn.ID,
			"item": map[string]any{"id": "answer-" + first8(turn.ID), "type": "agent_message", "phase": "final_answer", "text": turn.Result},
		}); err != nil {
			return err
		}
	}
	accounting := map[string]any{"started_at": nilIfZero(turn.StartedAt), "completed_at": nilIfZero(turn.CompletedAt), "duration_ms": nil}
	if turn.StartedAt > 0 && turn.CompletedAt >= turn.StartedAt {
		accounting["duration_ms"] = turn.CompletedAt - turn.StartedAt
	}
	return emitLane(map[string]any{
		"type": "turn.completed", "thread_id": state.SessionID, "turn_id": turn.ID,
		"status": turn.Status, "outcome": turn.Outcome, "exit": turn.Exit,
		"error": emptyStringAsNil(turn.Error), "accounting": accounting,
	})
}

//nolint:gocyclo // ACK must prove live-manager authority before its guarded disk fallback.
func acknowledgeGrokLaneTurn(paths nativePaths, sessionID, turnID string) error {
	state, err := readGrokLaneState(paths, sessionID)
	if err != nil {
		return err
	}
	if exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) {
		controlErr := errors.New("grok lane manager control socket is unavailable")
		if state.ControlSocket != "" {
			_, controlErr = requestControl(state.ControlSocket, map[string]any{
				"action": "ack", "sessionId": sessionID, "turnId": turnID,
			}, 5*time.Second)
		}
		if controlErr == nil {
			return nil
		}
		latest, readErr := readGrokLaneState(paths, sessionID)
		if readErr != nil {
			return fmt.Errorf("acknowledge live Grok lane turn: %w", controlErr)
		}
		for _, turn := range latest.Turns {
			if turn.ID == turnID && turn.Collected {
				return nil
			}
		}
		if exactProcessIdentityMatch(latest.ManagerPID, latest.ManagerProcStart) {
			return fmt.Errorf("acknowledge live Grok lane turn: %w", controlErr)
		}
	}

	lock, err := lockLaneStateFile(paths, "grok-"+sessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	state, err = readGrokLaneState(paths, sessionID)
	if err != nil {
		return err
	}
	found := false
	for index := range state.Turns {
		if state.Turns[index].ID == turnID {
			state.Turns[index].Collected = true
			state.CollectedTurnID = turnID
			for noticeIndex := range state.Notices {
				if state.Notices[noticeIndex].TurnID == turnID && state.Notices[noticeIndex].SentAt == 0 {
					state.Notices[noticeIndex].SentAt = time.Now().UnixMilli()
				}
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("grok lane turn %s was not found", turnID)
	}
	state.TurnID = firstGrokLaneDebt(state)
	return writeGrokLaneStateUnlocked(paths, state)
}

func grokLaneStatusEvent(state grokLaneState) map[string]any {
	var turnStatus, outcome, exit any
	if turn := grokLaneReportedTurn(state); turn != nil {
		turnStatus = emptyStringAsNil(turn.Status)
		if turn.Outcome != "" {
			outcome, exit = turn.Outcome, turn.Exit
		}
	}
	return map[string]any{
		"type": "lane.status", "product": "grok", "contract_version": grokLaneContractVersion,
		"name": state.Name, "thread_id": state.SessionID, "session_id": state.SessionID,
		"grok_session_id": emptyStringAsNil(state.GrokSessionID),
		"cwd":             state.Cwd, "status": state.Status, "turn_id": emptyStringAsNil(state.TurnID), "turn_status": turnStatus,
		"collected_turn_id": emptyStringAsNil(state.CollectedTurnID), "persistent": state.Persistent,
		"notify_target": emptyStringAsNil(state.NotifyTarget), "outcome": outcome, "exit": exit,
		"owner_session_id": emptyStringAsNil(state.OwnerSessionID), "auto_archive": state.AutoArchive,
		"auto_archive_after_seconds": float64(state.AutoArchiveDelayMS) / 1000,
		"auto_archive_at":            nilIfZero(state.AutoArchiveAt),
	}
}

func grokLaneReportedTurn(state grokLaneState) *grokLaneTurn {
	turnID := state.TurnID
	if turnID == "" {
		turnID = state.LatestTurnID
	}
	for index := range state.Turns {
		if state.Turns[index].ID == turnID {
			return &state.Turns[index]
		}
	}
	return nil
}

func waitGrokLaneArchived(paths nativePaths, sessionID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := readGrokLaneState(paths, sessionID)
		if err == nil && state.Status == "archived" && grokLaneCleanupComplete(paths, state) {
			return nil
		}
		if err == nil && state.Status == "archived" && !grokLaneStateNormalized(state) {
			if reconcileErr := reconcileGrokLaneManager(paths, state, time.Now().UnixMilli()); reconcileErr != nil {
				return reconcileErr
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("timed out waiting for Grok lane cleanup")
}

func grokLaneCleanupComplete(paths nativePaths, state grokLaneState) bool {
	if !grokLaneStateNormalized(state) {
		return false
	}
	if !processIdentityStoppedOrUnset(state.ManagerPID, state.ManagerProcStart) ||
		!processIdentityStoppedOrUnset(state.WorkerPID, state.WorkerProcStart) {
		return false
	}
	backend := ""
	requireBackendAbsent := false
	if state.ManagerPID > 1 {
		backend = filepath.Join(bridgeRuntimeRoot(defaultString(state.RuntimeDir, paths.runtimeDir), os.Getuid()), fmt.Sprintf("%d.sock", state.ManagerPID))
		requireBackendAbsent = observeProcessIdentity(state.ManagerPID, "").Status == processIdentityStale
	}
	return verifyGrokLaneCleanup(paths, state, requireBackendAbsent, backend) == nil
}

func grokLaneStateNormalized(state grokLaneState) bool {
	return state.ManagerPID == 0 && state.ManagerProcStart == "" && state.WorkerPID == 0 && state.WorkerProcStart == "" && state.WorkerStrongStart == "" && state.WorkerSessionID == 0 &&
		state.ControlSocket == "" && state.MessagingSocket == "" && state.StartupID == ""
}

//nolint:gocyclo // Forced archive is an ownership transaction with explicit notice, process, registry, and artifact stages.
func forceArchiveGrokLane(paths nativePaths, sessionID, reason string) error {
	observed, err := readGrokLaneState(paths, sessionID)
	if err != nil {
		return err
	}
	lock, err := acquireGrokLaneForForcedArchive(paths, observed)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lock)
	explicitArchive := strings.HasPrefix(reason, "explicit archive")
	if explicitArchive {
		noticeLock, noticeErr := lockGrokLaneNotices(paths, sessionID)
		if noticeErr != nil {
			return errors.New("grok lane terminal notice delivery is in progress; retry archive")
		}
		defer unlockLaneStateFile(noticeLock)
	}
	state, err := readGrokLaneState(paths, sessionID)
	if err != nil {
		return err
	}
	if exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) {
		return errors.New("refuse to force archive a replacement live Grok lane manager")
	}
	now := time.Now().UnixMilli()
	for index := range state.Turns {
		turn := &state.Turns[index]
		if containsString([]string{"queued", "active"}, turn.Status) {
			turn.Status, turn.Outcome, turn.Exit, turn.Error, turn.CompletedAt = "interrupted", "interrupted", 130, reason, now
			queueGrokLaneTerminalNotice(&state, *turn)
		}
	}
	state.ArchiveDroppedNotices = 0
	if explicitArchive {
		state.ArchiveDroppedNotices = cancelAllGrokLaneNotices(&state)
	}
	state.Status, state.AutoArchiveAt, state.StartupID = "archived", 0, ""
	if err := writeGrokLaneState(paths, state); err != nil {
		return err
	}
	if !explicitArchive {
		flushOrphanGrokLaneNotices(paths, state.SessionID)
	}
	registryGuard, cleanupRoots, err := grokLaneCleanupRoots(state, true)
	if err != nil {
		return err
	}
	defer registryGuard.close()
	if err := stopGrokTaggedProcesses(state.LaunchTokenHash, 0, cleanupRoots...); err != nil {
		return err
	}
	stopStaleGrokLaneWorker(grokLaneWorkerRoot(state))
	if err := stopGrokProcessSessionStrong(state.WorkerSessionID, state.WorkerProcStart, state.WorkerStrongStart, 0); err != nil {
		return err
	}
	if err := registryGuard.removeArtifacts(); err != nil {
		return err
	}
	if err := cleanupGrokLaneOwnedFiles(paths, state, 0, cleanupRoots...); err != nil {
		return err
	}
	state.ManagerPID, state.ManagerProcStart, state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart, state.WorkerSessionID = 0, "", 0, "", "", 0
	state.ControlSocket, state.MessagingSocket = "", ""
	return writeGrokLaneState(paths, state)
}

func acquireGrokLaneForForcedArchive(paths nativePaths, observed grokLaneState) (*os.File, error) {
	lock, acquired, err := tryLockLaneLifecycle(paths, "grok-"+observed.SessionID)
	if err != nil || acquired {
		return lock, err
	}
	stopExactGrokLaneManager(observed.ManagerPID, observed.ManagerProcStart)
	deadline := time.Now().Add(3 * time.Second)
	for {
		lock, acquired, err = tryLockLaneLifecycle(paths, "grok-"+observed.SessionID)
		if err != nil || acquired {
			return lock, err
		}
		latest, readErr := readGrokLaneState(paths, observed.SessionID)
		if readErr == nil && latest.ManagerPID > 1 &&
			(latest.ManagerPID != observed.ManagerPID || latest.ManagerProcStart != observed.ManagerProcStart) &&
			exactProcessIdentityMatch(latest.ManagerPID, latest.ManagerProcStart) {
			return nil, errors.New("grok lane changed managers during forced archive")
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out acquiring Grok lane lifecycle for forced archive")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func stopExactGrokLaneManager(pid int, procStart string) {
	if !exactProcessIdentityMatch(pid, procStart) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for exactProcessIdentityMatch(pid, procStart) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if exactProcessIdentityMatch(pid, procStart) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline = time.Now().Add(time.Second)
	for exactProcessIdentityMatch(pid, procStart) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
}

// cleanupGrokLaneOwnedFiles removes only artifacts whose lane/session/token
// ownership is corroborated. A recycled manager PID never authorizes deleting
// its numeric backend socket.
//
//nolint:gocyclo // Cleanup independently corroborates each process identity and owned artifact.
func cleanupGrokLaneOwnedFiles(paths nativePaths, state grokLaneState, currentManagerPID int, cleanupRoots ...grokSessionMember) error {
	if grokProcessSessionHasMembers(state.WorkerSessionID, currentManagerPID) {
		return errors.New("grok lane process session is still live")
	}
	if len(cleanupRoots) == 0 {
		cleanupRoots = append(cleanupRoots, grokLaneWorkerRoot(state))
	}
	if grokTaggedProcessesRemain(state.LaunchTokenHash, currentManagerPID, cleanupRoots...) {
		return errors.New("tagged Grok lane process is still live")
	}
	managerObservation := cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart)
	workerStatus := processIdentityStale
	if state.WorkerPID > 1 {
		workerStatus = grokProcessIdentityStatus(grokLaneWorkerRoot(state))
	}
	if workerStatus != processIdentityStale {
		return errors.New("cannot prove Grok lane worker cleanup")
	}
	safeBackendRemoval := state.ManagerPID <= 1
	if state.ManagerPID > 1 {
		rawManager := observeProcessIdentity(state.ManagerPID, "")
		switch {
		case currentManagerPID == state.ManagerPID && managerObservation.Status == processIdentityMatches:
			safeBackendRemoval = true
		case rawManager.Status == processIdentityStale:
			safeBackendRemoval = true
		case rawManager.Status == processIdentityUnknown:
			return errors.New("cannot prove Grok lane manager cleanup")
		case managerObservation.Status == processIdentityMatches:
			return errors.New("grok lane manager is still live")
		default:
			// The numeric PID was reused. The backend path may now belong to the
			// replacement process and must be preserved.
			safeBackendRemoval = false
		}
	}
	removeJSONIf(grokLaunchRecordPath(paths, state.SessionID), func(row map[string]any) bool {
		return stringValue(row["tokenHash"]) == state.LaunchTokenHash && stringValue(row["sessionId"]) == state.SessionID
	})
	if err := dropGrokLaneInbox(paths, state.SessionID); err != nil {
		return err
	}
	runtimeDir := defaultString(state.RuntimeDir, paths.runtimeDir)
	runtimeRoot := bridgeRuntimeRoot(runtimeDir, os.Getuid())
	stableSocket := filepath.Join(runtimeRoot, "session-"+sessionKey(state.SessionID)+".sock")
	backend := ""
	if state.ManagerPID > 1 {
		backend = filepath.Join(runtimeRoot, fmt.Sprintf("%d.sock", state.ManagerPID))
		if target, err := os.Readlink(stableSocket); err == nil {
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(stableSocket), resolved)
			}
			if samePath(resolved, backend) {
				_ = os.Remove(stableSocket)
			}
		}
		if safeBackendRemoval {
			_ = os.Remove(backend)
		}
		removeJSONIf(filepath.Join(paths.claudeRoot, "sessions", fmt.Sprintf("%d.json", state.ManagerPID)), func(row map[string]any) bool {
			return intValue(row["pid"]) == state.ManagerPID && stringValue(row["sessionId"]) == state.SessionID
		})
		removeJSONIf(filepath.Join(paths.dataRoot, "sessions", sessionKey(state.SessionID), "state.json"), func(row map[string]any) bool {
			return intValue(row["pid"]) == state.ManagerPID && stringValue(row["sessionId"]) == state.SessionID
		})
	} else if state.MessagingSocket != "" {
		_ = os.Remove(state.MessagingSocket)
	}
	if len(state.LaunchTokenHash) == 64 {
		hostPaths := grokRuntimePathsForKey(runtimeDir, os.Getuid(), state.LaunchTokenHash[:20])
		for _, path := range []string{hostPaths.ControlSocket, hostPaths.LeaderSocket, filepath.Join(hostPaths.LaunchDir, "leader.lock"), filepath.Join(hostPaths.LaunchDir, "diagnostics.log")} {
			_ = os.Remove(path)
		}
		_ = os.Remove(hostPaths.LaunchDir)
	}
	sessionDir := filepath.Join(paths.dataRoot, "sessions", sessionKey(state.SessionID))
	_ = os.Remove(filepath.Join(sessionDir, "inbox", "pending"))
	_ = os.Remove(filepath.Join(sessionDir, "inbox"))
	_ = os.Remove(sessionDir)
	return verifyGrokLaneCleanup(paths, state, safeBackendRemoval, backend)
}

func verifyGrokLaneCleanup(paths nativePaths, state grokLaneState, requireBackendAbsent bool, backend string) error {
	if _, err := os.Lstat(grokLaunchRecordPath(paths, state.SessionID)); err == nil || !os.IsNotExist(err) {
		return errors.New("grok lane launch ownership remains")
	}
	if grokLaneRegistryResidue(paths, state.SessionID) {
		return errors.New("grok lane registry publication remains")
	}
	runtimeDir := defaultString(state.RuntimeDir, paths.runtimeDir)
	runtimeRoot := bridgeRuntimeRoot(runtimeDir, os.Getuid())
	pathsToCheck := []string{
		filepath.Join(runtimeRoot, "session-"+sessionKey(state.SessionID)+".sock"),
		filepath.Join(paths.dataRoot, "sessions", sessionKey(state.SessionID)),
	}
	if state.ControlSocket != "" {
		pathsToCheck = append(pathsToCheck, state.ControlSocket)
	}
	if requireBackendAbsent && backend != "" {
		pathsToCheck = append(pathsToCheck, backend)
		if state.ManagerPID > 1 {
			pathsToCheck = append(pathsToCheck, filepath.Join(paths.claudeRoot, "sessions", fmt.Sprintf("%d.json", state.ManagerPID)))
		}
	}
	if len(state.LaunchTokenHash) == 64 {
		pathsToCheck = append(pathsToCheck, grokRuntimePathsForKey(runtimeDir, os.Getuid(), state.LaunchTokenHash[:20]).LaunchDir)
	}
	for _, path := range pathsToCheck {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return fmt.Errorf("grok lane cleanup residue remains at %s", path)
		}
	}
	return nil
}

func dropGrokLaneInbox(paths nativePaths, sessionID string) error {
	for {
		names, pending, err := nativeInboxNames(paths, sessionID)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		for _, name := range names {
			path := filepath.Join(pending, name)
			item := readJSONMap(path)
			if item == nil || stringValue(item["id"]) == "" {
				return fmt.Errorf("cannot account for malformed Grok lane inbox item %s", name)
			}
			record := readWakeRecord(paths, sessionID, stringValue(item["id"]))
			if record == nil {
				return fmt.Errorf("cannot account for Grok lane inbox item %s", name)
			}
			record.State, record.Delivery = "failed", "failed"
			if err := writeWakeRecord(paths, *record); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
}

func doctorGrokLane() (int, error) {
	paths := resolveNativePaths()
	grokBin := strings.TrimSpace(os.Getenv("GROK_PEER_GROK_BIN"))
	available := grokBin != ""
	version := ""
	versionError := ""
	if available {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, grokBin, "--no-auto-update", "--version") // #nosec G204 G702 -- launcher supplies a fail-closed validated Grok Build executable.
		body, err := command.Output()
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case timedOut:
			versionError = "Grok Build version check timed out"
		case err != nil:
			versionError = "Grok Build version check failed"
		default:
			version = strings.TrimSpace(string(body))
		}
	}
	_, supervisorErr := requestControl(paths.supervisorSock, map[string]any{"action": "status"}, 2*time.Second)
	supervisorReachable := supervisorErr == nil
	executable, _ := os.Executable()
	if err := emitLane(map[string]any{
		"type": "lane.doctor", "product": "grok", "contract_version": grokLaneContractVersion,
		"runtime_path": executable, "grok_available": available, "grok_path": emptyStringAsNil(grokBin),
		"grok_version": emptyStringAsNil(version), "grok_error": emptyStringAsNil(versionError),
		"state_root": profileDataRoot(paths), "supervisor_reachable": supervisorReachable,
		"supervisor_socket": paths.supervisorSock,
	}); err != nil {
		return 1, err
	}
	if !available || versionError != "" {
		return 1, nil
	}
	return 0, nil
}
