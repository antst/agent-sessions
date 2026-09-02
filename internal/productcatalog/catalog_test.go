package productcatalog

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestCatalogPreservesBaselineAndAddsValidatedSharedMetadata(t *testing.T) {
	wantIDs := []string{"codex", "claude", "grok", "qwen", "opencode", "kilo", "pi", "omp", "dsh"}
	products := All()
	if len(products) != len(wantIDs) {
		t.Fatalf("product count = %d", len(products))
	}
	if err := ValidateInventory(products); err != nil {
		t.Fatal(err)
	}
	for index, product := range products {
		if product.ID != wantIDs[index] || !product.Has(CapabilityLane) {
			t.Fatalf("descriptor %d = %#v", index, product)
		}
		if product.SupportState != SupportGeneral || product.TestedVersion == "" {
			t.Fatalf("shared metadata missing from %#v", product)
		}
		if !product.Acceptance.RealProductRequired || len(product.Acceptance.ExternalCells) != 0 {
			t.Fatalf("%s acceptance = %#v", product.ID, product.Acceptance)
		}
		if product.ID == "dsh" {
			if product.Has(CapabilityInteractive) || product.Has(CapabilityParent) || product.PeerAlias != "" || product.PeerTransport != "" || product.MessageTransport != "" || product.InstallRoot != "" || len(product.PluginArchivePaths) != 0 || product.NativeRegistration.Strategy != "" {
				t.Fatalf("DSH descriptor carries a peer surface: %#v", product)
			}
		} else if !product.Has(CapabilityInteractive) || !product.Has(CapabilityParent) || product.PeerTransport != "presence" || product.MessageTransport != "presence" || product.InstallRoot != "integrations/"+product.ID || product.NativeRegistration.Strategy == "" {
			t.Fatalf("%s peer metadata = %#v", product.ID, product)
		}
		if !reflect.DeepEqual(product.FederationCapabilities, []string{product.LaneCapability}) {
			t.Fatalf("%s federation capabilities = %v", product.ID, product.FederationCapabilities)
		}
		resolved, ok := ByID(product.ID)
		if !ok || !reflect.DeepEqual(resolved, product) {
			t.Fatalf("ByID(%q) = %#v, %v", product.ID, resolved, ok)
		}
		for _, alias := range []string{product.PeerAlias, product.LaneAlias} {
			if alias == "" {
				continue
			}
			if got, ok := ByCommand(alias); !ok || got.ID != product.ID {
				t.Fatalf("ByCommand(%q) = %#v, %v", alias, got, ok)
			}
		}
		if got, ok := ByLaneCapability(product.LaneCapability); !ok || got.ID != product.ID {
			t.Fatalf("ByLaneCapability(%q) = %#v, %v", product.LaneCapability, got, ok)
		}
	}
	if _, ok := ByID("Codex"); ok {
		t.Fatal("noncanonical product ID accepted")
	}
	if _, ok := ByCommand("codex"); ok {
		t.Fatal("native executable classified as managed alias")
	}
}

func TestRuntimeInventoryRecognizesAllLiveReconnectProducts(t *testing.T) {
	want := []string{"codex", "claude", "grok", "qwen", "opencode", "kilo", "pi", "omp", "dsh"}
	inventory := RuntimeInventory()
	if len(inventory) != len(want) {
		t.Fatalf("runtime product count = %d, want %d", len(inventory), len(want))
	}
	if err := ValidateInventory(inventory); err != nil {
		t.Fatal(err)
	}
	for index, id := range want {
		if inventory[index].ID != id {
			t.Fatalf("runtime product %d = %q, want %q", index, inventory[index].ID, id)
		}
		if resolved, ok := ByID(id); !ok || resolved.ID != id {
			t.Fatalf("ByID(%q) = %#v, %v", id, resolved, ok)
		}
	}
}

func TestCatalogMatchesReleaseInventoryWithoutASecondGoCatalog(t *testing.T) {
	catalog := All()
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
		if len(product.PluginArchivePaths) != 0 {
			wantRows = append(wantRows, product.ID+"|"+strings.Join(product.PluginArchivePaths, ","))
		}
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("release plugin inventory = %v, want %v", gotRows, wantRows)
	}
}

func TestCatalogReturnsDeepIsolatedCopies(t *testing.T) {
	products := All()
	products[0].PluginArchivePaths[0] = "mutated"
	products[0].Capabilities[0] = "mutated"
	products[0].RequiredDoctorFeatures[0] = "mutated"
	products[0].FederationCapabilities[0] = "mutated"
	products[0].Compatibility.TupleMembers = []TupleMember{{Name: "mutated", Version: "1"}}
	products[0].NativeRegistration.Args[0] = "mutated"
	products[6].NativeToolGrantArgs[0] = "mutated"
	products[6].NativeYoloArgs[0] = "mutated"
	products[0].NativeArgumentRules[0].Option = "--mutated"
	products[0].NativeArgumentRules[0].Replacement[0] = "mutated"
	products[0].NativeRegistration.AssetOnly = true
	products[0].Acceptance.ExternalCells = []ExternalAcceptanceCell{{ID: "mutated"}}
	again, _ := ByID("codex")
	if again.PluginArchivePaths[0] != ".agents" || again.Capabilities[0] != CapabilityInteractive || again.RequiredDoctorFeatures[0] != "native-cli" || again.FederationCapabilities[0] != "codex-lane" || len(again.Compatibility.TupleMembers) != 0 || again.NativeRegistration.Args[0] != "codex" || again.NativeRegistration.AssetOnly || len(again.Acceptance.ExternalCells) != 0 || len(again.NativeToolGrantArgs) != 0 || !reflect.DeepEqual(again.NativeYoloArgs, []string{"--yolo"}) || !reflect.DeepEqual(again.NativeArgumentRules, []NativeArgumentRule{{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"resume"}}}) {
		t.Fatalf("catalog leaked caller mutation: %#v", again)
	}
	claude, _ := ByID("claude")
	if !reflect.DeepEqual(claude.NativeToolGrantArgs, []string{"--allowedTools", "mcp__plugin_agent-sessions_agent_sessions__*"}) || !reflect.DeepEqual(claude.NativeYoloArgs, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("Claude launch policy = %#v", claude)
	}
	grok, _ := ByID("grok")
	if len(grok.NativeToolGrantArgs) != 0 || !reflect.DeepEqual(grok.NativeYoloArgs, []string{"--yolo"}) {
		t.Fatalf("Grok launch policy = %#v", grok)
	}
	qwen, _ := ByID("qwen")
	if len(qwen.NativeToolGrantArgs) != 0 || !reflect.DeepEqual(qwen.NativeYoloArgs, []string{"--yolo"}) {
		t.Fatalf("Qwen launch policy = %#v", qwen)
	}
	pi, _ := ByID("pi")
	if !reflect.DeepEqual(pi.NativeToolGrantArgs, []string{"--approve"}) || !reflect.DeepEqual(pi.NativeYoloArgs, []string{"--approve"}) {
		t.Fatalf("Pi launch policy leaked caller mutation: %#v", pi)
	}
	omp, _ := ByID("omp")
	if len(omp.NativeToolGrantArgs) != 0 || !reflect.DeepEqual(omp.NativeYoloArgs, []string{"--yolo"}) {
		t.Fatalf("OMP launch policy = %#v", omp)
	}
	for _, id := range []string{"opencode", "kilo"} {
		product, _ := ByID(id)
		if len(product.NativeToolGrantArgs) != 0 || !reflect.DeepEqual(product.NativeYoloArgs, []string{"--yolo"}) {
			t.Fatalf("%s launch policy = %#v", id, product)
		}
	}
	ordered := again.SortedCapabilities()
	if !sort.StringsAreSorted(ordered) {
		t.Fatalf("capabilities not sorted: %v", ordered)
	}
}

func TestCatalogOwnsUniformResumeTranslationsAndOnlyProvenGapHandlers(t *testing.T) {
	want := map[string][]NativeArgumentRule{
		"codex": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"resume"}},
		},
		"claude": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"--resume"}},
		},
		"grok": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"--resume"}},
		},
		"qwen": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"--resume"}},
		},
		"opencode": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"--session"}},
			{Kind: NativeArgumentHandler, Option: "--session", Handler: "opencode-session-list"},
			{Kind: NativeArgumentHandler, Option: "-s", Handler: "opencode-session-list"},
		},
		"kilo": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"--session"}},
			{Kind: NativeArgumentHandler, Option: "--session", Handler: "opencode-session-list"},
			{Kind: NativeArgumentHandler, Option: "-s", Handler: "opencode-session-list"},
		},
		"pi": {
			{Kind: NativeArgumentTranslation, Option: "--resume", Replacement: []string{"--session"}},
			{Kind: NativeArgumentHandler, Option: "--session", Handler: "pi-session-list"},
		},
		"omp": {
			{Kind: NativeArgumentHandler, Option: "--resume", Handler: "omp-session-list"},
		},
	}
	for product, rules := range want {
		descriptor, ok := ByID(product)
		if !ok {
			t.Fatalf("descriptor %q missing", product)
		}
		if !reflect.DeepEqual(descriptor.NativeArgumentRules, rules) {
			t.Fatalf("%s native argument rules = %#v, want %#v", product, descriptor.NativeArgumentRules, rules)
		}
	}
	dsh, _ := ByID("dsh")
	if len(dsh.NativeArgumentRules) != 0 {
		t.Fatalf("lane-only DSH carries peer argument rules: %#v", dsh.NativeArgumentRules)
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
