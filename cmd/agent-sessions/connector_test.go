package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
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
		{name: "claude", product: "claude", want: "claude"},
		{name: "qwen", product: "qwen", want: "qwen"},
		{name: "grok product", product: "grok", want: "grok"},
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
	automatic, err := resolveConnectorProduct("auto", getenv)
	if err != nil || automatic != "codex" || connectorClaimsLivePresence(automatic, getenv) {
		t.Fatalf("automatic discovered connector = %q, %v; must stay inactive", automatic, err)
	}
}

func TestProjectDiscoveredConnectorNeverReplacesNativePresence(t *testing.T) {
	for _, product := range []string{"opencode", "kilo", "pi", "omp", "dsh"} {
		if connectorOwnsLivePresence(product) {
			t.Fatalf("%s project connector would replace its native presence stream", product)
		}
	}
	for _, product := range []string{"codex", "claude", "grok", "qwen", "codebuddy"} {
		if !connectorOwnsLivePresence(product) {
			t.Fatalf("%s connector lost its only live presence stream", product)
		}
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

func TestQwenConnectorDeliversOverItsLiveProductAdapter(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(input, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_SESSIONS_QWEN_INPUT_FILE", input)
	params, _ := json.Marshal(map[string]any{"message_id": "message-1", "body": "hello"})
	if _, err := connectorNativeCall(context.Background(), liveSessionReport{UUID: "session-1", Product: "qwen"}, "message.deliver", params); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("native Qwen input = %q", body)
	}
}
