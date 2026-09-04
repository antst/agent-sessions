package productruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestLaneWorkerSchemaMatchesAuthoritativeGoStructs(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "integrations", "shared", "lane-worker.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	types := map[string]reflect.Type{
		"LaneOpenRequest":      reflect.TypeOf(LaneOpenRequest{}),
		"LaneWorkerHello":      reflect.TypeOf(LaneWorkerHello{}),
		"LaneDoctorResult":     reflect.TypeOf(LaneDoctorResult{}),
		"LaneTurnStartRequest": reflect.TypeOf(LaneTurnStartRequest{}),
		"LaneStatusProjection": reflect.TypeOf(LaneStatusProjection{}),
	}
	if len(schema.Definitions) != len(types) {
		t.Fatalf("schema definitions = %v", reflect.ValueOf(schema.Definitions).MapKeys())
	}
	for name, typ := range types {
		definition, ok := schema.Definitions[name]
		if !ok {
			t.Fatalf("schema omits %s", name)
		}
		properties, required := jsonFields(typ)
		actual := make([]string, 0, len(definition.Properties))
		for property := range definition.Properties {
			actual = append(actual, property)
		}
		sort.Strings(actual)
		sort.Strings(definition.Required)
		if !reflect.DeepEqual(actual, properties) || !reflect.DeepEqual(definition.Required, required) {
			t.Fatalf("%s schema fields = %v required %v; Go fields = %v required %v", name, actual, definition.Required, properties, required)
		}
	}
}

func TestLaneWorkerWireTypesRejectUnknownNullAndInvalidTimeout(t *testing.T) {
	schema := testLaneWireSchema(t)
	open := `{"name":"worker","cwd":"/tmp/work","permission_mode":"default","resume":false,"auto_archive_after_seconds":60,"arguments":[]}`
	if err := schema.Decode("LaneOpenRequest", []byte(open), &LaneOpenRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.TrimSuffix(open, "}") + `,"extra":true}`,
		strings.Replace(open, `"name":"worker"`, `"name":null`, 1),
	} {
		if err := schema.Decode("LaneOpenRequest", []byte(invalid), &LaneOpenRequest{}); err == nil {
			t.Fatalf("invalid open accepted: %s", invalid)
		}
	}
	turn := `{"input_id":"input-1","body":"work","mode":"followup","timeout_seconds":0.5}`
	if err := schema.Decode("LaneTurnStartRequest", []byte(turn), &LaneTurnStartRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(turn, "0.5", "0", 1),
		strings.Replace(turn, `"followup"`, `"other"`, 1),
		strings.TrimSuffix(turn, "}") + `,"extra":true}`,
	} {
		if err := schema.Decode("LaneTurnStartRequest", []byte(invalid), &LaneTurnStartRequest{}); err == nil {
			t.Fatalf("invalid turn accepted: %s", invalid)
		}
	}
}

func TestLaneWorkerSharedFixtures(t *testing.T) {
	schema := testLaneWireSchema(t)
	body, err := os.ReadFile(filepath.Join("..", "..", "integrations", "shared", "lane-worker.fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Definition string          `json:"definition"`
		Valid      bool            `json:"valid"`
		Value      json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &fixtures); err != nil || len(fixtures) != 49 {
		t.Fatalf("decode shared fixtures: %v (%d)", err, len(fixtures))
	}
	for _, fixture := range fixtures {
		var output any
		switch fixture.Definition {
		case "LaneOpenRequest":
			output = &LaneOpenRequest{}
		case "LaneWorkerHello":
			output = &LaneWorkerHello{}
		case "LaneDoctorResult":
			output = &LaneDoctorResult{}
		case "LaneTurnStartRequest":
			output = &LaneTurnStartRequest{}
		case "LaneStatusProjection":
			output = &LaneStatusProjection{}
		default:
			t.Fatalf("unknown shared fixture definition %q", fixture.Definition)
		}
		decodeErr := schema.Decode(fixture.Definition, fixture.Value, output)
		if (decodeErr == nil) != fixture.Valid {
			t.Fatalf("%s valid=%v decode error=%v", fixture.Definition, fixture.Valid, decodeErr)
		}
	}
}

func testLaneWireSchema(t *testing.T) *LaneWireSchema {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "integrations", "shared", "lane-worker.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ParseLaneWireSchema(body)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func jsonFields(typ reflect.Type) ([]string, []string) {
	properties, required := []string{}, []string{}
	for index := 0; index < typ.NumField(); index++ {
		tag := typ.Field(index).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		properties = append(properties, name)
		if options != "omitempty" {
			required = append(required, name)
		}
	}
	sort.Strings(properties)
	sort.Strings(required)
	return properties, required
}

func TestRegistrySupportsInjectedSyntheticProductWithoutInitRegistration(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	product := completeRuntime(descriptor)
	registry, err := NewRegistry([]productcatalog.Descriptor{descriptor}, []RuntimeProduct{product})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := registry.ByID("synthetic")
	if !ok || got.Descriptor.ID != "synthetic" || len(registry.All()) != 1 {
		t.Fatalf("synthetic runtime = %#v, %v", got, ok)
	}
	got.Descriptor.Capabilities[0] = "mutated"
	again, _ := registry.ByID("synthetic")
	if again.Descriptor.Capabilities[0] == "mutated" {
		t.Fatal("registry leaked descriptor mutation")
	}
	if _, present := productcatalog.ByID("synthetic"); present {
		t.Fatal("synthetic runtime escaped into production catalog")
	}
}

func TestRegistryRejectsMissingExtraAndMismatchedDrivers(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	complete := completeRuntime(descriptor)
	tests := []struct {
		name      string
		inventory []productcatalog.Descriptor
		products  []RuntimeProduct
	}{
		{name: "missing runtime", inventory: []productcatalog.Descriptor{descriptor}},
		{name: "duplicate runtime", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{complete, complete}},
		{name: "unknown runtime", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{completeRuntime(syntheticDescriptor("other"))}},
		{name: "missing lane", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Doctor: fakeDoctor{}}}},
		{name: "missing doctor", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Lane: fakeLaneDriver{}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.inventory, test.products); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
}

func TestRuntimeSecretsCannotSerializeOrAppearInFormatting(t *testing.T) {
	secret := NewSensitiveValue("raw-secret-value")
	if secret.String() != "[REDACTED]" || secret.GoString() != "[REDACTED]" || secret.Reveal() != "raw-secret-value" {
		t.Fatalf("sensitive value contract failed: %s", secret)
	}
	if _, err := json.Marshal(secret); err == nil {
		t.Fatal("sensitive value serialized")
	}
	if _, err := json.Marshal(NativeCommand{Path: "/bin/native", SensitiveEnv: []SensitiveEnvVar{{Name: "TOKEN", Value: secret}}}); err == nil {
		t.Fatal("native command serialized")
	}
	detail := NewRedactedString("raw-secret-value\n"+strings.Repeat("x", maxRedactedDetailBytes+20), secret)
	encoded, err := json.Marshal(detail)
	if err != nil || len(detail.String()) > maxRedactedDetailBytes || strings.Contains(string(encoded), "raw-secret-value") || strings.Contains(detail.String(), "\n") {
		t.Fatalf("redacted detail leaked or exceeded its bound: %q, %v", encoded, err)
	}
	automatic := NewRedactedString("request failed: Authorization: Bearer native-token password=hunter2")
	if strings.Contains(automatic.String(), "native-token") || strings.Contains(automatic.String(), "hunter2") {
		t.Fatalf("named native credential leaked: %q", automatic)
	}
	_, err = (fakeLaneDriver{}).Steer(context.Background(), NativeTurnRef{}, TurnStartRequest{})
	if !errors.Is(err, ErrUnsupportedSteer) {
		t.Fatal("typed unsupported steer was lost")
	}
	if reflect.TypeOf(NativeSessionRef{}).NumField() != 3 || reflect.TypeOf(NativeTurnRef{}).NumField() != 2 {
		t.Fatal("native references grew endpoint/token fields")
	}
}

func syntheticDescriptor(id string) productcatalog.Descriptor {
	descriptor := productcatalog.All()[0]
	descriptor.ID = id
	descriptor.Label = "Synthetic"
	descriptor.NativeExecutable = id
	descriptor.PeerAlias = id + "-peer"
	descriptor.LaneAlias = id + "-peer-lane"
	descriptor.LaneCapability = id + "-lane"
	descriptor.FederationCapabilities = []string{id + "-lane"}
	descriptor.PeerTransport = id + "-peer"
	descriptor.MessageTransport = id + "-message"
	descriptor.LaneTransport = id + "-lane"
	descriptor.DoctorProbeKey = id + "-doctor"
	descriptor.PermissionProfileKey = id + "-permission"
	descriptor.InstallRoot = "integrations/" + id
	return descriptor
}

func completeRuntime(descriptor productcatalog.Descriptor) RuntimeProduct {
	return RuntimeProduct{Descriptor: descriptor, Lane: fakeLaneDriver{}, Doctor: fakeDoctor{}}
}
