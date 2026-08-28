package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

// RunHostInstallCLI performs the offline host-role invocation used by
// install-all. It cannot select, install, or restart the hub role.
func RunHostInstallCLI(ctx context.Context, args []string) error {
	values, err := parseHostInstallOptions(args)
	if err != nil {
		return err
	}
	if values["--role"] != "host" {
		return errors.New("host installer accepts only --role host")
	}
	sourceRoot, prefix, version := values["--source-root"], values["--prefix"], values["--version"]
	if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix || prefix == string(filepath.Separator) {
		return errors.New("host install prefix must be a clean absolute non-root path")
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		return err
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHost)
	if err != nil {
		return err
	}
	hooks, err := NewDurableHostInstallHooks(nil, filepath.Join(layout.TransactionRoot, "connectors.json"))
	if err != nil {
		return err
	}
	service, err := newInstalledHostService(prefix)
	if err != nil {
		return err
	}
	lifecycle, err := NewHostInstallLifecycle(hooks, hostInstallReadiness(paths.ControlEndpoint))
	if err != nil {
		return err
	}
	roleHooks := &hostInstallRoleHooks{
		lifecycle: lifecycle,
		prefix:    prefix, paths: paths, stateRoot: paths.StateRoot, runtimeEndpoint: paths.ControlEndpoint,
	}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{Layout: layout, Service: service, Hooks: roleHooks})
	if err != nil {
		return err
	}
	return recoverThenInstallHostSource(ctx, engine, sourceRoot, version, func() error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		drivers, err := NewNativeConnectorDrivers(NativeConnectorOptions{
			CodexExecutable: values["--codex"], ClaudeExecutable: values["--claude"],
			GrokExecutable: values["--grok"], QwenExecutable: values["--qwen"],
			GrokUserPluginRoot: filepath.Join(home, ".grok", "plugins", "agent-sessions"),
		})
		if err != nil {
			return err
		}
		roleHooks.productExecutables, err = resolveInstalledProductExecutables(values)
		if err != nil {
			return err
		}
		return hooks.ConfigureDrivers(drivers)
	})
}

type hostReleaseTransaction interface {
	Recover(context.Context) error
	Install(context.Context, releaseinstall.InstallRequest) (releaseinstall.InstallResult, error)
}

func recoverThenInstallHostSource(
	ctx context.Context,
	engine hostReleaseTransaction,
	sourceRoot string,
	version string,
	configureDrivers func() error,
) error {
	// Recovery is authority over an unfinished prior transaction. It uses the
	// selected immutable release and durable connector provenance, and must
	// finish before this invocation reads its new source or resolves its native
	// product executables.
	if err := engine.Recover(ctx); err != nil {
		return fmt.Errorf("recover host install transaction: %w", err)
	}
	if !filepath.IsAbs(sourceRoot) || filepath.Clean(sourceRoot) != sourceRoot || sourceRoot == string(filepath.Separator) {
		return errors.New("host install source root must be a clean absolute non-root path")
	}
	identity, err := releaseinstall.ContentIdentity(sourceRoot)
	if err != nil {
		return err
	}
	if configureDrivers == nil {
		return errors.New("host install native connector configuration is unavailable")
	}
	if err := configureDrivers(); err != nil {
		return err
	}
	_, err = engine.Install(ctx, releaseinstall.InstallRequest{
		Version: version, ContentIdentity: identity, SourceRoot: sourceRoot, Executable: "agent-sessions",
	})
	return err
}

type hostInstallRoleHooks struct {
	lifecycle          *HostInstallLifecycle
	prefix             string
	paths              ProductionPaths
	stateRoot          string
	runtimeEndpoint    string
	productExecutables map[string]string
	restoreService     func() error
	restoreAliases     func() error
}

// Preflight is intentionally empty. The unreleased split runtime has no
// compatibility path; the operator supplies a clean Agent Sessions state root.
func (hooks *hostInstallRoleHooks) Preflight(
	context.Context,
	releaseinstall.InstallRequest,
) error {
	return nil
}

// Prepare implements releaseinstall.RoleHooks under the role install lock.
func (hooks *hostInstallRoleHooks) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	if hooks == nil {
		return errors.New("host install role hooks require a lifecycle")
	}
	surface, err := captureHostSurfaceRollback(hooks.prefix, hooks.stateRoot)
	if err != nil {
		return err
	}
	if err := saveHostSurfaceRollback(surface); err != nil {
		return err
	}
	if err := writeInstalledProductConfiguration(hooks.paths, hooks.productExecutables); err != nil {
		return errors.Join(err, hooks.rollbackSurface())
	}
	if err := hooks.lifecycle.Prepare(ctx, request); err != nil {
		return errors.Join(err, hooks.rollbackSurface())
	}
	if err := ensureHostServiceLogRoot(hooks.stateRoot); err != nil {
		return errors.Join(err, hooks.lifecycle.Rollback(ctx))
	}
	restoreService, err := installHostServiceDefinition(request.SourceRoot, hooks.prefix, hooks.stateRoot)
	if err != nil {
		return errors.Join(err, hooks.lifecycle.Rollback(ctx))
	}
	hooks.restoreService = restoreService
	restoreAliases, err := installHostAliases(hooks.prefix)
	if err != nil {
		return errors.Join(err, hooks.rollbackSurface(), hooks.lifecycle.Rollback(ctx))
	}
	hooks.restoreAliases = restoreAliases
	return nil
}

func resolveInstalledProductExecutables(values map[string]string) (map[string]string, error) {
	selected := map[string]string{
		"codex": values["--codex"], "claude": values["--claude"],
		"grok": values["--grok"], "qwen": values["--qwen"],
	}
	resolved := make(map[string]string, len(selected))
	for _, product := range productcatalog.Catalog().Products {
		candidate := selected[product.ID]
		path, err := exec.LookPath(candidate)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolve %s executable: %w", product.ID, err)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("resolved %s executable is not absolute", product.ID)
		}
		resolved[product.ID] = filepath.Clean(path)
	}
	return resolved, nil
}

func writeInstalledProductConfiguration(paths ProductionPaths, executables map[string]string) error {
	configuration, err := LoadDaemonConfig(paths.ConfigurationFile, paths)
	if err != nil {
		if !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		configuration, err = newDefaultDaemonConfiguration(paths)
		if err != nil {
			return err
		}
	} else {
		configuration.Revision++
		configuration.UpdatedAt = time.Now().UnixMilli()
	}
	overrides := make(map[string]ProductOverride)
	for _, product := range productcatalog.Catalog().Products {
		override := configuration.ProductOverrides[product.ID]
		override.Executable = executables[product.ID]
		if override.Executable != "" || override.Profile != "" {
			overrides[product.ID] = override
		}
	}
	configuration.ProductOverrides = overrides
	if err := configuration.Validate(paths); err != nil {
		return err
	}
	return writeDefaultConfiguration(paths.ConfigurationFile, configuration)
}

// FailureDisposition permits ordinary release rollback. Greenfield first
// installation has no cross-version prototype authority commit boundary.
func (hooks *hostInstallRoleHooks) FailureDisposition(
	_ context.Context,
	_ releaseinstall.Phase,
) (releaseinstall.FailureDisposition, error) {
	return releaseinstall.FailureDispositionRollback, nil
}

// Ready implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Ready(ctx context.Context, release releaseinstall.InstalledRelease) error {
	return hooks.lifecycle.Ready(ctx, release)
}

// BeforeRestart commits connectors from the selected immutable release before
// the daemon starts and snapshots its adapter readiness.
func (hooks *hostInstallRoleHooks) BeforeRestart(
	ctx context.Context,
	release releaseinstall.InstalledRelease,
) error {
	return hooks.lifecycle.BeforeRestart(ctx, release)
}

// Commit implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Commit(ctx context.Context) error {
	if err := hooks.lifecycle.Commit(ctx); err != nil {
		return err
	}
	if err := removeHostSurfaceRollback(hooks.prefix); err != nil {
		return err
	}
	hooks.restoreAliases, hooks.restoreService = nil, nil
	return nil
}

// Rollback implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Rollback(ctx context.Context) error {
	return errors.Join(hooks.rollbackSurface(), hooks.lifecycle.Rollback(ctx))
}

// Remove implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Remove(ctx context.Context) error {
	removeSurface, err := prepareHostSurfaceRemoval(hooks.prefix, hooks.runtimeEndpoint)
	if err != nil {
		return err
	}
	if err := hooks.lifecycle.Remove(ctx); err != nil {
		return err
	}
	return removeSurface(ctx)
}

func (hooks *hostInstallRoleHooks) rollbackSurface() error {
	var result error
	if hooks.restoreAliases != nil {
		result = errors.Join(result, hooks.restoreAliases())
		hooks.restoreAliases = nil
	}
	if hooks.restoreService != nil {
		result = errors.Join(result, hooks.restoreService())
		hooks.restoreService = nil
	}
	record, err := loadHostSurfaceRollback(hooks.prefix, hooks.stateRoot)
	if err == nil {
		if restoreErr := restoreHostSurfaceRollback(record); restoreErr != nil {
			result = errors.Join(result, restoreErr)
		} else {
			result = errors.Join(result, removeHostSurfaceRollback(hooks.prefix))
		}
	} else if !os.IsNotExist(err) {
		result = errors.Join(result, err)
	}
	return result
}

func parseHostInstallOptions(args []string) (map[string]string, error) {
	allowed := map[string]bool{
		"--role": true, "--source-root": true, "--prefix": true, "--version": true,
		"--codex": true, "--claude": true, "--grok": true, "--qwen": true,
	}
	values := map[string]string{}
	for len(args) != 0 {
		if len(args) < 2 || !allowed[args[0]] || args[1] == "" || values[args[0]] != "" {
			return nil, errors.New("usage: lifecycle install --role host --source-root ROOT --prefix PREFIX --version VERSION [--codex PATH --claude PATH --grok PATH --qwen PATH]")
		}
		values[args[0]], args = args[1], args[2:]
	}
	for _, required := range []string{"--role", "--source-root", "--prefix", "--version"} {
		if values[required] == "" {
			return nil, fmt.Errorf("host install requires %s", required)
		}
	}
	return values, nil
}

func installHostAliases(prefix string) (func() error, error) {
	return installHostAliasesWithHooks(prefix, nil)
}

func installHostAliasesWithHooks(
	prefix string, mutationHooks *hostSurfaceMutationHooks,
) (func() error, error) {
	binRoot := filepath.Join(prefix, "bin")
	target := filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions")
	names := hostAliasNames()
	snapshots := make([]hostSurfaceAliasRollback, 0, len(names))
	for _, name := range names {
		path := filepath.Join(binRoot, name)
		snapshot, err := snapshotHostSurfaceAlias(path, nil)
		if err != nil {
			return nil, errors.Join(err, restoreHostAliases(snapshots))
		}
		snapshots = append(snapshots, snapshot)
		expected := &hostSurfaceEntryExpectation{present: snapshot.Present, identity: snapshot.observed}
		if err := replaceHostSurfaceAliasExpected(path, target, mutationHooks, expected); err != nil {
			return nil, errors.Join(err, restoreHostAliases(snapshots))
		}
	}
	return func() error { return restoreHostAliases(snapshots) }, nil
}

func hostAliasNames() []string {
	return append([]string{"agent-sessions"}, productcatalog.Catalog().HostAliases...)
}

func replaceAlias(path, target string) error {
	return replaceHostSurfaceAlias(path, target, nil)
}

func restoreHostAliases(snapshots []hostSurfaceAliasRollback) error {
	var result error
	for index := len(snapshots) - 1; index >= 0; index-- {
		result = errors.Join(result, restoreHostSurfaceAlias(snapshots[index], nil))
	}
	return result
}

func hostInstallReadiness(expectedEndpoint string) HostReadinessProbe {
	return func(ctx context.Context, release releaseinstall.InstalledRelease) error {
		expectedIdentity, err := executableSHA256(release.Executable)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(20 * time.Second)
		var last error
		for time.Now().Before(deadline) {
			body, queryErr := QueryAdmin(ctx, "runtime.status")
			if queryErr == nil {
				var status HostStatusProjection
				queryErr = json.Unmarshal(body, &status)
				if queryErr == nil && status.RuntimeVersion == release.Version && status.RuntimeIdentity == expectedIdentity &&
					status.Endpoint == expectedEndpoint && status.Generation > 0 {
					return nil
				}
				if queryErr == nil {
					queryErr = errors.New("running host identity does not match selected release")
				}
			}
			last = queryErr
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return fmt.Errorf("host service did not publish exact readiness: %w", last)
	}
}

func executableSHA256(path string) (string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // Exact immutable selected release executable.
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
