package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticConnectorProductUsesManagedEnvironmentAndCodexFallback(t *testing.T) {
	for _, test := range []struct {
		name, product, grokSession, want string
	}{
		{name: "codex fallback", want: "codex"},
		{name: "codex explicit environment", product: "codex", want: "codex"},
		{name: "claude", product: "claude", want: "codex"},
		{name: "qwen", product: "qwen", want: "codex"},
		{name: "grok product", product: "grok", want: "codex"},
		{name: "OMP native extension", product: "omp", want: "codex"},
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

func TestManagedLaunchProductOverridesDiscoveredConnectorArgument(t *testing.T) {
	getenv := func(name string) string {
		if name == "AGENT_SESSIONS_PRODUCT" {
			return "omp"
		}
		return ""
	}
	got, err := resolveConnectorProduct("claude", getenv)
	if err != nil || got != "claude" {
		t.Fatalf("explicit connector product = %q, %v; want claude", got, err)
	}
	if connectorClaimsLivePresence(got, getenv) {
		t.Fatal("stale Claude connector argument would replace OMP native presence")
	}
	if !connectorDeclinesForeignManagedProduct("claude", got, getenv) {
		t.Fatal("foreign explicit Claude connector would serve a second OMP tool surface")
	}
	automatic, err := resolveConnectorProduct("auto", getenv)
	if err != nil || automatic != "codex" || connectorClaimsLivePresence(automatic, getenv) {
		t.Fatalf("automatic discovered connector = %q, %v; must stay inactive", automatic, err)
	}
	if connectorDeclinesForeignManagedProduct("auto", automatic, getenv) {
		t.Fatal("automatic connector lost its existing tools-only behavior")
	}
	matching := func(name string) string {
		if name == "AGENT_SESSIONS_PRODUCT" {
			return "claude"
		}
		return ""
	}
	if connectorDeclinesForeignManagedProduct("claude", "claude", matching) {
		t.Fatal("matching Claude connector was declined")
	}
}

func TestProjectDiscoveredConnectorNeverReplacesNativePresence(t *testing.T) {
	for _, product := range []string{"codex", "grok", "qwen", "opencode", "kilo", "pi", "omp", "dsh"} {
		if connectorOwnsLivePresence(product) {
			t.Fatalf("%s project connector would replace its native presence stream", product)
		}
	}
	for _, product := range []string{"claude"} {
		if !connectorOwnsLivePresence(product) {
			t.Fatalf("%s connector lost its only live presence stream", product)
		}
	}
}

func TestToolsOnlyConnectorUsesAmbientOrProductHostSession(t *testing.T) {
	grokArguments := map[string]any{"session_id": "host-session", "name": "identity"}
	if sourceID := connectorToolSource("grok", "grok-session", nil, grokArguments); sourceID != "grok-session" {
		t.Fatalf("Grok source = %q", sourceID)
	}
	if _, present := grokArguments["session_id"]; present {
		t.Fatal("Grok transport identity leaked into tool arguments")
	}
	codexArguments := map[string]any{"session_id": "model-session", "name": "identity"}
	codexParams := json.RawMessage(`{"name":"identity","arguments":{"session_id":"model-session"},"_meta":{"threadId":"codex-session"}}`)
	if sourceID := connectorToolSource("codex", "", codexParams, codexArguments); sourceID != "codex-session" {
		t.Fatalf("Codex source = %q", sourceID)
	}
	if _, present := codexArguments["session_id"]; present {
		t.Fatal("Codex transport identity leaked into tool arguments")
	}
	hostArguments := map[string]any{"session_id": "host-session"}
	if sourceID := connectorToolSource("qwen", "", nil, hostArguments); sourceID != "host-session" {
		t.Fatalf("host argument source = %q", sourceID)
	}
	if sourceID := connectorToolSource("codex", "", json.RawMessage(`{"name":"list_peers","arguments":{}}`), map[string]any{}); sourceID != "" {
		t.Fatalf("bare connector source = %q", sourceID)
	}
}

func TestClaudeConnectorDeliversOverItsLiveProductAdapter(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "claude.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	received := make(chan map[string]any, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		var message map[string]any
		if json.NewDecoder(bufio.NewReader(connection)).Decode(&message) == nil {
			received <- message
		}
	}()
	params, _ := json.Marshal(map[string]any{"message_id": "message-1", "body": "hello"})
	if _, err := connectorNativeCall(context.Background(), liveSessionReport{UUID: "session-1", Product: "claude"}, "message.deliver", params); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if message["msg_id"] != "message-1" || message["type"] != "user" {
			t.Fatalf("native Claude message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("native Claude delivery did not arrive")
	}
}
