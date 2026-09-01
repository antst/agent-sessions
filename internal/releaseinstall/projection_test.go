package releaseinstall

import (
	"bytes"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestInstallProjectionIsDeterministicCatalogDerivedAndSecretFree(t *testing.T) {
	inventory := productcatalog.All()[:2]
	inventory[0], inventory[1] = inventory[1], inventory[0]
	inventory[0].Authority.PeerAuth = "password"
	inventory[0].Authority.LaneAuth = "bearer"
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
	for _, forbidden := range [][]byte{[]byte("credential_value"), []byte("secret_value"), []byte("endpoint"), []byte("argv"), []byte("environment")} {
		if bytes.Contains(bytes.ToLower(first), forbidden) {
			t.Fatalf("projection contains forbidden secret-shaped field %q", forbidden)
		}
	}
	if !bytes.Contains(first, []byte(`"peer_auth": "password"`)) || !bytes.Contains(first, []byte(`"lane_auth": "bearer"`)) {
		t.Fatalf("projection rejected public authority mechanism enums: %s", first)
	}
}
