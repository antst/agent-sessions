// Package launcher implements the native user-facing agent-session launchers.
package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const bootstrapTimeout = 30 * time.Second

// ExitError carries a stable command exit status.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// Runtime describes the shared runtime selected for a launcher invocation.
type Runtime struct {
	Path          string
	Codex         string
	PluginRoot    string
	StateRoot     string
	PluginVersion string
}

// EnsureRuntime selects the installed runtime, activates the matching App
// Server plugin version, and starts the shared supervisor.
func EnsureRuntime() (Runtime, error) {
	selected, err := discoverRuntime()
	if err != nil {
		return Runtime{}, err
	}
	if err := ensureRuntimeEnvironment(); err != nil {
		return Runtime{}, err
	}
	profileRoot, versionMarker, err := runtimeProfile(selected)
	if err != nil {
		return Runtime{}, err
	}
	loadedVersion := readFirstLine(versionMarker)
	if err := preflightVersionChange(selected, loadedVersion); err != nil {
		return Runtime{}, err
	}
	unlock, err := acquireBootstrapLock(profileRoot)
	if err != nil {
		return Runtime{}, err
	}
	defer unlock()

	if readFirstLine(versionMarker) != selected.PluginVersion && !runtimeCommandSucceeds(selected.Path, "appserver", "stopped") {
		return Runtime{}, &ExitError{Code: 75, Err: errors.New("plugin update deferred; App Server started while waiting for the update lock")}
	}
	if err := activateRuntime(selected, versionMarker); err != nil {
		return Runtime{}, err
	}
	return selected, nil
}

func runtimeProfile(selected Runtime) (string, string, error) {
	profileKey, err := capture(selected.Path, "appserver", "profile-key")
	if err != nil {
		return "", "", fmt.Errorf("resolve Codex profile: %w", err)
	}
	profileKey = strings.TrimSpace(profileKey)
	if len(profileKey) != 20 || strings.Trim(profileKey, "0123456789abcdef") != "" {
		return "", "", errors.New("native runtime returned an invalid Codex profile key")
	}
	profileRoot := filepath.Join(selected.StateRoot, "profiles", profileKey)
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		return "", "", fmt.Errorf("create profile state: %w", err)
	}
	return profileRoot, filepath.Join(profileRoot, "app-server-plugin-version"), nil
}

func preflightVersionChange(selected Runtime, loadedVersion string) error {
	if loadedVersion == selected.PluginVersion {
		return nil
	}
	if os.Getenv("CODEX_THREAD_ID") != "" {
		return &ExitError{Code: 75, Err: errors.New("plugin update deferred; exit Codex clients, stop App Server, and retry from a host shell")}
	}
	if !runtimeCommandSucceeds(selected.Path, "appserver", "stopped") {
		return &ExitError{Code: 75, Err: errors.New("plugin update deferred; run codex app-server daemon stop after exiting every Codex client")}
	}
	return nil
}

func activateRuntime(selected Runtime, versionMarker string) error {
	if err := runQuiet(selected.Codex, "app-server", "daemon", "start"); err != nil {
		return fmt.Errorf("start Codex App Server: %w", err)
	}
	if err := writeAtomic(filepath.Join(selected.StateRoot, "native-runtime-path"), selected.Path); err != nil {
		return fmt.Errorf("publish runtime path: %w", err)
	}
	if err := writeAtomic(versionMarker, selected.PluginVersion); err != nil {
		return fmt.Errorf("publish plugin version: %w", err)
	}
	if err := runQuiet(selected.Path, "supervisor", "start", "--plugin-version", selected.PluginVersion); err != nil {
		return errors.New("app Server started, but peer supervisor replacement failed; rerun the launcher to retry it")
	}
	return nil
}

func discoverRuntime() (Runtime, error) {
	codex := os.Getenv("CODEX_PEER_CODEX_BIN")
	if codex == "" {
		var err error
		codex, err = exec.LookPath("codex")
		if err != nil {
			return Runtime{}, &ExitError{Code: 127, Err: errors.New("codex was not found on PATH")}
		}
	}
	pluginRoot := os.Getenv("CODEX_PEER_PLUGIN_ROOT")
	if pluginRoot == "" {
		executable, err := os.Executable()
		if err != nil {
			return Runtime{}, fmt.Errorf("resolve launcher executable: %w", err)
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return Runtime{}, fmt.Errorf("resolve launcher symlink: %w", err)
		}
		pluginRoot = filepath.Clean(filepath.Join(filepath.Dir(executable), "..", ".."))
	}
	pluginRoot, err := filepath.Abs(pluginRoot)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve plugin root: %w", err)
	}
	version, err := readPluginVersion(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"))
	if err != nil {
		return Runtime{}, err
	}
	stateRoot, err := stateRoot()
	if err != nil {
		return Runtime{}, err
	}
	runtimePath := os.Getenv("CODEX_PEER_NATIVE_RUNTIME")
	if runtimePath == "" {
		platform, err := platformKey()
		if err != nil {
			return Runtime{}, err
		}
		runtimePath = filepath.Join(pluginRoot, "bin", platform, "agent-session-runtime")
	}
	runtimePath, err = filepath.Abs(runtimePath)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve native runtime: %w", err)
	}
	info, err := os.Stat(runtimePath)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return Runtime{}, fmt.Errorf("native runtime is unavailable: %s", runtimePath)
	}
	return Runtime{Path: runtimePath, Codex: codex, PluginRoot: pluginRoot, StateRoot: stateRoot, PluginVersion: version}, nil
}

func readPluginVersion(path string) (string, error) {
	payload, err := os.ReadFile(path) //nolint:gosec // path is the selected plugin manifest.
	if err != nil {
		return "", fmt.Errorf("read plugin version: %w", err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil || strings.TrimSpace(manifest.Version) == "" {
		return "", errors.New("could not read the plugin version")
	}
	return manifest.Version, nil
}

func stateRoot() (string, error) {
	root := os.Getenv("CLAUDE_PEER_DATA_DIR")
	if root == "" {
		root = os.Getenv("XDG_STATE_HOME")
		if root != "" {
			root = filepath.Join(root, "claude-code-peer")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home directory: %w", err)
			}
			root = filepath.Join(home, ".local", "state", "claude-code-peer")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve state root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create state root: %w", err)
	}
	return root, nil
}

func ensureRuntimeEnvironment() error {
	if os.Getenv("XDG_RUNTIME_DIR") != "" || runtime.GOOS != "linux" {
		return nil
	}
	candidate := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return os.Setenv("XDG_RUNTIME_DIR", candidate)
	}
	return nil
}

func platformKey() (string, error) {
	platform := runtime.GOOS
	if platform != "linux" && platform != "darwin" {
		return "", fmt.Errorf("unsupported operating system: %s", platform)
	}
	switch runtime.GOARCH {
	case "amd64":
		return platform + "-x64", nil
	case "arm64":
		return platform + "-arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}

func acquireBootstrapLock(profileRoot string) (func(), error) {
	lockDir := filepath.Join(profileRoot, "runtime-bootstrap.lock.d")
	ownerPath := filepath.Join(lockDir, "owner")
	deadline := time.Now().Add(bootstrapTimeout)
	for time.Now().Before(deadline) {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			if err := os.WriteFile(ownerPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
				_ = os.Remove(lockDir)
				return nil, fmt.Errorf("write bootstrap lock owner: %w", err)
			}
			return func() {
				if readFirstLine(ownerPath) == strconv.Itoa(os.Getpid()) {
					_ = os.Remove(ownerPath)
					_ = os.Remove(lockDir)
				}
			}, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire runtime bootstrap lock: %w", err)
		}
		if pid, err := strconv.Atoi(readFirstLine(ownerPath)); err == nil && pid > 1 && processAbsent(pid) {
			_ = os.Remove(ownerPath)
			_ = os.Remove(lockDir)
			continue
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("timed out acquiring the runtime bootstrap lock")
}

func processAbsent(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func capture(path string, args ...string) (string, error) {
	// #nosec G204 -- path is the selected Codex or installed runtime executable.
	command := exec.Command(path, args...)
	command.Stderr = os.Stderr
	payload, err := command.Output()
	return string(payload), err
}

func runQuiet(path string, args ...string) error {
	// #nosec G204 -- path is the selected Codex or installed runtime executable.
	command := exec.Command(path, args...)
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = os.Stderr
	return command.Run()
}

func runtimeCommandSucceeds(path string, args ...string) bool {
	// #nosec G204 -- path is the selected installed runtime executable.
	command := exec.Command(path, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run() == nil
}

func readFirstLine(path string) string {
	payload, err := os.ReadFile(path) //nolint:gosec // path is a selected bridge state marker.
	if err != nil {
		return ""
	}
	if index := strings.IndexByte(string(payload), '\n'); index >= 0 {
		payload = payload[:index]
	}
	return strings.TrimSpace(string(payload))
}

func writeAtomic(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agent-sessions-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(value + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
