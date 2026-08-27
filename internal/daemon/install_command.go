package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

// RunHostInstallCLI performs the offline host-role invocation used by
// install-all. It cannot select, install, or restart the hub role.
//
//nolint:gocyclo // The one role entry point keeps parsing, staging hooks, aliases, service, and readiness visibly ordered.
func RunHostInstallCLI(ctx context.Context, args []string) error {
	values, err := parseHostInstallOptions(args)
	if err != nil {
		return err
	}
	if values["--role"] != "host" {
		return errors.New("host installer accepts only --role host")
	}
	sourceRoot, prefix, version := values["--source-root"], values["--prefix"], values["--version"]
	for name, path := range map[string]string{"source root": sourceRoot, "prefix": prefix} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
			return fmt.Errorf("host install %s must be a clean absolute non-root path", name)
		}
	}
	declaredVersion, err := os.ReadFile(filepath.Join(sourceRoot, "deploy", "agent-sessions", "VERSION")) //nolint:gosec // Exact selected release source.
	if err != nil || strings.TrimSpace(string(declaredVersion)) != version {
		return errors.New("host install version does not match the selected release source")
	}
	identity, err := releaseinstall.ContentIdentity(sourceRoot)
	if err != nil {
		return err
	}
	if err := writeReleaseManifest(sourceRoot, version, identity); err != nil {
		return err
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		return err
	}
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
	hooks, err := NewHostInstallHooks(drivers)
	if err != nil {
		return err
	}
	lifecycle, err := NewHostInstallLifecycle(hooks,
		func(context.Context, releaseinstall.InstallRequest) error { return nil },
		hostInstallReadiness(paths.ControlEndpoint),
	)
	if err != nil {
		return err
	}
	roleHooks := &hostInstallRoleHooks{
		lifecycle: lifecycle, prefix: prefix, stateRoot: paths.StateRoot, runtimeEndpoint: paths.ControlEndpoint,
	}
	service, err := newInstalledHostService(prefix)
	if err != nil {
		return err
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHost)
	if err != nil {
		return err
	}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{Layout: layout, Service: service, Hooks: roleHooks})
	if err != nil {
		return err
	}
	_, err = engine.Install(ctx, releaseinstall.InstallRequest{
		Version: version, ContentIdentity: identity, SourceRoot: sourceRoot, Executable: "agent-sessions",
	})
	if err != nil {
		return err
	}
	return nil
}

type hostInstallRoleHooks struct {
	lifecycle       *HostInstallLifecycle
	prefix          string
	stateRoot       string
	runtimeEndpoint string
	restoreService  func() error
	restoreAliases  func() error
}

// Prepare implements releaseinstall.RoleHooks under the role install lock.
func (hooks *hostInstallRoleHooks) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	if hooks == nil || hooks.lifecycle == nil {
		return errors.New("host install role hooks require a lifecycle")
	}
	if err := hooks.lifecycle.Prepare(ctx, request); err != nil {
		return err
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

// Ready implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Ready(ctx context.Context, release releaseinstall.InstalledRelease) error {
	return hooks.lifecycle.Ready(ctx, release)
}

// Commit implements releaseinstall.RoleHooks.
func (hooks *hostInstallRoleHooks) Commit(ctx context.Context) error {
	if err := hooks.lifecycle.Commit(ctx); err != nil {
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

func writeReleaseManifest(root, version, identity string) error {
	body, err := json.Marshal(map[string]string{"version": version, "content_identity": identity})
	if err != nil {
		return err
	}
	path := filepath.Join(root, "manifest.json")
	if info, statErr := os.Lstat(path); statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("release manifest changed filesystem type")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	temporary, err := os.CreateTemp(root, ".manifest-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
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

type aliasSnapshot struct {
	path    string
	present bool
	target  string
}

func installHostAliases(prefix string) (func() error, error) {
	binRoot := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binRoot, 0o755); err != nil { //nolint:gosec // User executable search directories must be traversable by conventional tools.
		return nil, err
	}
	target := filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions")
	names := hostAliasNames()
	snapshots := make([]aliasSnapshot, 0, len(names))
	for _, name := range names {
		path := filepath.Join(binRoot, name)
		snapshot := aliasSnapshot{path: path}
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				return nil, fmt.Errorf("refuse to replace non-symlink host alias %s", path)
			}
			snapshot.present = true
			snapshot.target, err = os.Readlink(path)
			if err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
		if err := replaceAlias(path, target); err != nil {
			return nil, errors.Join(err, restoreHostAliases(snapshots))
		}
	}
	return func() error { return restoreHostAliases(snapshots) }, nil
}

func hostAliasNames() []string {
	return append([]string{"agent-sessions"}, productcatalog.Catalog().HostAliases...)
}

func replaceAlias(path, target string) error {
	temporary := path + ".new"
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func restoreHostAliases(snapshots []aliasSnapshot) error {
	var result error
	for index := len(snapshots) - 1; index >= 0; index-- {
		snapshot := snapshots[index]
		if !snapshot.present {
			if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
			continue
		}
		result = errors.Join(result, replaceAlias(snapshot.path, snapshot.target))
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
