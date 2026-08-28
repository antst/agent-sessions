package federation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHubProductionListenerAppliesResourcePreflightAndEveryMetadataOnlySink(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root+"/config")
	t.Setenv("XDG_STATE_HOME", root+"/state")
	paths, err := ResolveHubPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveHubRuntimeStatus(context.Background(), paths, HubRuntimeStatus{
		SchemaVersion: 1, RuntimeVersion: "prior", RuntimeIdentity: "sha256:" + strings.Repeat("1", 64),
		PID: 99999999, ProcStart: "provably-dead", Listener: "127.0.0.1:1",
		Service:         map[string]any{"manager": "systemd-user", "unit": "agent-sessions-hub.service"},
		ProtocolVersion: ProtocolVersion, Routing: map[string]any{"healthy": true}, Debt: []map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunHub(ctx, HubOptions{
			Listen: "127.0.0.1:0", RuntimeVersion: "acceptance", RuntimeIdentity: "sha256:" + strings.Repeat("2", 64),
			ServiceManager: "systemd-user", ServiceUnit: "agent-sessions-hub.service",
			ResourcePreflight: func(context.Context, HubAdmissionRequest) error {
				return fmt.Errorf("disk resource unavailable: %w", syscall.ENOSPC)
			},
			Stdout: &stdout, Stderr: &stderr,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop production hub: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("production hub did not stop")
		}
	})

	var status HubStatusProjection
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err = ReadHubStatus(context.Background())
		if err == nil && status.PID > 0 && status.RuntimeVersion == "acceptance" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || status.PID == 0 || status.RuntimeVersion != "acceptance" {
		t.Fatalf("read running production hub status: %+v, %v", status, err)
	}
	connection, err := net.DialTimeout("tcp", status.Listener, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	canary := "T074_PRIVATE_HOST_CONTENT_MUST_NOT_ESCAPE"
	hello, err := json.Marshal(hubWireFrame{
		Type: "hello", Version: ProtocolVersion, HostID: canary, HostName: canary,
		RuntimeVersion: canary, RuntimeIdentity: "sha256:" + strings.Repeat("3", 64), Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var refused hubWireFrame
	if err := json.Unmarshal(reply, &refused); err != nil || refused.Type != "error" ||
		!strings.Contains(refused.Error, "disk resource unavailable") {
		t.Fatalf("resource refusal = %s, %v", reply, err)
	}
	status, err = ReadHubStatus(context.Background())
	if err != nil || status.ConnectedHosts != 0 {
		t.Fatalf("pre-acceptance resource refusal changed durable host count: %+v, %v", status, err)
	}

	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, canary) {
		t.Fatalf("production hub output leaked private host content: %s", combined)
	}
	for _, kind := range []string{"normal", "debug", "error", "crash_report", "metric", "trace"} {
		if !strings.Contains(combined, `"kind":"`+kind+`"`) {
			t.Errorf("production hub output omitted %s sink: %s", kind, combined)
		}
	}
	for _, metadata := range []string{"disk_full", "free space in the hub state filesystem and retry"} {
		if !strings.Contains(stderr.String(), metadata) {
			t.Errorf("resource refusal omitted %q: %s", metadata, stderr.String())
		}
	}
}
