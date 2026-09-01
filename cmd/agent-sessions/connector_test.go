package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAutomaticConnectorProductUsesManagedEnvironmentAndCodexFallback(t *testing.T) {
	for _, test := range []struct {
		name, product, grokSession, want string
	}{
		{name: "codex fallback", want: "codex"},
		{name: "codex explicit environment", product: "codex", want: "codex"},
		{name: "claude", product: "claude", want: "claude"},
		{name: "qwen", product: "qwen", want: "qwen"},
		{name: "grok product", product: "grok", want: "grok"},
		{name: "grok private launch", grokSession: "session", want: "grok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(name string) string {
				switch name {
				case "AGENT_SESSIONS_PRODUCT":
					return test.product
				case "AGENT_SESSIONS_GROK_SESSION_ID":
					return test.grokSession
				default:
					return ""
				}
			}
			got, err := resolveConnectorProduct("auto", getenv)
			if err != nil || got != test.want {
				t.Fatalf("resolve automatic connector = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if _, err := resolveConnectorProduct("auto", func(string) string { return "imaginary" }); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid automatic connector error = %v", err)
	}
}

func TestConnectorReportsItsProductSessionWithoutDurableLookup(t *testing.T) {
	getenv := func(name string) string {
		if name == "AGENT_SESSIONS_SESSION_ID" {
			return "qwen-session"
		}
		return ""
	}
	got, err := attestConnector("qwen", nil, getenv)
	if err != nil || got.AttachmentID != "qwen-session" || got.Evidence.ThreadID != "qwen-session" {
		t.Fatalf("Qwen connector report = %+v, %v", got, err)
	}
	codexParams, _ := json.Marshal(map[string]any{"_meta": map[string]any{
		"threadId":              "codex-thread",
		"x-codex-turn-metadata": map[string]any{"session_id": "codex-session", "thread_id": "codex-thread"},
	}})
	got, err = attestConnector("codex", codexParams, func(string) string { return "" })
	if err != nil || got.AttachmentID != "codex-thread" {
		t.Fatalf("Codex connector report = %+v, %v", got, err)
	}
}
