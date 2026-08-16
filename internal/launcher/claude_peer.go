package launcher

import (
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

	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const claudePeerReadyTimeout = 30 * time.Second

type claudePeerPlan struct {
	sessionID     string
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

// RunClaudePeer launches one native Claude session in a private registry and
// registers its real native socket with the host agent. Bare `claude` remains
// the communication opt-out and is never modified by this launcher.
func RunClaudePeer(args []string) error {
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
	status, err := federator.ReadAgentStatus(agentRuntimeDir())
	if err != nil {
		return fmt.Errorf("host agent is required for claude-peer; bare claude remains available: %w", err)
	}
	privateRoot := claudePeerPrivateRoot(status.HostID, plan.sessionID)
	profileLock, err := acquireClaudePeerProfileLock(privateRoot)
	if err != nil {
		return err
	}
	defer releaseClaudePeerProfileLock(profileLock)
	resolved, err := resolvePeerLaunchContext(
		plan.sessionID, "claude", plan.context, plan.alwaysApprove, plan.yoloSpecified,
	)
	if err != nil {
		return fmt.Errorf("resolve Agent Sessions peer preferences: %w", err)
	}
	if resolved.Preference.AlwaysApprove && !plan.alwaysApprove {
		plan.alwaysApprove = true
		plan.args = append(plan.args, "--dangerously-skip-permissions")
	} else if !resolved.Preference.AlwaysApprove && !claudePeerHasPermissionMode(plan.args) {
		// The durable default is an effective runtime decision, not merely an
		// argv omission: a user's Claude settings may otherwise enable bypass.
		plan.args = append(plan.args, "--permission-mode", "default")
	}
	publicRoot := claudePublicConfigRoot()
	if err := prepareClaudePeerProfile(privateRoot, publicRoot); err != nil {
		return err
	}
	if err := prepareClaudePeerAttachment(privateRoot, plan.sessionID); err != nil {
		return err
	}
	if err := projectAgentServiceRecord(privateRoot); err != nil {
		return fmt.Errorf("project host agent into Claude registry: %w", err)
	}
	environment := claudePeerEnvironment(os.Environ(), privateRoot, publicRoot, plan.sessionID)
	command := exec.Command(claude, append(plan.args, "--settings", `{"crossSessionInbound":"accept"}`)...) //nolint:gosec // executable and argv are direct native CLI values.
	command.Env, command.Stdin, command.Stdout, command.Stderr = environment, os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	return superviseClaudePeer(command, privateRoot, plan, resolved.Preference.AlwaysApprove)
}

//nolint:gocyclo // CLI parsing preserves native Claude flags while extracting the shared peer layer.
func parseClaudePeerArgs(args []string) (claudePeerPlan, error) {
	contextArgs, context, err := extractPeerLaunchContext(args, claudeOptionConsumesNext)
	if err != nil {
		return claudePeerPlan{}, err
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
	resume := false
	for index := 0; index < len(forwarded); index++ {
		argument := forwarded[index]
		switch {
		case argument == "--session-id":
			if index+1 >= len(forwarded) {
				return claudePeerPlan{}, usageError("--session-id requires a value")
			}
			plan.sessionID = forwarded[index+1]
			index++
		case strings.HasPrefix(argument, "--session-id="):
			plan.sessionID = strings.TrimPrefix(argument, "--session-id=")
		case argument == "--resume" || argument == "-r":
			if index+1 >= len(forwarded) {
				return claudePeerPlan{}, usageError(argument + " requires an exact session UUID")
			}
			plan.sessionID, resume = forwarded[index+1], true
			index++
		case strings.HasPrefix(argument, "--resume="):
			plan.sessionID, resume = strings.TrimPrefix(argument, "--resume="), true
		case argument == "--dangerously-skip-permissions":
			plan.alwaysApprove, plan.yoloSpecified = true, true
		case argument == "--permission-mode" && index+1 < len(forwarded):
			plan.alwaysApprove = forwarded[index+1] == "bypassPermissions"
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
	if plan.sessionID == "" {
		plan.sessionID, err = newClaudePeerSessionID()
		if err != nil {
			return claudePeerPlan{}, err
		}
		plan.args = append(plan.args, "--session-id", plan.sessionID)
	} else if !threadIDPattern.MatchString(plan.sessionID) {
		return claudePeerPlan{}, usageError("claude-peer requires an exact session UUID")
	}
	if resume && peerName != "" {
		plan.args = append(plan.args, "--name", peerName)
	} else if !resume && peerName != "" {
		plan.args = append(plan.args, "--name", peerName)
	}
	return plan, nil
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

func claudeExecutable() (string, error) {
	if path := strings.TrimSpace(os.Getenv("CLAUDE_PEER_CLAUDE_BIN")); path != "" {
		return path, nil
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", &ExitError{Code: 127, Err: errors.New("claude was not found on PATH")}
	}
	return path, nil
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

func claudePeerPrivateRoot(hostID, sessionID string) string {
	return federator.ClaudePeerPrivateRoot(hostID, sessionID)
}

func acquireClaudePeerProfileLock(privateRoot string) (*os.File, error) {
	lockPath := federator.ClaudePeerLifecycleLockPath(privateRoot)
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

func claudePublicConfigRoot() string {
	if root := strings.TrimSpace(os.Getenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")); root != "" {
		return root
	}
	return federator.DefaultClaudeConfigDir()
}

func prepareClaudePeerProfile(privateRoot, publicRoot string) error {
	if err := os.MkdirAll(filepath.Join(privateRoot, "sessions"), 0o700); err != nil {
		return fmt.Errorf("create private Claude registry: %w", err)
	}
	for _, name := range []string{"settings.json", "settings.local.json", "CLAUDE.md"} {
		body, err := os.ReadFile(filepath.Join(publicRoot, name)) //nolint:gosec // configured local Claude profile.
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Claude profile %s: %w", name, err)
		}
		if err := writeLauncherFileAtomic(filepath.Join(privateRoot, name), body, 0o600); err != nil {
			return err
		}
	}
	for _, name := range []string{"plugins", "skills", "commands", "agents"} {
		target := filepath.Join(publicRoot, name)
		if info, err := os.Stat(target); err != nil || !info.IsDir() {
			continue
		}
		link := filepath.Join(privateRoot, name)
		if current, err := os.Readlink(link); err == nil && filepath.Clean(current) == filepath.Clean(target) {
			continue
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("replace private Claude profile link %s: %w", name, err)
		}
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("link Claude profile %s: %w", name, err)
		}
	}
	return nil
}

// prepareClaudePeerAttachment runs while the deterministic profile lock is
// held. It prevents a resume from starting beside an orphaned native adapter
// and retires only exact, provably stale rows from an earlier attachment.
func prepareClaudePeerAttachment(privateRoot, sessionID string) error {
	directory := filepath.Join(privateRoot, "sessions")
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
		body, readErr := os.ReadFile(path) //nolint:gosec // locked deterministic private profile.
		if readErr != nil {
			return readErr
		}
		var marker struct {
			AgentService bool `json:"agentService"`
		}
		if json.Unmarshal(body, &marker) != nil {
			return fmt.Errorf("invalid native Claude record %s", path)
		}
		if marker.AgentService {
			continue
		}
		row, rowErr := parseClaudeNativePeerRecordForCleanup(body, pid, sessionID)
		if rowErr != nil {
			return fmt.Errorf("inspect prior Claude attachment: %w", rowErr)
		}
		if procinfo.Read(pid).Status != procinfo.Absent {
			return errors.New("this Claude peer session still has a live native attachment")
		}
		if err := cleanupClaudePeerNativeArtifacts(privateRoot, row, row.ProcStart, sessionID); err != nil {
			return err
		}
	}
	return nil
}

func claudePeerEnvironment(environment []string, privateRoot, publicRoot, sessionID string) []string {
	values := map[string]string{
		"CLAUDE_CONFIG_DIR": privateRoot, "CLAUDE_SECURESTORAGE_CONFIG_DIR": publicRoot,
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1", agentRuntimeDirEnv: agentRuntimeDir(),
		peerSessionIDEnv: sessionID, peerProductEnv: "claude",
	}
	for key, value := range values {
		environment = replaceLaneEnvironment(environment, key, value)
	}
	return environment
}

//nolint:gocyclo // Supervision combines native publication, agent registration, refresh, and cleanup.
func superviseClaudePeer(command *exec.Cmd, privateRoot string, plan claudePeerPlan, durableYolo bool) (resultErr error) {
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
	deadline := time.Now().Add(claudePeerReadyTimeout)
	var registration federator.PeerRegistration
	var nativeRow claudeNativePeerRecord
	defer func() {
		if nativeRow.PID <= 1 {
			nativeRow.PID = childPID
		}
		resultErr = errors.Join(resultErr, cleanupClaudePeerNativeArtifacts(
			privateRoot, nativeRow, childStart, plan.sessionID,
		))
	}()
	for registration.SessionID == "" {
		select {
		case waitErr := <-done:
			return waitErr
		case <-time.After(100 * time.Millisecond):
		}
		row, err := readClaudeNativePeerRecord(privateRoot, childPID, childStart)
		if err != nil {
			if time.Now().After(deadline) {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("claude-peer did not publish a native messaging socket: %w", err)
			}
			continue
		}
		if row.SessionID != plan.sessionID {
			_ = command.Process.Kill()
			<-done
			return errors.New("Claude published a different session ID than the requested stable session") //nolint:staticcheck // Claude is a product name.
		}
		actualYolo := effectiveClaudePeerYolo(row.PermissionMode, durableYolo)
		if plan.yoloSpecified && actualYolo != plan.alwaysApprove {
			_ = command.Process.Kill()
			<-done
			return errors.New("Claude published a permission mode that disagrees with the explicit claude-peer launch policy") //nolint:staticcheck // Claude is a product name.
		}
		if !plan.yoloSpecified && actualYolo != durableYolo {
			if _, err := resolvePeerLaunchContext(plan.sessionID, "claude", plan.context, actualYolo, true); err != nil {
				_ = command.Process.Kill()
				<-done
				return err
			}
			durableYolo = actualYolo
		}
		registration = claudePeerRegistration(row, plan, actualYolo, childPID, childStart)
		registration.LifecyclePID = os.Getpid()
		registration.LifecycleProcStart = federator.ProcessStart(os.Getpid())
		registration.PrivateConfigRoot = privateRoot
		nativeRow = row
		if registration.LifecycleProcStart == "" {
			_ = command.Process.Kill()
			<-done
			return errors.New("cannot corroborate claude-peer supervisor identity")
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
			_ = projectAgentServiceRecord(privateRoot)
			row, rowErr := readClaudeNativePeerRecord(privateRoot, childPID, childStart)
			if rowErr != nil || row.SessionID != plan.sessionID {
				continue
			}
			actualYolo := effectiveClaudePeerYolo(row.PermissionMode, durableYolo)
			if actualYolo != durableYolo {
				if resolved, resolveErr := resolvePeerLaunchContext(plan.sessionID, "claude", plan.context, actualYolo, true); resolveErr == nil {
					durableYolo = resolved.Preference.AlwaysApprove
				} else {
					continue
				}
			}
			registration = claudePeerRegistration(row, plan, durableYolo, childPID, childStart)
			registration.LifecyclePID = os.Getpid()
			registration.LifecycleProcStart = federator.ProcessStart(os.Getpid())
			registration.PrivateConfigRoot = privateRoot
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
	return federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: row.SessionID, Product: "claude",
		Name: defaultClaudePeerName(plan.peerName, row), Status: defaultClaudePeerStatus(row.Status),
		PermissionMode: permissionMode, Cwd: row.Cwd, PID: pid, ProcStart: procStart,
		Socket: row.MessagingSocketPath, StartedAt: row.StartedAt,
	}
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

func readClaudeNativePeerRecord(privateRoot string, pid int, expectedStart string) (claudeNativePeerRecord, error) {
	body, err := os.ReadFile(filepath.Join(privateRoot, "sessions", strconv.Itoa(pid)+".json")) //nolint:gosec // exact child PID and private profile.
	if err != nil {
		return claudeNativePeerRecord{}, err
	}
	var row claudeNativePeerRecord
	if json.Unmarshal(body, &row) != nil || row.PID != pid || row.SessionID == "" || row.MessagingSocketPath == "" ||
		row.ProcStart == "" || row.ProcStart != expectedStart || federator.ProcessStart(pid) != expectedStart ||
		row.Entrypoint != "cli" || row.Kind != "interactive" {
		return claudeNativePeerRecord{}, errors.New("invalid native Claude session record")
	}
	switch row.PermissionMode {
	case "", "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions":
	default:
		return claudeNativePeerRecord{}, errors.New("native Claude session published an unknown permission mode")
	}
	if filepath.Base(row.MessagingSocketPath) != strconv.Itoa(pid)+".sock" {
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
	privateRoot string,
	row claudeNativePeerRecord,
	expectedStart string,
	expectedSessionID string,
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
	recordPath := filepath.Join(privateRoot, "sessions", strconv.Itoa(row.PID)+".json")
	current, err := os.ReadFile(recordPath) //nolint:gosec // exact private child record.
	if err == nil {
		candidate, parseErr := parseClaudeNativePeerRecordForCleanup(current, pid, expectedSessionID)
		if parseErr != nil || candidate.ProcStart != expectedStart {
			return errors.New("native Claude record changed before cleanup")
		}
		row = candidate
		if procinfo.Read(pid).Status != procinfo.Absent {
			return errors.New("native Claude PID reappeared before record cleanup")
		}
		if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := removeClaudePeerKeySidecars(filepath.Join(privateRoot, "sessions"), row.PID); err != nil {
		return err
	}
	if row.MessagingSocketPath != "" && filepath.Base(row.MessagingSocketPath) == strconv.Itoa(row.PID)+".sock" {
		if info, statErr := os.Lstat(row.MessagingSocketPath); statErr == nil && info.Mode()&os.ModeSocket != 0 &&
			procinfo.Read(row.PID).Status == procinfo.Absent {
			if err := os.Remove(row.MessagingSocketPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
	}
	return nil
}

func parseClaudeNativePeerRecordForCleanup(body []byte, pid int, sessionID string) (claudeNativePeerRecord, error) {
	var row claudeNativePeerRecord
	if json.Unmarshal(body, &row) != nil || row.PID != pid || row.SessionID != sessionID || row.ProcStart == "" ||
		row.MessagingSocketPath == "" || filepath.Base(row.MessagingSocketPath) != strconv.Itoa(pid)+".sock" {
		return claudeNativePeerRecord{}, errors.New("invalid native Claude cleanup record")
	}
	return row, nil
}

func removeClaudePeerKeySidecars(directory string, pid int) error {
	prefix := strconv.Itoa(pid) + "."
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".key") {
			continue
		}
		digest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".key")
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || procinfo.Read(pid).Status != procinfo.Absent {
			continue
		}
		path := filepath.Join(directory, name)
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func projectAgentServiceRecord(privateRoot string) error {
	body, err := federator.AgentServiceRecord(agentRuntimeDir())
	if err != nil {
		return err
	}
	var record struct {
		PID          int  `json:"pid"`
		AgentService bool `json:"agentService"`
	}
	if json.Unmarshal(body, &record) != nil || !record.AgentService || record.PID <= 1 {
		return errors.New("host agent returned an invalid Claude service record")
	}
	directory := filepath.Join(privateRoot, "sessions")
	entries, _ := os.ReadDir(directory)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" || entry.Name() == strconv.Itoa(record.PID)+".json" {
			continue
		}
		old, readErr := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // private directory entry.
		if readErr == nil {
			var marker struct {
				AgentService bool `json:"agentService"`
			}
			if json.Unmarshal(old, &marker) == nil && marker.AgentService {
				_ = os.Remove(filepath.Join(directory, entry.Name()))
			}
		}
	}
	return writeLauncherFileAtomic(filepath.Join(directory, strconv.Itoa(record.PID)+".json"), body, 0o644)
}

func defaultClaudePeerName(explicit string, row claudeNativePeerRecord) string {
	if strings.TrimSpace(row.Name) != "" {
		return row.Name
	}
	if strings.TrimSpace(explicit) != "" {
		return explicit
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
