package releasepkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestInventoryContainsTwoBinariesAndFourPlatforms(t *testing.T) {
	wantBinaries := []string{"agent-sessions", "agent-sessions-hub"}
	if !reflect.DeepEqual(ExecutableNames, wantBinaries) {
		t.Fatalf("executables = %q, want %q", ExecutableNames, wantBinaries)
	}
	wantPlatforms := []Platform{
		{GOOS: "linux", GOARCH: "amd64", Name: "linux-x64"},
		{GOOS: "linux", GOARCH: "arm64", Name: "linux-arm64"},
		{GOOS: "darwin", GOARCH: "amd64", Name: "darwin-x64"},
		{GOOS: "darwin", GOARCH: "arm64", Name: "darwin-arm64"},
	}
	if !reflect.DeepEqual(SupportedPlatforms, wantPlatforms) {
		t.Fatalf("platforms = %#v, want %#v", SupportedPlatforms, wantPlatforms)
	}
}

func TestBuildArchivesContainsPayloadChecksumsAndNoRepositoryCleanup(t *testing.T) {
	source, binaries := releaseMatrixFixture(t)
	output := t.TempDir()
	artifacts, err := BuildArchives(BuildOptions{
		Version: "0.3.0", SourceRoot: source, BinaryRoot: binaries, OutputRoot: output,
		PayloadPaths: []string{"README.md", "plugins"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 4 {
		t.Fatalf("artifacts = %d, want 4", len(artifacts))
	}
	for _, artifact := range artifacts {
		if len(artifact.SHA256) != 64 {
			t.Fatalf("checksum for %s = %q", artifact.Path, artifact.SHA256)
		}
		body, err := os.ReadFile(artifact.Path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if artifact.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("checksum mismatch for %s", artifact.Path)
		}
		names := archiveNames(t, artifact.Path)
		packageRoot := "agent-sessions-0.3.0-" + artifact.Platform.Name
		for _, required := range []string{
			packageRoot + "/README.md",
			packageRoot + "/plugins/manifest.json",
			packageRoot + "/bin/" + artifact.Platform.Name + "/agent-sessions",
			packageRoot + "/bin/" + artifact.Platform.Name + "/agent-sessions-hub",
			packageRoot + "/.agent-sessions-prebuilt",
		} {
			if !containsString(names, required) {
				t.Fatalf("%s missing %s; entries=%q", artifact.Path, required, names)
			}
		}
		for _, name := range names {
			if strings.Contains(name, "cleanup-pre-unification") || strings.Contains(name, "pre-unification-cleanup.yml") {
				t.Fatalf("repository-only cleanup leaked into release: %s", name)
			}
		}
	}
	checksumBody, err := os.ReadFile(filepath.Join(output, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(checksumBody)), "\n")
	if len(lines) != 4 || !sort.StringsAreSorted(lines) {
		t.Fatalf("checksum manifest = %q, want four sorted rows", lines)
	}
}

func TestBuildArchivesIsReproducibleAndRequiresNoGoToolchain(t *testing.T) {
	source, binaries := releaseMatrixFixture(t)
	priorPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Join(t.TempDir(), "no-go-toolchain"))
	firstRoot, secondRoot := t.TempDir(), t.TempDir()
	options := BuildOptions{
		Version: "0.3.0", SourceRoot: source, BinaryRoot: binaries,
		PayloadPaths: []string{"README.md", "plugins"},
	}
	options.OutputRoot = firstRoot
	first, err := BuildArchives(options)
	if err != nil {
		t.Fatalf("prebuilt packaging with PATH=%q (was %q): %v", os.Getenv("PATH"), priorPath, err)
	}
	options.OutputRoot = secondRoot
	second, err := BuildArchives(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("artifact counts differ: %d/%d", len(first), len(second))
	}
	for index := range first {
		firstBody, err := os.ReadFile(first[index].Path)
		if err != nil {
			t.Fatal(err)
		}
		secondBody, err := os.ReadFile(second[index].Path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstBody, secondBody) || first[index].SHA256 != second[index].SHA256 {
			t.Fatalf("non-reproducible artifact for %s", first[index].Platform.Name)
		}
	}
	firstChecksums, _ := os.ReadFile(filepath.Join(firstRoot, "SHA256SUMS"))
	secondChecksums, _ := os.ReadFile(filepath.Join(secondRoot, "SHA256SUMS"))
	if !bytes.Equal(firstChecksums, secondChecksums) {
		t.Fatal("checksum manifests are not reproducible")
	}
}

func releaseMatrixFixture(t *testing.T) (string, string) {
	t.Helper()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugins", "manifest.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// These repository-only files exist beside the allowed payload and must not
	// be selected merely because packaging walks the source root.
	if err := os.WriteFile(filepath.Join(source, "cleanup-pre-unification"), []byte("forbidden\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "pre-unification-cleanup.yml"), []byte("forbidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binaries := t.TempDir()
	for _, platform := range SupportedPlatforms {
		root := filepath.Join(binaries, platform.Name)
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, executable := range ExecutableNames {
			body := platform.Name + ":" + executable + "\n"
			if err := os.WriteFile(filepath.Join(root, executable), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return source, binaries
}

func archiveNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	var names []string
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			return names
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		names = append(names, strings.TrimSuffix(header.Name, "/"))
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
