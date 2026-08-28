package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/servicecontrol"
)

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

func installHostServiceDefinition(sourceRoot, prefix, stateRoot string) (func() error, error) {
	return installHostServiceDefinitionWithHooks(sourceRoot, prefix, stateRoot, nil)
}

func ensureHostServiceLogRoot(stateRoot string) error {
	logRoot := filepath.Join(stateRoot, "logs")
	directory, err := openDaemonDirectory(logRoot, true, 0o700)
	if err != nil {
		return fmt.Errorf("open host service log root: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := requireCurrentUserOwnedDirectory(directory); err != nil {
		return errors.New("host service log root is not an owner-only real directory")
	}
	info, err := directory.Stat()
	if err != nil || info.Mode().Perm() != 0o700 {
		return errors.New("host service log root is not an owner-only real directory")
	}
	return verifyHostSurfaceDirectoryPath(logRoot, directory)
}

func installHostServiceDefinitionWithHooks(
	sourceRoot, prefix, stateRoot string, mutationHooks *hostSurfaceMutationHooks,
) (func() error, error) {
	asset := filepath.Join(sourceRoot, "deploy", "agent-sessions", "systemd", "user", "agent-sessions.service")
	if runtime.GOOS == "darwin" {
		asset = filepath.Join(sourceRoot, "deploy", "agent-sessions", "launchd", "net.antst.agent-sessions.plist")
	}
	body, _, err := readDaemonBoundedRegularFileSnapshot(asset, maxHostSurfaceServiceBytes, nil, true, nil)
	if err != nil || len(body) == 0 {
		return nil, errors.New("host service definition asset is missing or unbounded")
	}
	body = []byte(strings.ReplaceAll(strings.ReplaceAll(string(body), "@PREFIX@", prefix), "@STATE_ROOT@", stateRoot))
	if len(body) > maxHostSurfaceServiceBytes || strings.Contains(string(body), "@PREFIX@") ||
		strings.Contains(string(body), "@STATE_ROOT@") {
		return nil, errors.New("host service definition contains unresolved placeholders")
	}
	target := hostServiceDefinitionPath()
	snapshot, err := snapshotHostSurfaceService(target, nil)
	if err != nil {
		return nil, err
	}
	expected := &hostSurfaceEntryExpectation{present: snapshot.Present, identity: snapshot.observed}
	if err := replaceHostSurfaceRegularFileExpected(target, body, 0o600, 0o700, mutationHooks, expected); err != nil {
		return nil, errors.Join(err, restoreHostSurfaceService(snapshot, nil))
	}
	return func() error { return restoreHostSurfaceService(snapshot, nil) }, nil
}

type installedHostService struct {
	descriptor servicecontrol.RoleDescriptor
	controller *servicecontrol.Controller
	runner     servicecontrol.CommandRunner
	activity   func(context.Context, string, string) (string, error)
}

// Observe implements releaseinstall.RoleService.
func (service *installedHostService) Observe(ctx context.Context) (releaseinstall.RoleServiceState, error) {
	if runtime.GOOS == "darwin" {
		return observeInstalledLaunchdService(ctx, service.descriptor)
	}
	return observeInstalledSystemdService(ctx, service.descriptor)
}

// Reload implements releaseinstall.RoleService.
func (service *installedHostService) Reload(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return service.runner.Run(ctx, "systemctl", "--user", "daemon-reload")
}

// Enable implements releaseinstall.RoleService.
func (service *installedHostService) Enable(ctx context.Context) error {
	return service.controller.Enable(ctx, service.descriptor)
}

func newInstalledHostService(prefix string) (*installedHostService, error) {
	descriptor, err := HostServiceRole(prefix)
	if err != nil {
		return nil, err
	}
	runner := servicecontrol.OSCommandRunner{}
	return &installedHostService{
		descriptor: descriptor, controller: servicecontrol.NewController(runner), runner: runner,
		activity: productionLegacyServiceActivity,
	}, nil
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
	if err := service.Stop(ctx); err != nil {
		return err
	}
	if err := service.controller.Enable(ctx, service.descriptor); err != nil {
		return err
	}
	return service.controller.Start(ctx, service.descriptor)
}

// Stop implements releaseinstall.RoleService.
func (service *installedHostService) Stop(ctx context.Context) error {
	stopErr := service.controller.Stop(ctx, service.descriptor)
	if stopErr == nil {
		return nil
	}
	observe := service.activity
	if observe == nil {
		observe = productionLegacyServiceActivity
	}
	manager, unit := installedHostServiceIdentity(service.descriptor)
	activity, observeErr := observe(ctx, manager, unit)
	if observeErr == nil && (activity == "absent" || activity == "inactive") {
		return nil
	}
	if observeErr != nil {
		return errors.Join(stopErr, fmt.Errorf("reobserve failed host service stop: %w", observeErr))
	}
	return stopErr
}

func installedHostServiceIdentity(descriptor servicecontrol.RoleDescriptor) (string, string) {
	if runtime.GOOS == "darwin" {
		return "launchd-user", descriptor.Label
	}
	return "systemd-user", descriptor.ServiceName
}

func observeInstalledSystemdService(
	ctx context.Context,
	descriptor servicecontrol.RoleDescriptor,
) (releaseinstall.RoleServiceState, error) {
	command := exec.CommandContext(ctx, "systemctl", "--user", "show", descriptor.ServiceName, //nolint:gosec // HostServiceRole validated the closed unit name.
		"--property=LoadState", "--property=UnitFileState", "--property=ActiveState", "--property=MainPID", "--no-pager")
	output, err := command.Output()
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	if properties["LoadState"] == "not-found" {
		return releaseinstall.RoleServiceState{}, nil
	}
	if err != nil {
		return releaseinstall.RoleServiceState{}, fmt.Errorf("observe host systemd service: %w", err)
	}
	enabled := properties["UnitFileState"] == "enabled" || properties["UnitFileState"] == "enabled-runtime" ||
		properties["UnitFileState"] == "linked" || properties["UnitFileState"] == "linked-runtime" ||
		properties["UnitFileState"] == "alias"
	active := properties["ActiveState"]
	running := active == "active" || active == "activating" || active == "reloading" ||
		active == "deactivating" || properties["MainPID"] != "" && properties["MainPID"] != "0"
	return releaseinstall.RoleServiceState{Enabled: enabled, Running: running}, nil
}

func observeInstalledLaunchdService(
	ctx context.Context,
	descriptor servicecontrol.RoleDescriptor,
) (releaseinstall.RoleServiceState, error) {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	output, runtimeErr := exec.CommandContext(ctx, "launchctl", "print", domain+"/"+descriptor.Label).Output() //nolint:gosec // Closed descriptor label.
	loaded := runtimeErr == nil
	if runtimeErr != nil && !launchdServiceNotFound(runtimeErr) {
		return releaseinstall.RoleServiceState{}, fmt.Errorf("observe host launchd runtime: %w", runtimeErr)
	}
	definition, err := os.Lstat(descriptor.DefinitionPath)
	if os.IsNotExist(err) {
		if loaded {
			return releaseinstall.RoleServiceState{}, errors.New("loaded host launchd job has no exact service definition")
		}
		return releaseinstall.RoleServiceState{}, nil
	}
	if err != nil || !definition.Mode().IsRegular() || definition.Mode()&os.ModeSymlink != 0 {
		return releaseinstall.RoleServiceState{}, errors.New("observe host launchd definition identity")
	}
	disabledOutput, err := exec.CommandContext(ctx, "launchctl", "print-disabled", domain).Output() //nolint:gosec // Closed current-user domain.
	if err != nil {
		return releaseinstall.RoleServiceState{}, fmt.Errorf("observe host launchd enablement: %w", err)
	}
	disabledNeedle := `"` + descriptor.Label + `" => true`
	enabled := !strings.Contains(string(disabledOutput), disabledNeedle)
	lower := strings.ToLower(string(output))
	running := strings.Contains(lower, "state = running") || strings.Contains(lower, "pid =")
	return releaseinstall.RoleServiceState{Enabled: enabled, Running: running}, nil
}

func launchdServiceNotFound(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	if exit.ExitCode() == 113 {
		return true
	}
	return strings.Contains(strings.ToLower(string(exit.Stderr)), "could not find service")
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
	startErr := service.controller.Start(ctx, service.descriptor)
	if startErr == nil {
		return nil
	}
	state, observeErr := service.Observe(ctx)
	if observeErr == nil && state.Running {
		return nil
	}
	if observeErr != nil {
		return errors.Join(startErr, fmt.Errorf("reobserve host service start: %w", observeErr))
	}
	return startErr
}

// Verify implements releaseinstall.RoleService.

// VerifyCandidate implements releaseinstall.RoleService.
func (*installedHostService) VerifyCandidate(
	ctx context.Context,
	release releaseinstall.InstalledRelease,
) error {
	return verifyHostCandidateWithQuery(ctx, release, QueryAdmin)
}

func verifyHostCandidateWithQuery(
	ctx context.Context,
	release releaseinstall.InstalledRelease,
	query func(context.Context, string) (json.RawMessage, error),
) error {
	want, err := executableSHA256(release.Executable)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var last error
	for {
		body, queryErr := query(bounded, "runtime.status")
		if queryErr == nil {
			var status HostStatusProjection
			queryErr = json.Unmarshal(body, &status)
			if queryErr == nil && status.RuntimeVersion == release.Version && status.RuntimeIdentity == want && status.Generation > 0 {
				return nil
			}
			if queryErr == nil {
				queryErr = errors.New("running host service does not match the selected candidate release")
			}
		}
		last = queryErr
		select {
		case <-bounded.Done():
			return fmt.Errorf("selected candidate host service did not become ready: %w", errors.Join(last, bounded.Err()))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Verify implements releaseinstall.RoleService for the restored selected release.
func (service *installedHostService) Verify(ctx context.Context) error {
	want, identityErr := executableSHA256(service.descriptor.Program)
	if identityErr != nil {
		return identityErr
	}
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		body, err := QueryAdmin(ctx, "runtime.status")
		if err == nil {
			var status HostStatusProjection
			err = json.Unmarshal(body, &status)
			if err == nil && status.RuntimeIdentity == want && status.Generation > 0 {
				return nil
			}
			if err == nil {
				err = errors.New("running host service does not match the restored release")
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("prior host service did not recover after rollback: %w", last)
}
