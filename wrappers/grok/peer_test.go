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
	"syscall"
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
	for _, arguments := range [][]string{{"--single", "prompt"}, {"-pprompt"}, {"--prompt-file", "prompt.txt"}, {"--prompt-json", `[]`}, {"--output-format", "json"}, {"--json-schema", `{}`}, {"--max-turns", "1"}, {"--include-partial-messages"}} {
		_, err = InteractivePlan(arguments, nil)
		check(t, err != nil && strings.Contains(err.Error(), "Agentbus Grok lane"), "headless surface accepted: %#v / %v", arguments, err)
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
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID, Groups: []string{"peer-group"}}, filepath.Join(root, "leader.sock"), root, nil)
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
	leaderPID := filepath.Join(root, "leader.pid")
	t.Setenv("GROK_TEST_INTERACTIVE_STARTED", started)
	t.Setenv("GROK_TEST_LEADER_PID", leaderPID)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID}, os.Environ())
	must(t, err)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunInteractive(ctx, plan) }()
	hello := <-hellos
	check(t, hello.SessionID == testSessionID, "hello = %#v", hello)
	hello.ack <- true
	leaderPidfd := interactivePidfd(t, leaderPID)
	defer unix.Close(leaderPidfd)
	path := filepath.Join(root, "lanes", host.LaunchTokenDigest(testSessionID)+".sock")
	t.Setenv(mcp.LaneSocketEnv, path)
	client, err := mcp.NewLaneBackend()
	must(t, err)
	result, err := client.Action(context.Background(), "interrupt", json.RawMessage(`{"session_id":"lane@local"}`))
	must(t, err)
	check(t, string(result) == `{}`, "local action = %s", result)
	<-fileReady(started)
	cancel(testSignal{syscall.SIGINT})
	var exited *exec.ExitError
	err = <-done
	check(t, errors.As(err, &exited) && exited.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGINT, "signalled Grok child = %v", err)
	check(t, !processRunning(t, leaderPidfd), "private leader survived interactive shutdown")
	check(t, !exists(path), "peer endpoint remains")
	frames := records(t, recordPath)
	leaderPath := filepath.Join(root, "grok-"+host.LaunchTokenDigest(testSessionID)+".sock")
	check(t, containsStart(frames, "--leader", "--session-id", testSessionID, leaderPath), "managed child did not use the private leader")
	check(t, containsStart(frames, "agent", "leader", "--relay-on-demand", leaderPath, path), "private leader or local action endpoint absent")
	check(t, containsStart(frames, "agent", "stdio", leaderPath), "private observer absent")
	check(t, containsStart(frames, path), "child did not inherit local endpoint")
}

func TestRejectedHelloStopsInteractiveOwner(t *testing.T) {
	root := shortRoot(t)
	socket := filepath.Join(root, "agentbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	pidPath := filepath.Join(root, "interactive.pid")
	t.Setenv(host.SocketEnv, socket)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_INTERACTIVE_PID", pidPath)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID}, os.Environ())
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- RunInteractive(context.Background(), plan) }()
	hello := <-hellos
	pidfd := interactivePidfd(t, pidPath)
	hello.ack <- false
	err = <-done
	check(t, strings.Contains(err.Error(), "invalid_hello"), "launcher error = %v", err)
	check(t, !processRunning(t, pidfd), "TUI survived rejected hello")
	check(t, !exists(filepath.Join(root, "lanes", host.LaunchTokenDigest(testSessionID)+".sock")), "peer endpoint remains")
	unix.Close(pidfd)
}

func TestTerminalPeerStopsInteractiveOwner(t *testing.T) {
	root := shortRoot(t)
	socket := filepath.Join(root, "agentbus.sock")
	terminal := make(chan struct{})
	server, hellos := fakeDaemonTerminal(t, socket, terminal)
	defer server.Close()
	pidPath := filepath.Join(root, "interactive.pid")
	t.Setenv(host.SocketEnv, socket)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_INTERACTIVE_PID", pidPath)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID}, os.Environ())
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- RunInteractive(context.Background(), plan) }()
	hello := <-hellos
	hello.ack <- true
	pidfd := interactivePidfd(t, pidPath)
	path := filepath.Join(root, "lanes", host.LaunchTokenDigest(testSessionID)+".sock")
	t.Setenv(mcp.LaneSocketEnv, path)
	client, err := mcp.NewLaneBackend()
	must(t, err)
	_, err = client.Action(context.Background(), "interrupt", json.RawMessage(`{"session_id":"lane@local"}`))
	must(t, err)
	close(terminal)
	err = <-done
	check(t, strings.Contains(err.Error(), "superseded"), "launcher error = %v", err)
	check(t, !processRunning(t, pidfd), "TUI survived terminal peer")
	check(t, !exists(path), "peer endpoint remains")
	unix.Close(pidfd)
}

func TestObserverExitStopsInteractiveOwner(t *testing.T) {
	root := shortRoot(t)
	socket := filepath.Join(root, "agentbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	pidPath := filepath.Join(root, "interactive.pid")
	t.Setenv(host.SocketEnv, socket)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_INTERACTIVE_PID", pidPath)
	t.Setenv("GROK_TEST_OBSERVER_EXIT", "1")
	plan, err := InteractivePlan([]string{"--session-id", testSessionID}, os.Environ())
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- RunInteractive(context.Background(), plan) }()
	hello := <-hellos
	hello.ack <- true
	pidfd := interactivePidfd(t, pidPath)
	err = <-done
	check(t, strings.Contains(err.Error(), "Grok observer closed"), "launcher error = %v", err)
	check(t, !processRunning(t, pidfd), "TUI survived observer exit")
	check(t, !exists(filepath.Join(root, "lanes", host.LaunchTokenDigest(testSessionID)+".sock")), "peer endpoint remains")
	unix.Close(pidfd)
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
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID}, filepath.Join(root, "leader.sock"), root, nil)
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

func TestRosterIdentityChangeReplacesPeerAndSettlesOldRun(t *testing.T) {
	root := t.TempDir()
	server, hellos := fakeDaemon(t, filepath.Join(root, "agentbus.sock"))
	defer server.Close()
	t.Setenv(host.SocketEnv, filepath.Join(root, "agentbus.sock"))
	t.Setenv(host.SessionIDEnv, testSessionID)
	t.Setenv("GROK_TEST_SESSION_IDS", testSessionID+",new-session")
	change := filepath.Join(root, "roster-change")
	t.Setenv("GROK_TEST_ROSTER_CHANGE", change)
	t.Setenv("GROK_TEST_HOLD_RUN", "1")
	opened := make(chan struct {
		backend *PeerBackend
		err     error
	}, 1)
	go func() {
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID, Groups: []string{"peer-group"}}, filepath.Join(root, "leader.sock"), root, nil)
		opened <- struct {
			backend *PeerBackend
			err     error
		}{backend, err}
	}()
	first := <-hellos
	first.ack <- true
	result := <-opened
	must(t, result.err)
	backend := result.backend
	caller := backend.Caller()
	turn, err := caller.Start(sessionkit.TurnRunRequest{SessionID: "lane@local", Input: "hold"})
	must(t, err)
	must(t, os.WriteFile(change, []byte("change"), 0o600))
	replacement := <-hellos
	check(t, replacement.SessionID == "new-session", "replacement = %#v", replacement)
	state, err := caller.Status(sessionkit.StatusRequest{TurnID: turn.TurnID})
	must(t, err)
	check(t, state.State == "unavailable", "old run before replacement ack = %#v", state)
	replacement.ack <- true
	must(t, backend.Prepare(context.Background(), nil))
	backend.mu.Lock()
	identity := backend.identity
	backend.mu.Unlock()
	check(t, identity.SessionID == "new-session", "current identity = %#v", identity)
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
		backend, err := newPeerBackend(context.Background(), sessionkit.PeerIdentity{Product: "grok", SessionID: testSessionID, Name: testSessionID}, filepath.Join(root, "leader.sock"), root, nil)
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

type testSignal struct{ os.Signal }

func (s testSignal) Error() string           { return s.String() }
func (s testSignal) CaughtSignal() os.Signal { return s.Signal }

func fakeDaemon(t *testing.T, path string) (net.Listener, <-chan hello) {
	return fakeDaemonTerminal(t, path, nil)
}

func fakeDaemonTerminal(t *testing.T, path string, terminal <-chan struct{}) (net.Listener, <-chan hello) {
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
					if accepted && terminal != nil {
						<-terminal
						write.Lock()
						_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "session.superseded", "params": map[string]any{}})
						write.Unlock()
					}
				}(frame.ID, value)
				continue
			}
			if frame.Method == "" {
				continue
			}
			if frame.Method == "turn.run" && os.Getenv("GROK_TEST_HOLD_RUN") != "" {
				continue
			}
			write.Lock()
			_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
			write.Unlock()
		}
	}()
	return listener, hellos
}

func interactivePidfd(t *testing.T, path string) int {
	t.Helper()
	<-fileReady(path)
	var pid int
	body, err := os.ReadFile(path)
	must(t, err)
	_, err = fmt.Sscan(string(body), &pid)
	must(t, err)
	pidfd, err := unix.PidfdOpen(pid, 0)
	must(t, err)
	return pidfd
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
