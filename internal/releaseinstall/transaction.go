// Package releaseinstall owns Agent Sessions release and connector transactions.
package releaseinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

// Role selects the independently installed host or hub image.
type Role string

const (
	// HostRole installs the per-user host daemon and connector aliases.
	HostRole Role = "host"
	// HubRole installs only the independent central hub image.
	HubRole Role = "hub"
)

var releaseComponentPattern = regexp.MustCompile(`^[[:alnum:]][[:alnum:]._-]*$`)

// BinaryName returns the one image selected by a role transaction.
func (role Role) BinaryName() string {
	if role == HubRole {
		return "agent-sessions-hub"
	}
	return "agent-sessions"
}

func (role Role) valid() bool {
	return role == HostRole || role == HubRole
}

func (role Role) aliases() []string {
	if role == HubRole {
		return []string{"agent-sessions-hub"}
	}
	aliases := []string{"agent-sessions"}
	for _, product := range productcatalog.All() {
		for _, alias := range []string{product.PeerAlias, product.LaneAlias} {
			if alias != "" {
				aliases = append(aliases, alias)
			}
		}
	}
	return aliases
}

// Service performs the platform-specific validation and lifecycle boundary.
// Activate must validate and select the candidate in one call; it is invoked
// exactly once after the filesystem commit.
type Service interface {
	// Validate checks the private staged release before it can be selected.
	Validate(context.Context, string) error
	// Activate selects and starts or restarts the committed release.
	Activate(context.Context, string) error
	// Remove stops and disables the exact role service.
	Remove(context.Context) error
}

// InstallRequest names a fully assembled release tree. SourceDir is copied
// into a private stage before validation and is never selected directly.
type InstallRequest struct {
	Role      Role
	Version   string
	Platform  string
	SourceDir string
}

// Transaction installs or removes one role below Prefix.
// AfterCommit is an optional failure-injection/audit hook called after the
// staged tree is committed but before selection changes.
type Transaction struct {
	Prefix      string
	Service     Service
	AfterCommit func(string) error
}

// Install validates a private stage, atomically commits and selects it, then
// activates the service once. Any failure restores the exact prior release
// and selection.
//
//nolint:gocyclo // The rollback transaction is intentionally linear so every filesystem commit point has an adjacent failure path.
func (transaction Transaction) Install(ctx context.Context, request InstallRequest) (returnErr error) {
	if err := transaction.validateInstallRequest(request); err != nil {
		return err
	}
	canonicalPrefix, err := pathidentity.FuturePath(transaction.Prefix)
	if err != nil {
		return fmt.Errorf("canonicalize install prefix: %w", err)
	}
	transaction.Prefix = canonicalPrefix
	roleRoot := filepath.Join(transaction.Prefix, "libexec", "agent-sessions", string(request.Role))
	releasesRoot := filepath.Join(roleRoot, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil { //nolint:gosec // Installed releases must be traversable by normal user tooling.
		return fmt.Errorf("create release root: %w", err)
	}
	stage, err := os.MkdirTemp(releasesRoot, ".stage-")
	if err != nil {
		return fmt.Errorf("create release stage: %w", err)
	}
	defer func() {
		if stage != "" {
			returnErr = errors.Join(returnErr, removeTree(stage, releasesRoot))
		}
	}()
	if err := copyDirectory(request.SourceDir, stage); err != nil {
		return fmt.Errorf("stage release: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "PLATFORM"), []byte(request.Platform+"\n"), 0o644); err != nil { //nolint:gosec // Public release metadata is intentionally world-readable.
		return fmt.Errorf("write staged platform: %w", err)
	}
	if err := validateStagedBinary(stage, request.Role.BinaryName()); err != nil {
		return err
	}
	if err := transaction.Service.Validate(ctx, stage); err != nil {
		return fmt.Errorf("validate staged release: %w", err)
	}

	currentPath := filepath.Join(roleRoot, "current")
	priorCurrent, err := resolveSelectedRelease(currentPath, releasesRoot)
	if err != nil {
		return err
	}
	releasePath := filepath.Join(releasesRoot, request.Version)
	backupPath := ""
	if _, statErr := os.Lstat(releasePath); statErr == nil {
		info, infoErr := os.Lstat(releasePath)
		if infoErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("existing release is not a real directory: %s", releasePath)
		}
		backupPath, err = os.MkdirTemp(releasesRoot, ".prior-")
		if err != nil {
			return err
		}
		if removeErr := os.Remove(backupPath); removeErr != nil {
			return removeErr
		}
		if err := os.Rename(releasePath, backupPath); err != nil {
			return fmt.Errorf("backup selected release: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	committed := false
	rollback := func(cause error) error {
		var rollbackErr error
		if committed {
			rollbackErr = errors.Join(rollbackErr, removeTree(releasePath, releasesRoot))
		}
		if backupPath != "" {
			rollbackErr = errors.Join(rollbackErr, os.Rename(backupPath, releasePath))
			backupPath = ""
		}
		rollbackErr = errors.Join(rollbackErr, transaction.restoreSelection(request.Role, priorCurrent, currentPath))
		return errors.Join(cause, rollbackErr)
	}
	if err := os.Rename(stage, releasePath); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, releasePath)
		}
		return fmt.Errorf("commit staged release: %w", err)
	}
	stage = ""
	committed = true
	if transaction.AfterCommit != nil {
		if err := transaction.AfterCommit(releasePath); err != nil {
			return rollback(fmt.Errorf("after release commit: %w", err))
		}
	}
	if err := transaction.selectRelease(request.Role, releasePath, currentPath); err != nil {
		return rollback(err)
	}
	if err := transaction.Service.Activate(ctx, releasePath); err != nil {
		return rollback(fmt.Errorf("activate selected release: %w", err))
	}
	if backupPath != "" {
		if err := removeTree(backupPath, releasesRoot); err != nil {
			return fmt.Errorf("remove prior release backup: %w", err)
		}
	}
	return nil
}

// Remove stops and disables the role before removing only its managed aliases
// and release tree. Shared state and the other role are outside this boundary.
func (transaction Transaction) Remove(ctx context.Context, role Role) error {
	if err := transaction.validateBase(role); err != nil {
		return err
	}
	canonicalPrefix, err := pathidentity.FuturePath(transaction.Prefix)
	if err != nil {
		return fmt.Errorf("canonicalize install prefix: %w", err)
	}
	transaction.Prefix = canonicalPrefix
	roleRoot := filepath.Join(transaction.Prefix, "libexec", "agent-sessions", string(role))
	info, err := os.Lstat(roleRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed role root is not a real directory: %s", roleRoot)
	}
	if err := transaction.Service.Remove(ctx); err != nil {
		return fmt.Errorf("remove role service: %w", err)
	}
	for _, alias := range role.aliases() {
		path := filepath.Join(transaction.Prefix, "bin", alias)
		if err := removeManagedAlias(path, roleRoot); err != nil {
			return err
		}
	}
	return removeTree(roleRoot, filepath.Join(transaction.Prefix, "libexec", "agent-sessions"))
}

func (transaction Transaction) validateInstallRequest(request InstallRequest) error {
	if err := transaction.validateBase(request.Role); err != nil {
		return err
	}
	if !releaseComponentPattern.MatchString(request.Version) || !releaseComponentPattern.MatchString(request.Platform) {
		return errors.New("release version and platform must be safe path components")
	}
	if !filepath.IsAbs(request.SourceDir) {
		return errors.New("release source must be absolute")
	}
	info, err := os.Lstat(request.SourceDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release source must be a real directory")
	}
	return nil
}

func (transaction Transaction) validateBase(role Role) error {
	if !role.valid() {
		return fmt.Errorf("unsupported install role %q", role)
	}
	if !filepath.IsAbs(transaction.Prefix) || filepath.Clean(transaction.Prefix) == string(filepath.Separator) {
		return errors.New("install prefix must be an absolute non-root path")
	}
	if transaction.Service == nil {
		return errors.New("service transaction is required")
	}
	return nil
}

func validateStagedBinary(stage, binary string) error {
	path := filepath.Join(stage, "bin", binary)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 {
		return fmt.Errorf("staged release lacks executable %s", binary)
	}
	return nil
}

func resolveSelectedRelease(currentPath, releasesRoot string) (string, error) {
	info, err := os.Lstat(currentPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("selected release is not a symbolic link: %s", currentPath)
	}
	target, err := filepath.EvalSymlinks(currentPath)
	if err != nil {
		return "", err
	}
	rootWithSeparator := filepath.Clean(releasesRoot) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(target), rootWithSeparator) {
		return "", fmt.Errorf("selected release is outside managed root: %s", target)
	}
	return target, nil
}

func (transaction Transaction) selectRelease(role Role, releasePath, currentPath string) error {
	if err := replaceSymlink(releasePath, currentPath); err != nil {
		return fmt.Errorf("select release: %w", err)
	}
	for _, alias := range role.aliases() {
		path := filepath.Join(transaction.Prefix, "bin", alias)
		if err := replaceSymlink(filepath.Join(currentPath, "bin", role.BinaryName()), path); err != nil {
			return fmt.Errorf("select alias %s: %w", alias, err)
		}
	}
	return nil
}

func (transaction Transaction) restoreSelection(role Role, priorCurrent, currentPath string) error {
	var result error
	if priorCurrent == "" {
		result = errors.Join(result, removeIfSymlink(currentPath))
		for _, alias := range role.aliases() {
			result = errors.Join(result, removeIfSymlink(filepath.Join(transaction.Prefix, "bin", alias)))
		}
		return result
	}
	result = errors.Join(result, replaceSymlink(priorCurrent, currentPath))
	for _, alias := range role.aliases() {
		result = errors.Join(result, replaceSymlink(filepath.Join(currentPath, "bin", role.BinaryName()), filepath.Join(transaction.Prefix, "bin", alias)))
	}
	return result
}

func replaceSymlink(target, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // User-local command aliases require traversable bin directories.
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".link-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	if err := os.Symlink(target, temporaryPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
}

func removeManagedAlias(path, roleRoot string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refuse to remove non-symbolic managed alias: %s", path)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(roleRoot)+string(filepath.Separator)) {
		return fmt.Errorf("refuse to remove alias outside managed role: %s", path)
	}
	return os.Remove(path)
}

func removeIfSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refuse to replace non-symbolic path: %s", path)
	}
	return os.Remove(path)
}

func copyDirectory(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destinationPath := filepath.Join(destination, entry.Name())
		info, statErr := os.Lstat(sourcePath)
		if statErr != nil {
			return statErr
		}
		switch {
		case info.IsDir() && info.Mode()&os.ModeSymlink == 0:
			if err := os.Mkdir(destinationPath, info.Mode().Perm()); err != nil {
				return err
			}
			if err := copyDirectory(sourcePath, destinationPath); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyRegularFile(sourcePath, destinationPath, info); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported or indirect release entry: %s", sourcePath)
		}
	}
	return nil
}

func copyRegularFile(source, destination string, expected os.FileInfo) error {
	input, err := os.Open(source) //nolint:gosec // The opened file is identity-checked against the prior no-follow metadata below.
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return fmt.Errorf("release source changed during copy: %s", source)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, expected.Mode().Perm()) //nolint:gosec // Destination is a fresh child of the private stage.
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}

func removeTree(path, parent string) error {
	if path == "" {
		return nil
	}
	cleanPath, cleanParent := filepath.Clean(path), filepath.Clean(parent)
	if !strings.HasPrefix(cleanPath, cleanParent+string(filepath.Separator)) {
		return fmt.Errorf("refuse to remove path outside managed parent: %s", path)
	}
	info, err := os.Lstat(cleanPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to recursively remove symbolic tree: %s", path)
	}
	return os.RemoveAll(cleanPath)
}
