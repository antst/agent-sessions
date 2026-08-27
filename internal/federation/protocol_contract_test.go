package federation

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestFederationUsesTheAuthoritativeHubProtocolDescriptor(t *testing.T) {
	want := productcatalog.Catalog().HubProtocol
	got := ProtocolDescriptor()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("federation protocol descriptor = %+v, catalog = %+v", got, want)
	}
	if got.Version != 3 || got.ReleaseCoupled {
		t.Fatalf("software interoperability boundary = %+v", got)
	}
}

func TestHubProtocolDescriptorMatchesFeatureContractAndCheckedDocs(t *testing.T) {
	root := filepath.Join("..", "..")
	paths := []string{
		filepath.Join(root, "specs", "002-unified-user-daemon", "contracts", "federation-protocol.md"),
		filepath.Join(root, "docs", "FEDERATION.md"),
	}
	wantInventory := ProtocolInventoryMarkdown()
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, required := range []string{
			"agent-sessions-hub",
			"one central",
			"global groups",
			"host-suffixed",
		} {
			if !strings.Contains(text, required) {
				t.Errorf("%s omits protocol fact %q", path, required)
			}
		}
		start := strings.Index(text, ProtocolInventoryStart)
		end := strings.Index(text, ProtocolInventoryEnd)
		if start < 0 || end < start {
			t.Errorf("%s omits the bounded generated protocol inventory", path)
			continue
		}
		end += len(ProtocolInventoryEnd)
		if got := text[start:end]; got != wantInventory {
			t.Errorf("%s protocol inventory differs from descriptor\ngot:\n%s\nwant:\n%s", path, got, wantInventory)
		}
	}
}

func TestProtocolCompatibilityIsExactAndCapabilityNeutral(t *testing.T) {
	descriptor := ProtocolDescriptor()
	if !CompatibleProtocol(descriptor.Version) || CompatibleProtocol(descriptor.Version-1) ||
		CompatibleProtocol(descriptor.Version+1) {
		t.Fatal("protocol compatibility is not exact equality")
	}
	for _, capability := range descriptor.Capabilities {
		if !CompatibleProtocol(descriptor.Version) {
			t.Fatalf("capability %q altered protocol compatibility", capability)
		}
	}
}
