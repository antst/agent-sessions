package productcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestProjectionIsCanonicalSortedIsolatedAndSecretFree(t *testing.T) {
	inventory := All()
	inventory[0], inventory[3] = inventory[3], inventory[0]
	first, err := ProjectionJSON(inventory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectionJSON(inventory)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("projection is nondeterministic: %v", err)
	}
	if len(first) == 0 || first[len(first)-1] != '\n' || bytes.HasSuffix(first, []byte("\n\n")) {
		t.Fatalf("projection newline contract failed: %q", first)
	}
	for _, forbidden := range [][]byte{[]byte("credential_value"), []byte("secret_value"), []byte("endpoint"), []byte("argv"), []byte("environment")} {
		if bytes.Contains(bytes.ToLower(first), forbidden) {
			t.Fatalf("projection contains secret-shaped field %q", forbidden)
		}
	}
	if bytes.Contains(first, []byte(`"authority"`)) || bytes.Contains(first, []byte(`"connector_attester"`)) {
		t.Fatalf("projection contains removed trust metadata: %s", first)
	}
	var decoded Projection
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ProjectionSchemaV1 || len(decoded.Products) != 9 || decoded.Products[0].ID != "claude" || decoded.Products[8].ID != "qwen" {
		t.Fatalf("projection = %#v", decoded)
	}
	for _, product := range decoded.Products {
		if product.LaneTransport == "" || product.DoctorProbeKey == "" || product.PermissionProfileKey == "" ||
			len(product.RequiredDoctorFeatures) == 0 || !product.Acceptance.RealProductRequired {
			t.Fatalf("projection omitted derived contract fields: %#v", product)
		}
		if product.ID == "dsh" {
			if product.PeerAlias != "" || product.PeerTransport != "" || product.MessageTransport != "" || product.InstallRoot != "" || len(product.PluginArchivePaths) != 0 || product.NativeRegistration.Strategy != "" {
				t.Fatalf("lane-only DSH projection carries a peer integration: %#v", product)
			}
		} else if product.PeerAlias == "" || product.PeerTransport == "" || product.MessageTransport == "" || product.InstallRoot == "" || len(product.PluginArchivePaths) == 0 || product.NativeRegistration.Strategy == "" {
			t.Fatalf("peer projection omitted integration fields: %#v", product)
		}
	}
	copyProjection, err := BuildProjection(inventory)
	if err != nil {
		t.Fatal(err)
	}
	copyProjection.Products[0].Capabilities[0] = "changed"
	again, _ := BuildProjection(inventory)
	if reflect.DeepEqual(copyProjection, again) {
		t.Fatal("projection leaked mutation")
	}
}

func TestValidateInventoryRejectsTokenTupleAndDuplicateDrift(t *testing.T) {
	if err := ValidateInventory(All()); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "-bad", "bad-", "bad--token", "2bad", "Upper", "slash/value", string(bytes.Repeat([]byte{'a'}, 65))} {
		if ValidateToken(invalid) == nil {
			t.Fatalf("invalid token %q accepted", invalid)
		}
	}
	if err := ValidateToken("valid-token-2"); err != nil {
		t.Fatal(err)
	}
	for _, valid := range []string{"10.28.1", "0.1.2-alpha.3", "v1.2.3+build_4"} {
		if err := ValidateVersion(valid); err != nil {
			t.Fatalf("valid version %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", ".1", "1/2", "1 2", string(bytes.Repeat([]byte{'1'}, 129))} {
		if ValidateVersion(invalid) == nil {
			t.Fatalf("invalid version %q accepted", invalid)
		}
	}

	duplicate := All()
	duplicate[1].ID = duplicate[0].ID
	if err := ValidateInventory(duplicate); err == nil {
		t.Fatal("duplicate product accepted")
	}
	brokenTuple := All()
	brokenTuple[0].Compatibility.TupleMembers = []TupleMember{{Name: "cli", Version: "1"}}
	if err := ValidateInventory(brokenTuple); err == nil {
		t.Fatal("non-exact tuple accepted")
	}

	tests := []struct {
		name   string
		mutate func([]Descriptor)
	}{
		{name: "unknown capability", mutate: func(products []Descriptor) {
			products[0].Capabilities[0] = Capability("interactiv")
		}},
		{name: "invalid resume style", mutate: func(products []Descriptor) {
			products[0].ResumeStyle = ResumeStyle("guess")
		}},
		{name: "split federation capability", mutate: func(products []Descriptor) {
			products[0].FederationCapabilities[0] = "alternate-lane"
		}},
		{name: "wrong install root", mutate: func(products []Descriptor) {
			products[0].InstallRoot = "integrations/other"
		}},
		{name: "assetless product with registration", mutate: func(products []Descriptor) {
			products[8].NativeRegistration.Strategy = "unexpected-registration"
		}},
		{name: "lane-only product with peer alias", mutate: func(products []Descriptor) {
			products[8].PeerAlias = "dsh-peer"
		}},
		{name: "traversing archive path", mutate: func(products []Descriptor) {
			products[0].PluginArchivePaths[0] = "../outside"
		}},
		{name: "absolute archive path", mutate: func(products []Descriptor) {
			products[0].PluginArchivePaths[0] = "/outside"
		}},
		{name: "duplicate archive path", mutate: func(products []Descriptor) {
			products[0].PluginArchivePaths[1] = products[0].PluginArchivePaths[0]
		}},
		{name: "duplicate doctor feature", mutate: func(products []Descriptor) {
			products[0].RequiredDoctorFeatures = append(products[0].RequiredDoctorFeatures, products[0].RequiredDoctorFeatures[0])
		}},
		{name: "unsorted registration args", mutate: func(products []Descriptor) {
			products[0].NativeRegistration.Args = []string{"z", "a"}
		}},
		{name: "duplicate acceptance cell", mutate: func(products []Descriptor) {
			products[0].Acceptance.ExternalCells = []ExternalAcceptanceCell{{ID: "account"}, {ID: "account"}}
		}},
		{name: "unsorted acceptance cells", mutate: func(products []Descriptor) {
			products[0].Acceptance.ExternalCells = []ExternalAcceptanceCell{{ID: "z-cell"}, {ID: "a-cell"}}
		}},
		{name: "oversized registration args", mutate: func(products []Descriptor) {
			products[0].NativeRegistration.Args = nil
			for index := 0; index <= maxNativeRegistrationArgs; index++ {
				products[0].NativeRegistration.Args = append(products[0].NativeRegistration.Args, fmt.Sprintf("arg-%02d", index))
			}
		}},
		{name: "oversized acceptance cells", mutate: func(products []Descriptor) {
			products[0].Acceptance.ExternalCells = nil
			for index := 0; index <= maxExternalAcceptanceCells; index++ {
				products[0].Acceptance.ExternalCells = append(products[0].Acceptance.ExternalCells, ExternalAcceptanceCell{ID: fmt.Sprintf("cell-%02d", index)})
			}
		}},
		{name: "missing real product acceptance", mutate: func(products []Descriptor) {
			products[0].Acceptance.RealProductRequired = false
		}},
		{name: "tuple missing package manager version", mutate: func(products []Descriptor) {
			products[0].Compatibility = Compatibility{Policy: VersionExact, PackageManager: "pnpm", TupleMembers: []TupleMember{{Name: "cli", Version: "1.0.0"}}}
		}},
		{name: "package manager version without manager", mutate: func(products []Descriptor) {
			products[0].Compatibility = Compatibility{Policy: VersionExact, PackageManagerVersion: "10.28.1", TupleMembers: []TupleMember{{Name: "cli", Version: "1.0.0"}}}
		}},
		{name: "package manager outside exact tuple", mutate: func(products []Descriptor) {
			products[0].Compatibility = Compatibility{Policy: VersionMinimum, PackageManager: "pnpm", PackageManagerVersion: "10.28.1"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			products := All()
			test.mutate(products)
			if err := ValidateInventory(products); err == nil {
				t.Fatal("invalid catalog mutation accepted")
			}
		})
	}
}

func TestExactTupleProjectsPinnedPackageManagerVersion(t *testing.T) {
	inventory := All()
	inventory[0].Compatibility = Compatibility{
		Policy: VersionExact, PackageManager: "pnpm", PackageManagerVersion: "10.28.1",
		TupleMembers: []TupleMember{
			{Name: "@example/tool", Version: "0.1.2-alpha.3"},
			{Name: "@deepseek-ai/dsh", Version: "0.1.2-alpha.3"},
			{Name: "@deepseek-ai/dsh-acp-app", Version: "0.1.2-alpha.3"},
		},
	}
	body, err := ProjectionJSON(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"package_manager": "pnpm"`)) || !bytes.Contains(body, []byte(`"package_manager_version": "10.28.1"`)) {
		t.Fatalf("exact package-manager identity missing from projection: %s", body)
	}
	legacy := All()[1]
	if legacy.Compatibility.PackageManager != "" || legacy.Compatibility.PackageManagerVersion != "" {
		t.Fatalf("non-package-managed descriptor changed backward behavior: %#v", legacy.Compatibility)
	}
	legacyBody, err := ProjectionJSON([]Descriptor{legacy})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacyBody, []byte(`"package_manager_version"`)) {
		t.Fatalf("legacy descriptors did not omit additive manager version: %s", legacyBody)
	}
}
