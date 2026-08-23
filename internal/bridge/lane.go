package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/antst/agent-sessions/internal/federator"
)

type laneOptions struct {
	command            string
	name               string
	cwd                string
	model              string
	effort             string
	sandbox            string
	approvalPolicy     string
	web                *bool
	configs            []string
	timeout            time.Duration
	promptFile         string
	notifyTarget       string
	notifyExplicit     bool
	disableNotify      bool
	persistent         bool
	persistentSet      bool
	autoArchive        bool
	noAutoArchiveSet   bool
	autoArchiveDelay   time.Duration
	autoArchiveCustom  bool
	ownerPID           int
	ownerProcStart     string
	ownerSessionID     string
	schemaFile         string
	outputSchema       json.RawMessage
	worktree           bool
	target             string
	allowDuplicateName bool
	all                bool
	mine               bool
	help               bool
	groupOptions       laneGroupOptions
}

const laneContractVersion = 2
const defaultLaneAutoArchiveDelay = time.Minute

type laneState struct {
	Type                  string          `json:"type"`
	Name                  string          `json:"name"`
	ThreadID              string          `json:"threadId"`
	SessionID             string          `json:"sessionId"`
	Cwd                   string          `json:"cwd"`
	Socket                string          `json:"socketPath,omitempty"`
	Address               string          `json:"address,omitempty"`
	Status                string          `json:"status"`
	TurnID                string          `json:"turnId,omitempty"`
	LatestTurnID          string          `json:"latestTurnId,omitempty"`
	PendingTurnIDs        []string        `json:"pendingTurnIds,omitempty"`
	PendingQueueVer       int             `json:"pendingQueueVersion,omitempty"`
	CollectedTurnID       string          `json:"collectedTurnId,omitempty"`
	CreatedAt             int64           `json:"createdAt"`
	UpdatedAt             int64           `json:"updatedAt"`
	NotifyTarget          string          `json:"notifyTarget,omitempty"`
	Persistent            bool            `json:"persistent,omitempty"`
	AutoArchive           bool            `json:"autoArchive,omitempty"`
	AutoArchiveDelayMS    int64           `json:"autoArchiveDelayMs,omitempty"`
	AutoArchiveAt         int64           `json:"autoArchiveAt,omitempty"`
	OwnerPID              int             `json:"ownerPid,omitempty"`
	OwnerProcStart        string          `json:"ownerProcStart,omitempty"`
	OwnerSessionID        string          `json:"ownerSessionId,omitempty"`
	OutputSchema          json.RawMessage `json:"outputSchema,omitempty"`
	SchemaAttempts        int             `json:"schemaAttempts,omitempty"`
	SchemaRetryByID       map[string]int  `json:"schemaRetryById,omitempty"`
	WorktreePath          string          `json:"worktreePath,omitempty"`
	OriginalCwd           string          `json:"originalCwd,omitempty"`
	PermissionMode        string          `json:"permissionMode,omitempty"`
	DeadlineAt            int64           `json:"deadlineAt,omitempty"`
	TimedOutTurnID        string          `json:"timedOutTurnId,omitempty"`
	TerminalOutcome       string          `json:"terminalOutcome,omitempty"`
	TerminalTurnID        string          `json:"terminalTurnId,omitempty"`
	Groups                []string        `json:"groups,omitempty"`
	ExplicitGroups        []string        `json:"explicitGroups,omitempty"`
	ParentSessionID       string          `json:"parentSessionId,omitempty"`
	ParentHostID          string          `json:"parentHostId,omitempty"`
	ParentAgentRuntimeDir string          `json:"parentAgentRuntimeDir,omitempty"`
	InheritParentGroups   bool            `json:"inheritParentGroups,omitempty"`
}

func laneAutoArchiveDelay(state laneState) time.Duration {
	if state.AutoArchiveDelayMS > 0 {
		return time.Duration(state.AutoArchiveDelayMS) * time.Millisecond
	}
	return defaultLaneAutoArchiveDelay
}

func laneAutoArchiveDelaySeconds(state laneState) float64 {
	return laneAutoArchiveDelay(state).Seconds()
}

func laneUsage() string {
	return `codex-peer-lane — named, messageable Codex lanes on the shared App Server

Usage:
  codex-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  codex-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  codex-peer-lane resume THREAD_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  codex-peer-lane wait THREAD_OR_NAME [--timeout SECONDS]
  codex-peer-lane status THREAD_OR_NAME
  codex-peer-lane interrupt THREAD_OR_NAME
  codex-peer-lane archive THREAD_OR_NAME
  codex-peer-lane list [--all] [--mine]
  codex-peer-lane doctor [--json]

run waits and emits an exec-compatible JSONL event stream. SIGINT or SIGTERM
asks App Server to interrupt that turn. start returns after the named thread is
registered as a Claude-visible peer; wait can later collect the detached turn.

Policy options are optional pass-throughs; omitted values inherit Codex config:
  -n, --name NAME
  -C, --cd DIR
  -m, --model MODEL
      --effort LEVEL
      --sandbox read-only|workspace-write|danger-full-access
      --approval-policy POLICY
      --web | --no-web
  -c, --config KEY=VALUE       repeatable; dotted keys accepted
      --timeout SECONDS        run/start/resume: durable turn deadline
                               wait: collection-call bound; never interrupts
      --all                    include archived lanes in list
      --mine                   list only lanes owned by this orchestrator
      --prompt-file FILE       otherwise read prompt from stdin
      --persistent             survive the recorded lifecycle owner
      --auto-archive-after S   archive after S seconds (minimum 0.001)
      --no-auto-archive        keep an idle completed lane available
      --notify PEER            persistent lanes: send terminal pointers here
      --no-notify              parent-owned lanes: suppress owner notification
      --schema FILE            constrain and validate the final answer as JSON
      --worktree               create a detached git worktree for this lane
	  --group GROUP           add a child group; repeatable
	  --inherit-groups        also inherit the parent's non-private groups
	  --no-inherit-groups     retain only the mandatory parent anchor
      --json                   accepted for codex-exec compatibility
      --skip-git-repo-check    accepted compatibility no-op

Headless lanes cannot answer interactive approval prompts. Autonomous callers
should normally pass --approval-policy never; the wrapper never chooses it.
`
}

// parseLaneArgs keeps option validation in one switch so wrapper behavior is
// auditable against the public command-line contract.
//
//nolint:gocyclo
func parseLaneArgs(argv []string) (laneOptions, error) {
	options := laneOptions{cwd: mustGetwd(), autoArchive: true, autoArchiveDelay: defaultLaneAutoArchiveDelay}
	if len(argv) == 0 {
		options.help = true
		return options, nil
	}
	for _, argument := range argv {
		if argument == "-h" || argument == "--help" {
			options.help = true
			return options, nil
		}
	}
	options.command = argv[0]
	if !containsString([]string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"}, options.command) {
		return options, fmt.Errorf("unknown command %q", options.command)
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
			options.name = value
		case "-C", "--cd":
			value, err = take()
			options.cwd = value
		case "-m", "--model":
			value, err = take()
			options.model = value
		case "--effort", "--reasoning-effort":
			value, err = take()
			options.effort = value
		case "--sandbox":
			value, err = take()
			options.sandbox = value
		case "--approval-policy":
			value, err = take()
			options.approvalPolicy = value
		case "--timeout":
			value, err = take()
			if err == nil {
				seconds, parseErr := strconv.ParseFloat(value, 64)
				if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
					err = errors.New("--timeout must be a non-negative number of seconds")
				} else {
					options.timeout = time.Duration(seconds * float64(time.Second))
				}
			}
		case "--prompt-file":
			value, err = take()
			options.promptFile = value
		case "--notify":
			value, err = take()
			options.notifyTarget = value
			options.notifyExplicit = true
		case "--no-notify":
			options.disableNotify = true
		case "--persistent":
			options.persistent, options.persistentSet = true, true
		case "--no-auto-archive":
			options.autoArchive, options.noAutoArchiveSet = false, true
		case "--auto-archive-after":
			value, err = take()
			if err == nil {
				seconds, parseErr := strconv.ParseFloat(value, 64)
				if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0.001 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
					err = errors.New("--auto-archive-after must be at least 0.001 seconds")
				} else {
					options.autoArchiveDelay = time.Duration(seconds * float64(time.Second))
					options.autoArchiveCustom = true
				}
			}
		case "--schema":
			value, err = take()
			options.schemaFile = value
		case "--worktree":
			options.worktree = true
		case "--group":
			value, err = take()
			options.groupOptions.groups = append(options.groupOptions.groups, value)
			options.groupOptions.groupsSpecified = true
		case "--inherit-groups":
			options.groupOptions.inheritParentGroups, options.groupOptions.inheritGroupsSpecified = true, true
		case "--no-inherit-groups":
			options.groupOptions.inheritParentGroups, options.groupOptions.inheritGroupsSpecified = false, true
		case "-c", "--config":
			value, err = take()
			options.configs = append(options.configs, value)
		case "--web":
			value := true
			options.web = &value
		case "--no-web":
			value := false
			options.web = &value
		case "--allow-duplicate-name":
			options.allowDuplicateName = true
		case "--all":
			options.all = true
		case "--mine":
			options.mine = true
		case "--json", "--skip-git-repo-check", "-":
			// Compatibility options; JSONL and stdin are already the native behavior.
		default:
			if strings.HasPrefix(argument, "-") {
				return options, fmt.Errorf("unknown option %s", argument)
			}
			positionals = append(positionals, argument)
		}
		if err != nil {
			return options, err
		}
	}
	if options.notifyTarget != "" && options.disableNotify {
		return options, errors.New("--notify and --no-notify cannot be used together")
	}
	if options.autoArchiveCustom && !options.autoArchive {
		return options, errors.New("--auto-archive-after and --no-auto-archive cannot be used together")
	}
	if options.autoArchiveCustom && !containsString([]string{"run", "start", "resume"}, options.command) {
		return options, fmt.Errorf("--auto-archive-after is not valid for %s", options.command)
	}
	if options.notifyExplicit && !options.persistent && options.command != "resume" {
		return options, errors.New("--notify requires --persistent; parent-owned lanes notify their owner automatically")
	}
	if options.mine && options.command != "list" {
		return options, fmt.Errorf("--mine is not valid for %s", options.command)
	}
	if err := validateLaneGroupCommand(options.command, options.groupOptions); err != nil {
		return options, err
	}
	switch options.command {
	case "run", "start":
		if strings.TrimSpace(options.name) == "" {
			return options, fmt.Errorf("%s requires --name", options.command)
		}
		if len(positionals) != 0 {
			return options, fmt.Errorf("%s does not accept a prompt on argv; use stdin or --prompt-file", options.command)
		}
		if options.sandbox != "" && !containsString([]string{"read-only", "workspace-write", "danger-full-access"}, options.sandbox) {
			return options, fmt.Errorf("unsupported sandbox %q", options.sandbox)
		}
	case "resume":
		if len(positionals) != 1 {
			return options, errors.New("resume requires exactly one THREAD_OR_NAME")
		}
		if options.worktree {
			return options, errors.New("resume cannot create a new worktree; it reuses the lane's existing cwd")
		}
		options.target = positionals[0]
	case "list", "doctor":
		if len(positionals) != 0 {
			return options, fmt.Errorf("%s does not accept positional arguments", options.command)
		}
	default:
		if len(positionals) != 1 {
			return options, fmt.Errorf("%s requires exactly one THREAD_OR_NAME", options.command)
		}
		options.target = positionals[0]
	}
	return options, nil
}

func runLaneCommand(argv []string) int {
	options, err := parseLaneArgs(argv)
	if err != nil {
		_ = emitLane(map[string]any{"type": "error", "message": err.Error()})
		fmt.Fprintf(os.Stderr, "codex-peer-lane: %v\n", err)
		return 1
	}
	if options.help {
		fmt.Print(laneUsage())
		return 0
	}
	options = withLaneLaunchContext(options)
	var code int
	switch options.command {
	case "run":
		code, err = startLaneNative(options, true)
	case "start":
		code, err = startLaneNative(options, false)
	case "resume":
		code, err = resumeLaneNative(options)
	case "wait":
		code, err = waitLaneNative(options)
	case "status":
		code, err = statusLaneNative(options)
	case "interrupt":
		code, err = interruptLaneNative(options)
	case "archive":
		code, err = archiveLaneNative(options)
	case "list":
		code, err = listLanesNative(options)
	case "doctor":
		code, err = doctorLaneNative()
	}
	if err != nil {
		_ = emitLane(map[string]any{"type": "error", "message": err.Error(), "timeout": errors.Is(err, context.DeadlineExceeded)})
		fmt.Fprintf(os.Stderr, "codex-peer-lane: %v\n", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return 124
		}
		return 1
	}
	return code
}

func withLaneLaunchContext(options laneOptions) laneOptions {
	listMine := options.command == "list" && options.mine
	if !containsString([]string{"run", "start", "resume"}, options.command) && !listMine {
		return options
	}
	owner := inferPeerParent(resolveNativePaths(), os.Getpid())
	return withLaneResolvedParent(options, owner)
}

func withLaneResolvedParent(options laneOptions, owner laneOwner) laneOptions {
	listMine := options.command == "list" && options.mine
	options.groupOptions = applyAgentParentContext(options.groupOptions, &owner)
	peerOwner := owner.SessionID != ""
	if listMine {
		if peerOwner {
			options.ownerPID, options.ownerProcStart, options.ownerSessionID = owner.PID, owner.ProcStart, owner.SessionID
		}
		return options
	}
	if peerOwner {
		options.groupOptions.parentSessionID = owner.SessionID
		if !options.persistent {
			options.ownerPID = owner.PID
			options.ownerProcStart = owner.ProcStart
			options.ownerSessionID = owner.SessionID
			if !options.disableNotify {
				options.notifyTarget = "session:" + owner.SessionID
			}
		}
	}
	return options
}

//nolint:gocyclo // Startup is a linear sequence with explicit cleanup branches.
func startLaneNative(options laneOptions, wait bool) (int, error) {
	if err := validateLaneOwner(options.persistent, options.ownerPID, options.ownerProcStart); err != nil {
		return 1, err
	}
	prompt, err := readLanePrompt(options)
	if err != nil {
		return 1, err
	}
	name := sanitizeName(options.name)
	paths := resolveNativePaths()
	if options.schemaFile != "" {
		options.outputSchema, err = readLaneOutputSchema(options.schemaFile)
		if err != nil {
			return 1, err
		}
	}
	originalCwd := absolutePath(options.cwd)
	worktreePath := ""
	setupReady := false
	var client *appServerClient
	var state laneState
	defer func() {
		if setupReady {
			return
		}
		if worktreePath != "" {
			_ = removeLaneWorktree(originalCwd, worktreePath)
		}
	}()
	if options.worktree {
		worktreePath, err = createLaneWorktree(paths, name, originalCwd)
		if err != nil {
			return 1, err
		}
		options.cwd = worktreePath
	}
	var nameLock *os.File
	unlockNameLock := func() {
		if nameLock == nil {
			return
		}
		_ = syscall.Flock(int(nameLock.Fd()), syscall.LOCK_UN)
		_ = nameLock.Close()
		nameLock = nil
	}
	// Names are selectors inside the caller's visible groups, not global
	// ownership keys. Exact lane IDs remain the lifecycle authority, and an
	// ambiguous visible name fails at routing/resolution time.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, err = dialAppServer(ctx, paths.appServerSock)
	cancel()
	if err != nil {
		return 1, err
	}
	defer client.close()
	// Registered after client.close so rollback executes first (defer is LIFO)
	// and can still archive a persistent thread created by a partial setup.
	defer func() {
		if setupReady || state.ThreadID == "" {
			return
		}
		state.Status = "failed"
		state.TerminalOutcome = "failed"
		state.TerminalTurnID = state.TurnID
		if state.TurnID != "" {
			_ = requestWithTimeout(client, 10*time.Second, "turn/interrupt", map[string]any{
				"threadId": state.ThreadID, "turnId": state.TurnID,
			}, nil)
		}
		archiveErr := requestWithTimeout(client, 10*time.Second, "thread/archive", map[string]any{"threadId": state.ThreadID}, nil)
		if archiveErr != nil && unmaterializedLaneRolloutMissing(state, archiveErr) {
			archiveErr = deletePreparedThread(client, state.ThreadID)
		}
		if archiveErr != nil {
			// Never make a failed rollback invisible. Retaining the requested name
			// and thread id lets the caller retry archive through the lane API.
			_ = recordLaneState(paths, state)
			return
		}
		if retireErr := markRetiredThread(paths, state.ThreadID); retireErr != nil {
			state.Status = "archived"
			_ = recordLaneState(paths, state)
			return
		}
		_, _ = requestControl(paths.supervisorSock, map[string]any{"action": "retire", "sessionId": state.ThreadID}, 2*time.Second)
		_ = os.Remove(laneStatePath(paths, state.ThreadID))
	}()
	params, err := laneThreadStartParams(options)
	if err != nil {
		return 1, err
	}
	var started struct {
		Thread appThread `json:"thread"`
	}
	if err := requestWithTimeout(client, 60*time.Second, "thread/start", params, &started); err != nil {
		return 1, err
	}
	thread := started.Thread
	state = laneState{
		Type: "codex-peer-lane", Name: name, ThreadID: thread.ID,
		SessionID: defaultString(thread.SessionID, thread.ID), Cwd: defaultString(thread.Cwd, absolutePath(options.cwd)),
		Status: "starting", CreatedAt: time.Now().UnixMilli(), NotifyTarget: options.notifyTarget,
		Persistent: options.persistent, OwnerPID: options.ownerPID, OwnerProcStart: options.ownerProcStart, OwnerSessionID: options.ownerSessionID,
		AutoArchive: options.autoArchive, AutoArchiveDelayMS: options.autoArchiveDelay.Milliseconds(),
		OutputSchema: options.outputSchema, WorktreePath: worktreePath,
		PermissionMode: permissionModeForApprovalPolicy(options.approvalPolicy),
	}
	groupState, alwaysApprove, err := resolveLaneGroupState(
		state.SessionID, "codex", options.groupOptions,
		permissionModeForApprovalPolicy(options.approvalPolicy) == "bypassPermissions", true,
	)
	if err != nil {
		return 1, fmt.Errorf("resolve lane groups: %w", err)
	}
	if alwaysApprove {
		options.approvalPolicy, options.sandbox, state.PermissionMode = "never", "danger-full-access", "bypassPermissions"
	}
	state.Groups, state.ExplicitGroups = groupState.Groups, groupState.ExplicitGroups
	state.ParentSessionID, state.InheritParentGroups = groupState.ParentSessionID, groupState.InheritParentGroups
	state.ParentHostID = groupState.ParentHostID
	state.ParentAgentRuntimeDir = groupState.ParentAgentRuntimeDir
	if worktreePath != "" {
		state.OriginalCwd = originalCwd
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/name/set", map[string]any{
		"threadId": thread.ID, "name": name,
	}, nil); err != nil {
		return 1, err
	}
	if err := recordLaneState(paths, state); err != nil {
		return 1, err
	}
	var turnStarted struct {
		Turn appTurn `json:"turn"`
	}
	if err := requestWithTimeout(client, 60*time.Second, "turn/start", laneTurnStartParams(options, state.ThreadID, prompt), &turnStarted); err != nil {
		return 1, err
	}
	state.TurnID = turnStarted.Turn.ID
	state.LatestTurnID = state.TurnID
	state.PendingTurnIDs = []string{state.TurnID}
	state.PendingQueueVer = 1
	state.Status = normalizeStatus(turnStarted.Turn.Status)
	if state.Status == "" {
		state.Status = "in_progress"
	}
	setLaneDeadline(&state, options.timeout)
	if err := recordLaneState(paths, state); err != nil {
		return 1, err
	}
	registerRequest := map[string]any{
		"action": "register", "sessionId": thread.ID, "cwd": state.Cwd,
		"name": name, "nameSource": "lane",
		"permissionMode": permissionModeForApprovalPolicy(options.approvalPolicy),
		"status":         "busy",
	}
	if laneAgentConfigured() {
		registerRequest["agentRuntimeDir"] = laneAgentRuntimeDir()
	}
	registered, err := requestControl(paths.supervisorSock, registerRequest, 15*time.Second)
	if err != nil {
		return 1, err
	}
	stateMap, _ := registered["state"].(map[string]any)
	state.Socket = stringValue(stateMap["socketPath"])
	if state.Socket != "" {
		state.Address = "uds:" + state.Socket
	}
	if err := recordLaneState(paths, state); err != nil {
		return 1, err
	}
	// Name serialization protects uniqueness and registration only. Holding it
	// while a model turn runs would make otherwise independent lanes serial.
	unlockNameLock()
	setupReady = true
	_ = emitLane(map[string]any{
		"type": "thread.started", "thread_id": state.ThreadID, "session_id": state.SessionID, "peer_name": name,
		"cwd": state.Cwd, "worktree_path": emptyStringAsNil(state.WorktreePath),
	})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": state.ThreadID, "turn_id": state.TurnID})
	// Ready means both the intended initial turn and the durable lane state
	// exist.  Publishing earlier lets an inbound peer race a second turn.
	_ = emitLane(map[string]any{
		"type": "lane.ready", "name": name, "thread_id": state.ThreadID, "session_id": state.SessionID, "address": state.Address,
		"turn_id": state.TurnID, "cwd": state.Cwd, "worktree_path": emptyStringAsNil(state.WorktreePath),
		"notify_target": emptyStringAsNil(state.NotifyTarget), "persistent": state.Persistent,
		"auto_archive": state.AutoArchive, "auto_archive_after_seconds": laneAutoArchiveDelaySeconds(state),
		"owner_session_id": emptyStringAsNil(state.OwnerSessionID),
		"contract_version": laneContractVersion,
	})
	if !wait {
		return 0, nil
	}
	status, code, collected, err := waitForLaneTurn(client, &state, options.timeout)
	state.Status = status
	if collected {
		state.CollectedTurnID = state.TurnID
	}
	if recordErr := persistLaneWaitState(paths, state, collected); recordErr != nil && err == nil {
		return 1, fmt.Errorf("persist lane collection cursor: %w", recordErr)
	}
	return code, err
}

//nolint:gocyclo // Resume deliberately keeps recovery, registration, and turn startup in one transaction-like sequence.
func resumeLaneNative(options laneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveLaneState(paths, options.target)
	if err != nil {
		return 1, err
	}
	desiredPersistent := state.Persistent || options.persistentSet
	if options.notifyExplicit && !desiredPersistent {
		return 1, errors.New("--notify requires a persistent lane; pass --persistent to promote this lane")
	}
	if err := validateLaneOwner(desiredPersistent, options.ownerPID, options.ownerProcStart); err != nil {
		return 1, err
	}
	prompt, err := readLanePrompt(options)
	if err != nil {
		return 1, err
	}
	lifecycleLock, err := lockLaneLifecycle(paths, state.ThreadID)
	if err != nil {
		return 1, err
	}
	unlockLifecycle := func() {
		if lifecycleLock == nil {
			return
		}
		unlockLaneLifecycle(lifecycleLock)
		lifecycleLock = nil
	}
	defer unlockLifecycle()
	if state.Type == "codex-peer-lane" {
		if latest, readErr := readLaneStateFile(paths, state.ThreadID); readErr == nil {
			state = latest
		} else if !os.IsNotExist(readErr) {
			return 1, readErr
		}
	}
	originalState := state
	if options.schemaFile != "" {
		options.outputSchema, err = readLaneOutputSchema(options.schemaFile)
		if err != nil {
			return 1, err
		}
		state.OutputSchema = options.outputSchema
	} else {
		options.outputSchema = state.OutputSchema
	}
	if options.approvalPolicy != "" {
		state.PermissionMode = permissionModeForApprovalPolicy(options.approvalPolicy)
	}
	client, err := connectLaneClient(paths)
	if err != nil {
		return 1, err
	}
	defer client.close()
	wasRetired := isRetiredThreadNative(paths, state.ThreadID)
	unarchived := false
	resumeReady := false
	stateMutated := false
	defer func() {
		needsRollback := stateMutated || (wasRetired && unarchived)
		if resumeReady || !needsRollback {
			return
		}
		stateRestored := false
		if stateMutated {
			var restoreErr error
			stateRestored, restoreErr = restoreLaneAfterFailedResume(paths, originalState, state)
			if restoreErr != nil || !stateRestored {
				// A turn/start response can be lost after App Server accepted it.
				// Preserve any turn already adopted by the notification path rather
				// than rolling it back into an uncollectable transcript entry.
				return
			}
		}
		if wasRetired && unarchived {
			_ = requestWithTimeout(client, 10*time.Second, "thread/archive", map[string]any{"threadId": state.ThreadID}, nil)
			_ = markRetiredThread(paths, state.ThreadID)
			originalState.Status = "archived"
			_, _ = requestControl(paths.supervisorSock, map[string]any{"action": "retire", "sessionId": state.ThreadID}, 2*time.Second)
		} else if originalState.Name != "" && originalState.Name != state.Name {
			_ = requestWithTimeout(client, 10*time.Second, "thread/name/set", map[string]any{
				"threadId": state.ThreadID, "name": originalState.Name,
			}, nil)
		}
		if !stateRestored {
			_ = recordLaneState(paths, originalState)
		}
	}()
	if wasRetired {
		if err := requestWithTimeout(client, 30*time.Second, "thread/unarchive", map[string]any{"threadId": state.ThreadID}, nil); err != nil {
			return 1, err
		}
		unarchived = true
		clearRetiredThread(paths, state.ThreadID)
	}
	thread, err := resumeThreadForPeer(client, state.ThreadID)
	if err != nil {
		return 1, err
	}
	name := state.Name
	if options.name != "" {
		name = sanitizeName(options.name)
	} else if name == "" {
		name = defaultString(thread.Name, defaultPeerName(defaultString(thread.Cwd, state.Cwd), state.ThreadID))
	}
	nameLock, err := lockLaneNames(paths)
	if err != nil {
		return 1, err
	}
	unlockNameLock := func() {
		if nameLock == nil {
			return
		}
		_ = syscall.Flock(int(nameLock.Fd()), syscall.LOCK_UN)
		_ = nameLock.Close()
		nameLock = nil
	}
	defer unlockNameLock()
	if thread.Name != name {
		if err := requestWithTimeout(client, 30*time.Second, "thread/name/set", map[string]any{"threadId": state.ThreadID, "name": name}, nil); err != nil {
			return 1, err
		}
	}
	state.Name = name
	state.SessionID = defaultString(thread.SessionID, defaultString(state.SessionID, state.ThreadID))
	state.Cwd = defaultString(thread.Cwd, state.Cwd)
	groupState, alwaysApprove, err := resolveLaneGroupState(
		state.SessionID, "codex", options.groupOptions,
		permissionModeForApprovalPolicy(options.approvalPolicy) == "bypassPermissions", options.approvalPolicy != "",
	)
	if err != nil {
		return 1, fmt.Errorf("resolve lane groups: %w", err)
	}
	if alwaysApprove {
		options.approvalPolicy, options.sandbox, state.PermissionMode = "never", "danger-full-access", "bypassPermissions"
	}
	state.Groups, state.ExplicitGroups = groupState.Groups, groupState.ExplicitGroups
	state.ParentSessionID, state.InheritParentGroups = groupState.ParentSessionID, groupState.InheritParentGroups
	state.ParentHostID = groupState.ParentHostID
	state.ParentAgentRuntimeDir = groupState.ParentAgentRuntimeDir
	applyLaneLifecycleOptions(&state, options)
	state.Status = "starting"
	state.TurnID = ""
	state.LatestTurnID = ""
	state.PendingTurnIDs = nil
	state.PendingQueueVer = 1
	state.CollectedTurnID = ""
	state.SchemaAttempts = 0
	state.SchemaRetryByID = nil
	state.TimedOutTurnID = ""
	state.TerminalOutcome = ""
	state.TerminalTurnID = ""
	state.DeadlineAt = 0
	if err := recordLaneState(paths, state); err != nil {
		return 1, err
	}
	stateMutated = true
	var started struct {
		Turn appTurn `json:"turn"`
	}
	if err := requestWithTimeout(client, 60*time.Second, "turn/start", laneTurnStartParams(options, state.ThreadID, prompt), &started); err != nil {
		return 1, err
	}
	state.TurnID = started.Turn.ID
	state.LatestTurnID = state.TurnID
	state.PendingTurnIDs = []string{state.TurnID}
	state.PendingQueueVer = 1
	state.Status = normalizeStatus(started.Turn.Status)
	if state.Status == "" {
		state.Status = "in_progress"
	}
	setLaneDeadline(&state, options.timeout)
	if err := recordLaneState(paths, state); err != nil {
		return 1, err
	}
	registerRequest := map[string]any{
		"action": "register", "sessionId": state.ThreadID, "cwd": state.Cwd,
		"name": name, "nameSource": "lane", "permissionMode": defaultString(state.PermissionMode, "default"), "status": "busy",
	}
	if laneAgentConfigured() {
		registerRequest["agentRuntimeDir"] = laneAgentRuntimeDir()
	}
	registered, err := requestControl(paths.supervisorSock, registerRequest, 15*time.Second)
	if err != nil {
		return 1, err
	}
	if stateMap, ok := registered["state"].(map[string]any); ok {
		state.Socket = stringValue(stateMap["socketPath"])
		if state.Socket != "" {
			state.Address = "uds:" + state.Socket
		}
	}
	if err := recordLaneState(paths, state); err != nil {
		return 1, err
	}
	resumeReady = true
	unlockLifecycle()
	// The durable registration is complete. Do not serialize the model turn
	// behind the global lane-name reservation lock.
	unlockNameLock()
	_ = emitLane(map[string]any{
		"type": "thread.resumed", "thread_id": state.ThreadID, "session_id": state.SessionID, "peer_name": name,
		"cwd": state.Cwd, "worktree_path": emptyStringAsNil(state.WorktreePath),
	})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": state.ThreadID, "turn_id": state.TurnID})
	_ = emitLane(map[string]any{
		"type": "lane.ready", "name": name, "thread_id": state.ThreadID, "session_id": state.SessionID,
		"address": state.Address, "turn_id": state.TurnID, "resumed": true,
		"cwd": state.Cwd, "worktree_path": emptyStringAsNil(state.WorktreePath),
		"notify_target": emptyStringAsNil(state.NotifyTarget), "persistent": state.Persistent,
		"auto_archive": state.AutoArchive, "auto_archive_after_seconds": laneAutoArchiveDelaySeconds(state),
		"owner_session_id": emptyStringAsNil(state.OwnerSessionID),
		"contract_version": laneContractVersion,
	})
	status, code, collected, err := waitForLaneTurnWithPolicy(client, &state, options.timeout, true, false)
	state.Status = status
	if collected {
		state.CollectedTurnID = state.TurnID
	}
	if recordErr := persistLaneWaitState(paths, state, collected); recordErr != nil && err == nil {
		return 1, fmt.Errorf("persist lane collection cursor: %w", recordErr)
	}
	return code, err
}

func applyLaneLifecycleOptions(state *laneState, options laneOptions) {
	if options.persistentSet {
		state.Persistent = true
	}
	switch {
	case options.notifyExplicit:
		state.NotifyTarget = options.notifyTarget
	case options.disableNotify:
		state.NotifyTarget = ""
	case !state.Persistent:
		state.NotifyTarget = options.notifyTarget
	}
	if options.autoArchiveCustom {
		state.AutoArchive = true
		state.AutoArchiveDelayMS = options.autoArchiveDelay.Milliseconds()
	}
	if options.noAutoArchiveSet {
		state.AutoArchive = false
	}
	state.AutoArchiveAt = 0
	if state.Persistent {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = 0, "", ""
	} else {
		state.OwnerPID = options.ownerPID
		state.OwnerProcStart = options.ownerProcStart
		state.OwnerSessionID = options.ownerSessionID
	}
}

func restoreLaneAfterFailedResume(paths nativePaths, original, observed laneState) (bool, error) {
	lock, err := lockLaneStateFile(paths, original.ThreadID)
	if err != nil {
		return false, err
	}
	defer unlockLaneStateFile(lock)
	latest, err := readLaneStateFile(paths, original.ThreadID)
	if err != nil {
		return false, err
	}
	if latest.Status != "starting" || latest.TurnID != "" || latest.LatestTurnID != "" || len(latest.PendingTurnIDs) != 0 {
		return false, nil
	}
	if observed.TurnID != "" {
		// turn/start returned a concrete turn, but the first state commit failed.
		// Keep that accepted work collectable instead of restoring an older cursor
		// and leaving a live App Server turn untracked.
		return false, writeLaneStateUnlocked(paths, observed)
	}
	return true, writeLaneStateUnlocked(paths, original)
}

func waitLaneNative(options laneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveLaneState(paths, options.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" || isRetiredThreadNative(paths, state.ThreadID) {
		return 1, fmt.Errorf("lane %s is archived; wait cannot recover an uncollected prior turn. resume starts a new follow-up turn; use --no-auto-archive or a longer --auto-archive-after grace to prevent this", state.ThreadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, err := dialAppServer(ctx, paths.appServerSock)
	cancel()
	if err != nil {
		return 1, err
	}
	defer client.close()
	if _, err := resumeThreadForPeer(client, state.ThreadID); err != nil {
		return 1, err
	}
	normalizeLanePendingTurns(&state)
	if len(state.PendingTurnIDs) > 0 {
		state.TurnID = state.PendingTurnIDs[0]
	} else if state.CollectedTurnID == state.TurnID {
		state.TurnID = ""
	}
	status, code, collected, err := waitForLaneTurnWithPolicy(client, &state, options.timeout, false, false)
	if status != "" {
		state.Status = status
	}
	if collected {
		state.CollectedTurnID = state.TurnID
	}
	if recordErr := persistLaneWaitState(paths, state, collected); recordErr != nil && err == nil {
		return 1, fmt.Errorf("persist lane collection cursor: %w", recordErr)
	}
	return code, err
}

func statusLaneNative(options laneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveLaneState(paths, options.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" || isRetiredThreadNative(paths, state.ThreadID) {
		outcome := state.TerminalOutcome
		var exit any
		if outcome != "" {
			exit = laneExitCode(outcome)
		}
		_ = emitLane(map[string]any{
			"type": "lane.status", "name": state.Name, "thread_id": state.ThreadID,
			"session_id": defaultString(state.SessionID, state.ThreadID), "status": "archived", "cwd": state.Cwd,
			"turn_id": emptyStringAsNil(state.TurnID), "turn_status": nil,
			"collected_turn_id":          emptyStringAsNil(state.CollectedTurnID),
			"worktree_path":              emptyStringAsNil(state.WorktreePath),
			"notify_target":              emptyStringAsNil(state.NotifyTarget),
			"persistent":                 state.Persistent,
			"auto_archive":               state.AutoArchive,
			"auto_archive_after_seconds": laneAutoArchiveDelaySeconds(state),
			"auto_archive_at":            nilIfZero(state.AutoArchiveAt),
			"owner_session_id":           emptyStringAsNil(state.OwnerSessionID),
			"outcome":                    emptyStringAsNil(outcome),
			"exit":                       exit,
		})
		return 0, nil
	}
	client, err := connectLaneClient(paths)
	if err != nil {
		return 1, err
	}
	defer client.close()
	thread, err := resumeThreadForPeer(client, state.ThreadID)
	if err != nil {
		return 1, err
	}
	turns, err := listLaneTurns(client, state.ThreadID, "notLoaded")
	if err != nil {
		return 1, err
	}
	var turn *appTurn
	if len(turns) > 0 {
		turn = &turns[0]
	}
	event := map[string]any{
		"type": "lane.status", "name": defaultString(thread.Name, state.Name), "thread_id": thread.ID,
		"session_id": defaultString(thread.SessionID, defaultString(state.SessionID, thread.ID)),
		"status":     statusType(thread.Status), "cwd": thread.Cwd,
		"collected_turn_id":          emptyStringAsNil(state.CollectedTurnID),
		"worktree_path":              emptyStringAsNil(state.WorktreePath),
		"notify_target":              emptyStringAsNil(state.NotifyTarget),
		"persistent":                 state.Persistent,
		"auto_archive":               state.AutoArchive,
		"auto_archive_after_seconds": laneAutoArchiveDelaySeconds(state),
		"auto_archive_at":            nilIfZero(state.AutoArchiveAt),
		"owner_session_id":           emptyStringAsNil(state.OwnerSessionID),
	}
	if turn != nil {
		turnStatus := normalizeStatus(turn.Status)
		event["turn_id"] = turn.ID
		event["turn_status"] = turnStatus
		if statusTerminal(turnStatus) {
			outcome := laneTerminalOutcome(state, turn.ID, turnStatus)
			event["outcome"] = outcome
			event["exit"] = laneExitCode(outcome)
		} else {
			event["outcome"] = nil
			event["exit"] = nil
		}
	} else {
		event["turn_id"] = nil
		event["turn_status"] = nil
		event["outcome"] = nil
		event["exit"] = nil
	}
	_ = emitLane(event)
	return 0, nil
}

func listLanesNative(options laneOptions) (int, error) {
	paths := resolveNativePaths()
	if options.mine && !validLaneOwner(options.ownerPID, options.ownerProcStart) {
		return 1, errors.New("cannot establish the current orchestrator identity for --mine")
	}
	retired := readRetiredThreads(paths)
	states := readLaneStates(paths)
	sort.Slice(states, func(i, j int) bool {
		left := strings.ToLower(states[i].Name) + "\x00" + states[i].ThreadID
		right := strings.ToLower(states[j].Name) + "\x00" + states[j].ThreadID
		return left < right
	})
	lanes := make([]map[string]any, 0, len(states))
	for _, state := range states {
		archived := state.Status == "archived" || retired[state.ThreadID]
		if !includeLaneInList(state, archived, options) {
			continue
		}
		status := defaultString(state.Status, "unknown")
		if archived {
			status = "archived"
		}
		var exit any
		if state.TerminalOutcome != "" {
			exit = laneExitCode(state.TerminalOutcome)
		}
		lanes = append(lanes, map[string]any{
			"name": state.Name, "thread_id": state.ThreadID,
			"session_id": defaultString(state.SessionID, state.ThreadID),
			"cwd":        state.Cwd, "status": status,
			"turn_id": emptyStringAsNil(state.TurnID), "collected_turn_id": emptyStringAsNil(state.CollectedTurnID),
			"worktree_path": emptyStringAsNil(state.WorktreePath), "notify_target": emptyStringAsNil(state.NotifyTarget),
			"persistent": state.Persistent, "owner_session_id": emptyStringAsNil(state.OwnerSessionID),
			"auto_archive": state.AutoArchive, "auto_archive_after_seconds": laneAutoArchiveDelaySeconds(state),
			"auto_archive_at": nilIfZero(state.AutoArchiveAt),
			"outcome":         emptyStringAsNil(state.TerminalOutcome), "exit": exit,
		})
	}
	if err := emitLane(map[string]any{
		"type": "lane.list", "contract_version": laneContractVersion, "lanes": lanes,
	}); err != nil {
		return 1, err
	}
	return 0, nil
}

func includeLaneInList(state laneState, archived bool, options laneOptions) bool {
	if state.Type != "codex-peer-lane" || state.ThreadID == "" || strings.TrimSpace(state.Name) == "" || (archived && !options.all) {
		return false
	}
	return !options.mine || (!state.Persistent && sameLaneOwner(state.OwnerPID, state.OwnerProcStart, options.ownerPID, options.ownerProcStart))
}

func validLaneOwner(pid int, procStart string) bool {
	return exactProcessIdentityMatch(pid, procStart)
}

func validateLaneOwner(persistent bool, pid int, procStart string) error {
	if persistent {
		return nil
	}
	if !exactProcessIdentityMatch(pid, procStart) {
		return errors.New("cannot corroborate a stable lifecycle owner; retry from a live Codex, Claude, Grok, or Qwen session, or use --persistent")
	}
	return nil
}

func sameLaneOwner(leftPID int, leftProcStart string, rightPID int, rightProcStart string) bool {
	return leftPID > 1 && leftPID == rightPID && leftProcStart != "" && leftProcStart == rightProcStart
}

func inferPeerParent(paths nativePaths, startPID int) laneOwner {
	candidates := make([]laneOwner, 0, 4)
	if owner, ok := inferCodexParent(paths, startPID); ok {
		candidates = append(candidates, owner)
	}
	if owner, ok := inferClaudeParent(paths, startPID); ok {
		candidates = append(candidates, owner)
	}
	if owner, ok := inferGrokParent(paths, startPID); ok {
		candidates = append(candidates, owner)
	}
	if owner, ok := inferQwenParent(paths, startPID); ok {
		candidates = append(candidates, owner)
	}
	if len(candidates) == 0 {
		if owner, ok := inferRegisteredPeerParent(startPID, federator.ResolveParentContext); ok {
			candidates = append(candidates, owner)
		}
	}
	if len(candidates) == 0 {
		return laneOwner{}
	}
	deepest := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.PID != deepest.PID && processHasAncestor(candidate.PID, deepest.PID) {
			deepest = candidate
		}
	}
	return deepest
}

func doctorLaneNative() (int, error) {
	paths := resolveNativePaths()
	appServerReachable := probeUnixSocket(paths.appServerSock, 500*time.Millisecond)
	supervisor, supervisorErr := requestControl(paths.supervisorSock, map[string]any{"action": "status"}, 2*time.Second)
	supervisorReachable := supervisorErr == nil
	runtimeVersion := stringValue(supervisor["pluginVersion"])
	if runtimeVersion == "" {
		runtimeVersion = stringValue(readJSONMap(paths.supervisorState)["pluginVersion"])
	}
	executable, _ := os.Executable()
	if err := emitLane(map[string]any{
		"type": "lane.doctor", "contract_version": laneContractVersion,
		"runtime_version": emptyStringAsNil(runtimeVersion), "runtime_path": executable,
		"appserver_reachable": appServerReachable, "appserver_socket": paths.appServerSock,
		"supervisor_reachable": supervisorReachable, "supervisor_socket": paths.supervisorSock,
		"codex_home": paths.codexHome, "state_root": profileDataRoot(paths),
	}); err != nil {
		return 1, err
	}
	if !appServerReachable || !supervisorReachable {
		return 1, nil
	}
	return 0, nil
}

func interruptLaneNative(options laneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveLaneState(paths, options.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" || isRetiredThreadNative(paths, state.ThreadID) {
		return 1, fmt.Errorf("lane %s is archived", state.ThreadID)
	}
	client, err := connectLaneClient(paths)
	if err != nil {
		return 1, err
	}
	defer client.close()
	if _, err := resumeThreadForPeer(client, state.ThreadID); err != nil {
		return 1, err
	}
	turns, err := listLaneTurns(client, state.ThreadID, "notLoaded")
	if err != nil {
		return 1, err
	}
	turn := findActiveLaneTurn(turns)
	if turn == nil {
		return 1, fmt.Errorf("lane %s has no active turn", state.ThreadID)
	}
	if err := requestWithTimeout(client, 30*time.Second, "turn/interrupt", map[string]any{
		"threadId": state.ThreadID, "turnId": turn.ID,
	}, nil); err != nil {
		return 1, err
	}
	if err := recordLaneInterrupted(paths, state.ThreadID, turn.ID); err != nil {
		return 1, fmt.Errorf("persist interrupted lane turn: %w", err)
	}
	_ = emitLane(map[string]any{"type": "turn.interrupted", "thread_id": state.ThreadID, "turn_id": turn.ID})
	return 0, nil
}

//nolint:gocyclo // Archive keeps App Server, retirement, notice, and metadata commits explicit.
func archiveLaneNative(options laneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveLaneState(paths, options.target)
	if err != nil {
		return 1, err
	}
	lifecycleLock, err := lockLaneLifecycle(paths, state.ThreadID)
	if err != nil {
		return 1, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	if state.Type == "codex-peer-lane" {
		if latest, readErr := readLaneStateFile(paths, state.ThreadID); readErr == nil {
			state = latest
		} else if !os.IsNotExist(readErr) {
			return 1, readErr
		}
	}
	if state.Status == "archived" || isRetiredThreadNative(paths, state.ThreadID) {
		return reaffirmArchivedLane(paths, state), nil
	}
	client, err := connectLaneClient(paths)
	if err != nil {
		return 1, err
	}
	defer client.close()
	unmaterialized := false
	if state.Type == "codex-peer-lane" {
		if err := settleLaneTurnBeforeArchive(client, paths, state); err != nil {
			if !unmaterializedLaneRolloutMissing(state, err) {
				return 1, fmt.Errorf("refuse to archive before active turn is terminal: %w", err)
			}
			unmaterialized = true
		}
	}
	flushed, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "flush_notices", "sessionId": state.ThreadID,
	}, 10*time.Second)
	if err != nil {
		return 1, fmt.Errorf("refuse to archive before terminal notices are checked: %w", err)
	}
	droppedNotices := 0
	if intValue(flushed["pending"]) > 0 {
		droppedNotices, err = cancelLaneNotices(paths, state.ThreadID, "lane archived after terminal notice target remained unreachable")
		if err != nil {
			return 1, fmt.Errorf("cancel undeliverable terminal notices before archive: %w", err)
		}
	}
	if unmaterialized {
		if err := deletePreparedThread(client, state.ThreadID); err != nil {
			return 1, fmt.Errorf("delete unmaterialized lane thread: %w", err)
		}
	} else if err := requestWithTimeout(client, 30*time.Second, "thread/archive", map[string]any{"threadId": state.ThreadID}, nil); err != nil {
		return 1, err
	}
	if err := markRetiredThread(paths, state.ThreadID); err != nil {
		return 1, fmt.Errorf("archive succeeded but peer retirement could not be persisted: %w", err)
	}
	lateDropped, cancelErr := cancelLaneNotices(paths, state.ThreadID, "lane archived before terminal notice delivery")
	droppedNotices += lateDropped
	if cancelErr != nil {
		return 1, fmt.Errorf("archive succeeded but terminal notice cancellation failed: %w", cancelErr)
	}
	if _, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "retire", "sessionId": state.ThreadID,
	}, 5*time.Second); err != nil {
		return 1, fmt.Errorf("archive succeeded but peer retirement failed: %w", err)
	}
	state.Status = "archived"
	if state.Type == "codex-peer-lane" {
		if err := persistArchivedLaneState(paths, state); err != nil {
			return 1, fmt.Errorf("archive succeeded but lane metadata could not be retained for resume: %w", err)
		}
	}
	_ = clearLaneThreadItemSpool(paths, state.ThreadID)
	_ = emitLane(map[string]any{
		"type": "lane.archived", "name": defaultString(state.Name, state.ThreadID), "thread_id": state.ThreadID,
		"notices_dropped": droppedNotices,
	})
	return 0, nil
}

func unmaterializedLaneRolloutMissing(state laneState, err error) bool {
	if err == nil || state.Type != "codex-peer-lane" || state.Status != "failed" ||
		state.TurnID != "" || state.LatestTurnID != "" || len(state.PendingTurnIDs) != 0 ||
		state.CollectedTurnID != "" || state.TerminalTurnID != "" ||
		(state.TerminalOutcome != "" && state.TerminalOutcome != "failed") {
		return false
	}
	return isRolloutMissingRPC(err)
}

func settleLaneTurnBeforeArchive(client *appServerClient, paths nativePaths, state laneState) error {
	if _, err := resumeThreadForPeer(client, state.ThreadID); err != nil {
		return fmt.Errorf("load lane before archive: %w", err)
	}
	turns, err := listLaneTurns(client, state.ThreadID, "notLoaded")
	if err != nil {
		return err
	}
	active := findActiveLaneTurn(turns)
	if active == nil {
		return reconcileLaneTerminalBeforeArchive(paths, state, turns)
	}
	interruptErr := requestWithTimeout(client, 30*time.Second, "turn/interrupt", map[string]any{
		"threadId": state.ThreadID, "turnId": active.ID,
	}, nil)
	if interruptErr == nil {
		return recordLaneArchiveTerminal(paths, state.ThreadID, active.ID, "interrupted")
	}
	// The turn can finish between the history read and turn/interrupt. Accept
	// that race only after a fresh authoritative history row proves terminal.
	refreshed, refreshErr := listLaneTurns(client, state.ThreadID, "notLoaded")
	if refreshErr != nil {
		return errors.Join(
			fmt.Errorf("interrupt active turn: %w", interruptErr),
			fmt.Errorf("refresh active turn after interrupt race: %w", refreshErr),
		)
	}
	for _, turn := range refreshed {
		if turn.ID != active.ID {
			continue
		}
		if status, collectable := laneCollectableTerminal(paths, state, turn); collectable {
			return recordLaneArchiveTerminal(paths, state.ThreadID, turn.ID, status)
		}
		break
	}
	return fmt.Errorf("interrupt active turn %s: %w", active.ID, interruptErr)
}

// reconcileLaneTerminalBeforeArchive closes the crash window where App Server
// accepted turn/interrupt but the process died before the terminal state was
// persisted locally. A retry must prove and record the owed turn's terminal
// history instead of treating "no active turn" as sufficient to archive.
func reconcileLaneTerminalBeforeArchive(paths nativePaths, observed laneState, turns []appTurn) error {
	latest, err := readLaneStateFile(paths, observed.ThreadID)
	if os.IsNotExist(err) {
		latest = observed
	} else if err != nil {
		return err
	}
	normalizeLanePendingTurns(&latest)
	targetTurnID := ""
	switch {
	case len(latest.PendingTurnIDs) > 0:
		targetTurnID = latest.PendingTurnIDs[len(latest.PendingTurnIDs)-1]
	case latest.LatestTurnID != "" && latest.LatestTurnID != latest.CollectedTurnID:
		targetTurnID = latest.LatestTurnID
	case latest.TurnID != "" && latest.TurnID != latest.CollectedTurnID:
		targetTurnID = latest.TurnID
	}
	if targetTurnID == "" || (latest.TerminalTurnID == targetTurnID && latest.TerminalOutcome != "") {
		return nil
	}
	for _, turn := range turns {
		if turn.ID != targetTurnID {
			continue
		}
		status, collectable := laneCollectableTerminal(paths, latest, turn)
		if !collectable {
			return fmt.Errorf("turn %s has no authoritative terminal evidence (status %q)", targetTurnID, status)
		}
		return recordLaneArchiveTerminal(paths, latest.ThreadID, targetTurnID, status)
	}
	return fmt.Errorf("turn %s is still owed but absent from App Server history", targetTurnID)
}

func reaffirmArchivedLane(paths nativePaths, state laneState) int {
	retirementReasserted := true
	if _, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "retire", "sessionId": state.ThreadID,
	}, 5*time.Second); err != nil {
		retirementReasserted = false
		fmt.Fprintf(os.Stderr, "codex-peer-lane: archived thread retirement will be reconciled later: %v\n", err)
	}
	state.Status = "archived"
	if state.Type == "codex-peer-lane" {
		_ = persistArchivedLaneState(paths, state)
	}
	_ = emitLane(map[string]any{
		"type": "lane.archived", "name": defaultString(state.Name, state.ThreadID),
		"thread_id": state.ThreadID, "already_archived": true,
		"retirement_reasserted": retirementReasserted,
	})
	return 0
}

func persistArchivedLaneState(paths nativePaths, observed laneState) error {
	lock, err := lockLaneStateFile(paths, observed.ThreadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	latest, err := readLaneStateFile(paths, observed.ThreadID)
	if os.IsNotExist(err) {
		latest = observed
	} else if err != nil {
		return err
	}
	latest.Status = "archived"
	latest.DeadlineAt = 0
	latest.AutoArchiveAt = 0
	return writeLaneStateUnlocked(paths, latest)
}

// waitForLaneTurn applies a caller-owned lane deadline while collecting the
// turn started by run/start/resume.
func waitForLaneTurn(client *appServerClient, state *laneState, timeout time.Duration) (string, int, bool, error) {
	return waitForLaneTurnWithPolicy(client, state, timeout, true, true)
}

// waitForLaneTurnWithPolicy separates a lane deadline from a collector
// deadline. run/start/resume own the turn they started and may enforce their
// caller-supplied timeout. wait merely observes an existing lane, so its
// timeout must return control to the collector without interrupting or
// relabelling that lane.
//
//nolint:gocyclo
func waitForLaneTurnWithPolicy(
	client *appServerClient,
	state *laneState,
	timeout time.Duration,
	terminateTurnOnTimeout bool,
	interruptTurnOnSignal bool,
) (string, int, bool, error) {
	paths := resolveNativePaths()
	seen := map[string]bool{}
	var usage any
	refreshState := func() error {
		// The supervisor appends peer-wake turns and replaces schema drafts in a
		// separate process. Tests and legacy callers without a state file keep
		// their local cursor until the first durable write.
		if latest, err := readLaneStateFile(paths, state.ThreadID); err == nil {
			*state = latest
		} else if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	checkThread := func() (string, bool, error) {
		// Refresh before every selection so a blocked collector observes durable
		// queue changes instead of keeping its entry-time snapshot.
		if err := refreshState(); err != nil {
			return "", false, err
		}
		turns, err := listLaneTurns(client, state.ThreadID, "full")
		if err != nil {
			return "", false, err
		}
		turn := selectLaneTurnForState(turns, *state)
		if turn == nil {
			return "", false, nil
		}
		if state.TerminalOutcome != "" && state.TerminalTurnID == "" {
			// Associate state written by older bridge versions before changing
			// the collector's preferred turn.
			state.TerminalTurnID = state.TurnID
		}
		state.TurnID = turn.ID
		turn.Items = append(turn.Items, readLaneItemSpool(paths, state.ThreadID, turn.ID)...)
		for _, raw := range turn.Items {
			if err := emitLaneItem(raw, turn.ID, seen); err != nil {
				return "", false, err
			}
		}
		status := normalizeStatus(turn.Status)
		if statusTerminal(status) {
			if err := reloadLaneTerminalState(paths, state, turn.ID); err != nil {
				return "", false, err
			}
			confirmedStatus, confirmed := laneCollectableTerminal(paths, *state, *turn)
			if !confirmed {
				// A newly started turn can briefly inherit the preceding terminal
				// status in Codex's projection. Do not acknowledge that row until a
				// terminal notification or complete persisted record corroborates it.
				return status, false, nil
			}
			status = confirmedStatus
			if status == "completed" && !laneTurnHasFinalAnswer(*turn) {
				// App Server can expose the terminal row before its items are
				// materialized. A successful turn is not collectable until its
				// persisted final answer can be replayed.
				return status, false, nil
			}
			if usage == nil {
				usage = readLaneMetric(paths, state.ThreadID, turn.ID)
			}
			if status == "completed" {
				if retried, attempt, err := confirmLaneSchemaTerminal(paths, state, turn.ID); err != nil {
					return "failed", true, err
				} else if retried {
					latest, readErr := readLaneStateFile(paths, state.ThreadID)
					if readErr != nil {
						return "", false, readErr
					}
					*state = latest
					if err := emitLane(map[string]any{"type": "turn.schema_retry", "thread_id": state.ThreadID, "turn_id": turn.ID, "attempt": attempt}); err != nil {
						return "", false, err
					}
					return state.Status, false, nil
				}
			}
			outcome := laneTerminalOutcome(*state, turn.ID, status)
			if err := emitLane(map[string]any{
				"type": "turn.completed", "turn_id": turn.ID, "status": status,
				"outcome": outcome, "exit": laneExitCode(outcome),
				"usage": normalizeLaneUsage(usage), "error": turn.Error,
				"accounting": laneAccounting(*turn, usage),
			}); err != nil {
				return "", false, err
			}
			return outcome, true, nil
		}
		return status, false, nil
	}
	if status, done, err := checkThread(); err != nil {
		return "", 1, false, err
	} else if done {
		return status, laneExitCode(status), true, nil
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var timer <-chan time.Time
	if timeout > 0 {
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()
		timer = deadline.C
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	interrupt := func() {
		_ = requestWithTimeout(client, 10*time.Second, "turn/interrupt", map[string]any{
			"threadId": state.ThreadID, "turnId": state.TurnID,
		}, nil)
	}
	for {
		select {
		case notification := <-client.notifications:
			var params map[string]any
			if json.Unmarshal(notification.Params, &params) != nil || stringValue(params["threadId"]) != state.ThreadID {
				continue
			}
			if err := refreshState(); err != nil {
				return "", 1, false, err
			}
			turnID := laneNotificationTurnID(params)
			if state.TurnID == "" && turnID != "" && turnID != state.CollectedTurnID {
				state.TurnID = turnID
			}
			if turnID != "" && turnID != state.TurnID {
				continue
			}
			switch notification.Method {
			case "turn/started":
				if state.TurnID != "" {
					if err := emitLane(map[string]any{"type": "turn.started", "thread_id": state.ThreadID, "turn_id": state.TurnID}); err != nil {
						return "", 1, false, err
					}
				}
			case "thread/tokenUsage/updated":
				usage = params["tokenUsage"]
			case "item/completed":
				raw, _ := json.Marshal(params["item"])
				if err := emitLaneItem(raw, state.TurnID, seen); err != nil {
					return "", 1, false, err
				}
			case "turn/completed":
				turnMap, _ := params["turn"].(map[string]any)
				if id := stringValue(turnMap["id"]); id != "" && id != state.TurnID {
					continue
				}
				if err := persistLaneTerminalObservation(paths, state.ThreadID, turnMap); err != nil {
					return "", 1, false, err
				}
				// App Server may publish turn/completed before all item/completed
				// notifications reach this subscriber. Re-read the persisted turn and
				// replay its items before acknowledging the collection cursor.
				status, done, checkErr := checkThread()
				if checkErr != nil {
					return "", 1, false, checkErr
				}
				if done {
					return status, laneExitCode(status), true, nil
				}
			}
		case <-ticker.C:
			status, done, err := checkThread()
			if err != nil {
				return "", 1, false, err
			}
			if done {
				return status, laneExitCode(status), true, nil
			}
		case <-timer:
			if terminateTurnOnTimeout && state.TurnID != "" {
				markLaneTurnTimedOut(paths, state)
				interrupt()
				return "timed_out", 124, false, fmt.Errorf("timed out after %s: %w", timeout, context.DeadlineExceeded)
			}
			return "", 124, false, fmt.Errorf("timed out waiting to collect a turn after %s: %w", timeout, context.DeadlineExceeded)
		case <-signals:
			return laneWaitSignalStatus(*state, interruptTurnOnSignal, interrupt), 130, false, nil
		case <-client.done:
			return "", 1, false, client.closeReason()
		}
	}
}

func laneWaitSignalStatus(state laneState, interruptTurnOnSignal bool, interrupt func()) string {
	if interruptTurnOnSignal {
		interrupt()
		return "interrupted"
	}
	// Detached resume and wait processes are collectors, not turn owners.
	// Their signal exits the collector while the durable turn keeps running.
	return state.Status
}

func setLaneDeadline(state *laneState, timeout time.Duration) {
	state.DeadlineAt = 0
	state.TimedOutTurnID = ""
	state.TerminalOutcome = ""
	state.TerminalTurnID = ""
	if timeout > 0 {
		state.DeadlineAt = time.Now().Add(timeout).UnixMilli()
	}
}

func markLaneTurnTimedOut(paths nativePaths, state *laneState) {
	if _, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "timeout_turn", "sessionId": state.ThreadID, "turnId": state.TurnID,
	}, 5*time.Second); err == nil {
		if latest, readErr := readLaneStateFile(paths, state.ThreadID); readErr == nil {
			*state = latest
		}
		return
	}
	// Preserve the deadline outcome even if the supervisor control path is
	// temporarily unavailable. The next reconciliation will retry interrupting
	// the same turn from this durable state.
	state.TimedOutTurnID = state.TurnID
	state.TerminalOutcome = "timed_out"
	state.TerminalTurnID = state.TurnID
	_ = recordLaneState(paths, *state)
}

func readLanePrompt(options laneOptions) (string, error) {
	const maxPromptBytes = 64 * 1024 * 1024
	var reader io.Reader = os.Stdin
	var file *os.File
	var err error
	if options.promptFile != "" {
		file, err = os.Open(options.promptFile)
		if err != nil {
			return "", err
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	body, err := readBoundedLanePrompt(reader, maxPromptBytes)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(body)) == "" {
		return "", errors.New("prompt is empty")
	}
	return string(body), nil
}

func readBoundedLanePrompt(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("prompt exceeds the %d-byte limit", limit)
	}
	return body, nil
}

func laneThreadStartParams(options laneOptions) (map[string]any, error) {
	params := map[string]any{"cwd": absolutePath(options.cwd), "ephemeral": false, "serviceName": "codex-peer-lane"}
	if options.model != "" {
		params["model"] = options.model
	}
	if options.sandbox != "" {
		params["sandbox"] = options.sandbox
	}
	if options.approvalPolicy != "" {
		params["approvalPolicy"] = options.approvalPolicy
	}
	config := map[string]any{}
	for _, entry := range options.configs {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			return nil, fmt.Errorf("config override must be KEY=VALUE: %s", entry)
		}
		var value any
		if json.Unmarshal([]byte(entry[separator+1:]), &value) != nil {
			value = strings.Trim(entry[separator+1:], "\"'")
		}
		if err := assignDottedNative(config, entry[:separator], value); err != nil {
			return nil, err
		}
	}
	// App Server's external code-mode host delegates nested tools back to its
	// UI client through item/tool/call. A headless lane has no TUI dispatcher;
	// the in-server host keeps shell/patch tools native, while our supervisor
	// dispatches the MCP subset needed for peer messaging.
	_ = assignDottedNative(config, "features.code_mode_host", false)
	if options.web != nil {
		_ = assignDottedNative(config, "tools.web_search", *options.web)
	}
	if len(config) > 0 {
		params["config"] = config
	}
	return params, nil
}

func laneTurnStartParams(options laneOptions, threadID, prompt string) map[string]any {
	params := map[string]any{"threadId": threadID, "input": []map[string]any{{"type": "text", "text": prompt}}}
	if options.model != "" {
		params["model"] = options.model
	}
	if options.effort != "" {
		params["effort"] = options.effort
	}
	if options.approvalPolicy != "" {
		params["approvalPolicy"] = options.approvalPolicy
	}
	if options.sandbox != "" {
		typeName := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[options.sandbox]
		params["sandboxPolicy"] = map[string]any{"type": typeName}
	}
	if len(options.outputSchema) > 0 {
		var schema any
		if json.Unmarshal(options.outputSchema, &schema) == nil {
			params["outputSchema"] = schema
		}
	}
	return params
}

func readLaneOutputSchema(file string) (json.RawMessage, error) {
	body, err := os.ReadFile(file) //nolint:gosec // caller explicitly selects the schema file.
	if err != nil {
		return nil, err
	}
	if len(body) > maxFrameBytes {
		return nil, errors.New("output schema exceeds 1 MiB")
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		return nil, fmt.Errorf("parse output schema: %w", err)
	}
	if len(schema) == 0 {
		return nil, errors.New("output schema must be a non-empty JSON object")
	}
	compact, _ := json.Marshal(schema)
	if _, err := jsonschema.CompileString("lane-output-schema.json", string(compact)); err != nil {
		return nil, fmt.Errorf("compile output schema: %w", err)
	}
	return compact, nil
}

func readLaneStateFile(paths nativePaths, threadID string) (laneState, error) {
	body, err := os.ReadFile(laneStatePath(paths, threadID))
	if err != nil {
		return laneState{}, err
	}
	var state laneState
	if err := json.Unmarshal(body, &state); err != nil {
		return laneState{}, err
	}
	if state.ThreadID != threadID {
		return laneState{}, errors.New("lane state/thread mismatch")
	}
	return state, nil
}

func validateLaneTurnOutput(client *appServerClient, paths nativePaths, state laneState, turnID string) error {
	turns, err := listLaneTurns(client, state.ThreadID, "full")
	if err != nil {
		return err
	}
	answer := ""
	for _, turn := range turns {
		if turn.ID != turnID {
			continue
		}
		turn.Items = append(turn.Items, readLaneItemSpool(paths, state.ThreadID, turn.ID)...)
		for _, raw := range turn.Items {
			var item map[string]any
			if json.Unmarshal(raw, &item) != nil || snakeCaseNative(stringValue(item["type"])) != "agent_message" {
				continue
			}
			if stringValue(item["phase"]) == "final_answer" {
				answer = stringValue(item["text"])
			}
		}
		break
	}
	if strings.TrimSpace(answer) == "" {
		return errors.New("final answer is missing")
	}
	decoder := json.NewDecoder(strings.NewReader(answer))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("final answer is not JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("final answer contains trailing non-JSON content")
	}
	schema, err := jsonschema.CompileString("lane-output-schema.json", string(state.OutputSchema))
	if err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("final answer violates schema: %w", err)
	}
	return nil
}

func confirmLaneSchemaTerminal(paths nativePaths, state *laneState, turnID string) (bool, int, error) {
	if len(state.OutputSchema) == 0 {
		return false, 0, nil
	}
	response, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "schema_terminal", "sessionId": state.ThreadID, "turnId": turnID,
	}, 75*time.Second)
	if err != nil {
		return false, 0, err
	}
	retried, _ := response["retried"].(bool)
	return retried, intValue(response["attempt"]), nil
}

func laneAccounting(turn appTurn, usage any) map[string]any {
	return map[string]any{
		"duration_ms":    turn.DurationMS,
		"started_at":     turn.StartedAt,
		"completed_at":   turn.CompletedAt,
		"tokens":         normalizeLaneUsage(usage),
		"cost":           nil,
		"cost_available": false,
	}
}

func laneMetricPath(paths nativePaths, threadID, turnID string) string {
	return filepath.Join(profileDataRoot(paths), "metrics", sessionKey(threadID+"\x00"+turnID)+".json")
}

func laneTerminalObservationPath(paths nativePaths, threadID, turnID string) string {
	return filepath.Join(
		profileDataRoot(paths), "lane-terminals", sessionKey(threadID), sessionKey(turnID)+".json",
	)
}

func persistLaneTerminalObservation(paths nativePaths, threadID string, turn map[string]any) error {
	turnID := stringValue(turn["id"])
	status := normalizeStatus(turn["status"])
	if threadID == "" || turnID == "" || !statusTerminal(status) {
		return nil
	}
	return writeJSONAtomic(laneTerminalObservationPath(paths, threadID, turnID), map[string]any{
		"threadId": threadID, "turnId": turnID, "status": status,
		"observedAt": time.Now().UnixMilli(),
	})
}

// laneCollectableTerminal rejects projection-only terminal rows that have no
// completion evidence. Codex can transiently expose a newly started turn as
// interrupted before the model actually finishes. A notification observation
// is authoritative; a complete historical row is the crash-recovery fallback.
func laneCollectableTerminal(paths nativePaths, state laneState, turn appTurn) (string, bool) {
	status := normalizeStatus(turn.Status)
	if !statusTerminal(status) {
		return status, false
	}
	observation := readJSONMap(laneTerminalObservationPath(paths, state.ThreadID, turn.ID))
	if stringValue(observation["threadId"]) == state.ThreadID && stringValue(observation["turnId"]) == turn.ID {
		observedStatus := normalizeStatus(observation["status"])
		if statusTerminal(observedStatus) {
			if observedStatus == "completed" && !laneTurnHasFinalAnswer(turn) {
				return observedStatus, false
			}
			return observedStatus, true
		}
	}
	if status == "completed" {
		return status, laneTurnHasFinalAnswer(turn)
	}
	return status, turn.CompletedAt != nil || turn.DurationMS != nil
}

func readLaneMetric(paths nativePaths, threadID, turnID string) any {
	row := readJSONMap(laneMetricPath(paths, threadID, turnID))
	if stringValue(row["threadId"]) != threadID || stringValue(row["turnId"]) != turnID {
		return nil
	}
	return row["tokenUsage"]
}

func laneItemSpoolDir(paths nativePaths, threadID, turnID string) string {
	return filepath.Join(profileDataRoot(paths), "lane-items", sessionKey(threadID), sessionKey(turnID))
}

func persistLaneItem(paths nativePaths, threadID, turnID string, item map[string]any) error {
	lock, err := lockLaneItemSpool(paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneItemSpool(lock)
	state, err := readLaneStateFile(paths, threadID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	normalizeLanePendingTurns(&state)
	if !laneTurnPending(state, turnID) || state.CollectedTurnID == turnID || isRetiredThreadNative(paths, threadID) {
		return nil
	}
	itemID := defaultString(stringValue(item["id"]), randomID())
	name := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), sessionKey(itemID))
	return writeJSONAtomic(filepath.Join(laneItemSpoolDir(paths, threadID, turnID), name), map[string]any{
		"threadId": threadID, "turnId": turnID, "item": item,
	})
}

func readLaneItemSpool(paths nativePaths, threadID, turnID string) []json.RawMessage {
	entries, _ := os.ReadDir(laneItemSpoolDir(paths, threadID, turnID))
	items := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		row := readJSONMap(filepath.Join(laneItemSpoolDir(paths, threadID, turnID), entry.Name()))
		if stringValue(row["threadId"]) != threadID || stringValue(row["turnId"]) != turnID {
			continue
		}
		body, err := json.Marshal(row["item"])
		if err == nil {
			items = append(items, body)
		}
	}
	return items
}

func clearLaneItemSpool(paths nativePaths, threadID, turnID string) error {
	return os.RemoveAll(laneItemSpoolDir(paths, threadID, turnID))
}

func clearLaneThreadItemSpool(paths nativePaths, threadID string) error {
	lock, err := lockLaneItemSpool(paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneItemSpool(lock)
	return os.RemoveAll(filepath.Join(profileDataRoot(paths), "lane-items", sessionKey(threadID)))
}

func persistLaneWaitState(paths nativePaths, state laneState, collected bool) error {
	acknowledgedTurnID := state.TurnID
	lock, err := lockLaneItemSpool(paths, state.ThreadID)
	if err != nil {
		return err
	}
	defer unlockLaneItemSpool(lock)
	stateLock, err := lockLaneStateFile(paths, state.ThreadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(stateLock)
	latest, readErr := readLaneStateFile(paths, state.ThreadID)
	if readErr == nil {
		mergeLaneWaitState(&latest, state, collected)
		state = latest
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err := writeLaneStateUnlocked(paths, state); err != nil {
		return err
	}
	if !collected {
		return nil
	}
	// Cursor first, cleanup second: a crash can leave harmless garbage but can
	// never erase the only recoverable copy before acknowledgement is durable.
	_ = clearLaneItemSpool(paths, state.ThreadID, acknowledgedTurnID)
	return nil
}

func mergeLaneWaitState(latest *laneState, observed laneState, collected bool) {
	normalizeLanePendingTurns(latest)
	archived := latest.Status == "archived"
	// The supervisor can advance durable deadline/outcome state while a
	// collector is blocked. Apply only collector-owned fields to the newest
	// record instead of overwriting it with the stale snapshot.
	if !archived && observed.Status != "" && (observed.Status != "timed_out" || latest.TerminalOutcome != "timed_out") {
		latest.Status = observed.Status
	}
	switch {
	case collected:
		mergeLaneAcknowledgement(latest, observed)
	case len(latest.PendingTurnIDs) > 0:
		latest.TurnID = latest.PendingTurnIDs[0]
	default:
		latest.TurnID = observed.TurnID
	}
	if archived {
		latest.Status = "archived"
		latest.AutoArchiveAt = 0
	}
}

func mergeLaneAcknowledgement(latest *laneState, observed laneState) {
	outcome := observed.Status
	outcomeTurnID := latest.TerminalTurnID
	if outcomeTurnID == "" {
		outcomeTurnID = defaultString(latest.TimedOutTurnID, latest.TurnID)
	}
	if outcomeTurnID == observed.TurnID && latest.TerminalOutcome != "" {
		// A supervisor-owned deadline or terminal observation is more precise
		// than a stale collector's raw App Server status.
		outcome = latest.TerminalOutcome
	}
	acknowledgeLanePendingTurn(latest, observed.TurnID)
	latest.CollectedTurnID = observed.TurnID
	latest.TerminalOutcome = outcome
	latest.TerminalTurnID = observed.TurnID
	// A later terminal turn can be deferred behind older collection debt. Its
	// supervisor event cannot safely start the grace period until validation,
	// so collection is the durable fallback that arms auto-archive.
	if latest.Status != "archived" && latest.AutoArchive && latest.LatestTurnID == observed.TurnID && latest.AutoArchiveAt == 0 {
		latest.AutoArchiveAt = time.Now().Add(laneAutoArchiveDelay(*latest)).UnixMilli()
	}
	if outcome == "timed_out" {
		latest.TimedOutTurnID = observed.TurnID
	} else {
		latest.TimedOutTurnID = ""
	}
}

func reloadLaneTerminalState(paths nativePaths, state *laneState, turnID string) error {
	latest, err := readLaneStateFile(paths, state.ThreadID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if latest.TurnID != turnID && latest.TerminalTurnID != turnID && latest.TimedOutTurnID != turnID {
		return nil
	}
	state.DeadlineAt = latest.DeadlineAt
	state.TimedOutTurnID = latest.TimedOutTurnID
	state.TerminalOutcome = latest.TerminalOutcome
	state.TerminalTurnID = latest.TerminalTurnID
	state.SchemaAttempts = latest.SchemaAttempts
	state.OutputSchema = latest.OutputSchema
	return nil
}

func lockLaneItemSpool(paths nativePaths, threadID string) (*os.File, error) {
	lockPath := filepath.Join(profileDataRoot(paths), "lane-item-locks", sessionKey(threadID)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // path is bridge-owned and the caller-controlled id is hashed.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func unlockLaneItemSpool(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func createLaneWorktree(paths nativePaths, name, cwd string) (string, error) {
	canonicalCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve lane cwd symlinks: %w", err)
	}
	repositoryBody, err := exec.Command( //nolint:gosec // fixed git query; the canonical path is an explicit argument.
		"git", "-C", canonicalCwd, "rev-parse", "--show-toplevel",
	).Output()
	if err != nil {
		return "", fmt.Errorf("locate git repository for worktree: %w", err)
	}
	repository, err := filepath.EvalSymlinks(strings.TrimSpace(string(repositoryBody)))
	if err != nil {
		return "", fmt.Errorf("resolve git repository symlinks: %w", err)
	}
	relativeCwd, err := filepath.Rel(repository, canonicalCwd)
	if err != nil || relativeCwd == ".." || strings.HasPrefix(relativeCwd, ".."+string(os.PathSeparator)) {
		return "", errors.New("lane cwd is outside its reported git repository")
	}
	parent := filepath.Join(profileDataRoot(paths), "worktrees")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(parent, sanitizeName(name)+"-"+first8(randomID()))
	command := exec.Command("git", "-C", repository, "worktree", "add", "--detach", path, "HEAD") //nolint:gosec // fixed git subcommand; paths are explicit arguments.
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("create isolated worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	worktreeCwd := filepath.Join(path, relativeCwd)
	if info, statErr := os.Stat(worktreeCwd); statErr != nil || !info.IsDir() {
		cleanup := exec.Command("git", "-C", repository, "worktree", "remove", "--force", path) //nolint:gosec // fixed git subcommand; explicit paths are separate arguments.
		_, _ = cleanup.CombinedOutput()
		return "", fmt.Errorf("lane cwd %s is not present in the detached worktree", relativeCwd)
	}
	return worktreeCwd, nil
}

func removeLaneWorktree(originalCwd, worktreeCwd string) error {
	root := filepath.Clean(worktreeCwd)
	for {
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return errors.New("failed lane worktree has no repository root")
		}
		root = parent
	}
	command := exec.Command("git", "-C", originalCwd, "worktree", "remove", "--force", root) //nolint:gosec // fixed git subcommand; explicit paths are separate arguments.
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove failed lane worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func assignDottedNative(target map[string]any, key string, value any) error {
	parts := []string{}
	for _, part := range strings.Split(key, ".") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return errors.New("config key must not be empty")
	}
	cursor := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := cursor[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cursor[part] = next
		}
		cursor = next
	}
	cursor[parts[len(parts)-1]] = value
	return nil
}

func lockLaneNames(paths nativePaths) (*os.File, error) {
	if err := os.MkdirAll(paths.dataRoot, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(paths.dataRoot, "lane-name.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock lane-name registry: %w", err)
	}
	return file, nil
}

func recordLaneState(paths nativePaths, state laneState) error {
	lock, err := lockLaneStateFile(paths, state.ThreadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	return writeLaneStateUnlocked(paths, state)
}

// recordLaneInterrupted records current App Server activity without replacing
// an older terminal turn that is still owed to the collector.
func recordLaneInterrupted(paths nativePaths, threadID, turnID string) error {
	lock, err := lockLaneStateFile(paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	state, err := readLaneStateFile(paths, threadID)
	if err != nil {
		return err
	}
	appendLanePendingTurn(&state, turnID)
	state.LatestTurnID = turnID
	if state.TurnID == turnID {
		state.Status = "interrupted"
		state.DeadlineAt = 0
		state.TerminalTurnID = turnID
		if state.TimedOutTurnID != turnID || state.TerminalOutcome != "timed_out" {
			state.TerminalOutcome = "interrupted"
			state.TimedOutTurnID = ""
		}
	}
	return writeLaneStateUnlocked(paths, state)
}

func recordLaneArchiveTerminal(paths nativePaths, threadID, turnID, status string) error {
	status = normalizeStatus(status)
	if turnID == "" || !statusTerminal(status) {
		return fmt.Errorf("turn %s has non-terminal archive status %q", turnID, status)
	}
	if err := persistLaneTerminalObservation(paths, threadID, map[string]any{
		"id": turnID, "status": status,
	}); err != nil {
		return err
	}
	lock, err := lockLaneStateFile(paths, threadID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	state, err := readLaneStateFile(paths, threadID)
	if err != nil {
		return err
	}
	appendLanePendingTurn(&state, turnID)
	state.LatestTurnID = turnID
	state.Status = status
	state.DeadlineAt = 0
	state.TerminalTurnID = turnID
	if status == "interrupted" && state.TimedOutTurnID == turnID && state.TerminalOutcome == "timed_out" {
		state.TerminalOutcome = "timed_out"
	} else {
		state.TerminalOutcome = status
		state.TimedOutTurnID = ""
	}
	return writeLaneStateUnlocked(paths, state)
}

func writeLaneStateUnlocked(paths nativePaths, state laneState) error {
	state.UpdatedAt = time.Now().UnixMilli()
	return writeJSONAtomic(laneStatePath(paths, state.ThreadID), state)
}

// PendingTurnIDs is the durable collection order. Older state files are
// migrated lazily from the former TurnID/CollectedTurnID cursor pair.
func normalizeLanePendingTurns(state *laneState) {
	migrating := state.PendingQueueVer == 0
	unique := make([]string, 0, len(state.PendingTurnIDs)+1)
	seen := map[string]bool{}
	for _, turnID := range state.PendingTurnIDs {
		if turnID == "" || seen[turnID] || turnID == state.CollectedTurnID {
			continue
		}
		seen[turnID] = true
		unique = append(unique, turnID)
	}
	if len(unique) == 0 && state.TurnID != "" && state.TurnID != state.CollectedTurnID {
		seen[state.TurnID] = true
		unique = append(unique, state.TurnID)
	}
	// Pre-queue bridge versions persisted only the newest activity separately.
	// Preserve that recoverable tail during lazy migration; new state already
	// contains it in PendingTurnIDs and therefore does not duplicate it.
	if migrating && state.LatestTurnID != "" && state.LatestTurnID != state.CollectedTurnID && !seen[state.LatestTurnID] {
		unique = append(unique, state.LatestTurnID)
	}
	state.PendingTurnIDs = unique
	state.PendingQueueVer = 1
	if len(unique) > 0 {
		state.TurnID = unique[0]
	}
}

func laneTurnPending(state laneState, turnID string) bool {
	for _, pending := range state.PendingTurnIDs {
		if pending == turnID {
			return true
		}
	}
	return false
}

func laneTurnIsCollectionHead(paths nativePaths, threadID, turnID string) bool {
	state, err := readLaneStateFile(paths, threadID)
	if err != nil {
		return true
	}
	normalizeLanePendingTurns(&state)
	return state.TurnID == turnID
}

func appendLanePendingTurn(state *laneState, turnID string) {
	normalizeLanePendingTurns(state)
	if turnID == "" || laneTurnPending(*state, turnID) || turnID == state.CollectedTurnID {
		return
	}
	state.PendingTurnIDs = append(state.PendingTurnIDs, turnID)
	state.TurnID = state.PendingTurnIDs[0]
}

func replaceLanePendingTurn(state *laneState, draftTurnID, correctionTurnID string) bool {
	normalizeLanePendingTurns(state)
	for index, pending := range state.PendingTurnIDs {
		if pending != draftTurnID {
			continue
		}
		state.PendingTurnIDs[index] = correctionTurnID
		state.TurnID = state.PendingTurnIDs[0]
		return true
	}
	return false
}

func acknowledgeLanePendingTurn(state *laneState, turnID string) {
	normalizeLanePendingTurns(state)
	delete(state.SchemaRetryByID, turnID)
	remaining := state.PendingTurnIDs[:0]
	for _, pending := range state.PendingTurnIDs {
		if pending != turnID {
			remaining = append(remaining, pending)
		}
	}
	state.PendingTurnIDs = remaining
	if len(remaining) > 0 {
		state.TurnID = remaining[0]
	} else {
		state.TurnID = turnID
	}
}

func lockLaneStateFile(paths nativePaths, threadID string) (*os.File, error) {
	directory := filepath.Join(profileDataRoot(paths), "lane-state-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionKey(threadID)+".lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // thread id is hashed.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockLaneLifecycle(paths nativePaths, threadID string) (*os.File, error) {
	directory := filepath.Join(profileDataRoot(paths), "lane-lifecycle-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionKey(threadID)+".lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // thread id is hashed.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func tryLockLaneLifecycle(paths nativePaths, threadID string) (*os.File, bool, error) {
	directory := filepath.Join(profileDataRoot(paths), "lane-lifecycle-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionKey(threadID)+".lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // thread id is hashed.
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, false, nil
		}
		return nil, false, err
	}
	return file, true, nil
}

func unlockLaneLifecycle(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func unlockLaneStateFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func laneStatePath(paths nativePaths, threadID string) string {
	return filepath.Join(profileDataRoot(paths), "lanes", sessionKey(threadID)+".json")
}

func readLaneStates(paths nativePaths) []laneState {
	directory := filepath.Join(profileDataRoot(paths), "lanes")
	entries, _ := os.ReadDir(directory)
	states := []laneState{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // entry comes from ReadDir on the bridge-owned lane directory.
		if err != nil {
			continue
		}
		var state laneState
		if json.Unmarshal(body, &state) == nil && state.Type == "codex-peer-lane" && state.ThreadID != "" &&
			entry.Name() == sessionKey(state.ThreadID)+".json" {
			states = append(states, state)
		}
	}
	return states
}

func resolveLaneState(paths nativePaths, target string) (laneState, error) {
	states := readLaneStates(paths)
	for _, state := range states {
		if state.ThreadID == target || state.SessionID == target {
			return state, nil
		}
	}
	matches := []laneState{}
	for _, state := range states {
		if strings.EqualFold(state.Name, target) {
			matches = append(matches, state)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return laneState{}, fmt.Errorf("lane name %q is ambiguous; use a thread id", target)
	}
	if looksLikeUUID(target) {
		return laneState{ThreadID: target, SessionID: target}, nil
	}
	return laneState{}, fmt.Errorf("unknown lane %q", target)
}

func connectLaneClient(paths nativePaths) (*appServerClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return dialAppServer(ctx, paths.appServerSock)
}

func listLaneTurns(client *appServerClient, threadID, itemsView string) ([]appTurn, error) {
	turns := []appTurn{}
	cursor := ""
	seenCursors := map[string]bool{}
	for len(turns) < 1000 {
		params := map[string]any{
			"threadId": threadID, "limit": 100, "sortDirection": "desc", "itemsView": itemsView,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appTurn `json:"data"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := requestWithTimeout(client, 30*time.Second, "thread/turns/list", params, &page); err != nil {
			return nil, err
		}
		turns = append(turns, page.Data...)
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			break
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return turns, nil
}

// selectLaneTurn accepts newest-first App Server history and returns the next
// turn the caller has not collected. This makes wait a cursor over turns: the
// initial detached turn is collected once, then a later wait blocks until a
// peer-wake turn exists instead of replaying the prior completion.
func selectLaneTurn(turns []appTurn, preferred, collected string) *appTurn {
	if collected == "" && preferred != "" {
		for index := range turns {
			if turns[index].ID == preferred {
				return &turns[index]
			}
		}
		return nil
	}
	if collected == "" {
		if len(turns) > 0 {
			return &turns[0]
		}
		return nil
	}
	unseen := []int{}
	for index := range turns {
		if turns[index].ID == collected {
			break
		}
		unseen = append(unseen, index)
	}
	if len(unseen) == 0 {
		return nil
	}
	return &turns[unseen[len(unseen)-1]]
}

// selectLaneTurnForState makes the durable queue authoritative once a lane has
// migrated to it. Raw App Server history retains schema-rejected drafts, so a
// history cursor would reselect a draft after its correction replaced it.
func selectLaneTurnForState(turns []appTurn, state laneState) *appTurn {
	if state.PendingQueueVer == 0 {
		return selectLaneTurn(turns, state.TurnID, state.CollectedTurnID)
	}
	if len(state.PendingTurnIDs) == 0 {
		return nil
	}
	next := state.PendingTurnIDs[0]
	for index := range turns {
		if turns[index].ID == next {
			return &turns[index]
		}
	}
	return nil
}

func findActiveLaneTurn(turns []appTurn) *appTurn {
	for index := range turns {
		if !statusTerminal(normalizeStatus(turns[index].Status)) {
			return &turns[index]
		}
	}
	return nil
}

func laneNotificationTurnID(params map[string]any) string {
	if id := stringValue(params["turnId"]); id != "" {
		return id
	}
	if turn, ok := params["turn"].(map[string]any); ok {
		return stringValue(turn["id"])
	}
	return ""
}

func laneTurnHasFinalAnswer(turn appTurn) bool {
	for _, raw := range turn.Items {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if snakeCaseNative(stringValue(item["type"])) == "agent_message" && stringValue(item["phase"]) == "final_answer" {
			return true
		}
	}
	return false
}

func emptyStringAsNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nilIfZero(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func emitLaneItem(raw json.RawMessage, turnID string, seen map[string]bool) error {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		return fmt.Errorf("decode lane item: %w", err)
	}
	item["type"] = snakeCaseNative(stringValue(item["type"]))
	id := stringValue(item["id"])
	idKey := "id:" + id
	if id != "" && seen[idKey] {
		return nil
	}
	if id != "" {
		seen[idKey] = true
	}
	// Codex can expose the same logical item once under its provider id and
	// again under a projection-local positional id (item-N). Collapse only
	// cross-namespace duplicates; two distinct provider ids remain distinct.
	if semantic := laneItemSemanticKey(item); semantic != "" {
		positional := laneProjectionItemID(id)
		if seen["semantic-positional:"+semantic] || (positional && seen["semantic-any:"+semantic]) {
			return nil
		}
		seen["semantic-any:"+semantic] = true
		if positional {
			seen["semantic-positional:"+semantic] = true
		}
	}
	return emitLane(map[string]any{"type": "item.completed", "item": item, "turn_id": turnID})
}

func laneProjectionItemID(id string) bool {
	if !strings.HasPrefix(id, "item-") || len(id) == len("item-") {
		return false
	}
	for _, current := range id[len("item-"):] {
		if current < '0' || current > '9' {
			return false
		}
	}
	return true
}

func laneItemSemanticKey(item map[string]any) string {
	copyItem := make(map[string]any, len(item)-1)
	for key, value := range item {
		if key != "id" {
			copyItem[key] = value
		}
	}
	body, err := json.Marshal(copyItem)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest[:])
}

func emitLane(value map[string]any) error {
	body, _ := json.Marshal(value)
	body = append(body, '\n')
	_, err := os.Stdout.Write(body)
	return err
}

func normalizeLaneUsage(value any) any {
	usage, _ := value.(map[string]any)
	if usage == nil {
		return nil
	}
	if last, ok := usage["last"].(map[string]any); ok {
		usage = last
	} else if total, ok := usage["total"].(map[string]any); ok {
		usage = total
	}
	return map[string]any{
		"input_tokens":            numberFallback(usage, "inputTokens", "input_tokens", "totalTokens"),
		"cached_input_tokens":     numberFallback(usage, "cachedInputTokens", "cached_input_tokens"),
		"output_tokens":           numberFallback(usage, "outputTokens", "output_tokens"),
		"reasoning_output_tokens": numberFallback(usage, "reasoningOutputTokens", "reasoning_output_tokens"),
		"total_tokens":            numberFallback(usage, "totalTokens", "total_tokens"),
	}
}

func numberFallback(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if current, ok := value[key]; ok {
			return current
		}
	}
	return nil
}

func normalizeStatus(value any) string {
	switch current := value.(type) {
	case string:
		return snakeCaseNative(current)
	case map[string]any:
		return snakeCaseNative(stringValue(current["type"]))
	}
	return ""
}

func snakeCaseNative(value string) string {
	var output strings.Builder
	for index, r := range value {
		if index > 0 && r >= 'A' && r <= 'Z' {
			output.WriteByte('_')
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		output.WriteRune(r)
	}
	return output.String()
}

func statusTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func laneExitCode(status string) int {
	if status == "completed" {
		return 0
	}
	if status == "timed_out" {
		return 124
	}
	if status == "interrupted" {
		return 130
	}
	return 1
}

func laneTerminalOutcome(state laneState, turnID, status string) string {
	outcomeTurnID := state.TerminalTurnID
	if outcomeTurnID == "" {
		// Backward compatibility for state written before outcomes carried an
		// explicit turn id.
		outcomeTurnID = state.TurnID
	}
	if outcomeTurnID == turnID && state.TerminalOutcome != "" {
		return state.TerminalOutcome
	}
	return status
}

func absolutePath(value string) string {
	path, err := filepath.Abs(value)
	if err != nil {
		return value
	}
	return path
}

func looksLikeUUID(value string) bool {
	value = strings.TrimPrefix(strings.ToLower(value), "urn:uuid:")
	parts := strings.Split(value, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 16, 64); err != nil {
			return false
		}
	}
	return true
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
