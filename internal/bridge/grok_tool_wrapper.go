//go:build linux || darwin

package bridge

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

const (
	grokToolRegistryVersion = 1
	grokToolWrapperModeEnv  = "AGENT_SESSIONS_GROK_TOOL_WRAPPER"
	grokToolRealShellEnv    = "AGENT_SESSIONS_GROK_REAL_SHELL"
	grokToolWrapperPathEnv  = "AGENT_SESSIONS_GROK_WRAPPER_PATH"
)

type grokToolRootRecord struct {
	Version     int    `json:"version"`
	SessionID   string `json:"sessionId"`
	TokenHash   string `json:"tokenHash"`
	PID         int    `json:"pid"`
	ProcStart   string `json:"procStart"`
	StrongStart string `json:"strongStart"`
	CreatedAt   int64  `json:"createdAt"`
}

type grokToolRegistryPaths struct {
	lock, roots, wrapper string
}

type grokToolRegistryGuard struct {
	lock  *os.File
	paths grokToolRegistryPaths
	roots []grokSessionMember
}

func grokToolRegistryPathsForState(state grokLaneState) (grokToolRegistryPaths, error) {
	if len(state.LaunchTokenHash) != 64 {
		return grokToolRegistryPaths{}, errors.New("invalid Grok lane tool registry capability")
	}
	runtimeDir := defaultString(state.RuntimeDir, os.TempDir())
	host := grokRuntimePathsForKey(runtimeDir, os.Getuid(), state.LaunchTokenHash[:20])
	paths := grokToolRegistryPaths{
		lock:  filepath.Join(host.LaunchDir, "tool-register.lock"),
		roots: filepath.Join(host.LaunchDir, "tool-roots"),
	}
	if state.ToolRegistryVersion == grokToolRegistryVersion && !containsString([]string{"bash", "zsh"}, state.ToolShellName) {
		return grokToolRegistryPaths{}, errors.New("invalid grok tool shell registry name")
	}
	if containsString([]string{"bash", "zsh"}, state.ToolShellName) {
		paths.wrapper = filepath.Join(host.LaunchDir, state.ToolShellName)
	}
	return paths, nil
}

func selectGrokLaneRealShell(environment []string) (string, error) {
	values := map[string]string{}
	for _, entry := range environment {
		if separator := strings.IndexByte(entry, '='); separator > 0 {
			values[entry[:separator]] = entry[separator+1:]
		}
	}
	candidates := []string{values["GROK_SHELL"], values["SHELL"], "/bin/zsh", "/bin/bash"}
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || !containsString([]string{"bash", "zsh"}, filepath.Base(resolved)) {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return resolved, nil
		}
	}
	return "", errors.New("grok lane requires an absolute executable bash or zsh shell")
}

//nolint:gocyclo // Setup durably declares intent before creating each private registry artifact and validates preexisting paths.
func (m *grokLaneManager) prepareToolRegistry() error {
	realShell, err := selectGrokLaneRealShell(os.Environ())
	if err != nil {
		return err
	}
	runtimeExecutable, err := os.Executable()
	if err != nil {
		return errors.New("resolve Grok lane runtime for tool wrapper")
	}
	runtimeExecutable, err = filepath.EvalSymlinks(runtimeExecutable)
	if err != nil || samePath(runtimeExecutable, realShell) {
		return errors.New("resolve distinct Grok lane runtime tool wrapper")
	}
	paths, err := grokToolRegistryPathsForState(m.state)
	if err != nil {
		return err
	}
	paths.wrapper = filepath.Join(m.hostPaths.LaunchDir, filepath.Base(realShell))
	m.toolShellPath, m.toolRealShell = paths.wrapper, realShell
	m.state.ToolRegistryVersion = grokToolRegistryVersion
	m.state.ToolShellName = filepath.Base(realShell)
	m.state.ToolRealShell = realShell
	m.mu.Lock()
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("persist Grok lane tool registry intent: %w", err)
	}
	lock, err := os.OpenFile(paths.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create Grok lane tool registration lock: %w", err)
	}
	_ = lock.Close()
	if err := os.Mkdir(paths.roots, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create Grok lane tool registry: %w", err)
	}
	if info, statErr := os.Lstat(paths.wrapper); statErr == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("refuse non-symlink Grok lane tool wrapper")
		}
		target, readErr := filepath.EvalSymlinks(paths.wrapper)
		if readErr != nil || !samePath(target, runtimeExecutable) {
			return errors.New("refuse mismatched Grok lane tool wrapper")
		}
	} else if !os.IsNotExist(statErr) {
		return errors.New("inspect Grok lane tool wrapper")
	} else if err := os.Symlink(runtimeExecutable, paths.wrapper); err != nil {
		return fmt.Errorf("create Grok lane tool wrapper: %w", err)
	}
	return nil
}

func grokLaneWorkerEnvironment(environment []string, launchToken string, state grokLaneState, wrapperPath, realShell string) []string {
	blocked := map[string]bool{
		"SHELL": true, "GROK_SHELL": true,
		grokToolWrapperModeEnv: true, grokToolRealShellEnv: true,
		grokToolWrapperPathEnv:   true,
		peerSessionIDEnvironment: true, "AGENT_SESSIONS_PRODUCT": true, agentRuntimeDirEnvironment: true,
		remoteParentEnvironment: true,
	}
	result := grokLaneManagerEnvironment(environment, launchToken, state.SessionID)
	filtered := result[:0]
	for _, entry := range result {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if !blocked[name] {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered,
		"SHELL="+wrapperPath, "GROK_SHELL="+wrapperPath,
		grokToolWrapperModeEnv+"=1", grokToolRealShellEnv+"="+realShell,
		grokToolWrapperPathEnv+"="+wrapperPath,
		peerSessionIDEnvironment+"="+state.SessionID, "AGENT_SESSIONS_PRODUCT=grok",
		agentRuntimeDirEnvironment+"="+laneAgentRuntimeDir(),
	)
}

func runGrokToolWrapper(argv []string) int {
	if err := registerGrokToolRoot(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-session-runtime: refuse untracked Grok tool shell")
		return 126
	}
	realShell := strings.TrimSpace(os.Getenv(grokToolRealShellEnv))
	if !filepath.IsAbs(realShell) || !containsString([]string{"bash", "zsh"}, filepath.Base(realShell)) {
		return 126
	}
	arguments := append([]string{realShell}, argv...)
	if err := syscall.Exec(realShell, arguments, os.Environ()); err != nil { //nolint:gosec // exact absolute shell validated and persisted by the manager.
		fmt.Fprintln(os.Stderr, "agent-session-runtime: execute registered Grok tool shell failed")
		return 126
	}
	return 0
}

func isGrokToolWrapperInvocation() bool {
	if os.Getenv(grokToolWrapperModeEnv) != "1" || len(os.Args) == 0 {
		return false
	}
	expected := strings.TrimSpace(os.Getenv(grokToolWrapperPathEnv))
	if !filepath.IsAbs(expected) || !containsString([]string{"bash", "zsh"}, filepath.Base(expected)) {
		return false
	}
	if filepath.IsAbs(os.Args[0]) {
		return samePath(os.Args[0], expected)
	}
	return os.Args[0] == filepath.Base(expected)
}

//nolint:gocyclo // Wrapper admission validates capability, lifecycle, paths, locking, and strong identity in one transaction.
func registerGrokToolRoot() error {
	if !isGrokToolWrapperInvocation() {
		return errors.New("tool wrapper mode is unavailable")
	}
	sessionID := strings.TrimSpace(os.Getenv(grokSessionIDEnv))
	launchToken := strings.TrimSpace(os.Getenv(grokLaunchTokenEnv))
	wrapperPath := strings.TrimSpace(os.Getenv(grokToolWrapperPathEnv))
	if !validSessionID(sessionID) || !validGrokLaunchToken(launchToken) || !filepath.IsAbs(wrapperPath) {
		return errors.New("invalid Grok tool wrapper context")
	}
	statePath := grokLaneStatePath(resolveNativePaths(), sessionID)
	body, err := os.ReadFile(statePath) //nolint:gosec // absolute state path is capability-bound and checked against the persisted session.
	if err != nil {
		return err
	}
	var state grokLaneState
	if json.Unmarshal(body, &state) != nil || state.Type != "grok-peer-lane" || state.SessionID != sessionID || state.ToolRegistryVersion != grokToolRegistryVersion ||
		subtle.ConstantTimeCompare([]byte(state.LaunchTokenHash), []byte(grokTokenHash(launchToken))) != 1 {
		return errors.New("grok tool wrapper state mismatch")
	}
	paths, err := grokToolRegistryPathsForState(state)
	if err != nil || !samePath(wrapperPath, paths.wrapper) {
		return errors.New("grok tool wrapper path mismatch")
	}
	lock, err := os.OpenFile(paths.lock, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	// Re-read only after admission is serialized with archival cleanup. A late
	// wrapper must never exec after the lane has durably begun closing.
	body, err = os.ReadFile(statePath) //nolint:gosec // same capability-bound state path, re-read under the registration lock.
	if err != nil || json.Unmarshal(body, &state) != nil || state.Status == "archived" || state.Status == "" ||
		state.SessionID != sessionID || state.LaunchTokenHash != grokTokenHash(launchToken) ||
		state.ToolRealShell == "" || state.ToolRealShell != os.Getenv(grokToolRealShellEnv) ||
		!exactProcessIdentityMatch(state.ManagerPID, state.ManagerProcStart) {
		return errors.New("grok lane is not admitting tool shells")
	}
	info := procinfo.Read(os.Getpid())
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		return errors.New("capture strong Grok tool process identity")
	}
	record := grokToolRootRecord{
		Version: grokToolRegistryVersion, SessionID: sessionID, TokenHash: state.LaunchTokenHash,
		PID: os.Getpid(), ProcStart: info.Start, StrongStart: info.StrongStart, CreatedAt: time.Now().UnixMilli(),
	}
	recordPath := filepath.Join(paths.roots, strconv.Itoa(record.PID)+".json")
	if existingBody, readErr := os.ReadFile(recordPath); readErr == nil { //nolint:gosec // numeric PID filename under the fixed private registry.
		var existing grokToolRootRecord
		if json.Unmarshal(existingBody, &existing) != nil || existing.PID != record.PID || existing.ProcStart == "" || existing.StrongStart == "" {
			return errors.New("conflicting Grok tool process identity")
		}
		if existing.StrongStart != record.StrongStart && grokProcessIdentityStatus(grokSessionMember{
			PID: existing.PID, ProcStart: existing.ProcStart, StrongStart: existing.StrongStart,
		}) != processIdentityStale {
			return errors.New("live or unverifiable Grok tool process identity conflict")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	return writeJSONAtomic(recordPath, record)
}

//nolint:gocyclo // Registry validation deliberately fails closed on every malformed ownership field and filesystem state.
func lockGrokToolRegistry(state grokLaneState, exclusive bool) (*grokToolRegistryGuard, error) {
	if state.ToolRegistryVersion == 0 {
		return &grokToolRegistryGuard{}, nil
	}
	if state.ToolRegistryVersion != grokToolRegistryVersion {
		return nil, errors.New("unsupported Grok tool registry version")
	}
	paths, err := grokToolRegistryPathsForState(state)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(paths.lock, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		_, wrapperErr := os.Lstat(paths.wrapper)
		_, rootsErr := os.Lstat(paths.roots)
		if os.IsNotExist(wrapperErr) && os.IsNotExist(rootsErr) {
			return &grokToolRegistryGuard{paths: paths}, nil
		}
		return nil, errors.New("grok tool registration lock is missing")
	}
	if err != nil {
		return nil, errors.New("open Grok tool registration lock")
	}
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(lock.Fd()), operation); err != nil {
		_ = lock.Close()
		return nil, errors.New("lock Grok tool registry")
	}
	guard := &grokToolRegistryGuard{lock: lock, paths: paths}
	entries, err := os.ReadDir(paths.roots)
	if err != nil && !os.IsNotExist(err) {
		guard.close()
		return nil, errors.New("read Grok tool registry")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			guard.close()
			return nil, errors.New("malformed Grok tool registry entry")
		}
		body, readErr := os.ReadFile(filepath.Join(paths.roots, entry.Name()))
		var record grokToolRootRecord
		if readErr != nil || json.Unmarshal(body, &record) != nil || record.Version != grokToolRegistryVersion ||
			record.SessionID != state.SessionID || record.TokenHash != state.LaunchTokenHash || record.PID <= 1 ||
			record.ProcStart == "" || record.StrongStart == "" || entry.Name() != strconv.Itoa(record.PID)+".json" {
			guard.close()
			return nil, errors.New("invalid Grok tool registry entry")
		}
		guard.roots = append(guard.roots, grokSessionMember{PID: record.PID, ProcStart: record.ProcStart, StrongStart: record.StrongStart})
	}
	return guard, nil
}

func (guard *grokToolRegistryGuard) close() {
	if guard == nil || guard.lock == nil {
		return
	}
	_ = syscall.Flock(int(guard.lock.Fd()), syscall.LOCK_UN)
	_ = guard.lock.Close()
	guard.lock = nil
}

func (guard *grokToolRegistryGuard) removeArtifacts() error {
	if guard == nil {
		return nil
	}
	entries, err := os.ReadDir(guard.paths.roots)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("read Grok tool registry during cleanup")
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(guard.paths.roots, entry.Name())); err != nil && !os.IsNotExist(err) {
			return errors.New("remove Grok tool registry entry")
		}
	}
	for _, path := range []string{guard.paths.roots, guard.paths.wrapper, guard.paths.lock} {
		if path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return errors.New("remove Grok tool registry artifact")
			}
		}
	}
	return nil
}

func grokLaneWorkerRoot(state grokLaneState) grokSessionMember {
	return grokSessionMember{PID: state.WorkerPID, ProcStart: state.WorkerProcStart, StrongStart: state.WorkerStrongStart}
}

func grokLaneCleanupRoots(state grokLaneState, exclusive bool) (*grokToolRegistryGuard, []grokSessionMember, error) {
	guard, err := lockGrokToolRegistry(state, exclusive)
	if err != nil {
		return nil, nil, err
	}
	roots := make([]grokSessionMember, 0, len(guard.roots)+1)
	worker := grokLaneWorkerRoot(state)
	if worker.PID > 1 && worker.ProcStart != "" {
		roots = append(roots, worker)
	}
	roots = append(roots, guard.roots...)
	return guard, roots, nil
}
