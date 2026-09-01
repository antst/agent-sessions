package bridge

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestHostInstallerReadinessBudgetCoversCodexMCPRefresh(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install-host"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?s)wait_for_host_service\(\).*?local deadline=([0-9]+)`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("host installer readiness budget is not explicit")
	}
	tenths, err := strconv.Atoi(string(match[1]))
	if err != nil || tenths < 600 {
		t.Fatalf("host installer readiness budget = %q tenths, want at least 600 for two bounded Codex refresh operations", match[1])
	}
}

func TestBridgeProductProjectionCoversRuntimeAndMCP(t *testing.T) {
	products := mcpLaneProductIDs()
	for _, descriptor := range federator.ProductDescriptors() {
		projected, ok := bridgeProductByID(descriptor.ID)
		if !ok || projected.descriptor != descriptor {
			t.Fatalf("bridge product projection %s = %+v, %v", descriptor.ID, projected, ok)
		}
		byRole, ok := bridgeProductByLaneRole(descriptor.LaneRuntimeRole)
		if !ok || byRole.descriptor.ID != descriptor.ID {
			t.Fatalf("bridge lane role %q does not resolve to %s", descriptor.LaneRuntimeRole, descriptor.ID)
		}
		if !slices.Contains(products, descriptor.ID) {
			t.Fatalf("MCP product enum is missing %s", descriptor.ID)
		}
	}
	if len(products) != len(federator.ProductDescriptors()) {
		t.Fatalf("MCP product count = %d, want %d", len(products), len(federator.ProductDescriptors()))
	}
	for _, definition := range nativeToolDefinitions {
		if stringValue(definition["name"]) != "lane" {
			continue
		}
		description := stringValue(definition["description"])
		if !strings.Contains(description, "supported-product lane") {
			t.Fatalf("MCP lane description is not product-neutral: %q", description)
		}
		return
	}
	t.Fatal("MCP lane tool definition is missing")
}

func TestAuthoritativeReleaseInventoryCoversEveryProductSurface(t *testing.T) {
	root := filepath.Join("..", "..")
	inventory := filepath.Join(root, "scripts", "release-inventory")
	command := exec.Command(inventory, "binaries")
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	binaries := strings.Fields(string(body))
	if !slices.Equal(binaries, []string{"agent-sessions", "agent-sessions-hub"}) {
		t.Fatalf("release executable inventory = %q, want the exact host and hub images", binaries)
	}
	aliasBody, err := exec.Command(inventory, "host-aliases").Output()
	if err != nil {
		t.Fatal(err)
	}
	hostAliases := strings.Fields(string(aliasBody))
	for _, descriptor := range federator.ProductDescriptors() {
		for _, executable := range []string{descriptor.PeerExecutable, descriptor.LaneExecutable} {
			if !slices.Contains(hostAliases, executable) {
				t.Errorf("host alias inventory omits %s %s", descriptor.ID, executable)
			}
		}
		for _, document := range []string{
			filepath.Join(root, "docs", strings.ToUpper(descriptor.ID)+"-ADAPTER.md"),
			filepath.Join(root, "docs", strings.ToUpper(descriptor.ID)+"-INSTALL.md"),
			filepath.Join(root, "docs", strings.ToUpper(descriptor.ID)+"-LANES.md"),
		} {
			if info, statErr := os.Stat(document); statErr != nil || !info.Mode().IsRegular() {
				t.Errorf("%s documentation surface is missing: %s", descriptor.ID, document)
			}
		}
		for _, skill := range []string{
			filepath.Join(root, "skills", descriptor.ID+"-lane", "SKILL.md"),
			filepath.Join(root, "claude", "skills", descriptor.ID+"-lane", "SKILL.md"),
			filepath.Join(root, "qwen", "skills", descriptor.ID+"-lane", "SKILL.md"),
		} {
			if info, statErr := os.Stat(skill); statErr != nil || !info.Mode().IsRegular() {
				t.Errorf("%s lane skill surface is missing: %s", descriptor.ID, skill)
			}
		}
	}
	pluginBody, err := exec.Command(inventory, "plugins").Output()
	if err != nil {
		t.Fatal(err)
	}
	plugins := map[string][]string{}
	for _, row := range strings.Fields(string(pluginBody)) {
		product, paths, ok := strings.Cut(row, "|")
		if !ok || product == "" || paths == "" {
			t.Fatalf("invalid authoritative plugin inventory row %q", row)
		}
		plugins[product] = strings.Split(paths, ",")
	}
	if len(plugins) != len(federator.ProductDescriptors()) {
		t.Fatalf("release plugin inventory = %v", plugins)
	}
	for _, descriptor := range federator.ProductDescriptors() {
		paths, ok := plugins[descriptor.ID]
		if !ok || len(paths) == 0 {
			t.Errorf("release plugin inventory omits %s", descriptor.ID)
		}
		for _, path := range paths {
			if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(path))); statErr != nil {
				t.Errorf("%s plugin inventory path %s is missing: %v", descriptor.ID, path, statErr)
			}
		}
	}
	messagingSkills := []string{
		"skills/agent-sessions/SKILL.md", "claude/skills/agent-sessions/SKILL.md",
		"grok/skills/agent-sessions/SKILL.md", "qwen/skills/agent-sessions/SKILL.md",
	}
	var canonicalMessagingSkill []byte
	for index, messagingSkill := range messagingSkills {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(messagingSkill))); err != nil {
			t.Errorf("messaging skill is missing: %s: %v", messagingSkill, err)
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(messagingSkill)))
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			canonicalMessagingSkill = body
		} else if !bytes.Equal(body, canonicalMessagingSkill) {
			t.Errorf("%s drifted from the canonical Agent Sessions skill", messagingSkill)
		}
	}
	for _, required := range []string{
		"agent_sessions.list_peers", "agent_sessions.send_message", "agent_sessions.broadcast",
		"agent_sessions.lane", "list native agents", "owner_session_id",
	} {
		if !bytes.Contains(canonicalMessagingSkill, []byte(required)) {
			t.Errorf("canonical Agent Sessions skill omits %q", required)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "agent-sessions", "agents", "openai.yaml")); err != nil {
		t.Errorf("Codex Agent Sessions skill metadata is missing: %v", err)
	}
	grokSkill, err := os.ReadFile(filepath.Join(root, "grok", "skills", "agent-lanes", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range federator.ProductDescriptors() {
		if !strings.Contains(string(grokSkill), `"product":"`+descriptor.ID+`"`) {
			t.Errorf("Grok all-target lane skill omits structured %s examples", descriptor.ID)
		}
	}

	workflowBody, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBody)
	if !strings.Contains(workflow, "make build-release-platform") ||
		!strings.Contains(workflow, "release-inventory matrix-json") ||
		!strings.Contains(workflow, "fromJSON(needs.inventory.outputs.matrix)") ||
		!strings.Contains(workflow, "matrix:\n        os: [ubuntu-latest, macos-latest]") ||
		!strings.Contains(workflow, "cat deploy/agent-sessions/VERSION") ||
		strings.Contains(workflow, "agent-session-runtime release-evidence") ||
		strings.Contains(workflow, "cat deploy/peer-federator/VERSION") ||
		strings.Contains(workflow, "for binary in agent-session-runtime") ||
		strings.Contains(workflow, "platform: linux-x64") {
		t.Fatal("CI workflow does not consume the authoritative release build entrypoint")
	}
	for _, gate := range []string{
		"make test-race", "go vet ./...", "make lint", "scripts/release-final-gate",
		"releasetool evidence generate", "releasetool evidence validate",
		"specs/002-unified-user-daemon/contracts/release-evidence.schema.json",
		"scripts/release-tag-verify", "scripts/release-commit-verify", "gh run download",
		"scripts/release-publication-preflight", "retention-days: 90",
		"agent-sessions-v${{ needs.inventory.outputs.version }}-release-evidence-${{ github.sha }}",
	} {
		if !strings.Contains(workflow, gate) {
			t.Errorf("CI workflow omits release gate %q", gate)
		}
	}
	if strings.Contains(workflow, "tag_version=\"${RELEASE_TAG#v}\"") ||
		strings.Contains(workflow, "RELEASE_VERSION=\"${GITHUB_REF_NAME#v}\"") {
		t.Fatal("CI workflow injects a ref-dependent package version")
	}
	for _, script := range []string{
		"release-commit-verify", "release-tag-preflight", "release-tag-verify", "release-publication-preflight",
	} {
		body, readErr := os.ReadFile(filepath.Join(root, "scripts", script))
		if readErr != nil {
			t.Fatal(readErr)
		}
		text := string(body)
		switch script {
		case "release-commit-verify":
			if !strings.Contains(text, "verify-commit") || !strings.Contains(text, "commit.verification.verified") ||
				!strings.Contains(text, "web-flow") {
				t.Error("release commit gate does not require a maintainer or verified GitHub web-flow signature")
			}
		case "release-tag-preflight":
			if !strings.Contains(text, "show-ref --verify") || !strings.Contains(text, "ls-remote --tags") {
				t.Error("pre-tag gate does not reject both local and remote collisions")
			}
		case "release-tag-verify":
			if !strings.Contains(text, "verify-tag") || !strings.Contains(text, "Agent-Sessions-Evidence-SHA256") {
				t.Error("tag release does not require the exact signed evidence-bound tag")
			}
		case "release-publication-preflight":
			if !strings.Contains(text, "gh release view") || !strings.Contains(text, ".assets[]?") ||
				strings.Contains(text, "releases?per_page") {
				t.Error("publication gate does not independently reject release and asset collisions")
			}
		}
	}

	schemaBody, err := os.ReadFile(filepath.Join(root, "specs", "002-unified-user-daemon", "contracts", "release-evidence.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if json.Unmarshal(schemaBody, &schema) != nil || !strings.Contains(string(schemaBody), "agent-sessions-v0\\\\.3\\\\.0-release-evidence") ||
		!strings.Contains(string(schemaBody), "agent-sessions-0\\\\.3\\\\.0-") ||
		!strings.Contains(string(schemaBody), `"agent-sessions-hub"`) || strings.Contains(string(schemaBody), `"peer-federator"`) {
		t.Fatal("normative release evidence schema does not pin the v0.3.0 two-image release")
	}
}

func TestPublicMCPNamespaceIsProductNeutral(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, manifest := range []string{".mcp.json", "claude/.mcp.json", "grok/.mcp.json", "qwen/mcp.json"} {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifest)))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatalf("decode %s: %v", manifest, err)
		}
		servers, _ := document["mcpServers"].(map[string]any)
		if len(servers) != 1 || servers["agent_sessions"] == nil {
			t.Errorf("%s MCP namespace = %v, want exactly agent_sessions", manifest, servers)
		}
		server, _ := servers["agent_sessions"].(map[string]any)
		arguments, _ := server["args"].([]any)
		if len(arguments) < 4 || stringValue(arguments[len(arguments)-2]) != "--release-identity" ||
			stringValue(arguments[len(arguments)-1]) != "@AGENT_SESSIONS_RELEASE_ID@" {
			t.Errorf("%s MCP command is not bound to the installed host image identity", manifest)
		}
	}

	publicRoots := []string{"README.md", "docs", "skills", "claude/skills", "grok/skills", "qwen/skills"}
	for _, publicRoot := range publicRoots {
		path := filepath.Join(root, filepath.FromSlash(publicRoot))
		err := filepath.Walk(path, func(file string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() || (filepath.Ext(file) != ".md" && filepath.Base(file) != "README.md") {
				return nil
			}
			body, readErr := os.ReadFile(file)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(body), "claude_peer") {
				t.Errorf("legacy product-specific MCP namespace remains in %s", file)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeSkillsRequireStructuredMessagingWithoutNativeFallback(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, relative := range []string{
		"claude/skills/agent-sessions/SKILL.md",
		"claude/skills/codex-lane/SKILL.md",
		"claude/skills/claude-lane/SKILL.md",
		"claude/skills/grok-lane/SKILL.md",
	} {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, "agent_sessions") {
			t.Errorf("%s does not direct Claude to structured Agent Sessions tools", relative)
		}
		for _, forbidden := range []string{"AGENT_SESSIONS_FRAME", "agent-sessions--HOST"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s retains native-carrier fallback %q", relative, forbidden)
			}
		}
	}
}

func TestExistingLaneParsedGroupOptionsAreAdvertisedInHelp(t *testing.T) {
	for product, usage := range map[string]string{
		"codex": laneUsage(), "claude": claudeLaneUsage(), "grok": grokLaneUsage(), "qwen": qwenLaneUsage(),
	} {
		for _, option := range []string{"--group GROUP", "--inherit-groups", "--no-inherit-groups"} {
			if !strings.Contains(usage, option) {
				t.Errorf("%s parser option %q is absent from help", product, option)
			}
		}
	}
}

func TestUnifiedLaneHelpDoesNotAdvertiseLegacyNotifyRouting(t *testing.T) {
	for product, usage := range map[string]string{
		"codex": laneUsage(), "claude": claudeLaneUsage(), "grok": grokLaneUsage(), "qwen": qwenLaneUsage(),
	} {
		for _, obsolete := range []string{"--notify", "--no-notify", "notify_target"} {
			if strings.Contains(usage, obsolete) {
				t.Errorf("%s unified help advertises obsolete %s", product, obsolete)
			}
		}
	}
}
