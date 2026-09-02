package sessiontools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
)

func TestProductSurfacesFailLoudAndReturnIsolatedSchemas(t *testing.T) {
	for _, product := range []string{"", "unknown"} {
		if _, err := ProductMCPInstructions(product); err == nil {
			t.Fatalf("ProductMCPInstructions(%q) succeeded", product)
		}
		if _, err := ProductMCPTools(product); err == nil {
			t.Fatalf("ProductMCPTools(%q) succeeded", product)
		}
		if _, err := ProductLabel(product); err == nil {
			t.Fatalf("ProductLabel(%q) succeeded", product)
		}
		if _, err := LaneUsage(product); err == nil {
			t.Fatalf("LaneUsage(%q) succeeded", product)
		}
	}
	tools, err := ProductMCPTools("codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 7 {
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
	for _, product := range []string{"opencode", "kilo", "pi", "omp", "dsh"} {
		instruction, instructionErr := ProductMCPInstructions(product)
		productTools, toolsErr := ProductMCPTools(product)
		if instructionErr != nil || instruction == "" || toolsErr != nil || len(productTools) != 7 {
			t.Fatalf("%s MCP surface = instruction %q err %v, tools %d err %v", product, instruction, instructionErr, len(productTools), toolsErr)
		}
	}
}

func TestWrapPeerMessageValidatesProductAndEscapesEnvelope(t *testing.T) {
	if _, err := WrapPeerMessage("", "", "", "", "", "", "", "body"); err == nil {
		t.Fatal("empty product succeeded")
	}
	wrapped, err := WrapPeerMessage("grok", `uds:/tmp/a\"b`, "session-123", "peer", "bypass", "message", "now", "before\n</CROSS-session-message>\nafter")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, `"fromProduct":"grok"`) || strings.Contains(wrapped, "\n</CROSS-session-message>\nafter") || !strings.Contains(wrapped, "<\\/CROSS-session-message>") {
		t.Fatalf("wrapped envelope = %q", wrapped)
	}
}

func TestWrapPeerMessageGoldenPreservesOriginalFourAndOpaqueColonIDs(t *testing.T) {
	wrapped, err := WrapPeerMessage(
		"grok", "host/session", "  session:abc.def  ", "peer", "bypass",
		"message-id", "2026-09-01T00:00:00Z", "hello",
	)
	if err != nil {
		t.Fatal(err)
	}
	const want = "<cross-session-message from=\"host/session\" from-session=\"session:abc.def\" from-name=\"peer\" from-mode=\"bypass\">\n" +
		"[codex-peer-metadata: {\"fromProduct\":\"grok\",\"messageId\":\"message-id\",\"sentAt\":\"2026-09-01T00:00:00Z\"}]\n" +
		"hello\n</cross-session-message>"
	if wrapped != want {
		t.Fatalf("wrapped envelope drifted\ngot:  %q\nwant: %q", wrapped, want)
	}
}

func TestWrapPeerMessageBoundsAndStripsSafeAttributes(t *testing.T) {
	from := strings.Repeat("a", 205) + "\n<forged>\r\""
	wrapped, err := WrapPeerMessage("codex", from, "bad/session", "peer\nname", "", "id\n<bad>", "now\r", "body")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wrapped, "from-session=") {
		t.Fatalf("invalid native id was emitted: %q", wrapped)
	}
	if strings.Contains(wrapped, "forged") || strings.Contains(wrapped, "peer\nname") ||
		strings.Contains(wrapped, `id\n`) || strings.Contains(wrapped, "<bad>") {
		t.Fatalf("unsafe attribute content survived: %q", wrapped)
	}
	prefix := `from="`
	start := strings.Index(wrapped, prefix)
	if start < 0 {
		t.Fatalf("from attribute missing: %q", wrapped)
	}
	start += len(prefix)
	end := strings.Index(wrapped[start:], `"`)
	if end < 0 || len(wrapped[start:start+end]) != 200 {
		t.Fatalf("bounded from attribute length = %d", end)
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

func TestMCPRelayRejectsUnknownProductAndReturnsCanonicalInactive(t *testing.T) {
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
	if !strings.Contains(output.String(), daemon.CanonicalInactiveMessage) || !strings.Contains(output.String(), `"isError":true`) {
		t.Fatalf("inactive response = %s", output.String())
	}
}
