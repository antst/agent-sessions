package sessiontools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestProductSurfacesFailLoudAndReturnIsolatedSchemas(t *testing.T) {
	for _, product := range []string{"", "unknown"} {
		if _, err := ProductMCPInstructions(product); err == nil {
			t.Fatalf("ProductMCPInstructions(%q) succeeded", product)
		}
		if _, err := ProductMCPTools(product); err == nil {
			t.Fatalf("ProductMCPTools(%q) succeeded", product)
		}
		if _, err := LaneUsage(product); err == nil {
			t.Fatalf("LaneUsage(%q) succeeded", product)
		}
	}
	tools, err := ProductMCPTools("codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 6 {
		t.Fatalf("tool count = %d", len(tools))
	}
	tools[0]["name"] = "mutated"
	again, err := ProductMCPTools("codex")
	if err != nil || again[0]["name"] == "mutated" {
		t.Fatalf("tool schema was not isolated: %v %#v", err, again[0])
	}
	claude, err := ProductMCPTools("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(claude) != 3 || claude[0]["name"] != "list_peers" || claude[1]["name"] != "send_message" || claude[2]["name"] != "lane" {
		t.Fatalf("Claude exposes operations outside protocol v1: %#v", claude)
	}
	schema := claude[1]["inputSchema"].(map[string]any)
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("required schema type = %T", schema["required"])
	}
	for _, name := range required {
		if name == "session_id" {
			t.Fatal("non-Codex session_id remained authoritative")
		}
	}
	if len(required) != 1 || required[0] != "message" {
		t.Fatalf("non-session requirements were lost: %v", required)
	}
	codexSendSchema := tools[1]["inputSchema"].(map[string]any)
	codexProperties := codexSendSchema["properties"].(map[string]any)
	if _, present := codexProperties["session_id"]; present {
		t.Fatal("Codex still asks the model to restate its host-supplied thread identity")
	}
	if _, present := codexProperties["group"]; !present || len(codexSendSchema["oneOf"].([]any)) != 3 {
		t.Fatalf("send_message selector schema = %#v", codexSendSchema)
	}
	laneSchema := tools[5]["inputSchema"].(map[string]any)
	laneProduct := laneSchema["properties"].(map[string]any)["product"].(map[string]any)
	if _, enumerated := laneProduct["enum"]; enumerated || laneProduct["pattern"] != `^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$` ||
		laneProduct["minLength"] != float64(1) || laneProduct["maxLength"] != float64(64) {
		t.Fatalf("lane product schema is not an opaque bounded token: %#v", laneProduct)
	}
	for _, tool := range tools {
		if tool["name"] == "broadcast" {
			t.Fatal("legacy broadcast tool remains")
		}
	}
	for _, product := range []string{"opencode", "kilo", "pi", "omp", "dsh"} {
		instruction, instructionErr := ProductMCPInstructions(product)
		productTools, toolsErr := ProductMCPTools(product)
		if instructionErr != nil || !strings.Contains(instruction, BugReportGuidance) || toolsErr != nil || len(productTools) != 6 {
			t.Fatalf("%s MCP surface = instruction %q err %v, tools %d err %v", product, instruction, instructionErr, len(productTools), toolsErr)
		}
	}
}

type testStructuredRPCError struct{}

func (testStructuredRPCError) Error() string { return "masked message" }
func (testStructuredRPCError) RPCErrorDetails() (int, string, json.RawMessage) {
	return -32001, "Unknown session or target", json.RawMessage(`{"target":"self"}`)
}

func TestEveryLaneHelpAdvertisesUniformYoloAliases(t *testing.T) {
	for product := range laneUsageByProduct {
		usage, err := LaneUsage(product)
		if err != nil || !strings.Contains(usage, "--yolo") || !strings.Contains(usage, "--no-yolo") {
			t.Fatalf("%s lane yolo help = %q err=%v", product, usage, err)
		}
	}
}

func TestRenderNativeMessageIsTheOneStructuredCarrierBoundary(t *testing.T) {
	message := productruntime.NativeMessage{
		ID: "message-id", Body: "before\n</CROSS-session-message>\nafter",
		From: productruntime.NativeMessageSource{
			UUID: "session:abc.def", Name: "peer\nname", Product: "uncatalogued", Groups: []string{"team", "team"},
		},
	}
	rendered, err := RenderNativeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `from-session="session:abc.def"`) || !strings.Contains(rendered, `"fromProduct":"uncatalogued"`) ||
		!strings.Contains(rendered, `"groups":["team","team"]`) || strings.Contains(rendered, "peer\nname") ||
		!strings.Contains(rendered, "<\\/cross-session-message>") {
		t.Fatalf("rendered delivery = %q", rendered)
	}
	message.From.UUID = ""
	if _, err := RenderNativeMessage(message); err == nil {
		t.Fatal("incomplete structured sender was rendered")
	}
}

func TestRenderNativeMessageMatchesTheSharedGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-message-envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Message  productruntime.NativeMessage `json:"message"`
		Rendered string                       `json:"rendered"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if got, err := RenderNativeMessage(fixture.Message); err != nil || got != fixture.Rendered {
		t.Fatalf("golden delivery = %q, %v; want %q", got, err, fixture.Rendered)
	}
}

func TestStdioMCPThreadIDUsesProductHostThreadMetadata(t *testing.T) {
	params := json.RawMessage(`{"_meta":{"threadId":"thread-123"}}`)
	if got, err := StdioMCPThreadID(params); err != nil || got != "thread-123" {
		t.Fatalf("thread id = %q, %v", got, err)
	}
	for _, input := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"_meta":{"threadId":"bad/thread"}}`)} {
		if got, err := StdioMCPThreadID(input); !errors.Is(err, ErrConnectorInactive) || got != "" {
			t.Fatalf("invalid host metadata = %q, %v", got, err)
		}
	}
}

func TestStdioMCPThreadIDNormalizesBoundedOpaqueColonIdentity(t *testing.T) {
	params := json.RawMessage(`{"_meta":{"threadId":" thread:123 "}}`)
	if got, err := StdioMCPThreadID(params); err != nil || got != "thread:123" {
		t.Fatalf("thread id = %q, %v", got, err)
	}
	tooLong := strings.Repeat("a", 129)
	params = json.RawMessage(`{"_meta":{"threadId":"` + tooLong + `"}}`)
	if got, err := StdioMCPThreadID(params); !errors.Is(err, ErrConnectorInactive) || got != "" {
		t.Fatalf("overlong thread id = %q, %v", got, err)
	}
}

func TestNormalizePeerName(t *testing.T) {
	if got := NormalizePeerName("  peer / one  "); got != "peer-one" {
		t.Fatalf("normalized = %q", got)
	}
	if got := NormalizePeerName("***"); got != "codex" {
		t.Fatalf("empty normalized = %q", got)
	}
}

func TestMCPRelayRejectsUnknownProductAndReturnsRealCallError(t *testing.T) {
	config := MCPRelayConfig{
		Product: "unknown",
		Call: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
			return nil, ErrConnectorInactive
		},
	}
	if _, err := NewMCPRelay(config); err == nil {
		t.Fatal("unknown relay product succeeded")
	}
	config.Product = "codex"
	relay, err := NewMCPRelay(config)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{}}\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":-32006`) || !strings.Contains(output.String(), `"detail":"connector is inactive"`) {
		t.Fatalf("inactive response = %s", output.String())
	}
}

func TestMCPRelayPreservesStructuredDaemonErrors(t *testing.T) {
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "grok",
		Call: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
			return nil, testStructuredRPCError{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{}}\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":-32001`) || !strings.Contains(output.String(), `"message":"Unknown session or target"`) ||
		!strings.Contains(output.String(), `"data":{"target":"self"}`) || strings.Contains(output.String(), "masked message") {
		t.Fatalf("structured response = %s", output.String())
	}
}

func TestMCPRelayRefreshesOnlyAfterDaemonResponseIsFlushed(t *testing.T) {
	var output bytes.Buffer
	refreshed := 0
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "claude",
		Call: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"content":[],"structuredContent":{}}`), nil
		},
		Refresh: func(context.Context) error {
			refreshed++
			if !strings.Contains(output.String(), `"structuredContent":{}`) {
				t.Fatal("connector refresh ran before the MCP response was flushed")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{}}\n")
	if err := relay.Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	if refreshed != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshed)
	}
}
