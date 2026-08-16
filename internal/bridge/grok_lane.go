package bridge

import (
	"context"
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
)

const (
	grokLaneContractVersion     = 1
	grokLaneMCPReadyTimeout     = 35 * time.Second
	grokLaneManagerReadyTimeout = grokACPStartupTimeout + grokLaneMCPReadyTimeout + 10*time.Second
)

type grokLaneOptions struct {
	command            string
	name               string
	nameSet            bool
	target             string
	cwd                string
	cwdSet             bool
	model              string
	modelSet           bool
	reasoningEffort    string
	reasoningEffortSet bool
	permissionMode     string
	permissionModeSet  bool
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
      --allow-duplicate-name
      --prompt-file FILE
      --all
      --mine

Headless Grok Build lanes use explicit always-approve mode. They own separate
ACP sessions and never attach to an interactive grok-peer conversation.
`
}

//nolint:gocyclo // The lane CLI contract is centralized so unsupported option combinations fail closed.
func parseGrokLaneArgs(argv []string) (grokLaneOptions, error) {
	o := grokLaneOptions{
		cwd: mustGetwd(), permissionMode: "bypassPermissions", autoArchive: true,
		autoArchiveDelay: defaultLaneAutoArchiveDelay,
	}
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
		case "-C", "--cd":
			value, err = take()
			o.cwd, o.cwdSet = value, true
		case "-m", "--model":
			value, err = take()
			o.model, o.modelSet = value, true
		case "--reasoning-effort", "--effort":
			value, err = take()
			o.reasoningEffort, o.reasoningEffortSet = value, true
		case "--permission-mode":
			value, err = take()
			o.permissionMode, o.permissionModeSet = value, true
		case "--always-approve", "--yolo":
			o.permissionMode, o.permissionModeSet = "bypassPermissions", true
		case "--timeout":
			value, err = take()
			if err == nil {
				o.timeout, err = parseGrokLaneSeconds(value, false, "--timeout")
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
				o.autoArchiveDelay, err = parseGrokLaneSeconds(value, true, "--auto-archive-after")
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
	if o.permissionMode != "bypassPermissions" {
		return o, fmt.Errorf("unsupported headless Grok permission mode %q; use bypassPermissions", o.permissionMode)
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
	if o.autoArchiveCustom && !containsString([]string{"run", "start", "resume"}, o.command) {
		return o, fmt.Errorf("--auto-archive-after is not valid for %s", o.command)
	}
	if o.mine && o.command != "list" {
		return o, fmt.Errorf("--mine is not valid for %s", o.command)
	}
	if err := validateGrokLaneCommandOptions(o); err != nil {
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

func parseGrokLaneSeconds(value string, positive bool, flag string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	minimum := 0.0
	message := flag + " must be a non-negative number of seconds"
	if positive {
		minimum = 0.001
		message = flag + " must be at least 0.001 seconds"
	}
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < minimum || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, errors.New(message)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (o grokLaneOptions) hasLaunchOptions() bool {
	return o.nameSet || o.cwdSet || o.modelSet || o.reasoningEffortSet || o.permissionModeSet ||
		o.promptFile != "" || o.notifyExplicit || o.disableNotify || o.persistentSet || o.noAutoArchiveSet || o.autoArchiveCustom ||
		o.allowDuplicateName || o.stdinMarker
}

func validateGrokLaneCommandOptions(o grokLaneOptions) error {
	invalid := func(flag string, set bool) error {
		if set {
			return fmt.Errorf("%s is not valid for %s", flag, o.command)
		}
		return nil
	}
	checks := [][2]any{}
	switch o.command {
	case "run", "start":
		checks = append(checks, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	case "resume":
		checks = append(checks, [2]any{"--name", o.nameSet}, [2]any{"--cd", o.cwdSet}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	case "wait":
		checks = append(checks, [2]any{"launch options", o.hasLaunchOptions()}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	case "list":
		checks = append(checks, [2]any{"launch options", o.hasLaunchOptions()}, [2]any{"--timeout", o.timeoutSet}, [2]any{"--json", o.json})
	case "doctor":
		checks = append(checks, [2]any{"launch options", o.hasLaunchOptions()}, [2]any{"--timeout", o.timeoutSet}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine})
	default:
		checks = append(checks, [2]any{"launch options", o.hasLaunchOptions()}, [2]any{"--timeout", o.timeoutSet}, [2]any{"--all", o.all}, [2]any{"--mine", o.mine}, [2]any{"--json", o.json})
	}
	for _, check := range checks {
		if err := invalid(check[0].(string), check[1].(bool)); err != nil {
			return err
		}
	}
	return nil
}

func runGrokLaneCommand(argv []string) int {
	o, err := parseGrokLaneArgs(argv)
	if err != nil {
		return reportGrokLaneError(err)
	}
	if o.help {
		fmt.Print(grokLaneUsage())
		return 0
	}
	if err := reconcileGrokLaneManagers(resolveNativePaths()); err != nil {
		return reportGrokLaneError(err)
	}
	o = withGrokLaneLaunchContext(o)
	var code int
	switch o.command {
	case "run":
		code, err = startGrokLane(o, true)
	case "start":
		code, err = startGrokLane(o, false)
	case "resume":
		code, err = resumeGrokLane(o)
	case "wait":
		code, err = waitGrokLane(o)
	case "status":
		code, err = statusGrokLane(o)
	case "interrupt":
		code, err = interruptGrokLane(o)
	case "archive":
		code, err = archiveGrokLane(o)
	case "list":
		code, err = listGrokLanes(o)
	case "doctor":
		code, err = doctorGrokLane()
	}
	if err != nil {
		return reportGrokLaneError(err)
	}
	return code
}

func reportGrokLaneError(err error) int {
	_ = emitLane(map[string]any{"type": "error", "message": err.Error(), "timeout": errors.Is(err, context.DeadlineExceeded)})
	fmt.Fprintf(os.Stderr, "grok-peer-lane: %v\n", err)
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return 1
}

func withGrokLaneLaunchContext(o grokLaneOptions) grokLaneOptions {
	listMine := o.command == "list" && o.mine
	if (!containsString([]string{"run", "start", "resume"}, o.command) && !listMine) || (o.persistent && !listMine) {
		return o
	}
	if owner, ok := inferPeerParent(resolveNativePaths(), os.Getpid()); ok {
		o.ownerPID, o.ownerProcStart, o.ownerSessionID = owner.PID, owner.ProcStart, owner.SessionID
		if !listMine && !o.disableNotify && !o.notifyExplicit {
			o.notifyTarget = "session:" + owner.SessionID
		}
	}
	return o
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
	entries, _ := os.ReadDir(directory)
	states := []grokLaneState{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var state grokLaneState
		body, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // bridge-owned state directory.
		if err != nil || json.Unmarshal(body, &state) != nil || state.Type != "grok-peer-lane" || entry.Name() != sessionKey(state.SessionID)+".json" {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].CreatedAt > states[j].CreatedAt })
	return states
}

func resolveGrokLaneState(paths nativePaths, target string) (grokLaneState, error) {
	target = strings.TrimSpace(target)
	byName := []grokLaneState{}
	for _, state := range readGrokLaneStates(paths) {
		if state.SessionID == target {
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
		active := []grokLaneState{}
		for _, state := range byName {
			if state.Status != "archived" {
				active = append(active, state)
			}
		}
		if len(active) == 1 {
			return active[0], nil
		}
		return grokLaneState{}, fmt.Errorf("grok lane name %q is ambiguous; use a session ID", target)
	}
	return grokLaneState{}, fmt.Errorf("no Grok lane matching %q", target)
}

//nolint:gocyclo // Collision checks intentionally cover all three lane products and live peers.
func assertGrokLaneNameAvailable(paths nativePaths, name string, allowDuplicate bool, sessionID string) error {
	if allowDuplicate {
		return nil
	}
	for _, state := range readGrokLaneStates(paths) {
		if state.SessionID != sessionID && state.Status != "archived" && strings.EqualFold(state.Name, name) {
			return fmt.Errorf("grok lane name %q already belongs to session %s", name, state.SessionID)
		}
	}
	for _, state := range readClaudeLaneStates(paths) {
		if state.Status != "archived" && strings.EqualFold(state.Name, name) {
			return fmt.Errorf("lane name %q already belongs to Claude session %s", name, state.SessionID)
		}
	}
	for _, state := range readLaneStates(paths) {
		if state.Status != "archived" && strings.EqualFold(state.Name, name) {
			return fmt.Errorf("lane name %q already belongs to Codex thread %s", name, state.ThreadID)
		}
	}
	peers, err := listNativePeerSessions(paths)
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if peer.SessionID != sessionID && strings.EqualFold(peer.Name, name) {
			return fmt.Errorf("live peer name %q already exists; choose a unique name or pass --allow-duplicate-name", name)
		}
	}
	return nil
}

func newGrokLaneTurn(prompt string, timeout time.Duration) grokLaneTurn {
	now := time.Now().UnixMilli()
	return grokLaneTurn{ID: randomID(), Prompt: prompt, Status: "queued", CreatedAt: now, TimeoutMS: timeout.Milliseconds()}
}

func startGrokLane(o grokLaneOptions, wait bool) (int, error) {
	if err := validateLaneOwner(o.persistent, o.ownerPID, o.ownerProcStart); err != nil {
		return 1, err
	}
	prompt, err := readLanePrompt(laneOptions{promptFile: o.promptFile})
	if err != nil {
		return 1, err
	}
	paths := resolveNativePaths()
	name := sanitizeName(o.name)
	nameLock, err := lockLaneNames(paths)
	if err != nil {
		return 1, err
	}
	defer func() {
		if nameLock != nil {
			unlockLaneStateFile(nameLock)
		}
	}()
	if err := assertGrokLaneNameAvailable(paths, name, o.allowDuplicateName, ""); err != nil {
		return 1, err
	}
	cwd, err := canonicalGrokLaneDirectory(o.cwd)
	if err != nil {
		return 1, err
	}
	sessionID := randomID()
	launchToken := randomID() + randomID()
	hostPaths := grokRuntimePaths(paths.runtimeDir, os.Getuid(), launchToken)
	turn := newGrokLaneTurn(prompt, o.timeout)
	now := time.Now().UnixMilli()
	state := grokLaneState{
		Type: "grok-peer-lane", Name: name, SessionID: sessionID, Cwd: cwd, Status: "starting",
		ControlSocket:   hostPaths.ControlSocket,
		ManagerLog:      filepath.Join(profileDataRoot(paths), "grok-lane-logs", sessionKey(sessionID)+".log"),
		LaunchTokenHash: grokTokenHash(launchToken), RuntimeDir: paths.runtimeDir,
		OwnerPID: o.ownerPID, OwnerProcStart: o.ownerProcStart, OwnerSessionID: o.ownerSessionID,
		NotifyTarget: o.notifyTarget,
		Persistent:   o.persistent, AutoArchive: o.autoArchive, AutoArchiveDelayMS: o.autoArchiveDelay.Milliseconds(),
		PermissionMode: o.permissionMode, Model: o.model, ReasoningEffort: o.reasoningEffort,
		Turns: []grokLaneTurn{turn}, TurnID: turn.ID, LatestTurnID: turn.ID, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeGrokLaneState(paths, state); err != nil {
		return 1, err
	}
	unlockLaneStateFile(nameLock)
	nameLock = nil
	managerPID, managerProcStart, err := spawnGrokLaneManager(state, launchToken)
	if err != nil {
		if removeErr := os.Remove(grokLaneStatePath(paths, sessionID)); removeErr != nil && !os.IsNotExist(removeErr) {
			return 1, fmt.Errorf("%w; remove failed startup state: %w", err, removeErr)
		}
		return 1, err
	}
	ready, err := waitGrokLaneReady(paths, sessionID, managerPID, managerProcStart, grokLaneManagerReadyTimeout)
	if err != nil {
		stopExactGrokLaneManager(managerPID, managerProcStart)
		if archiveErr := forceArchiveGrokLane(paths, sessionID, "manager readiness failed"); archiveErr != nil {
			return 1, fmt.Errorf("%w; archive failed startup: %w", err, archiveErr)
		}
		return 1, err
	}
	_ = emitLane(map[string]any{"type": "thread.started", "thread_id": sessionID, "session_id": sessionID})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": sessionID, "turn_id": turn.ID})
	if err := emitGrokLaneReady(ready); err != nil {
		return 1, err
	}
	if !wait {
		return 0, nil
	}
	return waitGrokLane(grokLaneOptions{target: sessionID, timeout: laneCollectionBound(o.timeout)})
}

func canonicalGrokLaneDirectory(value string) (string, error) {
	cwd, err := canonicalLaunchDirectory(value)
	if err != nil {
		return "", fmt.Errorf("resolve Grok lane cwd: %w", err)
	}
	return cwd, nil
}

func spawnGrokLaneManager(state grokLaneState, launchToken string) (int, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	command := exec.Command(executable, "grok-lane-manager", "--session-id", state.SessionID) //nolint:gosec // current installed runtime.
	if err := os.MkdirAll(filepath.Dir(state.ManagerLog), 0o700); err != nil {
		return 0, "", err
	}
	logFile, err := os.OpenFile(state.ManagerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = logFile.Close() }()
	command.Stdin, command.Stdout, command.Stderr = nil, logFile, logFile
	command.Env = grokLaneManagerEnvironment(os.Environ(), launchToken, state.SessionID)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, "", err
	}
	pid := command.Process.Pid
	procStart, err := captureProcessStart(pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return 0, "", fmt.Errorf("capture Grok lane manager identity: %w", err)
	}
	_ = command.Process.Release()
	return pid, procStart, nil
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
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := readGrokLaneState(paths, sessionID)
		if err == nil && state.ManagerPID == managerPID && state.Status != "starting" &&
			state.MessagingSocket != "" && probeUnixSocket(state.MessagingSocket, 200*time.Millisecond) {
			return state, nil
		}
		if cleanupProcessIdentityStatus(managerPID, managerProcStart).Status == processIdentityStale {
			return grokLaneState{}, errors.New("grok lane manager exited during startup; inspect its private manager log")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return grokLaneState{}, errors.New("timed out starting Grok lane manager; inspect its private manager log")
}

func emitGrokLaneReady(state grokLaneState) error {
	return emitLane(map[string]any{
		"type": "lane.ready", "contract_version": grokLaneContractVersion, "product": "grok",
		"name": state.Name, "thread_id": state.SessionID, "session_id": state.SessionID,
		"grok_session_id": emptyStringAsNil(state.GrokSessionID),
		"turn_id":         state.TurnID, "cwd": state.Cwd, "address": encodeNativeAddress(state.MessagingSocket),
		"owner_session_id": emptyStringAsNil(state.OwnerSessionID), "persistent": state.Persistent,
		"notify_target": emptyStringAsNil(state.NotifyTarget),
		"auto_archive":  state.AutoArchive, "auto_archive_after_seconds": float64(state.AutoArchiveDelayMS) / 1000,
	})
}

func resumeGrokLane(o grokLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveGrokLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	desiredPersistent := state.Persistent || o.persistentSet
	if o.notifyExplicit && !desiredPersistent {
		return 1, errors.New("--notify requires a persistent lane; pass --persistent to promote this lane")
	}
	if err := validateLaneOwner(desiredPersistent, o.ownerPID, o.ownerProcStart); err != nil {
		return 1, err
	}
	prompt, err := readLanePrompt(laneOptions{promptFile: o.promptFile})
	if err != nil {
		return 1, err
	}
	if debt := firstGrokLaneDebt(state); debt != "" {
		return 1, fmt.Errorf("collect outstanding Grok lane turn %s before resume", debt)
	}
	turn := newGrokLaneTurn(prompt, o.timeout)
	if grokLaneManagerLive(state) {
		return resumeLiveGrokLane(paths, state, turn, o, desiredPersistent)
	}
	return resumeArchivedGrokLane(paths, state, turn, o, desiredPersistent)
}

func resumeLiveGrokLane(paths nativePaths, state grokLaneState, turn grokLaneTurn, o grokLaneOptions, persistent bool) (int, error) {
	request := map[string]any{
		"action": "resume", "sessionId": state.SessionID, "turn": turn,
		"persistent": persistent, "ownerPid": o.ownerPID, "ownerProcStart": o.ownerProcStart,
		"ownerSessionId": o.ownerSessionID,
	}
	switch {
	case o.notifyExplicit:
		request["notifySet"], request["notifyTarget"] = true, o.notifyTarget
	case o.disableNotify:
		request["notifySet"], request["notifyTarget"] = true, ""
	case !persistent:
		request["notifySet"], request["notifyTarget"] = true, o.notifyTarget
	}
	if o.autoArchiveCustom {
		request["autoArchive"] = true
		request["autoArchiveDelayMs"] = o.autoArchiveDelay.Milliseconds()
	}
	if o.noAutoArchiveSet {
		request["autoArchive"] = false
	}
	if _, err := requestControl(state.ControlSocket, request, 10*time.Second); err != nil {
		return 1, err
	}
	_ = emitLane(map[string]any{"type": "thread.resumed", "thread_id": state.SessionID, "session_id": state.SessionID})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": state.SessionID, "turn_id": turn.ID})
	if ready, readErr := readGrokLaneState(paths, state.SessionID); readErr == nil {
		if err := emitGrokLaneReady(ready); err != nil {
			return 1, err
		}
	}
	return waitGrokLane(grokLaneOptions{target: state.SessionID, timeout: laneCollectionBound(o.timeout)})
}

//nolint:gocyclo // Archived resume is one guarded state/ownership transaction with explicit rollback.
func resumeArchivedGrokLane(paths nativePaths, state grokLaneState, turn grokLaneTurn, o grokLaneOptions, persistent bool) (int, error) {
	if state.Status != "archived" {
		if err := forceArchiveGrokLane(paths, state.SessionID, "stale manager before resume"); err != nil {
			return 1, err
		}
	}
	nameLock, err := lockLaneNames(paths)
	if err != nil {
		return 1, err
	}
	defer func() {
		if nameLock != nil {
			unlockLaneStateFile(nameLock)
		}
	}()
	state, err = readGrokLaneState(paths, state.SessionID)
	if err != nil {
		return 1, err
	}
	if err := assertGrokLaneNameAvailable(paths, state.Name, o.allowDuplicateName, state.SessionID); err != nil {
		return 1, err
	}
	lifecycle, err := lockLaneLifecycle(paths, "grok-"+state.SessionID)
	if err != nil {
		return 1, err
	}
	unlocked := false
	defer func() {
		if !unlocked {
			unlockLaneLifecycle(lifecycle)
		}
	}()
	latest, err := readGrokLaneState(paths, state.SessionID)
	if err != nil {
		return 1, err
	}
	if latest.Status != "archived" {
		return 1, errors.New("grok lane changed lifecycle while resuming")
	}
	if debt := firstGrokLaneDebt(latest); debt != "" {
		return 1, fmt.Errorf("collect outstanding Grok lane turn %s before resume", debt)
	}
	original := cloneGrokLaneState(latest)
	state = latest
	launchToken := randomID() + randomID()
	startupID := randomID()
	hostPaths := grokRuntimePaths(paths.runtimeDir, os.Getuid(), launchToken)
	state.Status, state.ControlSocket, state.LaunchTokenHash, state.RuntimeDir = "starting", hostPaths.ControlSocket, grokTokenHash(launchToken), paths.runtimeDir
	state.ManagerPID, state.ManagerProcStart, state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart, state.WorkerSessionID, state.MessagingSocket = 0, "", 0, "", "", 0, ""
	state.StartupID = startupID
	state.Persistent = persistent
	if persistent {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = 0, "", ""
	} else {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = o.ownerPID, o.ownerProcStart, o.ownerSessionID
	}
	switch {
	case o.notifyExplicit:
		state.NotifyTarget = o.notifyTarget
	case o.disableNotify:
		state.NotifyTarget = ""
	case !persistent:
		state.NotifyTarget = o.notifyTarget
	}
	if o.autoArchiveCustom {
		state.AutoArchive, state.AutoArchiveDelayMS = true, o.autoArchiveDelay.Milliseconds()
	}
	if o.noAutoArchiveSet {
		state.AutoArchive, state.AutoArchiveAt = false, 0
	}
	state.Turns = append(state.Turns, turn)
	state.TurnID, state.LatestTurnID, state.AutoArchiveAt = turn.ID, turn.ID, 0
	if err := writeGrokLaneState(paths, state); err != nil {
		return 1, err
	}
	managerPID, managerProcStart, err := spawnGrokLaneManager(state, launchToken)
	if err != nil {
		unlockLaneLifecycle(lifecycle)
		unlocked = true
		if rollbackErr := rollbackGrokLaneResume(paths, original, startupID); rollbackErr != nil {
			return 1, fmt.Errorf("%w; rollback failed: %w", err, rollbackErr)
		}
		return 1, err
	}
	unlockLaneLifecycle(lifecycle)
	unlocked = true
	unlockLaneStateFile(nameLock)
	nameLock = nil
	ready, err := waitGrokLaneReady(paths, state.SessionID, managerPID, managerProcStart, grokLaneManagerReadyTimeout)
	if err != nil {
		stopExactGrokLaneManager(managerPID, managerProcStart)
		if rollbackErr := rollbackGrokLaneResume(paths, original, startupID); rollbackErr != nil {
			return 1, fmt.Errorf("%w; rollback failed: %w", err, rollbackErr)
		}
		return 1, err
	}
	_ = emitLane(map[string]any{"type": "thread.resumed", "thread_id": state.SessionID, "session_id": state.SessionID})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": state.SessionID, "turn_id": turn.ID})
	if err := emitGrokLaneReady(ready); err != nil {
		return 1, err
	}
	return waitGrokLane(grokLaneOptions{target: state.SessionID, timeout: laneCollectionBound(o.timeout)})
}

func rollbackGrokLaneResume(paths nativePaths, original grokLaneState, startupID string) error {
	lifecycle, err := lockLaneLifecycle(paths, "grok-"+original.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycle)
	latest, err := readGrokLaneState(paths, original.SessionID)
	if err != nil {
		return err
	}
	if latest.StartupID != startupID {
		return nil
	}
	if processIdentityMayBeLive(latest.ManagerPID, latest.ManagerProcStart) {
		return errors.New("replacement Grok lane manager still owns failed resume")
	}
	registryGuard, cleanupRoots, err := grokLaneCleanupRoots(latest, true)
	if err != nil {
		return err
	}
	defer registryGuard.close()
	if err := stopGrokTaggedProcesses(latest.LaunchTokenHash, 0, cleanupRoots...); err != nil {
		return err
	}
	stopStaleGrokLaneWorker(grokLaneWorkerRoot(latest))
	if err := stopGrokProcessSessionStrong(latest.WorkerSessionID, latest.WorkerProcStart, latest.WorkerStrongStart, 0); err != nil {
		return err
	}
	if err := registryGuard.removeArtifacts(); err != nil {
		return err
	}
	if err := cleanupGrokLaneOwnedFiles(paths, latest, 0, cleanupRoots...); err != nil {
		return err
	}
	return writeGrokLaneState(paths, original)
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

func laneCollectionBound(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return timeout + 5*time.Second
}

func grokLaneManagerLive(state grokLaneState) bool {
	return state.ManagerPID > 1 && state.ManagerProcStart != "" && exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) &&
		state.ControlSocket != "" && probeUnixSocket(state.ControlSocket, 250*time.Millisecond)
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

func statusGrokLane(o grokLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveGrokLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	if state.ManagerPID <= 1 || cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart).Status == processIdentityStale {
		if err := reconcileGrokLaneManager(paths, state, time.Now().UnixMilli()); err != nil {
			return 1, err
		}
		state, err = readGrokLaneState(paths, state.SessionID)
		if err != nil {
			return 1, err
		}
	}
	return 0, emitLane(grokLaneStatusEvent(state))
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

func listGrokLanes(o grokLaneOptions) (int, error) {
	if o.mine && !validLaneOwner(o.ownerPID, o.ownerProcStart) {
		return 1, errors.New("cannot establish the current orchestrator identity for --mine")
	}
	rows := []map[string]any{}
	for _, state := range readGrokLaneStates(resolveNativePaths()) {
		if !o.all && state.Status == "archived" {
			continue
		}
		if o.mine && (state.Persistent || !sameLaneOwner(state.OwnerPID, state.OwnerProcStart, o.ownerPID, o.ownerProcStart)) {
			continue
		}
		row := grokLaneStatusEvent(state)
		delete(row, "type")
		rows = append(rows, row)
	}
	return 0, emitLane(map[string]any{"type": "lane.list", "product": "grok", "contract_version": grokLaneContractVersion, "lanes": rows})
}

func interruptGrokLane(o grokLaneOptions) (int, error) {
	state, err := resolveGrokLaneState(resolveNativePaths(), o.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" || !grokLaneManagerLive(state) {
		return 1, fmt.Errorf("grok lane %s is not live", state.SessionID)
	}
	response, err := requestControl(state.ControlSocket, map[string]any{"action": "interrupt", "sessionId": state.SessionID}, 5*time.Second)
	if err != nil {
		return 1, err
	}
	return 0, emitLane(map[string]any{"type": "turn.interrupted", "thread_id": state.SessionID, "turn_id": response["turnId"]})
}

func archiveGrokLane(o grokLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveGrokLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" {
		if grokLaneCleanupComplete(paths, state) && !grokLaneHasUnsentNotices(state) {
			return 0, emitLane(map[string]any{"type": "lane.archived", "product": "grok", "name": state.Name, "thread_id": state.SessionID, "already_archived": true, "dropped_notices": 0})
		}
		if err := forceArchiveGrokLane(paths, state.SessionID, "explicit archive: reconcile archived Grok lane residue"); err != nil {
			return 1, err
		}
		latest, _ := readGrokLaneState(paths, state.SessionID)
		return 0, emitLane(map[string]any{"type": "lane.archived", "product": "grok", "name": state.Name, "thread_id": state.SessionID, "dropped_notices": latest.ArchiveDroppedNotices})
	}
	if grokLaneManagerLive(state) {
		if _, err := requestControl(state.ControlSocket, map[string]any{"action": "archive", "sessionId": state.SessionID}, 10*time.Second); err != nil {
			return 1, err
		}
		if err := waitGrokLaneArchived(paths, state.SessionID, 10*time.Second); err != nil {
			return 1, err
		}
	} else if err := forceArchiveGrokLane(paths, state.SessionID, "explicit archive: manager unavailable"); err != nil {
		return 1, err
	}
	latest, _ := readGrokLaneState(paths, state.SessionID)
	return 0, emitLane(map[string]any{"type": "lane.archived", "product": "grok", "name": state.Name, "thread_id": state.SessionID, "dropped_notices": latest.ArchiveDroppedNotices})
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
