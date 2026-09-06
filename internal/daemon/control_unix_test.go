//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/pathidentity"
)

func TestBlockingControlCallDiesWithItsCaller(t *testing.T) {
	entered := make(chan struct{})
	allowAdmission := make(chan struct{})
	exited := make(chan struct{})
	server := startControlTestServer(t, 1, func(ctx context.Context, request ControlRequest) (json.RawMessage, error) {
		if request.Operation != "attachment.codex.pending" {
			t.Fatalf("operation = %q", request.Operation)
		}
		close(entered)
		<-allowAdmission
		if !AdmitControlCall(ctx) {
			t.Error("blocking control call had no admission channel")
		}
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	})
	result := make(chan struct {
		call *ControlCall
		err  error
	}, 1)
	go func() {
		call, err := BeginControlCall(context.Background(), server.Endpoint(), ControlRequest{
			ID: "pending", Role: RoleLauncher, Operation: "attachment.codex.pending", Generation: 1,
			IdempotencyKey: "pending", Payload: json.RawMessage(`{}`), WaitAdmission: true,
		})
		result <- struct {
			call *ControlCall
			err  error
		}{call: call, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pending call did not reach daemon")
	}
	select {
	case <-result:
		t.Fatal("blocking call returned before daemon admission")
	default:
	}
	close(allowAdmission)
	admitted := <-result
	if admitted.err != nil {
		t.Fatal(admitted.err)
	}
	call := admitted.call
	if err := call.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("caller EOF did not cancel pending daemon work")
	}
}

func TestAdmittedControlCallCarriesItsFinalResponseOnTheSameConnection(t *testing.T) {
	release := make(chan struct{})
	server := startControlTestServer(t, 1, func(ctx context.Context, _ ControlRequest) (json.RawMessage, error) {
		if !AdmitControlCall(ctx) {
			return nil, errors.New("admission channel is unavailable")
		}
		<-release
		return json.RawMessage(`{"selected":"native-id"}`), nil
	})
	call, err := BeginControlCall(context.Background(), server.Endpoint(), ControlRequest{
		ID: "pending-final", Role: RoleLauncher, Operation: "attachment.codex.pending", Generation: 1,
		IdempotencyKey: "pending-final", Payload: json.RawMessage(`{}`), WaitAdmission: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	response, err := call.Await()
	if err != nil || !response.OK || string(response.Payload) != `{"selected":"native-id"}` || response.Admitted {
		t.Fatalf("final response = %+v, %v", response, err)
	}
}

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
	first, err := StartControlServer(context.Background(), root, 3, "", func(context.Context, ControlRequest) (json.RawMessage, error) {
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
	if second, err := StartControlServer(context.Background(), root, 4, "", nil); err == nil {
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
	server, err := StartControlServer(context.Background(), root, 7, "", func(context.Context, ControlRequest) (json.RawMessage, error) {
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
