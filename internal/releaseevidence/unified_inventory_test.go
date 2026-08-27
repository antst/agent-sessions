package releaseevidence

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
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
	if value := strings.TrimSpace(string(body)); value == "" || strings.ContainsAny(value, " \t\r\n") {
		t.Fatalf("authoritative source-release version is not one non-empty token: %q", body)
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
