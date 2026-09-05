package livepresence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type sessionFixtureFile struct {
	Cases []struct {
		Name, Definition, Raw string
		Valid                 bool
		Value                 json.RawMessage
		Repeat                *struct {
			Path  []string
			Text  string
			Count int
		}
	} `json:"cases"`
}

func TestUniversalSessionSchemaFixtures(t *testing.T) {
	shared := filepath.Join("..", "protocol")
	rawSchema, err := os.ReadFile(filepath.Join(shared, "session.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := CompileSessionSchema(rawSchema)
	if err != nil {
		t.Fatal(err)
	}
	rawFixtures, err := os.ReadFile(filepath.Join(shared, "session.fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures sessionFixtureFile
	if err := json.Unmarshal(rawFixtures, &fixtures); err != nil {
		t.Fatal(err)
	}
	coverage := make(map[string][2]bool)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			raw := fixture.Value
			if fixture.Raw != "" {
				raw = []byte(fixture.Raw)
			}
			if fixture.Repeat != nil {
				raw = repeatedSessionFixture(t, raw, fixture.Repeat.Path, fixture.Repeat.Text, fixture.Repeat.Count)
			}
			err := schema.ValidateJSON(fixture.Definition, raw)
			if (err == nil) != fixture.Valid {
				t.Fatalf("valid = %t, want %t: %v", err == nil, fixture.Valid, err)
			}
			seen := coverage[fixture.Definition]
			seen[0], seen[1] = seen[0] || fixture.Valid, seen[1] || !fixture.Valid
			coverage[fixture.Definition] = seen
		})
	}
	for _, definition := range schema.Definitions() {
		if coverage[definition] != [2]bool{true, true} {
			t.Errorf("definition %s lacks valid and invalid fixtures: %v", definition, coverage[definition])
		}
	}
	if len(coverage) != len(schema.Definitions()) {
		t.Fatalf("fixtures cover %d definitions, schema has %d", len(coverage), len(schema.Definitions()))
	}
}

func TestUniversalSessionSchemaRejectsUnsharedKeywords(t *testing.T) {
	path := filepath.Join("..", "protocol", "session.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	defs := root["$defs"].(map[string]any)
	hello := defs["SessionHelloRequest"].(map[string]any)
	properties := hello["properties"].(map[string]any)
	properties["product"].(map[string]any)["pattern"] = "^never-shared$"
	mutated, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileSessionSchema(mutated); err == nil || !strings.Contains(err.Error(), "unsupported schema keyword") {
		t.Fatalf("unsupported keyword error = %v", err)
	}
	delete(properties["product"].(map[string]any), "pattern")
	open := defs["SessionOpenRequest"].(map[string]any)["properties"].(map[string]any)["open"].(map[string]any)
	open["$ref"] = "#/$defs/Missing"
	mutated, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileSessionSchema(mutated); err == nil {
		t.Fatal("unknown definition reference compiled")
	}
	root["$defs"] = map[string]any{}
	mutated, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileSessionSchema(mutated); err == nil {
		t.Fatal("empty definition table compiled")
	}
}

func repeatedSessionFixture(t *testing.T, raw []byte, path []string, text string, count int) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	current := value.(map[string]any)
	for _, key := range path[:len(path)-1] {
		current = current[key].(map[string]any)
	}
	current[path[len(path)-1]] = strings.Repeat(text, count)
	expanded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return expanded
}

func TestUniversalSessionSchemaDefinitionsStayClosed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "protocol", "session.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := CompileSessionSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DeliveryReceipt", "DeliverySource", "ExtraArgument", "HostProducts", "LaneDescribeRequest", "LaneDescribeResult", "LaneSpawnRequest", "LaneSpawnResult", "MessageDeliverRequest", "MessageDeliverResult", "MessageSendDelivery", "MessageSendRequest", "MessageSendResult", "RPCError", "RPCErrorResponse", "SessionCloseRequest", "SessionCloseResult", "SessionHelloRequest", "SessionHelloResult", "SessionListRequest", "SessionListResult", "SessionOpenOptions", "SessionOpenRequest", "SessionOpenResult", "SessionSummary", "SessionSupersededRequest", "SessionSupersededResult", "SpawnFailedData", "TurnInterruptRequest", "TurnInterruptResult", "TurnRunRequest", "TurnRunResult"}
	if got := schema.Definitions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("definitions = %v, want %v", got, want)
	}
}

func TestGeneratedProtocolMatchesDesign(t *testing.T) {
	design, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "designs", "UNIVERSAL-SESSION-PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	wire := protocolSection(t, string(design), "## 1. Wire\n", "## 2. Daemon\n")
	kit := protocolSection(t, string(design), "### 3.1 Product contract\n", "### 3.2 Full-duplex lifecycle\n")
	want := wire + strings.TrimSuffix(kit, "\n")
	got, err := os.ReadFile(filepath.Join("..", "..", "docs", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("bus/docs/PROTOCOL.md drifted; regenerate it verbatim from design sections 1 and 3.1")
	}
}

func protocolSection(t *testing.T, document, start, end string) string {
	t.Helper()
	from, to := strings.Index(document, start), strings.Index(document, end)
	if from < 0 || to <= from {
		t.Fatalf("missing protocol section %q before %q", start, end)
	}
	return document[from:to]
}
