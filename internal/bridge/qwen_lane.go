package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
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

func runQwenLaneCommand(argv []string) int {
	return runProductLaneCommand(argv, productLaneCommands[qwenLaneOptions]{
		binary: "qwen-peer-lane", usage: qwenLaneUsage, parse: parseQwenLaneArgs, parseExit: 2,
		help: func(o qwenLaneOptions) bool { return o.help },
		prepare: func(o qwenLaneOptions) (qwenLaneOptions, error) {
			o = withQwenLaneLaunchContext(o)
			return o, reconcileQwenLaneManagers(resolveNativePaths())
		},
		command: func(o qwenLaneOptions) string { return o.command },
		start:   startQwenLane, resume: resumeQwenLane, wait: waitQwenLane, status: statusQwenLane,
		interrupt: interruptQwenLane, archive: archiveQwenLane, list: listQwenLanes, doctor: doctorQwenLane,
	})
}

func withQwenLaneLaunchContext(o qwenLaneOptions) qwenLaneOptions {
	o.laneCommonOptions = withCurrentLaneParent(o.laneCommonOptions)
	return o
}

func withQwenLaneResolvedParent(o qwenLaneOptions, owner laneOwner) qwenLaneOptions {
	o.laneCommonOptions = withResolvedLaneParent(o.laneCommonOptions, owner)
	return o
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

func newQwenLaneTurn(prompt string, timeout time.Duration) qwenLaneTurn {
	digest := sha256.Sum256([]byte(prompt))
	return qwenLaneTurn{
		ID: randomID(), Prompt: prompt, RequestDigest: hex.EncodeToString(digest[:]), Status: "queued",
		CreatedAt: time.Now().UnixMilli(), TimeoutMS: timeout.Milliseconds(),
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

func canonicalQwenLaneDirectory(value string) (string, error) {
	cwd, err := canonicalLaunchDirectory(value)
	if err != nil {
		return "", fmt.Errorf("resolve Qwen lane cwd: %w", err)
	}
	return cwd, nil
}

func startQwenLane(o qwenLaneOptions, wait bool) (int, error) {
	if err := validateLaneOwner(o.persistent, o.ownerPID, o.ownerProcStart); err != nil {
		return 1, err
	}
	prompt, err := readLanePrompt(laneOptions{laneCommonOptions: laneCommonOptions{promptFile: o.promptFile}})
	if err != nil {
		return 1, err
	}
	cwd, err := canonicalQwenLaneDirectory(o.cwd)
	if err != nil {
		return 1, err
	}
	profile, err := resolveQwenLaneProfile(o)
	if err != nil {
		return 1, err
	}
	if err := admitQwenLane(cwd, profile); err != nil {
		return 1, err
	}
	paths := resolveNativePaths()
	threadID := randomID()
	turn := newQwenLaneTurn(prompt, o.timeout)
	launchToken := randomID() + randomID()
	now := time.Now().UnixMilli()
	state := qwenLaneState{
		Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
		Name: sanitizeName(o.name), ThreadID: threadID, Cwd: cwd, Profile: profile, Status: "starting",
		ControlSocket: qwenLaneControlSocket(paths, threadID), ManagerLog: filepath.Join(profileDataRoot(paths), "qwen-lane-logs", sessionKey(threadID)+".log"),
		RuntimeDir: paths.runtimeDir, LaunchTokenHash: qwenLaneTokenHash(launchToken), LaunchPreference: o.launchPreference,
		RequestedInitialMode: o.permissionMode, CurrentNativeMode: "unknown", NativeArchiveState: "active",
		OwnerPID: o.ownerPID, OwnerProcStart: o.ownerProcStart, OwnerSessionID: o.ownerSessionID,
		NotifyTarget: o.notifyTarget, Persistent: o.persistent, AutoArchive: o.autoArchive, AutoArchiveDelayMS: o.autoArchiveDelay.Milliseconds(),
		Turns: []qwenLaneTurn{turn}, PendingTurnIDs: []string{turn.ID}, LatestTurnID: turn.ID, CreatedAt: now, UpdatedAt: now,
	}
	groupState, _, err := resolveLaneGroupState(threadID, "qwen", o.groupOptions, o.permissionMode == "yolo", o.permissionModeSet)
	if err != nil {
		return 1, fmt.Errorf("resolve lane groups: %w", err)
	}
	state.Groups, state.ExplicitGroups = groupState.Groups, groupState.ExplicitGroups
	state.ParentSessionID, state.ParentHostID, state.ParentAgentRuntimeDir = groupState.ParentSessionID, groupState.ParentHostID, groupState.ParentAgentRuntimeDir
	state.InheritParentGroups = groupState.InheritParentGroups
	if err := writeQwenLaneState(paths, state); err != nil {
		return 1, err
	}
	managerPID, managerStart, err := spawnQwenLaneManager(state, launchToken)
	if err != nil {
		_ = os.Remove(qwenLaneStatePath(paths, threadID))
		return 1, err
	}
	ready, err := waitQwenLaneReady(paths, threadID, managerPID, managerStart, qwenLaneManagerReadyTimeout)
	if err != nil {
		stopExactQwenLaneManager(managerPID, managerStart)
		return 1, err
	}
	_ = emitLane(map[string]any{"type": "thread.started", "thread_id": threadID, "session_id": threadID})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": threadID, "turn_id": turn.ID})
	if err := emitQwenLaneReady(ready); err != nil {
		return 1, err
	}
	if !wait {
		return 0, nil
	}
	return waitQwenLane(qwenLaneOptions{laneCommonOptions: laneCommonOptions{
		target: threadID, timeout: laneCollectionBound(o.timeout),
	}})
}

func spawnQwenLaneManager(state qwenLaneState, launchToken string) (int, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(state.ManagerLog), 0o700); err != nil {
		return 0, "", err
	}
	logFile, err := os.OpenFile(state.ManagerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = logFile.Close() }()
	command := exec.Command(executable, "qwen-lane-manager", "--session-id", state.ThreadID) //nolint:gosec // current installed runtime.
	command.Stdin, command.Stdout, command.Stderr = nil, logFile, logFile
	command.Env = qwenLaneManagerEnvironment(os.Environ(), launchToken)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, "", err
	}
	pid := command.Process.Pid
	procStart, err := captureProcessStart(pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return 0, "", fmt.Errorf("capture Qwen lane manager identity: %w", err)
	}
	_ = command.Process.Release()
	return pid, procStart, nil
}

func qwenLaneManagerEnvironment(environment []string, token string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, qwenLaneLaunchTokenEnv+"=") {
			result = append(result, entry)
		}
	}
	return append(result, qwenLaneLaunchTokenEnv+"="+token)
}

func waitQwenLaneReady(paths nativePaths, threadID string, managerPID int, managerStart string, timeout time.Duration) (qwenLaneState, error) {
	return waitProductLaneReady(
		managerPID, managerStart, timeout,
		func() (qwenLaneState, error) { return readQwenLaneState(paths, threadID) },
		func(state *qwenLaneState) bool {
			return state.ManagerPID == managerPID && state.Status != "starting" && state.MessagingSocket != "" &&
				probeUnixSocket(state.MessagingSocket, 200*time.Millisecond)
		},
		"qwen lane manager exited during startup; inspect its private manager log",
		"timed out starting Qwen lane manager; inspect its private manager log",
	)
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

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func resumeQwenLane(o qwenLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveQwenLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	profile, err := resolveQwenLaneProfile(o)
	if err != nil {
		return 1, err
	}
	if err := qwenprofile.MatchResume(state.Profile, profile); err != nil {
		return 1, err
	}
	if err := admitQwenLane(state.Cwd, profile); err != nil {
		return 1, err
	}
	if debt := firstQwenLaneDebt(state); debt != "" {
		return 1, fmt.Errorf("collect outstanding Qwen lane turn %s before resume", debt)
	}
	desiredPersistent := state.Persistent || o.persistentSet
	if o.notifyExplicit && !desiredPersistent {
		return 1, errors.New("--notify requires a persistent lane; pass --persistent to promote this lane")
	}
	if err := validateLaneOwner(desiredPersistent, o.ownerPID, o.ownerProcStart); err != nil {
		return 1, err
	}
	groupState, _, err := resolveLaneGroupState(state.ThreadID, "qwen", o.groupOptions, o.permissionMode == "yolo", o.permissionModeSet)
	if err != nil {
		return 1, fmt.Errorf("resolve lane groups: %w", err)
	}
	state.Groups, state.ExplicitGroups = groupState.Groups, groupState.ExplicitGroups
	state.ParentSessionID, state.ParentHostID, state.ParentAgentRuntimeDir = groupState.ParentSessionID, groupState.ParentHostID, groupState.ParentAgentRuntimeDir
	state.InheritParentGroups = groupState.InheritParentGroups
	prompt, err := readLanePrompt(laneOptions{laneCommonOptions: laneCommonOptions{promptFile: o.promptFile}})
	if err != nil {
		return 1, err
	}
	turn := newQwenLaneTurn(prompt, o.timeout)
	if qwenLaneManagerLive(state) {
		request := map[string]any{
			"action": "resume", "sessionId": state.ThreadID, "turn": turn,
			"persistent": desiredPersistent, "ownerPid": o.ownerPID, "ownerProcStart": o.ownerProcStart, "ownerSessionId": o.ownerSessionID,
			"groups": state.Groups, "explicitGroups": state.ExplicitGroups,
			"parentSessionId": state.ParentSessionID, "parentHostId": state.ParentHostID,
			"parentAgentRuntimeDir": state.ParentAgentRuntimeDir, "inheritParentGroups": state.InheritParentGroups,
		}
		if o.permissionModeSet {
			request["requestedInitialMode"], request["launchPreference"] = o.permissionMode, o.launchPreference
		}
		switch {
		case o.notifyExplicit:
			request["notifySet"], request["notifyTarget"] = true, o.notifyTarget
		case o.disableNotify:
			request["notifySet"], request["notifyTarget"] = true, ""
		case !desiredPersistent:
			request["notifySet"], request["notifyTarget"] = true, o.notifyTarget
		}
		if o.autoArchiveCustom {
			request["autoArchive"], request["autoArchiveDelayMs"] = true, o.autoArchiveDelay.Milliseconds()
		}
		if o.noAutoArchiveSet {
			request["autoArchive"] = false
		}
		if _, err := requestControl(state.ControlSocket, request, 10*time.Second); err != nil {
			return 1, err
		}
		_ = emitLane(map[string]any{"type": "thread.resumed", "thread_id": state.ThreadID, "session_id": state.ThreadID})
		return waitQwenLane(qwenLaneOptions{laneCommonOptions: laneCommonOptions{
			target: state.ThreadID, timeout: laneCollectionBound(o.timeout),
		}})
	}
	if state.Status != "archived" || state.NativeArchiveState != "archived" {
		return 1, errors.New("qwen lane is not cleanly archived")
	}
	archivedState := cloneQwenLaneState(state)
	if err := executeQwenArchiveTransaction(state, "unarchive"); err != nil {
		return 1, err
	}
	launchToken := randomID() + randomID()
	state.Status, state.NativeArchiveState, state.StartupID = "starting", "active", randomID()
	state.LaunchTokenHash, state.ControlSocket, state.RuntimeDir = qwenLaneTokenHash(launchToken), qwenLaneControlSocket(paths, state.ThreadID), paths.runtimeDir
	state.ManagerPID, state.ManagerProcStart, state.ManagerStrongStart = 0, "", ""
	state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart, state.MessagingSocket = 0, "", "", ""
	state.ToolRegistryVersion, state.ToolWrapperPath, state.ToolRealBash = 0, "", ""
	state.Persistent = desiredPersistent
	if desiredPersistent {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = 0, "", ""
	} else {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = o.ownerPID, o.ownerProcStart, o.ownerSessionID
	}
	switch {
	case o.notifyExplicit:
		state.NotifyTarget = o.notifyTarget
	case o.disableNotify:
		state.NotifyTarget = ""
	case !desiredPersistent:
		state.NotifyTarget = o.notifyTarget
	}
	if o.autoArchiveCustom {
		state.AutoArchive, state.AutoArchiveDelayMS = true, o.autoArchiveDelay.Milliseconds()
	}
	if o.noAutoArchiveSet {
		state.AutoArchive = false
	}
	if o.permissionModeSet {
		state.RequestedInitialMode, state.LaunchPreference = o.permissionMode, o.launchPreference
	} else {
		state.RequestedInitialMode, state.LaunchPreference = "", "native_default"
	}
	state.Turns = append(state.Turns, turn)
	state.PendingTurnIDs = append(state.PendingTurnIDs, turn.ID)
	state.LatestTurnID, state.AutoArchiveAt = turn.ID, 0
	if err := writeQwenLaneState(paths, state); err != nil {
		return 1, compensateQwenLaneResume(paths, archivedState, state, err)
	}
	managerPID, managerStart, err := spawnQwenLaneManager(state, launchToken)
	if err != nil {
		return 1, compensateQwenLaneResume(paths, archivedState, state, err)
	}
	ready, err := waitQwenLaneReady(paths, state.ThreadID, managerPID, managerStart, qwenLaneManagerReadyTimeout)
	if err != nil {
		stopExactQwenLaneManager(managerPID, managerStart)
		return 1, compensateQwenLaneResume(paths, archivedState, state, err)
	}
	_ = emitLane(map[string]any{"type": "thread.resumed", "thread_id": state.ThreadID, "session_id": state.ThreadID})
	if err := emitQwenLaneReady(ready); err != nil {
		stopExactQwenLaneManager(managerPID, managerStart)
		return 1, compensateQwenLaneResume(paths, archivedState, state, err)
	}
	return waitQwenLane(qwenLaneOptions{laneCommonOptions: laneCommonOptions{
		target: state.ThreadID, timeout: laneCollectionBound(o.timeout),
	}})
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

func waitQwenLane(o qwenLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveQwenLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	lock, err := lockLaneStateFile(paths, "qwen-collect-"+state.ThreadID)
	if err != nil {
		return 1, err
	}
	defer unlockLaneStateFile(lock)
	deadline := time.Time{}
	if o.timeout > 0 {
		deadline = time.Now().Add(o.timeout)
	}
	for {
		state, err = readQwenLaneState(paths, state.ThreadID)
		if err != nil {
			return 1, err
		}
		for _, turn := range state.Turns {
			if turn.Collected || !containsString([]string{"completed", "failed", "interrupted", "timed_out"}, turn.Status) {
				continue
			}
			if err := emitQwenLaneTurn(state, turn); err != nil {
				return 1, err
			}
			if err := acknowledgeQwenLaneTurn(paths, state.ThreadID, turn.ID); err != nil {
				return 1, err
			}
			return turn.Exit, nil
		}
		if state.Status == "archived" {
			return 1, fmt.Errorf("qwen lane %s is archived and has no collectable turn", state.ThreadID)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return 124, context.DeadlineExceeded
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func emitQwenLaneTurn(state qwenLaneState, turn qwenLaneTurn) error {
	if err := emitLane(map[string]any{"type": "item.completed", "thread_id": state.ThreadID, "turn_id": turn.ID,
		"item": map[string]any{"id": "user-" + first8(turn.ID), "type": "user_message", "text": turn.Prompt}}); err != nil {
		return err
	}
	if turn.Result != "" {
		if err := emitLane(map[string]any{"type": "item.completed", "thread_id": state.ThreadID, "turn_id": turn.ID,
			"item": map[string]any{"id": "answer-" + first8(turn.ID), "type": "agent_message", "phase": "final_answer", "text": turn.Result}}); err != nil {
			return err
		}
	}
	return emitLane(map[string]any{"type": "turn.completed", "thread_id": state.ThreadID, "turn_id": turn.ID,
		"status": turn.Status, "outcome": turn.Outcome, "exit": turn.Exit, "error": emptyStringAsNil(turn.Error), "stop_reason": emptyStringAsNil(turn.StopReason)})
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

func statusQwenLane(o qwenLaneOptions) (int, error) {
	state, err := resolveQwenLaneState(resolveNativePaths(), o.target)
	if err != nil {
		return 1, err
	}
	return 0, emitLane(qwenLaneStatusEvent(state))
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

func listQwenLanes(o qwenLaneOptions) (int, error) {
	return listProductLaneStates(
		o.laneCommonOptions, "qwen", qwenLaneContractVersion, readQwenLaneStates(resolveNativePaths()),
		func(state *qwenLaneState) string { return state.Status },
		func(state *qwenLaneState) (bool, int, string) {
			return state.Persistent, state.OwnerPID, state.OwnerProcStart
		},
		qwenLaneStatusEvent,
	)
}

func interruptQwenLane(o qwenLaneOptions) (int, error) {
	state, err := resolveQwenLaneState(resolveNativePaths(), o.target)
	if err != nil {
		return 1, err
	}
	if !qwenLaneManagerLive(state) {
		return 1, fmt.Errorf("qwen lane %s is not live", state.ThreadID)
	}
	response, err := requestControl(state.ControlSocket, map[string]any{"action": "interrupt", "sessionId": state.ThreadID}, 5*time.Second)
	if err != nil {
		return 1, err
	}
	return 0, emitLane(map[string]any{"type": "turn.interrupted", "thread_id": state.ThreadID, "turn_id": response["turnId"]})
}

func archiveQwenLane(o qwenLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveQwenLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" && state.NativeArchiveState == "archived" && len(state.CleanupDebt) == 0 {
		return 0, emitLane(map[string]any{"type": "lane.archived", "product": "qwen", "name": state.Name, "thread_id": state.ThreadID, "already_archived": true})
	}
	if qwenLaneManagerLive(state) {
		if _, err := requestControl(state.ControlSocket, map[string]any{"action": "archive", "sessionId": state.ThreadID}, 30*time.Second); err != nil {
			return 1, err
		}
	} else if err := forceArchiveQwenLane(paths, state.ThreadID, "explicit archive with manager unavailable"); err != nil {
		return 1, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		latest, readErr := readQwenLaneState(paths, state.ThreadID)
		if readErr == nil && latest.Status == "archived" && latest.NativeArchiveState == "archived" && len(latest.CleanupDebt) == 0 {
			return 0, emitLane(map[string]any{"type": "lane.archived", "product": "qwen", "name": latest.Name, "thread_id": latest.ThreadID})
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 1, errors.New("timed out waiting for Qwen lane archive")
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
		return forceArchiveQwenLane(paths, state.ThreadID, "Qwen lane manager exited")
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
func forceArchiveQwenLane(paths nativePaths, threadID, reason string) error {
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
	if strings.HasPrefix(reason, "explicit archive") {
		cancelAllQwenLaneNotices(&state)
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
		_ = reconcileDeadConnectorArtifacts(paths)
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

func admitQwenLane(cwd string, profile qwenprofile.Identity) error {
	executable := strings.TrimSpace(os.Getenv(qwenLaneExecutableEnv))
	if executable == "" {
		return errors.New("validated Qwen executable is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	report, err := qwenreadiness.Check(ctx, qwenreadiness.Request{
		Executable: executable, Workspace: cwd, Profile: profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(os.Environ()),
	})
	if err != nil {
		return fmt.Errorf("check Qwen lane readiness: %w", err)
	}
	if !report.Ready {
		issues := make([]string, 0, len(report.Issues))
		for _, issue := range report.Issues {
			issues = append(issues, issue.Code+": "+issue.Message)
		}
		return fmt.Errorf("qwen lane is not ready: %s", strings.Join(issues, "; "))
	}
	return nil
}
