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
	for _, product := range []string{"codex", "grok", "opencode", "kilo", "pi", "omp", "dsh"} {
		if connectorOwnsLivePresence(product) {
			t.Fatalf("%s project connector would replace its native presence stream", product)
		}
	}
	for _, product := range []string{"claude", "qwen"} {
		if !connectorOwnsLivePresence(product) {
			t.Fatalf("%s connector lost its only live presence stream", product)
		}
	}
}

func TestQwenConnectorProjectsOnlyProductOwnedNativeTitle(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".qwen")
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if connectorCwd, err := os.Getwd(); err != nil || filepath.Clean(connectorCwd) == filepath.Clean(cwd) {
		t.Fatal("test requires the connector cwd to differ from the Qwen session cwd")
	}
	t.Setenv("QWEN_HOME", home)
	report := liveSessionReport{UUID: "11111111-2222-4333-8444-555555555555", Name: "unaccepted", Product: "qwen"}
	if name, ok := qwenConnectorNativeName(report); ok || name != "" {
		t.Fatalf("missing product title projected as %q/%v", name, ok)
	}
	path := filepath.Join(home, "projects", "workspace", "chats", report.UUID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	encodedCwd, err := json.Marshal(cwd)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"sessionId":"11111111-2222-4333-8444-555555555555","cwd":` + string(encodedCwd) + `,"type":"system","subtype":"custom_title","systemPayload":{"customTitle":"native title","titleSource":"manual"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, ok := qwenConnectorNativeName(report); !ok || name != "native title" {
		t.Fatalf("product title projection = %q/%v", name, ok)
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
