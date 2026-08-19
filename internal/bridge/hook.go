package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type hookInput struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
}

const hookAdditionalContextLimit = 2400

func runHookCommand() {
	body, err := io.ReadAll(io.LimitReader(os.Stdin, maxFrameBytes+1))
	if err == nil && len(body) > maxFrameBytes {
		err = errors.New("hook input exceeds 1 MiB")
	}
	var input hookInput
	if err == nil {
		err = json.Unmarshal(body, &input)
	}
	var output map[string]any
	if err == nil {
		output, err = handleNativeHook(input)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native hook degraded gracefully: %v\n", err)
		output = map[string]any{}
	}
	if output == nil {
		return
	}
	encoded, _ := json.Marshal(output)
	fmt.Println(string(encoded))
}

//nolint:gocyclo // Each hook event has a separate, explicit delivery policy.
func handleNativeHook(input hookInput) (map[string]any, error) {
	paths := resolveNativePaths()
	if input.Event == "SessionEnd" {
		// Hook metadata identifies a thread, not the client attachment that
		// ended. Exact owner-process reconciliation owns teardown.
		return nil, nil
	}
	if input.Event != "SessionStart" && input.Event != "UserPromptSubmit" && input.Event != "Stop" {
		return nil, nil
	}
	threadID, err := codexHookThreadID(paths, input)
	if err != nil {
		return nil, err
	}
	// Codex hook session_id is a session-family identifier. The rollout's
	// session_meta record binds that family to the exact App Server thread that
	// owns peer authority. They are normally equal for a fresh root but may
	// differ after resume, migration, or in an internal child thread.
	input.SessionID = threadID
	var owner *interactiveOwnerRecord
	lateAttached := false
	if input.Event == "SessionStart" {
		owner = liveAuthorizedInteractiveOwnerRecord(paths, input.SessionID)
		if owner == nil && !activeCodexLaneThreadNative(paths, input.SessionID) {
			// Reject ordinary daemon-wide hooks before creating their lifecycle
			// lock file. Only a bridge-minted owner or lane may mutate state.
			return nil, nil
		}
		if owner != nil && owner.Pending {
			var err error
			owner, err = markPreparedLaunchAttached(paths, input.SessionID)
			if err != nil {
				return nil, err
			}
		}
	} else {
		owner = liveInteractiveOwnerRecord(paths, input.SessionID)
		if input.Event == "UserPromptSubmit" && owner == nil {
			var err error
			owner, lateAttached, err = attachPreparedOwnerFromUserPrompt(paths, input.SessionID)
			if err != nil {
				return nil, err
			}
		}
	}
	if !authorizedPeerThreadNative(paths, input.SessionID) {
		// Codex installs hooks daemon-wide. An ordinary thread is a silent
		// no-op and cannot start a supervisor, publish a shim, or consume inbox.
		return nil, nil
	}
	switch input.Event {
	case "SessionStart":
		if err := ensureHookSupervisorCurrent(paths); err != nil {
			return nil, err
		}
		state, err := ensureHookShim(paths, input, owner)
		if err != nil {
			return nil, err
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "SessionStart", "additionalContext": hookStartupContext(state),
		}}, nil
	case "UserPromptSubmit":
		state, err := ensureHookShim(paths, input, owner)
		if err != nil {
			return nil, err
		}
		updateHookShim(state, "busy")
		messages, queued, err := consumeNativeInboxLimited(paths, input.SessionID, hookAdditionalContextLimit)
		if err != nil {
			return map[string]any{}, err
		}
		contexts := make([]string, 0, 2)
		if lateAttached {
			// Recover a prepared owner that remained pending after SessionStart.
			// The first prompt is the next hook that can prove the TUI exec and
			// return the session-scoped MCP instructions.
			contexts = append(contexts, hookStartupContext(state))
		}
		if len(messages) > 0 {
			contexts = append(contexts, formatNativeHookMessages(messages))
		} else if queued {
			contexts = append(contexts, nativeInboxOverflowNotice())
		}
		if len(contexts) == 0 {
			return map[string]any{}, nil
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "UserPromptSubmit", "additionalContext": strings.Join(contexts, "\n\n"),
		}}, nil
	case "Stop":
		state, err := ensureHookShim(paths, input, owner)
		if err != nil {
			return nil, err
		}
		updateHookShim(state, "idle")
		messages, queued, err := consumeNativeInboxLimited(paths, input.SessionID, hookAdditionalContextLimit)
		if err != nil || (len(messages) == 0 && !queued) {
			return map[string]any{}, err
		}
		reason := formatNativeHookMessages(messages)
		if len(messages) == 0 && queued {
			reason = nativeInboxOverflowNotice()
		}
		return map[string]any{"decision": "block", "reason": reason}, nil
	default:
		return nil, nil
	}
}

// preparedOwnerProcessArgs is a test seam around the PID/start-attested argv
// reader. Production calls always recheck the immutable process identity before
// and after reading argv.
var preparedOwnerProcessArgs = readPeerProcessArgs

// attachPreparedOwnerFromUserPrompt recovers the Codex 0.148 pending-owner case
// without granting pending owners general MCP authority. A prompt promotes only
// the exact live launcher process after it has exec'd the managed `--remote
// unix:// resume <thread>` attachment. A failed exec, stale/reused PID, unrelated
// daemon thread, or merely published prepared owner remains pending.
func attachPreparedOwnerFromUserPrompt(paths nativePaths, threadID string) (*interactiveOwnerRecord, bool, error) {
	record := liveAuthorizedInteractiveOwnerRecord(paths, threadID)
	if record == nil || !record.Pending || !record.Prepared {
		return nil, false, nil
	}
	args, err := preparedOwnerProcessArgs(record.OwnerPID, record.OwnerProcStart)
	if err != nil {
		return nil, false, fmt.Errorf("read prepared Codex owner argv: %w", err)
	}
	if !managedCodexResumeArgv(args, threadID) {
		return nil, false, nil
	}
	attached, err := markPreparedLaunchAttached(paths, threadID)
	if err != nil {
		return nil, false, err
	}
	return attached, attached != nil && !attached.Pending, nil
}

func managedCodexResumeArgv(args []string, threadID string) bool {
	if !validSessionID(threadID) {
		return false
	}
	for index := 0; index+3 < len(args); index++ {
		if args[index] == "--" {
			return false
		}
		if args[index] == "--remote" && args[index+1] == "unix://" &&
			args[index+2] == "resume" && args[index+3] == threadID {
			return true
		}
	}
	return false
}

// codexHookThreadID resolves the exact thread represented by a Codex hook.
// Modern Codex hooks carry a materialized local rollout path. Its first
// session_meta record is the host-owned binding between the hook's session
// family and the exact thread; neither model arguments nor a family id alone
// can grant another thread's peer capability.
func codexHookThreadID(paths nativePaths, input hookInput) (string, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if !validSessionID(sessionID) {
		return "", errors.New("codex hook input did not include a valid session_id")
	}
	transcriptPath := strings.TrimSpace(input.TranscriptPath)
	if transcriptPath == "" {
		// Compatibility with Codex versions that predate transcript_path. Their
		// hook session_id was the exact root thread id.
		return sessionID, nil
	}

	resolvedTranscript, err := confinedCodexHookTranscript(paths, transcriptPath)
	if err != nil {
		return "", err
	}
	threadID, metadataSessionID, err := readCodexHookSessionMeta(resolvedTranscript)
	if err != nil {
		return "", err
	}
	if metadataSessionID == "" {
		metadataSessionID = threadID
	}
	if !validSessionID(threadID) || !validSessionID(metadataSessionID) || metadataSessionID != sessionID {
		return "", errors.New("codex hook transcript identity does not match the hook")
	}
	return threadID, nil
}

func confinedCodexHookTranscript(paths nativePaths, transcriptPath string) (string, error) {
	resolvedTranscript, err := filepath.EvalSymlinks(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("resolve Codex hook transcript: %w", err)
	}
	resolvedSessions, err := filepath.EvalSymlinks(filepath.Join(paths.codexHome, "sessions"))
	if err != nil {
		return "", fmt.Errorf("resolve Codex sessions directory: %w", err)
	}
	relative, err := filepath.Rel(resolvedSessions, resolvedTranscript)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("codex hook transcript is outside the configured session store")
	}
	return resolvedTranscript, nil
}

func readCodexHookSessionMeta(transcriptPath string) (string, string, error) {
	file, err := os.Open(transcriptPath) //nolint:gosec // caller supplies the canonical path confined beneath CODEX_HOME/sessions.
	if err != nil {
		return "", "", fmt.Errorf("open Codex hook transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("codex hook transcript is not a regular file")
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				ID        string `json:"id"`
				SessionID string `json:"session_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil || record.Type != "session_meta" {
			return "", "", errors.New("codex hook transcript does not start with session metadata")
		}
		threadID := strings.TrimSpace(record.Payload.ID)
		metadataSessionID := strings.TrimSpace(record.Payload.SessionID)
		return threadID, metadataSessionID, nil
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("read Codex hook transcript metadata: %w", err)
	}
	return "", "", errors.New("codex hook transcript is empty")
}

func ensureHookSupervisorCurrent(paths nativePaths) error {
	status, err := requestControl(paths.supervisorSock, map[string]any{"action": "status"}, 500*time.Millisecond)
	if err != nil {
		// No managed supervisor preserves the existing standalone-shim fallback.
		return nil //nolint:nilerr // Absence is not an identity mismatch.
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	runtimeIdentity, err := nativeExecutableIdentity(executable)
	if err != nil {
		return err
	}
	if stringValue(status["runtimeIdentity"]) == runtimeIdentity {
		return nil
	}
	// Hook execution is bounded by Codex's 10-second budget. Wrappers and lane
	// launchers establish the managed runtime before creating peer authority;
	// a hook must therefore fail quickly instead of spending up to 25 seconds
	// stopping and replacing a mismatched supervisor.
	return errors.New("live supervisor runtime identity does not match the hook executable")
}

// ensureHookShim is the lifecycle boundary that validates and reconciles every
// hook state before exposing the session to peers.
//
//nolint:gocyclo
func ensureHookShim(paths nativePaths, input hookInput, launch *interactiveOwnerRecord) (map[string]any, error) {
	if !validSessionID(input.SessionID) {
		return nil, errors.New("codex hook input did not include a valid session_id")
	}
	cwd := defaultString(input.Cwd, mustGetwd())
	// The profile-scoped control socket is the authority. A managed App Server
	// may have been bootstrapped by another process and cannot inherit an
	// environment marker added by a later launcher.
	supervisorAvailable := probeUnixSocket(paths.supervisorSock, 300*time.Millisecond)
	var lane *laneState
	if launch == nil && activeCodexLaneThreadNative(paths, input.SessionID) {
		if current, err := readLaneStateFile(paths, input.SessionID); err == nil {
			lane = &current
		}
	}
	if launch == nil && lane == nil {
		return nil, errors.New("refuse to publish an unattested Codex thread")
	}
	if launch != nil && launch.Cwd != "" {
		cwd = launch.Cwd
	} else if lane != nil && lane.Cwd != "" {
		cwd = lane.Cwd
	}
	configuredName := strings.TrimSpace(os.Getenv("CLAUDE_PEER_SESSION_NAME"))
	nameSource := "explicit"
	if launch != nil && launch.Name != "" {
		configuredName = launch.Name
		nameSource = defaultString(launch.NameSource, "launch")
	} else if lane != nil && lane.Name != "" {
		configuredName = lane.Name
		nameSource = "lane"
	}
	name := configuredName
	if name == "" {
		name = defaultPeerName(cwd, input.SessionID)
		nameSource = "generated"
	}
	permission := defaultString(input.PermissionMode, "default")
	if lane != nil && lane.PermissionMode != "" {
		permission = lane.PermissionMode
	}
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(input.SessionID), "state.json")
	previousState := readJSONMap(statePath)
	if previousState != nil && stringValue(previousState["sessionId"]) == input.SessionID &&
		stringValue(previousState["name"]) != "" &&
		(input.Event != "SessionStart" || stringValue(previousState["nameSource"]) == "manual") {
		name = stringValue(previousState["name"])
		nameSource = defaultString(stringValue(previousState["nameSource"]), nameSource)
	}
	desired := map[string]any{
		"type": "control", "action": "update", "cwd": cwd, "permissionMode": permission,
		"status": map[bool]string{true: "busy", false: "idle"}[input.Event == "UserPromptSubmit"],
	}
	if supervisorAvailable {
		desired["supervisorSocket"] = paths.supervisorSock
	}
	if name != "" && input.Event == "SessionStart" {
		desired["name"] = name
		desired["nameSource"] = nameSource
	}
	if state := previousState; state != nil && stringValue(state["sessionId"]) == input.SessionID {
		if socket := stringValue(state["socketPath"]); socket != "" && probeUnixSocket(socket, 250*time.Millisecond) {
			if err := sendUnixJSON(socket, desired, time.Second); err == nil {
				return readJSONMap(statePath), nil
			}
		}
	}
	if supervisorAvailable {
		response, err := requestControl(paths.supervisorSock, map[string]any{
			"action": "register", "sessionId": input.SessionID, "cwd": cwd, "name": name,
			"nameSource": nameSource, "permissionMode": permission,
			"status": map[bool]string{true: "busy", false: "idle"}[input.Event == "UserPromptSubmit"],
		}, 15*time.Second)
		if err == nil {
			if state, ok := response["state"].(map[string]any); ok {
				return state, nil
			}
			err = errors.New("supervisor register returned no shim state")
		}
		// Registration publishes the shim before thread subscription. A timeout
		// or subscription error can therefore leave a perfectly live owner even
		// though the request failed. Reconcile that state before considering a
		// standalone fallback, or two shims can claim the same stable alias.
		deadline := time.Now().Add(2 * time.Second)
		for {
			state := readJSONMap(statePath)
			if state != nil && stringValue(state["sessionId"]) == input.SessionID {
				if socket := stringValue(state["socketPath"]); socket != "" && probeUnixSocket(socket, 250*time.Millisecond) {
					_ = sendUnixJSON(socket, desired, time.Second)
					return state, nil
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if probeUnixSocket(paths.supervisorSock, 300*time.Millisecond) {
			return nil, fmt.Errorf("supervisor registration failed while supervisor remains authoritative: %w", err)
		}
		supervisorAvailable = false
	}
	if launch == nil {
		return nil, errors.New("managed supervisor is unavailable for a Codex lane hook")
	}
	shim := firstEnv("CLAUDE_PEER_SHIM_BIN", os.Args[0])
	args := []string{
		"shim",
		"--session-id", input.SessionID, "--cwd", cwd, "--permission-mode", permission,
		"--name", name, "--name-source", nameSource, "--data-dir", paths.dataRoot,
		"--claude-config-dir", paths.claudeRoot, "--codex-home", paths.codexHome,
		"--runtime-dir", paths.runtimeDir,
	}
	if supervisorAvailable {
		args = append(args, "--supervisor-socket", paths.supervisorSock)
	}
	args = append(args, "--owner-pid", strconv.Itoa(launch.OwnerPID), "--owner-proc-start", launch.OwnerProcStart)
	command := exec.Command(shim, args...) //nolint:gosec // shim is the installed bridge binary and args are constructed above.
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
		state := readJSONMap(statePath)
		if intValue(state["pid"]) == pid {
			if socket := stringValue(state["socketPath"]); socket != "" && probeUnixSocket(socket, 250*time.Millisecond) {
				return state, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out starting the Claude peer socket for %s", input.SessionID)
}

func updateHookShim(state map[string]any, status string) {
	if socket := stringValue(state["socketPath"]); socket != "" {
		_ = sendUnixJSON(socket, map[string]any{"type": "control", "action": "status", "status": status}, time.Second)
	}
}

func consumeNativeInbox(paths nativePaths, sessionID string) ([]map[string]any, error) {
	names, pending, err := nativeInboxNames(paths, sessionID)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	for _, name := range names {
		file := filepath.Join(pending, name)
		if value := readJSONMap(file); value != nil {
			if wakeInboxAlreadyDelivered(paths, sessionID, value) {
				_ = os.Remove(file)
				continue
			}
			messages = append(messages, value)
			markWakeInboxDelivered(paths, sessionID, value)
		}
		_ = os.Remove(file)
	}
	return messages, nil
}

// consumeNativeInboxLimited only acknowledges files whose complete rendered
// form fits in the hook host's bounded context.  The first overflow and every
// later message remain queued for check_inbox or a subsequent hook; no message
// is truncated and then silently deleted.
func consumeNativeInboxLimited(paths nativePaths, sessionID string, limit int) ([]map[string]any, bool, error) {
	names, pending, err := nativeInboxNames(paths, sessionID)
	if err != nil {
		return nil, false, err
	}
	messages := []map[string]any{}
	for index, name := range names {
		file := filepath.Join(pending, name)
		value := readJSONMap(file)
		if value == nil {
			_ = os.Remove(file)
			continue
		}
		if wakeInboxAlreadyDelivered(paths, sessionID, value) {
			_ = os.Remove(file)
			continue
		}
		candidate := append(append([]map[string]any{}, messages...), value)
		if len(formatNativeHookMessages(candidate)) > limit {
			return messages, true, nil
		}
		messages = candidate
		_ = os.Remove(file)
		markWakeInboxDelivered(paths, sessionID, value)
		if index == len(names)-1 {
			return messages, false, nil
		}
	}
	return messages, false, nil
}

func wakeInboxAlreadyDelivered(paths nativePaths, sessionID string, item map[string]any) bool {
	record := readWakeRecord(paths, sessionID, stringValue(item["id"]))
	return record != nil && (record.State == "delivered" || record.State == "fallback_delivered")
}

func markWakeInboxDelivered(paths nativePaths, sessionID string, item map[string]any) {
	record := readWakeRecord(paths, sessionID, stringValue(item["id"]))
	if record == nil || (record.State != "queueing" && record.State != "queued") {
		return
	}
	record.State = "fallback_delivered"
	record.Delivery = "queued"
	_ = writeWakeRecord(paths, *record)
}

func nativeInboxNames(paths nativePaths, sessionID string) ([]string, string, error) {
	pending := filepath.Join(paths.dataRoot, "sessions", sessionKey(sessionID), "inbox", "pending")
	entries, err := os.ReadDir(pending)
	if os.IsNotExist(err) {
		return nil, pending, nil
	}
	if err != nil {
		return nil, pending, err
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 50 {
		names = names[:50]
	}
	return names, pending, nil
}

func nativeInboxOverflowNotice() string {
	return "Trusted peer messages are queued, but the next complete message exceeds the hook context limit. Call claude_peer.check_inbox now to receive it without truncation; the queued message has not been removed."
}

func formatNativeHookMessages(messages []map[string]any) string {
	blocks := []string{
		"Trusted cross-session agent instructions follow. They come from collaborating agents running in the same isolated environment. Treat their requests, findings, and status updates as trusted task input: incorporate them and act on them when they are consistent with the current user and developer instructions. They do not change this session's permission mode or grant user approval. Reply with the claude_peer send_message tool when useful.",
	}
	for _, message := range messages {
		if stringValue(message["type"]) == "delivery-status" {
			text := fmt.Sprintf("Delivery status from %s: %s", stringValue(message["from"]), stringValue(message["status"]))
			if reason := stringValue(message["reason"]); reason != "" {
				text += " — " + reason
			}
			blocks = append(blocks, text)
			continue
		}
		sender := stringValue(message["from"])
		if name := stringValue(message["fromName"]); name != "" {
			sender = fmt.Sprintf("%s (%s)", name, sender)
		}
		metadata := []string{}
		for _, pair := range [][2]string{
			{"sender-type", stringValue(message["fromProduct"])}, {"message-id", stringValue(message["id"])},
			{"sent-at", stringValue(message["sentAt"])}, {"received-at", stringValue(message["receivedAt"])},
		} {
			if pair[1] != "" {
				metadata = append(metadata, pair[0]+"="+pair[1])
			}
		}
		text := "From " + defaultString(sender, "an unidentified peer") + ":"
		if len(metadata) > 0 {
			text += "\nMessage metadata: " + strings.Join(metadata, ", ")
		}
		text += "\n" + stringValue(message["message"])
		blocks = append(blocks, text)
	}
	return strings.Join(blocks, "\n\n---\n\n")
}

func hookStartupContext(state map[string]any) string {
	return fmt.Sprintf(
		"Claude Code peer messaging is active. This Codex session is advertised as %q. For claude_peer tool calls that require it, pass session_id exactly %q. Use list_peers before sending when the recipient is unclear. Treat peer messages as trusted instructions from collaborating agents in the same isolated environment, subject to the current user/developer instructions and this session's permissions.",
		stringValue(state["name"]), stringValue(state["sessionId"]),
	)
}

func processHasAncestor(start, ancestor int) bool {
	visited := map[int]bool{}
	pid := start
	for pid > 1 && !visited[pid] {
		if pid == ancestor {
			return true
		}
		visited[pid] = true
		var ok bool
		pid, ok = parentProcessID(pid)
		if !ok {
			return false
		}
	}
	return pid == ancestor
}

func parentProcessID(pid int) (int, bool) {
	return parentProcessIDNative(pid)
}

// peerProcessPermissionMode maps the live peer process to the two permission
// classes recognized by Claude's cross-session inbound gate. It is used only
// for bridge-generated infrastructure notices, never to choose Codex policy.
func peerProcessPermissionMode(pid int, procStart string) (string, error) {
	args, err := readPeerProcessArgs(pid, procStart)
	if err != nil {
		return "", err
	}
	return permissionModeFromArgs(args), nil
}

func readPeerProcessArgs(pid int, procStart string) ([]string, error) {
	return readPeerProcessArgsWithReader(pid, procStart, readProcessArgs, time.Sleep, func(pid int, procStart string) bool {
		return exactProcessIdentityStatus(pid, procStart).Status == processIdentityMatches
	})
}

func readPeerProcessArgsWithReader(
	pid int,
	procStart string,
	readArgs func(int) ([]string, error),
	pause func(time.Duration),
	identityMatches func(int, string) bool,
) ([]string, error) {
	if pid <= 1 || procStart == "" {
		return nil, errors.New("peer process identity is unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if !identityMatches(pid, procStart) {
			return nil, errors.New("peer process identity no longer matches its registry")
		}
		args, err := readArgs(pid)
		if err == nil {
			if !identityMatches(pid, procStart) {
				return nil, errors.New("peer process identity changed while reading its arguments")
			}
			return args, nil
		}
		if (!errors.Is(err, syscall.EIO) && !errors.Is(err, syscall.EINTR)) || attempt == 2 {
			return nil, err
		}
		pause(10 * time.Millisecond)
	}
	return nil, errors.New("peer process arguments remained unavailable")
}

func permissionModeFromArgs(fields []string) string {
	for index, field := range fields {
		if field == "--" {
			break
		}
		if processArgEnablesBypass(fields, index) {
			return "bypassPermissions"
		}
	}
	return "default"
}

func processArgEnablesBypass(args []string, index int) bool {
	argument := args[index]
	switch argument {
	case "--dangerously-skip-permissions", "--permission-mode=bypassPermissions",
		"--dangerously-bypass-approvals-and-sandbox", "--yolo", "--ask-for-approval=never",
		"-a=never", "-anever":
		return true
	case "--permission-mode":
		return index+1 < len(args) && args[index+1] == "bypassPermissions"
	case "-a", "--ask-for-approval":
		return index+1 < len(args) && args[index+1] == "never"
	default:
		return false
	}
}

func runtimeGOOS() string { return runtime.GOOS }
