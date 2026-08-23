package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

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
	if len(binaries) != 11 {
		t.Fatalf("release executable inventory = %q", binaries)
	}
	for _, descriptor := range federator.ProductDescriptors() {
		for _, executable := range []string{descriptor.PeerExecutable, descriptor.LaneExecutable} {
			if !slices.Contains(binaries, executable) {
				t.Errorf("release inventory omits %s %s", descriptor.ID, executable)
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
	for _, common := range []string{"agent-session-runtime", "peer", "peer-federator"} {
		if !slices.Contains(binaries, common) {
			t.Errorf("release inventory omits shared executable %s", common)
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
		strings.Contains(workflow, "for binary in agent-session-runtime") ||
		strings.Contains(workflow, "platform: linux-x64") {
		t.Fatal("CI workflow does not consume the authoritative release build entrypoint")
	}
	for _, gate := range []string{
		"make test-race", "go vet ./...", "make lint", "scripts/release-final-gate",
		"release-evidence generate", "release-evidence validate",
		"specs/001-qwen-support/contracts/release-evidence.schema.json",
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
			if !strings.Contains(text, "gh release view") || !strings.Contains(text, ".assets[]?") {
				t.Error("publication gate does not independently reject release and asset collisions")
			}
		}
	}

	schemaBody, err := os.ReadFile(filepath.Join(root, "specs", "001-qwen-support", "contracts", "release-evidence.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if json.Unmarshal(schemaBody, &schema) != nil || !strings.Contains(string(schemaBody), "agent-sessions-v0\\\\.2\\\\.4-release-evidence") ||
		!strings.Contains(string(schemaBody), "agent-sessions-0\\\\.2\\\\.4-") {
		t.Fatal("normative release evidence schema does not pin v0.2.4 artifact identities")
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
