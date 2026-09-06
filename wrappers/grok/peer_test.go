package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/wrappers/host"
	"github.com/antst/sessionbus/wrappers/mcp"
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
	for _, arguments := range [][]string{{"--always-approve"}, {"--yolo"}, {"--permission-mode", "always-approve"}, {"--permission-mode=always-approve"}} {
		check(t, interactivePermission(arguments) == "bypassPermissions", "%#v did not select bypass permission", arguments)
	}
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
		check(t, err != nil && strings.Contains(err.Error(), "Sessionbus Grok lane"), "headless surface accepted: %#v / %v", arguments, err)
	}
	plan, err = InteractivePlan([]string{"sessions", "list"}, []string{"PATH=/bin"})
	must(t, err)
	check(t, slices.Equal(plan.Args, []string{"sessions", "list"}) && environment(plan.Env, host.SessionIDEnv) == "", "native command was wrapped: %#v", plan)
	plan, err = InteractivePlan([]string{"--", "--leader"}, nil)
	must(t, err)
	separator := slices.Index(plan.Args, "--")
	check(t, separator > 0 && plan.Args[separator-1] == "--leader" && plan.Args[separator+1] == "--leader", "post-separator literal changed: %#v", plan.Args)
}

func TestProductHelperOwnsPeerAndLazilyObserves(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "sessionbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	recordPath := filepath.Join(root, "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_TITLES", "product title,"+testSessionID)
	blocked := filepath.Join(root, "interject-release")
	t.Setenv("GROK_TEST_INTERJECT_BLOCK", blocked)
	cwd, err := os.Getwd()
	must(t, err)
	t.Setenv("GROK_TEST_CWD", cwd)
	t.Setenv("GROK_TEST_ACTIVITY", "idle")
	environment := setEnvironment(setEnvironment(setEnvironment(os.Environ(), grokSessionIDEnv, testSessionID), grokLeaderSocketEnv, filepath.Join(root, "leader.sock")), host.GroupsEnv, `["peer-group"]`)
	opened := make(chan struct {
		backend *PeerBackend
		err     error
	}, 1)
	go func() {
		backend, err := NewPeerBackend(context.Background(), environment)
		opened <- struct {
			backend *PeerBackend
			err     error
		}{backend, err}
	}()
	first := <-hellos
	check(t, first.SessionID == testSessionID && first.Name == testSessionID && slices.Equal(first.Groups, []string{"peer-group"}), "first hello = %#v", first)
	check(t, len(records(t, recordPath)) == 0, "helper opened observer before delivery")
	first.ack <- true
	result := <-opened
	must(t, result.err)
	backend := result.backend
	<-backend.peer.Ready()
	check(t, backend.Caller() == backend.peer.Caller, "helper constructed a second Caller")
	must(t, backend.Prepare(context.Background(), nil))
	check(t, len(records(t, recordPath)) == 0, "tool preparation opened observer")

	deliveryCtx, cancel := context.WithCancel(context.Background())
	delivered := deliverPeer(backend, deliveryCtx, delivery("peer message"))
	rehello := <-hellos
	check(t, rehello.SessionID == testSessionID && rehello.Name == "product title", "title re-hello = %#v", rehello)
	cancel()
	check(t, errors.Is((<-delivered).err, context.Canceled), "cancelled delivery did not return its context error")
	publishTestFile(blocked, []byte("ready"))
	request := delivery("again")
	request.MessageID = "again"
	delivered = deliverPeer(backend, context.Background(), request)
	corrected := <-hellos
	check(t, corrected.Name == testSessionID, "corrective title re-hello = %#v", corrected)
	corrected.ack <- true
	answer := <-delivered
	check(t, answer.err == nil && answer.receipt.Disposition == "injected" && len(peerClientPIDs(t, records(t, recordPath))) == 1, "observer was not retained: %#v", answer)
	rehello.ack <- true
	converged := <-hellos
	check(t, converged.Name == testSessionID, "late-ack convergence hello = %#v", converged)
	converged.ack <- true
	<-converged.done
	listed, err := backend.Caller().List(context.Background(), sessionkit.SessionListRequest{})
	must(t, err)
	check(t, len(listed.Sessions) == 1 && listed.Sessions[0].Name == testSessionID+"@local", "final bus identity = %#v", listed.Sessions)
	backend.Shutdown()
}

func TestProductSessionIDsCreateDistinctPeers(t *testing.T) {
	for index, id := range []string{testSessionID, "01a07800-94fb-7b12-b531-2f0509e033f1"} {
		root := t.TempDir()
		socket := filepath.Join(root, "sessionbus.sock")
		server, hellos := fakeDaemon(t, socket)
		t.Setenv(host.SocketEnv, socket)
		environment := setEnvironment(setEnvironment(os.Environ(), grokSessionIDEnv, id), grokLeaderSocketEnv, filepath.Join(root, "leader.sock"))
		opened := make(chan *PeerBackend, 1)
		go func() { backend, _ := NewPeerBackend(context.Background(), environment); opened <- backend }()
		hello := <-hellos
		check(t, hello.SessionID == id && hello.Name == id, "helper %d hello = %#v", index+1, hello)
		hello.ack <- true
		backend := <-opened
		check(t, backend != nil && backend.identity.SessionID == id, "helper %d identity changed", index+1)
		backend.Shutdown()
		server.Close()
	}
}

func TestPeerMCPServesWhileBusAdmissionIsHeld(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "sessionbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	environment := setEnvironment(setEnvironment(os.Environ(), grokSessionIDEnv, testSessionID), grokLeaderSocketEnv, filepath.Join(root, "leader.sock"))
	backend, err := NewPeerBackend(context.Background(), environment)
	must(t, err)
	input, writeInput := io.Pipe()
	readOutput, output := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- (&mcp.Server{Backend: backend}).Serve(context.Background(), input, output)
		_ = output.Close()
	}()
	encoder := json.NewEncoder(writeInput)
	scanner := bufio.NewScanner(readOutput)
	check(t, mcpResponse(t, encoder, scanner, 1, "initialize", map[string]any{})["result"] != nil, "MCP initialize failed")
	check(t, mcpResponse(t, encoder, scanner, 2, "tools/list", map[string]any{})["result"] != nil, "MCP tools/list failed")
	hello := <-hellos
	hello.ack <- false
	<-backend.peer.Closed()
	terminal := mcpResponse(t, encoder, scanner, 3, "tools/call", map[string]any{"name": "sessionbus", "arguments": map[string]any{"action": "list"}})
	failed := terminal["error"].(map[string]any)
	check(t, failed["code"] == float64(-32602) && failed["message"] == "invalid_hello", "terminal tools/call = %#v", terminal)
	must(t, writeInput.Close())
	must(t, <-served)
	backend.Shutdown()
}

func TestPeerDeliveryOwnerShutdownCrossesBlockedInterject(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "sessionbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	recordPath := filepath.Join(root, "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_INTERJECT_BLOCK", filepath.Join(root, "never"))
	observerPID := filepath.Join(root, "observer.pid")
	t.Setenv("GROK_TEST_OBSERVER_PID", observerPID)
	environment := setEnvironment(setEnvironment(os.Environ(), grokSessionIDEnv, testSessionID), grokLeaderSocketEnv, filepath.Join(root, "leader.sock"))
	backend, err := NewPeerBackend(context.Background(), environment)
	must(t, err)
	hello := <-hellos
	hello.ack <- true
	<-backend.peer.Ready()
	delivered := deliverPeer(backend, context.Background(), delivery("blocked"))
	waitFrame(t, recordPath, "_x.ai/interject", 1)
	pidfd := interactivePidfd(t, observerPID)
	defer unix.Close(pidfd)
	closed := make(chan struct{})
	go func() { backend.Shutdown(); close(closed) }()
	<-closed
	result := <-delivered
	check(t, result.err == nil && result.receipt.Reason == "shutting down", "blocked delivery = %#v / %v", result.receipt, result.err)
	check(t, !processRunning(t, pidfd), "blocked observer survived helper shutdown")
}

func TestPeerDeliveryOwnerSerializesTwoReceipts(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "sessionbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	recordPath := filepath.Join(root, "record")
	t.Setenv("GROK_TEST_RECORD", recordPath)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	cwd, err := os.Getwd()
	must(t, err)
	t.Setenv("GROK_TEST_CWD", cwd)
	environment := setEnvironment(setEnvironment(os.Environ(), grokSessionIDEnv, testSessionID), grokLeaderSocketEnv, filepath.Join(root, "leader.sock"))
	backend, err := NewPeerBackend(context.Background(), environment)
	must(t, err)
	hello := <-hellos
	hello.ack <- true
	<-backend.peer.Ready()
	results := make(chan peerDeliveryResult, 2)
	for _, message := range []string{"first", "second"} {
		go func(message string) {
			request := delivery(message)
			request.MessageID = message
			receipt, err := backend.deliver(context.Background(), backend.identity, request)
			results <- peerDeliveryResult{receipt: receipt, err: err}
		}(message)
	}
	for range 2 {
		result := <-results
		must(t, result.err)
		check(t, result.receipt.Disposition == "injected", "delivery receipt = %#v", result.receipt)
	}
	check(t, len(peerClientPIDs(t, records(t, recordPath))) == 1, "serialized deliveries opened more than one observer")
	check(t, countFrames(records(t, recordPath), "_x.ai/interject") == 2, "serialized deliveries lost an interject")
	backend.Shutdown()
}

func TestPeerHelperRequiresProductIdentity(t *testing.T) {
	_, err := NewPeerBackend(context.Background(), nil)
	check(t, err != nil && strings.Contains(err.Error(), "start Grok with grok-peer"), "missing identity = %v", err)
}

func TestInteractiveLauncherOwnsLeaderHoldAndTUI(t *testing.T) {
	root := shortRoot(t)
	recordPath := filepath.Join(root, "record")
	socket := filepath.Join(root, "sessionbus.sock")
	t.Setenv(host.SocketEnv, socket)
	t.Setenv("GROK_TEST_RECORD", recordPath)
	started := filepath.Join(root, "interactive-started")
	leaderPID, holdPID, tuiPID := filepath.Join(root, "leader.pid"), filepath.Join(root, "hold.pid"), filepath.Join(root, "tui.pid")
	t.Setenv("GROK_TEST_INTERACTIVE_STARTED", started)
	t.Setenv("GROK_TEST_LEADER_PID", leaderPID)
	t.Setenv("GROK_TEST_OBSERVER_PID", holdPID)
	t.Setenv("GROK_TEST_INTERACTIVE_PID", tuiPID)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID, "--group", "team", "--cwd", root}, os.Environ())
	must(t, err)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunInteractive(ctx, plan) }()
	<-fileReady(started)
	waitFrame(t, recordPath, "authenticate", 1)
	frames := records(t, recordPath)
	clients := peerClientPIDs(t, frames)
	check(t, len(clients) == 1 && slices.Equal(peerClientMethods(frames, clients[0]), []string{"initialize", "authenticate"}), "startup hold was not the only quiet ACP client: %#v", frames)
	check(t, countFrames(frames, "_x.ai/sessions/list") == 0, "launcher queried the roster")
	check(t, containsStartEnv(frames, "leader", host.SocketEnv, socket) && containsStartEnv(frames, "leader", host.GroupsEnv, `["team"]`), "leader did not inherit helper bus identity")
	check(t, !containsStart(frames, "SESSIONBUS_LANE_SOCKET"), "interactive launcher published a private action endpoint")
	leaderPidfd, holdPidfd, tuiPidfd := interactivePidfd(t, leaderPID), interactivePidfd(t, holdPID), interactivePidfd(t, tuiPID)
	defer unix.Close(leaderPidfd)
	defer unix.Close(holdPidfd)
	defer unix.Close(tuiPidfd)
	cancel(testSignal{syscall.SIGINT})
	var exited *exec.ExitError
	err = <-done
	check(t, errors.As(err, &exited) && exited.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGINT, "signalled Grok child = %v", err)
	check(t, !processRunning(t, leaderPidfd) && !processRunning(t, holdPidfd) && !processRunning(t, tuiPidfd), "interactive dependency survived shutdown")
	check(t, !exists(filepath.Join(root, "lanes", host.LaunchTokenDigest(testSessionID)+".sock")), "peer endpoint remains")
}

func TestStartupHoldExitStopsInteractiveOwner(t *testing.T) {
	root := shortRoot(t)
	recordPath := filepath.Join(root, "record")
	t.Setenv(host.SocketEnv, filepath.Join(root, "sessionbus.sock"))
	t.Setenv("GROK_TEST_RECORD", recordPath)
	tuiPID, leaderPID, holdPID := filepath.Join(root, "tui.pid"), filepath.Join(root, "leader.pid"), filepath.Join(root, "hold.pid")
	release := filepath.Join(root, "release-hold")
	t.Setenv("GROK_TEST_INTERACTIVE_PID", tuiPID)
	t.Setenv("GROK_TEST_LEADER_PID", leaderPID)
	t.Setenv("GROK_TEST_OBSERVER_PID", holdPID)
	t.Setenv("GROK_TEST_HOLD_EXIT", release)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID, "--cwd", root}, os.Environ())
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- RunInteractive(context.Background(), plan) }()
	tuiPidfd, leaderPidfd, holdPidfd := interactivePidfd(t, tuiPID), interactivePidfd(t, leaderPID), interactivePidfd(t, holdPID)
	defer unix.Close(tuiPidfd)
	defer unix.Close(leaderPidfd)
	defer unix.Close(holdPidfd)
	must(t, os.WriteFile(release, nil, 0o600))
	err = <-done
	check(t, strings.Contains(err.Error(), "Grok startup hold closed"), "launcher error = %v", err)
	check(t, !processRunning(t, tuiPidfd) && !processRunning(t, holdPidfd) && !processRunning(t, leaderPidfd), "bootstrap process survived startup-hold failure")
	clients := peerClientPIDs(t, records(t, recordPath))
	check(t, len(clients) == 1 && slices.Equal(peerClientMethods(records(t, recordPath), clients[0]), []string{"initialize", "authenticate"}), "startup hold was not quiet")
}

func TestInteractiveLauncherReturnsProductExit(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(host.SocketEnv, filepath.Join(root, "sessionbus.sock"))
	leaderPID, holdPID := filepath.Join(root, "leader.pid"), filepath.Join(root, "hold.pid")
	interactivePID, release := filepath.Join(root, "interactive.pid"), filepath.Join(root, "release")
	t.Setenv("GROK_TEST_LEADER_PID", leaderPID)
	t.Setenv("GROK_TEST_OBSERVER_PID", holdPID)
	t.Setenv("GROK_TEST_INTERACTIVE_PID", interactivePID)
	t.Setenv("GROK_TEST_INTERACTIVE_EXIT", "7")
	t.Setenv("GROK_TEST_INTERACTIVE_EXIT_BARRIER", release)
	plan, err := InteractivePlan([]string{"--session-id", testSessionID, "--cwd", root}, os.Environ())
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- RunInteractive(context.Background(), plan) }()
	leaderPidfd, holdPidfd := interactivePidfd(t, leaderPID), interactivePidfd(t, holdPID)
	must(t, os.WriteFile(release, nil, 0o600))
	err = <-done
	var exited *exec.ExitError
	check(t, errors.As(err, &exited) && exited.ExitCode() == 7, "exit = %v", err)
	check(t, !processRunning(t, leaderPidfd) && !processRunning(t, holdPidfd), "Grok dependencies survived the TUI")
	unix.Close(leaderPidfd)
	unix.Close(holdPidfd)
}

func TestLeaderCreatesDefaultStateRoot(t *testing.T) {
	root := filepath.Join(shortRoot(t), "state")
	t.Setenv(host.SocketEnv, "")
	t.Setenv("XDG_STATE_HOME", root)
	socket := sessionkit.Socket()
	check(t, !exists(filepath.Dir(socket)), "default run directory already exists")
	leader, err := startLeader(socket, host.LaunchTokenDigest(testSessionID), t.TempDir(), "default", os.Environ())
	must(t, err)
	check(t, grokSocketReady(leaderSocket(socket, host.LaunchTokenDigest(testSessionID))), "leader socket was not created")
	must(t, closeNative("leader", leader))
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

func TestPeerShutdownKillsItsObserverProcessGroup(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "sessionbus.sock")
	server, hellos := fakeDaemon(t, socket)
	defer server.Close()
	t.Setenv(host.SocketEnv, socket)
	t.Setenv("GROK_TEST_SESSION_ID", testSessionID)
	t.Setenv("GROK_TEST_DESCENDANT_PID", filepath.Join(root, "descendant.pid"))
	cwd, err := os.Getwd()
	must(t, err)
	t.Setenv("GROK_TEST_CWD", cwd)
	environment := setEnvironment(setEnvironment(os.Environ(), grokSessionIDEnv, testSessionID), grokLeaderSocketEnv, filepath.Join(root, "leader.sock"))
	opened := make(chan *PeerBackend, 1)
	go func() { backend, _ := NewPeerBackend(context.Background(), environment); opened <- backend }()
	hello := <-hellos
	hello.ack <- true
	backend := <-opened
	receipt, err := backend.deliver(context.Background(), backend.identity, delivery("peer message"))
	must(t, err)
	check(t, receipt.Disposition == "injected", "delivery = %#v", receipt)
	body, err := os.ReadFile(filepath.Join(root, "descendant.pid"))
	must(t, err)
	var pid int
	_, err = fmt.Sscan(string(body), &pid)
	must(t, err)
	pidfd := pidfd(t, pid)
	defer unix.Close(pidfd)
	backend.Shutdown()
	waitProcessExit(t, pidfd)
	check(t, !processRunning(t, pidfd), "observer descendant survived shutdown")
}

type hello struct {
	SessionID string         `json:"session_id"`
	Name      string         `json:"name"`
	Product   string         `json:"product"`
	Groups    []string       `json:"groups"`
	Info      map[string]any `json:"info"`
	ack       chan bool
	done      chan struct{}
}

type testSignal struct{ os.Signal }

func (s testSignal) Error() string           { return s.String() }
func (s testSignal) CaughtSignal() os.Signal { return s.Signal }

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
		var admitted hello
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
				value.done = make(chan struct{})
				hellos <- value
				go func(id int64, value hello) {
					defer close(value.done)
					accepted := <-value.ack
					write.Lock()
					if accepted {
						admitted = value
						_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
					} else {
						_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "invalid_hello"}})
					}
					write.Unlock()
				}(frame.ID, value)
				continue
			}
			if frame.Method == "session.list" {
				write.Lock()
				row := sessionkit.SessionSummary{SessionID: admitted.SessionID + "@local", Kind: "peer", Product: admitted.Product, Name: admitted.Name + "@local", Groups: admitted.Groups, Connected: true, Info: admitted.Info}
				_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": sessionkit.SessionListResult{Sessions: []sessionkit.SessionSummary{row}}})
				write.Unlock()
				continue
			}
			if frame.Method != "" {
				write.Lock()
				_ = json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
				write.Unlock()
			}
		}
	}()
	return listener, hellos
}

func deliverPeer(backend *PeerBackend, ctx context.Context, request sessionkit.DeliveryRequest) <-chan peerDeliveryResult {
	result := make(chan peerDeliveryResult, 1)
	go func() {
		receipt, err := backend.deliver(ctx, backend.identity, request)
		result <- peerDeliveryResult{receipt: receipt, err: err}
	}()
	return result
}

func mcpResponse(t *testing.T, encoder *json.Encoder, scanner *bufio.Scanner, id int, method string, params any) map[string]any {
	t.Helper()
	must(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
	check(t, scanner.Scan(), "MCP response absent: %v", scanner.Err())
	var response map[string]any
	must(t, json.Unmarshal(scanner.Bytes(), &response))
	check(t, response["id"] == float64(id), "MCP response id = %#v", response)
	return response
}

func interactivePidfd(t *testing.T, path string) int {
	t.Helper()
	<-fileReady(path)
	body, err := os.ReadFile(path)
	must(t, err)
	var pid int
	_, err = fmt.Sscan(string(body), &pid)
	must(t, err)
	return pidfd(t, pid)
}

func peerClientPIDs(t *testing.T, rows []json.RawMessage) []int {
	t.Helper()
	var result []int
	for _, raw := range rows {
		var record struct {
			Kind  string `json:"kind"`
			Value struct {
				PID       int      `json:"pid"`
				Arguments []string `json:"arguments"`
			} `json:"value"`
		}
		must(t, json.Unmarshal(raw, &record))
		if record.Kind == "START" && slices.Contains(record.Value.Arguments, "--leader") && slices.Contains(record.Value.Arguments, "stdio") {
			result = append(result, record.Value.PID)
		}
	}
	return result
}

func peerClientMethods(rows []json.RawMessage, pid int) []string {
	var result []string
	for _, raw := range rows {
		var record struct {
			Kind  string `json:"kind"`
			Value struct {
				PID    int    `json:"_testPID"`
				Method string `json:"method"`
			} `json:"value"`
		}
		if json.Unmarshal(raw, &record) == nil && record.Kind == "FRAME" && record.Value.PID == pid {
			result = append(result, record.Value.Method)
		}
	}
	return result
}

func containsStartEnv(rows []json.RawMessage, argument, name, value string) bool {
	for _, raw := range rows {
		var record struct {
			Kind  string `json:"kind"`
			Value struct {
				Arguments   []string          `json:"arguments"`
				Environment map[string]string `json:"environment"`
			} `json:"value"`
		}
		if json.Unmarshal(raw, &record) == nil && record.Kind == "START" && slices.Contains(record.Value.Arguments, argument) && record.Value.Environment[name] == value {
			return true
		}
	}
	return false
}

func pidfd(t *testing.T, pid int) int {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	must(t, err)
	return fd
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

func waitProcessExit(t *testing.T, pidfd int) {
	t.Helper()
	poll := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	_, err := unix.Poll(poll, -1)
	must(t, err)
}
