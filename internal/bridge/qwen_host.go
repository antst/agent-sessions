package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
)

const qwenHostAdmissionTimeout = 20 * time.Second

type qwenHostArguments struct {
	Qwen            string
	Version         string
	RuntimeDir      string
	AgentRuntimeDir string
	Registration    federator.PeerRegistration
	Native          []string
}

type qwenHostProcessExitError struct {
	Code int
	Err  error
}

func (e *qwenHostProcessExitError) Error() string { return e.Err.Error() }
func (e *qwenHostProcessExitError) Unwrap() error { return e.Err }

func runQwenHostCommand(args []string) int {
	config, err := parseQwenHostArguments(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions qwen-host: %v\n", err)
		return 2
	}
	if err := runQwenInteractiveHost(context.Background(), config); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions qwen-host: %v\n", err)
		var nativeExit *qwenHostProcessExitError
		if errors.As(err, &nativeExit) && nativeExit.Code > 0 && nativeExit.Code <= 255 {
			return nativeExit.Code
		}
		return 1
	}
	return 0
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func parseQwenHostArguments(args []string) (qwenHostArguments, error) {
	var result qwenHostArguments
	registrationJSON := ""
	for index := 0; index < len(args); index++ {
		if args[index] == "--" {
			result.Native = append([]string(nil), args[index+1:]...)
			break
		}
		if index+1 >= len(args) {
			return result, errors.New("qwen-host options require values and a native -- boundary")
		}
		value := args[index+1]
		switch args[index] {
		case "--qwen":
			result.Qwen = value
		case "--version":
			result.Version = value
		case "--runtime-dir":
			result.RuntimeDir = value
		case "--agent-runtime-dir":
			result.AgentRuntimeDir = value
		case "--registration-json":
			registrationJSON = value
		default:
			return result, fmt.Errorf("unknown qwen-host option %q", args[index])
		}
		index++
	}
	if result.Qwen == "" || result.Version == "" || result.RuntimeDir == "" ||
		result.AgentRuntimeDir == "" || registrationJSON == "" {
		return result, errors.New("qwen-host launch identity is incomplete")
	}
	if err := json.Unmarshal([]byte(registrationJSON), &result.Registration); err != nil {
		return result, fmt.Errorf("decode Qwen preparation: %w", err)
	}
	registration := result.Registration
	if registration.Product != "qwen" || registration.SessionID == "" || registration.QwenPreparation == nil ||
		registration.PID != os.Getpid() || registration.ProcStart != readProcStart(os.Getpid()) ||
		registration.LifecyclePID != os.Getpid() || registration.LifecycleProcStart != registration.ProcStart {
		return result, errors.New("qwen-host preparation does not match the exec-preserved lifecycle")
	}
	return result, nil
}

//nolint:gocyclo // Native admission, publication, signal forwarding, and exact rollback are separate lifecycle gates.
func runQwenInteractiveHost(ctx context.Context, config qwenHostArguments) error {
	registration := config.Registration
	payload := registration.QwenPreparation
	ownedArtifacts, err := observeQwenOwnedArtifacts(registration.LifecycleRoot, []string{payload.Input.Path, payload.Events.Path})
	if err != nil {
		return fmt.Errorf("observe private Qwen launch artifacts: %w", err)
	}
	command := exec.Command(config.Qwen, config.Native...) //nolint:gosec // Exact executable was admitted by readiness; native argv was parsed by qwen-peer.
	command.Dir = payload.CanonicalCwd
	command.Env = qwenPeerEnvironment(os.Environ(), registration.SessionID, config.AgentRuntimeDir)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		cleanupErr := removeQwenHostArtifacts(registration.LifecycleRoot, ownedArtifacts)
		if cleanupErr == nil {
			cleanupErr = federator.CancelPeerPreparation(config.AgentRuntimeDir, registration)
		}
		return errors.Join(fmt.Errorf("start native Qwen: %w", err), cleanupErr)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	rollback := func() error {
		if command.Process != nil {
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		if cleanupErr := removeQwenHostArtifacts(registration.LifecycleRoot, ownedArtifacts); cleanupErr != nil {
			return cleanupErr
		}
		return federator.CancelPeerPreparation(config.AgentRuntimeDir, registration)
	}

	expected := qwenAdmissionExpectation{
		SessionID: registration.SessionID, Cwd: payload.CanonicalCwd, Version: config.Version,
		ProtocolVersion: 2, RequiredEvents: qwenRequiredDualOutputEvents(),
	}
	cursor, _, err := waitForQwenAdmission(payload.Events.Path, expected, done, qwenHostAdmissionTimeout)
	if err != nil {
		return errors.Join(err, rollback())
	}
	defer func() { _ = cursor.Close() }()
	input, err := openQwenInputWriter(payload.Input.Path)
	if err != nil {
		return errors.Join(err, rollback())
	}
	defer func() { _ = input.Close() }()

	daemon := newDaemon(map[string]string{
		"session-id": registration.SessionID, "cwd": payload.CanonicalCwd, "name": registration.Name,
		"name-source": "launch", "entrypoint": "qwen", "permission-mode": "unknown",
		"qwen-home":         qwenNativeHome(payload.Profile),
		"data-dir":          strings.TrimSpace(os.Getenv("AGENT_SESSIONS_STATE_ROOT")),
		"claude-config-dir": strings.TrimSpace(os.Getenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR")),
		"codex-home":        strings.TrimSpace(os.Getenv("CODEX_HOME")), "runtime-dir": config.RuntimeDir,
		"agent-runtime-dir": config.AgentRuntimeDir,
	})
	daemon.registrationOverride = func(current federator.PeerRegistration) federator.PeerRegistration {
		current.LifecycleRoot = registration.LifecycleRoot
		current.QwenPreparation = registration.QwenPreparation
		current.QwenCapabilityDigest = registration.QwenCapabilityDigest
		if current.PermissionMode == "unknown" {
			current.PermissionMode = ""
		}
		return current
	}
	daemon.messageQueued = func(item map[string]any) {
		if submitErr := input.Submit(formatNativeHookMessages([]map[string]any{item})); submitErr == nil {
			daemon.removePendingMessage(stringValue(item["id"]))
		}
	}
	if err := daemon.start(); err != nil {
		return errors.Join(fmt.Errorf("publish managed Qwen peer: %w", err), rollback())
	}
	defer daemon.shutdown()

	eventContext, stopEvents := context.WithCancel(context.Background())
	defer stopEvents()
	go projectQwenEvents(eventContext, cursor, daemon)

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	requestedExitCode := 0
	select {
	case err = <-done:
	case signalValue := <-signals:
		if command.Process != nil {
			_ = command.Process.Signal(signalValue)
		}
		switch signalValue {
		case os.Interrupt:
			requestedExitCode = 130
		case syscall.SIGTERM:
			requestedExitCode = 143
		}
		err = <-done
	case <-ctx.Done():
		if command.Process != nil {
			_ = command.Process.Signal(syscall.SIGTERM)
		}
		requestedExitCode = 143
		err = <-done
	}
	daemon.shutdown()
	if cleanupErr := removeQwenHostArtifacts(registration.LifecycleRoot, ownedArtifacts); cleanupErr != nil {
		return cleanupErr
	}
	if cancelErr := federator.CancelPeerPreparation(config.AgentRuntimeDir, registration); cancelErr != nil {
		return fmt.Errorf("retire Qwen preparation: %w", cancelErr)
	}
	if err != nil {
		if requestedExitCode != 0 {
			return &qwenHostProcessExitError{Code: requestedExitCode, Err: err}
		}
		var native *exec.ExitError
		if errors.As(err, &native) && native.ExitCode() > 0 {
			return &qwenHostProcessExitError{Code: native.ExitCode(), Err: err}
		}
		return err
	}
	return nil
}

func qwenNativeHome(profile federator.QwenProfileIdentity) string {
	if profile.QwenHomeSet {
		return profile.QwenHome
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".qwen")
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func removeQwenHostArtifacts(root string, artifacts []qwenOwnedArtifact) error {
	defer closeQwenOwnedArtifacts(artifacts)
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0o700 {
		return errors.New("qwen cleanup ownership root changed")
	}
	for _, artifact := range artifacts {
		info, statErr := os.Lstat(artifact.Path)
		if statErr != nil || artifact.identity == nil || !info.Mode().IsRegular() ||
			info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, artifact.identity) {
			return &qwenCleanupDebtError{Paths: []string{artifact.Path}}
		}
	}
	for _, artifact := range artifacts {
		if err := os.Remove(artifact.Path); err != nil {
			return err
		}
	}
	if err := os.Remove(root); err != nil {
		return err
	}
	if err := os.Remove(filepath.Dir(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func qwenPeerEnvironment(environment []string, sessionID, agentRuntimeDir string) []string {
	replacements := map[string]string{
		peerSessionIDEnvironment:   sessionID,
		"AGENT_SESSIONS_PRODUCT":   "qwen",
		agentRuntimeDirEnvironment: agentRuntimeDir,
	}
	result := make([]string, 0, len(environment)+len(replacements))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := replacements[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}

func waitForQwenAdmission(
	path string,
	expected qwenAdmissionExpectation,
	done <-chan error,
	timeout time.Duration,
) (*qwenEventCursor, qwenSessionStart, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		cursor, start, err := admitQwenDualOutput(path, expected)
		if err == nil {
			return cursor, start, nil
		}
		select {
		case nativeErr := <-done:
			return nil, qwenSessionStart{}, fmt.Errorf("native Qwen exited before admission: %w", nativeErr)
		case <-deadline.C:
			return nil, qwenSessionStart{}, fmt.Errorf("timed out admitting native Qwen session: %w", err)
		case <-ticker.C:
		}
	}
}

func projectQwenEvents(ctx context.Context, cursor *qwenEventCursor, daemon *daemon) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			events, err := cursor.ReadAvailable()
			if err != nil {
				return
			}
			for _, raw := range events {
				var event map[string]any
				if json.Unmarshal(raw, &event) != nil {
					continue
				}
				status := ""
				mode := ""
				switch stringValue(event["type"]) {
				case "user", "assistant", "stream_event":
					status = "busy"
				case "control_request":
					status = "waiting"
				case "control_response", "result":
					status = "idle"
				case "current_mode_update", "approval_mode_changed":
					mode = defaultString(stringValue(event["current_mode_id"]), stringValue(event["currentModeId"]))
				}
				if status != "" {
					daemon.handleControl(map[string]any{"action": "status", "status": status})
				}
				if mode != "" {
					daemon.handleControl(map[string]any{"action": "permission_mode", "permissionMode": mode})
				}
			}
		}
	}
}

func (d *daemon) removePendingMessage(messageID string) {
	messageID = safeID(messageID)
	if messageID == "" {
		return
	}
	entries, _ := os.ReadDir(d.pendingDir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-"+messageID+".json") {
			_ = os.Remove(filepath.Join(d.pendingDir, entry.Name()))
		}
	}
}
