package releaseinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"

	"golang.org/x/sys/unix"
)

type invalidSourceEntryCase struct {
	name   string
	create func(*testing.T, string)
}

func invalidSourceEntryCases() []invalidSourceEntryCase {
	return []invalidSourceEntryCase{
		{name: "symlink", create: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Symlink("manifest.json", path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "named pipe", create: func(t *testing.T, path string) {
			t.Helper()
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

func TestInstallRequestAcceptsGeneratedSourceReleaseManifestWithExactChecksumBinding(t *testing.T) {
	root := releaseTestTempDir(t)
	executable := filepath.Join(root, "bin", "agent-sessions")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("host release"), 0o700); err != nil {
		t.Fatal(err)
	}
	connectorPaths := []string{
		".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills",
		".claude-plugin", "claude", "grok", "qwen",
	}
	for _, path := range connectorPaths {
		if err := createTestConnectorRoot(root, path); err != nil {
			t.Fatal(err)
		}
	}
	servicePaths := []string{
		"deploy/agent-sessions/systemd/user/agent-sessions.service",
		"deploy/agent-sessions/launchd/net.antst.agent-sessions.plist",
	}
	for _, path := range servicePaths {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte("service asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func(body []byte) string {
		hash := sha256.Sum256(body)
		return hex.EncodeToString(hash[:])
	}
	writeMetadata := func(executablePath string) string {
		t.Helper()
		manifest := []byte(`{"schema_version":1,"release_version":"0.3.0","hub_protocol_version":3,"platform":"` + currentReleasePlatform() + `","checksums":"SHA256SUMS","executables":[{"name":"agent-sessions","role":"host","path":"` + executablePath + `"}],"connector_payloads":[{"product":"codex","plugin_id":"agent-sessions","archive_paths":[".agents",".codex-plugin",".mcp.json","hooks","scripts","skills"]},{"product":"claude","plugin_id":"agent-sessions","archive_paths":[".claude-plugin","claude"]},{"product":"grok","plugin_id":"agent-sessions","archive_paths":["grok"]},{"product":"qwen","plugin_id":"agent-sessions","archive_paths":["qwen"]}],"service_assets":{"host":["deploy/agent-sessions/systemd/user/agent-sessions.service","deploy/agent-sessions/launchd/net.antst.agent-sessions.plist"],"hub":[]}}`)
		if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifest, 0o600); err != nil {
			t.Fatal(err)
		}
		checksums := digest([]byte("host release")) + "  bin/agent-sessions\n" + digest(manifest) + "  manifest.json\n"
		checksums += digest([]byte("{}\n")) + "  .mcp.json\n"
		for _, path := range servicePaths {
			checksums += digest([]byte("service asset")) + "  " + path + "\n"
		}
		if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(checksums), 0o600); err != nil {
			t.Fatal(err)
		}
		identity, err := ContentIdentity(root)
		if err != nil {
			t.Fatal(err)
		}
		return identity
	}
	mismatchedIdentity := writeMetadata("bin/linux-x64/agent-sessions")
	if err := validateInstallRequest(InstallRequest{
		Version: "0.3.0", ContentIdentity: mismatchedIdentity, SourceRoot: root, Executable: "agent-sessions",
	}); err == nil {
		t.Fatal("manifest executable path did not bind the projected binary used by install")
	}
	identity := writeMetadata("bin/agent-sessions")
	request := InstallRequest{
		Version: "0.3.0", ContentIdentity: identity, SourceRoot: root, Executable: "agent-sessions",
	}
	if err := validateInstallRequest(request); err != nil {
		t.Fatalf("generated source release manifest was rejected: %v", err)
	}
	if err := os.WriteFile(executable, []byte("changed host release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallRequest(request); err == nil {
		t.Fatal("changed payload retained generated manifest install authority")
	}
}

func TestInstallRequestRejectsLegacySelfAssertedManifest(t *testing.T) {
	root := releaseTestTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "agent-sessions"), []byte("host"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := testContentIdentity("legacy")
	if err := os.WriteFile(filepath.Join(root, "manifest.json"),
		[]byte(`{"version":"0.2.4","content_identity":"`+identity+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallRequest(InstallRequest{
		Version: "0.2.4", ContentIdentity: identity, SourceRoot: root, Executable: "agent-sessions",
	}); err == nil {
		t.Fatal("legacy self-asserted manifest bypassed schema-1 inventory and checksums")
	}
}

func TestSourceReleaseChecksumValidationRejectsIndirectAndUnsupportedEntries(t *testing.T) {
	for _, test := range invalidSourceEntryCases() {
		t.Run(test.name, func(t *testing.T) {
			root := releaseTestTempDir(t)
			entry := filepath.Join(root, "indirect")
			test.create(t, entry)
			checksums := strings.Repeat("0", 64) + "  indirect\n"
			if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(checksums), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifySourceReleaseChecksums(root, "SHA256SUMS"); err == nil {
				t.Fatalf("checksum validation accepted %s entry", test.name)
			}
		})
	}
	t.Run("checksum symlink", func(t *testing.T) {
		root := releaseTestTempDir(t)
		target := filepath.Join(root, "outside-checksums")
		if err := os.WriteFile(target, []byte(strings.Repeat("0", 64)+"  payload\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "SHA256SUMS")); err != nil {
			t.Fatal(err)
		}
		if err := verifySourceReleaseChecksums(root, "SHA256SUMS"); err == nil {
			t.Fatal("checksum validation followed an indirect checksum manifest")
		}
	})
}

func TestMakeTreeImmutableNeverFollowsOrCommitsSwappedSymlink(t *testing.T) {
	assertTreeChmodSwapIsBoundToOpenedEntry(t, 0o600, makeTreeImmutableWithHook)
}

func TestMakeTreeWritableForRemovalNeverFollowsSwappedSymlink(t *testing.T) {
	assertTreeChmodSwapIsBoundToOpenedEntry(t, 0o400, makeTreeWritableForRemovalWithHook)
}

func assertTreeChmodSwapIsBoundToOpenedEntry(
	t *testing.T,
	initialMode os.FileMode,
	operation func(string, func(string)) error,
) {
	t.Helper()
	root := releaseTestTempDir(t)
	target := filepath.Join(root, "payload")
	outside := filepath.Join(releaseTestTempDir(t), "outside")
	if err := os.WriteFile(target, []byte("payload"), initialMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), initialMode); err != nil {
		t.Fatal(err)
	}
	swapped := false
	err := operation(root, func(relative string) {
		if relative != "payload" || swapped {
			return
		}
		swapped = true
		if renameErr := os.Rename(target, target+".opened"); renameErr != nil {
			t.Fatal(renameErr)
		}
		if symlinkErr := os.Symlink(outside, target); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	})
	if err == nil || !swapped {
		t.Fatalf("descriptor traversal accepted a swapped symlink: swapped=%v err=%v", swapped, err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != initialMode {
		t.Fatalf("descriptor traversal chmod followed outside symlink: mode=%v", info.Mode().Perm())
	}
}

func TestInstallRequestRejectsIntermediateSymlinkInAbsoluteSourceRoot(t *testing.T) {
	parent := releaseTestTempDir(t)
	realParent := filepath.Join(parent, "real-parent")
	realRoot := filepath.Join(realParent, "release")
	request := createReleaseSource(t, realRoot, "1.2.3", "root-link", "agent-sessions")
	aliasParent := filepath.Join(parent, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	request.SourceRoot = filepath.Join(aliasParent, "release")
	if err := validateInstallRequest(request); err == nil {
		t.Fatal("release validation followed an intermediate symlink in the absolute source root")
	}
}

func TestSourceReleaseManifestRequiresExactCanonicalRoleInventory(t *testing.T) {
	t.Run("valid host and hub", func(t *testing.T) {
		for _, role := range []Role{RoleHost, RoleHub} {
			request := createReleaseSource(t, filepath.Join(releaseTestTempDir(t), string(role)), "1.2.3", "exact", roleExecutable(role))
			if err := validateInstallRequest(request); err != nil {
				t.Fatalf("exact %s inventory was rejected: %v", role, err)
			}
		}
	})
	t.Run("connector roots", func(t *testing.T) {
		request := createReleaseSource(t, filepath.Join(releaseTestTempDir(t), "host"), "1.2.3", "connector", "agent-sessions")
		request = rewriteTestSourceManifest(t, request, func(manifest *sourceReleaseManifest) {
			manifest.ConnectorPayloads[0].ArchivePaths = []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "docs"}
		})
		if err := validateInstallRequest(request); err == nil {
			t.Fatal("host manifest accepted a present but noncanonical connector payload root")
		}
	})
	t.Run("current platform", func(t *testing.T) {
		request := createReleaseSource(t, filepath.Join(releaseTestTempDir(t), "host"), "1.2.3", "platform", "agent-sessions")
		request = rewriteTestSourceManifest(t, request, func(manifest *sourceReleaseManifest) {
			if currentReleasePlatform() == "linux-x64" {
				manifest.Platform = "darwin-arm64"
			} else {
				manifest.Platform = "linux-x64"
			}
		})
		if err := validateInstallRequest(request); err == nil {
			t.Fatal("release manifest for another platform was accepted")
		}
	})
	t.Run("host service assets", func(t *testing.T) {
		request := createReleaseSource(t, filepath.Join(releaseTestTempDir(t), "host"), "1.2.3", "host-service", "agent-sessions")
		request = rewriteTestSourceManifest(t, request, func(manifest *sourceReleaseManifest) {
			manifest.ServiceAssets.Host[0], manifest.ServiceAssets.Host[1] = manifest.ServiceAssets.Host[1], manifest.ServiceAssets.Host[0]
		})
		if err := validateInstallRequest(request); err == nil {
			t.Fatal("host manifest accepted a reordered non-exact service inventory")
		}
	})
	t.Run("hub service assets", func(t *testing.T) {
		request := createReleaseSource(t, filepath.Join(releaseTestTempDir(t), "hub"), "1.2.3", "hub-service", "agent-sessions-hub")
		request = rewriteTestSourceManifest(t, request, func(manifest *sourceReleaseManifest) {
			manifest.ServiceAssets.Hub = manifest.ServiceAssets.Hub[:2]
		})
		if err := validateInstallRequest(request); err == nil {
			t.Fatal("hub manifest accepted an incomplete exact service inventory")
		}
	})
	for _, test := range []struct {
		name     string
		relative string
		makePath func(string) error
	}{
		{name: "mcp manifest as directory", relative: ".mcp.json", makePath: func(path string) error {
			return os.Mkdir(path, 0o700)
		}},
		{name: "connector root as regular file", relative: "qwen", makePath: func(path string) error {
			return os.WriteFile(path, []byte("not a connector root"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := createReleaseSource(t, filepath.Join(releaseTestTempDir(t), "host"), "1.2.3", test.name, "agent-sessions")
			path := filepath.Join(request.SourceRoot, test.relative)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := test.makePath(path); err != nil {
				t.Fatal(err)
			}
			request = rewriteTestSourceManifest(t, request, func(*sourceReleaseManifest) {})
			if err := validateInstallRequest(request); err == nil {
				t.Fatalf("host manifest accepted %s with the wrong filesystem kind", test.relative)
			}
		})
	}
}

func TestInvalidSourceFilesystemTypesAreRejectedBeforeInstallMutation(t *testing.T) {
	for _, test := range invalidSourceEntryCases() {
		t.Run(test.name, func(t *testing.T) {
			source, version := generatedHostSource(t)
			test.create(t, filepath.Join(source, "unsupported-entry"))
			layout, err := ResolveRoleLayout(filepath.Join(releaseTestTempDir(t), "install"), RoleHost)
			if err != nil {
				t.Fatal(err)
			}
			service, hooks := &fakeRoleService{}, &fakeRoleHooks{}
			engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Install(context.Background(), InstallRequest{
				Version: version, ContentIdentity: testContentIdentity(test.name), SourceRoot: source,
				Executable: "agent-sessions",
			}); err == nil {
				t.Fatalf("install accepted a source containing a %s", test.name)
			}
			if _, err := os.Lstat(layout.ReleaseRoot); !os.IsNotExist(err) {
				t.Fatalf("invalid source created install-owned paths before rejection: %v", err)
			}
			if len(service.calls) != 0 || len(hooks.calls) != 0 {
				t.Fatalf("invalid source invoked service/hooks: %q / %q", service.calls, hooks.calls)
			}
		})
	}
}

func generatedHostSource(t *testing.T) (string, string) {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	versionBody, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy", "agent-sessions", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionBody))
	root := releaseTestTempDir(t)
	for _, path := range []string{
		".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills",
		".claude-plugin", "claude", "grok", "qwen", "bin",
	} {
		if path == "bin" {
			if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := createTestConnectorRoot(root, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "agent-sessions"), []byte("host image"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"deploy/agent-sessions/systemd/user/agent-sessions.service",
		"deploy/agent-sessions/launchd/net.antst.agent-sessions.plist",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte("service asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", filepath.Join(repositoryRoot, "scripts", "stage-release-metadata"),
		root, currentReleasePlatform(), version, "host")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("stage generated host source: %v: %s", err, output)
	}
	return root, version
}

func TestHostInstallStagingUsesCanonicalReleaseMetadataAndValidationIsReadOnly(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	versionBody, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy", "agent-sessions", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionBody))
	dryRun := exec.Command("make", "-n", "install-all", "PREFIX="+filepath.Join(releaseTestTempDir(t), "prefix"))
	dryRun.Dir = repositoryRoot
	dryRunBody, err := dryRun.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect supported install-all recipe: %v: %s", err, dryRunBody)
	}
	metadataIndex := strings.Index(string(dryRunBody), "scripts/stage-release-metadata")
	installIndex := strings.Index(string(dryRunBody), "lifecycle install")
	if metadataIndex < 0 || installIndex < 0 || metadataIndex >= installIndex {
		t.Fatalf("install-all does not stage canonical metadata before lifecycle install: %s", dryRunBody)
	}
	root := releaseTestTempDir(t)
	executable := filepath.Join(root, "bin", "agent-sessions")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("canonical staged host"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills",
		".claude-plugin", "claude", "grok", "qwen",
	} {
		if err := createTestConnectorRoot(root, path); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		"deploy/agent-sessions/systemd/user/agent-sessions.service",
		"deploy/agent-sessions/launchd/net.antst.agent-sessions.plist",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte("service asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", filepath.Join(repositoryRoot, "scripts", "stage-release-metadata"),
		root, currentReleasePlatform(), version, "host")
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	manifestPath, checksumsPath := filepath.Join(root, "manifest.json"), filepath.Join(root, "SHA256SUMS")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	checksumsBefore, err := os.ReadFile(checksumsPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ContentIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallRequest(InstallRequest{
		Version: version, ContentIdentity: identity, SourceRoot: root, Executable: "agent-sessions",
	}); err != nil {
		t.Fatal(err)
	}
	manifestAfter, manifestErr := os.ReadFile(manifestPath)
	checksumsAfter, checksumsErr := os.ReadFile(checksumsPath)
	if manifestErr != nil || checksumsErr != nil || !reflect.DeepEqual(manifestBefore, manifestAfter) ||
		!reflect.DeepEqual(checksumsBefore, checksumsAfter) {
		t.Fatal("install validation mutated canonical release metadata")
	}
}

func TestRoleLayoutsAreDisjointAndReleaseIdentityIncludesContent(t *testing.T) {
	root := releaseTestTempDir(t)
	host, err := ResolveRoleLayout(root, RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := ResolveRoleLayout(root, RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	for field, pair := range map[string][2]string{
		"release root": {host.ReleaseRoot, hub.ReleaseRoot}, "current selection": {host.CurrentSelection, hub.CurrentSelection},
		"transaction root": {host.TransactionRoot, hub.TransactionRoot}, "install lock": {host.InstallLock, hub.InstallLock},
	} {
		if pair[0] == pair[1] || strings.HasPrefix(pair[0], pair[1]+string(filepath.Separator)) ||
			strings.HasPrefix(pair[1], pair[0]+string(filepath.Separator)) {
			t.Errorf("%s is not role-disjoint: %q / %q", field, pair[0], pair[1])
		}
	}
	first, err := ReleaseID("1.2.3", testContentIdentity("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReleaseID("1.2.3", testContentIdentity("b"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.Contains(first, "1.2.3") || !strings.Contains(second, "1.2.3") {
		t.Fatalf("same-version content identities alias: %q / %q", first, second)
	}
}

func TestCurrentReleaseSelectionRequiresExactGeneratedRoleRelativeTarget(t *testing.T) {
	for _, target := range []string{
		"releases/a/b", "releases/../evil", "releases/not-generated", "/tmp/outside",
	} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			layout, err := ResolveRoleLayout(filepath.Join(releaseTestTempDir(t), "install"), RoleHost)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(layout.CurrentSelection), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, layout.CurrentSelection); err != nil {
				t.Fatal(err)
			}
			if _, err := currentReleaseID(layout); err == nil {
				t.Fatalf("current release accepted malformed target %q", target)
			}
		})
	}
}

func TestInstallStagesImmutableReleaseCommitsPointerAndRequiresReadiness(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	request := fixture.release("1.2.3", "a", "agent-sessions")
	fixture.hooks.wantReadyVersion = "1.2.3"
	fixture.hooks.wantReadyIdentity = request.ContentIdentity
	result, err := fixture.engine.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != PhaseComplete || result.ReleaseID == "" {
		t.Fatalf("install result = %+v", result)
	}
	selected, err := os.Readlink(fixture.layout.CurrentSelection)
	if err != nil {
		t.Fatal(err)
	}
	if selected != filepath.Join("releases", result.ReleaseID) {
		t.Fatalf("current selection = %q", selected)
	}
	releaseRoot := filepath.Join(fixture.layout.ReleasesRoot, result.ReleaseID)
	if fixture.hooks.preparedSource != releaseRoot || fixture.hooks.preparedSource == request.SourceRoot {
		t.Fatalf("prepare source = %q, want immutable release %q rather than input stage %q", fixture.hooks.preparedSource, releaseRoot, request.SourceRoot)
	}
	if err := filepath.Walk(releaseRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("staged release path remains writable: %s %v", path, info.Mode())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart"}) ||
		!reflect.DeepEqual(fixture.hooks.calls, []string{"prepare", "ready", "commit"}) {
		t.Fatalf("service/hooks calls = %q / %q", fixture.service.calls, fixture.hooks.calls)
	}
}

func TestExistingImmutableReleaseReuseRequiresExactContentIdentity(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	wanted := fixture.release("1.2.3", "wanted", "agent-sessions")
	wrong := fixture.release("1.2.3", "wrong", "agent-sessions")
	releaseID, err := ReleaseID(wanted.Version, wanted.ContentIdentity)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(fixture.layout.ReleasesRoot, releaseID)
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := copySecureReleaseTree(wrong.SourceRoot, destination)
	if err != nil {
		t.Fatal(err)
	}
	if identity == wanted.ContentIdentity {
		t.Fatal("wrong release fixture unexpectedly has the requested content identity")
	}
	if err := makeTreeImmutable(destination); err != nil {
		t.Fatal(err)
	}
	if err := stageImmutableRelease(wanted, destination); err == nil {
		t.Fatal("internally valid wrong content was reused under the requested release identity")
	}
}

func TestLoadActiveJournalRejectsIndirectUnboundedAndPermissiveState(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(*testing.T, string)
	}{
		{name: "symlink", create: func(t *testing.T, path string) {
			t.Helper()
			outside := filepath.Join(releaseTestTempDir(t), "outside.json")
			if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", create: func(t *testing.T, path string) {
			t.Helper()
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversize", create: func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, make([]byte, maximumReleaseJournalBytes+1), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "permissive", create: func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout, err := ResolveRoleLayout(filepath.Join(releaseTestTempDir(t), "install"), RoleHost)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(layout.TransactionRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			test.create(t, filepath.Join(layout.TransactionRoot, "active.json"))
			if _, err := LoadActiveJournal(layout); err == nil {
				t.Fatalf("LoadActiveJournal accepted %s state", test.name)
			}
		})
	}
	t.Run("permissive transaction directory", func(t *testing.T) {
		layout, err := ResolveRoleLayout(filepath.Join(releaseTestTempDir(t), "install"), RoleHost)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(layout.TransactionRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadActiveJournal(layout); err == nil {
			t.Fatal("LoadActiveJournal accepted a non-private transaction directory")
		}
	})
	t.Run("wrong owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("requires privilege to construct a wrong-owner journal")
		}
		layout, err := ResolveRoleLayout(filepath.Join(releaseTestTempDir(t), "install"), RoleHost)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(layout.TransactionRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(layout.TransactionRoot, "active.json")
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 1, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadActiveJournal(layout); err == nil {
			t.Fatal("LoadActiveJournal accepted a wrong-owner journal")
		}
	})
}

func TestReleaseStateDirectoryCreationRequiresDurableParentEntry(t *testing.T) {
	parent := releaseTestTempDir(t)
	target := filepath.Join(parent, "transactions")
	wantErr := errors.New("parent sync failed")
	directory, err := openOwnedReleaseDirectoryWithParentSync(target, true, func(int) error { return wantErr })
	if directory != nil {
		_ = directory.Close()
		t.Fatal("directory creation returned a handle after parent sync failure")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("directory creation error = %v, want %v", err, wantErr)
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("failed durable directory creation retained an unsynced entry: %v", statErr)
	}
	directory, err = openOwnedReleaseDirectory(target, true)
	if err != nil {
		t.Fatalf("retry durable directory creation: %v", err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLockRejectsIndirectAndPermissiveState(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(*testing.T, string)
	}{
		{name: "symlink", create: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(releaseTestTempDir(t), "outside"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", create: func(t *testing.T, path string) {
			t.Helper()
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "permissive", create: func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout, err := ResolveRoleLayout(filepath.Join(releaseTestTempDir(t), "install"), RoleHost)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(layout.ReleaseRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			test.create(t, layout.InstallLock)
			if unlock, err := acquireInstallLock(layout); err == nil {
				unlock()
				t.Fatalf("install lock accepted %s state", test.name)
			}
		})
	}
}

func TestLoadActiveJournalRejectsMismatchedAuthorityAndTrailingJSON(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "journal", "agent-sessions")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.layout.TransactionRoot, "active.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var original TransactionJournal
	if err := json.Unmarshal(body, &original); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*TransactionJournal) []byte
	}{
		{name: "release identity", mutate: func(journal *TransactionJournal) []byte {
			journal.ToRelease = "other-release"
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "role executable", mutate: func(journal *TransactionJournal) []byte {
			journal.Executable = "agent-sessions-hub"
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "unsafe prior", mutate: func(journal *TransactionJournal) []byte {
			journal.FromRelease = "../outside"
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "absent exact prior", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhasePointerCommitted
			journal.FromRelease = "9.9.9-0123456789abcdef"
			journal.ServiceTransitionAttempted = false
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "ready without service transition intent", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhaseReady
			journal.ServiceTransitionAttempted = false
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "staged with service transition intent", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhaseStaged
			journal.ServiceTransitionAttempted = true
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "prepared with service transition intent", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhasePrepared
			journal.ServiceTransitionAttempted = true
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "rolling back without failure authority", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhaseRollingBack
			journal.FailureCode = ""
			journal.RetryCode = ""
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "rolling back with retry authority", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhaseRollingBack
			journal.FailureCode = "failed"
			journal.RetryCode = "retry"
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "retryable rollback without original failure", mutate: func(journal *TransactionJournal) []byte {
			journal.Phase = PhaseRollbackRetryable
			journal.FailureCode = ""
			journal.RetryCode = "retry"
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "zero update", mutate: func(journal *TransactionJournal) []byte {
			journal.UpdatedAt = 0
			mutated, _ := json.Marshal(journal)
			return mutated
		}},
		{name: "trailing json", mutate: func(journal *TransactionJournal) []byte {
			mutated, _ := json.Marshal(journal)
			return append(mutated, []byte(` {"second":true}`)...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := original
			if err := os.WriteFile(path, test.mutate(&journal), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadActiveJournal(fixture.layout); err == nil {
				t.Fatalf("LoadActiveJournal accepted %s mismatch", test.name)
			}
			serviceCalls, hookCalls := len(fixture.service.calls), len(fixture.hooks.calls)
			if err := fixture.engine.Recover(context.Background()); err == nil {
				t.Fatalf("Recover accepted %s mismatch", test.name)
			}
			if len(fixture.service.calls) != serviceCalls || len(fixture.hooks.calls) != hookCalls {
				t.Fatalf("Recover mutated service/hooks for %s mismatch", test.name)
			}
		})
	}
}

func TestSameVersionDifferentContentStagesDistinctImmutableReleases(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	first, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "a", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "b", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ReleaseID == second.ReleaseID {
		t.Fatal("same-version releases with different bytes shared one release identity")
	}
	for _, id := range []string{first.ReleaseID, second.ReleaseID} {
		if info, statErr := os.Stat(filepath.Join(fixture.layout.ReleasesRoot, id)); statErr != nil || info.Mode().Perm()&0o222 != 0 {
			t.Errorf("release %s is missing or mutable: %v, %v", id, info, statErr)
		}
	}
}

func TestReadinessFailureRestoresExactPriorSelectionAndHookState(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	prior, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "a", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.calls, fixture.hooks.calls = nil, nil
	fixture.hooks.readyErr = errors.New("candidate identity does not match")
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "b", "agent-sessions")); err == nil {
		t.Fatal("readiness failure committed the candidate")
	}
	selected, err := os.Readlink(fixture.layout.CurrentSelection)
	if err != nil {
		t.Fatal(err)
	}
	if selected != filepath.Join("releases", prior.ReleaseID) {
		t.Fatalf("rollback selected %q, want prior %q", selected, prior.ReleaseID)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart", "stop", "start", "verify", "enable"}) ||
		!reflect.DeepEqual(fixture.hooks.calls, []string{"prepare", "ready", "rollback"}) {
		t.Fatalf("rollback service/hooks calls = %q / %q", fixture.service.calls, fixture.hooks.calls)
	}
}

func TestRollbackRestoresAllPriorServiceEnablementAndRunningStates(t *testing.T) {
	for _, wanted := range []RoleServiceState{
		{},
		{Enabled: true},
		{Running: true},
		{Enabled: true, Running: true},
	} {
		name := fmt.Sprintf("enabled_%t_running_%t", wanted.Enabled, wanted.Running)
		t.Run(name, func(t *testing.T) {
			fixture := newTransactionFixture(t, RoleHost)
			if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "prior", "agent-sessions")); err != nil {
				t.Fatal(err)
			}
			fixture.service.enabled = wanted.Enabled
			if wanted.Running {
				fixture.service.authorities = 1
			} else {
				fixture.service.authorities = 0
			}
			fixture.service.calls = nil
			fixture.hooks.readyErr = errors.New("candidate is not ready")
			if _, err := fixture.engine.Install(context.Background(), fixture.release("1.1.0", name, "agent-sessions")); err == nil {
				t.Fatal("readiness failure reported success")
			}
			got := RoleServiceState{Enabled: fixture.service.enabled, Running: fixture.service.authorities > 0}
			if got != wanted {
				t.Fatalf("restored service state = %+v, want %+v; calls=%q", got, wanted, fixture.service.calls)
			}
			journal, err := LoadActiveJournal(fixture.layout)
			if err != nil || journal.Phase != PhaseRolledBack || !journal.ServiceStateObserved ||
				journal.ServiceWasEnabled != wanted.Enabled || journal.ServiceWasRunning != wanted.Running ||
				!journal.CandidateStopped || !journal.CandidateDisabled || !journal.ServiceSurfaceReloaded ||
				!journal.ServiceEnablementRestored {
				t.Fatalf("terminal rollback journal = %+v, %v", journal, err)
			}
		})
	}
}

func TestFreshRecoveryAfterCrashAmbiguousCandidateStopDispatchesExactlyOnce(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	fixture.hooks.readyErr = errors.New("candidate is not ready")
	fixture.engine.SetCrashPoint(PhaseRollingBack)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "stop-crash", "agent-sessions")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("rollback crash = %v", err)
	}
	if err := fixture.service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.service.stopDispatches != 1 {
		t.Fatalf("physical stop dispatches before recovery = %d, want 1", fixture.service.stopDispatches)
	}
	freshHooks := &fakeRoleHooks{}
	fresh, err := NewEngine(EngineOptions{Layout: fixture.layout, Service: fixture.service, Hooks: freshHooks})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.service.stopDispatches != 1 {
		t.Fatalf("crash recovery redispatched candidate stop: %d", fixture.service.stopDispatches)
	}
	if fixture.service.authorities != 0 || fixture.service.enabled {
		t.Fatalf("failed first install remained active/enabled: %+v", fixture.service)
	}
	if !reflect.DeepEqual(freshHooks.calls, []string{"rollback"}) {
		t.Fatalf("fresh rollback hooks = %q, want one metadata/surface rollback", freshHooks.calls)
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil || journal.Phase != PhaseRolledBack || !journal.CandidateStopped || !journal.CandidateDisabled ||
		!journal.HooksRestored || !journal.ServiceSurfaceReloaded || !journal.ServiceEnablementRestored {
		t.Fatalf("recovered rollback journal = %+v, %v", journal, err)
	}
}

type candidateReplayAwareRoleService struct {
	*fakeRoleService
	candidateReady bool
}

func (service *candidateReplayAwareRoleService) VerifyCandidate(context.Context, InstalledRelease) error {
	if service.candidateReady {
		return nil
	}
	return errors.New("candidate is not ready")
}

func TestFreshRecoveryDoesNotRepeatCrashAmbiguousCandidateRestart(t *testing.T) {
	root := releaseTestTempDir(t)
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	layout, err := ResolveRoleLayout(filepath.Join(root, "install"), RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	service := &candidateReplayAwareRoleService{fakeRoleService: &fakeRoleService{}}
	hooks := &fakeRoleHooks{}
	engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(PhasePointerCommitted)
	request := createReleaseSource(t, filepath.Join(root, "source"), "1.0.0", "restart-crash", "agent-sessions")
	if _, err := engine.Install(context.Background(), request); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("pointer crash = %v", err)
	}
	journal, err := LoadActiveJournal(layout)
	if err != nil {
		t.Fatal(err)
	}
	journal.ServiceTransitionAttempted = true
	if err := saveJournal(layout, journal); err != nil {
		t.Fatal(err)
	}
	if err := service.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.candidateReady = true
	before := len(service.calls)
	fresh, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: &fakeRoleHooks{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(service.calls) != before {
		t.Fatalf("candidate recovery repeated restart: %q", service.calls)
	}
}

func TestFreshRollbackRecoveryDoesNotRepeatCrashAmbiguousPriorStart(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "prior-start", "agent-sessions")); err != nil {
		t.Fatal(err)
	}
	fixture.hooks.readyErr = errors.New("candidate is not ready")
	fixture.engine.SetCrashPoint(PhaseRollingBack)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.1.0", "prior-start-crash", "agent-sessions")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("rollback crash = %v", err)
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := selectRelease(fixture.layout, journal.FromRelease); err != nil {
		t.Fatal(err)
	}
	journal.CandidateStopped = true
	journal.CandidateDisabled = true
	journal.SelectionRestored = true
	journal.HooksRestored = true
	journal.ServiceSurfaceReloaded = true
	if err := saveJournal(fixture.layout, journal); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.service.startDispatches != 1 {
		t.Fatalf("physical prior start dispatches = %d, want 1", fixture.service.startDispatches)
	}
	fresh, err := NewEngine(EngineOptions{Layout: fixture.layout, Service: fixture.service, Hooks: &fakeRoleHooks{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.service.startDispatches != 1 {
		t.Fatalf("rollback recovery repeated prior start: %d", fixture.service.startDispatches)
	}
}

func TestCrashJournalRecoversPointerCommittedTransactionWithoutSecondAuthority(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	prior, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "a", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.engine.SetCrashPoint(PhasePointerCommitted)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.1.0", "b", "agent-sessions")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("pointer crash = %v", err)
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil || journal.Phase != PhasePointerCommitted || journal.FromRelease != prior.ReleaseID {
		t.Fatalf("crash journal = %+v, %v", journal, err)
	}
	fixture.engine.SetCrashPoint("")
	if err := fixture.engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if journal, err = LoadActiveJournal(fixture.layout); err != nil || journal.Phase != PhaseComplete {
		t.Fatalf("recovered journal = %+v, %v", journal, err)
	}
	if fixture.service.maxAuthorities > 1 {
		t.Fatalf("recovery observed %d simultaneous authorities", fixture.service.maxAuthorities)
	}
}

func TestInstallRefusesToOverwriteAnUnfinishedJournalBeforeRecovery(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	first := fixture.release("1.0.0", "unfinished", "agent-sessions")
	fixture.engine.SetCrashPoint(PhasePointerCommitted)
	if _, err := fixture.engine.Install(context.Background(), first); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("first install crash = %v", err)
	}
	second := fixture.release("2.0.0", "must-not-overwrite", "agent-sessions")
	if _, err := fixture.engine.Install(context.Background(), second); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("second install = %v, want recovery required", err)
	}
	journal, err := LoadActiveJournal(fixture.layout)
	firstID, _ := ReleaseID(first.Version, first.ContentIdentity)
	secondID, _ := ReleaseID(second.Version, second.ContentIdentity)
	if err != nil || journal.Phase != PhasePointerCommitted || journal.ToRelease != firstID {
		t.Fatalf("unfinished journal was overwritten: %+v, %v", journal, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.layout.ReleasesRoot, secondID)); !os.IsNotExist(err) {
		t.Fatalf("second release was staged before recovery: %v", err)
	}
}

func TestReadyPhaseRecoveryDoesNotRestartTheAlreadyReadyCandidate(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	fixture.engine.SetCrashPoint(PhaseReady)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "ready-crash", "agent-sessions")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("ready crash = %v", err)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart"}) {
		t.Fatalf("pre-recovery service calls = %q", fixture.service.calls)
	}
	fixture.engine.SetCrashPoint("")
	if err := fixture.engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart"}) {
		t.Fatalf("ready recovery restarted an already-ready candidate: %q", fixture.service.calls)
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil || journal.Phase != PhaseComplete {
		t.Fatalf("ready recovery journal = %+v, %v", journal, err)
	}
}

func TestRolePreflightRunsUnderSharedLockBeforeAnySecondRequestMutation(t *testing.T) {
	root := releaseTestTempDir(t)
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	layout, err := ResolveRoleLayout(filepath.Join(root, "install"), RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	firstBase, secondBase := &fakeRoleHooks{}, &fakeRoleHooks{}
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	firstHooks := &preflightRoleHooks{fakeRoleHooks: firstBase, entered: firstEntered, release: releaseFirst}
	var secondBlocked atomic.Bool
	secondEntered := make(chan struct{})
	secondHooks := &preflightRoleHooks{
		fakeRoleHooks: secondBase, entered: secondEntered, blocker: &secondBlocked,
	}
	firstService, secondService := &fakeRoleService{}, &fakeRoleService{}
	firstEngine, err := NewEngine(EngineOptions{Layout: layout, Service: firstService, Hooks: firstHooks})
	if err != nil {
		t.Fatal(err)
	}
	secondEngine, err := NewEngine(EngineOptions{Layout: layout, Service: secondService, Hooks: secondHooks})
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := createReleaseSource(t, filepath.Join(root, "first-source"), "1.0.0", "first", "agent-sessions")
	secondRequest := createReleaseSource(t, filepath.Join(root, "second-source"), "2.0.0", "second", "agent-sessions")
	firstResult := make(chan error, 1)
	go func() {
		_, installErr := firstEngine.Install(context.Background(), firstRequest)
		firstResult <- installErr
	}()
	<-firstEntered
	secondResult := make(chan error, 1)
	go func() {
		_, installErr := secondEngine.Install(context.Background(), secondRequest)
		secondResult <- installErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for secondEngine.mu.TryLock() {
		secondEngine.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("second install did not begin while first preflight held the shared lock")
		}
		runtime.Gosched()
	}
	select {
	case <-secondEntered:
		t.Fatal("second preflight crossed the first engine's held role lock")
	default:
	}
	secondBlocked.Store(true)
	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	if err := <-secondResult; err == nil || !strings.Contains(err.Error(), "new exact blocker") {
		t.Fatalf("second lock-held preflight error = %v", err)
	}
	journal, err := LoadActiveJournal(layout)
	firstID, _ := ReleaseID(firstRequest.Version, firstRequest.ContentIdentity)
	secondID, _ := ReleaseID(secondRequest.Version, secondRequest.ContentIdentity)
	if err != nil || journal.Phase != PhaseComplete || journal.ToRelease != firstID {
		t.Fatalf("second refusal changed first terminal journal: %+v, %v", journal, err)
	}
	if _, err := os.Lstat(filepath.Join(layout.ReleasesRoot, secondID)); !os.IsNotExist(err) {
		t.Fatalf("second refusal staged a release: %v", err)
	}
	if len(secondBase.calls) != 0 || len(secondService.calls) != 0 {
		t.Fatalf("second refusal invoked prepared hooks/service: %q / %q", secondBase.calls, secondService.calls)
	}
}

func TestPreflightSourceSwapCannotRedirectStagingReads(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, InstallRequest)
	}{
		{name: "executable symlink", mutate: func(t *testing.T, request InstallRequest) {
			path := filepath.Join(request.SourceRoot, "bin", request.Executable)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(request.SourceRoot, "manifest.json"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "checksum fifo", mutate: func(t *testing.T, request InstallRequest) {
			path := filepath.Join(request.SourceRoot, "SHA256SUMS")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "nested directory symlink", mutate: func(t *testing.T, request InstallRequest) {
			path := filepath.Join(request.SourceRoot, "qwen")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(request.SourceRoot, "docs"), path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTransactionFixture(t, RoleHost)
			request := fixture.release("1.2.3", strings.ReplaceAll(test.name, " ", "-"), "agent-sessions")
			base := &fakeRoleHooks{}
			hooks := &preflightRoleHooks{fakeRoleHooks: base, mutate: func() { test.mutate(t, request) }}
			engine, err := NewEngine(EngineOptions{Layout: fixture.layout, Service: fixture.service, Hooks: hooks})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Install(context.Background(), request); err == nil {
				t.Fatalf("install followed a preflight-swapped %s", test.name)
			}
			if _, err := os.Lstat(filepath.Join(fixture.layout.TransactionRoot, "active.json")); !os.IsNotExist(err) {
				t.Fatalf("source swap created a release journal: %v", err)
			}
			if len(base.calls) != 0 || len(fixture.service.calls) != 0 {
				t.Fatalf("source swap invoked prepare/service: %q / %q", base.calls, fixture.service.calls)
			}
		})
	}
}

func TestPrepareFailureRollsBackToDurableTerminalWithoutServiceTransition(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	fixture.hooks.prepareErr = errors.New("prepared legacy shutdown failed after durable selection")
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "prepare-fail", "agent-sessions")); err == nil {
		t.Fatal("prepare failure reported success")
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil || journal.Phase != PhaseRolledBack || journal.FailureCode != "role_prepare_failed" {
		t.Fatalf("prepare rollback journal = %+v, %v", journal, err)
	}
	if len(fixture.service.calls) != 0 {
		t.Fatalf("pre-selection rollback disrupted the selected service: %q", fixture.service.calls)
	}
	if !reflect.DeepEqual(fixture.hooks.calls, []string{"prepare", "rollback"}) {
		t.Fatalf("prepare rollback hooks = %q", fixture.hooks.calls)
	}
}

func TestRollbackRecoveryNeverStartsCandidateBesideRestoredLegacyAuthority(t *testing.T) {
	for _, test := range []struct {
		name             string
		crashRollingBack bool
		failRollbackOnce bool
		wantFirstPhase   Phase
	}{
		{name: "terminal rollback", wantFirstPhase: PhaseRolledBack},
		{name: "crash after durable rollback intent", crashRollingBack: true, wantFirstPhase: PhaseRollingBack},
		{name: "retryable hook restoration", failRollbackOnce: true, wantFirstPhase: PhaseRollbackRetryable},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := releaseTestTempDir(t)
			t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
			layout, err := ResolveRoleLayout(filepath.Join(root, "install"), RoleHost)
			if err != nil {
				t.Fatal(err)
			}
			tracker := &authorityTracker{legacy: 1, maximum: 1}
			service := &trackedRoleService{tracker: tracker}
			hooks := &migrationRollbackHooks{tracker: tracker, failRollbackOnce: test.failRollbackOnce}
			engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
			if err != nil {
				t.Fatal(err)
			}
			if test.crashRollingBack {
				engine.SetCrashPoint(PhaseRollingBack)
			}
			request := createReleaseSource(t, filepath.Join(root, "source"), "1.2.3", "migration", "agent-sessions")
			if _, err := engine.Install(context.Background(), request); err == nil {
				t.Fatal("candidate readiness failure reported success")
			}
			journal, err := LoadActiveJournal(layout)
			if err != nil || journal.Phase != test.wantFirstPhase {
				t.Fatalf("first rollback journal = %+v, %v", journal, err)
			}
			engine.SetCrashPoint("")
			if err := engine.Recover(context.Background()); err != nil {
				t.Fatalf("resume rollback: %v", err)
			}
			journal, err = LoadActiveJournal(layout)
			if err != nil || journal.Phase != PhaseRolledBack || journal.RetryCode != "" {
				t.Fatalf("terminal rollback journal = %+v, %v", journal, err)
			}
			callsBefore := append([]string(nil), service.calls...)
			if err := engine.Recover(context.Background()); err != nil {
				t.Fatalf("terminal recovery retry: %v", err)
			}
			if !reflect.DeepEqual(service.calls, callsBefore) {
				t.Fatalf("terminal rollback retry started a candidate: %q -> %q", callsBefore, service.calls)
			}
			if tracker.maximum > 1 || tracker.legacy != 1 || tracker.candidate != 0 {
				t.Fatalf("authority overlap/terminal state = %+v", tracker)
			}
		})
	}
}

func TestRoleClassifiedPostAuthorityFailuresRollForwardWithoutRestoringLegacy(t *testing.T) {
	for _, test := range []struct {
		name          string
		readyErrOnce  bool
		commitErrOnce bool
		classifierErr error
	}{
		{name: "readiness fails after authority commit", readyErrOnce: true},
		{name: "role commit fails after ready", commitErrOnce: true},
		{name: "classifier uncertainty fails forward", readyErrOnce: true, classifierErr: errors.New("migration journal unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := releaseTestTempDir(t)
			t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
			layout, err := ResolveRoleLayout(filepath.Join(root, "install"), RoleHost)
			if err != nil {
				t.Fatal(err)
			}
			service := &fakeRoleService{}
			hooks := &forwardRecoveryHooks{
				readyErrOnce:  test.readyErrOnce,
				commitErrOnce: test.commitErrOnce,
				classifierErr: test.classifierErr,
			}
			engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
			if err != nil {
				t.Fatal(err)
			}
			request := createReleaseSource(t, filepath.Join(root, "source"), "1.2.3", test.name, "agent-sessions")
			if _, err := engine.Install(context.Background(), request); err == nil {
				t.Fatal("post-authority failure reported success")
			}
			journal, err := LoadActiveJournal(layout)
			if err != nil || journal.Phase != PhaseRollForwardRetryable {
				t.Fatalf("post-authority journal = %+v, %v", journal, err)
			}
			if containsCall(service.calls, "stop") || hooks.rollbackCalls != 0 {
				t.Fatalf("post-authority failure attempted rollback: %q / %d", service.calls, hooks.rollbackCalls)
			}
			if err := engine.Recover(context.Background()); err != nil {
				t.Fatalf("roll-forward recovery: %v", err)
			}
			journal, err = LoadActiveJournal(layout)
			if err != nil || journal.Phase != PhaseComplete {
				t.Fatalf("roll-forward terminal journal = %+v, %v", journal, err)
			}
			if !reflect.DeepEqual(service.calls, []string{"restart"}) || hooks.rollbackCalls != 0 {
				t.Fatalf("roll-forward started/stopped another authority: %q / %d", service.calls, hooks.rollbackCalls)
			}
		})
	}
}

func TestUnclassifiedRoleCommitFailureRetainsRoleNeutralRollback(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHub)
	fixture.hooks.commitErr = errors.New("hub role commit failed")
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "hub-commit", "agent-sessions-hub")); err == nil {
		t.Fatal("unclassified role commit failure reported success")
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil || journal.Phase != PhaseRolledBack || journal.FailureCode != "role_commit_failed" {
		t.Fatalf("unclassified role rollback journal = %+v, %v", journal, err)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart", "stop", "disable"}) ||
		!reflect.DeepEqual(fixture.hooks.calls, []string{"prepare", "ready", "commit", "rollback"}) {
		t.Fatalf("unclassified rollback calls = %q / %q", fixture.service.calls, fixture.hooks.calls)
	}
}

func TestDurableRollForwardRecoveryCannotReclassifyBackToRollback(t *testing.T) {
	root := releaseTestTempDir(t)
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	layout, err := ResolveRoleLayout(filepath.Join(root, "install"), RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeRoleService{}
	hooks := &forwardRecoveryHooks{commitFailures: 2, rollbackAfterFirstClassification: true}
	engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	request := createReleaseSource(t, filepath.Join(root, "source"), "1.2.3", "durable-forward", "agent-sessions")
	if _, err := engine.Install(context.Background(), request); err == nil {
		t.Fatal("first role commit failure reported success")
	}
	if err := engine.Recover(context.Background()); err == nil {
		t.Fatal("second role commit failure reported success")
	}
	journal, err := LoadActiveJournal(layout)
	if err != nil || journal.Phase != PhaseRollForwardRetryable {
		t.Fatalf("durable forward journal was reclassified = %+v, %v", journal, err)
	}
	if hooks.dispositionCalls != 1 || hooks.rollbackCalls != 0 || containsCall(service.calls, "stop") {
		t.Fatalf("durable forward recovery consulted rollback again: classifications=%d rollback=%d service=%q",
			hooks.dispositionCalls, hooks.rollbackCalls, service.calls)
	}
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatalf("terminal forward recovery: %v", err)
	}
	journal, err = LoadActiveJournal(layout)
	if err != nil || journal.Phase != PhaseComplete {
		t.Fatalf("terminal forward journal = %+v, %v", journal, err)
	}
}

func TestRoleNeutralRemovalPreservesStateAndPurgeIsRevisionBound(t *testing.T) {
	for _, role := range []Role{RoleHost, RoleHub} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newTransactionFixture(t, role)
			if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "a", roleExecutable(role))); err != nil {
				t.Fatal(err)
			}
			preserved := filepath.Join(fixture.layout.PreservedStateRoot, "identity.json")
			if err := os.MkdirAll(filepath.Dir(preserved), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(preserved, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fixture.engine.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !containsCall(fixture.service.calls, "stop") || !containsCall(fixture.service.calls, "disable") ||
				!containsCall(fixture.hooks.calls, "remove") {
				t.Fatalf("normal removal omitted service stop/disable or role hook: %q / %q", fixture.service.calls, fixture.hooks.calls)
			}
			if _, err := os.Stat(fixture.layout.CurrentSelection); !os.IsNotExist(err) {
				t.Errorf("normal removal retained current selection: %v", err)
			}
			if body, err := os.ReadFile(preserved); err != nil || string(body) != "preserve" {
				t.Fatalf("normal removal changed preserved state: %q, %v", body, err)
			}
			plan, err := fixture.engine.PlanPurge(context.Background())
			if err != nil || plan.Role != role || plan.Revision == "" || len(plan.Targets) == 0 {
				t.Fatalf("purge plan = %+v, %v", plan, err)
			}
			if err := os.WriteFile(filepath.Join(fixture.layout.PreservedStateRoot, "revision-change"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fixture.engine.ApplyPurge(context.Background(), plan); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("stale purge plan = %v, want revision conflict", err)
			}
		})
	}
}

func TestTerminalUpgradeJournalDoesNotBlockReinstallAfterReleaseRemoval(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "first", "agent-sessions")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.1.0", "upgrade", "agent-sessions")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.engine.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.Install(context.Background(), fixture.release("2.0.0", "reinstall", "agent-sessions")); err != nil {
		t.Fatalf("reinstall after terminal removal: %v", err)
	}
}

func TestRoleInstallRejectsOppositeExecutableBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		role       Role
		executable string
	}{
		{role: RoleHost, executable: "agent-sessions-hub"},
		{role: RoleHub, executable: "agent-sessions"},
	} {
		t.Run(string(test.role), func(t *testing.T) {
			fixture := newTransactionFixture(t, test.role)
			request := fixture.release("1.0.0", "role-mismatch", test.executable)
			if _, err := fixture.engine.Install(context.Background(), request); err == nil ||
				!strings.Contains(err.Error(), "install requires executable") {
				t.Fatalf("opposite-role install error = %v", err)
			}
			if _, err := os.Lstat(fixture.layout.ReleaseRoot); !os.IsNotExist(err) {
				t.Fatalf("opposite-role install created role state: %v", err)
			}
		})
	}
}

func TestHostRemovalAndPurgeNeverSelectHubOwnedRoots(t *testing.T) {
	root := releaseTestTempDir(t)
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	hostLayout, err := ResolveRoleLayout(root, RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	hubLayout, err := ResolveRoleLayout(root, RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewEngine(EngineOptions{Layout: hostLayout, Service: &fakeRoleService{}, Hooks: &fakeRoleHooks{}})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewEngine(EngineOptions{Layout: hubLayout, Service: &fakeRoleService{}, Hooks: &fakeRoleHooks{}})
	if err != nil {
		t.Fatal(err)
	}
	hostRequest := createReleaseSource(t, filepath.Join(root, "host-source"), "1", "a", "agent-sessions")
	hubRequest := createReleaseSource(t, filepath.Join(root, "hub-source"), "9", "b", "agent-sessions-hub")
	if _, err := host.Install(context.Background(), hostRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Install(context.Background(), hubRequest); err != nil {
		t.Fatal(err)
	}
	if err := host.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(hubLayout.CurrentSelection); err != nil {
		t.Fatalf("host removal changed hub selection: %v", err)
	}
	plan, err := host.PlanPurge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range plan.Targets {
		if target == hubLayout.ReleaseRoot || strings.HasPrefix(target, hubLayout.ReleaseRoot+string(filepath.Separator)) {
			t.Errorf("host purge plan contains hub-owned target %q", target)
		}
	}
}

func TestPurgePlanBindsEveryTargetAndResumesAfterAnInterruptedPrefix(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	first := filepath.Join(releaseTestTempDir(fixture.t), "configuration")
	second := filepath.Join(releaseTestTempDir(fixture.t), "state")
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "owned.json"), []byte(root), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := NewEngine(EngineOptions{
		Layout: fixture.layout, Service: fixture.service, Hooks: &fakeRoleHooks{},
		PurgeTargets: []string{first, second}, PurgeExclusions: []string{filepath.Join(releaseTestTempDir(fixture.t), "vendor")},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.PlanPurge(context.Background())
	if err != nil || len(plan.TargetRevisions) != 2 || plan.Revision == "" {
		t.Fatalf("multi-target purge plan = %+v, %v", plan, err)
	}
	if err := removePurgeTarget(first); err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplyPurge(context.Background(), plan); err != nil {
		t.Fatalf("resume purge after an already-deleted exact prefix: %v", err)
	}
	for _, root := range []string{first, second} {
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Errorf("purge retained %s: %v", root, err)
		}
	}
}

type transactionFixture struct {
	t       *testing.T
	layout  RoleLayout
	engine  *Engine
	service *fakeRoleService
	hooks   *fakeRoleHooks
	source  string
}

func newTransactionFixture(t *testing.T, role Role) transactionFixture {
	t.Helper()
	root := releaseTestTempDir(t)
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	layout, err := ResolveRoleLayout(root, role)
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeRoleService{}
	hooks := &fakeRoleHooks{}
	source := filepath.Join(root, "sources")
	engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	return transactionFixture{t: t, layout: layout, engine: engine, service: service, hooks: hooks, source: source}
}

func createReleaseSource(t *testing.T, source, version, seed, executable string) InstallRequest {
	t.Helper()
	for _, directory := range []string{"bin", "docs"} {
		if err := os.MkdirAll(filepath.Join(source, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "bin", executable), []byte("test "+executable+" image "+seed), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sourceReleaseManifest{
		SchemaVersion: 1, ReleaseVersion: version, HubProtocolVersion: productcatalog.ProtocolVersion,
		Platform: currentReleasePlatform(), Checksums: "SHA256SUMS",
	}
	role := RoleHost
	if executable == "agent-sessions-hub" {
		role = RoleHub
	}
	manifest.Executables = []sourceReleaseExecutable{{Name: executable, Role: role, Path: "bin/" + executable}}
	if role == RoleHost {
		connectorPaths := [][]string{
			{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"},
			{".claude-plugin", "claude"}, {"grok"}, {"qwen"},
		}
		for index, product := range productcatalog.ProductDescriptors() {
			manifest.ConnectorPayloads = append(manifest.ConnectorPayloads, sourceReleaseConnector{
				Product: product.ID, PluginID: "agent-sessions", ArchivePaths: connectorPaths[index],
			})
			for _, path := range connectorPaths[index] {
				if err := createTestConnectorRoot(source, path); err != nil {
					t.Fatal(err)
				}
			}
		}
		manifest.ServiceAssets.Host = append([]string(nil), canonicalRoleServiceAssets[RoleHost]...)
	} else {
		manifest.ServiceAssets.Hub = append([]string(nil), canonicalRoleServiceAssets[RoleHub]...)
	}
	for _, path := range append(append([]string(nil), manifest.ServiceAssets.Host...), manifest.ServiceAssets.Hub...) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, path), []byte("test service "+seed), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestSourceChecksums(t, source)
	identity, err := ContentIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	return InstallRequest{Version: version, ContentIdentity: identity, SourceRoot: source, Executable: executable}
}

func rewriteTestSourceManifest(
	t *testing.T,
	request InstallRequest,
	mutate func(*sourceReleaseManifest),
) InstallRequest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(request.SourceRoot, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sourceReleaseManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(&manifest)
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(request.SourceRoot, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestSourceChecksums(t, request.SourceRoot)
	request.ContentIdentity, err = ContentIdentity(request.SourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func createTestConnectorRoot(root, relative string) error {
	path := filepath.Join(root, relative)
	if relative == ".mcp.json" {
		return os.WriteFile(path, []byte("{}\n"), 0o600)
	}
	return os.MkdirAll(path, 0o700)
}

func (f transactionFixture) release(version, identitySeed, executable string) InstallRequest {
	return createReleaseSource(f.t, filepath.Join(f.source, version+"-"+identitySeed), version, identitySeed, executable)
}

func writeTestSourceChecksums(t *testing.T, root string) {
	t.Helper()
	var rows []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "SHA256SUMS" {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		rows = append(rows, hex.EncodeToString(digest[:])+"  "+filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeRoleService struct {
	calls           []string
	authorities     int
	maxAuthorities  int
	enabled         bool
	stopDispatches  int
	startDispatches int
}

func (s *fakeRoleService) Observe(context.Context) (RoleServiceState, error) {
	return RoleServiceState{Enabled: s.enabled, Running: s.authorities > 0}, nil
}
func (*fakeRoleService) Reload(context.Context) error { return nil }
func (s *fakeRoleService) Enable(context.Context) error {
	s.calls = append(s.calls, "enable")
	s.enabled = true
	return nil
}

func (s *fakeRoleService) Restart(context.Context) error {
	s.calls = append(s.calls, "restart")
	s.enabled = true
	s.authorities = 1
	if s.authorities > s.maxAuthorities {
		s.maxAuthorities = s.authorities
	}
	return nil
}
func (s *fakeRoleService) Stop(context.Context) error {
	s.calls = append(s.calls, "stop")
	if s.authorities > 0 {
		s.stopDispatches++
	}
	s.authorities = 0
	return nil
}
func (s *fakeRoleService) Disable(context.Context) error {
	s.calls = append(s.calls, "disable")
	s.enabled = false
	return nil
}
func (s *fakeRoleService) Start(context.Context) error {
	s.calls = append(s.calls, "start")
	if s.authorities == 0 {
		s.startDispatches++
	}
	s.authorities = 1
	if s.authorities > s.maxAuthorities {
		s.maxAuthorities = s.authorities
	}
	return nil
}
func (s *fakeRoleService) Verify(context.Context) error {
	if s.authorities == 0 {
		return errors.New("service is not running")
	}
	s.calls = append(s.calls, "verify")
	return nil
}
func (*fakeRoleService) VerifyCandidate(context.Context, InstalledRelease) error {
	return errors.New("candidate is not already verified")
}

type fakeRoleHooks struct {
	calls             []string
	prepareErr        error
	readyErr          error
	commitErr         error
	wantReadyVersion  string
	wantReadyIdentity string
	preparedSource    string
}

func (h *fakeRoleHooks) Prepare(_ context.Context, request InstallRequest) error {
	h.calls = append(h.calls, "prepare")
	h.preparedSource = request.SourceRoot
	return h.prepareErr
}

type preflightRoleHooks struct {
	*fakeRoleHooks
	entered chan struct{}
	release <-chan struct{}
	blocker *atomic.Bool
	mutate  func()
}

func (hooks *preflightRoleHooks) Preflight(ctx context.Context, _ InstallRequest) error {
	if hooks.entered != nil {
		close(hooks.entered)
	}
	if hooks.release != nil {
		select {
		case <-hooks.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if hooks.mutate != nil {
		hooks.mutate()
	}
	if hooks.blocker != nil && hooks.blocker.Load() {
		return errors.New("new exact blocker")
	}
	return nil
}

type authorityTracker struct {
	candidate int
	legacy    int
	maximum   int
}

func (tracker *authorityTracker) observe() {
	if total := tracker.candidate + tracker.legacy; total > tracker.maximum {
		tracker.maximum = total
	}
}

type trackedRoleService struct {
	tracker *authorityTracker
	calls   []string
}

func (service *trackedRoleService) Observe(context.Context) (RoleServiceState, error) {
	return RoleServiceState{Enabled: service.tracker.candidate > 0, Running: service.tracker.candidate > 0}, nil
}
func (*trackedRoleService) Reload(context.Context) error { return nil }
func (*trackedRoleService) Enable(context.Context) error { return nil }

func (service *trackedRoleService) Restart(context.Context) error {
	service.calls = append(service.calls, "restart")
	service.tracker.candidate = 1
	service.tracker.observe()
	return nil
}

func (service *trackedRoleService) Stop(context.Context) error {
	service.calls = append(service.calls, "stop")
	service.tracker.candidate = 0
	service.tracker.observe()
	return nil
}

func (service *trackedRoleService) Disable(context.Context) error { return nil }

func (service *trackedRoleService) Start(context.Context) error {
	service.calls = append(service.calls, "start")
	service.tracker.candidate = 1
	service.tracker.observe()
	return nil
}

func (service *trackedRoleService) Verify(context.Context) error {
	service.calls = append(service.calls, "verify")
	return nil
}
func (*trackedRoleService) VerifyCandidate(context.Context, InstalledRelease) error {
	return errors.New("candidate is not already verified")
}

type migrationRollbackHooks struct {
	tracker          *authorityTracker
	failRollbackOnce bool
}

func (hooks *migrationRollbackHooks) Prepare(context.Context, InstallRequest) error {
	hooks.tracker.legacy = 0
	hooks.tracker.observe()
	return nil
}

func (*migrationRollbackHooks) Ready(context.Context, InstalledRelease) error {
	return errors.New("candidate is not ready")
}

func (*migrationRollbackHooks) Commit(context.Context) error { return nil }

func (hooks *migrationRollbackHooks) Rollback(context.Context) error {
	if hooks.failRollbackOnce {
		hooks.failRollbackOnce = false
		return errors.New("legacy restore is temporarily unavailable")
	}
	hooks.tracker.legacy = 1
	hooks.tracker.observe()
	return nil
}

func (*migrationRollbackHooks) Remove(context.Context) error { return nil }

type forwardRecoveryHooks struct {
	readyErrOnce                     bool
	commitErrOnce                    bool
	commitFailures                   int
	classifierErr                    error
	rollbackAfterFirstClassification bool
	dispositionCalls                 int
	authorityCommitted               bool
	rollbackCalls                    int
}

func (*forwardRecoveryHooks) Prepare(context.Context, InstallRequest) error { return nil }

func (hooks *forwardRecoveryHooks) Ready(context.Context, InstalledRelease) error {
	hooks.authorityCommitted = true
	if hooks.readyErrOnce {
		hooks.readyErrOnce = false
		return errors.New("legacy retirement requires retry after authority commit")
	}
	return nil
}

func (hooks *forwardRecoveryHooks) Commit(context.Context) error {
	if hooks.commitFailures > 0 {
		hooks.commitFailures--
		return errors.New("role commit requires retry")
	}
	if hooks.commitErrOnce {
		hooks.commitErrOnce = false
		return errors.New("role commit requires retry")
	}
	return nil
}

func (hooks *forwardRecoveryHooks) Rollback(context.Context) error {
	hooks.rollbackCalls++
	return nil
}

func (*forwardRecoveryHooks) Remove(context.Context) error { return nil }

func (hooks *forwardRecoveryHooks) FailureDisposition(context.Context, Phase) (FailureDisposition, error) {
	hooks.dispositionCalls++
	if hooks.classifierErr != nil {
		err := hooks.classifierErr
		hooks.classifierErr = nil
		return "", err
	}
	if hooks.authorityCommitted {
		if hooks.rollbackAfterFirstClassification && hooks.dispositionCalls > 1 {
			return FailureDispositionRollback, nil
		}
		return FailureDispositionRollForward, nil
	}
	return FailureDispositionRollback, nil
}

func (h *fakeRoleHooks) Ready(_ context.Context, release InstalledRelease) error {
	h.calls = append(h.calls, "ready")
	if h.wantReadyVersion != "" && (release.Version != h.wantReadyVersion || release.ContentIdentity != h.wantReadyIdentity) {
		return errors.New("readiness received the wrong release identity")
	}
	return h.readyErr
}
func (h *fakeRoleHooks) Commit(context.Context) error {
	h.calls = append(h.calls, "commit")
	return h.commitErr
}
func (h *fakeRoleHooks) Rollback(context.Context) error {
	h.calls = append(h.calls, "rollback")
	return nil
}
func (h *fakeRoleHooks) Remove(context.Context) error {
	h.calls = append(h.calls, "remove")
	return nil
}

func roleExecutable(role Role) string {
	if role == RoleHub {
		return "agent-sessions-hub"
	}
	return "agent-sessions"
}

func containsCall(calls []string, wanted string) bool {
	for _, call := range calls {
		if call == wanted {
			return true
		}
	}
	return false
}

func testContentIdentity(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)
}

func makeTestReleaseTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
