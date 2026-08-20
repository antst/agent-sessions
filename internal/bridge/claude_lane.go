package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const claudeLaneContractVersion = 1

type claudeLaneOptions struct {
	command            string
	name               string
	nameSet            bool
	cwd                string
	cwdSet             bool
	model              string
	effort             string
	permissionMode     string
	maxBudgetUSD       string
	tools              string
	toolsSet           bool
	allowedTools       string
	allowedToolsSet    bool
	disallowedTools    string
	disallowedToolsSet bool
	timeout            time.Duration
	timeoutSet         bool
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
	bare               bool
	bareSet            bool
	modelSet           bool
	effortSet          bool
	permissionModeSet  bool
	maxBudgetUSDSet    bool
	schemaSet          bool
	target             string
	allowDuplicateName bool
	all                bool
	mine               bool
	json               bool
	stdinMarker        bool
	help               bool
	groupOptions       laneGroupOptions
}

type claudeLaneTurn struct {
	ID          string          `json:"id"`
	MessageID   string          `json:"messageId,omitempty"`
	Prompt      string          `json:"prompt"`
	Status      string          `json:"status"`
	Outcome     string          `json:"outcome,omitempty"`
	Exit        int             `json:"exit,omitempty"`
	Error       string          `json:"error,omitempty"`
	Result      string          `json:"result,omitempty"`
	ResultFrame json.RawMessage `json:"resultFrame,omitempty"`
	CreatedAt   int64           `json:"createdAt"`
	StartedAt   int64           `json:"startedAt,omitempty"`
	CompletedAt int64           `json:"completedAt,omitempty"`
	DeadlineAt  int64           `json:"deadlineAt,omitempty"`
	TimeoutMS   int64           `json:"timeoutMs,omitempty"`
	Collected   bool            `json:"collected,omitempty"`
}

// Claude normally replays a stream-json user envelope immediately before it
// begins model work. Bound that acknowledgement separately from the caller's
// execution timeout so a missing or malformed replay frame cannot strand a
// submitted turn forever. A native peer turn may legitimately run first; its
// terminal result refreshes this watchdog before the queued local turn resumes.
const claudeLaneSubmissionAckTimeout = 30 * time.Second
const claudeLaneManagerReadyTimeout = 35 * time.Second
const claudeLaneRetainedCollectedTurns = 64

type claudeLaneState struct {
	Type             string `json:"type"`
	Name             string `json:"name"`
	ThreadID         string `json:"threadId"`
	SessionID        string `json:"sessionId"`
	Cwd              string `json:"cwd"`
	OriginalCwd      string `json:"originalCwd,omitempty"`
	WorktreePath     string `json:"worktreePath,omitempty"`
	Status           string `json:"status"`
	ManagerPID       int    `json:"managerPid,omitempty"`
	ManagerProcStart string `json:"managerProcStart,omitempty"`
	ControlSocket    string `json:"controlSocket,omitempty"`
	ManagerLog       string `json:"managerLog,omitempty"`
	StartupID        string `json:"startupId,omitempty"`
	// Legacy proxy fields are read only so upgrades can retire <=0.0.5 lane
	// shims and aliases. New Claude lanes never populate them.
	ShimPID               int                `json:"shimPid,omitempty"`
	ShimProcStart         string             `json:"shimProcStart,omitempty"`
	ShimSocket            string             `json:"shimSocket,omitempty"`
	WorkerPID             int                `json:"workerPid,omitempty"`
	WorkerProcStart       string             `json:"workerProcStart,omitempty"`
	WorkerSocket          string             `json:"workerSocket,omitempty"`
	WorkerConfigDir       string             `json:"workerConfigDir,omitempty"`
	WorkerSecureConfigDir string             `json:"workerSecureConfigDir,omitempty"`
	WorkerSharedProfile   bool               `json:"workerSharedProfile,omitempty"`
	WorkerConfigEnvSet    bool               `json:"workerConfigEnvSet,omitempty"`
	WorkerConfigEnvValue  string             `json:"workerConfigEnvValue,omitempty"`
	WorkerSecureEnvSet    bool               `json:"workerSecureEnvSet,omitempty"`
	WorkerSessionStarted  bool               `json:"workerSessionStarted,omitempty"`
	WorkerSocketAlias     string             `json:"workerSocketAlias,omitempty"`
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
	Effort                string             `json:"effort,omitempty"`
	MaxBudgetUSD          string             `json:"maxBudgetUsd,omitempty"`
	Tools                 string             `json:"tools,omitempty"`
	ToolsSet              bool               `json:"toolsSet,omitempty"`
	AllowedTools          string             `json:"allowedTools,omitempty"`
	AllowedToolsSet       bool               `json:"allowedToolsSet,omitempty"`
	DisallowedTools       string             `json:"disallowedTools,omitempty"`
	DisallowedToolsSet    bool               `json:"disallowedToolsSet,omitempty"`
	OutputSchema          json.RawMessage    `json:"outputSchema,omitempty"`
	Bare                  bool               `json:"bare,omitempty"`
	Turns                 []claudeLaneTurn   `json:"turns,omitempty"`
	TurnID                string             `json:"turnId,omitempty"`
	LatestTurnID          string             `json:"latestTurnId,omitempty"`
	CollectedTurnID       string             `json:"collectedTurnId,omitempty"`
	TerminalOutcome       string             `json:"terminalOutcome,omitempty"`
	CleanupError          string             `json:"cleanupError,omitempty"`
	Groups                []string           `json:"groups,omitempty"`
	ExplicitGroups        []string           `json:"explicitGroups,omitempty"`
	ParentSessionID       string             `json:"parentSessionId,omitempty"`
	ParentHostID          string             `json:"parentHostId,omitempty"`
	ParentAgentRuntimeDir string             `json:"parentAgentRuntimeDir,omitempty"`
	InheritParentGroups   bool               `json:"inheritParentGroups,omitempty"`
	Notices               []claudeLaneNotice `json:"notices,omitempty"`
	CreatedAt             int64              `json:"createdAt"`
	UpdatedAt             int64              `json:"updatedAt"`
}

type claudeLaneNotice struct {
	ID            string `json:"id"`
	TurnID        string `json:"turnId"`
	Target        string `json:"target"`
	Message       string `json:"message"`
	CreatedAt     int64  `json:"createdAt"`
	LastAttemptAt int64  `json:"lastAttemptAt,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	SentAt        int64  `json:"sentAt,omitempty"`
}

func claudeLaneUsage() string {
	return `claude-peer-lane — named, messageable Claude Code lanes

Usage:
  claude-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  claude-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  claude-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  claude-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  claude-peer-lane status SESSION_OR_NAME
  claude-peer-lane interrupt SESSION_OR_NAME
  claude-peer-lane archive SESSION_OR_NAME
  claude-peer-lane list [--all] [--mine]
  claude-peer-lane doctor [--json]

Claude policy options are passed through without inventing a Codex sandbox mapping:
  -n, --name NAME
  -C, --cd DIR
  -m, --model MODEL
      --effort LEVEL
      --permission-mode MODE   acceptEdits|auto|bypassPermissions|manual|dontAsk|plan
      --max-budget-usd USD
      --tools LIST
      --allowed-tools LIST
      --disallowed-tools LIST
                               SendMessage,ListAgents are allowed by default
                               native cross-session input is accepted headlessly
      --schema FILE            passed to Claude as --json-schema
      --bare                   unavailable for messageable native-worker lanes
      --timeout SECONDS        run/start/resume: durable turn deadline
                               wait: collection-call bound; never interrupts
      --persistent             survive the recorded lifecycle owner
      --auto-archive-after S   archive after S seconds (minimum 0.001)
      --no-auto-archive        keep an idle completed lane available
      --notify PEER            persistent lanes: send terminal pointers here
      --no-notify              parent-owned lanes: suppress owner notification
      --worktree               create a detached git worktree for this lane
	  --group GROUP           add a child group; repeatable
	  --inherit-groups        also inherit the parent's non-private groups
	  --no-inherit-groups     retain only the mandatory parent anchor
      --prompt-file FILE
      --all                    include archived lanes in list
      --mine                   list only lanes owned by this orchestrator

The default permission mode is dontAsk: headless Claude cannot answer approval
prompts. A corroborated bypass owner is inherited unless a mode is explicit.
`
}

//nolint:gocyclo // The CLI contract is intentionally centralized and explicit.
func parseClaudeLaneArgs(argv []string) (claudeLaneOptions, error) {
	o := claudeLaneOptions{
		cwd: mustGetwd(), permissionMode: "dontAsk", autoArchive: true,
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
		takeAllowEmpty := func() (string, error) {
			if index+1 >= len(argv) {
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
			o.model = value
			o.modelSet = true
		case "--effort":
			value, err = take()
			o.effort = value
			o.effortSet = true
		case "--permission-mode":
			value, err = take()
			o.permissionMode = value
			o.permissionModeSet = true
		case "--max-budget-usd":
			value, err = take()
			o.maxBudgetUSD = value
			o.maxBudgetUSDSet = true
		case "--tools":
			value, err = takeAllowEmpty()
			o.tools = value
			o.toolsSet = true
		case "--allowed-tools":
			value, err = take()
			o.allowedTools = value
			o.allowedToolsSet = true
		case "--disallowed-tools":
			value, err = take()
			o.disallowedTools = value
			o.disallowedToolsSet = true
		case "--schema":
			value, err = take()
			o.schemaFile = value
			o.schemaSet = true
		case "--bare":
			o.bare = true
			o.bareSet = true
		case "--timeout":
			value, err = take()
			if err == nil {
				o.timeout, err = parseClaudeLaneSeconds(value, false, "--timeout")
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
				o.autoArchiveDelay, err = parseClaudeLaneSeconds(value, true, "--auto-archive-after")
				o.autoArchiveCustom = err == nil
			}
		case "--worktree":
			o.worktree = true
		case "--group":
			value, err = take()
			o.groupOptions.groups = append(o.groupOptions.groups, value)
			o.groupOptions.groupsSpecified = true
		case "--inherit-groups":
			o.groupOptions.inheritParentGroups, o.groupOptions.inheritGroupsSpecified = true, true
		case "--no-inherit-groups":
			o.groupOptions.inheritParentGroups, o.groupOptions.inheritGroupsSpecified = false, true
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
	if o.notifyTarget != "" && o.disableNotify {
		return o, errors.New("--notify and --no-notify cannot be used together")
	}
	if o.notifyExplicit && !o.persistent && o.command != "resume" {
		return o, errors.New("--notify requires --persistent; parent-owned lanes notify their owner automatically")
	}
	if o.autoArchiveCustom && !o.autoArchive {
		return o, errors.New("--auto-archive-after and --no-auto-archive cannot be used together")
	}
	if o.bare {
		return o, errors.New("--bare is incompatible with messageable Claude lanes because Claude Code does not publish a native peer socket in bare mode")
	}
	if o.autoArchiveCustom && !containsString([]string{"run", "start", "resume"}, o.command) {
		return o, fmt.Errorf("--auto-archive-after is not valid for %s", o.command)
	}
	if o.mine && o.command != "list" {
		return o, fmt.Errorf("--mine is not valid for %s", o.command)
	}
	if err := validateLaneGroupCommand(o.command, o.groupOptions); err != nil {
		return o, err
	}
	if !containsString([]string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"}, o.permissionMode) {
		return o, fmt.Errorf("unsupported Claude permission mode %q", o.permissionMode)
	}
	if err := validateClaudeLaneCommandOptions(o); err != nil {
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
		if o.worktree {
			return o, errors.New("resume reuses the lane cwd and cannot create a worktree")
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

func parseClaudeLaneSeconds(value string, positive bool, flag string) (time.Duration, error) {
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

func (o claudeLaneOptions) hasWorkerPolicyOptions() bool {
	return o.modelSet || o.effortSet || o.permissionModeSet || o.maxBudgetUSDSet ||
		o.toolsSet || o.allowedToolsSet || o.disallowedToolsSet || o.schemaSet || o.bareSet
}

func validateClaudeLaneCommandOptions(o claudeLaneOptions) error {
	invalid := func(flag string, set bool) error {
		if set {
			return fmt.Errorf("%s is not valid for %s", flag, o.command)
		}
		return nil
	}
	checks := [][2]any{}
	switch o.command {
	case "run", "start":
		checks = append(checks, [2]any{"--all", o.all}, [2]any{"--json", o.json})
	case "resume":
		checks = append(checks,
			[2]any{"--name", o.nameSet}, [2]any{"--cd", o.cwdSet}, [2]any{"--worktree", o.worktree},
			[2]any{"--all", o.all}, [2]any{"--json", o.json})
	case "wait":
		checks = append(checks, claudeLaneNonWaitOptions(o)...)
	case "list":
		checks = append(checks, claudeLaneOperationalOptions(o)...)
		checks = append(checks, [2]any{"--json", o.json}, [2]any{"--timeout", o.timeoutSet})
	case "doctor":
		checks = append(checks, claudeLaneOperationalOptions(o)...)
		checks = append(checks, [2]any{"--all", o.all}, [2]any{"--timeout", o.timeoutSet})
	default:
		checks = append(checks, claudeLaneOperationalOptions(o)...)
		checks = append(checks, [2]any{"--all", o.all}, [2]any{"--json", o.json}, [2]any{"--timeout", o.timeoutSet})
	}
	for _, check := range checks {
		if err := invalid(check[0].(string), check[1].(bool)); err != nil {
			return err
		}
	}
	return nil
}

func claudeLaneOperationalOptions(o claudeLaneOptions) [][2]any {
	return [][2]any{
		{"--name", o.nameSet}, {"--cd", o.cwdSet}, {"--model/policy", o.hasWorkerPolicyOptions()},
		{"--prompt-file", o.promptFile != ""}, {"--notify", o.notifyExplicit}, {"--no-notify", o.disableNotify},
		{"--persistent", o.persistentSet}, {"--no-auto-archive", o.noAutoArchiveSet},
		{"--auto-archive-after", o.autoArchiveCustom}, {"--worktree", o.worktree},
		{"--allow-duplicate-name", o.allowDuplicateName}, {"-", o.stdinMarker},
	}
}

func claudeLaneNonWaitOptions(o claudeLaneOptions) [][2]any {
	checks := claudeLaneOperationalOptions(o)
	return append(checks, [2]any{"--all", o.all}, [2]any{"--json", o.json})
}

func runClaudeLaneCommand(argv []string) int {
	o, err := parseClaudeLaneArgs(argv)
	if err != nil {
		_ = emitLane(map[string]any{"type": "error", "message": err.Error()})
		fmt.Fprintf(os.Stderr, "claude-peer-lane: %v\n", err)
		return 1
	}
	if o.help {
		fmt.Print(claudeLaneUsage())
		return 0
	}
	o = withClaudeLaneLaunchContext(o)
	var code int
	switch o.command {
	case "run":
		code, err = startClaudeLane(o, true)
	case "start":
		code, err = startClaudeLane(o, false)
	case "resume":
		code, err = resumeClaudeLane(o)
	case "wait":
		code, err = waitClaudeLane(o)
	case "status":
		code, err = statusClaudeLane(o)
	case "interrupt":
		code, err = interruptClaudeLane(o)
	case "archive":
		code, err = archiveClaudeLane(o)
	case "list":
		code, err = listClaudeLanes(o)
	case "doctor":
		code, err = doctorClaudeLane()
	}
	if err != nil {
		_ = emitLane(map[string]any{"type": "error", "message": err.Error(), "timeout": errors.Is(err, context.DeadlineExceeded)})
		fmt.Fprintf(os.Stderr, "claude-peer-lane: %v\n", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return 124
		}
		return 1
	}
	return code
}

func withClaudeLaneLaunchContext(o claudeLaneOptions) claudeLaneOptions {
	listMine := o.command == "list" && o.mine
	if !containsString([]string{"run", "start", "resume"}, o.command) && !listMine {
		return o
	}
	owner := inferPeerParent(resolveNativePaths(), os.Getpid())
	return withClaudeLaneResolvedParent(o, owner)
}

func withClaudeLaneResolvedParent(o claudeLaneOptions, owner laneOwner) claudeLaneOptions {
	listMine := o.command == "list" && o.mine
	o.groupOptions = applyAgentParentContext(o.groupOptions, &owner)
	ok := owner.SessionID != ""
	if ok {
		o.groupOptions.parentSessionID = owner.SessionID
		if !o.persistent || listMine {
			o = applyClaudeLaneOwnerContext(o, owner)
		}
		if !listMine && !o.persistent && !o.disableNotify {
			o.notifyTarget = "session:" + owner.SessionID
		}
		return o
	}
	if listMine {
		return o
	}
	return o
}

func applyClaudeLaneOwnerContext(o claudeLaneOptions, owner laneOwner) claudeLaneOptions {
	o.ownerPID, o.ownerProcStart, o.ownerSessionID = owner.PID, owner.ProcStart, owner.SessionID
	if !o.permissionModeSet && owner.PermissionMode == "bypassPermissions" {
		o.permissionMode = "bypassPermissions"
	}
	return o
}

func inferCodexParent(paths nativePaths, startPID int) (laneOwner, bool) {
	// Codex exposes one shared host process rather than a per-thread host PID.
	// Its ancestry therefore proves that this is a Codex launch context, while
	// the live session bridge row/state pair supplies the lifecycle identity for
	// the specific CODEX_THREAD_ID. Never treat the environment value alone as
	// either proof.
	hostPID := findCodexHostOwnerPID(startPID)
	sessionID := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
	hostObservation := observeProcessIdentity(hostPID, "")
	if hostPID <= 1 || !validSessionID(sessionID) || !processHasAncestor(startPID, hostPID) ||
		hostObservation.Status != processIdentityMatches {
		return laneOwner{}, false
	}
	peers, err := listNativePeerSessions(paths)
	if err != nil {
		return laneOwner{}, false
	}
	peer, ok := matchingCodexOwnerPeer(peers, sessionID)
	if !ok {
		return laneOwner{}, false
	}
	bridgeState, stateErr := readOwnNativeState(paths, sessionID)
	if stateErr != nil || !codexBridgeStateMatchesPeer(bridgeState, peer) {
		return laneOwner{}, false
	}
	permissionMode := corroboratedOwnerPermissionMode(peer, peer.PID)
	if liveMode := stringValue(bridgeState["permissionMode"]); liveMode == "default" || liveMode == "bypassPermissions" {
		permissionMode = liveMode
	}
	return laneOwner{PID: peer.PID, ProcStart: peer.ProcStart, SessionID: sessionID, PermissionMode: permissionMode}, true
}

func matchingCodexOwnerPeer(peers []peerSession, sessionID string) (peerSession, bool) {
	for _, peer := range peers {
		if peer.SessionID == sessionID && peer.Entrypoint == "codex" {
			return peer, true
		}
	}
	return peerSession{}, false
}

func codexBridgeStateMatchesPeer(state map[string]any, peer peerSession) bool {
	return intValue(state["pid"]) == peer.PID && stringValue(state["procStart"]) == peer.ProcStart &&
		stringValue(state["sessionId"]) == peer.SessionID && samePath(stringValue(state["socketPath"]), peer.MessagingSocketPath)
}

func corroboratedOwnerPermissionMode(peer peerSession, pid int) string {
	permissionMode, err := peerProcessPermissionMode(pid, peer.ProcStart)
	if err != nil {
		return "default"
	}
	// A local registry row is discovery data, not a policy authority. Accept its
	// declared class only when the live process argv independently agrees.
	if peer.PermissionMode != "" && peer.PermissionMode != permissionMode {
		return "default"
	}
	return permissionMode
}

// findCodexHostOwnerPID recognizes only a real Codex host. A native lane
// launcher must not become its own lifecycle owner.
func findCodexHostOwnerPID(startPID int) int {
	visited := map[int]bool{}
	pid := startPID
	for pid > 1 && !visited[pid] {
		visited[pid] = true
		args, err := readProcessArgs(pid)
		if err == nil && processArgsLookLikeCodexHost(args) {
			return pid
		}
		parent, ok := parentProcessID(pid)
		if !ok {
			return 0
		}
		pid = parent
	}
	return 0
}

func processArgsLookLikeCodexHost(args []string) bool {
	return procinfo.LooksLikeCodexHost(args)
}

func claudeLaneStatePath(paths nativePaths, sessionID string) string {
	return filepath.Join(profileDataRoot(paths), "claude-lanes", sessionKey(sessionID)+".json")
}

func claudeLaneControlSocket(paths nativePaths, sessionID string) string {
	return filepath.Join(bridgeRuntimeRoot(paths.runtimeDir, os.Getuid()), "cl-"+sessionKey(sessionID)+".sock")
}

func validateClaudeLaneSocketPath(path string) error {
	limit := 107
	if runtimeGOOS() == "darwin" {
		limit = 103
	}
	if len([]byte(path)) > limit {
		return fmt.Errorf("claude lane control socket path is %d bytes; platform limit is %d: %s", len([]byte(path)), limit, path)
	}
	return nil
}

func readClaudeLaneState(paths nativePaths, sessionID string) (claudeLaneState, error) {
	body, err := os.ReadFile(claudeLaneStatePath(paths, sessionID))
	if err != nil {
		return claudeLaneState{}, err
	}
	var state claudeLaneState
	if err := json.Unmarshal(body, &state); err != nil {
		return claudeLaneState{}, err
	}
	if state.SessionID != sessionID || state.Type != "claude-peer-lane" {
		return claudeLaneState{}, errors.New("claude lane state/session mismatch")
	}
	return state, nil
}

func writeClaudeLaneState(paths nativePaths, state claudeLaneState) error {
	lock, err := lockLaneStateFile(paths, "claude-"+state.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	return writeClaudeLaneStateUnlocked(paths, state)
}

func writeClaudeLaneStateUnlocked(paths nativePaths, state claudeLaneState) error {
	state.UpdatedAt = time.Now().UnixMilli()
	return writeJSONAtomic(claudeLaneStatePath(paths, state.SessionID), state)
}

func readClaudeLaneStates(paths nativePaths) []claudeLaneState {
	directory := filepath.Join(profileDataRoot(paths), "claude-lanes")
	entries, _ := os.ReadDir(directory)
	states := []claudeLaneState{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var state claudeLaneState
		body, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // directory is bridge-owned.
		if err != nil || json.Unmarshal(body, &state) != nil || state.Type != "claude-peer-lane" || entry.Name() != sessionKey(state.SessionID)+".json" {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].CreatedAt > states[j].CreatedAt })
	return states
}

func resolveClaudeLaneState(paths nativePaths, target string) (claudeLaneState, error) {
	target = strings.TrimSpace(target)
	states := readClaudeLaneStates(paths)
	byID := []claudeLaneState{}
	byName := []claudeLaneState{}
	for _, state := range states {
		if state.SessionID == target || state.ThreadID == target {
			byID = append(byID, state)
		}
		if strings.EqualFold(state.Name, target) {
			byName = append(byName, state)
		}
	}
	if len(byID) == 1 {
		return byID[0], nil
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		active := []claudeLaneState{}
		for _, state := range byName {
			if state.Status != "archived" {
				active = append(active, state)
			}
		}
		if len(active) == 1 {
			return active[0], nil
		}
		return claudeLaneState{}, fmt.Errorf("claude lane name %q is ambiguous; use a session ID", target)
	}
	return claudeLaneState{}, fmt.Errorf("no Claude lane matching %q", target)
}

func readClaudeLanePrompt(o claudeLaneOptions) (string, error) {
	return readLanePrompt(laneOptions{promptFile: o.promptFile})
}

func readClaudeLaneSchema(file string) (json.RawMessage, error) {
	if file == "" {
		return nil, nil
	}
	return readLaneOutputSchema(file)
}

func newClaudeLaneTurn(prompt string, timeout time.Duration) claudeLaneTurn {
	now := time.Now().UnixMilli()
	turn := claudeLaneTurn{ID: randomID(), Prompt: prompt, Status: "queued", CreatedAt: now, TimeoutMS: timeout.Milliseconds()}
	return turn
}

//nolint:gocyclo // Startup keeps name, worktree, manager, and rollback ownership in one transaction.
func startClaudeLane(o claudeLaneOptions, wait bool) (int, error) {
	if err := validateLaneOwner(o.persistent, o.ownerPID, o.ownerProcStart); err != nil {
		return 1, err
	}
	prompt, err := readClaudeLanePrompt(o)
	if err != nil {
		return 1, err
	}
	o.outputSchema, err = readClaudeLaneSchema(o.schemaFile)
	if err != nil {
		return 1, err
	}
	paths := resolveNativePaths()
	profileSource, err := sharedClaudeLaneSource(paths)
	if err != nil {
		return 1, err
	}
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
	// Group visibility, not a host-global registry, scopes lane names. Exact
	// session IDs remain authoritative when a visible name is ambiguous.
	cwd := absolutePath(o.cwd)
	originalCwd := ""
	worktreePath := ""
	if o.worktree {
		originalCwd = cwd
		worktreePath, err = createLaneWorktree(paths, name, cwd)
		if err != nil {
			return 1, err
		}
		cwd = worktreePath
	}
	setupReady := false
	defer func() {
		if !setupReady && worktreePath != "" {
			_ = removeLaneWorktree(originalCwd, worktreePath)
		}
	}()
	sessionID := randomID()
	turn := newClaudeLaneTurn(prompt, o.timeout)
	now := time.Now().UnixMilli()
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: name, ThreadID: sessionID, SessionID: sessionID,
		Cwd: cwd, OriginalCwd: originalCwd, WorktreePath: worktreePath, Status: "starting",
		ControlSocket: claudeLaneControlSocket(paths, sessionID), StartupID: randomID(),
		ManagerLog: filepath.Join(profileDataRoot(paths), "claude-lane-logs", sessionKey(sessionID)+".log"), OwnerPID: o.ownerPID,
		WorkerConfigDir: profileSource.ConfigRoot, WorkerSecureConfigDir: profileSource.SecureConfig,
		WorkerSharedProfile: true,
		WorkerConfigEnvSet:  profileSource.ConfigEnvSet, WorkerConfigEnvValue: profileSource.ConfigEnvValue,
		WorkerSecureEnvSet: profileSource.SecureEnvSet,
		OwnerProcStart:     o.ownerProcStart, OwnerSessionID: o.ownerSessionID,
		NotifyTarget: o.notifyTarget, Persistent: o.persistent, AutoArchive: o.autoArchive,
		AutoArchiveDelayMS: o.autoArchiveDelay.Milliseconds(), PermissionMode: o.permissionMode,
		Model: o.model, Effort: o.effort, MaxBudgetUSD: o.maxBudgetUSD, Tools: o.tools, ToolsSet: o.toolsSet,
		AllowedTools: o.allowedTools, AllowedToolsSet: o.allowedToolsSet,
		DisallowedTools: o.disallowedTools, DisallowedToolsSet: o.disallowedToolsSet, OutputSchema: o.outputSchema,
		Bare: o.bare, Turns: []claudeLaneTurn{turn}, TurnID: turn.ID, LatestTurnID: turn.ID,
		CreatedAt: now, UpdatedAt: now,
	}
	groupState, alwaysApprove, err := resolveLaneGroupState(
		sessionID, "claude", o.groupOptions,
		o.permissionMode == "bypassPermissions", true,
	)
	if err != nil {
		return 1, fmt.Errorf("resolve lane groups: %w", err)
	}
	if alwaysApprove {
		state.PermissionMode = "bypassPermissions"
	}
	state.Groups, state.ExplicitGroups = groupState.Groups, groupState.ExplicitGroups
	state.ParentSessionID, state.InheritParentGroups = groupState.ParentSessionID, groupState.InheritParentGroups
	state.ParentHostID = groupState.ParentHostID
	state.ParentAgentRuntimeDir = groupState.ParentAgentRuntimeDir
	if err := writeClaudeLaneState(paths, state); err != nil {
		return 1, err
	}
	// The durable state now reserves the name; do not serialize unrelated lane
	// starts behind Claude process startup and registry publication.
	unlockLaneStateFile(nameLock)
	nameLock = nil
	managerPID, err := spawnClaudeLaneManager(sessionID)
	if err != nil {
		_ = os.Remove(claudeLaneStatePath(paths, sessionID))
		return 1, err
	}
	ready, err := waitClaudeLaneReady(paths, sessionID, managerPID, claudeLaneManagerReadyTimeout)
	if err != nil {
		_ = syscall.Kill(managerPID, syscall.SIGTERM)
		if archiveErr := forceArchiveClaudeLane(paths, sessionID, "manager readiness failed"); archiveErr == nil {
			setupReady = true
			worktreeErr := error(nil)
			if worktreePath != "" {
				worktreeErr = removeLaneWorktree(originalCwd, worktreePath)
			}
			if worktreeErr == nil {
				if removeErr := os.Remove(claudeLaneStatePath(paths, sessionID)); removeErr != nil && !os.IsNotExist(removeErr) && worktreePath != "" {
					if retained, readErr := readClaudeLaneState(paths, sessionID); readErr == nil {
						retained.Cwd, retained.WorktreePath = originalCwd, ""
						_ = writeClaudeLaneState(paths, retained)
					}
				}
			}
		} else {
			// Retain both the archived record and its cwd if cleanup could not
			// prove that every manager-owned process was retired.
			setupReady = true
		}
		return 1, err
	}
	setupReady = true
	_ = emitLane(map[string]any{"type": "thread.started", "thread_id": sessionID, "session_id": sessionID})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": sessionID, "turn_id": turn.ID})
	if err := emitClaudeLaneReady(ready); err != nil {
		return 1, err
	}
	if !wait {
		return 0, nil
	}
	return waitClaudeLane(claudeLaneOptions{target: sessionID, command: "wait", timeout: claudeLaneCollectionBound(o.timeout)})
}

func spawnClaudeLaneManager(sessionID string) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	command := exec.Command(executable, "claude-lane-manager", "--session-id", sessionID) //nolint:gosec // current installed bridge.
	paths := resolveNativePaths()
	logPath := filepath.Join(profileDataRoot(paths), "claude-lane-logs", sessionKey(sessionID)+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) //nolint:gosec // path is derived from a hashed bridge-owned session id.
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()
	command.Stdin, command.Stdout, command.Stderr = nil, logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	return pid, nil
}

func waitClaudeLaneReady(paths nativePaths, sessionID string, managerPID int, timeout time.Duration) (claudeLaneState, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := readClaudeLaneState(paths, sessionID)
		if err == nil && state.ManagerPID == managerPID && state.Status != "starting" && state.WorkerSocket != "" && probeUnixSocket(state.WorkerSocket, 200*time.Millisecond) {
			return state, nil
		}
		if observeProcessIdentity(managerPID, "").Status == processIdentityStale {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return claudeLaneState{}, errors.New("timed out starting Claude lane manager")
}

func emitClaudeLaneReady(state claudeLaneState) error {
	return emitLane(map[string]any{
		"type": "lane.ready", "contract_version": claudeLaneContractVersion,
		"product": "claude", "name": state.Name, "thread_id": state.ThreadID,
		"session_id": state.SessionID, "turn_id": state.TurnID, "cwd": state.Cwd,
		"address": encodeNativeAddress(state.WorkerSocket), "worktree_path": emptyStringAsNil(state.WorktreePath),
		"notify_target": emptyStringAsNil(state.NotifyTarget), "owner_session_id": emptyStringAsNil(state.OwnerSessionID),
		"persistent": state.Persistent, "auto_archive": state.AutoArchive,
		"auto_archive_after_seconds": float64(state.AutoArchiveDelayMS) / 1000,
	})
}

//nolint:gocyclo // Resume separates live-manager and durable restart paths explicitly.
func resumeClaudeLane(o claudeLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveClaudeLaneState(paths, o.target)
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
	prompt, err := readClaudeLanePrompt(o)
	if err != nil {
		return 1, err
	}
	if o.schemaFile != "" {
		o.outputSchema, err = readClaudeLaneSchema(o.schemaFile)
		if err != nil {
			return 1, err
		}
	}
	if err := validateClaudeLaneResumeState(state); err != nil {
		return 1, err
	}
	if debt := firstClaudeLaneDebt(state); debt != "" {
		return 1, fmt.Errorf("collect outstanding Claude lane turn %s before resume", debt)
	}
	groupState, alwaysApprove, err := resolveLaneGroupState(
		state.SessionID, "claude", o.groupOptions,
		o.permissionMode == "bypassPermissions", o.permissionModeSet,
	)
	if err != nil {
		return 1, fmt.Errorf("resolve lane groups: %w", err)
	}
	if alwaysApprove && !o.permissionModeSet {
		o.permissionMode = "bypassPermissions"
	}
	turn := newClaudeLaneTurn(prompt, o.timeout)
	managerIdentity := cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart)
	if managerIdentity.Status == processIdentityUnknown && corroboratedLegacyClaudeLaneManager(paths, state) {
		// Pre-Darwin-attestation managers persisted no start token. Their exact
		// role, session argument, deterministic control path, and kernel socket
		// peer PID are sufficient to keep using their own control protocol, but
		// never to signal or otherwise authorize the tokenless process.
		managerIdentity.Status = processIdentityMatches
	}
	if state.Status != "archived" && managerIdentity.Status == processIdentityUnknown {
		return 1, errors.New("cannot currently corroborate the Claude lane manager; preserve it and retry, or stop a legacy manager normally before retrying")
	}
	if state.Status == "archived" || managerIdentity.Status == processIdentityStale || !probeUnixSocket(state.ControlSocket, 250*time.Millisecond) {
		var originalState claudeLaneState
		nameLock, nameErr := lockLaneNames(paths)
		if nameErr != nil {
			return 1, nameErr
		}
		defer func() {
			if nameLock != nil {
				unlockLaneStateFile(nameLock)
			}
		}()
		lifecycle, lockErr := lockLaneLifecycle(paths, "claude-"+state.SessionID)
		if lockErr != nil {
			return 1, lockErr
		}
		latest, readErr := readClaudeLaneState(paths, state.SessionID)
		if readErr != nil {
			unlockLaneLifecycle(lifecycle)
			return 1, readErr
		}
		state = latest
		originalState = latest
		if debt := firstClaudeLaneDebt(state); debt != "" {
			unlockLaneLifecycle(lifecycle)
			return 1, fmt.Errorf("collect outstanding Claude lane turn %s before resume", debt)
		}
		if cleanupErr := cleanupClaudeLaneResidue(paths, state, os.Getpid()); cleanupErr != nil {
			unlockLaneLifecycle(lifecycle)
			return 1, cleanupErr
		}
		startupID := randomID()
		state.Status = "starting"
		state.StartupID = startupID
		state.ManagerPID, state.ManagerProcStart, state.ShimPID, state.WorkerPID = 0, "", 0, 0
		state.ShimProcStart, state.ShimSocket, state.WorkerProcStart, state.WorkerSocket, state.WorkerSocketAlias = "", "", "", "", ""
		state.AutoArchiveAt = 0
		applyClaudeLaneResumeOptions(&state, o)
		applyClaudeLaneWorkerOptions(&state, o)
		if alwaysApprove {
			state.PermissionMode = "bypassPermissions"
		}
		state.Groups, state.ExplicitGroups = groupState.Groups, groupState.ExplicitGroups
		state.ParentSessionID, state.InheritParentGroups = groupState.ParentSessionID, groupState.InheritParentGroups
		state.ParentHostID = groupState.ParentHostID
		state.ParentAgentRuntimeDir = groupState.ParentAgentRuntimeDir
		state.Turns = append(state.Turns, turn)
		state.TurnID, state.LatestTurnID = oldestClaudeLaneDebt(state), turn.ID
		if err := writeClaudeLaneState(paths, state); err != nil {
			unlockLaneLifecycle(lifecycle)
			return 1, err
		}
		pid, spawnErr := spawnClaudeLaneManager(state.SessionID)
		unlockLaneLifecycle(lifecycle)
		unlockLaneStateFile(nameLock)
		nameLock = nil
		if spawnErr != nil {
			rollbackClaudeLaneResume(paths, originalState, startupID, 0)
			return 1, spawnErr
		}
		state, err = waitClaudeLaneReady(paths, state.SessionID, pid, claudeLaneManagerReadyTimeout)
		if err != nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			rollbackClaudeLaneResume(paths, originalState, startupID, pid)
			return 1, err
		}
	} else {
		if o.hasWorkerPolicyOptions() {
			return 1, errors.New("claude worker policy options can change only when resuming an archived lane")
		}
		request := map[string]any{
			"action": "resume", "sessionId": state.SessionID, "turn": turn,
			"groups": groupState.Groups, "explicitGroups": groupState.ExplicitGroups,
			"parentSessionId": groupState.ParentSessionID, "parentHostId": groupState.ParentHostID,
			"parentAgentRuntimeDir": groupState.ParentAgentRuntimeDir,
			"inheritParentGroups":   groupState.InheritParentGroups,
		}
		if o.autoArchiveCustom {
			request["autoArchiveDelayMs"] = o.autoArchiveDelay.Milliseconds()
		}
		if !o.autoArchive {
			request["autoArchive"] = false
		}
		request["persistent"] = desiredPersistent
		if !desiredPersistent {
			request["ownerPid"], request["ownerProcStart"], request["ownerSessionId"] = o.ownerPID, o.ownerProcStart, o.ownerSessionID
		}
		switch {
		case o.notifyExplicit:
			request["notifySet"], request["notifyTarget"] = true, o.notifyTarget
		case o.disableNotify:
			request["notifySet"], request["notifyTarget"] = true, ""
		case !desiredPersistent:
			request["notifySet"] = true
			if o.ownerSessionID != "" {
				request["notifyTarget"] = "session:" + o.ownerSessionID
			} else {
				request["notifyTarget"] = ""
			}
		}
		response, requestErr := requestControl(state.ControlSocket, request, 5*time.Second)
		if requestErr != nil {
			return 1, requestErr
		}
		turn.ID = defaultString(stringValue(response["turnId"]), turn.ID)
		latest, readErr := readClaudeLaneState(paths, state.SessionID)
		if readErr != nil {
			return 1, readErr
		}
		state = latest
	}
	_ = emitLane(map[string]any{"type": "thread.resumed", "thread_id": state.SessionID, "session_id": state.SessionID})
	_ = emitLane(map[string]any{"type": "turn.started", "thread_id": state.SessionID, "turn_id": turn.ID})
	if err := emitClaudeLaneReady(state); err != nil {
		return 1, err
	}
	return waitClaudeLane(claudeLaneOptions{target: state.SessionID, command: "wait", timeout: claudeLaneCollectionBound(o.timeout)})
}

func corroboratedLegacyClaudeLaneManager(paths nativePaths, state claudeLaneState) bool {
	if state.ManagerPID <= 1 || state.ManagerProcStart != "" || state.SessionID == "" ||
		!samePath(state.ControlSocket, claudeLaneControlSocket(paths, state.SessionID)) {
		return false
	}
	args, err := readProcessArgs(state.ManagerPID)
	if err != nil || !processArgsIdentifyClaudeLaneManager(args, state.SessionID) {
		return false
	}
	conn, err := net.DialTimeout("unix", state.ControlSocket, 250*time.Millisecond) // #nosec G704 -- path is the deterministic private Claude lane control socket.
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	peerPID, err := unixPeerPID(conn)
	return err == nil && peerPID == state.ManagerPID
}

func processArgsIdentifyClaudeLaneManager(args []string, sessionID string) bool {
	if len(args) < 4 || !sessionRuntimeBasename(filepath.Base(args[0])) || args[1] != "claude-lane-manager" {
		return false
	}
	for index := 2; index+1 < len(args); index++ {
		if args[index] == "--session-id" {
			return args[index+1] == sessionID
		}
	}
	return false
}

func sessionRuntimeBasename(base string) bool {
	return base == "agent-session-runtime" || base == "codex-messaging"
}

func validateClaudeLaneResumeState(state claudeLaneState) error {
	if state.Bare {
		return errors.New("this legacy lane used --bare and cannot publish the native peer socket required by current Claude lanes; archive it and start a new lane")
	}
	if !state.WorkerSharedProfile {
		return errors.New("legacy private Claude lane cannot resume in shared-profile mode; archive it and start a new lane")
	}
	if laneAgentConfigured() {
		status, err := federator.ReadAgentStatus(laneAgentRuntimeDir())
		if err != nil {
			return fmt.Errorf("read host agent shared Claude registry: %w", err)
		}
		if err := validateClaudeLaneAgentProfile(state, status); err != nil {
			return err
		}
	}
	return nil
}

func validateClaudeLaneAgentProfile(state claudeLaneState, status federator.AgentStatus) error {
	if !samePath(filepath.Join(state.WorkerConfigDir, "sessions"), status.RegistryDir) {
		return fmt.Errorf(
			"archived Claude lane profile %s does not match host agent registry %s",
			state.WorkerConfigDir, status.RegistryDir,
		)
	}
	profile := claudeLaneAgentProfile(status)
	if state.WorkerConfigEnvSet != profile.ConfigEnvSet ||
		(state.WorkerConfigEnvSet && state.WorkerConfigEnvValue != profile.ConfigEnvValue) ||
		state.WorkerSecureEnvSet != profile.SecureEnvSet ||
		(state.WorkerSecureEnvSet && state.WorkerSecureConfigDir != profile.SecureConfig) {
		return errors.New("archived Claude lane credential namespace does not match the running host agent")
	}
	return nil
}

func applyClaudeLaneWorkerOptions(state *claudeLaneState, o claudeLaneOptions) {
	if o.modelSet {
		state.Model = o.model
	}
	if o.effortSet {
		state.Effort = o.effort
	}
	if o.permissionModeSet {
		state.PermissionMode = o.permissionMode
	}
	if o.maxBudgetUSDSet {
		state.MaxBudgetUSD = o.maxBudgetUSD
	}
	if o.toolsSet {
		state.Tools, state.ToolsSet = o.tools, true
	}
	if o.allowedToolsSet {
		state.AllowedTools, state.AllowedToolsSet = o.allowedTools, true
	}
	if o.disallowedToolsSet {
		state.DisallowedTools, state.DisallowedToolsSet = o.disallowedTools, true
	}
	if o.schemaSet {
		state.OutputSchema = o.outputSchema
	}
	if o.bareSet {
		state.Bare = o.bare
	}
}

func claudeLaneCollectionBound(turnDeadline time.Duration) time.Duration {
	if turnDeadline <= 0 {
		return 0
	}
	return turnDeadline + 30*time.Second
}

func applyClaudeLaneResumeOptions(state *claudeLaneState, o claudeLaneOptions) {
	if o.autoArchiveCustom {
		state.AutoArchive, state.AutoArchiveDelayMS = true, o.autoArchiveDelay.Milliseconds()
	}
	if !o.autoArchive {
		state.AutoArchive, state.AutoArchiveAt = false, 0
	}
	if o.persistentSet {
		state.Persistent = true
	}
	if state.Persistent {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = 0, "", ""
	} else {
		state.OwnerPID, state.OwnerProcStart, state.OwnerSessionID = o.ownerPID, o.ownerProcStart, o.ownerSessionID
	}
	switch {
	case o.notifyExplicit:
		state.NotifyTarget = o.notifyTarget
	case o.disableNotify:
		state.NotifyTarget = ""
	case !state.Persistent:
		if o.ownerSessionID != "" {
			state.NotifyTarget = "session:" + o.ownerSessionID
		} else {
			state.NotifyTarget = ""
		}
	}
}

func oldestClaudeLaneDebt(state claudeLaneState) string {
	for _, turn := range state.Turns {
		if !turn.Collected {
			return turn.ID
		}
	}
	return state.LatestTurnID
}

func firstClaudeLaneDebt(state claudeLaneState) string {
	for _, turn := range state.Turns {
		if !turn.Collected {
			return turn.ID
		}
	}
	return ""
}

func rollbackClaudeLaneResume(paths nativePaths, original claudeLaneState, startupID string, managerPID int) {
	deadline := time.Now().Add(2 * time.Second)
	for managerPID > 1 && observeProcessIdentity(managerPID, "").Status != processIdentityStale && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	lock, err := lockLaneLifecycle(paths, "claude-"+original.SessionID)
	if err != nil {
		return
	}
	defer unlockLaneLifecycle(lock)
	latest, err := readClaudeLaneState(paths, original.SessionID)
	if err != nil || latest.StartupID != startupID ||
		processIdentityMayBeLive(latest.ManagerPID, latest.ManagerProcStart) {
		return
	}
	original.ManagerPID, original.ManagerProcStart = 0, ""
	original.WorkerPID, original.WorkerProcStart, original.WorkerSocket, original.WorkerSocketAlias = 0, "", "", ""
	original.ShimPID, original.ShimSocket, original.StartupID = 0, "", ""
	_ = writeClaudeLaneState(paths, original)
}

func waitClaudeLane(o claudeLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveClaudeLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	collectionLock, err := lockClaudeLaneCollection(paths, state.SessionID)
	if err != nil {
		return 1, err
	}
	defer unlockLaneStateFile(collectionLock)
	deadline := time.Time{}
	if o.timeout > 0 {
		deadline = time.Now().Add(o.timeout)
	}
	for {
		latest, readErr := readClaudeLaneState(paths, state.SessionID)
		if readErr != nil {
			return 1, readErr
		}
		state = latest
		for _, turn := range state.Turns {
			if turn.Collected || !containsString([]string{"completed", "failed", "interrupted", "timed_out"}, turn.Status) {
				continue
			}
			if err := emitClaudeLaneTurn(state, turn); err != nil {
				return 1, err
			}
			if err := acknowledgeClaudeLaneTurn(paths, state.SessionID, turn.ID); err != nil {
				return 1, err
			}
			return turn.Exit, nil
		}
		if state.Status == "archived" {
			return 1, fmt.Errorf("claude lane %s is archived and has no collectable turn", state.SessionID)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return 124, context.DeadlineExceeded
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func lockClaudeLaneCollection(paths nativePaths, sessionID string) (*os.File, error) {
	directory := filepath.Join(profileDataRoot(paths), "claude-lane-collection-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionKey(sessionID)+".lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // session id is hashed.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func emitClaudeLaneTurn(state claudeLaneState, turn claudeLaneTurn) error {
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
	var result map[string]any
	_ = json.Unmarshal(turn.ResultFrame, &result)
	accounting := map[string]any{
		"started_at": nilIfZero(turn.StartedAt), "completed_at": nilIfZero(turn.CompletedAt),
		"duration_ms": nil, "cost": result["total_cost_usd"], "cost_available": result["total_cost_usd"] != nil,
		"tokens": result["usage"],
	}
	if turn.StartedAt > 0 && turn.CompletedAt >= turn.StartedAt {
		accounting["duration_ms"] = turn.CompletedAt - turn.StartedAt
	}
	return emitLane(map[string]any{
		"type": "turn.completed", "thread_id": state.SessionID, "turn_id": turn.ID,
		"status": turn.Status, "outcome": turn.Outcome, "exit": turn.Exit,
		"error": emptyStringAsNil(turn.Error), "usage": result["usage"], "accounting": accounting,
	})
}

func acknowledgeClaudeLaneTurn(paths nativePaths, sessionID, turnID string) error {
	lock, err := lockLaneStateFile(paths, "claude-"+sessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	state, err := readClaudeLaneState(paths, sessionID)
	if err != nil {
		return err
	}
	for index := range state.Turns {
		if state.Turns[index].ID == turnID {
			state.Turns[index].Collected = true
			state.Turns[index].ResultFrame = nil
			state.CollectedTurnID = turnID
			break
		}
	}
	for index := range state.Notices {
		if state.Notices[index].TurnID == turnID && state.Notices[index].SentAt == 0 {
			state.Notices[index].SentAt = time.Now().UnixMilli()
		}
	}
	state.TurnID = oldestClaudeLaneDebt(state)
	if state.Status == "archived" {
		state.AutoArchiveAt = 0
	}
	return writeClaudeLaneStateUnlocked(paths, state)
}

func statusClaudeLane(o claudeLaneOptions) (int, error) {
	state, err := resolveClaudeLaneState(resolveNativePaths(), o.target)
	if err != nil {
		return 1, err
	}
	return 0, emitLane(claudeLaneStatusEvent(state))
}

func claudeLaneStatusEvent(state claudeLaneState) map[string]any {
	var outcome, exit any
	if turn := claudeLaneReportedTurn(state); turn != nil && turn.Outcome != "" {
		outcome, exit = turn.Outcome, turn.Exit
	}
	return map[string]any{
		"type": "lane.status", "product": "claude", "name": state.Name,
		"thread_id": state.ThreadID, "session_id": state.SessionID, "cwd": state.Cwd,
		"status": state.Status, "turn_id": emptyStringAsNil(state.TurnID), "turn_status": claudeLaneTurnStatus(state),
		"collected_turn_id": emptyStringAsNil(state.CollectedTurnID),
		"worktree_path":     emptyStringAsNil(state.WorktreePath), "notify_target": emptyStringAsNil(state.NotifyTarget),
		"persistent": state.Persistent, "owner_session_id": emptyStringAsNil(state.OwnerSessionID),
		"auto_archive": state.AutoArchive, "auto_archive_after_seconds": float64(state.AutoArchiveDelayMS) / 1000,
		"auto_archive_at": nilIfZero(state.AutoArchiveAt), "outcome": outcome, "exit": exit,
	}
}

func claudeLaneReportedTurn(state claudeLaneState) *claudeLaneTurn {
	for index := range state.Turns {
		if state.Turns[index].ID == state.TurnID {
			return &state.Turns[index]
		}
	}
	return nil
}

func claudeLaneTurnStatus(state claudeLaneState) any {
	if turn := claudeLaneReportedTurn(state); turn != nil {
		if turn.Status == "submitted" {
			return "active"
		}
		return emptyStringAsNil(turn.Status)
	}
	return nil
}

func listClaudeLanes(o claudeLaneOptions) (int, error) {
	if o.mine && !validLaneOwner(o.ownerPID, o.ownerProcStart) {
		return 1, errors.New("cannot establish the current orchestrator identity for --mine")
	}
	rows := []map[string]any{}
	for _, state := range readClaudeLaneStates(resolveNativePaths()) {
		if !o.all && state.Status == "archived" {
			continue
		}
		if o.mine && (state.Persistent || !sameLaneOwner(state.OwnerPID, state.OwnerProcStart, o.ownerPID, o.ownerProcStart)) {
			continue
		}
		row := claudeLaneStatusEvent(state)
		delete(row, "type")
		rows = append(rows, row)
	}
	return 0, emitLane(map[string]any{"type": "lane.list", "product": "claude", "contract_version": claudeLaneContractVersion, "lanes": rows})
}

func doctorClaudeLane() (int, error) {
	paths := resolveNativePaths()
	claudeBin, err := exec.LookPath(firstEnv("CLAUDE_PEER_CLAUDE_BIN", "claude"))
	available := err == nil
	_, supervisorErr := requestControl(paths.supervisorSock, map[string]any{"action": "status"}, 2*time.Second)
	supervisorReachable := supervisorErr == nil
	version := ""
	authenticated := false
	authMethod := ""
	authProvider := ""
	authError := ""
	if available {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		body, versionErr := exec.CommandContext(ctx, claudeBin, "--version").Output() //nolint:gosec // configured local Claude executable.
		cancel()
		if versionErr == nil {
			version = strings.TrimSpace(string(body))
		}
		status, statusErr := inspectClaudeLaneAuthentication(claudeBin, paths)
		authenticated = status.LoggedIn && statusErr == nil
		authMethod = status.AuthMethod
		authProvider = status.APIProvider
		if statusErr != nil {
			authError = statusErr.Error()
		} else if !status.LoggedIn {
			authError = "Claude Code is not authenticated"
		}
	}
	executable, _ := os.Executable()
	if err := emitLane(map[string]any{
		"type": "lane.doctor", "product": "claude", "contract_version": claudeLaneContractVersion,
		"runtime_path": executable, "claude_available": available, "claude_path": emptyStringAsNil(claudeBin),
		"claude_version": emptyStringAsNil(version), "claude_logged_in": authenticated,
		"claude_auth_method": emptyStringAsNil(authMethod), "claude_api_provider": emptyStringAsNil(authProvider),
		"claude_auth_error": emptyStringAsNil(authError), "state_root": profileDataRoot(paths),
		"supervisor_reachable": supervisorReachable, "supervisor_socket": paths.supervisorSock,
	}); err != nil {
		return 1, err
	}
	if !available || !authenticated || !supervisorReachable {
		return 1, nil
	}
	return 0, nil
}

type claudeAuthenticationStatus struct {
	LoggedIn    bool   `json:"loggedIn"`
	AuthMethod  string `json:"authMethod"`
	APIProvider string `json:"apiProvider"`
}

func inspectClaudeAuthentication(claudeBin string) (claudeAuthenticationStatus, error) {
	return inspectClaudeAuthenticationWithEnvironment(claudeBin, nil)
}

func inspectClaudeLaneAuthentication(claudeBin string, paths nativePaths) (claudeAuthenticationStatus, error) {
	source, err := sharedClaudeLaneSource(paths)
	if err != nil {
		return claudeAuthenticationStatus{}, err
	}
	environment := claudeAuthenticationEnvironment(os.Environ(), source)
	return inspectClaudeAuthenticationWithEnvironment(claudeBin, environment)
}

func inspectClaudeAuthenticationWithEnvironment(claudeBin string, environment []string) (claudeAuthenticationStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, claudeBin, "auth", "status", "--json")
	if environment != nil {
		command.Env = environment
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	body, commandErr := command.Output()
	if ctx.Err() != nil {
		return claudeAuthenticationStatus{}, errors.New("authentication check for Claude Code timed out")
	}
	var status claudeAuthenticationStatus
	if err := json.Unmarshal(body, &status); err != nil {
		if commandErr != nil {
			detail := strings.TrimSpace(stderr.String())
			if detail != "" {
				return status, fmt.Errorf("authentication check for Claude Code failed: %w: %s", commandErr, detail)
			}
			return status, fmt.Errorf("authentication check for Claude Code failed: %w", commandErr)
		}
		return status, fmt.Errorf("decode Claude Code authentication status: %w", err)
	}
	if commandErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(commandErr, &exitErr) || exitErr.ExitCode() != 1 || status.LoggedIn {
			detail := strings.TrimSpace(stderr.String())
			if detail != "" {
				return status, fmt.Errorf("authentication check for Claude Code failed: %w: %s", commandErr, detail)
			}
			return status, fmt.Errorf("authentication check for Claude Code failed: %w", commandErr)
		}
	}
	return status, nil
}

func claudeAuthenticationEnvironment(environment []string, source claudeprofile.Source) []string {
	result := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if !claudePrivateEnvironmentBlocked(name) {
			result = append(result, entry)
		}
	}
	result = append(result, "CLAUDE_PEER_CLAUDE_CONFIG_DIR="+source.ConfigRoot)
	if source.ConfigEnvSet {
		result = append(result, "CLAUDE_CONFIG_DIR="+source.ConfigEnvValue)
	}
	if source.SecureEnvSet {
		result = append(result, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+source.SecureConfig)
	}
	return result
}

func sharedClaudeLaneSource(paths nativePaths) (claudeprofile.Source, error) {
	if !laneAgentConfigured() {
		return claudeprofile.SharedSource(paths.claudeRoot)
	}
	status, err := federator.ReadAgentStatus(laneAgentRuntimeDir())
	if err != nil {
		return claudeprofile.Source{}, fmt.Errorf("read host agent shared Claude registry: %w", err)
	}
	source := claudeLaneAgentProfile(status)
	if !samePath(filepath.Join(source.ConfigRoot, "sessions"), status.RegistryDir) {
		return claudeprofile.Source{}, fmt.Errorf(
			"claude lane profile %s does not match host agent registry %s", source.ConfigRoot, status.RegistryDir,
		)
	}
	return source, nil
}

func claudeLaneAgentProfile(status federator.AgentStatus) claudeprofile.Source {
	root := filepath.Dir(status.RegistryDir)
	statePath := filepath.Join(root, ".claude.json")
	if !status.ClaudeConfigEnvSet || status.ClaudeConfigEnvValue == "" {
		home, _ := os.UserHomeDir()
		statePath = filepath.Join(home, ".claude.json")
	}
	return claudeprofile.Source{
		ConfigRoot: root, StatePath: statePath, SecureConfig: status.ClaudeSecureConfig,
		ConfigEnvSet: status.ClaudeConfigEnvSet, ConfigEnvValue: status.ClaudeConfigEnvValue,
		SecureEnvSet: status.ClaudeSecureEnvSet,
	}
}

func claudePrivateEnvironmentBlocked(name string) bool {
	switch name {
	case "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_ENTRYPOINT", "CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "CLAUDE_CODE_HARBOR_KITE", "CLAUDE_CODE_SIMPLE",
		"CLAUDE_CONFIG_DIR", "CLAUDE_PEER_CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR":
		return true
	default:
		return false
	}
}

func interruptClaudeLane(o claudeLaneOptions) (int, error) {
	state, err := resolveClaudeLaneState(resolveNativePaths(), o.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" {
		return 1, fmt.Errorf("claude lane %s is archived", state.SessionID)
	}
	response, err := requestControl(state.ControlSocket, map[string]any{"action": "interrupt", "sessionId": state.SessionID}, 5*time.Second)
	if err != nil {
		return 1, err
	}
	return 0, emitLane(map[string]any{"type": "turn.interrupted", "thread_id": state.SessionID, "turn_id": response["turnId"]})
}

func archiveClaudeLane(o claudeLaneOptions) (int, error) {
	paths := resolveNativePaths()
	state, err := resolveClaudeLaneState(paths, o.target)
	if err != nil {
		return 1, err
	}
	if state.Status == "archived" {
		if state.CleanupError != "" {
			if err := forceArchiveClaudeLane(paths, state.SessionID, "retry failed cleanup"); err != nil {
				return 1, fmt.Errorf("retry Claude lane cleanup after %s: %w", state.CleanupError, err)
			}
		}
		return 0, emitLane(map[string]any{"type": "lane.archived", "product": "claude", "name": state.Name, "thread_id": state.SessionID, "already_archived": true})
	}
	if probeUnixSocket(state.ControlSocket, 250*time.Millisecond) {
		if _, err := requestControl(state.ControlSocket, map[string]any{"action": "archive", "sessionId": state.SessionID}, 10*time.Second); err != nil {
			return 1, err
		}
		if err := waitClaudeLaneArchived(paths, state.SessionID, 10*time.Second); err != nil {
			return 1, err
		}
	} else if err := forceArchiveClaudeLane(paths, state.SessionID, "manager unavailable"); err != nil {
		return 1, err
	}
	return 0, emitLane(map[string]any{"type": "lane.archived", "product": "claude", "name": state.Name, "thread_id": state.SessionID})
}

func waitClaudeLaneArchived(paths nativePaths, sessionID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := readClaudeLaneState(paths, sessionID)
		if err == nil && state.CleanupError != "" {
			return fmt.Errorf("claude lane cleanup failed: %s", state.CleanupError)
		}
		if err == nil && state.Status == "archived" &&
			state.ManagerPID == 0 && state.WorkerPID == 0 && state.WorkerSocket == "" {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("timed out waiting for claude lane cleanup")
}

type claudeLaneManager struct {
	mu                 sync.Mutex
	paths              nativePaths
	state              claudeLaneState
	listener           net.Listener
	lifecycleLock      *os.File
	worker             *exec.Cmd
	workerInput        io.WriteCloser
	workerDone         chan error
	done               chan struct{}
	cleanupOnce        sync.Once
	noticeMu           sync.Mutex
	closing            bool
	timeoutRequested   string
	interruptRequested string
	writeQueue         chan claudeLaneWrite
	captureProcStart   func(int) (string, error)
	lastAgentRefresh   time.Time
}

type claudeLaneWrite struct {
	turnID string
	body   []byte
}

func runClaudeLaneManager(argv []string) int {
	args := parseArgs(argv)
	sessionID := args["session-id"]
	if !validSessionID(sessionID) {
		fmt.Fprintln(os.Stderr, "claude-lane-manager requires --session-id")
		return 2
	}
	paths := resolveNativePaths()
	state, err := readClaudeLaneState(paths, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-lane-manager: %v\n", err)
		return 1
	}
	m := &claudeLaneManager{
		paths: paths, state: state, workerDone: make(chan error, 1), done: make(chan struct{}),
		writeQueue: make(chan claudeLaneWrite, 1),
	}
	if err := m.start(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-lane-manager: %v\n", err)
		_ = forceArchiveClaudeLane(paths, sessionID, "manager startup failed")
		return 1
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case <-signals:
			if m.shutdown("manager signalled", true) {
				return 0
			}
		case <-m.done:
			return 0
		}
	}
}

func (m *claudeLaneManager) start() error {
	lifecycle, err := lockLaneLifecycle(m.paths, "claude-"+m.state.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lifecycle)
	latest, err := readClaudeLaneState(m.paths, m.state.SessionID)
	if err != nil {
		return err
	}
	if latest.Status != "starting" {
		return fmt.Errorf("refuse to start Claude lane manager from state %q", latest.Status)
	}
	m.state = latest
	if err := validateClaudeLaneSocketPath(m.state.ControlSocket); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.state.ControlSocket), 0700); err != nil {
		return err
	}
	_ = os.Remove(m.state.ControlSocket)
	listener, err := net.Listen("unix", m.state.ControlSocket)
	if err != nil {
		return err
	}
	_ = os.Chmod(m.state.ControlSocket, 0600)
	m.listener = listener
	m.state.ManagerPID = os.Getpid()
	m.state.ManagerProcStart, err = m.captureProcessStart(m.state.ManagerPID)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(m.state.ControlSocket)
		return fmt.Errorf("capture Claude lane manager identity: %w", err)
	}
	if err := m.persistLocked(); err != nil {
		_ = listener.Close()
		return err
	}
	if err := m.startWorker(); err != nil {
		return err
	}
	go m.workerWriteLoop()
	m.mu.Lock()
	err = m.startNextTurnLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	// Some Claude hosts do not publish the native messaging socket until the
	// first stream-json envelope is consumed. Submit before requiring the socket,
	// but keep lane.ready blocked until the row and socket are both verified.
	err = m.captureWorkerPeer()
	m.mu.Lock()
	if err == nil {
		if m.state.Status == "starting" {
			m.state.Status = "idle"
		}
		m.state.StartupID = ""
		err = m.persistLocked()
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := m.registerAgentPeer(); err != nil {
		return fmt.Errorf("register Claude lane with host agent: %w", err)
	}
	go m.acceptLoop()
	go m.maintenanceLoop()
	return nil
}

func (m *claudeLaneManager) startWorker() error {
	claudeBin, err := exec.LookPath(firstEnv("CLAUDE_PEER_CLAUDE_BIN", "claude"))
	if err != nil {
		return fmt.Errorf("find Claude Code: %w", err)
	}
	args := claudeLaneWorkerArgs(m.state)
	if !m.state.WorkerSharedProfile {
		return errors.New("legacy private Claude lane cannot resume in shared-profile mode; archive it and start a new lane")
	}
	source := claudeprofile.Source{
		ConfigRoot: m.state.WorkerConfigDir, ConfigEnvSet: m.state.WorkerConfigEnvSet,
		ConfigEnvValue: m.state.WorkerConfigEnvValue, SecureConfig: m.state.WorkerSecureConfigDir,
		SecureEnvSet: m.state.WorkerSecureEnvSet,
	}
	command := exec.Command(claudeBin, args...) //nolint:gosec // configured Claude executable and validated options.
	command.Dir = m.state.Cwd
	command.Env = claudeLaneWorkerEnv(os.Environ(), m.state.SessionID, source)
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	// Result failures are authoritative on stdout. Discard unbounded SDK
	// diagnostics rather than allowing a worker to grow the manager log forever.
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return err
	}
	m.worker, m.workerInput = command, stdin
	m.state.WorkerPID = command.Process.Pid
	m.state.WorkerProcStart, err = m.captureProcessStart(m.state.WorkerPID)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		m.worker, m.workerInput = nil, nil
		m.state.WorkerPID, m.state.WorkerProcStart = 0, ""
		return fmt.Errorf("capture Claude lane worker identity: %w", err)
	}
	// Persist process ownership before waiting for SDK peer publication so a
	// startup failure can still reap the process, registry row, and socket.
	if err := m.persistLocked(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	go func() {
		scanErr := m.scanWorker(stdout)
		if scanErr != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		waitErr := command.Wait()
		if scanErr != nil {
			m.workerDone <- fmt.Errorf("read Claude worker stream: %w", scanErr)
			return
		}
		m.workerDone <- waitErr
	}()
	return nil
}

func claudeLaneWorkerArgs(state claudeLaneState) []string {
	args := []string{
		"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages", "--verbose",
		"--no-chrome",
		"--settings", `{"crossSessionInbound":"accept"}`,
	}
	if state.WorkerSessionStarted {
		args = append(args, "--resume", state.SessionID)
	} else {
		args = append(args, "--session-id", state.SessionID)
	}
	// The native SDK worker is the lane's sole discoverable peer. Keep its session
	// name equal to the stable lane name so native inbound and outbound messages
	// use the same identity that lane lifecycle commands expose.
	args = append(args, "--name", state.Name, "--permission-mode", state.PermissionMode)
	if state.Model != "" {
		args = append(args, "--model", state.Model)
	}
	if state.Effort != "" {
		args = append(args, "--effort", state.Effort)
	}
	if state.MaxBudgetUSD != "" {
		args = append(args, "--max-budget-usd", state.MaxBudgetUSD)
	}
	if state.ToolsSet || state.Tools != "" {
		args = append(args, "--tools", state.Tools)
	}
	allowedTools := "SendMessage,ListAgents"
	if state.AllowedTools != "" {
		allowedTools = state.AllowedTools + "," + allowedTools
	}
	args = append(args, "--allowedTools", allowedTools)
	if state.DisallowedToolsSet || state.DisallowedTools != "" {
		args = append(args, "--disallowedTools", state.DisallowedTools)
	}
	if len(state.OutputSchema) > 0 {
		args = append(args, "--json-schema", string(state.OutputSchema))
	}
	return args
}

func claudeLaneWorkerEnv(environment []string, sessionID string, source claudeprofile.Source) []string {
	blocked := map[string]bool{
		// A nested lane must derive its Codex owner from live ancestry and registry
		// identity, never from the outer orchestrator's inherited thread ID.
		"CODEX_THREAD_ID":        true,
		peerSessionIDEnvironment: true, "AGENT_SESSIONS_PRODUCT": true, agentRuntimeDirEnvironment: true,
		remoteParentEnvironment: true,
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if !blocked[name] && !claudePrivateEnvironmentBlocked(name) {
			result = append(result, entry)
		}
	}
	// SendMessage, ListAgents, and native inbound peer turns are exposed only
	// when Agent Teams is enabled. The SDK worker remains an ordinary native
	// Claude row in the shared profile as well as a grouped Agent Sessions peer.
	result = append(result, "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1", "CLAUDE_CODE_HARBOR_KITE=1")
	result = append(result, peerSessionIDEnvironment+"="+sessionID, "AGENT_SESSIONS_PRODUCT=claude")
	result = append(result, agentRuntimeDirEnvironment+"="+laneAgentRuntimeDir())
	result = append(result, "CLAUDE_PEER_CLAUDE_CONFIG_DIR="+source.ConfigRoot)
	if source.ConfigEnvSet {
		result = append(result, "CLAUDE_CONFIG_DIR="+source.ConfigEnvValue)
	}
	if source.SecureEnvSet {
		result = append(result, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+source.SecureConfig)
	}
	return result
}

// captureWorkerPeer verifies Claude's native SDK registry row and records its
// live socket as the lane address. The manager never rewrites or proxies it.
func (m *claudeLaneManager) captureWorkerPeer() error {
	m.mu.Lock()
	workerPID := m.state.WorkerPID
	workerProcStart := m.state.WorkerProcStart
	sessionID := m.state.SessionID
	m.mu.Unlock()
	registryRoot := m.state.WorkerConfigDir
	if registryRoot == "" {
		registryRoot = m.paths.claudeRoot
	}
	registry := filepath.Join(registryRoot, "sessions", strconv.Itoa(workerPID)+".json")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if workerErr, exited := claudeWorkerStartupExit(m.workerDone); exited {
			return workerErr
		}
		row := readJSONMap(registry)
		socket, ready, err := verifiedClaudeWorkerPeerSocket(row, m.paths.runtimeDir, workerPID, workerProcStart, sessionID)
		if err != nil {
			return err
		}
		if !ready {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		m.mu.Lock()
		if m.state.WorkerPID != workerPID || m.state.WorkerProcStart != workerProcStart || m.state.SessionID != sessionID {
			m.mu.Unlock()
			return errors.New("claude worker identity changed during peer capture")
		}
		m.state.WorkerSocket = socket
		m.mu.Unlock()
		return nil
	}
	return errors.New("timed out locating Claude worker peer row")
}

func (m *claudeLaneManager) agentPeerRegistration() federator.PeerRegistration {
	m.mu.Lock()
	defer m.mu.Unlock()
	permissionMode := "default"
	if m.state.PermissionMode == "bypassPermissions" {
		permissionMode = "bypassPermissions"
	}
	return federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: m.state.SessionID,
		Product: "claude", Name: m.state.Name, Status: defaultString(m.state.Status, "idle"),
		PermissionMode: permissionMode, Cwd: m.state.Cwd,
		PID: m.state.WorkerPID, ProcStart: m.state.WorkerProcStart, Socket: m.state.WorkerSocket,
		LifecyclePID: m.state.WorkerPID, LifecycleProcStart: m.state.WorkerProcStart,
		ClaudeConfigRoot: m.state.WorkerConfigDir,
		StartedAt:        m.state.CreatedAt,
	}
}

func (m *claudeLaneManager) registerAgentPeer() error {
	if !laneAgentConfigured() {
		return nil
	}
	_, err := federator.RegisterPeer(laneAgentRuntimeDir(), m.agentPeerRegistration())
	if err == nil {
		m.lastAgentRefresh = time.Now()
	}
	return err
}

func (m *claudeLaneManager) unregisterAgentPeer() {
	if !laneAgentConfigured() {
		return
	}
	_ = federator.UnregisterPeer(laneAgentRuntimeDir(), m.agentPeerRegistration())
}

func claudeWorkerStartupExit(workerDone <-chan error) (error, bool) {
	select {
	case workerErr := <-workerDone:
		if workerErr == nil {
			return errors.New("claude worker exited during startup"), true
		}
		return fmt.Errorf("claude worker exited during startup: %w", workerErr), true
	default:
		return nil, false
	}
}

func verifiedClaudeWorkerPeerSocket(
	row map[string]any,
	runtimeDir string,
	workerPID int,
	workerProcStart string,
	sessionID string,
) (string, bool, error) {
	if intValue(row["pid"]) != workerPID || stringValue(row["sessionId"]) != sessionID {
		return "", false, nil
	}
	socket := stringValue(row["messagingSocketPath"])
	if socket == "" {
		return "", false, nil
	}
	if stringValue(row["entrypoint"]) != "sdk-cli" || !exactProcessIdentityMatch(workerPID, workerProcStart) ||
		stringValue(row["procStart"]) != workerProcStart {
		return "", false, errors.New("refuse an unverified Claude worker peer row")
	}
	if !ownedClaudeWorkerSocketPath(socket, runtimeDir, workerPID) {
		return "", false, errors.New("refuse Claude worker socket outside an owned runtime directory")
	}
	info, statErr := os.Lstat(socket)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 {
		return "", false, errors.New("claude worker registry does not point to its live Unix socket")
	}
	return socket, true, nil
}

func (m *claudeLaneManager) scanWorker(reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 16*maxFrameBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var frame map[string]any
		if json.Unmarshal(line, &frame) != nil {
			continue
		}
		m.handleWorkerFrame(frame, line)
	}
	return scanner.Err()
}

//nolint:gocyclo // Stream-frame dispatch keeps terminal classification in one auditable path.
func (m *claudeLaneManager) handleWorkerFrame(frame map[string]any, raw []byte) {
	if stringValue(frame["type"]) == "command_lifecycle" && stringValue(frame["state"]) == "started" {
		m.recordNativeWorkerTurnStarted(frame)
		return
	}
	if stringValue(frame["type"]) == "system" && stringValue(frame["subtype"]) == "init" {
		m.mu.Lock()
		if !m.state.WorkerSessionStarted {
			m.state.WorkerSessionStarted = true
			_ = m.persistLocked()
		}
		m.mu.Unlock()
		return
	}
	if stringValue(frame["type"]) == "user" {
		m.recordWorkerUserFrame(frame)
		return
	}
	if stringValue(frame["type"]) != "result" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.state.Status == "archived" {
		return
	}
	index := m.activeTurnIndexLocked()
	if index < 0 {
		// Manager execution is FIFO and permits at most one submitted launcher
		// turn. Claude result frames carry no turn identifier, so that scheduler
		// invariant is the only stable correlation available when the local user
		// replay was normalized or omitted.
		index = m.submittedTurnIndexLocked()
		if index < 0 {
			return
		}
		m.activateSubmittedTurnLocked(index)
	}
	now := time.Now().UnixMilli()
	turn := &m.state.Turns[index]
	turn.Result = stringValue(frame["result"])
	turn.ResultFrame = append(json.RawMessage(nil), raw...)
	turn.CompletedAt = now
	turn.DeadlineAt = 0
	if m.timeoutRequested == turn.ID {
		turn.Status, turn.Outcome, turn.Exit = "timed_out", "timed_out", 124
		m.timeoutRequested = ""
	} else if m.interruptRequested == turn.ID {
		turn.Status, turn.Outcome, turn.Exit = "interrupted", "interrupted", 130
		m.interruptRequested = ""
	} else if isError, _ := frame["is_error"].(bool); !isError && stringValue(frame["subtype"]) == "success" {
		turn.Status, turn.Outcome, turn.Exit = "completed", "completed", 0
	} else if stringValue(frame["subtype"]) == "interrupted" || stringValue(frame["terminal_reason"]) == "interrupted" {
		turn.Status, turn.Outcome, turn.Exit = "interrupted", "interrupted", 130
	} else {
		turn.Status, turn.Outcome, turn.Exit = "failed", "failed", 1
		turn.Error = defaultString(stringValue(frame["error"]), stringValue(frame["subtype"]))
	}
	m.state.Status = "idle"
	m.state.TerminalOutcome = turn.Outcome
	if m.hasOutstandingExecutionLocked() {
		m.state.Status, m.state.AutoArchiveAt = "active", 0
		m.refreshSubmittedWatchdogLocked(now)
	} else if m.state.AutoArchive {
		m.state.AutoArchiveAt = now + m.state.AutoArchiveDelayMS
	}
	m.queueTerminalNoticeLocked(*turn)
	_ = m.persistLocked()
	m.flushTerminalNoticesAsyncLocked()
	_ = m.startNextTurnLocked()
}

// recordNativeWorkerTurnStarted adopts a turn that Claude started from its
// native peer socket. Manager-submitted turns stay submitted until their local
// user replay frame proves that Claude actually began them.
func (m *claudeLaneManager) recordNativeWorkerTurnStarted(frame map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.state.Status == "archived" {
		return
	}
	turnID := stringValue(frame["command_uuid"])
	if !validSessionID(turnID) {
		turnID = randomID()
	}
	for index := range m.state.Turns {
		if m.state.Turns[index].ID == turnID {
			// Claude emits command_lifecycle for both standalone peer commands
			// and peer steering of an existing turn, so this frame confirms the
			// UUID but cannot by itself distinguish those two scheduler cases.
			return
		}
	}
	// A peer command that starts while another stream turn is active is a
	// native steer of that turn, not a second collectable result.
	if m.activeTurnIndexLocked() >= 0 {
		return
	}
	now := time.Now().UnixMilli()
	hadDebt := firstClaudeLaneDebt(m.state) != ""
	m.state.Turns = append(m.state.Turns, claudeLaneTurn{
		ID: turnID, Status: "active", CreatedAt: now, StartedAt: now,
	})
	m.state.Status, m.state.AutoArchiveAt, m.state.LatestTurnID = "active", 0, turnID
	if !hadDebt {
		m.state.TurnID = turnID
	}
	_ = m.persistLocked()
}

//nolint:gocyclo // Keep native-peer steering and launcher replay arbitration under the same state lock.
func (m *claudeLaneManager) recordWorkerUserFrame(frame map[string]any) {
	origin, _ := frame["origin"].(map[string]any)
	if stringValue(origin["kind"]) != "peer" {
		prompt := claudeWorkerUserText(frame)
		if prompt == "" {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closing || m.state.Status == "archived" {
			return
		}
		submitted := m.submittedTurnIndexLocked()
		if submitted < 0 {
			return
		}
		if active := m.activeTurnIndexLocked(); active >= 0 {
			// Claude replayed the launcher envelope while the manager still
			// held a provisional native-peer turn. That ordering proves the
			// peer input steered the launcher turn and shares its one result;
			// retain the launcher identity and discard the provisional peer
			// record before the result arrives.
			peerTurnID := m.state.Turns[active].ID
			launcherTurnID := m.state.Turns[submitted].ID
			m.state.Turns = append(m.state.Turns[:active], m.state.Turns[active+1:]...)
			if active < submitted {
				submitted--
			}
			if m.interruptRequested == peerTurnID {
				m.interruptRequested = launcherTurnID
			}
			if m.timeoutRequested == peerTurnID {
				m.timeoutRequested = launcherTurnID
			}
			m.activateSubmittedTurnLocked(submitted)
			_ = m.persistLocked()
			return
		}
		m.activateSubmittedTurnLocked(submitted)
		_ = m.persistLocked()
		return
	}
	prompt := claudeWorkerUserText(frame)
	turnID := stringValue(frame["uuid"])
	if !validSessionID(turnID) {
		turnID = randomID()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.state.Status == "archived" {
		return
	}
	index := m.activeTurnIndexLocked()
	if index >= 0 {
		turn := &m.state.Turns[index]
		if turn.ID == turnID {
			turn.Prompt = prompt
			turn.MessageID = stringValue(origin["msg_id"])
			_ = m.persistLocked()
		}
		// A different peer UUID is steering the active stream turn. It has no
		// independent result and must not overwrite that turn's identity.
		return
	}
	for current := range m.state.Turns {
		if m.state.Turns[current].ID == turnID {
			return
		}
	}
	{
		now := time.Now().UnixMilli()
		hadDebt := firstClaudeLaneDebt(m.state) != ""
		m.state.Turns = append(m.state.Turns, claudeLaneTurn{
			ID: turnID, Prompt: prompt, MessageID: stringValue(origin["msg_id"]),
			Status: "active", CreatedAt: now, StartedAt: now,
		})
		m.state.Status, m.state.AutoArchiveAt, m.state.LatestTurnID = "active", 0, turnID
		if !hadDebt {
			m.state.TurnID = turnID
		}
		_ = m.persistLocked()
		return
	}
}

func claudeWorkerUserText(frame map[string]any) string {
	message, _ := frame["message"].(map[string]any)
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		parts := []string{}
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if stringValue(block["type"]) == "text" && stringValue(block["text"]) != "" {
				parts = append(parts, stringValue(block["text"]))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func (m *claudeLaneManager) activeTurnIndexLocked() int {
	for index := range m.state.Turns {
		if m.state.Turns[index].Status == "active" {
			return index
		}
	}
	return -1
}

func (m *claudeLaneManager) submittedTurnIndexLocked() int {
	for index := range m.state.Turns {
		if m.state.Turns[index].Status == "submitted" {
			return index
		}
	}
	return -1
}

func (m *claudeLaneManager) activateSubmittedTurnLocked(index int) {
	turn := &m.state.Turns[index]
	now := time.Now().UnixMilli()
	turn.Status, turn.StartedAt, turn.DeadlineAt = "active", now, 0
	if turn.TimeoutMS > 0 {
		turn.DeadlineAt = now + turn.TimeoutMS
	}
	m.state.Status, m.state.AutoArchiveAt, m.state.LatestTurnID = "active", 0, turn.ID
}

func (m *claudeLaneManager) refreshSubmittedWatchdogLocked(now int64) {
	if index := m.submittedTurnIndexLocked(); index >= 0 {
		m.state.Turns[index].DeadlineAt = now + claudeLaneSubmissionAckTimeout.Milliseconds()
	}
}

func (m *claudeLaneManager) hasOutstandingExecutionLocked() bool {
	for _, turn := range m.state.Turns {
		if turn.Status == "active" || turn.Status == "submitted" || turn.Status == "queued" {
			return true
		}
	}
	return false
}

func (m *claudeLaneManager) startNextTurnLocked() error {
	if m.activeTurnIndexLocked() >= 0 || m.submittedTurnIndexLocked() >= 0 {
		return nil
	}
	for index := range m.state.Turns {
		turn := &m.state.Turns[index]
		if turn.Status != "queued" {
			continue
		}
		turn.Status, turn.StartedAt = "submitted", 0
		turn.DeadlineAt = time.Now().Add(claudeLaneSubmissionAckTimeout).UnixMilli()
		m.state.Status, m.state.AutoArchiveAt = "active", 0
		m.state.LatestTurnID = turn.ID
		if m.state.TurnID == "" || allClaudeTurnsCollected(m.state) {
			m.state.TurnID = turn.ID
		}
		if err := m.persistLocked(); err != nil {
			return err
		}
		envelope := map[string]any{
			"type": "user", "message": map[string]any{
				"role": "user", "content": []map[string]any{{"type": "text", "text": turn.Prompt}},
			},
		}
		body, _ := json.Marshal(envelope)
		select {
		case m.writeQueue <- claudeLaneWrite{turnID: turn.ID, body: append(body, '\n')}:
			return nil
		default:
			queueErr := errors.New("claude lane worker write queue is unexpectedly occupied")
			turn.Status, turn.Outcome, turn.Exit, turn.Error = "failed", "failed", 1, queueErr.Error()
			turn.CompletedAt, turn.DeadlineAt = time.Now().UnixMilli(), 0
			m.state.Status, m.state.TerminalOutcome = "idle", "failed"
			if m.state.AutoArchive {
				m.state.AutoArchiveAt = turn.CompletedAt + m.state.AutoArchiveDelayMS
			}
			m.queueTerminalNoticeLocked(*turn)
			_ = m.persistLocked()
			m.flushTerminalNoticesAsyncLocked()
			return queueErr
		}
	}
	return nil
}

func (m *claudeLaneManager) workerWriteLoop() {
	for {
		select {
		case write := <-m.writeQueue:
			if _, err := m.workerInput.Write(write.body); err != nil {
				m.recordWorkerWriteFailure(write.turnID, err)
			}
		case <-m.done:
			return
		}
	}
}

func (m *claudeLaneManager) recordWorkerWriteFailure(turnID string, writeErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index := range m.state.Turns {
		turn := &m.state.Turns[index]
		if turn.ID != turnID || (turn.Status != "active" && turn.Status != "submitted") {
			continue
		}
		turn.Status, turn.Outcome, turn.Exit, turn.Error = "failed", "failed", 1, writeErr.Error()
		turn.CompletedAt, turn.DeadlineAt = time.Now().UnixMilli(), 0
		m.state.Status, m.state.TerminalOutcome = "idle", "failed"
		if m.state.AutoArchive {
			m.state.AutoArchiveAt = turn.CompletedAt + m.state.AutoArchiveDelayMS
		}
		m.queueTerminalNoticeLocked(*turn)
		_ = m.persistLocked()
		m.flushTerminalNoticesAsyncLocked()
		_ = m.startNextTurnLocked()
		return
	}
}

func allClaudeTurnsCollected(state claudeLaneState) bool {
	for _, turn := range state.Turns {
		if !turn.Collected {
			return false
		}
	}
	return true
}

func (m *claudeLaneManager) acceptLoop() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
				continue
			}
		}
		go m.handleControlConn(conn)
	}
}

func (m *claudeLaneManager) handleControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	if !scanner.Scan() {
		return
	}
	var request map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		writeClaudeLaneControlResponse(conn, nil, err)
		return
	}
	response, err := m.handleControl(request)
	writeClaudeLaneControlResponse(conn, response, err)
	if err == nil && stringValue(request["action"]) == "archive" {
		go m.finishShutdown(true)
	}
}

func writeClaudeLaneControlResponse(conn net.Conn, response map[string]any, err error) {
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

//nolint:gocyclo // Control actions are explicit at the local socket boundary.
func (m *claudeLaneManager) handleControl(request map[string]any) (map[string]any, error) {
	if sessionID := stringValue(request["sessionId"]); sessionID != "" && sessionID != m.state.SessionID {
		return nil, errors.New("claude lane control session mismatch")
	}
	switch stringValue(request["action"]) {
	case "status":
		m.mu.Lock()
		defer m.mu.Unlock()
		return map[string]any{"status": m.state.Status, "sessionId": m.state.SessionID, "turnId": m.state.TurnID}, nil
	case "resume":
		body, _ := json.Marshal(request["turn"])
		var turn claudeLaneTurn
		if json.Unmarshal(body, &turn) != nil || turn.ID == "" || strings.TrimSpace(turn.Prompt) == "" {
			return nil, errors.New("invalid Claude lane resume turn")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closing || m.state.Status == "archived" {
			return nil, errors.New("claude lane is archived")
		}
		if err := m.refreshCollectionAcknowledgementsLocked(); err != nil {
			return nil, err
		}
		if debt := firstClaudeLaneDebt(m.state); debt != "" {
			return nil, fmt.Errorf("collect outstanding Claude lane turn %s before resume", debt)
		}
		if persistent, ok := request["persistent"].(bool); ok && !persistent {
			if err := validateLaneOwner(false, intValue(request["ownerPid"]), stringValue(request["ownerProcStart"])); err != nil {
				return nil, err
			}
		}
		if value := int64Value(request["autoArchiveDelayMs"]); value > 0 {
			m.state.AutoArchive, m.state.AutoArchiveDelayMS = true, value
		}
		if auto, ok := request["autoArchive"].(bool); ok && !auto {
			m.state.AutoArchive, m.state.AutoArchiveAt = false, 0
		}
		if persistent, ok := request["persistent"].(bool); ok {
			m.state.Persistent = persistent
			if persistent {
				m.state.OwnerPID, m.state.OwnerProcStart, m.state.OwnerSessionID = 0, "", ""
			}
		}
		if notifySet, _ := request["notifySet"].(bool); notifySet {
			m.state.NotifyTarget = stringValue(request["notifyTarget"])
		}
		if ownerPID := intValue(request["ownerPid"]); ownerPID > 1 && !m.state.Persistent {
			m.state.OwnerPID, m.state.OwnerProcStart = ownerPID, stringValue(request["ownerProcStart"])
			m.state.OwnerSessionID = stringValue(request["ownerSessionId"])
		}
		groupsBody, _ := json.Marshal(request["groups"])
		explicitBody, _ := json.Marshal(request["explicitGroups"])
		var groups, explicit []string
		if json.Unmarshal(groupsBody, &groups) != nil || json.Unmarshal(explicitBody, &explicit) != nil {
			return nil, errors.New("invalid Claude lane group state")
		}
		m.state.Groups, m.state.ExplicitGroups = groups, explicit
		m.state.ParentSessionID = stringValue(request["parentSessionId"])
		m.state.ParentHostID = stringValue(request["parentHostId"])
		m.state.ParentAgentRuntimeDir = stringValue(request["parentAgentRuntimeDir"])
		m.state.InheritParentGroups, _ = request["inheritParentGroups"].(bool)
		m.state.Turns = append(m.state.Turns, turn)
		m.state.AutoArchiveAt = 0
		if err := m.persistLocked(); err != nil {
			return nil, err
		}
		if err := m.startNextTurnLocked(); err != nil {
			return nil, err
		}
		delivery := "queued"
		if status := m.claudeTurnStatusLocked(turn.ID); status == "active" || status == "submitted" {
			delivery = "started"
		}
		return map[string]any{"delivery": delivery, "turnId": turn.ID}, nil
	case "interrupt":
		m.mu.Lock()
		defer m.mu.Unlock()
		index := m.activeTurnIndexLocked()
		if index < 0 {
			index = m.submittedTurnIndexLocked()
		}
		if index < 0 || m.worker == nil || m.worker.Process == nil {
			return nil, errors.New("claude lane has no active turn")
		}
		turnID := m.state.Turns[index].ID
		m.interruptRequested = turnID
		if err := m.worker.Process.Signal(os.Interrupt); err != nil {
			m.interruptRequested = ""
			return nil, err
		}
		return map[string]any{"turnId": turnID}, nil
	case "archive":
		started, err := m.markArchived("explicit archive", false)
		if err != nil {
			return nil, err
		}
		return map[string]any{"archiving": started}, nil
	default:
		return nil, fmt.Errorf("unknown Claude lane control action %q", stringValue(request["action"]))
	}
}

func (m *claudeLaneManager) claudeTurnStatusLocked(turnID string) string {
	for _, turn := range m.state.Turns {
		if turn.ID == turnID {
			return turn.Status
		}
	}
	return ""
}

func (m *claudeLaneManager) maintenanceLoop() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if m.maintain() {
				return
			}
		case err := <-m.workerDone:
			if m.handleWorkerExit(err) {
				return
			}
		case <-m.done:
			return
		}
	}
}

//nolint:gocyclo // One maintenance transaction arbitrates owner, execution, submission, and archive deadlines.
func (m *claudeLaneManager) maintain() bool {
	m.flushTerminalNotices()
	if laneAgentConfigured() && time.Since(m.lastAgentRefresh) >= 5*time.Second {
		_ = m.registerAgentPeer()
	}
	m.mu.Lock()
	if m.state.Status == "archived" {
		m.mu.Unlock()
		return m.shutdown("finish archived lane cleanup", false)
	}
	if !m.state.Persistent && m.state.OwnerPID > 1 &&
		cleanupProcessIdentityStatus(m.state.OwnerPID, m.state.OwnerProcStart).Status == processIdentityStale {
		m.mu.Unlock()
		return m.shutdown("lifecycle owner exited", true)
	}
	now := time.Now().UnixMilli()
	index := m.activeTurnIndexLocked()
	if index >= 0 {
		turn := &m.state.Turns[index]
		if turn.DeadlineAt > 0 && turn.DeadlineAt <= now && m.timeoutRequested != turn.ID {
			m.timeoutRequested = turn.ID
			if m.worker != nil && m.worker.Process != nil {
				_ = m.worker.Process.Signal(os.Interrupt)
			}
		}
	}
	if index < 0 {
		if submitted := m.submittedTurnIndexLocked(); submitted >= 0 {
			turn := &m.state.Turns[submitted]
			if turn.DeadlineAt > 0 && turn.DeadlineAt <= now {
				turn.Status, turn.Outcome, turn.Exit = "failed", "failed", 1
				turn.CompletedAt, turn.DeadlineAt = now, 0
				turn.Error = "Claude worker did not acknowledge the submitted turn"
				m.state.Status, m.state.TerminalOutcome, m.state.AutoArchiveAt = "idle", "failed", 0
				m.queueTerminalNoticeLocked(*turn)
				_ = m.persistLocked()
				m.mu.Unlock()
				m.flushTerminalNotices()
				return m.shutdown("submitted turn acknowledgement timed out", true)
			}
		}
	}
	due := index < 0 && m.state.AutoArchive && m.state.AutoArchiveAt > 0 && m.state.AutoArchiveAt <= now
	m.mu.Unlock()
	if due {
		return m.shutdown("auto-archive delay elapsed", false)
	}
	return false
}

func (m *claudeLaneManager) handleWorkerExit(workerErr error) bool {
	m.mu.Lock()
	if m.state.Status == "archived" {
		m.mu.Unlock()
		return m.shutdown("worker exited after archive", false)
	}
	index := m.activeTurnIndexLocked()
	now := time.Now().UnixMilli()
	if index >= 0 {
		turn := &m.state.Turns[index]
		turn.CompletedAt, turn.DeadlineAt = now, 0
		switch {
		case m.timeoutRequested == turn.ID:
			turn.Status, turn.Outcome, turn.Exit = "timed_out", "timed_out", 124
			turn.Error = "Claude turn exceeded its durable deadline"
			m.timeoutRequested = ""
		case m.interruptRequested == turn.ID:
			turn.Status, turn.Outcome, turn.Exit = "interrupted", "interrupted", 130
			turn.Error = "Claude turn was interrupted"
			m.interruptRequested = ""
		default:
			turn.Status, turn.Outcome, turn.Exit = "failed", "failed", 1
			if workerErr != nil {
				turn.Error = workerErr.Error()
			} else {
				turn.Error = "Claude worker exited before a result frame"
			}
		}
		m.state.TerminalOutcome = turn.Outcome
		m.queueTerminalNoticeLocked(*turn)
	}
	for current := range m.state.Turns {
		turn := &m.state.Turns[current]
		if turn.Status != "queued" && turn.Status != "submitted" {
			continue
		}
		turn.Status, turn.Outcome, turn.Exit = "interrupted", "interrupted", 130
		turn.CompletedAt, turn.DeadlineAt, turn.Error = now, 0, "Claude worker exited before the queued turn started"
		m.state.TerminalOutcome = turn.Outcome
		m.queueTerminalNoticeLocked(*turn)
	}
	m.state.Status = "archived"
	m.state.AutoArchiveAt = 0
	if err := m.persistLocked(); err != nil {
		sessionID := m.state.SessionID
		m.mu.Unlock()
		fmt.Fprintf(os.Stderr, "claude-lane-manager: record worker exit for %s: %v\n", sessionID, err)
		// Keep the manager alive with the in-memory archived state. The
		// maintenance tick will retry the durable archive reservation before
		// any process or transport cleanup is allowed to proceed.
		return false
	}
	m.mu.Unlock()
	m.flushTerminalNotices()
	return m.shutdown("Claude worker exited", false)
}

func (m *claudeLaneManager) queueTerminalNoticeLocked(turn claudeLaneTurn) {
	queueClaudeLaneTerminalNotice(&m.state, turn)
}

func queueClaudeLaneTerminalNotice(state *claudeLaneState, turn claudeLaneTurn) {
	if state.NotifyTarget == "" {
		return
	}
	for _, notice := range state.Notices {
		if notice.TurnID == turn.ID {
			return
		}
	}
	noticeID := sessionKey("claude-lane-terminal\x00" + state.SessionID + "\x00" + turn.ID)
	collect := laneCollectionPointer("claude", state.SessionID, state.ParentHostID, state.ParentAgentRuntimeDir, state.Groups)
	message := fmt.Sprintf(
		"CLAUDE_LANE_TERMINAL notice=%s name=%s session=%s turn=%s status=%s outcome=%s exit=%d collection=required\nCollect: %s",
		noticeID, state.Name, state.SessionID, turn.ID, turn.Status, turn.Outcome, turn.Exit, collect,
	)
	state.Notices = append(state.Notices, claudeLaneNotice{
		ID:     noticeID,
		TurnID: turn.ID, Target: state.NotifyTarget, Message: message, CreatedAt: time.Now().UnixMilli(),
	})
}

func (m *claudeLaneManager) flushTerminalNoticesAsyncLocked() {
	if claudeLaneHasUnsentNotices(m.state) {
		go m.flushTerminalNotices()
	}
}

func (m *claudeLaneManager) flushTerminalNotices() {
	m.noticeMu.Lock()
	defer m.noticeMu.Unlock()
	noticeLock, err := lockClaudeLaneNotices(m.paths, m.state.SessionID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(noticeLock)
	for {
		m.mu.Lock()
		if latest, readErr := readClaudeLaneState(m.paths, m.state.SessionID); readErr == nil {
			m.state.Notices = latest.Notices
		}
		index := -1
		for current := range m.state.Notices {
			if m.state.Notices[current].SentAt == 0 {
				index = current
				break
			}
		}
		if index < 0 {
			m.mu.Unlock()
			return
		}
		notice := m.state.Notices[index]
		state := m.state
		if notice.LastAttemptAt > 0 && time.Now().UnixMilli()-notice.LastAttemptAt < 250 {
			m.mu.Unlock()
			return
		}
		m.state.Notices[index].Attempts++
		m.state.Notices[index].LastAttemptAt = time.Now().UnixMilli()
		_ = m.persistLocked()
		m.mu.Unlock()

		target := currentClaudeLaneNotifyTarget(m.paths, state, notice.Target)
		err := deliverClaudeLaneNotice(m.paths, state, target, notice.Message)
		m.mu.Lock()
		for current := range m.state.Notices {
			if m.state.Notices[current].ID == notice.ID && err == nil {
				m.state.Notices[current].SentAt = time.Now().UnixMilli()
				break
			}
		}
		_ = m.persistLocked()
		m.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func deliverClaudeLaneNotice(paths nativePaths, state claudeLaneState, target, message string) error {
	_ = paths
	return deliverGroupedLaneNotice(state.SessionID, target, "", message)
}

func flushOrphanClaudeLaneNotices(paths nativePaths, sessionID string) {
	noticeLock, err := lockClaudeLaneNotices(paths, sessionID)
	if err != nil {
		return
	}
	defer unlockLaneStateFile(noticeLock)
	state, err := readClaudeLaneState(paths, sessionID)
	if err != nil {
		return
	}
	for _, notice := range state.Notices {
		if notice.SentAt != 0 {
			continue
		}
		target := currentClaudeLaneNotifyTarget(paths, state, notice.Target)
		if err := deliverClaudeLaneNotice(paths, state, target, notice.Message); err != nil {
			return
		}
		lock, lockErr := lockLaneStateFile(paths, "claude-"+sessionID)
		if lockErr != nil {
			return
		}
		latest, readErr := readClaudeLaneState(paths, sessionID)
		if readErr == nil {
			for index := range latest.Notices {
				if latest.Notices[index].ID == notice.ID && latest.Notices[index].SentAt == 0 {
					latest.Notices[index].SentAt = time.Now().UnixMilli()
				}
			}
			_ = writeClaudeLaneStateUnlocked(paths, latest)
			state = latest
		}
		unlockLaneStateFile(lock)
		if readErr != nil {
			return
		}
	}
}

func lockClaudeLaneNotices(paths nativePaths, sessionID string) (*os.File, error) {
	directory := filepath.Join(profileDataRoot(paths), "claude-lane-notice-locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionKey(sessionID)+".lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // session id is hashed.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func currentClaudeLaneNotifyTarget(paths nativePaths, state claudeLaneState, fallback string) string {
	if fallback == "" {
		return ""
	}
	if exactProcessIdentityMatch(state.OwnerPID, state.OwnerProcStart) {
		row := readJSONMap(filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(state.OwnerPID)+".json"))
		if intValue(row["pid"]) == state.OwnerPID && stringValue(row["procStart"]) == state.OwnerProcStart && validSessionID(stringValue(row["sessionId"])) {
			return "session:" + stringValue(row["sessionId"])
		}
	}
	return fallback
}

func (m *claudeLaneManager) refreshCollectionAcknowledgementsLocked() error {
	lock, err := lockLaneStateFile(m.paths, "claude-"+m.state.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	latest, err := readClaudeLaneState(m.paths, m.state.SessionID)
	if err != nil {
		return err
	}
	if latest.Status == "archived" && m.state.Status != "archived" {
		m.closing = true
		return errors.New("claude lane was archived by another lifecycle operation")
	}
	m.mergeCollectionAcknowledgementsLocked(latest)
	return nil
}

func (m *claudeLaneManager) mergeCollectionAcknowledgementsLocked(latest claudeLaneState) {
	collected := map[string]bool{}
	for _, turn := range latest.Turns {
		collected[turn.ID] = turn.Collected
	}
	for index := range m.state.Turns {
		if collected[m.state.Turns[index].ID] {
			m.state.Turns[index].Collected = true
			m.state.Turns[index].ResultFrame = nil
		}
	}
	m.state.CollectedTurnID = latest.CollectedTurnID
	compactClaudeLaneTurns(&m.state)
}

func compactClaudeLaneTurns(state *claudeLaneState) {
	collected := 0
	for index := range state.Turns {
		if state.Turns[index].Collected {
			collected++
			state.Turns[index].ResultFrame = nil
		}
	}
	drop := max(collected-claudeLaneRetainedCollectedTurns, 0)
	prefix := 0
	for prefix < len(state.Turns) && drop > 0 && state.Turns[prefix].Collected {
		prefix++
		drop--
	}
	// Manager code may retain a pointer to a live turn across persistLocked.
	// Reslicing a collected prefix preserves every surviving element's address;
	// filtering or copying the slice here would detach those later mutations.
	state.Turns = state.Turns[prefix:]
}

func (m *claudeLaneManager) persistLocked() error {
	lock, err := lockLaneStateFile(m.paths, "claude-"+m.state.SessionID)
	if err != nil {
		return err
	}
	defer unlockLaneStateFile(lock)
	if latest, readErr := readClaudeLaneState(m.paths, m.state.SessionID); readErr == nil {
		if latest.Status == "archived" && m.state.Status != "archived" {
			m.closing = true
			return errors.New("claude lane was archived by another lifecycle operation")
		}
		m.mergeCollectionAcknowledgementsLocked(latest)
		m.state.TurnID = oldestClaudeLaneDebt(m.state)
	}
	return writeClaudeLaneStateUnlocked(m.paths, m.state)
}

//nolint:gocyclo // Archive reservation deliberately revalidates every lifecycle gate while holding the cross-process lock.
func (m *claudeLaneManager) markArchived(reason string, dueOnly bool) (bool, error) {
	lifecycle, err := lockLaneLifecycle(m.paths, "claude-"+m.state.SessionID)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		unlockLaneLifecycle(lifecycle)
		return false, nil
	}
	if dueOnly {
		now := time.Now().UnixMilli()
		if m.hasOutstandingExecutionLocked() || !m.state.AutoArchive || m.state.AutoArchiveAt == 0 || m.state.AutoArchiveAt > now {
			m.mu.Unlock()
			unlockLaneLifecycle(lifecycle)
			return false, nil
		}
		for _, notice := range m.state.Notices {
			if notice.SentAt == 0 && now < notice.CreatedAt+30_000 {
				m.mu.Unlock()
				unlockLaneLifecycle(lifecycle)
				return false, nil
			}
		}
	}
	m.closing = true
	m.lifecycleLock = lifecycle
	if reason == "explicit archive" {
		cancelClaudeLaneNoticesLocked(&m.state)
	}
	if m.state.Status != "archived" {
		now := time.Now().UnixMilli()
		for index := range m.state.Turns {
			turn := &m.state.Turns[index]
			if turn.Status != "active" && turn.Status != "submitted" && turn.Status != "queued" {
				continue
			}
			turn.Status, turn.Outcome, turn.Exit, turn.CompletedAt = "interrupted", "interrupted", 130, now
			turn.DeadlineAt, turn.Error = 0, reason
			m.state.TerminalOutcome = "interrupted"
		}
		m.state.Status, m.state.AutoArchiveAt, m.state.StartupID = "archived", 0, ""
	}
	// Persist even when the in-memory state was already archived. That state
	// can be the result of a prior failed worker-exit write; cleanup must not
	// proceed until the same archive is known durable.
	if err := m.persistLocked(); err != nil {
		if latest, readErr := readClaudeLaneState(m.paths, m.state.SessionID); readErr == nil && latest.Status == "archived" {
			m.state = latest
		}
		m.closing = false
		m.lifecycleLock = nil
		m.mu.Unlock()
		unlockLaneLifecycle(lifecycle)
		return false, err
	}
	m.mu.Unlock()
	return true, nil
}

func cancelClaudeLaneNoticesLocked(state *claudeLaneState) {
	now := time.Now().UnixMilli()
	for index := range state.Notices {
		if state.Notices[index].SentAt == 0 {
			state.Notices[index].SentAt = now
		}
	}
}

func (m *claudeLaneManager) shutdown(reason string, interrupt bool) bool {
	started, err := m.markArchived(reason, reason == "auto-archive delay elapsed")
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-lane-manager: archive %s: %v\n", m.state.SessionID, err)
		return false
	}
	if !started {
		return false
	}
	m.finishShutdown(interrupt)
	return true
}

//nolint:gocyclo // Cleanup keeps each owned process, socket, and durable field in one auditable transaction.
func (m *claudeLaneManager) finishShutdown(interrupt bool) {
	m.cleanupOnce.Do(func() {
		m.mu.Lock()
		worker, input, listener := m.worker, m.workerInput, m.listener
		controlSocket, shimSocket := m.state.ControlSocket, m.state.ShimSocket
		shimPID, shimProcStart := m.state.ShimPID, m.state.ShimProcStart
		workerPID, workerProcStart := m.state.WorkerPID, m.state.WorkerProcStart
		workerAlias := m.state.WorkerSocketAlias
		workerPeerState := m.state
		m.mu.Unlock()
		m.unregisterAgentPeer()
		if listener != nil {
			_ = listener.Close()
		}
		_ = os.Remove(controlSocket)
		if input != nil {
			_ = input.Close()
		}
		if worker != nil && worker.Process != nil && interrupt {
			_ = worker.Process.Signal(os.Interrupt)
		}
		if shimSocket != "" {
			_ = sendUnixJSON(shimSocket, map[string]any{"type": "control", "action": "shutdown"}, time.Second)
		}
		if workerAlias != "" {
			removeClaudeWorkerAlias(workerAlias, shimSocket)
		}
		dropClaudeLaneInbox(m.paths, m.state.SessionID)
		// Closing stream input lets Claude retire its own SDK registry row and
		// native peer socket. Escalate only after a real graceful-exit window.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && (processIdentityMayBeLive(workerPID, workerProcStart) || processIdentityMayBeLive(shimPID, shimProcStart)) {
			time.Sleep(25 * time.Millisecond)
		}
		if exactProcessIdentityMatch(workerPID, workerProcStart) && worker != nil && worker.Process != nil {
			_ = worker.Process.Kill()
		}
		if exactProcessIdentityMatch(shimPID, shimProcStart) {
			_ = syscall.Kill(shimPID, syscall.SIGKILL)
		}
		deadline = time.Now().Add(time.Second)
		for time.Now().Before(deadline) && (processIdentityMayBeLive(workerPID, workerProcStart) || processIdentityMayBeLive(shimPID, shimProcStart)) {
			time.Sleep(25 * time.Millisecond)
		}
		workerStopped := processIdentityStoppedOrUnset(workerPID, workerProcStart)
		shimStopped := processIdentityStoppedOrUnset(shimPID, shimProcStart)
		var workerCleanupErr error
		if workerStopped {
			workerCleanupErr = cleanupClaudeNativeWorkerPeer(m.paths, workerPeerState)
		}
		m.mu.Lock()
		m.state.ManagerPID, m.state.ManagerProcStart = 0, ""
		if workerStopped && workerCleanupErr == nil {
			m.state.WorkerPID, m.state.WorkerProcStart, m.state.WorkerSocket, m.state.WorkerSocketAlias = 0, "", "", ""
			m.state.CleanupError = ""
		} else if workerCleanupErr != nil {
			m.state.CleanupError = workerCleanupErr.Error()
		}
		if shimStopped {
			m.state.ShimPID, m.state.ShimProcStart, m.state.ShimSocket = 0, "", ""
		}
		_ = m.persistLocked()
		lifecycle := m.lifecycleLock
		m.lifecycleLock = nil
		m.mu.Unlock()
		if lifecycle != nil {
			unlockLaneLifecycle(lifecycle)
		}
		close(m.done)
	})
}

//nolint:gocyclo // Shared-registry cleanup re-attests every PID-bound row, socket, and key before removal.
func cleanupClaudeNativeWorkerPeer(paths nativePaths, state claudeLaneState) error {
	if state.WorkerPID <= 1 {
		return nil
	}
	if !processIdentityStoppedOrUnset(state.WorkerPID, state.WorkerProcStart) {
		return errors.New("native Claude worker PID is not absent during cleanup")
	}
	registryRoot := state.WorkerConfigDir
	if registryRoot == "" {
		registryRoot = paths.claudeRoot
	}
	registry := filepath.Join(registryRoot, "sessions", strconv.Itoa(state.WorkerPID)+".json")
	workerSocket := state.WorkerSocket
	rowPresent := false
	body, err := os.ReadFile(registry) //nolint:gosec // exact PID row in the selected shared Claude registry.
	if err == nil {
		var row map[string]any
		if json.Unmarshal(body, &row) != nil || !claudeNativeWorkerRowOwned(row, state) {
			return errors.New("native Claude worker row changed before cleanup")
		}
		rowPresent = true
		rowSocket := stringValue(row["messagingSocketPath"])
		if workerSocket == "" {
			workerSocket = rowSocket
		} else if rowSocket != workerSocket {
			return errors.New("native Claude worker row changed its messaging socket before cleanup")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if workerSocket == "" {
		if rowPresent {
			if !processIdentityStoppedOrUnset(state.WorkerPID, state.WorkerProcStart) {
				return errors.New("native Claude worker PID reappeared before row cleanup")
			}
			if err := os.Remove(registry); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	// Do not touch a recyclable PID path unless the kernel proves that no
	// process currently owns it. Unknown observations and PID reuse preserve it.
	if !processIdentityAllowsPIDPathRemoval(observeProcessIdentity(state.WorkerPID, "")) {
		return errors.New("native Claude worker PID is not absent before transport cleanup")
	}
	if !ownedClaudeWorkerSocketPath(workerSocket, paths.runtimeDir, state.WorkerPID) {
		return errors.New("native Claude worker socket is not PID-bound")
	}
	keyName, err := federator.ClaudeServiceKeyName(state.WorkerPID, workerSocket)
	if err != nil {
		return errors.New("native Claude worker peer-token sidecar path is invalid")
	}
	keyPath := filepath.Join(registryRoot, "sessions", keyName)
	keyPresent := false
	if info, statErr := os.Lstat(keyPath); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("native Claude worker peer-token sidecar changed type")
		}
		keyPresent = true
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	socketPresent := false
	if info, statErr := os.Lstat(workerSocket); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("native Claude worker socket changed type")
		}
		socketPresent = true
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	// Remove transports before the registry row. If an unlink fails, the exact
	// row remains a durable source for a later cleanup retry even when startup
	// failed before WorkerSocket was captured into lane state.
	if socketPresent {
		if !processIdentityAllowsPIDPathRemoval(observeProcessIdentity(state.WorkerPID, "")) {
			return errors.New("native Claude worker PID reappeared before socket cleanup")
		}
		if err := os.Remove(workerSocket); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if keyPresent {
		if !processIdentityAllowsPIDPathRemoval(observeProcessIdentity(state.WorkerPID, "")) {
			return errors.New("native Claude worker PID reappeared before key cleanup")
		}
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if rowPresent {
		if !processIdentityStoppedOrUnset(state.WorkerPID, state.WorkerProcStart) {
			return errors.New("native Claude worker PID reappeared before row cleanup")
		}
		if err := os.Remove(registry); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *claudeLaneManager) captureProcessStart(pid int) (string, error) {
	if m.captureProcStart != nil {
		return m.captureProcStart(pid)
	}
	return captureProcessStart(pid)
}

func ownedClaudeWorkerSocketPath(path, runtimeRoot string, pid int) bool {
	if pid <= 1 || filepath.Base(path) != strconv.Itoa(pid)+".sock" {
		return false
	}
	return ownedRuntimePath(path, runtimeRoot) || ownedClaudeSDKSocketPath(path, pid)
}

func claudeNativeWorkerRowOwned(row map[string]any, state claudeLaneState) bool {
	if intValue(row["pid"]) != state.WorkerPID || stringValue(row["sessionId"]) != state.SessionID ||
		stringValue(row["entrypoint"]) != "sdk-cli" ||
		(state.WorkerSocket != "" && stringValue(row["messagingSocketPath"]) != state.WorkerSocket) {
		return false
	}
	if state.WorkerProcStart == "" {
		// Develop-era Darwin state omitted process-start tokens. Remove its exact
		// session row only after the kernel proves the PID is absent; a recycled
		// live PID remains Unknown and is preserved.
		return cleanupProcessIdentityStatus(state.WorkerPID, "").Status == processIdentityStale
	}
	return stringValue(row["procStart"]) == state.WorkerProcStart
}

func removeClaudeWorkerAlias(alias, shimSocket string) {
	target, err := os.Readlink(alias)
	if err == nil && samePath(target, shimSocket) {
		_ = os.Remove(alias)
	}
}

//nolint:gocyclo // The tri-state preflight and wait keep cleanup all-or-nothing.
func cleanupClaudeLaneResidue(paths nativePaths, state claudeLaneState, excludedPID int) error {
	processes := []struct {
		pid       int
		procStart string
		signal    syscall.Signal
	}{
		{state.ManagerPID, state.ManagerProcStart, syscall.SIGTERM},
		{state.WorkerPID, state.WorkerProcStart, syscall.SIGKILL},
		{state.ShimPID, state.ShimProcStart, syscall.SIGTERM},
	}
	// Probe every recorded owner before mutating anything. One unknown identity
	// preserves the complete process/transport/state transaction for a retry.
	for _, process := range processes {
		if process.pid <= 1 || process.pid == excludedPID {
			continue
		}
		if cleanupProcessIdentityStatus(process.pid, process.procStart).Status == processIdentityUnknown {
			return errors.New("cannot currently corroborate Claude lane process cleanup; retry without changing lane state")
		}
	}
	for _, process := range processes {
		if process.pid > 1 && process.pid != excludedPID && process.procStart != "" && exactProcessIdentityMatch(process.pid, process.procStart) {
			_ = syscall.Kill(process.pid, process.signal)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		allStopped := true
		for _, process := range processes {
			if process.pid <= 1 || process.pid == excludedPID {
				continue
			}
			switch cleanupProcessIdentityStatus(process.pid, process.procStart).Status {
			case processIdentityUnknown:
				return errors.New("cannot currently corroborate Claude lane process cleanup; retry without changing lane state")
			case processIdentityMatches:
				allStopped = false
			case processIdentityStale:
			}
		}
		if allStopped {
			break
		}
		if time.Now().After(deadline) {
			// A manager handling SIGTERM may be waiting for the lifecycle lock
			// held by this transaction. Escalate only identities that still match;
			// Unknown remains a preservation boundary.
			for _, process := range processes {
				if process.pid <= 1 || process.pid == excludedPID {
					continue
				}
				switch cleanupProcessIdentityStatus(process.pid, process.procStart).Status {
				case processIdentityUnknown:
					return errors.New("cannot currently corroborate Claude lane process cleanup; retry without changing lane state")
				case processIdentityMatches:
					_ = syscall.Kill(process.pid, syscall.SIGKILL)
				case processIdentityStale:
				}
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	deadline = time.Now().Add(time.Second)
	for _, process := range processes {
		if process.pid <= 1 || process.pid == excludedPID {
			continue
		}
		for {
			switch cleanupProcessIdentityStatus(process.pid, process.procStart).Status {
			case processIdentityStale:
				goto stopped
			case processIdentityUnknown:
				return errors.New("cannot currently corroborate Claude lane process cleanup; retry without changing lane state")
			case processIdentityMatches:
				if time.Now().After(deadline) {
					return errors.New("timed out proving Claude lane process cleanup")
				}
				time.Sleep(25 * time.Millisecond)
			}
		}
	stopped:
	}
	if err := cleanupClaudeNativeWorkerPeer(paths, state); err != nil {
		return err
	}
	if state.WorkerSocketAlias != "" {
		removeClaudeWorkerAlias(state.WorkerSocketAlias, state.ShimSocket)
	}
	_ = os.Remove(state.ControlSocket)
	return nil
}

func dropClaudeLaneInbox(paths nativePaths, sessionID string) {
	names, pending, err := nativeInboxNames(paths, sessionID)
	if err != nil {
		return
	}
	for _, name := range names {
		path := filepath.Join(pending, name)
		item := readJSONMap(path)
		if item != nil {
			if record := readWakeRecord(paths, sessionID, stringValue(item["id"])); record != nil {
				record.State, record.Delivery = "failed", "failed"
				_ = writeWakeRecord(paths, *record)
			}
		}
		_ = os.Remove(path)
	}
}

func forceArchiveClaudeLane(paths nativePaths, sessionID, reason string) error {
	lock, err := lockLaneLifecycle(paths, "claude-"+sessionID)
	if err != nil {
		return err
	}
	defer unlockLaneLifecycle(lock)
	state, err := readClaudeLaneState(paths, sessionID)
	if err != nil {
		return err
	}
	if err := cleanupClaudeLaneResidue(paths, state, os.Getpid()); err != nil {
		return err
	}
	for index := range state.Turns {
		if state.Turns[index].Status == "active" || state.Turns[index].Status == "submitted" || state.Turns[index].Status == "queued" {
			state.Turns[index].Status, state.Turns[index].Outcome, state.Turns[index].Exit = "interrupted", "interrupted", 130
			state.Turns[index].CompletedAt, state.Turns[index].DeadlineAt, state.Turns[index].Error = time.Now().UnixMilli(), 0, reason
			state.TerminalOutcome = "interrupted"
			queueClaudeLaneTerminalNotice(&state, state.Turns[index])
		}
	}
	state.Status, state.AutoArchiveAt, state.StartupID = "archived", 0, ""
	if reason == "manager unavailable" {
		cancelClaudeLaneNoticesLocked(&state)
	}
	if reason != "Claude lane manager exited" {
		dropClaudeLaneInbox(paths, sessionID)
	}
	state.ManagerPID, state.ManagerProcStart = 0, ""
	state.WorkerPID, state.WorkerProcStart, state.WorkerSocket, state.WorkerSocketAlias = 0, "", "", ""
	state.ShimPID, state.ShimProcStart, state.ShimSocket = 0, "", ""
	state.CleanupError = ""
	if err := writeClaudeLaneState(paths, state); err != nil {
		return err
	}
	return nil
}

func reconcileClaudeLaneManagers(paths nativePaths) {
	now := time.Now().UnixMilli()
	for _, state := range readClaudeLaneStates(paths) {
		reconcileClaudeLaneManager(paths, state, now)
	}
}

func reconcileClaudeLaneManager(paths nativePaths, state claudeLaneState, now int64) {
	managerIdentity := cleanupProcessIdentityStatus(state.ManagerPID, state.ManagerProcStart)
	managerUnavailable := state.ManagerPID <= 1 || managerIdentity.Status == processIdentityStale
	if claudeLaneHasUnsentNotices(state) && managerUnavailable {
		flushOrphanClaudeLaneNotices(paths, state.SessionID)
		if latest, err := readClaudeLaneState(paths, state.SessionID); err == nil {
			state = latest
		}
	}
	startingOrphan := state.Status == "starting" && state.ManagerPID <= 1 && state.UpdatedAt+20_000 <= now
	managerExited := state.ManagerPID > 1 && managerIdentity.Status == processIdentityStale
	archivedResidue := claudeLaneHasArchivedResidue(state)
	if startingOrphan || managerExited || archivedResidue {
		if forceArchiveClaudeLane(paths, state.SessionID, "Claude lane manager exited") == nil {
			flushOrphanClaudeLaneNotices(paths, state.SessionID)
		}
	}
}

func claudeLaneHasArchivedResidue(state claudeLaneState) bool {
	if state.Status != "archived" {
		return false
	}
	workerLive := processIdentityMayBeLive(state.WorkerPID, state.WorkerProcStart)
	shimLive := processIdentityMayBeLive(state.ShimPID, state.ShimProcStart)
	return workerLive || shimLive || state.WorkerSocket != "" || state.WorkerSocketAlias != ""
}

func claudeLaneHasUnsentNotices(state claudeLaneState) bool {
	for _, notice := range state.Notices {
		if notice.SentAt == 0 {
			return true
		}
	}
	return false
}

func int64Value(value any) int64 {
	switch current := value.(type) {
	case float64:
		return int64(current)
	case int64:
		return current
	case json.Number:
		result, _ := current.Int64()
		return result
	default:
		return 0
	}
}
