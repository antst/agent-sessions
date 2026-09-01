package bridge

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/sessiontools"
)

func TestExtractedSessionToolsPreserveOriginalFourPublicSchemas(t *testing.T) {
	projectedTools := map[string][]map[string]any{
		"codex": nativeToolDefinitions, "claude": claudeToolDefinitions,
		"grok": grokToolDefinitions, "qwen": qwenToolDefinitions,
	}
	projectedInstructions := map[string]string{
		"codex": mcpInstructions, "claude": claudeMCPInstructions,
		"grok": grokMCPInstructions, "qwen": qwenMCPInstructions,
	}
	for product, projected := range projectedTools {
		want, err := sessiontools.ProductMCPTools(product)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(projected, want) {
			t.Fatalf("%s bridge tool projection drifted\ngot:  %#v\nwant: %#v", product, projected, want)
		}
		body, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var canonical []map[string]any
		if err := json.Unmarshal(body, &canonical); err != nil {
			t.Fatal(err)
		}
		if got := ProductMCPTools(product); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("%s extracted tools drifted\ngot:  %#v\nwant: %#v", product, got, want)
		}
		wantInstruction, err := sessiontools.ProductMCPInstructions(product)
		if err != nil {
			t.Fatal(err)
		}
		if projectedInstructions[product] != wantInstruction || ProductMCPInstructions(product) != wantInstruction {
			t.Fatalf("%s extracted instructions drifted", product)
		}
	}
}

func TestBridgePeerEnvelopeMatchesSessionToolsGolden(t *testing.T) {
	const want = "<cross-session-message from=\"host/session\" from-session=\"session:abc.def\" from-name=\"peer\" from-mode=\"bypass\">\n" +
		"[codex-peer-metadata: {\"fromProduct\":\"grok\",\"messageId\":\"message-id\",\"sentAt\":\"2026-09-01T00:00:00Z\"}]\n" +
		"hello\n</cross-session-message>"
	got := wrapNativePeerMessageForProduct(
		"grok", "host/session", " session:abc.def ", "peer", "bypass",
		"message-id", "2026-09-01T00:00:00Z", "hello",
	)
	if got != want {
		t.Fatalf("bridge envelope drifted\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBridgePeerEnvelopeUnknownProductFailsLoud(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unknown bridge envelope product did not panic")
		}
	}()
	_ = wrapNativePeerMessageForProduct("unknown", "from", "session", "name", "", "id", "now", "body")
}
