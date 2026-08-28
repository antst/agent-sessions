package federation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/servicecontrol"
	"github.com/antst/agent-sessions/internal/statestore"
)

const maxHubLifecyclePlanBytes = 1024 * 1024

// HubServiceRole returns the independently selected central-hub service
// descriptor. The caller supplies the exact service-definition path so tests,
// package installation and platform integration share one descriptor.
func HubServiceRole(prefix, definitionPath, listen string) (servicecontrol.RoleDescriptor, error) {
	if !hubCleanAbsolutePath(prefix) {
		return servicecontrol.RoleDescriptor{}, errors.New("hub install prefix must be clean, absolute and non-root")
	}
	if !hubCleanAbsolutePath(definitionPath) {
		return servicecontrol.RoleDescriptor{}, errors.New("hub service definition path must be clean and absolute")
	}
	if err := validateHubServiceListen(listen); err != nil {
		return servicecontrol.RoleDescriptor{}, err
	}
	return servicecontrol.RoleDescriptor{
		Role: "hub", ServiceName: "agent-sessions-hub.service", Label: "net.antst.agent-sessions-hub",
		DefinitionPath:   definitionPath,
		Program:          filepath.Join(prefix, "libexec", "agent-sessions", "hub", "current", "bin", "agent-sessions-hub"),
		ProgramArguments: []string{"--listen", listen},
	}, nil
}

func hubServiceDefinitionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "net.antst.agent-sessions-hub.plist"), nil
	}
	configuration := os.Getenv("XDG_CONFIG_HOME")
	if configuration == "" {
		configuration = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configuration) {
		return "", errors.New("XDG_CONFIG_HOME must be absolute")
	}
	return filepath.Join(filepath.Clean(configuration), "systemd", "user", "agent-sessions-hub.service"), nil
}

func validateHubListen(listen string) error {
	if strings.TrimSpace(listen) == "" || strings.TrimSpace(listen) != listen ||
		strings.IndexFunc(listen, func(character rune) bool {
			return (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && !strings.ContainsRune(".:-[]", character)
		}) >= 0 {
		return errors.New("hub listen address must be non-empty and canonical")
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("hub listen address must use host:port form")
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort < 0 || numericPort > 65535 {
		return errors.New("hub listen port must be between 0 and 65535")
	}
	return nil
}

func validateHubServiceListen(listen string) error {
	if err := validateHubListen(listen); err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(listen)
	if port == "0" {
		return errors.New("installed hub listen port must be stable and nonzero")
	}
	return nil
}

func hubListenMatches(configured, actual string) bool {
	wantHost, wantPort, wantErr := net.SplitHostPort(configured)
	gotHost, gotPort, gotErr := net.SplitHostPort(actual)
	if wantErr != nil || gotErr != nil || wantPort != gotPort {
		return false
	}
	return wantHost == "" || strings.EqualFold(wantHost, gotHost)
}

type hubServiceDefinitionSnapshot struct {
	present  bool
	body     []byte
	mode     os.FileMode
	identity hubFileIdentity
}

func installHubServiceDefinition(sourceRoot, prefix, stateRoot, listen string) (func() error, error) {
	return installHubServiceDefinitionWithHooks(sourceRoot, prefix, stateRoot, listen, nil)
}

func installHubServiceDefinitionWithHooks(
	sourceRoot, prefix, stateRoot, listen string,
	hooks *hubDefinitionFSHooks,
) (func() error, error) {
	asset := filepath.Join(sourceRoot, "deploy", "agent-sessions-hub", "systemd", "user", "agent-sessions-hub.service")
	if runtime.GOOS == "darwin" {
		asset = filepath.Join(sourceRoot, "deploy", "agent-sessions-hub", "launchd", "net.antst.agent-sessions-hub.plist")
	}
	var afterSourceOpen func()
	if hooks != nil {
		afterSourceOpen = hooks.afterSourceOpen
	}
	body, _, _, err := readHubOwnedRegularFile(asset, maxHubLifecyclePlanBytes, afterSourceOpen)
	if err != nil || len(body) == 0 || len(body) > maxHubLifecyclePlanBytes {
		return nil, errors.New("hub service definition asset is missing or unbounded")
	}
	replacer := strings.NewReplacer("@PREFIX@", prefix, "@STATE_ROOT@", stateRoot, "@LISTEN@", listen)
	body = []byte(replacer.Replace(string(body)))
	if strings.Contains(string(body), "@PREFIX@") || strings.Contains(string(body), "@STATE_ROOT@") || strings.Contains(string(body), "@LISTEN@") {
		return nil, errors.New("hub service definition contains unresolved placeholders")
	}
	target, err := hubServiceDefinitionPath()
	if err != nil {
		return nil, err
	}
	var afterTargetOpen func()
	if hooks != nil {
		afterTargetOpen = hooks.afterTargetOpen
	}
	snapshot, err := snapshotHubServiceDefinition(target, afterTargetOpen)
	if err != nil {
		return nil, err
	}
	written, err := replaceHubServiceDefinition(target, body, 0o600, snapshot, hooks)
	if err != nil {
		return nil, err
	}
	return func() error {
		if !snapshot.present {
			return removeHubServiceDefinition(target, written, hooks)
		}
		_, err := replaceHubServiceDefinition(target, snapshot.body, snapshot.mode, written, hooks)
		return err
	}, nil
}

func writeHubFile(path string, body []byte, mode os.FileMode) error {
	snapshot, err := snapshotHubServiceDefinition(path, nil)
	if err != nil {
		return err
	}
	_, err = replaceHubServiceDefinition(path, body, mode, snapshot, nil)
	return err
}

type installedHubService struct {
	descriptor     servicecontrol.RoleDescriptor
	controller     *servicecontrol.Controller
	runner         servicecontrol.CommandRunner
	observeRuntime func(context.Context) (hubServiceObservation, error)
	enableService  func(context.Context) error
	restartService func(context.Context) error
	startService   func(context.Context) error
	stopService    func(context.Context) error
	readStatus     func(context.Context) (HubStatusProjection, error)
	processMatches func(HubStatusProjection) bool
	verifyTimeout  time.Duration
	verifyPoll     time.Duration
}

type hubServiceObservation struct {
	state  releaseinstall.RoleServiceState
	loaded bool
}

func (service *installedHubService) observeExact(ctx context.Context) (hubServiceObservation, error) {
	if service.observeRuntime != nil {
		return service.observeRuntime(ctx)
	}
	if runtime.GOOS == "darwin" {
		return service.observeDarwin(ctx)
	}
	return service.observeLinux(ctx)
}

func (service *installedHubService) observeDarwin(ctx context.Context) (hubServiceObservation, error) {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	command := exec.CommandContext(ctx, "launchctl", "print", domain+"/"+service.descriptor.Label) //nolint:gosec // Closed launchctl executable, current-user domain, and canonical descriptor label.
	output, runtimeErr := command.CombinedOutput()
	loaded := runtimeErr == nil
	if runtimeErr != nil {
		if ctx.Err() != nil {
			return hubServiceObservation{}, ctx.Err()
		}
		var exit *exec.ExitError
		if !errors.As(runtimeErr, &exit) || (exit.ExitCode() != 113 && !strings.Contains(string(output), "Could not find service")) {
			return hubServiceObservation{}, runtimeErr
		}
	}
	definition, err := snapshotHubServiceDefinition(service.descriptor.DefinitionPath, nil)
	if err != nil {
		return hubServiceObservation{}, errors.New("observe hub launchd definition identity")
	}
	if !definition.present {
		if loaded {
			return hubServiceObservation{}, errors.New("loaded hub launchd job has no exact service definition")
		}
		return hubServiceObservation{}, nil
	}
	disabledCommand := exec.CommandContext(ctx, "launchctl", "print-disabled", domain) //nolint:gosec // Closed launchctl executable and current-user domain.
	disabled, err := disabledCommand.Output()
	if err != nil {
		return hubServiceObservation{}, err
	}
	lower := strings.ToLower(string(output))
	return hubServiceObservation{
		loaded: loaded,
		state: releaseinstall.RoleServiceState{
			Enabled: !strings.Contains(string(disabled), `"`+service.descriptor.Label+`" => true`),
			Running: loaded && (strings.Contains(lower, "state = running") || strings.Contains(lower, "pid =")),
		},
	}, nil
}

func (service *installedHubService) observeLinux(ctx context.Context) (hubServiceObservation, error) {
	command := exec.CommandContext( //nolint:gosec // Closed systemctl executable and validated canonical service name.
		ctx, "systemctl", "--user", "show", service.descriptor.ServiceName,
		"--property=LoadState", "--property=UnitFileState", "--property=ActiveState", "--property=MainPID", "--no-pager",
	)
	output, err := command.Output()
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	if properties["LoadState"] == "not-found" {
		return hubServiceObservation{}, nil
	}
	if err != nil {
		return hubServiceObservation{}, err
	}
	enabled := properties["UnitFileState"] == "enabled" || properties["UnitFileState"] == "enabled-runtime" ||
		properties["UnitFileState"] == "linked" || properties["UnitFileState"] == "linked-runtime" ||
		properties["UnitFileState"] == "alias"
	active := properties["ActiveState"]
	running := active == "active" || active == "activating" || active == "reloading" ||
		active == "deactivating" || properties["MainPID"] != "" && properties["MainPID"] != "0"
	return hubServiceObservation{
		loaded: true, state: releaseinstall.RoleServiceState{Enabled: enabled, Running: running},
	}, nil
}

// Observe implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Observe(ctx context.Context) (releaseinstall.RoleServiceState, error) {
	observation, err := service.observeExact(ctx)
	return observation.state, err
}

// Reload implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Reload(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return service.runner.Run(ctx, "systemctl", "--user", "daemon-reload")
}

// Enable implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Enable(ctx context.Context) error {
	if service.enableService != nil {
		return service.enableService(ctx)
	}
	return service.controller.Enable(ctx, service.descriptor)
}

func (service *installedHubService) restartManagedService(ctx context.Context) error {
	if service.restartService != nil {
		return service.restartService(ctx)
	}
	return service.controller.Restart(ctx, service.descriptor)
}

func (service *installedHubService) startManagedService(ctx context.Context) error {
	if service.startService != nil {
		return service.startService(ctx)
	}
	return service.controller.Start(ctx, service.descriptor)
}

func (service *installedHubService) stopManagedService(ctx context.Context) error {
	if service.stopService != nil {
		return service.stopService(ctx)
	}
	return service.controller.Stop(ctx, service.descriptor)
}

func (service *installedHubService) readHubStatus(ctx context.Context) (HubStatusProjection, error) {
	if service.readStatus != nil {
		return service.readStatus(ctx)
	}
	return ReadHubStatus(ctx)
}

func (service *installedHubService) hubProcessMatches(status HubStatusProjection) bool {
	if service.processMatches != nil {
		return service.processMatches(status)
	}
	return hubStatusProcessMatches(status)
}

func (service *installedHubService) verificationWindow() (time.Duration, time.Duration) {
	timeout, poll := service.verifyTimeout, service.verifyPoll
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	return timeout, poll
}

func newInstalledHubService(prefix, listen string) (*installedHubService, error) {
	definition, err := hubServiceDefinitionPath()
	if err != nil {
		return nil, err
	}
	descriptor, err := HubServiceRole(prefix, definition, listen)
	if err != nil {
		return nil, err
	}
	runner := servicecontrol.OSCommandRunner{}
	return &installedHubService{descriptor: descriptor, controller: servicecontrol.NewController(runner), runner: runner}, nil
}

// Restart implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Restart(ctx context.Context) error {
	if runtime.GOOS == "linux" {
		if err := service.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("reload hub user service definition: %w", err)
		}
		_ = service.runner.Run(ctx, "systemctl", "--user", "reset-failed", service.descriptor.ServiceName)
		if err := service.Enable(ctx); err != nil {
			return err
		}
		restartErr := service.restartManagedService(ctx)
		if restartErr == nil {
			return nil
		}
		observation, observeErr := service.observeExact(ctx)
		if observeErr == nil && observation.state.Running {
			return nil
		}
		return errors.Join(restartErr, observeErr)
	}
	return service.restartDarwin(ctx)
}

func (service *installedHubService) restartDarwin(ctx context.Context) error {
	if err := service.Enable(ctx); err != nil {
		return err
	}
	observation, observeErr := service.observeExact(ctx)
	if observeErr != nil {
		return observeErr
	}
	if observation.loaded {
		stopErr := service.stopManagedService(ctx)
		if stopErr != nil {
			after, afterErr := service.observeExact(ctx)
			if afterErr != nil {
				return errors.Join(stopErr, afterErr)
			}
			if after.loaded {
				return stopErr
			}
		}
	}
	startErr := service.startManagedService(ctx)
	if startErr == nil {
		return nil
	}
	after, afterErr := service.observeExact(ctx)
	if afterErr == nil && after.state.Running {
		return nil
	}
	return errors.Join(startErr, afterErr)
}

// Stop implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Stop(ctx context.Context) error {
	observation, err := service.observeExact(ctx)
	if err != nil {
		return err
	}
	if !observation.loaded {
		return nil
	}
	stopErr := service.stopManagedService(ctx)
	if stopErr == nil {
		return nil
	}
	after, observeErr := service.observeExact(ctx)
	if observeErr == nil && !after.loaded {
		return nil
	}
	return errors.Join(stopErr, observeErr)
}

// Disable implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Disable(ctx context.Context) error {
	return service.controller.Disable(ctx, service.descriptor)
}

// Start implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Start(ctx context.Context) error {
	if runtime.GOOS == "linux" {
		if err := service.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		_ = service.runner.Run(ctx, "systemctl", "--user", "reset-failed", service.descriptor.ServiceName)
	}
	startErr := service.startManagedService(ctx)
	if startErr == nil {
		return nil
	}
	state, observeErr := service.Observe(ctx)
	if observeErr == nil && state.Running {
		return nil
	}
	if observeErr != nil {
		return errors.Join(startErr, observeErr)
	}
	return startErr
}

// Verify implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) Verify(ctx context.Context) error {
	want, identityErr := hubExecutableSHA256(service.descriptor.Program)
	if identityErr != nil {
		return identityErr
	}
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		status, err := ReadHubStatus(ctx)
		if err == nil && hubStatusProcessMatches(status) && status.RuntimeIdentity == want &&
			hubListenMatches(service.descriptor.ProgramArguments[1], status.Listener) && status.ProtocolVersion == ProtocolVersion {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("prior hub service did not recover after rollback: %w", last)
}

// VerifyCandidate implements the shared releaseinstall.RoleService transaction.
func (service *installedHubService) VerifyCandidate(
	ctx context.Context,
	release releaseinstall.InstalledRelease,
) error {
	want, err := hubExecutableSHA256(release.Executable)
	if err != nil {
		return err
	}
	timeout, poll := service.verificationWindow()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		status, statusErr := service.readHubStatus(ctx)
		if statusErr == nil && service.hubProcessMatches(status) && status.RuntimeVersion == release.Version &&
			status.RuntimeIdentity == want && hubListenMatches(service.descriptor.ProgramArguments[1], status.Listener) &&
			status.ProtocolVersion == ProtocolVersion {
			return nil
		}
		if statusErr == nil {
			statusErr = errors.New("running hub service does not match the selected candidate release")
		}
		last = statusErr
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	return fmt.Errorf("candidate hub service did not publish exact readiness: %w", last)
}

type hubInstallHooks struct {
	prefix, listen, stateRoot, configurationRoot string
	restoreDefinition                            func() error
	configurationStore                           *statestore.Store
	configurationPrior                           HubConfiguration
	configurationWrittenRevision                 statestore.Revision
	configurationPresent                         bool
}

// Prepare installs reversible hub-only configuration and service assets.
func (hooks *hubInstallHooks) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	if _, err := openHubStore(hooks.stateRoot); err != nil {
		return err
	}
	logRoot := filepath.Join(hooks.stateRoot, "logs")
	if err := os.Mkdir(logRoot, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	logInfo, err := os.Lstat(logRoot)
	if err != nil || !logInfo.IsDir() || logInfo.Mode()&os.ModeSymlink != 0 || logInfo.Mode().Perm() != 0o700 {
		return errors.New("hub service log root is not an owner-only real directory")
	}
	store, err := openHubStore(hooks.configurationRoot)
	if err != nil {
		return err
	}
	var prior HubConfiguration
	priorRevision, readErr := store.Read(ctx, hubConfigurationRecord, &prior)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	writtenRevision, err := store.CompareAndSwap(
		ctx, hubConfigurationRecord, priorRevision, HubConfiguration{SchemaVersion: 1, Listen: hooks.listen},
	)
	if err != nil {
		return err
	}
	hooks.configurationStore = store
	hooks.configurationPrior = prior
	hooks.configurationWrittenRevision = writtenRevision
	hooks.configurationPresent = readErr == nil

	restore, err := installHubServiceDefinition(request.SourceRoot, hooks.prefix, hooks.stateRoot, hooks.listen)
	if err != nil {
		_ = hooks.restoreConfiguration(ctx)
		return err
	}
	hooks.restoreDefinition = restore
	return nil
}

// Ready proves the selected hub's exact release identity and listener.
func (hooks *hubInstallHooks) Ready(ctx context.Context, release releaseinstall.InstalledRelease) error {
	want, err := hubExecutableSHA256(release.Executable)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		status, statusErr := ReadHubStatus(ctx)
		if statusErr == nil && hubStatusProcessMatches(status) &&
			status.RuntimeVersion == release.Version && status.RuntimeIdentity == want &&
			hubListenMatches(hooks.listen, status.Listener) && status.ProtocolVersion == ProtocolVersion {
			return nil
		}
		if statusErr == nil {
			statusErr = errors.New("running hub identity does not match selected release")
		}
		last = statusErr
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("hub service did not publish exact readiness: %w", last)
}

// Commit completes the reversible hub hook transaction.
func (*hubInstallHooks) Commit(context.Context) error { return nil }

// Rollback restores the prior hub configuration and service definition.
func (hooks *hubInstallHooks) Rollback(ctx context.Context) error {
	var result error
	if hooks.restoreDefinition != nil {
		result = errors.Join(result, hooks.restoreDefinition())
	}
	result = errors.Join(result, hooks.restoreConfiguration(ctx))
	return result
}

// Remove deletes only the hub service definition; config/state are preserved.
func (*hubInstallHooks) Remove(context.Context) error {
	path, err := hubServiceDefinitionPath()
	if err != nil {
		return err
	}
	snapshot, err := snapshotHubServiceDefinition(path, nil)
	if err != nil {
		return err
	}
	return removeHubServiceDefinition(path, snapshot, nil)
}

func (hooks *hubInstallHooks) restoreConfiguration(ctx context.Context) error {
	if hooks.configurationStore == nil || hooks.configurationWrittenRevision == 0 {
		return nil
	}
	if hooks.configurationPresent {
		_, err := hooks.configurationStore.CompareAndSwap(
			ctx, hubConfigurationRecord, hooks.configurationWrittenRevision, hooks.configurationPrior,
		)
		return err
	}
	path := hooks.configurationStore.RecordPath(hubConfigurationRecord)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("hub configuration record changed filesystem type during rollback")
	}
	return os.Remove(path)
}

type emptyHubHooks struct{}

// Prepare is an intentional offline purge no-op.
func (emptyHubHooks) Prepare(context.Context, releaseinstall.InstallRequest) error { return nil }

// Ready is an intentional offline purge no-op.
func (emptyHubHooks) Ready(context.Context, releaseinstall.InstalledRelease) error { return nil }

// Commit is an intentional offline purge no-op.
func (emptyHubHooks) Commit(context.Context) error { return nil }

// Rollback is an intentional offline purge no-op.
func (emptyHubHooks) Rollback(context.Context) error { return nil }

// Remove is an intentional offline purge no-op.
func (emptyHubHooks) Remove(context.Context) error { return nil }

// RunHubInstallCLI performs the hub-only immutable install/upgrade selected by
// make install-hub.
func RunHubInstallCLI(ctx context.Context, args []string) error {
	values, err := parseHubLifecycleOptions(args, true)
	if err != nil {
		return err
	}
	if values["--role"] != "hub" {
		return errors.New("hub installer accepts only --role hub")
	}
	prefix, sourceRoot, version, listen := values["--prefix"], values["--source-root"], values["--version"], values["--listen"]
	if !hubCleanAbsolutePath(prefix) {
		return errors.New("hub prefix must be a clean absolute non-root path")
	}
	if err := validateHubServiceListen(listen); err != nil {
		return err
	}
	paths, err := ResolveHubPaths()
	if err != nil {
		return err
	}
	hooks := &hubInstallHooks{
		prefix: prefix, listen: listen, stateRoot: paths.StateRoot, configurationRoot: paths.ConfigurationRoot,
	}
	engine, err := newHubEngine(prefix, listen, paths, hooks)
	if err != nil {
		return err
	}
	return recoverThenInstallHubSource(ctx, engine, sourceRoot, version)
}

type hubReleaseTransaction interface {
	Recover(context.Context) error
	Install(context.Context, releaseinstall.InstallRequest) (releaseinstall.InstallResult, error)
}

func recoverThenInstallHubSource(
	ctx context.Context,
	engine hubReleaseTransaction,
	sourceRoot string,
	version string,
) error {
	// Recovery is authoritative over the already selected immutable release and
	// must finish before this invocation observes any bytes from its new source.
	if err := engine.Recover(ctx); err != nil {
		return fmt.Errorf("recover hub install transaction: %w", err)
	}
	if !hubCleanAbsolutePath(sourceRoot) {
		return errors.New("hub source root must be a clean absolute non-root path")
	}
	identity, err := releaseinstall.ContentIdentity(sourceRoot)
	if err != nil {
		return err
	}
	_, err = engine.Install(ctx, releaseinstall.InstallRequest{
		Version: version, ContentIdentity: identity, SourceRoot: sourceRoot, Executable: "agent-sessions-hub",
	})
	return err
}

// RunHubRemoveCLI removes only the hub service and hub release selection while
// preserving its configuration and durable state.
func RunHubRemoveCLI(ctx context.Context, args []string) error {
	values, err := parseHubLifecycleOptions(args, false)
	if err != nil {
		return err
	}
	if values["--role"] != "hub" {
		return errors.New("hub remover accepts only --role hub")
	}
	prefix := values["--prefix"]
	if !hubCleanAbsolutePath(prefix) {
		return errors.New("hub prefix must be a clean absolute non-root path")
	}
	paths, err := ResolveHubPaths()
	if err != nil {
		return err
	}
	engine, err := newHubEngine(prefix, values["--listen"], paths, &hubInstallHooks{})
	if err != nil {
		return err
	}
	return engine.Remove(ctx)
}

func newHubEngine(prefix, listen string, paths HubPaths, hooks releaseinstall.RoleHooks) (*releaseinstall.Engine, error) {
	if listen == "" {
		listen = ":7419"
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHub)
	if err != nil {
		return nil, err
	}
	service, err := newInstalledHubService(prefix, listen)
	if err != nil {
		return nil, err
	}
	return releaseinstall.NewEngine(releaseinstall.EngineOptions{
		Layout: layout, Service: service, Hooks: hooks,
		PurgeTargets: []string{paths.ConfigurationRoot, paths.StateRoot},
	})
}

func parseHubLifecycleOptions(args []string, install bool) (map[string]string, error) {
	allowed := map[string]bool{"--role": true, "--prefix": true}
	required := []string{"--role", "--prefix"}
	if install {
		allowed["--source-root"], allowed["--version"], allowed["--listen"] = true, true, true
		required = append(required, "--source-root", "--version", "--listen")
	}
	values := map[string]string{}
	for len(args) != 0 {
		if len(args) < 2 || !allowed[args[0]] || args[1] == "" || values[args[0]] != "" {
			return nil, errors.New("invalid hub lifecycle arguments")
		}
		values[args[0]], args = args[1], args[2:]
	}
	for _, name := range required {
		if values[name] == "" {
			return nil, fmt.Errorf("hub lifecycle requires %s", name)
		}
	}
	return values, nil
}

// HubRemovalInspection is the exact normal-removal ownership projection.
type HubRemovalInspection struct {
	Role      string   `json:"role"`
	Blockers  []string `json:"blockers"`
	Targets   []string `json:"targets"`
	Preserved []string `json:"preserved"`
}

// InspectHubRemoval returns a metadata-only plan without starting the hub.
func InspectHubRemoval(prefix string) (HubRemovalInspection, error) {
	if !hubCleanAbsolutePath(prefix) {
		return HubRemovalInspection{}, errors.New("hub prefix must be a clean absolute non-root path")
	}
	paths, err := ResolveHubPaths()
	if err != nil {
		return HubRemovalInspection{}, err
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHub)
	if err != nil {
		return HubRemovalInspection{}, err
	}
	definition, err := hubServiceDefinitionPath()
	if err != nil {
		return HubRemovalInspection{}, err
	}
	return HubRemovalInspection{
		Role: "hub", Blockers: []string{},
		Targets:   []string{definition, layout.CurrentSelection, layout.ReleasesRoot},
		Preserved: []string{paths.ConfigurationRoot, paths.StateRoot},
	}, nil
}

// HubPurgeInspection is the public projection of one exact private plan.
type HubPurgeInspection struct {
	Role         string   `json:"role"`
	PlanRevision string   `json:"plan_revision"`
	Targets      []string `json:"targets"`
	Exclusions   []string `json:"exclusions"`
}

// HubPurgeApplyResult is the public committed purge projection.
type HubPurgeApplyResult struct {
	Role         string   `json:"role"`
	PlanRevision string   `json:"plan_revision"`
	Deleted      []string `json:"deleted"`
}

// RunHubPurgeInspectCLI writes one revision-bound offline hub-only purge plan.
func RunHubPurgeInspectCLI(ctx context.Context, prefix, planPath string) (HubPurgeInspection, error) {
	engine, layout, err := newHubPurgeEngine(prefix)
	if err != nil {
		return HubPurgeInspection{}, err
	}
	if err := requireHubRemoved(layout); err != nil {
		return HubPurgeInspection{}, err
	}
	plan, err := engine.PlanPurge(ctx)
	if err != nil {
		return HubPurgeInspection{}, err
	}
	if err := writeHubPurgePlan(planPath, plan); err != nil {
		return HubPurgeInspection{}, err
	}
	return HubPurgeInspection{Role: string(plan.Role), PlanRevision: plan.Revision, Targets: append([]string(nil), plan.Targets...), Exclusions: append([]string(nil), plan.Exclusions...)}, nil
}

// RunHubPurgeApplyCLI applies only a still-current offline hub-only plan.
func RunHubPurgeApplyCLI(ctx context.Context, prefix, planPath string) (HubPurgeApplyResult, error) {
	engine, layout, err := newHubPurgeEngine(prefix)
	if err != nil {
		return HubPurgeApplyResult{}, err
	}
	if err := requireHubRemoved(layout); err != nil {
		return HubPurgeApplyResult{}, err
	}
	plan, err := readHubPurgePlan(planPath)
	if err != nil {
		return HubPurgeApplyResult{}, err
	}
	if plan.Role != releaseinstall.RoleHub {
		return HubPurgeApplyResult{}, errors.New("purge plan is not owned by the hub role")
	}
	if err := engine.ApplyPurge(ctx, plan); err != nil {
		return HubPurgeApplyResult{}, err
	}
	return HubPurgeApplyResult{Role: string(plan.Role), PlanRevision: plan.Revision, Deleted: append([]string(nil), plan.Targets...)}, nil
}

func newHubPurgeEngine(prefix string) (*releaseinstall.Engine, releaseinstall.RoleLayout, error) {
	if prefix == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, releaseinstall.RoleLayout{}, err
		}
		prefix = filepath.Join(home, ".local")
	}
	paths, err := ResolveHubPaths()
	if err != nil {
		return nil, releaseinstall.RoleLayout{}, err
	}
	engine, err := newHubEngine(prefix, ":7419", paths, emptyHubHooks{})
	if err != nil {
		return nil, releaseinstall.RoleLayout{}, err
	}
	layout, _ := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHub)
	return engine, layout, nil
}

func requireHubRemoved(layout releaseinstall.RoleLayout) error {
	definition, err := hubServiceDefinitionPath()
	if err != nil {
		return err
	}
	for label, path := range map[string]string{"hub release selection": layout.CurrentSelection, "hub service definition": definition} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%s still exists at %s; run normal hub removal first", label, path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writeHubPurgePlan(path string, plan releaseinstall.PurgePlan) error {
	if !hubCleanAbsolutePath(path) {
		return errors.New("hub purge plan path must be clean, absolute, and non-root")
	}
	for _, target := range plan.Targets {
		if hubPathWithin(path, target) {
			return errors.New("hub purge plan file must be outside every purge target")
		}
	}
	body, err := json.Marshal(plan)
	if err != nil || len(body) > maxHubLifecyclePlanBytes {
		return errors.New("hub purge plan exceeds its bound")
	}
	return writeHubFile(path, body, 0o600)
}

func readHubPurgePlan(path string) (releaseinstall.PurgePlan, error) {
	if !hubCleanAbsolutePath(path) {
		return releaseinstall.PurgePlan{}, errors.New("hub purge plan path must be clean, absolute, and non-root")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxHubLifecyclePlanBytes {
		return releaseinstall.PurgePlan{}, errors.New("hub purge plan is not a bounded real regular file")
	}
	body, err := os.ReadFile(path) //nolint:gosec // Explicit bounded operator-selected lifecycle plan.
	if err != nil {
		return releaseinstall.PurgePlan{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var plan releaseinstall.PurgePlan
	if err := decoder.Decode(&plan); err != nil {
		return releaseinstall.PurgePlan{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return releaseinstall.PurgePlan{}, errors.New("hub purge plan contains trailing JSON")
	}
	return plan, nil
}

func hubExecutableSHA256(path string) (string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // Exact selected immutable executable.
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// CurrentHubRuntimeIdentity returns the exact content identity of this hub
// executable for readiness publication.
func CurrentHubRuntimeIdentity() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	return hubExecutableSHA256(executable)
}

func hubCleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func hubPathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
