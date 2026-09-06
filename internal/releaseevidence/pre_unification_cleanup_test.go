package releaseevidence

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/antst/agent-sessions/internal/releasepkg"
)

type cleanupPlan struct {
	ContractID   string `json:"contract_id"`
	PlanRevision string `json:"plan_revision"`
	Mutation     bool   `json:"mutation"`
	Selected     []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"selected"`
}

type cleanupContract struct {
	SchemaVersion int    `json:"schema_version"`
	ContractID    string `json:"contract_id"`
	LegacySource  string `json:"legacy_source"`
	RevisionRule  string `json:"revision_rule"`
	Targets       []struct {
		ID                        string     `json:"id"`
		Platform                  string     `json:"platform"`
		Path                      string     `json:"path"`
		ExpectedType              string     `json:"expected_type"`
		Owner                     string     `json:"owner"`
		RemovalScope              string     `json:"removal_scope"`
		LegacySource              string     `json:"legacy_source"`
		PreRemove                 [][]string `json:"pre_remove"`
		AllowMissingPreRemoveTool bool       `json:"allow_missing_pre_remove_tool"`
		AllowPreRemoveFailure     bool       `json:"allow_pre_remove_failure"`
	} `json:"targets"`
}

func TestPreUnificationCleanupHandlesMissingOptionalToolsAndReadOnlyTrees(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports Linux and macOS")
	}
	repository := filepath.Join("..", "..")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".agent-sessions-cleanup-fixture"), "")
	home := filepath.Join(root, "home")
	grokPlugin := filepath.Join(home, ".grok", "plugins", "agent-sessions")
	qwenExtension := filepath.Join(home, ".qwen", "extensions", "agent-sessions")
	readOnlyDirectory := filepath.Join(home, ".local", "state", "claude-code-peer", "profiles", "fixture", "worktree")
	for _, directory := range []string{grokPlugin, qwenExtension, readOnlyDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(directory, "residue"), "legacy residue\n")
	}
	if err := os.Chmod(readOnlyDirectory, 0o500); err != nil {
		t.Fatal(err)
	}

	plan := runCleanup(t, repository, root)
	if len(plan.Selected) != 3 {
		t.Fatalf("selected = %#v, want Grok, Qwen, and read-only legacy state", plan.Selected)
	}
	apply := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"), "--apply", plan.PlanRevision)
	apply.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	if output, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply cleanup with missing optional tools and read-only tree: %v: %s", err, output)
	}
	for _, removed := range []string{grokPlugin, qwenExtension, filepath.Join(home, ".local", "state", "claude-code-peer")} {
		if _, err := os.Lstat(removed); !os.IsNotExist(err) {
			t.Fatalf("legacy residue remains: %s: %v", removed, err)
		}
	}

	if err := os.MkdirAll(grokPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(grokPlugin, "residue"), "legacy residue\n")
	failingTool := filepath.Join(root, "failing-grok-peer")
	writeTestFile(t, failingTool, "#!/bin/sh\nexit 19\n")
	if err := os.Chmod(failingTool, 0o700); err != nil {
		t.Fatal(err)
	}
	grokAlias := filepath.Join(home, ".local", "bin", "grok-peer")
	if err := os.MkdirAll(filepath.Dir(grokAlias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(failingTool, grokAlias); err != nil {
		t.Fatal(err)
	}
	failingPlan := runCleanup(t, repository, root)
	apply = exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"), "--apply", failingPlan.PlanRevision)
	apply.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	if output, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply optional cleanup with non-idempotent native remover: %v: %s", err, output)
	}
	for _, removed := range []string{grokPlugin, grokAlias} {
		if _, err := os.Lstat(removed); !os.IsNotExist(err) {
			t.Fatalf("optional legacy residue remains after remover failure: %s: %v", removed, err)
		}
	}

	mandatoryPlugin := filepath.Join(home, ".codex", "plugins", "cache", "agent-sessions")
	if err := os.MkdirAll(mandatoryPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"))
	command.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "legacy-codex-plugin-cache requires unavailable exact tool") {
		t.Fatalf("missing mandatory registration tool was accepted: err=%v output=%s", err, output)
	}
}

func TestPreUnificationCleanupCanonicalizesFixedTmpSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports Linux and macOS")
	}
	repository := filepath.Join("..", "..")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".agent-sessions-cleanup-fixture"), "")
	canonicalTmp := filepath.Join(root, "private", "tmp")
	if err := os.MkdirAll(canonicalTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonicalTmp, filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	legacyRuntime := filepath.Join(canonicalTmp, "ccp-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(legacyRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyRuntime, err := filepath.EvalSymlinks(legacyRuntime)
	if err != nil {
		t.Fatal(err)
	}

	plan := runCleanup(t, repository, root)
	expectedID := "legacy-runtime-bridge-linux"
	if runtime.GOOS == "darwin" {
		expectedID = "legacy-runtime-bridge-darwin-compact"
	}
	if len(plan.Selected) != 1 || plan.Selected[0].ID != expectedID || plan.Selected[0].Path != legacyRuntime {
		t.Fatalf("canonical /tmp plan = %#v, want %s", plan.Selected, legacyRuntime)
	}
	apply := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"), "--apply", plan.PlanRevision)
	apply.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	if output, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply canonical /tmp cleanup: %v: %s", err, output)
	}
	if _, err := os.Lstat(legacyRuntime); !os.IsNotExist(err) {
		t.Fatalf("canonical /tmp residue remains: %v", err)
	}
}

func TestPreUnificationCleanupExpandsUIDAfterCanonicalizingSystemTmp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports Linux and macOS")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repository, "scripts", "cleanup-pre-unification")
	probe := `
import importlib.util, importlib.machinery, os, sys
loader = importlib.machinery.SourceFileLoader("cleanup_pre_unification", sys.argv[1])
spec = importlib.util.spec_from_loader(loader.name, loader)
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
print(module.expand_path("/tmp/ccp-${UID}", "/home/test", "/run/user/test", os.path.realpath("/tmp")))
`
	command := exec.Command("python3", "-c", probe, script)
	command.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("probe cleanup path expansion: %v: %s", err, output)
	}
	canonicalTmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalTmp, "ccp-"+strconv.Itoa(os.Getuid()))
	if got := strings.TrimSpace(string(output)); got != want {
		t.Fatalf("expanded system tmp path = %q, want %q", got, want)
	}
}

func TestPreUnificationCleanupPlansAppliesAndConverges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports Linux and macOS")
	}
	repository := filepath.Join("..", "..")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".agent-sessions-cleanup-fixture"), "")
	home := filepath.Join(root, "home")
	legacyInstall := filepath.Join(root, "home", ".local", "libexec", "agent-sessions")
	legacyState := filepath.Join(root, "home", ".local", "state", "claude-code-peer")
	aliasRoot := filepath.Join(root, "home", ".local", "bin")
	pluginRoots := []string{
		filepath.Join(home, ".codex", "plugins", "cache", "agent-sessions"),
		filepath.Join(home, ".claude", "plugins", "cache", "agent-sessions"),
		filepath.Join(home, ".grok", "plugins", "agent-sessions"),
		filepath.Join(home, ".qwen", "extensions", "agent-sessions"),
	}
	for _, directory := range []string{
		filepath.Join(legacyInstall, "bin", "linux-x64"),
		filepath.Join(legacyState, "sessions", "fixture"),
		aliasRoot,
		pluginRoots[0], pluginRoots[1], pluginRoots[2], pluginRoots[3],
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(legacyState, "sessions", "fixture", "opaque.json"), "opaque legacy bytes\n")
	if err := syscall.Mkfifo(filepath.Join(legacyState, "sessions", "fixture", "must-not-be-opened"), 0o600); err != nil {
		t.Fatal(err)
	}

	canaries := map[string]string{
		filepath.Join(home, ".codex", "auth.json"):                               "codex credential\n",
		filepath.Join(home, ".codex", "sessions", "transcript.jsonl"):            "codex transcript\n",
		filepath.Join(home, ".codex", "config.toml"):                             "codex setting\n",
		filepath.Join(home, ".codex", "ordinary.txt"):                            "codex ordinary\n",
		filepath.Join(home, ".codex", "plugins", "cache", "unrelated", "x"):      "codex unrelated plugin\n",
		filepath.Join(home, ".claude", ".credentials.json"):                      "claude credential\n",
		filepath.Join(home, ".claude", "projects", "history.jsonl"):              "claude history\n",
		filepath.Join(home, ".claude", "settings.json"):                          "claude setting\n",
		filepath.Join(home, ".claude", "ordinary.txt"):                           "claude ordinary\n",
		filepath.Join(home, ".claude", "plugins", "cache", "unrelated", "x"):     "claude unrelated plugin\n",
		filepath.Join(home, ".grok", "credentials.json"):                         "grok credential\n",
		filepath.Join(home, ".grok", "history.jsonl"):                            "grok history\n",
		filepath.Join(home, ".grok", "settings.json"):                            "grok setting\n",
		filepath.Join(home, ".grok", "ordinary.txt"):                             "grok ordinary\n",
		filepath.Join(home, ".grok", "plugins", "unrelated", "x"):                "grok unrelated plugin\n",
		filepath.Join(home, ".qwen", "oauth_creds.json"):                         "qwen credential\n",
		filepath.Join(home, ".qwen", "history.jsonl"):                            "qwen history\n",
		filepath.Join(home, ".qwen", "settings.json"):                            "qwen setting\n",
		filepath.Join(home, ".qwen", "ordinary.txt"):                             "qwen ordinary\n",
		filepath.Join(home, ".qwen", "extensions", "unrelated", "manifest.json"): "qwen unrelated plugin\n",
	}
	for path, body := range canaries {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, body)
	}
	cleanupToolLog := filepath.Join(root, "cleanup-tool.log")
	toolBody := "#!/bin/sh\nprintf '%s:%s\\n' \"${0##*/}\" \"$*\" >>\"$FAKE_CLEANUP_TOOL_LOG\"\n"
	for _, name := range []string{"codex", "claude", "qwen"} {
		tool := filepath.Join(aliasRoot, name)
		writeTestFile(t, tool, toolBody)
		if err := os.Chmod(tool, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	grokTool := filepath.Join(root, "tools", "grok-peer")
	if err := os.MkdirAll(filepath.Dir(grokTool), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, grokTool, toolBody)
	if err := os.Chmod(grokTool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(grokTool, filepath.Join(aliasRoot, "grok-peer")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(legacyInstall, "bin", "linux-x64", "agent-session-runtime"),
		filepath.Join(aliasRoot, "agent-session-runtime")); err != nil {
		t.Fatal(err)
	}

	plan, firstPlanBytes := runCleanupRaw(t, repository, root)
	secondPlan, secondPlanBytes := runCleanupRaw(t, repository, root)
	if !reflect.DeepEqual(firstPlanBytes, secondPlanBytes) || plan.PlanRevision != secondPlan.PlanRevision {
		t.Fatalf("identical metadata produced a non-deterministic plan:\n%s\n%s", firstPlanBytes, secondPlanBytes)
	}
	if plan.Mutation || plan.ContractID == "" || plan.PlanRevision == "" || len(plan.Selected) != 8 {
		t.Fatalf("initial cleanup plan = %#v", plan)
	}
	selectedIDs := make([]string, 0, len(plan.Selected))
	for _, selected := range plan.Selected {
		selectedIDs = append(selectedIDs, selected.ID)
	}
	if !sort.StringsAreSorted(selectedIDs) {
		t.Fatalf("cleanup plan is not stable-ID ordered: %q", selectedIDs)
	}
	if _, err := os.Stat(filepath.Join(legacyState, "sessions", "fixture", "opaque.json")); err != nil {
		t.Fatalf("plan-only mode mutated legacy state: %v", err)
	}

	if err := os.Symlink(filepath.Join(legacyInstall, "bin", "linux-x64", "peer-federator"),
		filepath.Join(aliasRoot, "peer-federator")); err != nil {
		t.Fatal(err)
	}
	stale := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"), "--apply", plan.PlanRevision)
	stale.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	if output, err := stale.CombinedOutput(); err == nil || !strings.Contains(string(output), "nothing removed") {
		t.Fatalf("changed plan was not rejected before mutation: err=%v output=%s", err, output)
	}
	if _, err := os.Stat(legacyInstall); err != nil {
		t.Fatalf("stale apply mutated the first target: %v", err)
	}

	plan = runCleanup(t, repository, root)
	unrelated := exec.Command("/bin/sleep", "300")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	}()
	apply := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"), "--apply", plan.PlanRevision)
	apply.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root, "FAKE_CLEANUP_TOOL_LOG="+cleanupToolLog)
	output, err := apply.CombinedOutput()
	if err != nil {
		t.Fatalf("apply cleanup: %v: %s", err, output)
	}
	var applied cleanupPlan
	if err := json.Unmarshal(output, &applied); err != nil || !applied.Mutation {
		t.Fatalf("applied result = %s, err=%v", output, err)
	}
	removedPaths := make([]string, 0, 5+len(pluginRoots))
	removedPaths = append(removedPaths, legacyInstall, legacyState,
		filepath.Join(aliasRoot, "agent-session-runtime"), filepath.Join(aliasRoot, "peer-federator"),
		filepath.Join(aliasRoot, "grok-peer"))
	removedPaths = append(removedPaths, pluginRoots...)
	for _, removed := range removedPaths {
		if _, err := os.Lstat(removed); !os.IsNotExist(err) {
			t.Fatalf("legacy target remains: %s: %v", removed, err)
		}
	}
	for path, want := range canaries {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("vendor canary changed: %s: %q, %v", path, body, err)
		}
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was not preserved: %v", err)
	}
	toolLog, err := os.ReadFile(cleanupToolLog)
	wantToolLog := "claude:plugin uninstall --scope user agent-sessions@agent-sessions\n" +
		"claude:plugin marketplace remove --scope user agent-sessions\n" +
		"codex:plugin remove agent-sessions@agent-sessions\n" +
		"codex:plugin marketplace remove agent-sessions\n" +
		"grok-peer:plugin uninstall agent-sessions --keep-data\n" +
		"qwen:extensions uninstall agent-sessions\n"
	if err != nil || string(toolLog) != wantToolLog {
		t.Fatalf("native registration cleanup = %q, %v", toolLog, err)
	}
	if second := runCleanup(t, repository, root); len(second.Selected) != 0 || second.Mutation {
		t.Fatalf("second cleanup plan did not converge: %#v", second)
	}
}

func TestPreUnificationCleanupRefusesUnifiedInstallation(t *testing.T) {
	repository := filepath.Join("..", "..")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".agent-sessions-cleanup-fixture"), "")
	unified := filepath.Join(root, "home", ".local", "libexec", "agent-sessions", "host")
	legacy := filepath.Join(root, "home", ".local", "state", "claude-code-peer")
	if err := os.MkdirAll(unified, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"))
	command.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "refuse pre-unification cleanup after unified host installation") {
		t.Fatalf("unified installation was not protected: err=%v output=%s", err, output)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("refused cleanup mutated legacy state: %v", err)
	}
}

func TestPreUnificationCleanupRevalidatesEachTargetImmediatelyBeforeDeletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports Linux and macOS")
	}
	repository := filepath.Join("..", "..")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".agent-sessions-cleanup-fixture"), "")
	home := filepath.Join(root, "home")
	plugin := filepath.Join(home, ".codex", "plugins", "cache", "agent-sessions")
	laterTarget := filepath.Join(home, ".local", "state", "claude-code-peer")
	if err := os.MkdirAll(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(laterTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(home, ".local", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(tool), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, tool, "#!/bin/sh\n/usr/bin/touch \"$FAKE_MUTATE_TARGET/changed-after-enumeration\"\n")
	if err := os.Chmod(tool, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := runCleanup(t, repository, root)
	if len(plan.Selected) != 2 {
		t.Fatalf("selected = %#v, want the plugin and later state target", plan.Selected)
	}
	apply := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"), "--apply", plan.PlanRevision)
	apply.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root, "FAKE_MUTATE_TARGET="+laterTarget)
	output, err := apply.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "cleanup target changed before deletion: legacy-state-bridge") {
		t.Fatalf("per-target metadata change did not fail closed: err=%v output=%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(laterTarget, "changed-after-enumeration")); err != nil {
		t.Fatalf("changed target was unexpectedly deleted: %v", err)
	}
}

func TestPreUnificationCleanupRequiresFixtureOwnershipAndNoSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports Linux and macOS")
	}
	repository := filepath.Join("..", "..")
	tool := filepath.Join(repository, "scripts", "cleanup-pre-unification")
	root := t.TempDir()
	command := exec.Command(tool)
	command.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "is not an explicit fixture-owned root") {
		t.Fatalf("unmarked fixture root was accepted: err=%v output=%s", err, output)
	}

	writeTestFile(t, filepath.Join(root, ".agent-sessions-cleanup-fixture"), "")
	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(root, "home")); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(tool)
	command.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "symlink ancestor") {
		t.Fatalf("fixture symlink escape was accepted: err=%v output=%s", err, output)
	}
}

func TestPreUnificationCleanupContractAndOperationalBoundary(t *testing.T) {
	repository := filepath.Join("..", "..")
	contractPath := filepath.Join(repository, "specs", "002-unified-user-daemon", "contracts", "pre-unification-cleanup.yml")
	body, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract cleanupContract
	if err := json.Unmarshal(body, &contract); err != nil {
		t.Fatalf("cleanup contract is not dependency-free YAML/JSON: %v", err)
	}
	if contract.SchemaVersion != 1 || contract.ContractID != "pre-unification-cleanup-c056fbc-v1" ||
		contract.LegacySource != "c056fbc5015d4ab0a673f66cac5404206f7bcee6" ||
		contract.RevisionRule != "lstat-v1:type,uid,gid,mode,device,inode,size,mtime_ns" {
		t.Fatalf("cleanup contract identity/inventory is incomplete: %#v", contract)
	}
	wantIDs := []string{
		"legacy-bin-agent-session-runtime", "legacy-bin-claude-peer", "legacy-bin-claude-peer-lane",
		"legacy-bin-codex-peer", "legacy-bin-codex-peer-lane", "legacy-bin-grok-peer", "legacy-bin-grok-peer-lane",
		"legacy-bin-peer", "legacy-bin-peer-federator", "legacy-bin-qwen-peer", "legacy-bin-qwen-peer-lane",
		"legacy-claude-marketplaces", "legacy-claude-plugin-cache", "legacy-codex-plugin-cache",
		"legacy-federator-config", "legacy-federator-docs", "legacy-grok-plugin", "legacy-install-root",
		"legacy-launchd-agent-log", "legacy-launchd-agent-template", "legacy-launchd-agent-unit",
		"legacy-launchd-hub-log", "legacy-launchd-hub-template", "legacy-launchd-hub-unit",
		"legacy-qwen-extension", "legacy-runtime-bridge-darwin", "legacy-runtime-bridge-darwin-compact",
		"legacy-runtime-bridge-linux", "legacy-runtime-bridge-xdg", "legacy-runtime-federator-darwin",
		"legacy-runtime-federator-xdg", "legacy-state-bridge", "legacy-state-federation",
		"legacy-systemd-agent-unit", "legacy-systemd-agent-wants", "legacy-systemd-hub-unit", "legacy-systemd-hub-wants",
	}
	sort.Strings(wantIDs)
	seen := map[string]bool{}
	paths := map[string]bool{}
	gotIDs := make([]string, 0, len(contract.Targets))
	for _, target := range contract.Targets {
		if target.ID == "" || seen[target.ID] || target.Path == "" || target.ExpectedType == "" ||
			target.Owner != "current-user" || target.RemovalScope == "" || target.LegacySource == "" ||
			strings.ContainsAny(target.Path, "*?[]") || paths[target.Platform+"\x00"+target.Path] ||
			(target.Platform != "all" && target.Platform != "linux" && target.Platform != "darwin") {
			t.Fatalf("unsafe or incomplete cleanup target: %#v", target)
		}
		seen[target.ID] = true
		paths[target.Platform+"\x00"+target.Path] = true
		gotIDs = append(gotIDs, target.ID)
		if strings.Contains(target.ID, "plugin") || strings.Contains(target.ID, "extension") {
			if len(target.PreRemove) == 0 {
				t.Fatalf("legacy integration has no native registration removal: %#v", target)
			}
		}
		if target.AllowMissingPreRemoveTool && target.ID != "legacy-grok-plugin" && target.ID != "legacy-qwen-extension" {
			t.Fatalf("unexpected missing-tool exception: %#v", target)
		}
	}
	sort.Strings(gotIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("cleanup target IDs differ from reviewed c056fbc inventory:\ngot  %q\nwant %q", gotIDs, wantIDs)
	}
	for _, operational := range []string{"Makefile", "scripts/install-host", "scripts/remove-host"} {
		text, err := os.ReadFile(filepath.Join(repository, operational))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(text), "cleanup-pre-unification") || strings.Contains(string(text), "pre-unification-cleanup.yml") {
			t.Fatalf("standard lifecycle surface names repository-only cleanup: %s", operational)
		}
	}
	for _, active := range []string{
		"Makefile", "scripts/release-inventory", "scripts/package-release",
		"scripts/install-host", "scripts/install-hub", "scripts/remove-host", "scripts/remove-hub",
		"deploy/agent-sessions/systemd/user/agent-sessions.service",
		"deploy/agent-sessions/launchd/net.antst.agent-sessions.plist",
	} {
		text, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(active)))
		if err != nil {
			t.Fatal(err)
		}
		for _, obsolete := range []string{"peer-federator", "agent-session-runtime", "codex-messaging"} {
			if strings.Contains(string(text), obsolete) {
				t.Fatalf("active greenfield lifecycle surface %s contains obsolete authority %q", active, obsolete)
			}
		}
	}
	for _, removed := range []string{
		"cmd/agent-session-runtime", "cmd/peer-federator", "deploy/peer-federator",
		"cmd/grok-peer", "cmd/qwen-peer", "cmd/peer",
		"cmd/codex-peer-lane", "cmd/claude-peer-lane", "cmd/grok-peer-lane", "cmd/qwen-peer-lane",
	} {
		if _, err := os.Lstat(filepath.Join(repository, filepath.FromSlash(removed))); !os.IsNotExist(err) {
			t.Fatalf("obsolete production surface still exists: %s: %v", removed, err)
		}
	}
	packageScript, err := os.ReadFile(filepath.Join(repository, "scripts", "package-release"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(packageScript), "cleanup-pre-unification") ||
		strings.Contains(string(packageScript), "pre-unification-cleanup.yml") {
		t.Fatal("release packaging names a repository-only cleanup artifact instead of using a positive payload allowlist")
	}
	cleanupScript, err := os.ReadFile(filepath.Join(repository, "scripts", "cleanup-pre-unification"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupSource := string(cleanupScript)
	for _, required := range []string{"for row in contract[\"targets\"]", "os.lstat(", "os.scandir("} {
		if !strings.Contains(cleanupSource, required) {
			t.Fatalf("cleanup utility lacks exact-contract/no-follow primitive %q", required)
		}
	}
	for _, discovery := range []string{"glob.glob(", ".glob(", ".rglob(", "os.walk(", "os.fwalk(", "psutil", "/proc/", "pgrep", "find -"} {
		if strings.Contains(cleanupSource, discovery) {
			t.Fatalf("cleanup utility contains forbidden discovery primitive %q", discovery)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("cleanup utility supports the two release host platforms")
	}
}

func TestPreUnificationCleanupArtifactsAreAbsentFromActualReleaseArchives(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	inventory := exec.Command(filepath.Join(repository, "scripts", "release-inventory"), "package-paths")
	output, err := inventory.Output()
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Fields(string(output))
	for _, path := range payload {
		if strings.Contains(path, "cleanup-pre-unification") || strings.Contains(path, "pre-unification-cleanup") {
			t.Fatalf("release payload explicitly includes repository-only cleanup artifact: %s", path)
		}
	}
	binaries := t.TempDir()
	for _, releasePlatform := range releasepkg.SupportedPlatforms {
		for _, executable := range releasepkg.ExecutableNames {
			path := filepath.Join(binaries, releasePlatform.Name, executable)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, path, releasePlatform.Name+":"+executable+"\n")
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	artifacts, err := releasepkg.BuildArchives(releasepkg.BuildOptions{
		Version: "0.3.0", SourceRoot: repository, BinaryRoot: binaries, OutputRoot: t.TempDir(), PayloadPaths: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		for _, name := range cleanupArchiveNames(t, artifact.Path) {
			if strings.Contains(name, "cleanup-pre-unification") || strings.Contains(name, "pre-unification-cleanup.yml") {
				t.Fatalf("repository-only cleanup leaked into %s as %s", artifact.Path, name)
			}
		}
	}
}

func runCleanup(t *testing.T, repository, root string) cleanupPlan {
	t.Helper()
	plan, _ := runCleanupRaw(t, repository, root)
	return plan
}

func runCleanupRaw(t *testing.T, repository, root string) (cleanupPlan, []byte) {
	t.Helper()
	command := exec.Command(filepath.Join(repository, "scripts", "cleanup-pre-unification"))
	command.Env = append(os.Environ(), "AGENT_SESSIONS_CLEANUP_TEST_ROOT="+root)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("plan cleanup: %v", err)
	}
	var plan cleanupPlan
	if err := json.Unmarshal(output, &plan); err != nil {
		t.Fatalf("decode cleanup plan %q: %v", output, err)
	}
	return plan, output
}

func cleanupArchiveNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compressed.Close() }()
	reader := tar.NewReader(compressed)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}
