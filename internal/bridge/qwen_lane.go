package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const (
	qwenLaneVersion             = 1
	qwenLaneContractVersion     = 1
	qwenLaneManagerReadyTimeout = 75 * time.Second
	qwenLaneLaunchTokenEnv      = "AGENT_SESSIONS_QWEN_LANE_CAPABILITY"
	qwenLaneExecutableEnv       = "QWEN_PEER_QWEN_BIN"
)

type qwenLaneOptions struct {
	laneCommonOptions
	qwenHome          string
	qwenHomeSet       bool
	permissionMode    string
	permissionModeSet bool
	launchPreference  string
}

type qwenLaneTurn struct {
	ID               string `json:"id"`
	MessageID        string `json:"messageId,omitempty"`
	Fingerprint      string `json:"fingerprint,omitempty"`
	QwenSessionID    string `json:"qwenSessionId,omitempty"`
	Prompt           string `json:"prompt"`
	RequestDigest    string `json:"requestDigest"`
	Status           string `json:"status"`
	Result           string `json:"result,omitempty"`
	Error            string `json:"error,omitempty"`
	StopReason       string `json:"stopReason,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	Exit             int    `json:"exit"`
	CreatedAt        int64  `json:"createdAt"`
	StartedAt        int64  `json:"startedAt,omitempty"`
	CompletedAt      int64  `json:"completedAt,omitempty"`
	TimeoutMS        int64  `json:"timeoutMs,omitempty"`
	TerminalRevision string `json:"terminalRevision,omitempty"`
	Collected        bool   `json:"collected,omitempty"`
	CollectedAt      int64  `json:"collectedAt,omitempty"`
}

type qwenLaneDebt struct {
	Operation string `json:"operation"`
	Error     string `json:"error"`
	Attempts  int    `json:"attempts"`
	UpdatedAt int64  `json:"updatedAt"`
}

type qwenLaneState struct {
	Version               int                  `json:"version"`
	ContractVersion       int                  `json:"contractVersion"`
	Type                  string               `json:"type"`
	Name                  string               `json:"name"`
	ThreadID              string               `json:"threadId"`
	QwenSessionID         string               `json:"qwenSessionId,omitempty"`
	Cwd                   string               `json:"cwd"`
	Profile               qwenprofile.Identity `json:"profile"`
	Status                string               `json:"status"`
	ManagerPID            int                  `json:"managerPid,omitempty"`
	ManagerProcStart      string               `json:"managerProcStart,omitempty"`
	ManagerStrongStart    string               `json:"managerStrongStart,omitempty"`
	WorkerPID             int                  `json:"workerPid,omitempty"`
	WorkerProcStart       string               `json:"workerProcStart,omitempty"`
	WorkerStrongStart     string               `json:"workerStrongStart,omitempty"`
	ToolRegistryVersion   int                  `json:"toolRegistryVersion,omitempty"`
	ToolWrapperPath       string               `json:"toolWrapperPath,omitempty"`
	ToolRealBash          string               `json:"toolRealBash,omitempty"`
	ControlSocket         string               `json:"controlSocket,omitempty"`
	MessagingSocket       string               `json:"messagingSocket,omitempty"`
	ManagerLog            string               `json:"managerLog,omitempty"`
	RuntimeDir            string               `json:"runtimeDir,omitempty"`
	LaunchTokenHash       string               `json:"launchTokenHash,omitempty"`
	LaunchPreference      string               `json:"launchPermissionPreference"`
	RequestedInitialMode  string               `json:"requestedInitialMode,omitempty"`
	InitialNativeMode     string               `json:"initialNativeMode,omitempty"`
	CurrentNativeMode     string               `json:"currentNativeMode,omitempty"`
	OwnerPID              int                  `json:"ownerPid,omitempty"`
	OwnerProcStart        string               `json:"ownerProcStart,omitempty"`
	OwnerSessionID        string               `json:"ownerSessionId,omitempty"`
	NotifyTarget          string               `json:"notifyTarget,omitempty"`
	Persistent            bool                 `json:"persistent,omitempty"`
	AutoArchive           bool                 `json:"autoArchive,omitempty"`
	AutoArchiveDelayMS    int64                `json:"autoArchiveDelayMs,omitempty"`
	AutoArchiveAt         int64                `json:"autoArchiveAt,omitempty"`
	Turns                 []qwenLaneTurn       `json:"turns,omitempty"`
	ActiveTurnID          string               `json:"activeTurnId,omitempty"`
	PendingTurnIDs        []string             `json:"pendingTurnIds,omitempty"`
	LatestTurnID          string               `json:"latestTurnId,omitempty"`
	CollectedTurnID       string               `json:"collectedTurnId,omitempty"`
	TerminalOutcome       string               `json:"terminalOutcome,omitempty"`
	ExitCode              int                  `json:"exitCode,omitempty"`
	NativeArchiveState    string               `json:"nativeArchiveState"`
	Notices               []claudeLaneNotice   `json:"notices,omitempty"`
	Groups                []string             `json:"groups,omitempty"`
	ExplicitGroups        []string             `json:"explicitGroups,omitempty"`
	ParentSessionID       string               `json:"parentSessionId,omitempty"`
	ParentHostID          string               `json:"parentHostId,omitempty"`
	ParentAgentRuntimeDir string               `json:"parentAgentRuntimeDir,omitempty"`
	InheritParentGroups   bool                 `json:"inheritParentGroups,omitempty"`
	StartupID             string               `json:"startupId,omitempty"`
	CleanupDebt           []qwenLaneDebt       `json:"cleanupDebt,omitempty"`
	CreatedAt             int64                `json:"createdAt"`
	UpdatedAt             int64                `json:"updatedAt"`
}

func qwenLaneUsage() string {
	return `qwen-peer-lane — named, messageable Qwen Code lanes

Usage:
  qwen-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  qwen-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  qwen-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  qwen-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  qwen-peer-lane status SESSION_OR_NAME
  qwen-peer-lane interrupt SESSION_OR_NAME
  qwen-peer-lane archive SESSION_OR_NAME
  qwen-peer-lane list [--all] [--mine]
  qwen-peer-lane doctor [--json]

Options:
  -n, --name NAME
  -C, --cwd DIR
  -g, --group GROUP       add a child group; repeatable
      --inherit-groups    also inherit the parent's non-private groups
      --no-inherit-groups retain only the mandatory parent anchor
      --qwen-home DIR     select an exact absolute Qwen profile
      --yolo              request native initial approval mode yolo
      --no-yolo           request native initial approval mode default
      --approval-mode MODE
                          pass an exact native initial mode
      --timeout SECONDS
      --notify PEER       persistent lanes: send terminal pointers here
      --no-notify
      --persistent
      --auto-archive-after SECONDS
      --no-auto-archive
      --prompt-file FILE
      --all
      --mine
      --json

Qwen remains the owner of approval behavior after launch. Agent Sessions
records the launch preference and current ACP mode without preventing native
mode changes.
`
}

func parseQwenLaneArgs(argv []string) (qwenLaneOptions, error) {
	o := qwenLaneOptions{
		laneCommonOptions: newLaneCommonOptions("SESSION_OR_NAME"), launchPreference: "native_default",
	}
	start, done, err := beginLaneOptionParse(argv, &o.laneCommonOptions)
	if done || err != nil {
		return o, err
	}
	permissionChoices := 0
	parser := newLaneFlagParser("qwen-peer-lane", &o.laneCommonOptions)
	parser.set.StringVar(&o.qwenHome, "qwen-home", o.qwenHome, "Qwen profile")
	parser.set.Var(&laneChoiceFlag{destination: &o.permissionMode, fixed: "yolo", count: &permissionChoices}, "yolo", "Qwen yolo mode")
	parser.set.Lookup("yolo").NoOptDefVal = "yolo"
	parser.set.Var(&laneChoiceFlag{destination: &o.permissionMode, fixed: "default", count: &permissionChoices}, "no-yolo", "Qwen default mode")
	parser.set.Lookup("no-yolo").NoOptDefVal = "default"
	parser.set.Var(&laneChoiceFlag{destination: &o.permissionMode, count: &permissionChoices}, "approval-mode", "Qwen approval mode")
	positionals, err := parser.parse(argv[start:])
	if err != nil {
		return o, err
	}
	o.qwenHomeSet = parser.set.Changed("qwen-home")
	o.permissionModeSet = permissionChoices != 0
	if permissionChoices > 1 {
		return o, errors.New("qwen lane permission options are repeated or contradictory")
	}
	switch {
	case parser.set.Changed("yolo"):
		o.launchPreference = "yolo"
	case parser.set.Changed("no-yolo"):
		o.launchPreference = "non_yolo"
	case parser.set.Changed("approval-mode"):
		o.launchPreference = "native:" + o.permissionMode
	}
	if o.permissionModeSet && !containsString([]string{"default", "yolo", "plan", "auto", "accept_edits"}, o.permissionMode) {
		return o, fmt.Errorf("unsupported Qwen approval mode %q", o.permissionMode)
	}
	if err := validateQwenLaneCommandOptions(o); err != nil {
		return o, err
	}
	if err := validateLaneCommonOptions(&o.laneCommonOptions, positionals); err != nil {
		return o, err
	}
	return o, nil
}

func (o qwenLaneOptions) hasLaunchOptions() bool {
	return o.nameSet || o.cwdSet || o.qwenHomeSet || o.permissionModeSet || o.promptFile != "" || o.notifyExplicit ||
		o.disableNotify || o.persistentSet || o.noAutoArchiveSet || o.autoArchiveCustom || o.allowDuplicateName || o.stdinMarker
}

func validateQwenLaneCommandOptions(o qwenLaneOptions) error {
	launch := o.hasLaunchOptions()
	checks := []laneOptionCheck{}
	switch o.command {
	case "run", "start":
		checks = append(checks, laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	case "resume":
		checks = append(checks, laneOption("--name", o.nameSet), laneOption("--cwd", o.cwdSet), laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	case "wait":
		checks = append(checks, laneOption("launch options", launch), laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	case "list":
		checks = append(checks, laneOption("launch options", launch), laneOption("--timeout", o.timeoutSet), laneOption("--json", o.json))
	case "doctor":
		checks = append(checks,
			laneOption("--name", o.nameSet), laneOption("--prompt-file", o.promptFile != ""),
			laneOption("--notify", o.notifyExplicit), laneOption("--no-notify", o.disableNotify),
			laneOption("--persistent", o.persistentSet), laneOption("--no-auto-archive", o.noAutoArchiveSet),
			laneOption("--auto-archive-after", o.autoArchiveCustom), laneOption("--allow-duplicate-name", o.allowDuplicateName),
			laneOption("group options", o.groupOptions.groupsSpecified || o.groupOptions.inheritGroupsSpecified),
			laneOption("--timeout", o.timeoutSet), laneOption("--all", o.all), laneOption("--mine", o.mine),
		)
	default:
		checks = append(checks, laneOption("launch options", launch), laneOption("--timeout", o.timeoutSet), laneOption("--all", o.all), laneOption("--mine", o.mine), laneOption("--json", o.json))
	}
	return validateLaneCommandOptions(o.command, checks)
}

func qwenLaneStatePath(paths nativePaths, threadID string) string {
	return filepath.Join(profileDataRoot(paths), "qwen-lanes", sessionKey(threadID)+".json")
}

func qwenLaneControlSocket(paths nativePaths, threadID string) string {
	return filepath.Join(bridgeRuntimeRoot(paths.runtimeDir, os.Getuid()), "qw-"+sessionKey(threadID)+".sock")
}

func readQwenLaneState(paths nativePaths, threadID string) (qwenLaneState, error) {
	body, err := os.ReadFile(qwenLaneStatePath(paths, threadID))
	if err != nil {
		return qwenLaneState{}, err
	}
	var state qwenLaneState
	if json.Unmarshal(body, &state) != nil || state.Version != qwenLaneVersion || state.ContractVersion != qwenLaneContractVersion ||
		state.Type != "qwen-peer-lane" || state.ThreadID != threadID || !validSessionID(threadID) {
		return qwenLaneState{}, errors.New("qwen lane state/thread mismatch")
	}
	return state, nil
}

func writeQwenLaneState(paths nativePaths, state qwenLaneState) error {
	lock, err := lockLaneStateFile(paths, "qwen-"+state.ThreadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	return writeQwenLaneStateUnlocked(paths, state)
}

func writeQwenLaneStateUnlocked(paths nativePaths, state qwenLaneState) error {
	state.UpdatedAt = time.Now().UnixMilli()
	return writeJSONAtomic(qwenLaneStatePath(paths, state.ThreadID), state)
}

func readQwenLaneStates(paths nativePaths) []qwenLaneState {
	directory := filepath.Join(profileDataRoot(paths), "qwen-lanes")
	return readProductLaneStates(directory, func(entryName string, state *qwenLaneState) bool {
		return state.Type == "qwen-peer-lane" && entryName == sessionKey(state.ThreadID)+".json"
	}, func(state *qwenLaneState) int64 { return state.CreatedAt })
}

func resolveQwenLaneState(paths nativePaths, target string) (qwenLaneState, error) {
	return resolveProductLaneState(
		target, readQwenLaneStates(paths),
		func(state *qwenLaneState, candidate string) bool { return state.ThreadID == candidate },
		func(state *qwenLaneState) string { return state.Name },
		func(state *qwenLaneState) string { return state.Status },
		"Qwen", "thread ID",
	)
}

func newQwenLaneTurn(prompt string) qwenLaneTurn {
	digest := sha256.Sum256([]byte(prompt))
	return qwenLaneTurn{
		ID: randomID(), Prompt: prompt, RequestDigest: hex.EncodeToString(digest[:]), Status: "queued",
		CreatedAt: time.Now().UnixMilli(),
	}
}

func resolveQwenLaneProfile(o qwenLaneOptions) (qwenprofile.Identity, error) {
	lookup := os.LookupEnv
	if o.qwenHomeSet {
		lookup = func(name string) (string, bool) {
			if name == "QWEN_HOME" {
				return o.qwenHome, true
			}
			return os.LookupEnv(name)
		}
	}
	return qwenprofile.ResolveEnvironment(lookup)
}

func emitQwenLaneReady(state qwenLaneState) error {
	return emitLane(map[string]any{
		"type": "lane.ready", "contract_version": qwenLaneContractVersion, "product": "qwen",
		"name": state.Name, "thread_id": state.ThreadID, "session_id": state.ThreadID,
		"qwen_session_id": emptyStringAsNil(state.QwenSessionID), "turn_id": emptyStringAsNil(state.LatestTurnID),
		"cwd": state.Cwd, "address": encodeNativeAddress(state.MessagingSocket),
		"owner_session_id": emptyStringAsNil(state.OwnerSessionID), "persistent": state.Persistent,
		"notify_target": emptyStringAsNil(state.NotifyTarget), "groups": state.Groups,
		"launch_permission_preference": state.LaunchPreference, "initial_native_mode": emptyStringAsNil(state.InitialNativeMode),
		"current_native_mode": defaultString(state.CurrentNativeMode, "unknown"),
		"auto_archive":        state.AutoArchive, "auto_archive_after_seconds": float64(state.AutoArchiveDelayMS) / 1000,
	})
}

func qwenLaneManagerLive(state qwenLaneState) bool {
	return state.ManagerPID > 1 && state.ManagerProcStart != "" && exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) &&
		state.ControlSocket != "" && probeUnixSocket(state.ControlSocket, 250*time.Millisecond)
}

func compensateQwenLaneResume(paths nativePaths, archived, attempted qwenLaneState, resumeErr error) error {
	archiveErr := executeQwenArchiveTransaction(attempted, "archive")
	if archiveErr == nil {
		if err := writeQwenLaneState(paths, archived); err != nil {
			return errors.Join(resumeErr, fmt.Errorf("restore archived Qwen lane state: %w", err))
		}
		return resumeErr
	}
	attempted.Status, attempted.NativeArchiveState = "cleanup_debt", "unknown"
	attempted.ManagerPID, attempted.ManagerProcStart, attempted.ManagerStrongStart = 0, "", ""
	attempted.WorkerPID, attempted.WorkerProcStart, attempted.WorkerStrongStart = 0, "", ""
	attempted.ControlSocket, attempted.MessagingSocket, attempted.StartupID = "", "", ""
	attempted.CleanupDebt = []qwenLaneDebt{{
		Operation: "resume_compensation", Error: archiveErr.Error(), Attempts: nextQwenCleanupAttempt(attempted),
		UpdatedAt: time.Now().UnixMilli(),
	}}
	if err := writeQwenLaneState(paths, attempted); err != nil {
		return errors.Join(resumeErr, archiveErr, fmt.Errorf("persist Qwen resume compensation debt: %w", err))
	}
	return errors.Join(resumeErr, archiveErr)
}

func firstQwenLaneDebt(state qwenLaneState) string {
	for _, turn := range state.Turns {
		if !turn.Collected {
			return turn.ID
		}
	}
	return ""
}

func acknowledgeQwenLaneTurn(paths nativePaths, threadID, turnID string) error {
	state, err := readQwenLaneState(paths, threadID)
	if err != nil {
		return err
	}
	if qwenLaneManagerLive(state) {
		if _, err := requestControl(state.ControlSocket, map[string]any{"action": "ack", "sessionId": threadID, "turnId": turnID}, 5*time.Second); err == nil {
			return nil
		}
	}
	lock, err := lockLaneStateFile(paths, "qwen-"+threadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	state, err = readQwenLaneState(paths, threadID)
	if err != nil {
		return err
	}
	found := false
	for index := range state.Turns {
		if state.Turns[index].ID == turnID {
			state.Turns[index].Collected, state.Turns[index].CollectedAt = true, time.Now().UnixMilli()
			state.CollectedTurnID, found = turnID, true
			break
		}
	}
	if !found {
		return fmt.Errorf("qwen lane turn %s was not found", turnID)
	}
	return writeQwenLaneStateUnlocked(paths, state)
}

func qwenLaneStatusEvent(state qwenLaneState) map[string]any {
	return map[string]any{
		"type": "lane.status", "product": "qwen", "contract_version": qwenLaneContractVersion,
		"name": state.Name, "thread_id": state.ThreadID, "session_id": state.ThreadID,
		"qwen_session_id": emptyStringAsNil(state.QwenSessionID), "cwd": state.Cwd, "status": state.Status,
		"active_turn_id": emptyStringAsNil(state.ActiveTurnID), "latest_turn_id": emptyStringAsNil(state.LatestTurnID),
		"collected_turn_id": emptyStringAsNil(state.CollectedTurnID), "persistent": state.Persistent,
		"notify_target": emptyStringAsNil(state.NotifyTarget), "owner_session_id": emptyStringAsNil(state.OwnerSessionID),
		"groups": state.Groups, "launch_permission_preference": state.LaunchPreference,
		"initial_native_mode": emptyStringAsNil(state.InitialNativeMode), "current_native_mode": defaultString(state.CurrentNativeMode, "unknown"),
		"native_archive_state": state.NativeArchiveState, "auto_archive": state.AutoArchive,
		"auto_archive_after_seconds": float64(state.AutoArchiveDelayMS) / 1000, "auto_archive_at": nilIfZero(state.AutoArchiveAt),
		"cleanup_debt": state.CleanupDebt,
	}
}

func reconcileQwenLaneManagers(paths nativePaths) error {
	now := time.Now().UnixMilli()
	for _, state := range readQwenLaneStates(paths) {
		if err := reconcileQwenLaneManager(paths, state, now); err != nil {
			return fmt.Errorf("reconcile Qwen lane %s: %w", state.ThreadID, err)
		}
	}
	return nil
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func reconcileQwenLaneManager(paths nativePaths, state qwenLaneState, now int64) error {
	manager := cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart)
	managerUnavailable := state.ManagerPID <= 1 || manager.Status == processIdentityStale
	if managerUnavailable && qwenLaneHasUnsentNotices(state) {
		flushOrphanQwenLaneNotices(paths, state.ThreadID)
		if latest, err := readQwenLaneState(paths, state.ThreadID); err == nil {
			state = latest
		}
	}
	startingOrphan := state.Status == "starting" && state.ManagerPID <= 1 && state.UpdatedAt+20_000 <= now
	managerExited := state.ManagerPID > 1 && manager.Status == processIdentityStale
	missingManager := !containsString([]string{"starting", "archived"}, state.Status) && state.ManagerPID <= 1
	archivedResidue := state.Status == "archived" && managerUnavailable && qwenLaneHasOwnedResidue(paths, state)
	if startingOrphan || managerExited || missingManager || archivedResidue || (state.Status == "cleanup_debt" && managerUnavailable) {
		return forceArchiveQwenLane(paths, state.ThreadID)
	}
	return nil
}

func qwenLaneHasOwnedResidue(paths nativePaths, state qwenLaneState) bool {
	if state.ManagerPID > 1 && cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart).Status == processIdentityMatches {
		return true
	}
	if state.WorkerPID > 1 && cleanupProcessIdentityStatus(state.WorkerPID, state.WorkerProcStart).Status != processIdentityStale {
		return true
	}
	if state.ControlSocket != "" || state.MessagingSocket != "" || state.ToolRegistryVersion != 0 || len(state.CleanupDebt) != 0 {
		return true
	}
	return qwenLaneBridgeResidue(paths, state)
}

func qwenLaneBridgeResidue(paths nativePaths, state qwenLaneState) bool {
	runtimeRoot := bridgeRuntimeRoot(defaultString(state.RuntimeDir, paths.runtimeDir), os.Getuid())
	for _, path := range []string{
		filepath.Join(paths.dataRoot, "sessions", sessionKey(state.ThreadID), "state.json"),
		filepath.Join(runtimeRoot, "session-"+sessionKey(state.ThreadID)+".sock"),
	} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return true
		}
	}
	entries, _ := os.ReadDir(filepath.Join(paths.claudeRoot, "sessions"))
	for _, entry := range entries {
		row := readJSONMap(filepath.Join(paths.claudeRoot, "sessions", entry.Name()))
		if stringValue(row["sessionId"]) == state.ThreadID && stringValue(row["entrypoint"]) == "qwen" {
			return true
		}
	}
	return false
}

//nolint:gocyclo // Forced archive is an exact-identity lifecycle and cleanup-debt transaction.
func forceArchiveQwenLane(paths nativePaths, threadID string) error {
	const reason = "Qwen lane manager exited"
	observed, err := readQwenLaneState(paths, threadID)
	if err != nil {
		return err
	}
	lifecycle, err := acquireQwenLaneForForcedArchive(paths, observed)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycle)
	state, err := readQwenLaneState(paths, threadID)
	if err != nil {
		return err
	}
	if exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) {
		return errors.New("refuse to force archive a replacement live Qwen lane manager")
	}
	now := time.Now().UnixMilli()
	for index := range state.Turns {
		turn := &state.Turns[index]
		if containsString([]string{"queued", "active"}, turn.Status) {
			turn.Status, turn.Outcome, turn.Exit, turn.Error, turn.CompletedAt = "interrupted", "interrupted", 130, reason, now
			queueQwenLaneTerminalNotice(&state, *turn)
		}
	}
	state.Status, state.AutoArchiveAt = "retiring", 0
	state.CleanupDebt = nil
	if err := writeQwenLaneState(paths, state); err != nil {
		return err
	}
	registry, cleanupErr := lockQwenToolRegistry(state)
	if registry != nil {
		defer registry.close()
	}
	if state.WorkerPID > 1 {
		worker := qwenLaneProcessIdentity(state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart)
		observation := procinfo.Read(worker.PID)
		switch {
		case observation.Status == procinfo.Absent:
		case knownToolRootIdentity(observation, worker):
			cleanupErr = errors.Join(cleanupErr, stopGrokProcessSessionStrong(worker.PID, worker.ProcStart, worker.StrongStart, 0))
		case observation.Status == procinfo.Known:
			// The exact worker is gone and its PID belongs to another process.
		case observation.Status == procinfo.Unknown:
			cleanupErr = errors.Join(cleanupErr, errors.New("cannot corroborate Qwen ACP worker identity"))
		}
	}
	// The native worker is the parent/reaper for detached tool processes. Close
	// admission before stopping it, but reconcile the children only after the
	// exact worker has exited so a retired child cannot remain zombie-live and
	// create false cleanup debt.
	if cleanupErr == nil && registry != nil && registry.ledger != nil {
		cleanupErr = registry.ledger.reconcileCleanup()
	}
	if cleanupErr == nil {
		cleanupStaleBridgeArtifacts(paths)
		if state.ControlSocket == qwenLaneControlSocket(paths, state.ThreadID) {
			_ = os.Remove(state.ControlSocket)
		}
		if registry != nil {
			cleanupErr = registry.removeArtifacts()
		}
	}
	if cleanupErr != nil {
		state.Status = "cleanup_debt"
		state.CleanupDebt = []qwenLaneDebt{{Operation: "forced_cleanup", Error: cleanupErr.Error(), Attempts: nextQwenCleanupAttempt(state), UpdatedAt: now}}
		_ = writeQwenLaneState(paths, state)
		return cleanupErr
	}
	state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart = 0, "", ""
	state.MessagingSocket, state.ControlSocket = "", ""
	state.ToolRegistryVersion, state.ToolWrapperPath, state.ToolRealBash = 0, "", ""
	if err := completeQwenLaneArchive(paths, state, reason); err != nil {
		return err
	}
	latest, err := readQwenLaneState(paths, state.ThreadID)
	if err != nil {
		return err
	}
	if qwenLaneBridgeResidue(paths, latest) {
		return errors.New("qwen lane bridge residue remains after forced archive")
	}
	return nil
}

func nextQwenCleanupAttempt(state qwenLaneState) int {
	attempt := 1
	for _, debt := range state.CleanupDebt {
		if debt.Attempts >= attempt {
			attempt = debt.Attempts + 1
		}
	}
	return attempt
}

func acquireQwenLaneForForcedArchive(paths nativePaths, observed qwenLaneState) (*os.File, error) {
	lock, acquired, err := tryLockLaneLifecycle(paths, "qwen-"+observed.ThreadID)
	if err != nil || acquired {
		return lock, err
	}
	stopExactQwenLaneManager(observed.ManagerPID, observed.ManagerProcStart)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		lock, acquired, err = tryLockLaneLifecycle(paths, "qwen-"+observed.ThreadID)
		if err != nil || acquired {
			return lock, err
		}
		latest, readErr := readQwenLaneState(paths, observed.ThreadID)
		if readErr == nil && latest.ManagerPID > 1 &&
			(latest.ManagerPID != observed.ManagerPID || latest.ManagerProcStart != observed.ManagerProcStart) &&
			exactProcessIdentityMatch(latest.ManagerPID, latest.ManagerProcStart) {
			return nil, errors.New("qwen lane changed managers during forced archive")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, errors.New("timed out acquiring Qwen lane lifecycle for forced archive")
}

func stopExactQwenLaneManager(pid int, procStart string) {
	if !exactProcessIdentityMatch(pid, procStart) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !exactProcessIdentityMatch(pid, procStart) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if exactProcessIdentityMatch(pid, procStart) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func qwenLaneTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func validQwenLaneToken(token string) bool {
	return len(token) >= 64 && len(token) <= 256
}

func qwenLaneProcessIdentity(pid int, start, strong string) toolRootProcessIdentity {
	return toolRootProcessIdentity{PID: pid, ProcStart: start, StrongStart: strong}
}
