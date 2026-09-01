package productcatalog

import (
	"bytes"
	"encoding/json"
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
	for _, forbidden := range [][]byte{[]byte("token"), []byte("password"), []byte("secret"), []byte("endpoint")} {
		if bytes.Contains(bytes.ToLower(first), forbidden) {
			t.Fatalf("projection contains secret-shaped field %q", forbidden)
		}
	}
	var decoded Projection
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ProjectionSchemaV1 || len(decoded.Products) != 4 || decoded.Products[0].ID != "claude" || decoded.Products[3].ID != "qwen" {
		t.Fatalf("projection = %#v", decoded)
	}
	for _, product := range decoded.Products {
		if product.PeerTransport == "" || product.MessageTransport == "" || product.LaneTransport == "" ||
			product.ConnectorAttesterKey == "" || product.DoctorProbeKey == "" || product.PermissionProfileKey == "" ||
			product.InstallRoot == "" || len(product.PluginArchivePaths) == 0 || len(product.RequiredDoctorFeatures) == 0 {
			t.Fatalf("projection omitted derived contract fields: %#v", product)
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
