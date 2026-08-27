package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antst/agent-sessions/internal/releaseinstall"
)

// MigrationInspector performs the read-only first-migration gate before a
// unified host release is selected. Mutation remains in the later migration
// transaction story.
type MigrationInspector func(context.Context, releaseinstall.InstallRequest) error

// HostReadinessProbe verifies that the selected service reports the exact
// release identity, generation, endpoint, state schema and routing readiness.
type HostReadinessProbe func(context.Context, releaseinstall.InstalledRelease) error

// HostInstallLifecycle composes host-only migration, connector and readiness
// hooks over the role-neutral release transaction engine.
type HostInstallLifecycle struct {
	connectors *HostInstallHooks
	migration  MigrationInspector
	readiness  HostReadinessProbe
}

// NewHostInstallLifecycle creates the host-specific portion of an install.
func NewHostInstallLifecycle(
	connectors *HostInstallHooks,
	migration MigrationInspector,
	readiness HostReadinessProbe,
) (*HostInstallLifecycle, error) {
	if connectors == nil || migration == nil || readiness == nil {
		return nil, errors.New("host install lifecycle requires connectors, migration inspection and readiness")
	}
	return &HostInstallLifecycle{connectors: connectors, migration: migration, readiness: readiness}, nil
}

// Prepare runs the no-mutation migration gate before connector preparation.
func (lifecycle *HostInstallLifecycle) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	if err := lifecycle.migration(ctx, request); err != nil {
		return fmt.Errorf("inspect first migration: %w", err)
	}
	return lifecycle.connectors.Prepare(ctx, request)
}

// Ready verifies the immutable executable identity boundary and then invokes
// the exact running-daemon readiness probe.
func (lifecycle *HostInstallLifecycle) Ready(ctx context.Context, release releaseinstall.InstalledRelease) error {
	if release.Role != releaseinstall.RoleHost || !filepath.IsAbs(release.Root) ||
		filepath.Clean(release.Root) != release.Root || !pathWithin(release.Executable, release.Root) {
		return errors.New("host readiness received a non-host or unsafe release identity")
	}
	info, err := os.Lstat(release.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("selected host release executable is missing or not executable")
	}
	return lifecycle.readiness(ctx, release)
}

// Commit commits the four-product connector transaction.
func (lifecycle *HostInstallLifecycle) Commit(ctx context.Context) error {
	return lifecycle.connectors.Commit(ctx)
}

// Rollback restores exact prior connector state.
func (lifecycle *HostInstallLifecycle) Rollback(ctx context.Context) error {
	return lifecycle.connectors.Rollback(ctx)
}

// Remove invokes only supported host connector removal.
func (lifecycle *HostInstallLifecycle) Remove(ctx context.Context) error {
	return lifecycle.connectors.Remove(ctx)
}
