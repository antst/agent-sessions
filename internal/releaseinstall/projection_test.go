package releaseinstall

import (
	"bytes"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestInstallProjectionIsDeterministicCatalogDerivedAndSecretFree(t *testing.T) {
	inventory := productcatalog.All()[:2]
	inventory[0], inventory[1] = inventory[1], inventory[0]
	inventory[0].Compatibility = productcatalog.Compatibility{
		Policy: productcatalog.VersionExact, PackageManager: "pnpm", PackageManagerVersion: "10.28.1",
		TupleMembers: []productcatalog.TupleMember{
			{Name: "@deepseek-ai/dsh-acp-app", Version: "0.1.2-alpha.3"},
			{Name: "@deepseek-ai/dsh", Version: "0.1.2-alpha.3"},
		},
	}
	first, err := ProjectionJSON(inventory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectionJSON(inventory)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("projection is nondeterministic: %v", err)
	}
	if !bytes.Contains(first, []byte(`"strategy": "legacy-native-plugin"`)) || !bytes.Contains(first, []byte(`"real_product_required": true`)) {
		t.Fatalf("projection omitted install/acceptance metadata: %s", first)
	}
	if !bytes.Contains(first, []byte(`"package_manager_version": "10.28.1"`)) {
		t.Fatalf("install projection omitted exact package-manager version: %s", first)
	}
	for _, forbidden := range [][]byte{[]byte("credential_value"), []byte("secret_value"), []byte("endpoint"), []byte("argv"), []byte("environment")} {
		if bytes.Contains(bytes.ToLower(first), forbidden) {
			t.Fatalf("projection contains forbidden secret-shaped field %q", forbidden)
		}
	}
	if bytes.Contains(first, []byte(`"authority"`)) {
		t.Fatalf("projection contains removed trust metadata: %s", first)
	}
}
