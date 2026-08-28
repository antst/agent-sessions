package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestAcceptanceResourcePreflightCoversExactInstalledFailureInventory(t *testing.T) {
	tests := []struct {
		resource string
		cause    error
	}{
		{resource: "disk", cause: syscall.ENOSPC},
		{resource: "memory", cause: syscall.ENOMEM},
		{resource: "file_descriptor", cause: syscall.EMFILE},
		{resource: "process", cause: syscall.EAGAIN},
	}
	for _, test := range tests {
		t.Run(test.resource, func(t *testing.T) {
			preflight := acceptanceHubResourcePreflight(test.resource)
			if preflight == nil {
				t.Fatal("acceptance resource preflight is nil")
			}
			err := preflight(context.Background(), federation.HubAdmissionRequest{
				Operation: "host.register", HostID: "host-a", RequestID: "request-a",
			})
			if !errors.Is(err, test.cause) {
				t.Fatalf("%s preflight = %v", test.resource, err)
			}
		})
	}
	if acceptanceHubResourcePreflight("") != nil {
		t.Fatal("release build unexpectedly enables acceptance resource injection")
	}
}

func TestHubHelpAndInvalidArgumentsUseDescriptorContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hub help exit = %d: %s", code, stderr.String())
	}
	for _, token := range []string{"usage: agent-sessions-hub", "--listen"} {
		if !strings.Contains(stdout.String(), token) {
			t.Errorf("hub help omitted %q: %s", token, stdout.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--listen"}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing listen exit = %d, want parser exit 2: %s", code, stderr.String())
	}
	if code := run([]string{"agent"}, &stdout, &stderr); code != 2 {
		t.Fatalf("obsolete host-agent mode exit = %d, want 2", code)
	}
}

func TestHubServePublishesIndependentStatusAndSupportsProtocolProbe(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runContext(ctx, []string{"--listen", "127.0.0.1:0"}, &stdout, &stderr) }()

	var status federation.HubStatusProjection
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		status, err = federation.ReadHubStatus(context.Background())
		if err == nil && status.Listener != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Listener == "" || status.ProtocolVersion != federation.ProtocolVersion || status.RuntimeIdentity == "" {
		t.Fatalf("hub readiness status = %+v; stderr=%s", status, stderr.String())
	}

	connection, err := net.DialTimeout("tcp", status.Listener, time.Second)
	if err != nil {
		t.Fatalf("dial hub probe: %v", err)
	}
	_, _ = connection.Write([]byte(`{"type":"probe","version":3}` + "\n"))
	var probe map[string]any
	if err := json.NewDecoder(connection).Decode(&probe); err != nil {
		t.Fatalf("decode probe result: %v", err)
	}
	_ = connection.Close()
	if probe["type"] != "probe_ok" || int(probe["version"].(float64)) != federation.ProtocolVersion {
		t.Fatalf("hub probe result = %#v", probe)
	}

	var statusOut, statusErr bytes.Buffer
	if code := runContext(context.Background(), []string{"status", "--json"}, &statusOut, &statusErr); code != 0 {
		t.Fatalf("hub status exit = %d: %s", code, statusErr.String())
	}
	var envelope diagnostics.Envelope
	if err := json.Unmarshal(statusOut.Bytes(), &envelope); err != nil {
		t.Fatalf("decode hub status envelope: %v: %s", err, statusOut.String())
	}
	if envelope.Event != "hub.status" || envelope.Metadata["listener"] != status.Listener {
		t.Fatalf("hub status envelope = %+v", envelope)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("hub serve exit = %d: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hub serve did not stop with its owning context")
	}
}

func TestHubStatusUnavailableDoesNotCreateStateOrStartService(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--json"}, &stdout, &stderr); code != 3 {
		t.Fatalf("unavailable hub status exit = %d, want 3: %s", code, stderr.String())
	}
	paths, err := federation.ResolveHubPaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.StateRoot); !os.IsNotExist(err) {
		t.Fatalf("read-only hub status created state: %v", err)
	}
}
