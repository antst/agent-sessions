package launcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const claudePeerReadyTimeout = 30 * time.Second

type claudePeerPlan struct {
	sessionID     string
	attachmentID  string
	resumeTarget  string
	resume        bool
	peerName      string
	context       peerLaunchContext
	args          []string
	informational bool
	alwaysApprove bool
	yoloSpecified bool
}

type claudeNativePeerRecord struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd"`
	Name                string `json:"name"`
	PermissionMode      string `json:"permissionMode"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	StartedAt           int64  `json:"startedAt"`
	ProcStart           string `json:"procStart"`
	Entrypoint          string `json:"entrypoint"`
	Kind                string `json:"kind"`
	Status              string `json:"status"`
}

// RunClaudePeer launches one native Claude session in the host agent's shared
// Claude profile and registers its real native socket. Bare `claude` remains
// the Agent Sessions opt-out and host settings are never modified.
func RunClaudePeer(args []string) error {
	return runClaudePeerWithDaemon(args, productionDaemonPeerDependencies())
}

func runClaudePeerWithDaemon(args []string, dependencies daemonPeerDependencies) error {
	plan, err := parseClaudePeerArgs(args)
	if err != nil {
		return err
	}
	if plan.informational {
		claude, executableErr := claudeExecutable()
		if executableErr != nil {
			return executableErr
		}
		return dependencies.exec(claude, plan.args, nil)
	}
	if dependencies.prepare == nil {
		return errors.New("claude peer daemon client is unavailable")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve Claude working directory: %w", err)
	}
	source, err := claudeprofile.CurrentSource()
	if err != nil {
		return err
	}
	permissionMode := "default"
	if plan.alwaysApprove {
		permissionMode = "bypassPermissions"
	}
	selector := plan.sessionID
	mode := "fresh"
	if plan.resume {
		mode, selector = "resume", plan.resumeTarget
	}
	prepared, err := dependencies.prepare(context.Background(), daemon.AttachmentPrepareRequest{
		Product: "claude", Kind: "interactive",
		ProfileIdentity: map[string]any{
			"profile": source.ConfigRoot, "config_env_set": source.ConfigEnvSet,
			"config_env_value": source.ConfigEnvValue, "secure_env_set": source.SecureEnvSet,
			"secure_config": source.SecureConfig,
		},
		Cwd: cwd, Name: plan.peerName, NameSource: "launch",
		Groups: append([]string(nil), plan.context.groups...), PermissionMode: permissionMode,
		Intent: daemon.InteractiveLaunchIntent{
			Mode: mode, Selector: selector, SelectorIsName: selector != "" && !threadIDPattern.MatchString(selector),
			NativeArguments: append([]string(nil), plan.args...), PermissionExplicit: plan.yoloSpecified,
		},
	})
	if err != nil {
		return fmt.Errorf("prepare Claude attachment: %w", err)
	}
	return executeDaemonPreparedPeer(context.Background(), "claude", prepared, dependencies)
}

//nolint:gocyclo // Legacy launch remains only as a migration reference until adapter extraction deletes it.
func runLegacyClaudePeer(args []string) error {
	plan, err := parseClaudePeerArgs(args)
	if err != nil {
		return err
	}
	claude, err := claudeExecutable()
	if err != nil {
		return err
	}
	if plan.informational {
		return Exec(claude, plan.args, nil)
	}
	status, err := claudePeerAgentStatus()
	if err != nil {
		return err
	}
	sharedRoot := filepath.Dir(status.RegistryDir)
	source, err := claudeprofile.CurrentSource()
	if err != nil {
		return err
	}
	if err := requireClaudePeerProfileMatch(source, status); err != nil {
		return err
	}
	lateBoundSelection := plan.sessionID == ""
	lifecycleRoot := claudePeerLifecycleRoot(status.StateDir, status.HostID, plan.attachmentID)
	managedSocket := federator.ClaudePeerMessagingSocketPath(status.RuntimeDir, plan.attachmentID)
	if err := federator.ValidateClaudePeerMessagingSocketPath(managedSocket); err != nil {
		return err
	}
	if _, err := os.Lstat(managedSocket); err == nil || !os.IsNotExist(err) {
		return errors.New("managed Claude messaging socket path already exists")
	}
	profileLock, err := acquireClaudePeerProfileLock(lifecycleRoot)
	if err != nil {
		return err
	}
	defer releaseClaudePeerProfileLock(profileLock)
	if err := prepareClaudePeerAttachment(sharedRoot, plan.attachmentID); err != nil {
		return err
	}
	// Native Claude binds its messaging socket before publishing the PID row.
	// Force an agent-owned exact path so crash cleanup does not have to infer a
	// transport path from an absent row. This final managed occurrence also
	// overrides a caller-provided native socket option without touching prompts
	// after `--`.
	plan.args = insertClaudeManagedArgs(plan.args, "--messaging-socket-path", managedSocket)
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	resolved, preferenceRequest, err := previewPeerLaunchContext(
		plan.attachmentID, "claude", plan.context, plan.alwaysApprove, plan.yoloSpecified,
	)
	if err != nil {
		return errors.Join(fmt.Errorf("preview Agent Sessions peer preferences: %w", err), removeClaudePeerLaunchSettings(settingsPath))
	}
	if resolved.Preference.AlwaysApprove && !plan.alwaysApprove {
		plan.alwaysApprove = true
		plan.args = insertClaudeManagedArgs(plan.args, "--dangerously-skip-permissions")
	} else if !resolved.Preference.AlwaysApprove && !claudePeerHasPermissionMode(plan.args) {
		// The durable default is an effective runtime decision, not merely an
		// argv omission: a user's Claude settings may otherwise enable bypass.
		plan.args = insertClaudeManagedArgs(plan.args, "--permission-mode", "default")
	}
	var settingsBody []byte
	plan.args, settingsBody, err = planClaudePeerLaunchSettings(
		plan.args, lifecycleRoot, !resolved.Preference.AlwaysApprove,
	)
	if err != nil {
		return errors.Join(err, removeClaudePeerLaunchSettings(settingsPath))
	}
	environment := claudePeerEnvironment(os.Environ(), sharedRoot, source, plan.attachmentID)
	gateReader, gateWriter, err := os.Pipe()
	if err != nil {
		return errors.Join(err, removeClaudePeerLaunchSettings(settingsPath))
	}
	defer func() {
		_ = gateReader.Close()
		_ = gateWriter.Close()
	}()
	gateArgs := make([]string, 0, 4+len(plan.args))
	gateArgs = append(gateArgs, "-c", `IFS= read -r gate <&3 || exit 125; test "$gate" = agent-sessions-go || exit 125; exec 3<&-; exec "$@"`, "claude-peer-gate", claude)
	gateArgs = append(gateArgs, plan.args...)
	command := exec.Command("/bin/sh", gateArgs...) //nolint:gosec // fixed gate script execs the native executable with an argv vector.
	command.Env, command.Stdin, command.Stdout, command.Stderr = environment, os.Stdin, os.Stdout, os.Stderr
	command.ExtraFiles = []*os.File{gateReader}
	if err := command.Start(); err != nil {
		return errors.Join(err, removeClaudePeerLaunchSettings(settingsPath))
	}
	_ = gateReader.Close()
	adapterStart, err := waitClaudePeerProcessStart(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.Join(err, removeClaudePeerLaunchSettings(settingsPath))
	}
	adapterStrongStart := procinfo.Read(command.Process.Pid).StrongStart
	if adapterStrongStart == "" {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.Join(errors.New("capture gated Claude adapter strong process identity"), removeClaudePeerLaunchSettings(settingsPath))
	}
	keyBaseline, err := federator.ClaudePeerKeySidecars(sharedRoot, command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.Join(fmt.Errorf("snapshot gated Claude peer keys: %w", err), removeClaudePeerLaunchSettings(settingsPath))
	}
	preparation := federator.PeerRegistration{
		Version: federation.GroupProtocolVersion, SessionID: plan.attachmentID, Product: "claude", Name: plan.peerName,
		PID: command.Process.Pid, ProcStart: adapterStart,
		LifecyclePID: os.Getpid(), LifecycleProcStart: federator.ProcessStart(os.Getpid()),
		LifecycleRoot: lifecycleRoot, ClaudeConfigRoot: sharedRoot,
		ClaudeKeyBaseline: keyBaseline, ClaudeKeyBaselineSet: true,
		ClaudeSocketPath: managedSocket, ClaudeSocketPathSet: true,
	}
	if lateBoundSelection {
		preparation.AttachmentID = plan.attachmentID
		err = federator.PrepareClaudePeerSelection(agentRuntimeDir(), preparation)
	} else {
		var prepared federator.ResolvedPreferences
		prepared, err = federator.PreparePeerLaunch(
			agentRuntimeDir(), preparation, preferenceRequest, resolved.Preference,
		)
		if err == nil {
			resolved = prepared
		}
	}
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.Join(err, removeClaudePeerLaunchSettings(settingsPath))
	}
	if err := writeClaudePeerLaunchSettings(settingsPath, settingsBody); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		settingsCleanupErr := removeClaudePeerLaunchSettings(settingsPath)
		var cancelErr error
		if settingsCleanupErr == nil {
			cancelErr = federator.CancelPeerPreparation(agentRuntimeDir(), preparation)
		}
		return errors.Join(err, settingsCleanupErr, cancelErr)
	}
	if _, err := gateWriter.Write([]byte("agent-sessions-go\n")); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		settingsCleanupErr := removeClaudePeerLaunchSettings(settingsPath)
		var cancelErr error
		if settingsCleanupErr == nil {
			cancelErr = federator.CancelPeerPreparation(agentRuntimeDir(), preparation)
		}
		return errors.Join(err, settingsCleanupErr, cancelErr)
	}
	_ = gateWriter.Close()
	var cleanupErr error
	runErr := superviseClaudePeer(
		command, lifecycleRoot, sharedRoot, managedSocket, keyBaseline, adapterStrongStart,
		plan, resolved.Preference.AlwaysApprove, &cleanupErr,
	)
	settingsCleanupErr := removeClaudePeerLaunchSettings(settingsPath)
	var cancelErr error
	if cleanupErr == nil && settingsCleanupErr == nil {
		cancelErr = federator.CancelPeerPreparation(agentRuntimeDir(), preparation)
	}
	return errors.Join(
		runErr, cleanupErr, settingsCleanupErr, cancelErr,
	)
}

func requireClaudePeerProfileMatch(source claudeprofile.Source, status federator.AgentStatus) error {
	if err := requireClaudePeerSharedRegistry(source.ConfigRoot, status.RegistryDir); err != nil {
		return err
	}
	if source.ConfigEnvSet != status.ClaudeConfigEnvSet ||
		(source.ConfigEnvSet && source.ConfigEnvValue != status.ClaudeConfigEnvValue) ||
		source.SecureEnvSet != status.ClaudeSecureEnvSet ||
		(source.SecureEnvSet && source.SecureConfig != status.ClaudeSecureConfig) {
		return errors.New("claude-peer native profile environment does not match the running host agent; restart the agent from the intended Claude environment")
	}
	return nil
}

func waitClaudePeerProcessStart(pid int) (string, error) {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if start := federator.ProcessStart(pid); start != "" {
			return start, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", errors.New("capture gated Claude adapter process identity")
}

func claudePeerAgentStatus() (federator.AgentStatus, error) {
	status, err := federator.ReadAgentStatus(agentRuntimeDir())
	if err != nil {
		return federator.AgentStatus{}, fmt.Errorf("host agent is required for claude-peer; bare claude remains available: %w", err)
	}
	return status, nil
}

//nolint:gocyclo // CLI parsing preserves native Claude flags while extracting the shared peer layer.
func parseClaudePeerArgs(args []string) (claudePeerPlan, error) {
	contextArgs, context, err := extractPeerLaunchContext(args, claudeOptionConsumesNext)
	if err != nil {
		return claudePeerPlan{}, err
	}
	// --yolo is a peer-level convenience shared with the other product
	// launchers. Translate it in place so native Claude receives its canonical
	// spelling and option order remains authoritative. Prompt text after `--`
	// is never interpreted as a wrapper option.
	for index, argument := range contextArgs {
		if argument == "--" {
			break
		}
		if argument == "--yolo" {
			contextArgs[index] = "--dangerously-skip-permissions"
		}
	}
	forwarded, peerName, err := extractPeerNameArgs(contextArgs)
	if err != nil {
		return claudePeerPlan{}, err
	}
	plan := claudePeerPlan{peerName: peerName, context: context, args: forwarded}
	for _, argument := range beforeDoubleDash(forwarded) {
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			plan.informational = true
			return plan, nil
		}
	}
	scanned := beforeDoubleDash(forwarded)
	for index := 0; index < len(scanned); index++ {
		argument := scanned[index]
		switch {
		case argument == "--bare":
			return claudePeerPlan{}, usageError("claude-peer requires native messaging; use bare claude to opt out")
		case argument == "--allow-dangerously-skip-permissions" ||
			strings.HasPrefix(argument, "--allow-dangerously-skip-permissions="):
			return claudePeerPlan{}, usageError("claude-peer cannot attest mutable in-session bypass; launch with --dangerously-skip-permissions instead")
		case argument == "--session-id":
			if index+1 >= len(scanned) {
				return claudePeerPlan{}, usageError("--session-id requires a value")
			}
			plan.sessionID = scanned[index+1]
			index++
		case strings.HasPrefix(argument, "--session-id="):
			plan.sessionID = strings.TrimPrefix(argument, "--session-id=")
		case argument == "--resume" || argument == "-r":
			if index+1 >= len(scanned) {
				return claudePeerPlan{}, usageError(argument + " requires a native Claude resume target")
			}
			plan.resumeTarget, plan.resume = scanned[index+1], true
			index++
		case strings.HasPrefix(argument, "--resume="):
			plan.resumeTarget, plan.resume = strings.TrimPrefix(argument, "--resume="), true
		case argument == "--dangerously-skip-permissions":
			plan.alwaysApprove, plan.yoloSpecified = true, true
		case argument == "--permission-mode" && index+1 < len(scanned):
			plan.alwaysApprove = scanned[index+1] == "bypassPermissions"
			plan.yoloSpecified = true
			index++
		case strings.HasPrefix(argument, "--permission-mode="):
			plan.alwaysApprove = strings.TrimPrefix(argument, "--permission-mode=") == "bypassPermissions"
			plan.yoloSpecified = true
		}
	}
	if context.forceNoYolo {
		if plan.alwaysApprove {
			return claudePeerPlan{}, usageError("--no-yolo conflicts with Claude bypass permissions")
		}
		plan.yoloSpecified = true
	}
	switch {
	case plan.resume:
		if strings.TrimSpace(plan.resumeTarget) == "" {
			return claudePeerPlan{}, usageError("--resume requires a native Claude resume target")
		}
		if threadIDPattern.MatchString(plan.resumeTarget) {
			plan.sessionID = plan.resumeTarget
			plan.attachmentID = plan.sessionID
		} else {
			plan.sessionID = ""
			plan.attachmentID, err = newClaudePeerSessionID()
			if err != nil {
				return claudePeerPlan{}, err
			}
		}
	case plan.sessionID == "":
		plan.sessionID, err = newClaudePeerSessionID()
		if err != nil {
			return claudePeerPlan{}, err
		}
		plan.attachmentID = plan.sessionID
		plan.args = insertClaudeManagedArgs(plan.args, "--session-id", plan.sessionID)
	case !threadIDPattern.MatchString(plan.sessionID):
		return claudePeerPlan{}, usageError("--session-id requires an exact session UUID")
	default:
		plan.attachmentID = plan.sessionID
	}
	if peerName != "" {
		plan.args = insertClaudeManagedArgs(plan.args, "--name", peerName)
	}
	// A detected Chrome extension can otherwise raise a first-run dialog before
	// native Claude publishes its messaging row. Managed peers must not stall on
	// an unrelated browser prompt, but an operator's explicit choice still wins.
	if !claudePeerHasChromeMode(plan.args) {
		plan.args = insertClaudeManagedArgs(plan.args, "--no-chrome")
	}
	return plan, nil
}

func insertClaudeManagedArgs(args []string, managed ...string) []string {
	insert := len(args)
	for index, argument := range args {
		if argument == "--" {
			insert = index
			break
		}
	}
	result := make([]string, 0, len(args)+len(managed))
	result = append(result, args[:insert]...)
	result = append(result, managed...)
	result = append(result, args[insert:]...)
	return result
}

func claudeOptionConsumesNext(option string) bool {
	name := strings.SplitN(option, "=", 2)[0]
	switch name {
	case "--add-dir", "--agent", "--agents", "--allowedTools", "--append-system-prompt", "--betas",
		"--debug-file", "--disallowedTools", "--effort", "--fallback-model", "--ide", "--input-format",
		"--json-schema", "--max-budget-usd", "--max-turns", "--mcp-config", "--model", "--name",
		"--output-format", "--permission-mode", "--permission-prompt-tool", "--plugin-dir", "--resume", "-r",
		"--session-id", "--settings", "--system-prompt", "--tools":
		return !strings.Contains(option, "=")
	default:
		return false
	}
}

func claudePeerHasPermissionMode(args []string) bool {
	for _, argument := range beforeDoubleDash(args) {
		if argument == "--permission-mode" || strings.HasPrefix(argument, "--permission-mode=") ||
			argument == "--dangerously-skip-permissions" {
			return true
		}
	}
	return false
}

func claudePeerHasChromeMode(args []string) bool {
	for _, argument := range beforeDoubleDash(args) {
		if argument == "--chrome" || argument == "--no-chrome" ||
			strings.HasPrefix(argument, "--chrome=") || strings.HasPrefix(argument, "--no-chrome=") {
			return true
		}
	}
	return false
}

func requireClaudePeerSharedRegistry(configRoot, registryDir string) error {
	want := filepath.Join(configRoot, "sessions")
	if !sameLauncherPath(want, registryDir) {
		return fmt.Errorf("claude-peer profile %s does not match host agent registry %s", configRoot, registryDir)
	}
	return nil
}

func sameLauncherPath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	leftResolved, leftErr := filepath.EvalSymlinks(leftAbsolute)
	rightResolved, rightErr := filepath.EvalSymlinks(rightAbsolute)
	if leftErr == nil && rightErr == nil {
		return filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
	}
	return filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

// prepareClaudePeerLaunchSettings merges the caller's effective settings with
// the constrained managed-session overrides. The host settings files are never written.
func prepareClaudePeerLaunchSettings(args []string, lifecycleRoot string) ([]string, error) {
	result, body, err := planClaudePeerLaunchSettings(args, lifecycleRoot, true)
	if err != nil {
		return nil, err
	}
	if err := writeClaudePeerLaunchSettings(filepath.Join(lifecycleRoot, "launch-settings.json"), body); err != nil {
		return nil, err
	}
	return result, nil
}

func planClaudePeerLaunchSettings(args []string, lifecycleRoot string, constrainPermissions bool) ([]string, []byte, error) {
	settings := map[string]json.RawMessage{}
	settingsValue := ""
	settingsSet := false
	without := make([]string, 0, len(args)+2)
	afterDoubleDash := false
	for len(args) > 0 {
		argument := args[0]
		args = args[1:]
		if argument == "--" {
			afterDoubleDash = true
			without = append(without, argument)
			continue
		}
		if afterDoubleDash {
			without = append(without, argument)
			continue
		}
		var value string
		switch {
		case argument == "--settings":
			if len(args) == 0 {
				return nil, nil, usageError("--settings requires a JSON object or file path")
			}
			value = args[0]
			args = args[1:]
		case strings.HasPrefix(argument, "--settings="):
			value = strings.TrimPrefix(argument, "--settings=")
		default:
			without = append(without, argument)
			continue
		}
		settingsValue, settingsSet = value, true // Claude treats the last singular --settings as effective.
	}
	if settingsSet {
		parsed, err := readClaudePeerSettings(settingsValue)
		if err != nil {
			return nil, nil, err
		}
		settings = parsed
	}
	settings["crossSessionInbound"] = json.RawMessage(`"accept"`)
	if constrainPermissions {
		if err := constrainClaudePeerPermissions(settings); err != nil {
			return nil, nil, err
		}
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(lifecycleRoot, "launch-settings.json")
	insert := len(without)
	for index, argument := range without {
		if argument == "--" {
			insert = index
			break
		}
	}
	result := make([]string, 0, len(without)+2)
	result = append(result, without[:insert]...)
	result = append(result, "--settings", path)
	result = append(result, without[insert:]...)
	return result, append(body, '\n'), nil
}

func constrainClaudePeerPermissions(settings map[string]json.RawMessage) error {
	permissions := map[string]json.RawMessage{}
	if raw, exists := settings["permissions"]; exists {
		if json.Unmarshal(raw, &permissions) != nil || permissions == nil {
			return errors.New("claude --settings permissions must contain a JSON object")
		}
	}
	permissions["disableBypassPermissionsMode"] = json.RawMessage(`"disable"`)
	body, err := json.Marshal(permissions)
	if err != nil {
		return err
	}
	settings["permissions"] = body
	return nil
}

func writeClaudePeerLaunchSettings(path string, body []byte) error {
	if err := writeLauncherFileAtomic(path, body, 0o600); err != nil {
		return fmt.Errorf("write managed Claude launch settings: %w", err)
	}
	return nil
}

func readClaudePeerSettings(value string) (map[string]json.RawMessage, error) {
	body := []byte(value)
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		var err error
		body, err = os.ReadFile(value) //nolint:gosec // explicit native --settings path supplied by the operator.
		if err != nil {
			return nil, fmt.Errorf("read Claude --settings: %w", err)
		}
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(body, &settings); err != nil || settings == nil {
		return nil, errors.New("claude --settings must contain a JSON object")
	}
	return settings, nil
}

func removeClaudePeerLaunchSettings(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("managed Claude launch settings changed type")
	}
	return os.Remove(path)
}

func claudeExecutable() (string, error) {
	return productExecutable("CLAUDE_PEER_CLAUDE_BIN", "claude")
}

func newClaudePeerSessionID() (string, error) {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", fmt.Errorf("generate Claude session ID: %w", err)
	}
	body[6] = body[6]&0x0f | 0x40
	body[8] = body[8]&0x3f | 0x80
	hexID := hex.EncodeToString(body)
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:], nil
}

func claudePeerLifecycleRoot(stateDir, hostID, sessionID string) string {
	if stateDir != "" {
		return federator.ClaudePeerLifecycleRootInState(stateDir, sessionID)
	}
	return federator.ClaudePeerLifecycleRoot(hostID, sessionID)
}

func acquireClaudePeerProfileLock(lifecycleRoot string) (*os.File, error) {
	lockPath := federator.ClaudePeerLifecycleLockPath(lifecycleRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // deterministic agent-owned session lock.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("this Claude peer session already has a live attachment")
	}
	return lock, nil
}

func releaseClaudePeerProfileLock(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

// prepareClaudePeerAttachment runs while the deterministic profile lock is
// held. In the shared registry it ignores unrelated native/service rows,
// rejects only a live attachment of the same UUID, and retires only an exact
// stale row from that UUID.
func prepareClaudePeerAttachment(configRoot, sessionID string) error {
	directory := filepath.Join(configRoot, "sessions")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		pidText := strings.TrimSuffix(name, ".json")
		pid, parseErr := strconv.Atoi(pidText)
		if parseErr != nil || pid <= 1 {
			continue
		}
		path := filepath.Join(directory, name)
		body, readErr := os.ReadFile(path) //nolint:gosec // locked exact row in the shared registry.
		if readErr != nil {
			return readErr
		}
		var marker struct {
			AgentService bool   `json:"agentService"`
			SessionID    string `json:"sessionId"`
		}
		if json.Unmarshal(body, &marker) != nil {
			continue
		}
		if marker.AgentService || marker.SessionID != sessionID {
			continue
		}
		row, rowErr := parseClaudeNativePeerRecordForCleanup(body, pid, sessionID, "")
		if rowErr != nil {
			return fmt.Errorf("inspect prior Claude attachment: %w", rowErr)
		}
		if procinfo.Read(pid).Status != procinfo.Absent {
			return errors.New("this Claude peer session still has a live native attachment")
		}
		if err := cleanupClaudePeerNativeArtifacts(configRoot, row, row.ProcStart, "", sessionID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func claudePeerEnvironment(environment []string, sharedRoot string, source claudeprofile.Source, sessionID string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		switch name {
		case "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET",
			"CLAUDE_CODE_ENTRYPOINT", "CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION",
			"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_HARBOR_KITE",
			"CLAUDE_PEER_CLAUDE_CONFIG_DIR":
		default:
			filtered = append(filtered, entry)
		}
	}
	environment = filtered
	values := map[string]string{
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR":        sharedRoot,
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1", agentRuntimeDirEnv: agentRuntimeDir(),
		"CLAUDE_CODE_HARBOR_KITE": "1",
		peerSessionIDEnv:          sessionID, peerProductEnv: "claude",
	}
	for key, value := range values {
		environment = envutil.Set(environment, key, value)
	}
	environment = applyClaudeProfileEnvironment(environment, source)
	return environment
}

func applyClaudeProfileEnvironment(environment []string, source claudeprofile.Source) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if name != "CLAUDE_CONFIG_DIR" && name != "CLAUDE_SECURESTORAGE_CONFIG_DIR" {
			result = append(result, entry)
		}
	}
	if source.ConfigEnvSet {
		result = append(result, "CLAUDE_CONFIG_DIR="+source.ConfigEnvValue)
	}
	if source.SecureEnvSet {
		result = append(result, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+source.SecureConfig)
	}
	return result
}

//nolint:gocyclo // Supervision combines native publication, agent registration, refresh, and cleanup.
func superviseClaudePeer(
	command *exec.Cmd,
	lifecycleRoot string,
	sharedRoot string,
	managedSocket string,
	keyBaseline []federator.ClaudeKeyBaselineEntry,
	adapterStrongStart string,
	plan claudePeerPlan,
	durableYolo bool,
	cleanupErr *error,
) (resultErr error) {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	childPID := command.Process.Pid
	childStart := federator.ProcessStart(childPID)
	if childStart == "" {
		_ = command.Process.Kill()
		<-done
		return errors.New("cannot corroborate native Claude process identity")
	}
	signals := make(chan os.Signal, 4)
	signalDone := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer func() {
		signal.Stop(signals)
		close(signalDone)
	}()
	go func() {
		for {
			select {
			case caught := <-signals:
				if federator.ProcessStart(childPID) == childStart {
					_ = command.Process.Signal(caught)
				}
			case <-signalDone:
				return
			}
		}
	}()
	deadline := time.Time{}
	if plan.sessionID != "" {
		deadline = time.Now().Add(claudePeerReadyTimeout)
	}
	var registration federator.PeerRegistration
	selectionPromoted := false
	var selectedPreference federation.SessionPreferences
	nativeRow := claudeNativePeerRecord{PID: childPID, MessagingSocketPath: managedSocket}
	var observedKeys []federator.ClaudeKeyBaselineEntry
	observeKeys := func() error {
		identity := procinfo.Read(childPID)
		if identity.Status != procinfo.Known || identity.Start != childStart || identity.StrongStart != adapterStrongStart {
			return errors.New("native Claude process identity changed before key observation")
		}
		keys, err := federator.ObserveClaudePeerNewKeySidecars(
			sharedRoot, childPID, childStart, adapterStrongStart, keyBaseline,
		)
		if err == nil {
			observedKeys = keys
		}
		return err
	}
	defer func() {
		if nativeRow.PID <= 1 {
			nativeRow.PID = childPID
		}
		expectedSessionID := plan.sessionID
		if nativeRow.SessionID != "" {
			expectedSessionID = nativeRow.SessionID
		}
		*cleanupErr = cleanupClaudePeerNativeArtifacts(
			sharedRoot, nativeRow, childStart, adapterStrongStart, expectedSessionID, keyBaseline, observedKeys,
		)
	}()
	for registration.SessionID == "" {
		select {
		case waitErr := <-done:
			return waitErr
		case <-time.After(100 * time.Millisecond):
		}
		if err := observeKeys(); err != nil {
			_ = command.Process.Kill()
			<-done
			return fmt.Errorf("observe native Claude publication key: %w", err)
		}
		row, err := readClaudeNativePeerRecord(sharedRoot, childPID, childStart, managedSocket)
		if err != nil {
			if !deadline.IsZero() && time.Now().After(deadline) {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("claude-peer did not publish a native messaging socket: %w", err)
			}
			continue
		}
		if deadline.IsZero() && plan.sessionID != "" {
			deadline = time.Now().Add(claudePeerReadyTimeout)
		}
		if plan.sessionID != "" && row.SessionID != plan.sessionID {
			_ = command.Process.Kill()
			<-done
			return errors.New("Claude published a different session ID than the requested stable session") //nolint:staticcheck // Claude is a product name.
		}
		if !threadIDPattern.MatchString(row.SessionID) {
			_ = command.Process.Kill()
			<-done
			return errors.New("Claude published an invalid native session UUID") //nolint:staticcheck // Claude is a product name.
		}
		nativeRow = row
		if plan.sessionID == "" && !selectionPromoted {
			title, selected := federator.ClaudeNativeSessionTitle(sharedRoot, row.SessionID)
			if !selected || !strings.EqualFold(strings.TrimSpace(title), strings.TrimSpace(plan.resumeTarget)) {
				// Native Claude first publishes a boot identity, then changes the
				// same PID/socket row after its resume selector resolves. Never
				// promote that transient identity; a duplicate-title picker may
				// remain here until the operator chooses the actual transcript.
				continue
			}
		}
		actualYolo := effectiveClaudePeerYolo(row.PermissionMode, durableYolo)
		if plan.sessionID == "" && !selectionPromoted {
			selected, _, previewErr := previewPeerLaunchContext(
				row.SessionID, "claude", plan.context, plan.alwaysApprove, plan.yoloSpecified,
			)
			if previewErr != nil {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("preview selected Claude session preferences: %w", previewErr)
			}
			if actualYolo != selected.Preference.AlwaysApprove {
				_ = command.Process.Kill()
				<-done
				return errors.New("Claude selected a session whose durable yolo preference differs from the native launch; pass --yolo or --no-yolo explicitly") //nolint:staticcheck // Claude is a product name.
			}
			selectedPreference = selected.Preference
		}
		if plan.yoloSpecified && actualYolo != plan.alwaysApprove {
			_ = command.Process.Kill()
			<-done
			return errors.New("Claude published a permission mode that disagrees with the explicit claude-peer launch policy") //nolint:staticcheck // Claude is a product name.
		}
		if !plan.yoloSpecified && actualYolo != durableYolo {
			_ = command.Process.Kill()
			<-done
			return errors.New("Claude published a permission mode that disagrees with the prepared durable launch policy") //nolint:staticcheck // Claude is a product name.
		}
		registration = claudePeerRegistration(row, plan, actualYolo, childPID, childStart)
		registration.LifecyclePID = os.Getpid()
		registration.LifecycleProcStart = federator.ProcessStart(os.Getpid())
		registration.LifecycleRoot = lifecycleRoot
		registration.ClaudeConfigRoot = sharedRoot
		registration.ClaudeKeyBaseline = append([]federator.ClaudeKeyBaselineEntry(nil), keyBaseline...)
		registration.ClaudeKeyBaselineSet = true
		registration.ClaudeSocketPath = managedSocket
		registration.ClaudeSocketPathSet = true
		if registration.LifecycleProcStart == "" {
			_ = command.Process.Kill()
			<-done
			return errors.New("cannot corroborate claude-peer supervisor identity")
		}
		if plan.sessionID == "" && !selectionPromoted {
			request := peerPreferenceRequest(
				row.SessionID, "claude", plan.context, plan.alwaysApprove, plan.yoloSpecified,
			)
			promoted, promoteErr := federator.PromoteClaudePeerSelection(
				agentRuntimeDir(), registration, request, selectedPreference,
			)
			if promoteErr != nil {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("adopt native Claude resume selection: %w", promoteErr)
			}
			durableYolo = promoted.Preference.AlwaysApprove
			selectionPromoted = true
		}
		if _, err := federator.RegisterPeer(agentRuntimeDir(), registration); err != nil {
			registration = federator.PeerRegistration{}
			if time.Now().After(deadline) {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("register claude-peer with host agent: %w", err)
			}
		}
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer func() { _ = federator.UnregisterPeer(agentRuntimeDir(), registration) }()
	for {
		select {
		case waitErr := <-done:
			if waitErr == nil {
				return nil
			}
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				return &ExitError{Code: exitErr.ExitCode(), Err: waitErr}
			}
			return waitErr
		case <-ticker.C:
			row, rowErr := readClaudeNativePeerRecord(sharedRoot, childPID, childStart, managedSocket)
			if rowErr != nil || row.SessionID != registration.SessionID {
				continue
			}
			if effectiveClaudePeerYolo(row.PermissionMode, durableYolo) && !durableYolo {
				_ = command.Process.Kill()
				<-done
				return errors.New("Claude entered bypass permissions outside the durable managed launch policy") //nolint:staticcheck // Claude is a product name.
			}
			registration = claudePeerRegistration(row, plan, durableYolo, childPID, childStart)
			registration.LifecyclePID = os.Getpid()
			registration.LifecycleProcStart = federator.ProcessStart(os.Getpid())
			registration.LifecycleRoot = lifecycleRoot
			registration.ClaudeConfigRoot = sharedRoot
			registration.ClaudeKeyBaseline = append([]federator.ClaudeKeyBaselineEntry(nil), keyBaseline...)
			registration.ClaudeKeyBaselineSet = true
			registration.ClaudeSocketPath = managedSocket
			registration.ClaudeSocketPathSet = true
			nativeRow = row
			_, _ = federator.RegisterPeer(agentRuntimeDir(), registration)
		}
	}
}

func effectiveClaudePeerYolo(permissionMode string, fallback bool) bool {
	if strings.TrimSpace(permissionMode) == "" {
		return fallback
	}
	return permissionMode == "bypassPermissions"
}

func claudePeerRegistration(row claudeNativePeerRecord, plan claudePeerPlan, yolo bool, pid int, procStart string) federator.PeerRegistration {
	permissionMode := "default"
	if yolo {
		permissionMode = "bypassPermissions"
	}
	registration := federator.PeerRegistration{
		Version: federation.GroupProtocolVersion, SessionID: row.SessionID, Product: "claude",
		Name: defaultClaudePeerName(plan, row), Status: defaultClaudePeerStatus(row.Status),
		PermissionMode: permissionMode, Cwd: row.Cwd, PID: pid, ProcStart: procStart,
		Socket: row.MessagingSocketPath, StartedAt: row.StartedAt,
	}
	if plan.sessionID == "" {
		registration.AttachmentID = plan.attachmentID
	}
	return registration
}

func defaultClaudePeerStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "busy", "active", "working":
		return "busy"
	case "waiting", "permission", "waiting_for_input":
		return "waiting"
	default:
		return "idle"
	}
}

//nolint:gocyclo // A native row must pass every independent process, mode, and socket attestation.
func readClaudeNativePeerRecord(configRoot string, pid int, expectedStart string, expectedSocket string) (claudeNativePeerRecord, error) {
	body, err := os.ReadFile(filepath.Join(configRoot, "sessions", strconv.Itoa(pid)+".json")) //nolint:gosec // exact child PID in the shared profile.
	if err != nil {
		return claudeNativePeerRecord{}, err
	}
	var row claudeNativePeerRecord
	if json.Unmarshal(body, &row) != nil || row.PID != pid || row.SessionID == "" || row.MessagingSocketPath == "" ||
		row.ProcStart == "" || row.ProcStart != expectedStart || federator.ProcessStart(pid) != expectedStart ||
		row.Entrypoint != "cli" || row.Kind != "interactive" ||
		expectedSocket != "" && row.MessagingSocketPath != expectedSocket {
		return claudeNativePeerRecord{}, errors.New("invalid native Claude session record")
	}
	switch row.PermissionMode {
	case "", "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions":
	default:
		return claudeNativePeerRecord{}, errors.New("native Claude session published an unknown permission mode")
	}
	if expectedSocket == "" && filepath.Base(row.MessagingSocketPath) != strconv.Itoa(pid)+".sock" {
		return claudeNativePeerRecord{}, errors.New("native Claude session socket is not PID-bound")
	}
	info, statErr := os.Lstat(row.MessagingSocketPath)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 {
		return claudeNativePeerRecord{}, errors.New("native Claude session socket is not live")
	}
	return row, nil
}

//nolint:gocyclo // Provisional and validated native artifacts are independently re-attested.
func cleanupClaudePeerNativeArtifacts(
	configRoot string,
	row claudeNativePeerRecord,
	expectedStart string,
	expectedStrongStart string,
	expectedSessionID string,
	keyBaseline []federator.ClaudeKeyBaselineEntry,
	observedKeys []federator.ClaudeKeyBaselineEntry,
) error {
	pid := row.PID
	if pid <= 1 {
		// Registration may never have completed. The child PID is recoverable
		// from expectedStart only at the call site, so callers pass a relaxed
		// row when publication was observed. With no row there is no artifact
		// path to remove.
		return nil
	}
	if procinfo.Read(pid).Status != procinfo.Absent {
		return errors.New("native Claude PID is not absent during cleanup")
	}
	recordPath := filepath.Join(configRoot, "sessions", strconv.Itoa(row.PID)+".json")
	recordPresent := false
	current, err := os.ReadFile(recordPath) //nolint:gosec // exact private child record.
	if err == nil {
		candidate, parseErr := parseClaudeNativePeerRecordForCleanup(
			current, pid, expectedSessionID, row.MessagingSocketPath,
		)
		if parseErr != nil || candidate.ProcStart != expectedStart {
			return errors.New("native Claude record changed before cleanup")
		}
		if row.MessagingSocketPath != "" && candidate.MessagingSocketPath != row.MessagingSocketPath {
			return errors.New("native Claude record changed its messaging socket before cleanup")
		}
		row = candidate
		recordPresent = true
	} else if !os.IsNotExist(err) {
		return err
	}
	keyPath := ""
	keyPresent := false
	if row.MessagingSocketPath != "" {
		if filepath.Base(row.MessagingSocketPath) != strconv.Itoa(row.PID)+".sock" && keyBaseline == nil {
			return errors.New("native Claude socket is not PID-bound")
		}
		keyName, keyErr := federator.ClaudeServiceKeyName(row.PID, row.MessagingSocketPath)
		if keyErr != nil {
			return errors.New("native Claude peer-token sidecar path is invalid")
		}
		keyPath = filepath.Join(configRoot, "sessions", keyName)
		if info, statErr := os.Lstat(keyPath); statErr == nil {
			if !info.Mode().IsRegular() {
				return errors.New("native Claude peer-token sidecar changed type")
			}
			keyPresent = true
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	socketPresent := false
	if row.MessagingSocketPath != "" {
		info, statErr := os.Lstat(row.MessagingSocketPath)
		if statErr == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return errors.New("native Claude socket path changed type")
			}
			socketPresent = true
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	// Keep the registry row until transport cleanup succeeds so a failed
	// attempt retains the exact socket identity for agent reconciliation.
	if socketPresent {
		if !recordPresent && keyBaseline != nil {
			if err := federator.ValidateClaudePeerObservedSocketKey(
				configRoot, pid, expectedStart, expectedStrongStart,
				row.MessagingSocketPath, keyBaseline, observedKeys,
			); err != nil {
				return err
			}
		}
		if procinfo.Read(row.PID).Status != procinfo.Absent {
			return errors.New("native Claude PID reappeared before socket cleanup")
		}
		if err := os.Remove(row.MessagingSocketPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if keyPresent && keyBaseline == nil {
		if procinfo.Read(row.PID).Status != procinfo.Absent {
			return errors.New("native Claude PID reappeared before key removal")
		}
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if keyBaseline != nil {
		if err := federator.CleanupClaudePeerNewKeySidecars(
			configRoot, pid, expectedStart, expectedStrongStart, keyBaseline, observedKeys, recordPresent,
		); err != nil {
			return err
		}
	}
	if recordPresent {
		if procinfo.Read(pid).Status != procinfo.Absent {
			return errors.New("native Claude PID reappeared before record cleanup")
		}
		if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func parseClaudeNativePeerRecordForCleanup(
	body []byte,
	pid int,
	sessionID string,
	expectedSocket string,
) (claudeNativePeerRecord, error) {
	var row claudeNativePeerRecord
	if json.Unmarshal(body, &row) != nil || row.PID != pid || row.SessionID != sessionID || row.ProcStart == "" {
		return claudeNativePeerRecord{}, errors.New("invalid native Claude cleanup record")
	}
	// Native Claude publishes its PID row after binding its messaging inbox. A
	// child may exit with either field absent, so cleanup accepts a provisional
	// row while still requiring exact PID/session/start identity. A socket must
	// be either the prepared managed path or the legacy PID-bound native path.
	if row.MessagingSocketPath != "" && row.MessagingSocketPath != expectedSocket &&
		filepath.Base(row.MessagingSocketPath) != strconv.Itoa(pid)+".sock" {
		return claudeNativePeerRecord{}, errors.New("invalid native Claude cleanup record")
	}
	return row, nil
}

func defaultClaudePeerName(plan claudePeerPlan, row claudeNativePeerRecord) string {
	if explicit := strings.TrimSpace(plan.peerName); explicit != "" {
		return explicit
	}
	if target := strings.TrimSpace(plan.resumeTarget); plan.resume && target != "" &&
		!threadIDPattern.MatchString(target) {
		return target
	}
	if native := strings.TrimSpace(row.Name); native != "" {
		return native
	}
	return "claude-" + row.SessionID[:8]
}

func writeLauncherFileAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp-" + strconv.Itoa(os.Getpid())
	// #nosec G703 -- caller supplies a profile-owned path.
	if err := os.WriteFile(temporary, body, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
