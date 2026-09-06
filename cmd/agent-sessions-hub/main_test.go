package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/federation"
)

func TestHubListenAddressUsesEnvironmentThenBoundedUserConfigThenDefault(t *testing.T) {
	home := t.TempDir()
	configRoot := filepath.Join(home, ".config", "agent-sessions")
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(configRoot, "hub.env")
	if err := os.WriteFile(config, []byte("# operator setting\nexport AGENT_SESSIONS_HUB_LISTEN='127.0.0.1:8420'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	got, err := hubListenAddress(getenv, home)
	if err != nil || got != "127.0.0.1:8420" {
		t.Fatalf("config listen = %q, %v", got, err)
	}
	got, err = hubListenAddress(func(name string) string {
		if name == "AGENT_SESSIONS_HUB_LISTEN" {
			return "127.0.0.1:9420"
		}
		return ""
	}, home)
	if err != nil || got != "127.0.0.1:9420" {
		t.Fatalf("environment listen = %q, %v", got, err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	got, err = hubListenAddress(getenv, home)
	if err != nil || got != ":7419" {
		t.Fatalf("default listen = %q, %v", got, err)
	}
	if err := os.WriteFile(config, []byte(strings.Repeat("x", maxHubConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = hubListenAddress(func(string) string { return "127.0.0.1:10420" }, home)
	if err != nil || got != "127.0.0.1:10420" {
		t.Fatalf("environment did not precede oversized config: %q, %v", got, err)
	}
	if _, err := hubListenAddress(getenv, home); err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("oversized config result = %v", err)
	}
}

func TestCommandHubSurvivesMalformedClientAndAcceptsProtocolProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	home := t.TempDir()
	go func() { done <- runWithContext(ctx, []string{"--listen", address}, home) }()
	waitForHub(t, address)

	hostile, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = hostile.Write([]byte("{malformed}\n"))
	_ = hostile.Close()

	probe, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"type": "probe", "version": federation.ProtocolVersion, "future": true}
	if err := json.NewEncoder(probe).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.NewDecoder(probe).Decode(&response); err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	if response["type"] != "probe_ok" || response["version"] != float64(federation.ProtocolVersion) {
		t.Fatalf("probe response = %#v", response)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command hub did not stop")
	}
}

func waitForHub(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hub command did not listen")
}
