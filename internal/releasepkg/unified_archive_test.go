package releasepkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type sourceReleaseManifest struct {
	SchemaVersion      int    `json:"schema_version"`
	ReleaseVersion     string `json:"release_version"`
	HubProtocolVersion int    `json:"hub_protocol_version"`
	Platform           string `json:"platform"`
	Checksums          string `json:"checksums"`
	Executables        []struct {
		Name string `json:"name"`
		Role string `json:"role"`
		Path string `json:"path"`
	} `json:"executables"`
	ConnectorPayloads []struct {
		Product      string   `json:"product"`
		ArchivePaths []string `json:"archive_paths"`
	} `json:"connector_payloads"`
	ServiceAssets struct {
		Host []string `json:"host"`
		Hub  []string `json:"hub"`
	} `json:"service_assets"`
}

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

func TestUnifiedReleaseInventoryDeclaresExactlyFourPlatforms(t *testing.T) {
	root := releaseRepositoryRoot(t)
	command := exec.Command(filepath.Join(root, "scripts", "release-inventory"), "platforms")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("read release platform inventory: %v: %s", err, output)
	}
	got := strings.Fields(string(output))
	want := []string{
		"linux|amd64|linux-x64", "linux|arm64|linux-arm64",
		"darwin|amd64|darwin-x64", "darwin|arm64|darwin-arm64",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("release platform inventory = %q, want %q", got, want)
	}
}

func TestUnifiedReleasePackagerRejectsNonAuthoritativeVersionAndPlatform(t *testing.T) {
	root := releaseRepositoryRoot(t)
	versionBody, err := os.ReadFile(filepath.Join(root, "deploy", "agent-sessions", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionBody))
	for name, arguments := range map[string][]string{
		"changed version":  {"linux-x64", "9.9.9", t.TempDir(), t.TempDir()},
		"unknown platform": {"contract-test", version, t.TempDir(), t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			command := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "package-release")}, arguments...)...)
			command.Dir = root
			if output, runErr := command.CombinedOutput(); runErr == nil {
				t.Fatalf("non-authoritative package identity was accepted: %s", output)
			}
		})
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

	versionBody, err := os.ReadFile(filepath.Join(root, "deploy", "agent-sessions", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionBody))
	command := exec.Command("bash", filepath.Join(root, "scripts", "package-release"),
		"linux-x64", version, binaryRoot, outputRoot)
	command.Dir = root
	command.Env = append(os.Environ(), "AGENT_SESSIONS_RELEASE_PACKAGER="+packager)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("package unified release from exact two binaries: %v: %s", err, output)
	}

	archivePath := filepath.Join(outputRoot, "agent-sessions-"+version+"-linux-x64.tar.gz")
	entries := readArchiveEntries(t, archivePath)
	packageName := "agent-sessions-" + version + "-linux-x64"
	prefix := packageName + "/bin/linux-x64/"
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

	manifestBody := readReleaseArchiveFile(t, archivePath, packageName+"/manifest.json")
	var manifest sourceReleaseManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatalf("decode generated source-release manifest: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.ReleaseVersion != version ||
		manifest.HubProtocolVersion != productcatalog.ProtocolVersion ||
		manifest.Platform != "linux-x64" || manifest.Checksums != "SHA256SUMS" {
		t.Fatalf("source-release manifest identity = %+v", manifest)
	}
	gotExecutables := make([]string, 0, len(manifest.Executables))
	for _, executable := range manifest.Executables {
		gotExecutables = append(gotExecutables, executable.Name+"|"+executable.Role+"|"+executable.Path)
	}
	wantExecutables := []string{
		"agent-sessions|host|bin/linux-x64/agent-sessions",
		"agent-sessions-hub|hub|bin/linux-x64/agent-sessions-hub",
	}
	if !slices.Equal(gotExecutables, wantExecutables) {
		t.Fatalf("manifest executables = %q, want %q", gotExecutables, wantExecutables)
	}
	products := make([]string, 0, len(manifest.ConnectorPayloads))
	for _, connector := range manifest.ConnectorPayloads {
		if len(connector.ArchivePaths) == 0 {
			t.Errorf("%s connector has no archive payload", connector.Product)
		}
		products = append(products, connector.Product)
	}
	if wantProducts := []string{"codex", "claude", "grok", "qwen"}; !slices.Equal(products, wantProducts) {
		t.Fatalf("manifest connector products = %q, want %q", products, wantProducts)
	}
	for _, assets := range [][]string{manifest.ServiceAssets.Host, manifest.ServiceAssets.Hub} {
		if len(assets) == 0 {
			t.Fatal("manifest omitted one independently deployed service asset inventory")
		}
		for _, asset := range assets {
			if !archiveEntryExists(entries, packageName+"/"+asset) {
				t.Errorf("manifest service asset is absent from archive: %s", asset)
			}
		}
	}

	checksumBody := string(readReleaseArchiveFile(t, archivePath, packageName+"/SHA256SUMS"))
	for _, relative := range []string{
		"manifest.json", "bin/linux-x64/agent-sessions", "bin/linux-x64/agent-sessions-hub",
		".mcp.json", "claude/.mcp.json", "grok/.mcp.json", "qwen/mcp.json",
		manifest.ServiceAssets.Host[0], manifest.ServiceAssets.Hub[0],
	} {
		body := readReleaseArchiveFile(t, archivePath, packageName+"/"+relative)
		digest := sha256.Sum256(body)
		wantLine := hex.EncodeToString(digest[:]) + "  " + relative
		if !strings.Contains(checksumBody, wantLine+"\n") {
			t.Errorf("archive checksum inventory omitted %s", relative)
		}
	}
	archiveBody, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveDigest := sha256.Sum256(archiveBody)
	sidecar, err := os.ReadFile(archivePath + ".sha256")
	if err != nil {
		t.Fatalf("read archive checksum sidecar: %v", err)
	}
	wantSidecar := hex.EncodeToString(archiveDigest[:]) + "  " + filepath.Base(archivePath) + "\n"
	if string(sidecar) != wantSidecar {
		t.Fatalf("archive checksum sidecar = %q, want %q", sidecar, wantSidecar)
	}
}

func archiveEntryExists(entries []*tar.Header, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func readReleaseArchiveFile(t *testing.T, path, name string) []byte {
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
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			t.Fatalf("archive entry is missing: %s", name)
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if header.Name == name {
			body, readErr := io.ReadAll(reader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			return body
		}
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
