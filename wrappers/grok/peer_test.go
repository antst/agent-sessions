package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
	"golang.org/x/sys/unix"
)

func TestInteractivePlan(t *testing.T) {
	plan, err := InteractivePlan([]string{"--model", "-g", "--group", "team", "prompt"}, []string{"PATH=/bin"})
	must(t, err)
	id := environment(plan.Env, host.SessionIDEnv)
	check(t, len(id) == 36 && id[14] == '4', "session id = %q", id)
	check(t, environment(plan.Env, host.NameEnv) == id, "name differs from id")
	check(t, environment(plan.Env, host.GroupsEnv) == `["team"]`, "groups = %q", environment(plan.Env, host.GroupsEnv))
	check(t, slices.Contains(plan.Args, "--leader") && slices.Contains(plan.Args, "--session-id"), "managed args = %#v", plan.Args)
	model := slices.Index(plan.Args, "--model")
	check(t, model >= 0 && plan.Args[model+1] == "-g", "native option value was consumed as a group: %#v", plan.Args)

	plan, err = InteractivePlan([]string{"--resume", testSessionID}, nil)
	must(t, err)
	check(t, environment(plan.Env, host.SessionIDEnv) == testSessionID && !slices.Contains(plan.Args, "--session-id"), "resume = %#v / %#v", plan.Env, plan.Args)
	_, err = InteractivePlan([]string{"--no-leader"}, nil)
	check(t, err != nil && err.Error() == "grok-peer requires the default leader", "no-leader error = %v", err)
	for _, arguments := range [][]string{{"--resume"}, {"--continue"}, {"--fork-session"}, {"--resume", testSessionID, "-r" + testSessionID}} {
		_, err = InteractivePlan(arguments, nil)
		check(t, err != nil, "unsupported managed surface accepted: %#v", arguments)
	}
	plan, err = InteractivePlan([]string{"sessions", "list"}, []string{"PATH=/bin"})
	must(t, err)
	check(t, slices.Equal(plan.Args, []string{"sessions", "list"}) && environment(plan.Env, host.SessionIDEnv) == "", "native command was wrapped: %#v", plan)
	plan, err = InteractivePlan([]string{"--", "prompt"}, nil)
	must(t, err)
	separator := slices.Index(plan.Args, "--")
	check(t, separator > 0 && slices.Index(plan.Args, "--leader") < separator, "leader follows separator: %#v", plan.Args)
	plan, err = InteractivePlan([]string{"--model", "--leader", "prompt"}, nil)
	must(t, err)
	model = slices.Index(plan.Args, "--model")
	check(t, model >= 0 && plan.Args[model+1] == "--leader" && count(plan.Args, "--leader") == 2, "leader-valued option changed: %#v", plan.Args)
	plan, err = InteractivePlan([]string{"--", "--leader"}, nil)
	must(t, err)
	separator = slices.Index(plan.Args, "--")
	check(t, separator > 0 && plan.Args[separator-1] == "--leader" && plan.Args[separator+1] == "--leader", "post-separator literal changed: %#v", plan.Args)
}

func TestPeerMCPUsesRosterTitleAndDefaultLeader(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "agentbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.SessionIDEnv, testSessionID)
	t.Setenv(host.NameEnv, testSessionID)
	t.Setenv(host.GroupsEnv, `["peer-group"]`)
	t.Setenv("GROK_TEST_TITLES", "product-title")
	t.Setenv("GROK_TEST_ROSTER_DELAY", "1")
	opened := make(chan struct {
		backend *PeerBackend
		err     error
	}, 1)
	go func() {
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID, Groups: []string{"peer-group"}})
		opened <- struct {
			backend *PeerBackend
			err     error
		}{backend, err}
	}()
	first := <-hellos
	select {
	case result := <-opened:
		t.Fatalf("backend passed readiness before hello ack: %v", result.err)
	default:
	}
	first.ack <- true
	result := <-opened
	must(t, result.err)
	backend := result.backend
	check(t, first.SessionID == testSessionID && first.Name == testSessionID && slices.Equal(first.Groups, []string{"peer-group"}), "first hello = %#v", first)
	prepared := make(chan error, 1)
	go func() { prepared <- backend.Prepare(context.Background(), nil) }()
	second := <-hellos
	second.ack <- true
	must(t, <-prepared)
	check(t, second.SessionID == testSessionID && second.Name == "product-title" && slices.Equal(second.Groups, first.Groups), "re-hello = %#v", second)
	receipt, err := backend.deliver(context.Background(), backend.identity, delivery("peer message"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "peer delivery = %#v", receipt)
	backend.Shutdown()
}

func TestInteractiveLauncherOwnsPeerAndLocalAction(t *testing.T) {
	root := shortRoot(t)
	recordPath := filepath.Join(root, "record")
	socket := filepath.Join(root, "agentbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_RECORD", recordPath)
	started := filepath.Join(root, "interactive-started")
	t.Setenv("GROK_TEST_INTERACTIVE_STARTED", started)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID}, os.Environ())
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunInteractive(ctx, plan) }()
	hello := <-hellos
	check(t, hello.SessionID == testSessionID, "hello = %#v", hello)
	hello.ack <- true
	path := filepath.Join(root, "lanes", host.LaunchTokenDigest(testSessionID)+".sock")
	t.Setenv(mcp.LaneSocketEnv, path)
	client, err := mcp.NewLaneBackend()
	must(t, err)
	result, err := client.Action(context.Background(), "interrupt", json.RawMessage(`{"session_id":"lane@local"}`))
	must(t, err)
	check(t, string(result) == `{}`, "local action = %s", result)
	<-fileReady(started)
	cancel()
	check(t, <-done != nil, "signalled Grok child exited successfully")
	check(t, !exists(path), "peer endpoint remains")
	frames := records(t, recordPath)
	check(t, containsStart(frames, "--leader", testSessionID), "managed child absent")
	check(t, containsStart(frames, path), "child did not inherit local endpoint")
}

func TestInteractiveLauncherReturnsProductExit(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(host.SocketEnv, filepath.Join(root, "agentbus.sock"))
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_INTERACTIVE_EXIT", "7")
	plan, err := InteractivePlan([]string{"--session-id", testSessionID}, os.Environ())
	must(t, err)
	err = RunInteractive(context.Background(), plan)
	var exited *exec.ExitError
	check(t, errors.As(err, &exited) && exited.ExitCode() == 7, "exit = %v", err)
}

func TestPrepareSerializesAndCommitsAcknowledgedTitles(t *testing.T) {
	root := t.TempDir()
	server, hellos := fakeDaemon(t, filepath.Join(root, "agentbus.sock"))
	defer server.Close()
	t.Setenv(host.SocketEnv, filepath.Join(root, "agentbus.sock"))
	t.Setenv(host.SessionIDEnv, testSessionID)
	t.Setenv(host.NameEnv, testSessionID)
	t.Setenv("GROK_TEST_TITLES", "first-title,second-title")
	opened := make(chan struct {
		backend *PeerBackend
		err     error
	}, 1)
	go func() {
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID})
		opened <- struct {
			backend *PeerBackend
			err     error
		}{backend, err}
	}()
	select {
	case hello := <-hellos:
		hello.ack <- true
	case result := <-opened:
		must(t, result.err)
	}
	result := <-opened
	must(t, result.err)
	backend := result.backend
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- backend.Prepare(context.Background(), nil) }()
	firstHello := <-hellos
	go func() { second <- backend.Prepare(context.Background(), nil) }()
	select {
	case crossed := <-hellos:
		t.Fatalf("title observation crossed unacknowledged re-hello: %q", crossed.Name)
	default:
	}
	firstHello.ack <- true
	must(t, <-first)
	secondHello := <-hellos
	secondHello.ack <- false
	check(t, (<-second) != nil, "failed re-hello was accepted")
	backend.mu.Lock()
	name := backend.identity.Name
	backend.mu.Unlock()
	check(t, name == "first-title", "failed title was published: %q", name)
	backend.Shutdown()
}

func TestExactRosterAuthority(t *testing.T) {
	yolo, resident := true, true
	live := peerSession{SessionID: testSessionID, Resident: &resident, Activity: "idle", Yolo: &yolo}
	for _, test := range []struct {
		name     string
		sessions []peerSession
		noLeader bool
	}{
		{"absent", nil, true},
		{"duplicate", []peerSession{live, live}, false},
		{"missing resident", []peerSession{{SessionID: testSessionID, Activity: "idle", Yolo: &yolo}}, false},
		{"not resident", []peerSession{{SessionID: testSessionID, Resident: new(bool), Activity: "idle", Yolo: &yolo}}, false},
		{"missing yolo", []peerSession{{SessionID: testSessionID, Resident: &resident, Activity: "idle"}}, false},
		{"unknown activity", []peerSession{{SessionID: testSessionID, Resident: &resident, Activity: "starting", Yolo: &yolo}}, false},
	} {
		_, err := exactRoster(test.sessions, testSessionID)
		check(t, err != nil && errors.Is(err, errNoLeader) == test.noLeader, "%s = %v", test.name, err)
	}
	_, err := exactRoster([]peerSession{live}, testSessionID)
	must(t, err)
}

func TestPeerShutdownKillsItsProcessGroup(t *testing.T) {
	root := t.TempDir()
	server, hellos := fakeDaemon(t, filepath.Join(root, "agentbus.sock"))
	defer server.Close()
	t.Setenv(host.SocketEnv, filepath.Join(root, "agentbus.sock"))
	t.Setenv(host.SessionIDEnv, testSessionID)
	t.Setenv("GROK_TEST_DESCENDANT_PID", filepath.Join(root, "descendant.pid"))
	opened := make(chan struct {
		backend *PeerBackend
		err     error
	}, 1)
	go func() {
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID})
		opened <- struct {
			backend *PeerBackend
			err     error
		}{backend, err}
	}()
	select {
	case hello := <-hellos:
		hello.ack <- true
	case result := <-opened:
		must(t, result.err)
	}
	result := <-opened
	must(t, result.err)
	backend := result.backend
	body, err := os.ReadFile(filepath.Join(root, "descendant.pid"))
	must(t, err)
	var pid int
	_, err = fmt.Sscan(string(body), &pid)
	must(t, err)
	pidfd, err := unix.PidfdOpen(pid, 0)
	must(t, err)
	defer unix.Close(pidfd)
	backend.Shutdown()
	deadline := time.Now().Add(3 * time.Second)
	for processRunning(t, pidfd) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	check(t, !processRunning(t, pidfd), "peer descendant %d survived group shutdown", pid)
}

type hello struct {
	SessionID string   `json:"session_id"`
	Name      string   `json:"name"`
	Groups    []string `json:"groups"`
	ack       chan bool
}

func fakeDaemon(t *testing.T, path string) (net.Listener, <-chan hello) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	must(t, err)
	hellos := make(chan hello, 4)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		var write sync.Mutex
		for scanner.Scan() {
			var frame struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &frame) != nil {
				return
			}
			if frame.Method == "session.hello" {
				var value hello
				_ = json.Unmarshal(frame.Params, &value)
				value.ack = make(chan bool, 1)
				hellos <- value
				go func(id int64, value hello) {
					accepted := <-value.ack
					write.Lock()
					if !accepted {
						_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "invalid_hello"}})
					} else {
						_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
					}
					write.Unlock()
				}(frame.ID, value)
				continue
			}
			write.Lock()
			_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
			write.Unlock()
		}
	}()
	return listener, hellos
}

func environment(values []string, name string) string {
	for _, value := range values {
		if key, body, found := strings.Cut(value, "="); found && key == name {
			return body
		}
	}
	return ""
}

func count(values []string, target string) int {
	total := 0
	for _, value := range values {
		if value == target {
			total++
		}
	}
	return total
}

func processRunning(t *testing.T, pidfd int) bool {
	t.Helper()
	poll := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	count, err := unix.Poll(poll, 0)
	must(t, err)
	return count == 0
}
