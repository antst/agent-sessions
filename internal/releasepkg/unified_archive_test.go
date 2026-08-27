package releasepkg

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestUnifiedReleaseInventoryDeclaresExactlyTwoBinaries(t *testing.T) {
	root := releaseRepositoryRoot(t)
	command := exec.Command(filepath.Join(root, "scripts", "release-inventory"), "binaries")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("read release binary inventory: %v: %s", err, output)
	}
	got := strings.Fields(string(output))
	want := []string{"agent-sessions", "agent-sessions-hub"}
	if !slices.Equal(got, want) {
		t.Fatalf("release binary inventory = %q, want exact two-binary inventory %q", got, want)
	}
}

func TestUnifiedReleaseArchiveContainsExactlyTwoBinaryImages(t *testing.T) {
	root := releaseRepositoryRoot(t)
	fixture := t.TempDir()
	binaryRoot := filepath.Join(fixture, "bin")
	outputRoot := filepath.Join(fixture, "out")
	if err := os.MkdirAll(binaryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-sessions", "agent-sessions-hub"} {
		writeReleaseExecutable(t, filepath.Join(binaryRoot, name), "#!/bin/sh\nexit 0\n")
	}
	packager := filepath.Join(fixture, "packager")
	writeReleaseExecutable(t, packager, `#!/bin/sh
set -eu
test "$1" = release-package
tar -C "$2" -czf "$4" "$3"
`)

	command := exec.Command("bash", filepath.Join(root, "scripts", "package-release"),
		"linux-x64", "9.9.9", binaryRoot, outputRoot)
	command.Dir = root
	command.Env = append(os.Environ(), "AGENT_SESSIONS_RELEASE_PACKAGER="+packager)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("package unified release from exact two binaries: %v: %s", err, output)
	}

	archivePath := filepath.Join(outputRoot, "agent-sessions-9.9.9-linux-x64.tar.gz")
	entries := readArchiveEntries(t, archivePath)
	prefix := "agent-sessions-9.9.9-linux-x64/bin/linux-x64/"
	var binaries []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name, prefix) {
			continue
		}
		name := strings.TrimPrefix(entry.Name, prefix)
		if name != "" && !strings.Contains(name, "/") {
			binaries = append(binaries, name)
		}
	}
	sort.Strings(binaries)
	want := []string{"agent-sessions", "agent-sessions-hub"}
	if !slices.Equal(binaries, want) {
		t.Fatalf("archive binary entries = %q, want %q", binaries, want)
	}
}

func releaseRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func writeReleaseExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
