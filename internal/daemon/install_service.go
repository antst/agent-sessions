package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/servicecontrol"
)

type serviceDefinitionSnapshot struct {
	present bool
	body    []byte
	mode    os.FileMode
}

func hostServiceDefinitionPath() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "net.antst.agent-sessions.plist")
	}
	configuration := os.Getenv("XDG_CONFIG_HOME")
	if configuration == "" {
		configuration = filepath.Join(home, ".config")
	}
	return filepath.Join(configuration, "systemd", "user", "agent-sessions.service")
}

//nolint:gocyclo // Snapshot, bounded rendering, atomic replacement, and exact rollback form one filesystem transaction.
func installHostServiceDefinition(sourceRoot, prefix, stateRoot string) (func() error, error) {
	asset := filepath.Join(sourceRoot, "deploy", "agent-sessions", "systemd", "user", "agent-sessions.service")
	if runtime.GOOS == "darwin" {
		asset = filepath.Join(sourceRoot, "deploy", "agent-sessions", "launchd", "net.antst.agent-sessions.plist")
	}
	body, err := os.ReadFile(asset) //nolint:gosec // Exact validated release service asset.
	if err != nil || len(body) == 0 || len(body) > 1024*1024 {
		return nil, errors.New("host service definition asset is missing or unbounded")
	}
	body = []byte(strings.ReplaceAll(strings.ReplaceAll(string(body), "@PREFIX@", prefix), "@STATE_ROOT@", stateRoot))
	if strings.Contains(string(body), "@PREFIX@") || strings.Contains(string(body), "@STATE_ROOT@") {
		return nil, errors.New("host service definition contains unresolved placeholders")
	}
	target := hostServiceDefinitionPath()
	snapshot := serviceDefinitionSnapshot{}
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1024*1024 {
			return nil, errors.New("existing host service definition changed filesystem type")
		}
		snapshot.present, snapshot.mode = true, info.Mode().Perm()
		snapshot.body, err = os.ReadFile(target) //nolint:gosec // Exact non-secret user service definition selected above.
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	if err := writeServiceDefinition(target, body, 0o600); err != nil {
		return nil, err
	}
	return func() error {
		if !snapshot.present {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return writeServiceDefinition(target, snapshot.body, snapshot.mode)
	}, nil
}

func writeServiceDefinition(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agent-sessions-service-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
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

type installedHostService struct {
	descriptor servicecontrol.RoleDescriptor
	controller *servicecontrol.Controller
	runner     servicecontrol.CommandRunner
}

func newInstalledHostService(prefix string) (*installedHostService, error) {
	descriptor, err := HostServiceRole(prefix)
	if err != nil {
		return nil, err
	}
	runner := servicecontrol.OSCommandRunner{}
	return &installedHostService{descriptor: descriptor, controller: servicecontrol.NewController(runner), runner: runner}, nil
}

// Restart implements releaseinstall.RoleService.
func (service *installedHostService) Restart(ctx context.Context) error {
	if runtime.GOOS == "linux" {
		if err := service.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("reload host user service definition: %w", err)
		}
		// systemctl reports nonzero when a newly installed unit has no failure
		// state to clear. Treat reset-failed as a scoped best-effort preflight;
		// the following restart remains the authoritative lifecycle result.
		_ = service.runner.Run(ctx, "systemctl", "--user", "reset-failed", service.descriptor.ServiceName)
		if err := service.controller.Enable(ctx, service.descriptor); err != nil {
			return err
		}
		return service.controller.Restart(ctx, service.descriptor)
	}
	_ = service.controller.Stop(ctx, service.descriptor)
	return service.controller.Enable(ctx, service.descriptor)
}

// Stop implements releaseinstall.RoleService.
func (service *installedHostService) Stop(ctx context.Context) error {
	return service.controller.Stop(ctx, service.descriptor)
}

// Disable implements releaseinstall.RoleService.
func (service *installedHostService) Disable(ctx context.Context) error {
	return service.controller.Disable(ctx, service.descriptor)
}

// Start implements releaseinstall.RoleService.
func (service *installedHostService) Start(ctx context.Context) error {
	if runtime.GOOS == "linux" {
		if err := service.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		_ = service.runner.Run(ctx, "systemctl", "--user", "reset-failed", service.descriptor.ServiceName)
	}
	return service.controller.Start(ctx, service.descriptor)
}

// Verify implements releaseinstall.RoleService.
func (*installedHostService) Verify(ctx context.Context) error {
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if _, err := QueryAdmin(ctx, "runtime.status"); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("prior host service did not recover after rollback: %w", last)
}
