package productcatalog

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestCatalogPreservesCompleteBaselineProductHelpPluginAndLaneInventory(t *testing.T) {
	want := []Descriptor{
		{
			ID: "codex", Label: "Codex", NativeExecutable: "codex", PeerAlias: "codex-peer",
			LaneAlias: "codex-peer-lane", LaneRuntimeRole: "lane", LaneCapability: "codex-lane",
			PluginArchivePaths: []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"},
			Capabilities:       []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay, CapabilityHook, CapabilityArchive},
			ResumeStyle:        ResumeSubcommand,
		},
		{
			ID: "claude", Label: "Claude Code", NativeExecutable: "claude", PeerAlias: "claude-peer",
			LaneAlias: "claude-peer-lane", LaneRuntimeRole: "claude-lane", LaneManagerRole: "claude-lane-manager", LaneCapability: "claude-lane",
			PluginArchivePaths:  []string{".claude-plugin", "claude"},
			Capabilities:        []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay},
			ResumeStyle:         ResumeFlag,
			TranscriptNameIndex: true,
		},
		{
			ID: "grok", Label: "Grok", NativeExecutable: "grok", PeerAlias: "grok-peer",
			LaneAlias: "grok-peer-lane", LaneRuntimeRole: "grok-lane", LaneManagerRole: "grok-lane-manager", LaneCapability: "grok-lane",
			PluginArchivePaths: []string{"grok"},
			Capabilities:       []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission},
			ResumeStyle:        ResumeFlag,
		},
		{
			ID: "qwen", Label: "Qwen Code", NativeExecutable: "qwen", PeerAlias: "qwen-peer",
			LaneAlias: "qwen-peer-lane", LaneRuntimeRole: "qwen-lane", LaneManagerRole: "qwen-lane-manager", LaneCapability: "qwen-lane",
			PluginArchivePaths: []string{"qwen"},
			Capabilities:       []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission},
			ResumeStyle:        ResumeFlag,
		},
	}
	got := All()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("product catalog = %#v, want %#v", got, want)
	}
	for _, descriptor := range want {
		resolved, ok := ByID(descriptor.ID)
		if !ok || !reflect.DeepEqual(resolved, descriptor) {
			t.Fatalf("ByID(%q) = %#v, %v", descriptor.ID, resolved, ok)
		}
		if peer, ok := ByCommand(descriptor.PeerAlias); !ok || peer.ID != descriptor.ID {
			t.Fatalf("ByCommand(%q) = %#v, %v", descriptor.PeerAlias, peer, ok)
		}
		if lane, ok := ByCommand(descriptor.LaneAlias); !ok || lane.ID != descriptor.ID {
			t.Fatalf("ByCommand(%q) = %#v, %v", descriptor.LaneAlias, lane, ok)
		}
	}
	if _, ok := ByID("Codex"); ok {
		t.Fatal("noncanonical product ID was accepted")
	}
	if _, ok := ByCommand("codex"); ok {
		t.Fatal("native vendor executable was classified as a managed command")
	}
}

func TestCatalogMatchesWorkingFederationAndReleaseInventories(t *testing.T) {
	catalog := All()
	legacy := federator.ProductDescriptors()
	if len(catalog) != len(legacy) {
		t.Fatalf("catalog products = %d, working federation products = %d", len(catalog), len(legacy))
	}
	for index, product := range catalog {
		baseline := legacy[index]
		if product.ID != baseline.ID || product.Label != baseline.Label ||
			product.PeerAlias != baseline.PeerExecutable || product.LaneAlias != baseline.LaneExecutable ||
			product.LaneRuntimeRole != baseline.LaneRuntimeRole || product.LaneManagerRole != baseline.LaneManagerRole ||
			product.LaneCapability != baseline.FederationCapability ||
			product.Has(CapabilityDynamicPermission) != baseline.DynamicPermission ||
			product.TranscriptNameIndex != baseline.TranscriptNameIndex {
			t.Fatalf("catalog product %#v drifted from working descriptor %#v", product, baseline)
		}
		args, ok := baseline.ResumeArguments(federator.SessionKindInteractive, "session-id")
		if !ok || !reflect.DeepEqual(args, product.ResumeArguments("session-id")) {
			t.Fatalf("%s resume arguments = %v/%v, want %v", product.ID, args, ok, product.ResumeArguments("session-id"))
		}
	}

	root := productCatalogRepositoryRoot(t)
	command := exec.Command(filepath.Join(root, "scripts", "release-inventory"), "plugins")
	command.Dir = root
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	gotRows := strings.Fields(strings.TrimSpace(string(body)))
	wantRows := make([]string, 0, len(catalog))
	for _, product := range catalog {
		wantRows = append(wantRows, product.ID+"|"+strings.Join(product.PluginArchivePaths, ","))
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("release plugin inventory = %v, want %v", gotRows, wantRows)
	}
}

func TestCatalogIsClosedUniqueAndReturnsIsolatedCopies(t *testing.T) {
	products := All()
	if len(products) != 4 {
		t.Fatalf("product count = %d, want 4", len(products))
	}
	ids, commands, capabilities := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, product := range products {
		if ids[product.ID] || commands[product.PeerAlias] || commands[product.LaneAlias] || capabilities[product.LaneCapability] {
			t.Fatalf("duplicate catalog identity in %#v", product)
		}
		ids[product.ID], commands[product.PeerAlias], commands[product.LaneAlias], capabilities[product.LaneCapability] = true, true, true, true
		ordered := append([]Capability(nil), product.Capabilities...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
		for index := 1; index < len(ordered); index++ {
			if ordered[index] == ordered[index-1] {
				t.Fatalf("%s repeats capability %s", product.ID, ordered[index])
			}
		}
	}
	products[0].PluginArchivePaths[0] = "mutated"
	products[0].Capabilities[0] = "mutated"
	again, _ := ByID("codex")
	if again.PluginArchivePaths[0] != ".agents" || again.Capabilities[0] != CapabilityInteractive {
		t.Fatalf("catalog leaked caller mutation: %#v", again)
	}
}

func productCatalogRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve catalog test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
