package releasepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Platform is one supported prebuilt release target.
type Platform struct {
	GOOS   string
	GOARCH string
	Name   string
}

var (
	// ExecutableNames is the complete release image inventory.
	ExecutableNames = []string{"agent-sessions", "agent-sessions-hub"}
	// SupportedPlatforms is the complete four-platform release matrix.
	SupportedPlatforms = []Platform{
		{GOOS: "linux", GOARCH: "amd64", Name: "linux-x64"},
		{GOOS: "linux", GOARCH: "arm64", Name: "linux-arm64"},
		{GOOS: "darwin", GOARCH: "amd64", Name: "darwin-x64"},
		{GOOS: "darwin", GOARCH: "arm64", Name: "darwin-arm64"},
	}
)

var inventoryComponentPattern = regexp.MustCompile(`^[[:alnum:]][[:alnum:]._-]*$`)

// BuildOptions names prebuilt input binaries and an explicit payload allowlist.
type BuildOptions struct {
	Version      string
	SourceRoot   string
	BinaryRoot   string
	OutputRoot   string
	PayloadPaths []string
}

// Artifact describes one completed deterministic archive.
type Artifact struct {
	Platform Platform
	Path     string
	SHA256   string
}

// BuildArchives creates all four archives from prebuilt images without
// invoking a compiler or any external command, then writes SHA256SUMS.
func BuildArchives(options BuildOptions) ([]Artifact, error) {
	if err := validateBuildOptions(options); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(options.OutputRoot, 0o755); err != nil { //nolint:gosec // Release artifact directories are intentionally traversable.
		return nil, err
	}
	artifacts := make([]Artifact, 0, len(SupportedPlatforms))
	for _, platform := range SupportedPlatforms {
		artifact, err := buildPlatformArchive(options, platform)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := writeChecksums(options.OutputRoot, artifacts); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func validateBuildOptions(options BuildOptions) error {
	if !inventoryComponentPattern.MatchString(options.Version) {
		return errors.New("release version must be a safe path component")
	}
	for label, path := range map[string]string{
		"source root": options.SourceRoot, "binary root": options.BinaryRoot, "output root": options.OutputRoot,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("%s must be an absolute non-root path", label)
		}
	}
	if len(options.PayloadPaths) == 0 {
		return errors.New("release payload allowlist is empty")
	}
	for _, path := range options.PayloadPaths {
		clean := filepath.Clean(path)
		if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe payload path %q", path)
		}
	}
	return nil
}

func buildPlatformArchive(options BuildOptions, platform Platform) (Artifact, error) {
	stage, err := os.MkdirTemp(options.OutputRoot, ".package-stage-")
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	packageName := "agent-sessions-" + options.Version + "-" + platform.Name
	packageRoot := filepath.Join(stage, packageName)
	if err := os.MkdirAll(filepath.Join(packageRoot, "bin", platform.Name), 0o755); err != nil { //nolint:gosec // Packaged executable directories require standard release permissions.
		return Artifact{}, err
	}
	for _, relative := range options.PayloadPaths {
		source := filepath.Join(options.SourceRoot, relative)
		destination := filepath.Join(packageRoot, relative)
		if err := copyInventoryEntry(source, destination); err != nil {
			return Artifact{}, fmt.Errorf("copy payload %s: %w", relative, err)
		}
	}
	for _, executable := range ExecutableNames {
		source := filepath.Join(options.BinaryRoot, platform.Name, executable)
		destination := filepath.Join(packageRoot, "bin", platform.Name, executable)
		info, statErr := os.Lstat(source)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 {
			return Artifact{}, fmt.Errorf("prebuilt executable is missing or indirect: %s", source)
		}
		if err := copyInventoryFile(source, destination, 0o755); err != nil {
			return Artifact{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(packageRoot, ".agent-sessions-prebuilt"), nil, 0o644); err != nil { //nolint:gosec // Public empty release marker is intentionally readable.
		return Artifact{}, err
	}
	archive := filepath.Join(options.OutputRoot, packageName+".tar.gz")
	if err := Create(stage, packageName, archive); err != nil {
		return Artifact{}, err
	}
	checksum, err := fileSHA256(archive)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Platform: platform, Path: archive, SHA256: checksum}, nil
}

func copyInventoryEntry(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir() && info.Mode()&os.ModeSymlink == 0:
		if err := os.MkdirAll(destination, 0o755); err != nil { //nolint:gosec // Archive payload directories use standard distributable permissions.
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyInventoryEntry(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return copyInventoryFile(source, destination, mode)
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil { //nolint:gosec // Archive payload parents use standard distributable permissions.
			return err
		}
		return os.Symlink(link, destination)
	default:
		return fmt.Errorf("unsupported payload entry %s", source)
	}
}

func copyInventoryFile(source, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil { //nolint:gosec // Archive payload parents use standard distributable permissions.
		return err
	}
	input, err := os.Open(source) //nolint:gosec // source is under an explicit release input root.
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // Destination is a fresh path inside the private package stage.
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // path is the private output archive.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeChecksums(outputRoot string, artifacts []Artifact) error {
	rows := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		rows = append(rows, artifact.SHA256+"  "+filepath.Base(artifact.Path))
	}
	sort.Strings(rows)
	temporary, err := os.CreateTemp(outputRoot, ".checksums-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.WriteString(strings.Join(rows, "\n") + "\n"); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(outputRoot, "SHA256SUMS")); err != nil {
		return err
	}
	committed = true
	return nil
}
