//go:build linux || darwin

package bridge

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/antst/agent-sessions/internal/procinfo"
)

const (
	qwenToolRegistryVersion = 1
	qwenToolWrapperModeEnv  = "AGENT_SESSIONS_QWEN_TOOL_WRAPPER"
	qwenToolRealBashEnv     = "AGENT_SESSIONS_QWEN_REAL_BASH"
	qwenToolWrapperPathEnv  = "AGENT_SESSIONS_QWEN_WRAPPER_PATH"
)

type qwenToolRegistryPaths struct {
	root, lock, ledgerRoot, wrapper string
}

type qwenToolRegistryGuard struct {
	lock   *os.File
	paths  qwenToolRegistryPaths
	ledger *toolRootLedger
}

func qwenToolRegistryPathsForState(state qwenLaneState) (qwenToolRegistryPaths, error) {
	if len(state.LaunchTokenHash) != 64 || strings.Trim(state.LaunchTokenHash, "0123456789abcdef") != "" {
		return qwenToolRegistryPaths{}, errors.New("invalid Qwen lane tool registry capability")
	}
	root := filepath.Join(bridgeRuntimeRoot(defaultString(state.RuntimeDir, os.TempDir()), os.Getuid()), "qwen-tools-"+state.LaunchTokenHash[:20])
	return qwenToolRegistryPaths{
		root: root, lock: filepath.Join(root, "register.lock"), ledgerRoot: filepath.Join(root, "roots"),
		wrapper: filepath.Join(root, "bash"),
	}, nil
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func (m *qwenLaneManager) prepareToolRegistry() error {
	realBash, err := exec.LookPath("bash")
	if err != nil {
		return errors.New("qwen lane requires an executable bash")
	}
	realBash, err = filepath.EvalSymlinks(realBash)
	if err != nil || !filepath.IsAbs(realBash) || filepath.Base(realBash) != "bash" {
		return errors.New("resolve exact Qwen lane bash executable")
	}
	runtimeExecutable, err := os.Executable()
	if err != nil {
		return errors.New("resolve Qwen lane tool wrapper")
	}
	runtimeExecutable, err = filepath.EvalSymlinks(runtimeExecutable)
	if err != nil || samePath(runtimeExecutable, realBash) {
		return errors.New("resolve distinct Qwen lane tool wrapper")
	}
	paths, err := qwenToolRegistryPathsForState(m.state)
	if err != nil {
		return err
	}
	if err := ensurePrivateToolRootDirectory(paths.root); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.ledgerRoot, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(paths.ledgerRoot, 0o700); err != nil { //nolint:gosec // exact private durable ledger directory.
		return err
	}
	lock, err := os.OpenFile(paths.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	_ = lock.Close()
	if info, statErr := os.Lstat(paths.wrapper); statErr == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("refuse non-symlink Qwen lane bash wrapper")
		}
		resolved, resolveErr := filepath.EvalSymlinks(paths.wrapper)
		if resolveErr != nil || !samePath(resolved, runtimeExecutable) {
			return errors.New("refuse mismatched Qwen lane bash wrapper")
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	} else if err := os.Symlink(runtimeExecutable, paths.wrapper); err != nil {
		return err
	}
	m.mu.Lock()
	m.state.ToolRegistryVersion, m.state.ToolWrapperPath, m.state.ToolRealBash = qwenToolRegistryVersion, paths.wrapper, realBash
	err = m.persistLocked()
	m.mu.Unlock()
	return err
}

func qwenToolRootLedgerConfig(state qwenLaneState) (toolRootLedgerConfig, error) {
	paths, err := qwenToolRegistryPathsForState(state)
	if err != nil {
		return toolRootLedgerConfig{}, err
	}
	config := toolRootLedgerConfig{
		Version: qwenToolRegistryVersion, Product: "qwen",
		ManagerIdentity:  qwenLaneProcessIdentity(state.ManagerPID, state.ManagerProcStart, state.ManagerStrongStart),
		WorkerIdentity:   qwenLaneProcessIdentity(state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart),
		CapabilityDigest: state.LaunchTokenHash, IntentRevision: state.ThreadID,
		Root: paths.ledgerRoot, ObserveProcess: procinfo.Read,
	}
	config.RetireRoot = func(root toolRootProcessIdentity) error {
		return stopGrokProcessSessionStrong(root.PID, root.ProcStart, root.StrongStart, state.ManagerPID)
	}
	if err := validateToolRootLedgerConfig(config); err != nil {
		return toolRootLedgerConfig{}, err
	}
	return config, nil
}

func (m *qwenLaneManager) prepareSharedToolRootLedger() error {
	m.mu.Lock()
	state := cloneQwenLaneState(m.state)
	m.mu.Unlock()
	config, err := qwenToolRootLedgerConfig(state)
	if err != nil {
		return err
	}
	_, err = prepareToolRootLedger(config)
	return err
}

func qwenLaneWorkerToolEnvironment(environment []string, state qwenLaneState) []string {
	blocked := map[string]bool{
		qwenToolWrapperModeEnv: true, qwenToolRealBashEnv: true, qwenToolWrapperPathEnv: true,
	}
	result := make([]string, 0, len(environment)+4)
	pathValue := ""
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if name == "PATH" && found {
			pathValue = value
			continue
		}
		if !blocked[name] {
			result = append(result, entry)
		}
	}
	result = append(result,
		"PATH="+filepath.Dir(state.ToolWrapperPath)+string(os.PathListSeparator)+pathValue,
		qwenToolWrapperModeEnv+"=1", qwenToolRealBashEnv+"="+state.ToolRealBash,
		qwenToolWrapperPathEnv+"="+state.ToolWrapperPath,
	)
	return result
}

func isQwenToolWrapperInvocation() bool {
	if os.Getenv(qwenToolWrapperModeEnv) != "1" || len(os.Args) == 0 {
		return false
	}
	expected := strings.TrimSpace(os.Getenv(qwenToolWrapperPathEnv))
	if !filepath.IsAbs(expected) || filepath.Base(expected) != "bash" {
		return false
	}
	if filepath.IsAbs(os.Args[0]) {
		return samePath(os.Args[0], expected)
	}
	return os.Args[0] == "bash"
}

func runQwenToolWrapper(argv []string) int {
	if err := registerQwenToolRoot(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-session-runtime: refuse untracked Qwen tool shell")
		return 126
	}
	realBash := strings.TrimSpace(os.Getenv(qwenToolRealBashEnv))
	if !filepath.IsAbs(realBash) || filepath.Base(realBash) != "bash" {
		return 126
	}
	if err := syscall.Exec(realBash, append([]string{realBash}, argv...), os.Environ()); err != nil { //nolint:gosec // exact persisted bash executable.
		return 126
	}
	return 0
}

func registerQwenToolRoot() error { //nolint:gocyclo // Wrapper admission checks every durable identity and path before exec.
	if !isQwenToolWrapperInvocation() {
		return errors.New("qwen tool wrapper mode is unavailable")
	}
	threadID := strings.TrimSpace(os.Getenv(peerSessionIDEnvironment))
	capability := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_QWEN_CAPABILITY"))
	if !validSessionID(threadID) || !validQwenLaneToken(capability) {
		return errors.New("invalid Qwen tool wrapper context")
	}
	paths := resolveNativePaths()
	body, err := os.ReadFile(qwenLaneStatePath(paths, threadID))
	if err != nil {
		return err
	}
	var state qwenLaneState
	if json.Unmarshal(body, &state) != nil || state.ThreadID != threadID || state.ToolRegistryVersion != qwenToolRegistryVersion ||
		subtle.ConstantTimeCompare([]byte(state.LaunchTokenHash), []byte(qwenLaneTokenHash(capability))) != 1 {
		return errors.New("qwen tool wrapper state mismatch")
	}
	registry, err := qwenToolRegistryPathsForState(state)
	if err != nil || !samePath(registry.wrapper, os.Getenv(qwenToolWrapperPathEnv)) {
		return errors.New("qwen tool wrapper path mismatch")
	}
	lock, err := os.OpenFile(registry.lock, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	state, err = readQwenLaneState(paths, threadID)
	if err != nil || state.Status == "archived" || state.Status == "retiring" ||
		state.ToolRealBash != os.Getenv(qwenToolRealBashEnv) || !exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) {
		return errors.New("qwen lane is not admitting tool shells")
	}
	info := procinfo.Read(os.Getpid())
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		return errors.New("capture strong Qwen tool process identity")
	}
	config, err := qwenToolRootLedgerConfig(state)
	if err != nil {
		return err
	}
	ledger, err := openToolRootLedger(config)
	if err != nil {
		return err
	}
	return ledger.register(toolRootProcessIdentity{PID: os.Getpid(), ProcStart: info.Start, StrongStart: info.StrongStart})
}

func lockQwenToolRegistry(state qwenLaneState) (*qwenToolRegistryGuard, error) {
	if state.ToolRegistryVersion == 0 {
		return &qwenToolRegistryGuard{}, nil
	}
	if state.ToolRegistryVersion != qwenToolRegistryVersion {
		return nil, errors.New("unsupported Qwen tool registry version")
	}
	paths, err := qwenToolRegistryPathsForState(state)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(paths.lock, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	guard := &qwenToolRegistryGuard{lock: lock, paths: paths}
	config, err := qwenToolRootLedgerConfig(state)
	if err != nil {
		guard.close()
		return nil, err
	}
	guard.ledger, err = openToolRootLedger(config)
	if err != nil {
		guard.close()
		return nil, err
	}
	if err := guard.ledger.closeAdmission(); err != nil {
		guard.close()
		return nil, err
	}
	return guard, nil
}

func (guard *qwenToolRegistryGuard) close() {
	if guard == nil || guard.lock == nil {
		return
	}
	_ = syscall.Flock(int(guard.lock.Fd()), syscall.LOCK_UN)
	_ = guard.lock.Close()
	guard.lock = nil
}

func (guard *qwenToolRegistryGuard) removeArtifacts() error {
	if guard == nil {
		return nil
	}
	for _, path := range []string{
		filepath.Join(guard.paths.ledgerRoot, "ledger.json"), filepath.Join(guard.paths.ledgerRoot, "ledger.lock"),
		guard.paths.ledgerRoot, guard.paths.wrapper, guard.paths.lock, guard.paths.root,
	} {
		if path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
