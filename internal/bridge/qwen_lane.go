package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	command            string
	name               string
	nameSet            bool
	target             string
	cwd                string
	cwdSet             bool
	qwenHome           string
	qwenHomeSet        bool
	permissionMode     string
	permissionModeSet  bool
	launchPreference   string
	timeout            time.Duration
	timeoutSet         bool
	promptFile         string
	notifyTarget       string
	notifyExplicit     bool
	disableNotify      bool
	persistent         bool
	persistentSet      bool
	autoArchive        bool
	autoArchiveDelay   time.Duration
	autoArchiveCustom  bool
	noAutoArchiveSet   bool
	allowDuplicateName bool
	all                bool
	mine               bool
	json               bool
	stdinMarker        bool
	ownerPID           int
	ownerProcStart     string
	ownerSessionID     string
	help               bool
	groupOptions       laneGroupOptions
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

//nolint:gocyclo // The complete CLI conflict contract must reject before state mutation.
func parseQwenLaneArgs(argv []string) (qwenLaneOptions, error) {
	o := qwenLaneOptions{cwd: mustGetwd(), autoArchive: true, autoArchiveDelay: defaultLaneAutoArchiveDelay, launchPreference: "native_default"}
	if len(argv) == 0 {
		o.help = true
		return o, nil
	}
	for _, argument := range argv {
		if argument == "-h" || argument == "--help" {
			o.help = true
			return o, nil
		}
	}
	o.command = argv[0]
	if !containsString([]string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"}, o.command) {
		return o, fmt.Errorf("unknown command %q", o.command)
	}
	positionals := []string{}
	permissionChoices := 0
	for index := 1; index < len(argv); index++ {
		argument := argv[index]
		take := func() (string, error) {
			if index+1 >= len(argv) || argv[index+1] == "" {
				return "", fmt.Errorf("%s requires a value", argument)
			}
			index++
			return argv[index], nil
		}
		var value string
		var err error
		switch argument {
		case "-n", "--name", "--peer-name":
			value, err = take()
			o.name, o.nameSet = value, true
		case "-C", "--cwd", "--cd":
			value, err = take()
			o.cwd, o.cwdSet = value, true
		case "-g", "--group":
			value, err = take()
			o.groupOptions.groups = append(o.groupOptions.groups, value)
			o.groupOptions.groupsSpecified = true
		case "--inherit-groups":
			o.groupOptions.inheritParentGroups, o.groupOptions.inheritGroupsSpecified = true, true
		case "--no-inherit-groups":
			o.groupOptions.inheritParentGroups, o.groupOptions.inheritGroupsSpecified = false, true
		case "--qwen-home":
			value, err = take()
			o.qwenHome, o.qwenHomeSet = value, true
		case "--yolo":
			permissionChoices++
			o.permissionMode, o.permissionModeSet, o.launchPreference = "yolo", true, "yolo"
		case "--no-yolo":
			permissionChoices++
			o.permissionMode, o.permissionModeSet, o.launchPreference = "default", true, "non_yolo"
		case "--approval-mode":
			permissionChoices++
			value, err = take()
			o.permissionMode, o.permissionModeSet, o.launchPreference = value, true, "native:"+value
		case "--timeout":
			value, err = take()
			if err == nil {
				o.timeout, err = parseQwenLaneSeconds(value, false, "--timeout")
				o.timeoutSet = err == nil
			}
		case "--prompt-file":
			value, err = take()
			o.promptFile = value
		case "--notify":
			value, err = take()
			o.notifyTarget, o.notifyExplicit = value, true
		case "--no-notify":
			o.disableNotify = true
		case "--persistent":
			o.persistent, o.persistentSet = true, true
		case "--no-auto-archive":
			o.autoArchive, o.noAutoArchiveSet = false, true
		case "--auto-archive-after":
			value, err = take()
			if err == nil {
				o.autoArchiveDelay, err = parseQwenLaneSeconds(value, true, "--auto-archive-after")
				o.autoArchiveCustom = err == nil
			}
		case "--allow-duplicate-name":
			o.allowDuplicateName = true
		case "--all":
			o.all = true
		case "--mine":
			o.mine = true
		case "--json":
			o.json = true
		case "-":
			o.stdinMarker = true
		default:
			if strings.HasPrefix(argument, "-") {
				return o, fmt.Errorf("unknown option %s", argument)
			}
			positionals = append(positionals, argument)
		}
		if err != nil {
			return o, err
		}
	}
	if permissionChoices > 1 {
		return o, errors.New("qwen lane permission options are repeated or contradictory")
	}
	if o.permissionModeSet && !containsString([]string{"default", "yolo", "plan", "auto", "accept_edits"}, o.permissionMode) {
		return o, fmt.Errorf("unsupported Qwen approval mode %q", o.permissionMode)
	}
	if o.notifyTarget != "" && o.disableNotify {
		return o, errors.New("--notify and --no-notify cannot be used together")
	}
	if o.notifyExplicit && !o.persistent && o.command != "resume" {
		return o, errors.New("--notify requires --persistent; parent-owned lanes notify their owner automatically")
	}
	if o.autoArchiveCustom && !o.autoArchive {
		return o, errors.New("--auto-archive-after and --no-auto-archive cannot be used together")
	}
	if o.mine && o.command != "list" {
		return o, fmt.Errorf("--mine is not valid for %s", o.command)
	}
	if err := validateLaneGroupCommand(o.command, o.groupOptions); err != nil {
		return o, err
	}
	if err := validateQwenLaneCommandOptions(o); err != nil {
		return o, err
	}
	switch o.command {
	case "run", "start":
		if strings.TrimSpace(o.name) == "" {
			return o, fmt.Errorf("%s requires --name", o.command)
		}
		if len(positionals) != 0 {
			return o, fmt.Errorf("%s does not accept a prompt on argv; use stdin or --prompt-file", o.command)
		}
	case "resume":
		if len(positionals) != 1 {
			return o, errors.New("resume requires exactly one SESSION_OR_NAME")
		}
		o.target = positionals[0]
	case "list", "doctor":
		if len(positionals) != 0 {
			return o, fmt.Errorf("%s does not accept positional arguments", o.command)
		}
	default:
		if len(positionals) != 1 {
			return o, fmt.Errorf("%s requires exactly one SESSION_OR_NAME", o.command)
		}
		o.target = positionals[0]
	}
	return o, nil
}

func parseQwenLaneSeconds(value string, positive bool, flag string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	minimum := 0.0
	message := flag + " must be a non-negative number of seconds"
	if positive {
		minimum, message = 0.001, flag+" must be at least 0.001 seconds"
	}
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < minimum || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, errors.New(message)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (o qwenLaneOptions) hasLaunchOptions() bool {
	return o.nameSet || o.cwdSet || o.qwenHomeSet || o.permissionModeSet || o.promptFile != "" || o.notifyExplicit ||
		o.disableNotify || o.persistentSet || o.noAutoArchiveSet || o.autoArchiveCustom || o.allowDuplicateName || o.stdinMarker
}

func validateQwenLaneCommandOptions(o qwenLaneOptions) error {
	invalid := func(flag string, set bool) error {
		if set {
			return fmt.Errorf("%s is not valid for %s", flag, o.command)
		}
		return nil
	}
	launch := o.hasLaunchOptions()
	checks := [][2]any{}
	switch o.command {
	case "run", "start":
		checks = append(checks, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	case "resume":
		checks = append(checks, [2]any{"--name", o.nameSet}, [2]any{"--cwd", o.cwdSet}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	case "wait":
		checks = append(checks, [2]any{"launch options", launch}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	case "list":
		checks = append(checks, [2]any{"launch options", launch}, [2]any{"--timeout", o.timeoutSet}, [2]any{"--json", o.json})
	case "doctor":
		checks = append(checks,
			[2]any{"--name", o.nameSet}, [2]any{"--prompt-file", o.promptFile != ""},
			[2]any{"--notify", o.notifyExplicit}, [2]any{"--no-notify", o.disableNotify},
			[2]any{"--persistent", o.persistentSet}, [2]any{"--no-auto-archive", o.noAutoArchiveSet},
			[2]any{"--auto-archive-after", o.autoArchiveCustom}, [2]any{"--allow-duplicate-name", o.allowDuplicateName},
			[2]any{"group options", o.groupOptions.groupsSpecified || o.groupOptions.inheritGroupsSpecified},
			[2]any{"--timeout", o.timeoutSet}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine},
		)
	default:
		checks = append(checks, [2]any{"launch options", launch}, [2]any{"--timeout", o.timeoutSet}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	}
	for _, check := range checks {
		if err := invalid(check[0].(string), check[1].(bool)); err != nil {
			return err
		}
	}
	return nil
}

func runQwenLaneCommand(argv []string) int {
	o, err := parseQwenLaneArgs(argv)
	if err != nil {
		return reportQwenLaneError(err, true)
	}
	if o.help {
		fmt.Print(qwenLaneUsage())
		return 0
	}
	o = withQwenLaneLaunchContext(o)
	if err := reconcileQwenLaneManagers(resolveNativePaths()); err != nil {
		return reportQwenLaneError(err, false)
	}
	var code int
	switch o.command {
	case "run":
		code, err = startQwenLane(o, true)
	case "start":
		code, err = startQwenLane(o, false)
	case "resume":
		code, err = resumeQwenLane(o)
	case "wait":
		code, err = waitQwenLane(o)
	case "status":
		code, err = statusQwenLane(o)
	case "interrupt":
		code, err = interruptQwenLane(o)
	case "archive":
		code, err = archiveQwenLane(o)
	case "list":
		code, err = listQwenLanes(o)
	case "doctor":
		code, err = doctorQwenLane(o)
	}
	if err != nil {
		return reportQwenLaneError(err, false)
	}
	return code
}

func reportQwenLaneError(err error, usage bool) int {
	_ = emitLane(map[string]any{"type": "error", "message": err.Error(), "timeout": errors.Is(err, context.DeadlineExceeded)})
	fmt.Fprintf(os.Stderr, "qwen-peer-lane: %v\n", err)
	if usage {
		return 2
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return 1
}

func withQwenLaneLaunchContext(o qwenLaneOptions) qwenLaneOptions {
	listMine := o.command == "list" && o.mine
	if !containsString([]string{"run", "start", "resume"}, o.command) && !listMine {
		return o
	}
	return withQwenLaneResolvedParent(o, inferPeerParent(resolveNativePaths(), os.Getpid()))
}

func withQwenLaneResolvedParent(o qwenLaneOptions, owner laneOwner) qwenLaneOptions {
	listMine := o.command == "list" && o.mine
	o.groupOptions = applyAgentParentContext(o.groupOptions, &owner)
	if owner.SessionID != "" {
		o.groupOptions.parentSessionID = owner.SessionID
		if !o.persistent || listMine {
			o.ownerPID, o.ownerProcStart, o.ownerSessionID = owner.PID, owner.ProcStart, owner.SessionID
		}
		if !listMine && !o.persistent && !o.disableNotify && !o.notifyExplicit {
			o.notifyTarget = "session:" + owner.SessionID
		}
	}
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
	entries, _ := os.ReadDir(directory)
	states := []qwenLaneState{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var state qwenLaneState
		body, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // bridge-owned state directory.
		if err != nil || json.Unmarshal(body, &state) != nil || state.Type != "qwen-peer-lane" || entry.Name() != sessionKey(state.ThreadID)+".json" {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].CreatedAt > states[j].CreatedAt })
	return states
}

func resolveQwenLaneState(paths nativePaths, target string) (qwenLaneState, error) {
	target = strings.TrimSpace(target)
	byName := []qwenLaneState{}
	for _, state := range readQwenLaneStates(paths) {
		if state.ThreadID == target {
			return state, nil
		}
		if strings.EqualFold(state.Name, target) {
			byName = append(byName, state)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		active := []qwenLaneState{}
		for _, state := range byName {
			if state.Status != "archived" {
				active = append(active, state)
			}
		}
		if len(active) == 1 {
			return active[0], nil
		}
		return qwenLaneState{}, fmt.Errorf("qwen lane name %q is ambiguous; use a thread ID", target)
	}
	return qwenLaneState{}, fmt.Errorf("no Qwen lane matching %q", target)
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
	prompt, err := readLanePrompt(laneOptions{promptFile: o.promptFile})
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
	return waitQwenLane(qwenLaneOptions{target: threadID, timeout: laneCollectionBound(o.timeout)})
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
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := readQwenLaneState(paths, threadID)
		if err == nil && state.ManagerPID == managerPID && state.Status != "starting" && state.MessagingSocket != "" &&
			probeUnixSocket(state.MessagingSocket, 200*time.Millisecond) {
			return state, nil
		}
		if cleanupProcessIdentityStatus(managerPID, managerStart).Status == processIdentityStale {
			return qwenLaneState{}, errors.New("qwen lane manager exited during startup; inspect its private manager log")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return qwenLaneState{}, errors.New("timed out starting Qwen lane manager; inspect its private manager log")
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
	prompt, err := readLanePrompt(laneOptions{promptFile: o.promptFile})
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
		return waitQwenLane(qwenLaneOptions{target: state.ThreadID, timeout: laneCollectionBound(o.timeout)})
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
	return waitQwenLane(qwenLaneOptions{target: state.ThreadID, timeout: laneCollectionBound(o.timeout)})
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
	if o.mine && !validLaneOwner(o.ownerPID, o.ownerProcStart) {
		return 1, errors.New("cannot establish the current orchestrator identity for --mine")
	}
	rows := []map[string]any{}
	for _, state := range readQwenLaneStates(resolveNativePaths()) {
		if !o.all && state.Status == "archived" {
			continue
		}
		if o.mine && (state.Persistent || !sameLaneOwner(state.OwnerPID, state.OwnerProcStart, o.ownerPID, o.ownerProcStart)) {
			continue
		}
		row := qwenLaneStatusEvent(state)
		delete(row, "type")
		rows = append(rows, row)
	}
	return 0, emitLane(map[string]any{"type": "lane.list", "product": "qwen", "contract_version": qwenLaneContractVersion, "lanes": rows})
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
		_ = cleanupStaleBridgeArtifacts(paths)
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
