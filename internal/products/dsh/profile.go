package dsh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
)

var errManagedProfileUnavailable = errors.New("DSH managed profile is unavailable")

// validateProfileIdentity is deliberately lexical. Product constructors may
// use it without taking a snapshot of the installed product or profile state.
func validateProfileIdentity(profile string) error {
	if profile == "" || profile == "." || profile == ".." || len(profile) > maxProfileBytes || profile != strings.TrimSpace(profile) || strings.ContainsAny(profile, "/\\") {
		return fmt.Errorf("%w: DSH profile identity is invalid", productruntime.ErrNativeRejected)
	}
	for _, character := range profile {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: DSH profile identity contains control characters", productruntime.ErrNativeRejected)
		}
	}
	return nil
}

// validateManagedDSHHomeShape checks only the authored path contract. Whether
// the directory exists and where symlinks resolve are live operation facts.
func validateManagedDSHHomeShape(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || temporaryPathLexical(path) {
		return fmt.Errorf("%w: DSH home must be a canonical Agent Sessions HOME/XDG-state path", productruntime.ErrUnsupportedPolicy)
	}
	return nil
}

func temporaryPathLexical(path string) bool {
	return within(filepath.Clean(path), "/tmp") || within(filepath.Clean(path), "/private/tmp")
}

func managedProfileManifestPath(dshHome, profile string) (string, error) {
	if err := validateManagedDSHHomeShape(dshHome); err != nil {
		return "", err
	}
	if err := validateProfileIdentity(profile); err != nil {
		return "", err
	}
	profilesRoot := filepath.Join(dshHome, "profiles")
	profileRoot := filepath.Join(profilesRoot, profile)
	manifestPath := filepath.Join(profileRoot, "package.json")
	if filepath.Dir(profileRoot) != profilesRoot || filepath.Base(profileRoot) != profile || filepath.Dir(manifestPath) != profileRoot {
		return "", fmt.Errorf("%w: DSH profile must remain one exact managed path segment", productruntime.ErrNativeRejected)
	}
	return manifestPath, nil
}

func validateConfiguredProfileManifestShape(dshHome, profile, configured string) error {
	expected, err := managedProfileManifestPath(dshHome, profile)
	if err != nil {
		return err
	}
	if configured != "" && (configured != expected || !filepath.IsAbs(configured) || filepath.Clean(configured) != configured) {
		return fmt.Errorf("%w: DSH profile manifest does not name the selected managed profile", productruntime.ErrIncompatible)
	}
	return nil
}

// validateManagedProfile is intentionally operation-time. It derives the one
// exact manifest from the selected profile instead of trusting a constructor-
// captured path, and rejects missing, moved, symlinked, or drifted profiles.
func validateManagedProfile(dshHome, profile string) (string, error) {
	if err := validateManagedDSHHome(dshHome); err != nil {
		return "", err
	}
	manifestPath, err := managedProfileManifestPath(dshHome, profile)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %w", productruntime.ErrUnavailable, errManagedProfileUnavailable)
	}
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: DSH managed profile manifest is not a regular file", productruntime.ErrIncompatible)
	}
	canonical, err := filepath.EvalSymlinks(manifestPath)
	if err != nil || canonical != manifestPath {
		return "", fmt.Errorf("%w: DSH managed profile manifest changed identity", productruntime.ErrIncompatible)
	}
	if manifestVersion(manifestPath, ProfilePackage) != PinnedVersion {
		return "", fmt.Errorf("%w: DSH managed profile manifest is not the exact pinned tuple", productruntime.ErrIncompatible)
	}
	return manifestPath, nil
}
