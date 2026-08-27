// Package launcher implements the native user-facing agent-session launchers.
package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/antst/agent-sessions/internal/daemon"
)

// ExitError carries a stable command exit status.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// Runtime describes the transitional legacy runtime selected for a launcher
// invocation. Later adapter stories remove this executable boundary entirely.
type Runtime struct {
	Path string
}

var queryLauncherDaemon = func(ctx context.Context) error {
	_, err := daemon.QueryAdmin(ctx, "runtime.status")
	return err
}

// EnsureRuntime is retained as a transitional caller boundary. It only
// verifies that the user-managed daemon is already available and discovers
// the installed legacy adapter executable. It must never create state or
// start, stop, restart, replace, or repair any process.
func EnsureRuntime() (Runtime, error) {
	if err := requireUserDaemon(); err != nil {
		return Runtime{}, err
	}
	return discoverRuntime()
}

func requireUserDaemon() error {
	err := queryLauncherDaemon(context.Background())
	if err == nil {
		return nil
	}
	var unavailable *daemon.UnavailableError
	if errors.As(err, &unavailable) {
		return &ExitError{Code: unavailable.ExitCode(), Err: err}
	}
	paths, pathErr := daemon.ResolveProductionPaths()
	if pathErr != nil {
		failure := &daemon.UnavailableError{Cause: errors.Join(err, pathErr), NextAction: daemon.InspectionCommand()}
		return &ExitError{Code: failure.ExitCode(), Err: failure}
	}
	failure := &daemon.UnavailableError{
		Endpoint: paths.ControlEndpoint, Cause: err, NextAction: daemon.InspectionCommand(),
	}
	return &ExitError{Code: failure.ExitCode(), Err: failure}
}

// discoverRuntime resolves the temporary legacy adapter image without
// creating its state root or activating any App Server/supervisor lifetime.
func discoverRuntime() (Runtime, error) {
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
	runtimePath := os.Getenv("CODEX_PEER_NATIVE_RUNTIME")
	if runtimePath == "" {
		platform, platformErr := platformKey()
		if platformErr != nil {
			return Runtime{}, platformErr
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
	return Runtime{Path: runtimePath}, nil
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

func capture(path string, args ...string) (string, error) {
	// #nosec G204 G702 -- path is the selected installed runtime executable.
	command := exec.Command(path, args...)
	command.Stderr = os.Stderr
	payload, err := command.Output()
	return string(payload), err
}
