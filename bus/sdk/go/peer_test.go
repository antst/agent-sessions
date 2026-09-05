package sessionkit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/daemon"
	"github.com/antst/agent-sessions/bus/internal/protocol"
	sdk "github.com/antst/agent-sessions/bus/sdk/go"
)

func TestPeerRehelloAndReconnect(t *testing.T) {
	directory := t.TempDir()
	socket, table := filepath.Join(directory, "agentbus.sock"), filepath.Join(directory, "sessions")
	service := startDaemon(t, socket, table)
	first := startPeer(t, socket, "peer-one", "first", []string{"team"})

	identity := sdk.PeerIdentity{Product: "fixture-client", SessionID: "peer-one", Name: "renamed", Groups: []string{"team"}, Info: map[string]any{"revision": 2}}
	if err := first.Rehello(identity); err != nil {
		t.Fatal(err)
	}
	listed, err := first.Caller.List(context.Background(), sdk.SessionListRequest{})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].Name != "renamed@local" {
		t.Fatalf("same-id re-hello = %#v, %v", listed.Sessions, err)
	}

	identity.SessionID, identity.Name, identity.Groups = "peer-two", "second", []string{"other"}
	if err = first.Rehello(identity); err != nil {
		t.Fatal(err)
	}
	listed, err = first.Caller.List(context.Background(), sdk.SessionListRequest{})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != "peer-two@local" {
		t.Fatalf("different-id re-hello = %#v, %v", listed.Sessions, err)
	}

	if err = service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Caller.List(context.Background(), sdk.SessionListRequest{}); rpcCode(err) != protocol.NotConnected {
		t.Fatalf("disconnected call = %v", err)
	}
	service = startDaemon(t, socket, table)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		listed, err = first.Caller.List(context.Background(), sdk.SessionListRequest{})
		if err == nil && len(listed.Sessions) == 1 && listed.Sessions[0].SessionID == "peer-two@local" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != "peer-two@local" {
		t.Fatalf("peer did not reconnect with its identity: %#v, %v", listed.Sessions, err)
	}

	replacement := startPeer(t, socket, "peer-two", "replacement", []string{"other"})
	select {
	case <-first.Closed():
	case <-time.After(time.Second):
		t.Fatal("superseded peer did not become terminal")
	}
	time.Sleep(2*time.Second + 50*time.Millisecond)
	listed, err = replacement.Caller.List(context.Background(), sdk.SessionListRequest{})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].Name != "replacement@local" {
		t.Fatalf("superseded peer reconnected: %#v, %v", listed.Sessions, err)
	}
}

func TestPeerRejectsChangedGroups(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "agentbus.sock")
	startDaemon(t, socket, filepath.Join(directory, "sessions"))
	client, err := sdk.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	identity := sdk.PeerIdentity{Protocol: 1, Product: "fixture-client", SessionID: "peer", Name: "peer", Groups: []string{"one", "two"}, Info: map[string]any{}}
	if _, err = client.Call(context.Background(), "session.hello", identity); err != nil {
		t.Fatal(err)
	}
	identity.Groups = []string{"two", "one"}
	_, err = client.Call(context.Background(), "session.hello", identity)
	if rpcCode(err) != protocol.InvalidHello {
		t.Fatalf("changed groups = %v", err)
	}
}

func startDaemon(t *testing.T, socket, table string) *daemon.Daemon {
	t.Helper()
	service, err := daemon.Start(daemon.Config{SocketPath: socket, TablePath: table})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func startPeer(t *testing.T, socket, id, name string, groups []string) *sdk.Peer {
	t.Helper()
	if err := os.Setenv("AGENTBUS_SOCKET", socket); err != nil {
		t.Fatal(err)
	}
	peer, err := sdk.ConnectPeer(sdk.PeerIdentity{Product: "fixture-client", SessionID: id, Name: name, Groups: groups, Info: map[string]any{}}, func(context.Context, sdk.DeliveryRequest) (sdk.DeliveryReceipt, error) {
		return sdk.DeliveryReceipt{Disposition: "injected"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.Ready():
	case <-time.After(time.Second):
		t.Fatal("peer did not connect")
	}
	t.Cleanup(peer.Shutdown)
	return peer
}

func rpcCode(err error) int {
	var value *sdk.ProtocolError
	if errors.As(err, &value) {
		return value.Code
	}
	return 0
}
