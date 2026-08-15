package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const agyLaunchTokenEnvironment = "AGENT_SESSIONS_AGY_LAUNCH_TOKEN"

type agyHookInput struct {
	ConversationID       string   `json:"conversationId"`
	WorkspacePaths       []string `json:"workspacePaths"`
	TranscriptPath       string   `json:"transcriptPath"`
	ArtifactDirectory    string   `json:"artifactDirectoryPath"`
	ModelName            string   `json:"modelName"`
	InvocationNumber     int      `json:"invocationNum"`
	InitialNumberOfSteps int      `json:"initialNumSteps"`
	FullyIdle            bool     `json:"fullyIdle"`
	TerminationReason    string   `json:"terminationReason"`
}

func runAgyLaunchCommand(argv []string) int {
	if len(argv) < 1 || argv[0] != "prepare" {
		fmt.Fprintln(os.Stderr, "usage: agent-session-runtime agy-launch prepare --cwd DIR --owner-pid PID --owner-proc-start TOKEN [--name NAME] [--permission-mode MODE]")
		return 2
	}
	args := parseArgs(argv[1:])
	ownerPID, err := strconv.Atoi(args["owner-pid"])
	if err != nil || ownerPID <= 1 || ownerPID != os.Getppid() {
		fmt.Fprintln(os.Stderr, "agy launch owner must be the runtime's live parent process")
		return 1
	}
	ownerStart := strings.TrimSpace(args["owner-proc-start"])
	if ownerStart == "" || !exactProcessIdentityMatch(ownerPID, ownerStart) {
		fmt.Fprintln(os.Stderr, "agy launch owner identity could not be corroborated")
		return 1
	}
	cwd, err := canonicalAgyCwd(args["cwd"])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	token, err := newAgentLaunchToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create agy launch token: %v\n", err)
		return 1
	}
	paths := resolveNativePaths()
	cleanupStaleAgentLaunches(paths)
	now := time.Now().UnixMilli()
	record := agentLaunchRecord{
		Product:  "agy",
		OwnerPID: ownerPID, OwnerProcStart: ownerStart, Cwd: cwd,
		Name: sanitizeNameOrEmpty(args["name"]), PermissionMode: defaultString(args["permission-mode"], "default"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := writeJSONAtomic(agentLaunchRecordPath(paths, token), record); err != nil {
		fmt.Fprintf(os.Stderr, "write agy launch record: %v\n", err)
		return 1
	}
	fmt.Println(token)
	return 0
}

func runAgyHookCommand(argv []string) {
	if len(argv) != 1 {
		fmt.Fprintln(os.Stderr, "agent-session-runtime agy-hook requires one hook event name")
		fmt.Println("{}")
		return
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, maxFrameBytes+1))
	if err == nil && len(body) > maxFrameBytes {
		err = errors.New("hook input exceeds 1 MiB")
	}
	var input agyHookInput
	if err == nil {
		err = json.Unmarshal(body, &input)
	}
	var output map[string]any
	if err == nil {
		output, err = handleAgyHook(argv[0], input)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions Antigravity hook degraded gracefully: %v\n", err)
		output = map[string]any{}
	}
	if output == nil {
		output = map[string]any{}
	}
	encoded, _ := json.Marshal(output)
	fmt.Println(string(encoded))
}

func handleAgyHook(event string, input agyHookInput) (map[string]any, error) {
	status, recognized := agyHookStatus(event)
	if !recognized {
		return map[string]any{}, nil
	}
	if !validSessionID(input.ConversationID) {
		return nil, errors.New("hook input omitted a valid Antigravity conversationId")
	}
	paths := resolveNativePaths()
	token := strings.TrimSpace(os.Getenv(agyLaunchTokenEnvironment))
	_, recordPath, err := authorizedAgentLaunch(paths, token, "agy", false)
	if err != nil {
		// Hooks are installed globally. Ordinary agy sessions are silent no-ops.
		return map[string]any{}, nil
	}
	unlock, err := lockAgentLaunch(paths, recordPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	record, _, err := authorizedAgentLaunch(paths, token, "agy", false)
	if err != nil {
		return nil, err
	}
	if record.ConversationID != "" && record.ConversationID != input.ConversationID {
		return nil, errors.New("launcher token is already attached to another Antigravity conversation")
	}
	record.ConversationID = input.ConversationID
	record.UpdatedAt = time.Now().UnixMilli()
	state, err := ensureAgyShim(paths, record, recordPath, status)
	if err != nil {
		return nil, err
	}

	messages, queued, err := consumeNativeInboxLimited(paths, input.ConversationID)
	if err != nil {
		return nil, err
	}
	context := ""
	if len(messages) > 0 {
		context = formatNativeHookMessages(messages)
	} else if queued {
		context = nativeInboxOverflowNotice()
	}
	startup := ""
	if event == "PreInvocation" && !record.StartupInjected {
		startup = agyStartupContext(state)
		record.StartupInjected = true
	}
	if err := writeJSONAtomic(recordPath, record); err != nil {
		return nil, err
	}

	return agyHookOutput(event, startup, context), nil
}

func agyHookStatus(event string) (string, bool) {
	switch event {
	case "PreInvocation":
		return "busy", true
	case "PostInvocation":
		return "waiting", true
	case "Stop":
		return "idle", true
	default:
		return "", false
	}
}

func agyHookOutput(event, startup, context string) map[string]any {
	combined := strings.TrimSpace(strings.Join(nonEmptyStrings(startup, context), "\n\n"))
	switch {
	case event == "PreInvocation" && combined != "":
		return map[string]any{"injectSteps": []map[string]any{{"ephemeralMessage": combined}}}
	case event == "PostInvocation" && context != "":
		return map[string]any{
			"injectSteps":         []map[string]any{{"ephemeralMessage": context}},
			"terminationBehavior": "force_continue",
		}
	case event == "Stop" && context != "":
		return map[string]any{"decision": "continue", "reason": context}
	default:
		return map[string]any{}
	}
}

func runAgyMCPCommand() int {
	return runAttestedMCPCommand("agent-sessions Antigravity mcp", true, func(_ json.RawMessage) (string, error) {
		paths := resolveNativePaths()
		record, _, err := authorizedAgentLaunch(paths, strings.TrimSpace(os.Getenv(agyLaunchTokenEnvironment)), "agy", true)
		if err != nil {
			return "", err
		}
		state, stateErr := readOwnNativeState(paths, record.ConversationID)
		if stateErr != nil || stringValue(state["entrypoint"]) != "agy" {
			return "", errors.New("peer shim is not live for the Antigravity conversation")
		}
		return record.ConversationID, nil
	})
}

func ensureAgyShim(paths nativePaths, record agentLaunchRecord, recordPath, status string) (map[string]any, error) {
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(record.ConversationID), "state.json")
	desired := map[string]any{
		"type": "control", "action": "update", "cwd": record.Cwd,
		"permissionMode": record.PermissionMode, "status": status,
	}
	if state, live, err := updateAgyShim(statePath, record, 0, desired); err != nil || live {
		return state, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	name := record.Name
	if name == "" {
		name = fmt.Sprintf("agy-%s-%s", sanitizeName(filepath.Base(record.Cwd)), first8(record.ConversationID))
	}
	args := []string{
		"shim", "--session-id", record.ConversationID, "--cwd", record.Cwd,
		"--permission-mode", record.PermissionMode, "--name", name, "--name-source", "launch",
		"--entrypoint", "agy", "--version", "agent-sessions/0.1.0",
		"--data-dir", paths.dataRoot, "--claude-config-dir", paths.claudeRoot,
		"--codex-home", paths.codexHome, "--runtime-dir", paths.runtimeDir,
		"--owner-pid", strconv.Itoa(record.OwnerPID), "--owner-proc-start", record.OwnerProcStart,
		"--launch-record", recordPath,
	}
	command := exec.Command(executable, args...) //nolint:gosec // executable is this installed native runtime.
	command.Env = overrideNativeEnv(os.Environ(), "GOMAXPROCS", "1")
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if state, live, _ := updateAgyShim(statePath, record, pid, desired); live {
			return state, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out starting the Antigravity peer socket for %s", record.ConversationID)
}

func updateAgyShim(statePath string, record agentLaunchRecord, expectedPID int, desired map[string]any) (map[string]any, bool, error) {
	state := readJSONMap(statePath)
	if state == nil || stringValue(state["sessionId"]) != record.ConversationID || stringValue(state["entrypoint"]) != "agy" {
		return nil, false, nil
	}
	if intValue(state["ownerPid"]) != record.OwnerPID || stringValue(state["ownerProcStart"]) != record.OwnerProcStart {
		return nil, false, errors.New("conversation is already published by another Antigravity launcher")
	}
	if expectedPID > 0 && intValue(state["pid"]) != expectedPID {
		return nil, false, nil
	}
	socket := stringValue(state["socketPath"])
	if socket == "" || !probeUnixSocket(socket, 250*time.Millisecond) {
		return nil, false, nil
	}
	if err := sendUnixJSON(socket, desired, time.Second); err != nil {
		return nil, false, err
	}
	return readJSONMap(statePath), true, nil
}

func authorizedPeerSessionNative(paths nativePaths, sessionID string) bool {
	if authorizedPeerThreadNative(paths, sessionID) {
		return true
	}
	return liveAgentLaunchForSession(paths, sessionID, "agy") != nil
}

func inferAgyParent(paths nativePaths, startPID int) (laneOwner, bool) {
	record, _, err := authorizedAgentLaunchForProcess(
		paths, strings.TrimSpace(os.Getenv(agyLaunchTokenEnvironment)), "agy", true, startPID,
	)
	if err != nil {
		return laneOwner{}, false
	}
	state, stateErr := readOwnNativeState(paths, record.ConversationID)
	if stateErr != nil || stringValue(state["entrypoint"]) != "agy" ||
		stringValue(state["sessionId"]) != record.ConversationID ||
		stringValue(state["socketPath"]) == "" ||
		!probeUnixSocket(stringValue(state["socketPath"]), 250*time.Millisecond) {
		return laneOwner{}, false
	}
	return laneOwner{
		PID: record.OwnerPID, ProcStart: record.OwnerProcStart,
		SessionID: record.ConversationID, PermissionMode: record.PermissionMode,
	}, true
}

func canonicalAgyCwd(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("agy launch cwd is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve agy launch cwd: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("agy launch cwd does not exist: %s", absolute)
	}
	return absolute, nil
}

func sanitizeNameOrEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return sanitizeName(value)
}

func agyStartupContext(state map[string]any) string {
	return fmt.Sprintf(
		"Agent Sessions peer messaging is active. This Antigravity conversation is advertised as %q with conversation ID %q. The agent_sessions MCP server derives this identity from the attested host process; session_id is optional and, when supplied, must match. Use list_peers before sending when the recipient is unclear.",
		stringValue(state["name"]), stringValue(state["sessionId"]),
	)
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}
