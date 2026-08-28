package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/antst/agent-sessions/internal/releaseinstall"
)

// HostReadinessProbe verifies that the selected service reports the exact
// release identity, generation, endpoint, state schema, and routing readiness.
type HostReadinessProbe func(context.Context, releaseinstall.InstalledRelease) error

// HostInstallLifecycle composes connector and readiness hooks over the
// role-neutral release transaction engine. First installation is greenfield:
// callers stop the unreleased prototype stack and remove its Agent Sessions-owned
// state before invoking this lifecycle.
type HostInstallLifecycle struct {
	connectors *HostInstallHooks
	readiness  HostReadinessProbe
}

// NewHostInstallLifecycle creates the host-specific portion of an install.
func NewHostInstallLifecycle(
	connectors *HostInstallHooks,
	readiness HostReadinessProbe,
) (*HostInstallLifecycle, error) {
	if connectors == nil || readiness == nil {
		return nil, errors.New("host install lifecycle requires connectors and readiness")
	}
	return &HostInstallLifecycle{connectors: connectors, readiness: readiness}, nil
}

// Prepare snapshots and stages all four native connectors.
func (lifecycle *HostInstallLifecycle) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	return lifecycle.connectors.Prepare(ctx, request)
}

// BeforeRestart binds the prepared connector transaction to the immutable
// installed release and commits it before the daemon snapshots product
// readiness. This also prevents native installers from retaining the
// invocation's temporary source path.
func (lifecycle *HostInstallLifecycle) BeforeRestart(
	ctx context.Context,
	release releaseinstall.InstalledRelease,
) error {
	return lifecycle.connectors.CommitInstalled(ctx, release)
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

// Commit confirms the already applied four-product connector transaction.
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
