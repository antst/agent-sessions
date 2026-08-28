package releaseevidence

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestUnifiedReleaseHasOneAuthoritativeVersionFile(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	var versionFiles []string
	err := filepath.WalkDir(filepath.Join(root, "deploy"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "VERSION" {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			versionFiles = append(versionFiles, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inventory deploy version files: %v", err)
	}
	sort.Strings(versionFiles)
	want := []string{"deploy/agent-sessions/VERSION"}
	if !slices.Equal(versionFiles, want) {
		t.Fatalf("source-release VERSION authorities = %q, want sole authority %q", versionFiles, want)
	}
	body, err := os.ReadFile(filepath.Join(root, want[0]))
	if err != nil {
		t.Fatalf("read authoritative source-release version: %v", err)
	}
	if value := strings.TrimSpace(string(body)); value != "0.3.0" {
		t.Fatalf("authoritative source-release version = %q, want final unified release 0.3.0", value)
	} else if strings.ContainsAny(value, " \t\r\n") {
		t.Fatalf("authoritative source-release version is not one non-empty token: %q", body)
	}
	for _, manifest := range []string{
		".codex-plugin/plugin.json", "claude/.claude-plugin/plugin.json",
		"grok/.grok-plugin/plugin.json", "qwen/plugin.json",
	} {
		body, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifest)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var document struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &document) != nil || document.Version != "0.3.0" {
			t.Errorf("connector manifest %s does not carry final source release 0.3.0", manifest)
		}
	}

	for _, relative := range []string{
		"Makefile",
		".github/workflows/ci.yml",
		"scripts/release-tag-preflight",
		"scripts/release-tag-verify",
	} {
		body, readErr := os.ReadFile(filepath.Join(root, relative))
		if readErr != nil {
			t.Fatalf("read release consumer %s: %v", relative, readErr)
		}
		if strings.Contains(string(body), "deploy/peer-federator/VERSION") {
			t.Errorf("%s still consumes the legacy version authority", relative)
		}
		if !strings.Contains(string(body), "deploy/agent-sessions/VERSION") {
			t.Errorf("%s does not consume the sole source-release version authority", relative)
		}
	}
}

func TestReleaseInventoryGeneratesCompleteManifestAndVerifiesSourcePointers(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	inventory := filepath.Join(root, "scripts", "release-inventory")
	verify := exec.Command(inventory, "verify-source-pointers")
	verify.Dir = root
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("verify connector source pointers: %v: %s", err, output)
	}

	aliases := exec.Command(inventory, "host-aliases")
	aliases.Dir = root
	aliasBody, err := aliases.CombinedOutput()
	if err != nil {
		t.Fatalf("read host alias inventory: %v: %s", err, aliasBody)
	}
	if got, want := strings.Fields(string(aliasBody)), productcatalog.Catalog().HostAliases; !slices.Equal(got, want) {
		t.Fatalf("release host aliases = %q, want catalog %q", got, want)
	}
	protocol := exec.Command(inventory, "protocol-version")
	protocol.Dir = root
	protocolBody, err := protocol.CombinedOutput()
	if err != nil {
		t.Fatalf("read authoritative protocol version: %v: %s", err, protocolBody)
	}
	if got, want := strings.TrimSpace(string(protocolBody)), strconv.Itoa(productcatalog.ProtocolVersion); got != want {
		t.Fatalf("release protocol version = %q, want product catalog %d", got, productcatalog.ProtocolVersion)
	}

	for _, platform := range []string{"linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"} {
		command := exec.Command(inventory, "source-release-manifest-json", platform, "0.3.0")
		command.Dir = root
		body, manifestErr := command.CombinedOutput()
		if manifestErr != nil {
			t.Fatalf("generate %s source-release manifest: %v: %s", platform, manifestErr, body)
		}
		var manifest struct {
			SchemaVersion      int    `json:"schema_version"`
			ReleaseVersion     string `json:"release_version"`
			HubProtocolVersion int    `json:"hub_protocol_version"`
			Platform           string `json:"platform"`
			Executables        []any  `json:"executables"`
			ConnectorPayloads  []any  `json:"connector_payloads"`
			ServiceAssets      struct {
				Host []string `json:"host"`
				Hub  []string `json:"hub"`
			} `json:"service_assets"`
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatalf("decode %s source-release manifest: %v", platform, err)
		}
		if manifest.SchemaVersion != 1 || manifest.ReleaseVersion != "0.3.0" ||
			manifest.HubProtocolVersion != productcatalog.ProtocolVersion ||
			manifest.Platform != platform || len(manifest.Executables) != 2 || len(manifest.ConnectorPayloads) != 4 ||
			len(manifest.ServiceAssets.Host) == 0 || len(manifest.ServiceAssets.Hub) == 0 {
			t.Errorf("incomplete %s source-release manifest: %+v", platform, manifest)
		}
	}
}

func TestAggregateHostInstallSupportsEveryOptionalProductSubsetShape(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	for name, available := range map[string][]string{
		"zero products":    {},
		"one product":      {"codex"},
		"several products": {"claude", "qwen"},
		"all products":     {"codex", "claude", "grok", "qwen"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := t.TempDir()
			record := filepath.Join(fixture, "install-record")
			binaryRoot := filepath.Join(fixture, "bin")
			if err := os.MkdirAll(binaryRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			installer := filepath.Join(binaryRoot, "agent-sessions")
			writeEvidenceExecutable(t, installer, `#!/bin/sh
set -eu
printf 'install %s\n' "$*" >> "$INSTALL_RECORD"
source_root=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --source-root) source_root=$2; shift 2 ;;
    --codex|--claude|--grok|--qwen)
      product=${1#--}
      if command -v "$2" >/dev/null 2>&1; then
        printf 'available:%s\n' "$product" >> "$INSTALL_RECORD"
      fi
      shift 2 ;;
    *) shift ;;
  esac
done
test -x "$source_root/bin/agent-sessions"
test -f "$source_root/deploy/agent-sessions/VERSION"
test ! -e "$source_root/deploy/agent-sessions-hub"
`)
			availableSet := make(map[string]bool, len(available))
			for _, product := range available {
				availableSet[product] = true
			}
			productCommand := func(product string) string {
				path := filepath.Join(fixture, product)
				if availableSet[product] {
					writeEvidenceExecutable(t, path, "#!/bin/sh\nexit 0\n")
				}
				return path
			}

			command := exec.Command("make", "-s", "-C", root, "install-all",
				"BINARY_NAMES=", "BIN_DIR="+binaryRoot,
				"HOST_INSTALLER="+installer,
				"INSTALL_ROOT="+filepath.Join(fixture, "install"),
				"PREFIX="+filepath.Join(fixture, "prefix"),
				"CODEX="+productCommand("codex"),
				"CLAUDE="+productCommand("claude"),
				"GROK="+productCommand("grok"),
				"QWEN="+productCommand("qwen"))
			command.Env = append(os.Environ(), "INSTALL_RECORD="+record, "HOME="+filepath.Join(fixture, "home"))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("aggregate host install for %v: %v: %s", available, err, output)
			}
			body, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("read aggregate install plan: %v", err)
			}
			plan := strings.Split(strings.TrimSpace(string(body)), "\n")
			assertHostInstallProductPlan(t, plan, availableSet)
		})
	}
}

func TestAggregateHostInstallExcludesHubLifecycle(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(body)
	start := strings.Index(makefile, "\ninstall-all:")
	end := strings.Index(makefile, "\ndev-install-all:")
	if start < 0 || end <= start {
		t.Fatal("Makefile does not contain a bounded aggregate host install target")
	}
	hostInstall := makefile[start:end]
	for _, required := range []string{
		"lifecycle install", "--role host", "host-package-paths", "--codex", "--claude", "--grok", "--qwen",
	} {
		if !strings.Contains(hostInstall, required) {
			t.Errorf("aggregate host install omitted canonical token %q", required)
		}
	}
	for _, forbidden := range []string{"install-hub", "agent-sessions-hub", "agent-sessions-hub.service", "net.antst.agent-sessions-hub"} {
		if strings.Contains(hostInstall, forbidden) {
			t.Errorf("aggregate host install contains forbidden hub lifecycle token %q", forbidden)
		}
	}
}

func TestHubInstallStagesCanonicalMetadataAndExactBinary(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	fixture := t.TempDir()
	record := filepath.Join(fixture, "hub-install-record")
	installer := filepath.Join(fixture, "hub-installer")
	writeEvidenceExecutable(t, installer, `#!/bin/sh
set -eu
test "$1" = lifecycle
test "$2" = install
source_root=
role=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --source-root) source_root=$2; shift 2 ;;
    --role) role=$2; shift 2 ;;
    *) shift ;;
  esac
done
test "$role" = hub
test -x "$source_root/bin/agent-sessions-hub"
test ! -e "$source_root/bin/agent-sessions"
test -f "$source_root/manifest.json"
test -f "$source_root/SHA256SUMS"
grep -F '"name":"agent-sessions-hub","role":"hub","path":"bin/agent-sessions-hub"' "$source_root/manifest.json" >/dev/null
printf '%s\n' "$source_root" > "$INSTALL_RECORD"
`)
	command := exec.Command("make", "-s", "-C", root, "install-hub",
		"BIN_DIR="+filepath.Join(fixture, "bin"),
		"HUB_INSTALLER="+installer,
		"INSTALL_ROOT="+filepath.Join(fixture, "install"),
		"PREFIX="+filepath.Join(fixture, "prefix"))
	command.Env = append(os.Environ(), "INSTALL_RECORD="+record)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("canonical hub install: %v: %s", err, output)
	}
	if body, err := os.ReadFile(record); err != nil || strings.TrimSpace(string(body)) == "" {
		t.Fatalf("hub installer did not receive staged source: %q, %v", body, err)
	}
}

func TestRolePackageInventoriesAndPackagerCannotMutateLifecycle(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	readInventory := func(name string) []string {
		t.Helper()
		command := exec.Command(filepath.Join(root, "scripts", "release-inventory"), name)
		command.Dir = root
		body, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("read %s: %v: %s", name, err, body)
		}
		return strings.Fields(string(body))
	}
	host := readInventory("host-package-paths")
	hub := readInventory("hub-package-paths")
	for _, required := range []string{"deploy/agent-sessions", ".agents", "claude", "grok", "qwen"} {
		if !slices.Contains(host, required) {
			t.Errorf("host package paths omit %q: %q", required, host)
		}
	}
	if slices.Contains(host, "deploy/agent-sessions-hub") {
		t.Fatalf("host package paths include hub service assets: %q", host)
	}
	if !slices.Contains(hub, "deploy/agent-sessions-hub") || slices.Contains(hub, "deploy/agent-sessions") {
		t.Fatalf("hub package paths cross role ownership: %q", hub)
	}

	body, err := os.ReadFile(filepath.Join(root, "scripts", "package-release"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"lifecycle install", "systemctl", "launchctl", "install-all", "install-hub"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("release packager contains lifecycle mutation token %q", forbidden)
		}
	}
}

func TestUnifiedReleaseRemovesEveryObsoleteHostEntrypointAndServiceAsset(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	obsolete := []string{
		"cmd/agent-session-runtime/main.go", "cmd/codex-peer/main.go", "cmd/claude-peer/main.go", "cmd/grok-peer/main.go",
		"cmd/qwen-peer/main.go", "cmd/peer/main.go", "cmd/codex-peer-lane/main.go", "cmd/claude-peer-lane/main.go",
		"cmd/grok-peer-lane/main.go", "cmd/qwen-peer-lane/main.go", "cmd/peer-federator/main.go",
		"deploy/peer-federator/systemd/user/peer-federator-agent.service",
		"deploy/peer-federator/systemd/user/peer-federator-hub.service",
		"deploy/peer-federator/systemd/user/agent.env.example", "deploy/peer-federator/systemd/user/hub.env.example",
		"deploy/peer-federator/launchd/net.antst.peer-federator.agent.plist.example",
		"deploy/peer-federator/launchd/net.antst.peer-federator.hub.plist.example",
		"deploy/peer-federator/VERSION",
	}
	for _, relative := range obsolete {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); err == nil {
			t.Errorf("obsolete unified-topology surface still exists: %s", relative)
		} else if !os.IsNotExist(err) {
			t.Errorf("inspect obsolete surface %s: %v", relative, err)
		}
	}
}

func TestUnifiedReleaseRetiresLegacyQwenRunnersWithNamedReplacements(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	for _, relative := range []string{
		"scripts/test-qwen-contract",
		"scripts/test-qwen-lane-contract",
		"scripts/test-qwen-composition",
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); err == nil {
			t.Errorf("obsolete pre-unification runner still exists: %s", relative)
		} else if !os.IsNotExist(err) {
			t.Errorf("inspect obsolete runner %s: %v", relative, err)
		}
	}

	testScript, err := os.ReadFile(filepath.Join(root, "scripts", "test"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(testScript), "test-qwen-") {
		t.Fatal("repository test runner still references a retired Qwen runner")
	}

	evidence, err := os.ReadFile(filepath.Join(root, "specs", "002-unified-user-daemon", "evidence", "baseline-functional-cells.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(evidence)
	for _, obsolete := range []string{"scripts/test-qwen-contract", "scripts/test-qwen-lane-contract", "scripts/test-qwen-composition"} {
		if strings.Contains(body, obsolete) {
			t.Errorf("baseline evidence still links retired runner %q", obsolete)
		}
	}
	for _, replacement := range []string{
		"scripts/test-unified-peers",
		"scripts/test-unified-lane-restart",
		"scripts/test-unified-lane-composition",
		"Q-01 through Q-10",
		"L-01 through L-30",
		"unified.lane_composition.cell",
	} {
		if !strings.Contains(body, replacement) {
			t.Errorf("baseline evidence omits named replacement contract %q", replacement)
		}
	}
}

func TestUnifiedShippedGuidanceContainsNoRetiredRuntimeTopology(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	paths := []string{
		"README.md",
		"docs/ACCEPTANCE-MATRIX.md",
		"docs/CLAUDE-INSTALL.md",
		"docs/QWEN-INSTALL.md",
		"docs/federation/OPERATIONS.md",
		"skills/codex-lane/SKILL.md",
		"skills/claude-lane/SKILL.md",
		"skills/claude-lane/references/contract.md",
		"skills/grok-lane/SKILL.md",
		"skills/grok-lane/references/contract.md",
		"skills/qwen-lane/SKILL.md",
		"claude/skills/codex-lane/SKILL.md",
		"claude/skills/codex-lane/references/events.md",
		"claude/skills/codex-lane/references/failures.md",
		"claude/skills/codex-lane/references/policy.md",
		"claude/skills/codex-lane/scripts/lane-preflight",
		"claude/skills/grok-lane/SKILL.md",
		"claude/skills/grok-lane/references/contract.md",
		"claude/skills/grok-lane/scripts/lane-preflight",
	}
	for _, relative := range paths {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read shipped guidance %s: %v", relative, err)
		}
		lower := strings.ToLower(string(body))
		for _, forbidden := range []string{
			"peer-federator", "agent-session-runtime", "native-runtime-path",
			"host-agent", "lane manager", "lane-manager", "supervisor",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("shipped guidance %s retains obsolete topology term %q", relative, forbidden)
			}
		}
	}

	for _, relative := range []string{
		"skills/codex-lane/SKILL.md",
		"skills/claude-lane/SKILL.md",
		"skills/grok-lane/SKILL.md",
		"skills/qwen-lane/SKILL.md",
		"claude/skills/codex-lane/SKILL.md",
		"claude/skills/claude-lane/SKILL.md",
		"claude/skills/grok-lane/SKILL.md",
		"claude/skills/qwen-lane/SKILL.md",
	} {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read remote lane guidance %s: %v", relative, err)
		}
		for _, required := range []string{"remote-daemon", "source-proxy", "--prompt-file", "unsupported"} {
			if !strings.Contains(string(body), required) {
				t.Errorf("remote lane guidance %s omits %q", relative, required)
			}
		}
		for _, forbidden := range []string{"doctor contract 1", "doctor contract 2", "excluded from `--mine`", "destination-local file"} {
			if strings.Contains(strings.ToLower(string(body)), forbidden) {
				t.Errorf("remote lane guidance %s retains invalid contract %q", relative, forbidden)
			}
		}
	}
}

func TestUnifiedExecutableInventoryMapsEveryHostAliasToOneDistinctHostImage(t *testing.T) {
	catalog := productcatalog.Catalog()
	if got, ok := catalog.ResolveExecutable("agent-sessions-hub"); !ok || got != "agent-sessions-hub" {
		t.Fatalf("hub executable resolves to %q, %v; want its distinct image", got, ok)
	}
	if slices.Contains(catalog.HostAliases, "agent-sessions-hub") {
		t.Fatal("hub executable is incorrectly installed as a host alias")
	}
	for _, alias := range catalog.HostAliases {
		if got, ok := catalog.ResolveExecutable(alias); !ok || got != "agent-sessions" {
			t.Errorf("installed host alias %q resolves to %q, %v; want exact agent-sessions image", alias, got, ok)
		}
	}
	if got, ok := catalog.ResolveExecutable("peer-federator"); ok || got != "" {
		t.Fatalf("obsolete peer-federator remains executable inventory: %q, %v", got, ok)
	}
}

func assertHostInstallProductPlan(t *testing.T, plan []string, available map[string]bool) {
	t.Helper()
	if len(plan) == 0 || !strings.HasPrefix(plan[0], "install lifecycle install --role host ") {
		t.Fatalf("aggregate install omitted the host transaction: %q", plan)
	}
	for _, required := range []string{"--source-root ", "--prefix ", "--version ", "--codex ", "--claude ", "--grok ", "--qwen "} {
		if !strings.Contains(plan[0], required) {
			t.Errorf("host transaction omitted %q: %q", required, plan)
		}
	}
	for _, product := range []string{"claude", "grok", "qwen"} {
		got := false
		for _, row := range plan[1:] {
			got = got || row == "available:"+product
		}
		if got != available[product] {
			t.Errorf("%s availability probe=%v, available=%v: %q", product, got, available[product], plan)
		}
	}
	codexAvailable := false
	for _, row := range plan[1:] {
		codexAvailable = codexAvailable || row == "available:codex"
		if strings.Contains(row, "hub") {
			t.Errorf("host install plan contains hub action %q", row)
		}
	}
	if codexAvailable != available["codex"] {
		t.Errorf("codex availability probe=%v, available=%v: %q", codexAvailable, available["codex"], plan)
	}
}

func evidenceRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func writeEvidenceExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
