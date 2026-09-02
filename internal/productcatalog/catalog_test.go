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
	wantIDs := []string{"codex", "claude", "grok", "qwen"}
	products := All()
	if len(products) != len(wantIDs) {
		t.Fatalf("product count = %d", len(products))
	}
	if err := ValidateInventory(products); err != nil {
		t.Fatal(err)
	}
	for index, product := range products {
		if product.ID != wantIDs[index] || !product.Has(CapabilityInteractive) || !product.Has(CapabilityLane) || !product.Has(CapabilityParent) {
			t.Fatalf("descriptor %d = %#v", index, product)
		}
		if product.SupportState != SupportGeneral || product.TestedVersion == "" || product.InstallRoot != "integrations/"+product.ID {
			t.Fatalf("shared metadata missing from %#v", product)
		}
		if product.NativeRegistration.Strategy != "legacy-native-plugin" || !reflect.DeepEqual(product.NativeRegistration.Args, []string{product.ID}) {
			t.Fatalf("%s native registration = %#v", product.ID, product.NativeRegistration)
		}
		if !product.Acceptance.RealProductRequired || len(product.Acceptance.ExternalCells) != 0 {
			t.Fatalf("%s acceptance = %#v", product.ID, product.Acceptance)
		}
		if product.PeerTransport != "presence" || product.MessageTransport != "presence" {
			t.Fatalf("%s live transports = %q/%q", product.ID, product.PeerTransport, product.MessageTransport)
		}
		if !reflect.DeepEqual(product.FederationCapabilities, []string{product.LaneCapability}) {
			t.Fatalf("%s federation capabilities = %v", product.ID, product.FederationCapabilities)
		}
		resolved, ok := ByID(product.ID)
		if !ok || !reflect.DeepEqual(resolved, product) {
			t.Fatalf("ByID(%q) = %#v, %v", product.ID, resolved, ok)
		}
		for _, alias := range []string{product.PeerAlias, product.LaneAlias} {
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
	want := []string{"codex", "claude", "grok", "qwen", "opencode", "kilo", "pi", "omp", "codebuddy", "dsh"}
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
		wantRows = append(wantRows, product.ID+"|"+strings.Join(product.PluginArchivePaths, ","))
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
	products[0].NativeRegistration.AssetOnly = true
	products[0].Acceptance.ExternalCells = []ExternalAcceptanceCell{{ID: "mutated"}}
	again, _ := ByID("codex")
	if again.PluginArchivePaths[0] != ".agents" || again.Capabilities[0] != CapabilityInteractive || again.RequiredDoctorFeatures[0] != "native-cli" || again.FederationCapabilities[0] != "codex-lane" || len(again.Compatibility.TupleMembers) != 0 || again.NativeRegistration.Args[0] != "codex" || again.NativeRegistration.AssetOnly || len(again.Acceptance.ExternalCells) != 0 {
		t.Fatalf("catalog leaked caller mutation: %#v", again)
	}
	ordered := again.SortedCapabilities()
	if !sort.StringsAreSorted(ordered) {
		t.Fatalf("capabilities not sorted: %v", ordered)
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
