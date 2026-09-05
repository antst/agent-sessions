package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

func TestMain(m *testing.M) {
	name := filepath.Base(os.Args[0])
	if strings.HasPrefix(name, "exit-worker") {
		fmt.Fprintln(os.Stderr, "fixture failed before hello")
		os.Exit(7)
	}
	if strings.HasPrefix(name, "fixture-worker") {
		worker := sessionkit.NewWorker(&fixtureProduct{product: name})
		_ = worker.Serve(context.Background())
		os.Exit(0)
	}
	if strings.HasPrefix(name, "ordered-worker") {
		if err := runOrderedWorker(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(8)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runOrderedWorker(product string) error {
	fd, err := net.Dial("unix", os.Getenv("AGENTBUS_SOCKET"))
	if err != nil {
		return err
	}
	var wire *rpc.Conn
	proof := make(chan error, 1)
	wire = rpc.New(fd, true, func(_ context.Context, request *rpc.Request) {
		if request.Method != "session.open" {
			return
		}
		go func() {
			if err := wire.Result(request, protocol.OpenResult{SessionID: "ordered-native"}); err != nil {
				proof <- err
				return
			}
			var listed protocol.SessionListResult
			if err := wire.Call(context.Background(), "session.list", protocol.SessionListRequest{SessionID: "ordered-native"}, &listed); err != nil || len(listed.Sessions) != 1 {
				proof <- fmt.Errorf("post-open list before commit: %v (%d rows)", err, len(listed.Sessions))
				return
			}
			var sent protocol.MessageSendResult
			proof <- wire.Call(context.Background(), "message.send", protocol.MessageSendRequest{Target: "parent", Message: "after-commit"}, &sent)
		}()
	})
	hello := protocol.WorkerHello{Protocol: 1, LaunchToken: os.Getenv("AGENTBUS_LAUNCH_TOKEN"), HelloDescription: protocol.HelloDescription{Product: product, SupportedOpenFields: []string{}, ExtraArguments: []protocol.ExtraArgument{}}}
	if err := wire.Call(context.Background(), "session.hello", hello, &struct{}{}); err != nil {
		return err
	}
	err = <-proof
	_ = wire.Close()
	return err
}

type fixtureProduct struct {
	product string
	mu      sync.Mutex
	stop    chan struct{}
}

func (p *fixtureProduct) Hello(context.Context) (sessionkit.HelloDescription, error) {
	return sessionkit.HelloDescription{Product: p.product, Version: "test", SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"}, ExtraArguments: []sessionkit.ExtraArgument{}}, nil
}
func (*fixtureProduct) Open(_ context.Context, request sessionkit.OpenRequest) (sessionkit.OpenResult, error) {
	if request.Name == "" || len(request.Groups) < 2 {
		return sessionkit.OpenResult{}, errors.New("identity missing")
	}
	if request.ResumeSessionID != "" {
		return sessionkit.OpenResult{SessionID: request.ResumeSessionID}, nil
	}
	return sessionkit.OpenResult{SessionID: fmt.Sprintf("native-%d", os.Getpid())}, nil
}

func (p *fixtureProduct) Run(_ context.Context, run *sessionkit.Run, input string) (sessionkit.TurnResult, error) {
	if input == "block" {
		p.mu.Lock()
		p.stop = make(chan struct{})
		stop := p.stop
		run.Native = stop
		if run.Interrupted() {
			close(stop)
			p.stop = nil
		}
		p.mu.Unlock()
		<-stop
		return sessionkit.TurnResult{Outcome: "interrupted", Result: "stopped"}, nil
	}
	return sessionkit.TurnResult{Outcome: "completed", Result: input}, nil
}
func (p *fixtureProduct) Interrupt(_ context.Context, _ *sessionkit.Run) error {
	p.mu.Lock()
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	p.mu.Unlock()
	return nil
}
func (*fixtureProduct) Deliver(_ context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	if request.MessageID == "" || request.From.SessionID == "" {
		return sessionkit.DeliveryReceipt{}, errors.New("delivery identity missing")
	}
	return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
}
func (*fixtureProduct) Close(context.Context) error { return nil }

type peerClient struct {
	wire       *rpc.Conn
	superseded chan struct{}
	deliveries chan protocol.DeliveryRequest
	mu         sync.Mutex
	identity   protocol.PeerHello
}

func connectPeer(t *testing.T, socket, id, name string, groups ...string) *peerClient {
	t.Helper()
	fd, err := net.Dial("unix", socket)
	must(t, err)
	peer := &peerClient{superseded: make(chan struct{}, 1), deliveries: make(chan protocol.DeliveryRequest, 8)}
	peer.wire = rpc.New(fd, true, func(_ context.Context, request *rpc.Request) {
		switch request.Method {
		case "session.superseded":
			peer.superseded <- struct{}{}
			_ = peer.wire.Result(request, struct{}{})
		case "message.deliver":
			peer.deliveries <- *request.Params.(*protocol.DeliveryRequest)
			_ = peer.wire.Result(request, protocol.DeliveryReceipt{Disposition: "injected"})
		}
	})
	peer.identity = protocol.PeerHello{Protocol: 1, Product: "fixture-client", SessionID: id, Name: name, Groups: groups, Info: map[string]any{"ready": true}}
	must(t, peer.wire.Call(context.Background(), "session.hello", peer.identity, &struct{}{}))
	t.Cleanup(func() { _ = peer.wire.Close() })
	return peer
}

func (p *peerClient) call(method string, params, result any) error {
	return p.wire.Call(context.Background(), method, params, result)
}

func TestDurableTableWritesSixColumnsAndLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "sessions.json")
	table, err := openTable(path)
	must(t, err)
	want := row{SessionID: "native@local", Product: "fixture-worker", Name: "parent/child@local", Groups: []string{"session:parent@local", "session:parent@local/child"}, Open: protocol.OpenOptions{Cwd: "/work"}, CreatedAt: time.Unix(10, 0).UTC()}
	must(t, table.insert(want))
	if err := table.insert(want); !errors.Is(err, errDuplicateRow) {
		t.Fatalf("duplicate insert = %v", err)
	}
	must(t, table.updateName(want.SessionID, "parent/renamed@local"))
	reloaded, err := openTable(path)
	must(t, err)
	rows := reloaded.list()
	if len(rows) != 1 || rows[0].Name != "parent/renamed@local" || rows[0].Open.Cwd != "/work" {
		t.Fatalf("loaded rows = %#v", rows)
	}
	must(t, reloaded.delete(want.SessionID))
	if len(reloaded.list()) != 0 {
		t.Fatal("deleted row remained")
	}
}

func TestPeerHelloRehelloReplacementAndSupersession(t *testing.T) {
	daemon, socket := startDaemon(t)
	first := connectPeer(t, socket, "peer-one", "first", "team")
	first.identity.Name, first.identity.Info = "renamed", map[string]any{"revision": float64(2)}
	must(t, first.call("session.hello", first.identity, &struct{}{}))
	var listed protocol.SessionListResult
	must(t, first.call("session.list", protocol.SessionListRequest{}, &listed))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Name != "renamed@local" {
		t.Fatalf("same-id re-hello = %#v", listed.Sessions)
	}

	first.identity.SessionID, first.identity.Name, first.identity.Groups = "peer-two", "second", []string{"other"}
	must(t, first.call("session.hello", first.identity, &struct{}{}))
	select {
	case <-first.superseded:
		t.Fatal("same-connection identity replacement sent superseded")
	default:
	}
	listed = protocol.SessionListResult{}
	must(t, first.call("session.list", protocol.SessionListRequest{}, &listed))
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != "peer-two@local" {
		t.Fatalf("different-id re-hello = %#v", listed.Sessions)
	}

	second := connectPeer(t, socket, "peer-two", "replacement", "other")
	select {
	case <-first.superseded:
	case <-time.After(time.Second):
		t.Fatal("displaced peer was not notified before close")
	}
	listed = protocol.SessionListResult{}
	must(t, second.call("session.list", protocol.SessionListRequest{}, &listed))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Name != "replacement@local" {
		t.Fatalf("replacement list = %#v", listed.Sessions)
	}

	bad := connectPeer(t, socket, "bad-groups", "bad", "one")
	bad.identity.Groups = []string{"two"}
	if code := rpcCode(bad.call("session.hello", bad.identity, &struct{}{})); code != protocol.InvalidHello {
		t.Fatalf("changed groups code = %d", code)
	}
	<-bad.wire.Done()
	_ = daemon
}

func TestWorkerHelloIsSingleUseAndUncommitted(t *testing.T) {
	d, socket := startDaemon(t)
	token := "one-use-token"
	d.mapsMutex.Lock()
	d.reservations[token] = &reservation{product: "fixture-worker", hello: make(chan launchResult, 1)}
	d.mapsMutex.Unlock()
	fd, err := net.Dial("unix", socket)
	must(t, err)
	worker := rpc.New(fd, true, nil)
	hello := protocol.WorkerHello{Protocol: 1, LaunchToken: token, HelloDescription: protocol.HelloDescription{Product: "fixture-worker", SupportedOpenFields: []string{}, ExtraArguments: []protocol.ExtraArgument{}}}
	must(t, worker.Call(context.Background(), "session.hello", hello, &struct{}{}))
	if code := rpcCode(worker.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{})); code != protocol.NotCommitted {
		t.Fatalf("uncommitted worker code = %d", code)
	}
	if code := rpcCode(worker.Call(context.Background(), "session.hello", hello, &struct{}{})); code != protocol.InvalidHello {
		t.Fatalf("worker re-hello code = %d", code)
	}
	<-worker.Done()
}

func TestSpawnRunCloseResumeForgetAndRestart(t *testing.T) {
	directory := t.TempDir()
	installFixture(t, directory, "fixture-worker")
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	tablePath, socket := filepath.Join(directory, "sessions.json"), filepath.Join(directory, "agentbus.sock")
	d, err := Start(Config{SocketPath: socket, TablePath: tablePath, Products: []string{"fixture-worker"}})
	must(t, err)
	parent := connectPeer(t, socket, "parent", "parent", "team")

	var description protocol.LaneDescribeResult
	must(t, parent.call("lane.describe", protocol.LaneDescribeRequest{Product: "fixture-worker"}, &description))
	if description.Product != "fixture-worker" || len(description.SupportedOpenFields) != 5 {
		t.Fatalf("description = %#v", description)
	}
	if code := rpcCode(parent.call("lane.describe", protocol.LaneDescribeRequest{Product: "fixture-worker", Host: "remote"}, &description)); code != protocol.UnknownHost {
		t.Fatalf("remote describe code = %d", code)
	}
	if code := rpcCode(parent.call("lane.describe", protocol.LaneDescribeRequest{Product: "missing-worker"}, &description)); code != protocol.UnknownProduct {
		t.Fatalf("missing product code = %d", code)
	}
	installFixture(t, directory, "exit-worker")
	if code := rpcCode(parent.call("lane.describe", protocol.LaneDescribeRequest{Product: "exit-worker"}, &description)); code != protocol.SpawnFailed {
		t.Fatalf("exiting product code = %d", code)
	}

	var spawned protocol.LaneSpawnResult
	open := protocol.OpenOptions{Cwd: "/work", Arguments: []string{"--flag"}}
	must(t, parent.call("lane.spawn", protocol.LaneSpawnRequest{Name: "child", Product: "fixture-worker", ExtraGroups: []string{"shared"}, Open: &open}, &spawned))
	if !strings.HasSuffix(spawned.SessionID, "@local") {
		t.Fatalf("spawn id = %q", spawned.SessionID)
	}
	var listed protocol.SessionListResult
	must(t, parent.call("session.list", protocol.SessionListRequest{SessionID: spawned.SessionID}, &listed))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Name != "parent/child@local" || !listed.Sessions[0].Connected {
		t.Fatalf("spawned summary = %#v", listed.Sessions)
	}
	wantGroups := []string{"session:parent@local", "session:parent@local/child", "shared"}
	if !slices.Equal(listed.Sessions[0].Groups, wantGroups) {
		t.Fatalf("groups = %#v, want %#v", listed.Sessions[0].Groups, wantGroups)
	}

	var turn protocol.TurnResult
	must(t, parent.call("turn.run", protocol.TurnRunRequest{SessionID: spawned.SessionID, Input: "echo"}, &turn))
	if turn.Outcome != "completed" || turn.Result != "echo" {
		t.Fatalf("turn = %#v", turn)
	}
	if code := rpcCode(parent.call("turn.interrupt", protocol.SessionTarget{SessionID: spawned.SessionID}, &struct{}{})); code != protocol.NotRunning {
		t.Fatalf("idle interrupt code = %d", code)
	}
	if code := rpcCode(parent.call("lane.spawn", protocol.LaneSpawnRequest{Name: "child", Product: "fixture-worker", Open: &protocol.OpenOptions{}}, &spawned)); code != protocol.NameTaken {
		t.Fatalf("duplicate name code = %d", code)
	}
	blocked := make(chan error, 1)
	go func() {
		blocked <- parent.call("turn.run", protocol.TurnRunRequest{SessionID: spawned.SessionID, Input: "block"}, &turn)
	}()
	waitRunning(t, parent, spawned.SessionID)
	if code := rpcCode(parent.call("turn.run", protocol.TurnRunRequest{SessionID: spawned.SessionID, Input: "second"}, &turn)); code != protocol.Busy {
		t.Fatalf("concurrent run code = %d", code)
	}
	must(t, parent.call("turn.interrupt", protocol.SessionTarget{SessionID: spawned.SessionID}, &struct{}{}))
	must(t, <-blocked)

	closed := make(chan error, 2)
	for range 2 {
		go func() {
			closed <- parent.call("session.close", protocol.SessionCloseRequest{SessionID: spawned.SessionID}, &struct{}{})
		}()
	}
	codes := []int{rpcCode(<-closed), rpcCode(<-closed)}
	if !(codes[0] == 0 && codes[1] == protocol.NotConnected || codes[1] == 0 && codes[0] == protocol.NotConnected) {
		t.Fatalf("concurrent close codes = %#v", codes)
	}
	listed = protocol.SessionListResult{}
	must(t, parent.call("session.list", protocol.SessionListRequest{SessionID: spawned.SessionID}, &listed))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Connected {
		t.Fatalf("closed summary = %#v", listed.Sessions)
	}
	must(t, parent.call("lane.spawn", protocol.LaneSpawnRequest{ResumeSessionID: spawned.SessionID}, &spawned))
	must(t, parent.call("session.close", protocol.SessionCloseRequest{SessionID: spawned.SessionID, Forget: true}, &struct{}{}))
	if code := rpcCode(parent.call("session.list", protocol.SessionListRequest{SessionID: spawned.SessionID}, &listed)); code != protocol.UnknownSession {
		t.Fatalf("forgotten list code = %d", code)
	}

	// Recreate one row, then prove restart loads it offline without a recovery spawn.
	must(t, parent.call("lane.spawn", protocol.LaneSpawnRequest{Name: "restart", Product: "fixture-worker", Open: &protocol.OpenOptions{}}, &spawned))
	must(t, parent.call("session.close", protocol.SessionCloseRequest{SessionID: spawned.SessionID}, &struct{}{}))
	must(t, d.Close())
	d, err = Start(Config{SocketPath: socket, TablePath: tablePath})
	must(t, err)
	t.Cleanup(func() { _ = d.Close() })
	d.mapsMutex.Lock()
	counts := []int{len(d.connections), len(d.pending), len(d.rowLocks), len(d.reservations)}
	d.mapsMutex.Unlock()
	if !slices.Equal(counts, []int{0, 0, 0, 0}) {
		t.Fatalf("restart live maps = %#v", counts)
	}
	parent = connectPeer(t, socket, "parent", "parent", "team")
	listed = protocol.SessionListResult{}
	must(t, parent.call("session.list", protocol.SessionListRequest{SessionID: spawned.SessionID}, &listed))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Connected || listed.Sessions[0].Running {
		t.Fatalf("restart summary = %#v", listed.Sessions)
	}
}

func TestVisibilityDeliveryAndLaneIdentityCollision(t *testing.T) {
	d, socket := startDaemon(t)
	first := connectPeer(t, socket, "first", "first", "shared")
	second := connectPeer(t, socket, "second", "second", "shared")
	hidden := connectPeer(t, socket, "hidden", "hidden", "private")

	var sent protocol.MessageSendResult
	must(t, first.call("message.send", protocol.MessageSendRequest{Target: "second", Message: "hello"}, &sent))
	if len(sent.Deliveries) != 1 || sent.Deliveries[0].Disposition != "injected" {
		t.Fatalf("delivery result = %#v", sent)
	}
	select {
	case delivered := <-second.deliveries:
		if delivered.Body != "hello" || delivered.From.SessionID != "first@local" {
			t.Fatalf("delivery = %#v", delivered)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery not received")
	}
	if code := rpcCode(first.call("message.send", protocol.MessageSendRequest{Target: "hidden", Message: "no"}, &sent)); code != protocol.UnknownSession {
		t.Fatalf("invisible target code = %d", code)
	}

	row := row{SessionID: "lane-id@local", Product: "fixture-worker", Name: "lane@local", Groups: []string{"shared", "session:lane"}, Open: protocol.OpenOptions{}, CreatedAt: time.Now()}
	must(t, d.table.insert(row))
	fd, err := net.Dial("unix", socket)
	must(t, err)
	collision := rpc.New(fd, true, nil)
	hello := protocol.PeerHello{Protocol: 1, Product: "fixture-client", SessionID: "lane-id", Name: "collision", Groups: []string{"shared"}, Info: map[string]any{}}
	if code := rpcCode(collision.Call(context.Background(), "session.hello", hello, &struct{}{})); code != protocol.InvalidHello {
		t.Fatalf("lane collision code = %d", code)
	}
	_ = collision.Close()
	_ = hidden
}

func TestOpenCommitPrecedesNextWorkerFrame(t *testing.T) {
	directory := t.TempDir()
	installFixture(t, directory, "ordered-worker")
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	d, socket := startDaemon(t)
	parent := connectPeer(t, socket, "parent", "parent", "team")
	var spawned protocol.LaneSpawnResult
	must(t, parent.call("lane.spawn", protocol.LaneSpawnRequest{Name: "ordered", Product: "ordered-worker", Open: &protocol.OpenOptions{}}, &spawned))
	select {
	case delivered := <-parent.deliveries:
		if delivered.Body != "after-commit" {
			t.Fatalf("delivery = %#v", delivered)
		}
	case <-time.After(time.Second):
		t.Fatal("worker's post-open request was not dispatched after commit")
	}
	_ = d
}

func startDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	directory := t.TempDir()
	socket := filepath.Join(directory, "agentbus.sock")
	d, err := Start(Config{SocketPath: socket, TablePath: filepath.Join(directory, "sessions.json")})
	must(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d, socket
}

func installFixture(t *testing.T, directory, name string) {
	t.Helper()
	executable, err := os.Executable()
	must(t, err)
	must(t, os.Symlink(executable, filepath.Join(directory, name)))
}

func rpcCode(err error) int {
	var rpcError *protocol.RPCError
	if errors.As(err, &rpcError) {
		return rpcError.Code
	}
	return 0
}

func waitRunning(t *testing.T, peer *peerClient, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var listed protocol.SessionListResult
		if peer.call("session.list", protocol.SessionListRequest{SessionID: id}, &listed) == nil && len(listed.Sessions) == 1 && listed.Sessions[0].Running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("turn never became running")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
