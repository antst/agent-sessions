package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestBridgeProductProjectionCoversRuntimeAndMCP(t *testing.T) {
	products := mcpLaneProductIDs()
	for _, descriptor := range productcatalog.ProductDescriptors() {
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
	if len(products) != len(productcatalog.ProductDescriptors()) {
		t.Fatalf("MCP product count = %d, want %d", len(products), len(productcatalog.ProductDescriptors()))
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

func TestRuntimeDispatchExcludesDaemonOwnedRoles(t *testing.T) {
	body, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	mainBody := string(body)
	obsolete := []string{
		"shim", "supervisor", "lane", "claude-lane", "claude-lane-manager",
		"grok-lane", "grok-lane-manager", "qwen-lane", "qwen-lane-manager",
		"grok-host", "qwen-host", "lane-watch", "host",
	}
	for _, role := range obsolete {
		if strings.Contains(mainBody, `case "`+role+`"`) {
			t.Errorf("runtime still dispatches daemon-owned role %q", role)
		}
	}
	for _, entrypoint := range []string{"func Main()", "func runShimMain("} {
		if strings.Contains(mainBody, entrypoint) {
			t.Errorf("runtime still exposes obsolete dispatcher %q", entrypoint)
		}
	}
	legacyEntrypoints := map[string][]string{
		"supervisor.go":        {"func runSupervisorCommand("},
		"claude_lane.go":       {"func runClaudeLaneCommand(", "func runClaudeLaneManager("},
		"grok_lane.go":         {"func runGrokLaneCommand("},
		"grok_lane_manager.go": {"func runGrokLaneManager("},
		"qwen_lane.go":         {"func runQwenLaneCommand("},
		"qwen_lane_manager.go": {"func runQwenLaneManager("},
		"grok.go":              {"func runGrokHostCommand("},
		"qwen_host.go":         {"func runQwenHostCommand("},
	}
	for file, symbols := range legacyEntrypoints {
		body, readErr := os.ReadFile(file)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Errorf("read %s: %v", file, readErr)
			continue
		}
		for _, symbol := range symbols {
			if strings.Contains(string(body), symbol) {
				t.Errorf("%s still exposes obsolete process entrypoint %q", file, symbol)
			}
		}
	}
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
	if !slices.Equal(binaries, productcatalog.Catalog().ReleaseExecutables) {
		t.Fatalf("release executable inventory = %q, want canonical images %q", binaries, productcatalog.Catalog().ReleaseExecutables)
	}
	for _, descriptor := range productcatalog.ProductDescriptors() {
		for _, executable := range []string{descriptor.PeerAlias, descriptor.LaneAlias} {
			if !slices.Contains(productcatalog.Catalog().HostAliases, executable) {
				t.Errorf("canonical host alias inventory omits %s %s", descriptor.ID, executable)
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
	if len(plugins) != len(productcatalog.ProductDescriptors()) {
		t.Fatalf("release plugin inventory = %v", plugins)
	}
	for _, descriptor := range productcatalog.ProductDescriptors() {
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
	for _, messagingSkill := range []string{
		"claude/skills/agent-sessions/SKILL.md", "grok/skills/agent-sessions/SKILL.md", "qwen/skills/agent-sessions/SKILL.md",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(messagingSkill))); err != nil {
			t.Errorf("messaging skill is missing: %s: %v", messagingSkill, err)
		}
	}
	grokSkill, err := os.ReadFile(filepath.Join(root, "grok", "skills", "agent-lanes", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range productcatalog.ProductDescriptors() {
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
		strings.Contains(workflow, "for binary in agent-session-runtime") ||
		strings.Contains(workflow, "platform: linux-x64") {
		t.Fatal("CI workflow does not consume the authoritative release build entrypoint")
	}
	lintStart, lintEnd := strings.Index(workflow, "\n  lint:\n"), strings.Index(workflow, "\n  test:\n")
	if lintStart < 0 || lintEnd <= lintStart {
		t.Fatal("CI workflow does not contain a bounded lint job")
	}
	lintJob := workflow[lintStart:lintEnd]
	if !strings.Contains(lintJob, "os: [ubuntu-latest, macos-latest]") ||
		!strings.Contains(lintJob, "runs-on: ${{ matrix.os }}") {
		t.Fatal("CI lint does not cover both Linux and Darwin build-tagged files")
	}
	for _, gate := range []string{
		"make test-race", "go vet ./...", "make lint", "scripts/release-final-gate",
		"release-evidence generate", "release-evidence validate",
		"specs/002-unified-user-daemon/contracts/release-evidence.schema.json",
		"scripts/release-tag-verify", "git verify-commit", "gh run download",
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
	for _, script := range []string{"release-tag-preflight", "release-tag-verify", "release-publication-preflight"} {
		body, readErr := os.ReadFile(filepath.Join(root, "scripts", script))
		if readErr != nil {
			t.Fatal(readErr)
		}
		text := string(body)
		switch script {
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
	if json.Unmarshal(schemaBody, &schema) != nil || strings.Contains(string(schemaBody), "0\\\\.2\\\\.4") ||
		!strings.Contains(string(schemaBody), "agent-sessions-v[0-9]+") ||
		!strings.Contains(string(schemaBody), "agent-sessions-[0-9]+") {
		t.Fatal("unified release evidence schema is not version-generic and source-authority compatible")
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

func TestBridgeProjectsAuthoritativeCatalogWithoutLegacyDescriptorTable(t *testing.T) {
	for _, authoritative := range productcatalog.Catalog().Products {
		projected, ok := bridgeProductByID(authoritative.ID)
		if !ok {
			t.Errorf("bridge projection omits authoritative product %q", authoritative.ID)
			continue
		}
		if projected.descriptor.ID != authoritative.ID ||
			projected.descriptor.PeerAlias != authoritative.PeerAlias ||
			projected.descriptor.LaneAlias != authoritative.LaneAlias ||
			projected.descriptor.LaneCapability != authoritative.LaneCapability {
			t.Errorf("bridge projection for %s = %+v, authoritative = %+v", authoritative.ID, projected.descriptor, authoritative)
		}
	}

	root := filepath.Join("..", "..")
	legacyTable := filepath.Join(root, "internal", "federator", "product.go")
	body, err := os.ReadFile(legacyTable)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "type ProductDescriptor struct") ||
		strings.Contains(string(body), "var productDescriptors") {
		t.Errorf("legacy federator product authority remains in %s", legacyTable)
	}
	for _, projection := range []string{
		"internal/bridge/product.go",
		"internal/launcher/product.go",
	} {
		body, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(projection)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(body), "internal/federator") {
			t.Errorf("%s still sources product descriptors from the legacy federator package", projection)
		}
	}
}

func TestConnectorPayloadsUseOneCanonicalStatelessRelayContract(t *testing.T) {
	root := filepath.Join("..", "..")
	tests := []struct {
		product, manifest, command string
	}{
		{product: "codex", manifest: ".mcp.json", command: "./scripts/native-entry"},
		{product: "claude", manifest: "claude/.mcp.json", command: "agent-sessions"},
		{product: "grok", manifest: "grok/.mcp.json", command: "${GROK_PLUGIN_ROOT}/scripts/native-entry"},
		{product: "qwen", manifest: "qwen/mcp.json", command: "./scripts/native-entry"},
	}
	for _, test := range tests {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(test.manifest)))
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]any
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatalf("decode %s: %v", test.manifest, err)
		}
		servers, _ := manifest["mcpServers"].(map[string]any)
		server, _ := servers["agent_sessions"].(map[string]any)
		arguments, _ := server["args"].([]any)
		wantArguments := []any{"connector", test.product, "mcp"}
		if len(servers) != 1 || stringValue(server["command"]) != test.command || !slices.Equal(arguments, wantArguments) {
			t.Errorf("%s connector = %#v, want command %q args %#v", test.product, server, test.command, wantArguments)
		}
	}
}
