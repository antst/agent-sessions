// Package releaseinstall implements immutable, role-disjoint release
// transactions shared by the host daemon and central hub deployments.
package releaseinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Role is one independently selected and serviced deployment image.
type Role string

const (
	// RoleHost owns the agent-sessions host daemon release.
	RoleHost Role = "host"
	// RoleHub owns the central agent-sessions-hub release.
	RoleHub Role = "hub"
)

// RoleLayout contains only paths owned by one deployment role.
type RoleLayout struct {
	Role               Role
	Root               string
	ReleaseRoot        string
	ReleasesRoot       string
	CurrentSelection   string
	TransactionRoot    string
	InstallLock        string
	PreservedStateRoot string
}

// ResolveRoleLayout derives disjoint host or hub release ownership below an
// absolute common installation root.
func ResolveRoleLayout(root string, role Role) (RoleLayout, error) {
	if role != RoleHost && role != RoleHub {
		return RoleLayout{}, fmt.Errorf("unsupported release role %q", role)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return RoleLayout{}, errors.New("release root must be a clean absolute non-root path")
	}
	roleRoot := filepath.Join(root, string(role))
	return RoleLayout{
		Role: role, Root: root, ReleaseRoot: roleRoot,
		ReleasesRoot: filepath.Join(roleRoot, "releases"), CurrentSelection: filepath.Join(roleRoot, "current"),
		TransactionRoot: filepath.Join(roleRoot, "transactions"), InstallLock: filepath.Join(roleRoot, "install.lock"),
		PreservedStateRoot: filepath.Join(root, "state", string(role)),
	}, nil
}

var safeVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)

// ReleaseID combines the declared version with an identity of the exact
// staged bytes, preventing same-version development builds from aliasing.
func ReleaseID(version, contentIdentity string) (string, error) {
	if !safeVersion.MatchString(version) {
		return "", fmt.Errorf("invalid declared release version %q", version)
	}
	if !strings.HasPrefix(contentIdentity, "sha256:") || len(strings.TrimPrefix(contentIdentity, "sha256:")) != 64 {
		return "", errors.New("release content identity must be one SHA-256 digest")
	}
	digest := strings.ToLower(strings.TrimPrefix(contentIdentity, "sha256:"))
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("release content identity is not hexadecimal")
	}
	identityHash := sha256.Sum256([]byte(contentIdentity))
	return version + "-" + hex.EncodeToString(identityHash[:8]), nil
}
