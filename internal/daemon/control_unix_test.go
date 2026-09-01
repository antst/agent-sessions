//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/pathidentity"
)

func TestControlEndpointIsFixedPrivateAndIndependentOfTMPDIR(t *testing.T) {
	root := shortDaemonTestRoot(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "first"))
	first, err := ControlEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "second", "very", "long"))
	second, err := ControlEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pathidentity.FuturePath(filepath.Join(root, "run", "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if first != want || second != want {
		t.Fatalf("control endpoints = %q / %q, want %q", first, second, want)
	}
	server := startControlTestServer(t, 1, func(context.Context, ControlRequest) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	info, err := os.Lstat(server.Endpoint())
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("control endpoint metadata = %+v, %v", info, err)
	}
	rootInfo, err := os.Stat(filepath.Dir(server.Endpoint()))
	if err != nil || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("control runtime root metadata = %+v, %v", rootInfo, err)
	}
}

func TestControlRejectsDuplicateDaemonWithoutTouchingLiveSocket(t *testing.T) {
	root := shortDaemonTestRoot(t)
	first, err := StartControlServer(context.Background(), root, 3, func(context.Context, ControlRequest) (json.RawMessage, error) {
		return json.RawMessage(`{"live":true}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	before, err := os.Lstat(first.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	if second, err := StartControlServer(context.Background(), root, 4, nil); err == nil {
		_ = second.Close()
		t.Fatal("second daemon acquired the live endpoint")
	}
	after, err := os.Lstat(first.Endpoint())
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("duplicate start touched live socket: before=%+v after=%+v err=%v", before, after, err)
	}
	response := callControlTest(t, first.Endpoint(), ControlRequest{ID: "still-live", Role: RoleAdmin, Operation: "status", Generation: 3})
	if !response.OK {
		t.Fatalf("first daemon stopped serving after duplicate attempt: %#v", response)
	}
}

func TestControlRecoversExactStaleSocketAfterAbruptExit(t *testing.T) {
	root := shortDaemonTestRoot(t)
	endpoint, err := ControlEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(endpoint); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale endpoint fixture = %+v, %v", info, err)
	}
	server, err := StartControlServer(context.Background(), root, 7, func(context.Context, ControlRequest) (json.RawMessage, error) {
		return json.RawMessage(`{"recovered":true}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	response := callControlTest(t, server.Endpoint(), ControlRequest{ID: "recovered", Role: RoleAdmin, Operation: "status", Generation: 7})
	if !response.OK || !strings.Contains(string(response.Payload), `"recovered":true`) {
		t.Fatalf("recovered control response = %#v", response)
	}
}
