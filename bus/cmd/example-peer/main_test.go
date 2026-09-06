package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/daemon"
	sdk "github.com/antst/agent-sessions/bus/sdk/go"
)

type runResult struct {
	result sdk.TurnResult
	err    error
}

func TestMain(m *testing.M) {
	if os.Getenv("AGENTBUS_LAUNCH_TOKEN") != "" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestHelloAndOpen(t *testing.T) {
	product := &example{}
	hello, err := product.Hello(context.Background())
	if err != nil || hello.Product != "example-peer" || len(hello.SupportedOpenFields) != 5 || len(hello.ExtraArguments) != 1 {
		t.Fatalf("hello = %#v, %v", hello, err)
	}
	open := sdk.OpenRequest{Name: "parent/child@local", Groups: []string{"parent", "child"}, Open: sdk.OpenOptions{
		Cwd: "/work", PermissionMode: "ask", Model: "model", ReasoningEffort: "high", Arguments: []string{"session_id=chosen"},
	}}
	result, err := product.Open(context.Background(), open)
	if err != nil || result.SessionID != "chosen" {
		t.Fatalf("chosen id = %#v, %v", result, err)
	}
	open.ResumeSessionID, open.Open.Arguments = "resume-id", nil
	result, err = product.Open(context.Background(), open)
	if err != nil || result.SessionID != "resume-id" {
		t.Fatalf("resume id = %#v, %v", result, err)
	}
	open.ResumeSessionID, open.Open.Arguments = "", []string{"bad"}
	if _, err = product.Open(context.Background(), open); err == nil {
		t.Fatal("invalid argument accepted")
	}
	open.Open.Arguments = nil
	if result, err = product.Open(context.Background(), open); err != nil || len(result.SessionID) != 32 {
		t.Fatalf("minted id = %#v, %v", result, err)
	}
}

func TestRunDeliveryInterruptAndCall(t *testing.T) {
	product := &example{}
	product.call = func(_ context.Context, method string, params, result any) error {
		if method != "session.list" || string(params.(json.RawMessage)) != `{}` {
			t.Fatalf("call = %q %#v", method, params)
		}
		*result.(*json.RawMessage) = json.RawMessage(`{"sessions":[]}`)
		return nil
	}
	for _, body := range []string{"first", "second"} {
		receipt, err := product.Deliver(context.Background(), sdk.DeliveryRequest{Body: body})
		if err != nil || receipt.Disposition != "queued_for_next_turn" {
			t.Fatalf("idle receipt = %#v, %v", receipt, err)
		}
	}
	result, err := product.Run(context.Background(), &sdk.Run{}, "plain")
	if err != nil || result.Outcome != "completed" || result.Result != "first\nsecond\nplain" {
		t.Fatalf("echo = %#v, %v", result, err)
	}
	result, err = product.Run(context.Background(), &sdk.Run{}, "call session.list {}")
	if err != nil || result.Result != `{"sessions":[]}` {
		t.Fatalf("call result = %#v, %v", result, err)
	}
	if _, err = product.Run(context.Background(), &sdk.Run{}, "fail scripted"); err == nil || err.Error() != "scripted" {
		t.Fatalf("failure = %v", err)
	}

	run := &sdk.Run{}
	terminal := make(chan runResult, 1)
	go func() {
		result, err := product.Run(context.Background(), run, "block")
		terminal <- runResult{result, err}
	}()
	waitActive(t, product)
	receipt, err := product.Deliver(context.Background(), sdk.DeliveryRequest{Body: "during"})
	if err != nil || receipt.Disposition != "injected" {
		t.Fatalf("active receipt = %#v, %v", receipt, err)
	}
	if err = product.Interrupt(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-terminal:
		result, err = got.result, got.err
	case <-time.After(time.Second):
		t.Fatal("block did not stop")
	}
	if err != nil || result.Outcome != "interrupted" || result.Result != "during" {
		t.Fatalf("interrupted result = %#v, %v", result, err)
	}
	if err = product.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledReferenceWorker(t *testing.T) {
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(executable, filepath.Join(directory, "example-peer")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	socket := filepath.Join(directory, "agentbus.sock")
	service, err := daemon.Start(daemon.Config{SocketPath: socket, TablePath: filepath.Join(directory, "sessions")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	t.Setenv("AGENTBUS_SOCKET", socket)
	peer, err := sdk.ConnectPeer(sdk.PeerIdentity{Product: "fixture", SessionID: "caller", Name: "caller", Groups: []string{"shared"}, Info: map[string]any{}}, func(context.Context, sdk.PeerIdentity, sdk.DeliveryRequest) (sdk.DeliveryReceipt, error) {
		return sdk.DeliveryReceipt{Disposition: "injected"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Shutdown()
	select {
	case <-peer.Ready():
	case <-time.After(time.Second):
		t.Fatal("caller did not connect")
	}
	spawned, err := peer.Caller.Spawn(context.Background(), sdk.LaneSpawnRequest{Name: "worker", Product: "example-peer", Open: &sdk.OpenOptions{Arguments: []string{"session_id=chosen"}}})
	if err != nil || spawned.SessionID != "chosen@local" {
		t.Fatalf("spawn = %#v, %v", spawned, err)
	}
	result, err := peer.Caller.Run(context.Background(), sdk.TurnRunRequest{SessionID: spawned.SessionID, Input: "call session.list {}"})
	if err != nil || result.Outcome != "completed" || !json.Valid([]byte(result.Result)) {
		t.Fatalf("worker call = %#v, %v", result, err)
	}
	if err = peer.Caller.Close(context.Background(), sdk.SessionCloseRequest{SessionID: spawned.SessionID, Forget: true}); err != nil {
		t.Fatal(err)
	}
}

func waitActive(t *testing.T, product *example) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		product.mu.Lock()
		active := product.active != nil
		product.mu.Unlock()
		if active {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("run did not become active")
}
