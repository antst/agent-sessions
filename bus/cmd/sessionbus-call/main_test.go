package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/sessionbus/bus/internal/daemon"
	"github.com/antst/sessionbus/bus/internal/protocol"
	"github.com/antst/sessionbus/bus/internal/rpc"
)

func TestOneShotResultAndError(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "sessionbus.sock")
	service, err := daemon.Start(daemon.Config{SocketPath: socket, TablePath: filepath.Join(directory, "sessions")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-socket", socket, "-name", "probe", "session.list", `{}`}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit %d: %s / %s", code, stdout.String(), stderr.String())
	}
	var listed protocol.SessionListResult
	if err = json.Unmarshal(stdout.Bytes(), &listed); err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].Name != "probe@local" {
		t.Fatalf("list = %#v, %v", listed, err)
	}

	stdout.Reset()
	if code := run([]string{"-socket", socket, "-g", "team", "turn.interrupt", `{"session_id":"missing"}`}, &stdout, &stderr); code != 1 {
		t.Fatalf("error exit = %d", code)
	}
	var rpcError protocol.RPCError
	if err = json.Unmarshal(stdout.Bytes(), &rpcError); err != nil || rpcError.Code != protocol.UnknownSession {
		t.Fatalf("error = %#v, %v (%s)", rpcError, err, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	params := `{"target":"self","message":"hello"}`
	if code := run([]string{"-socket", socket, "-name", "self", "-g", "team", "message.send", params}, &stdout, &stderr); code != 0 {
		t.Fatalf("send exit %d: %s / %s", code, stdout.String(), stderr.String())
	}
	var delivery protocol.DeliveryRequest
	if err = json.Unmarshal(stderr.Bytes(), &delivery); err != nil || delivery.Body != "hello" {
		t.Fatalf("displayed delivery = %#v, %v", delivery, err)
	}
}

func TestUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"session.list", "{"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Fatalf("usage = %d, %q, %q", code, stdout.String(), stderr.String())
	}
}

func TestSchemaErrorNamesPathAndConstraint(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "sessionbus.sock")
	service, err := daemon.Start(daemon.Config{SocketPath: socket, TablePath: filepath.Join(directory, "sessions")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	var stdout, stderr bytes.Buffer
	params := `{"resume_session_id":"lane@local","name":"child"}`
	if code := run([]string{"-socket", socket, "lane.spawn", params}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d: %s / %s", code, stdout.String(), stderr.String())
	}
	var output map[string]string
	decodeErr := json.Unmarshal(stdout.Bytes(), &output)
	want := `LaneSpawnRequest: "name" is not allowed with "resume_session_id"`
	if decodeErr != nil || output["error"] != want || stderr.Len() != 0 {
		t.Fatalf("output = %q / %q", stdout.String(), stderr.String())
	}
}

func TestTurnRunWaitsForTerminal(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sessionbus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan *rpc.Request, 2)
	serverReady := make(chan *rpc.Conn, 1)
	go func() {
		fd, _ := listener.Accept()
		serverReady <- rpc.New(fd, false, func(_ context.Context, request *rpc.Request) { requests <- request })
	}()
	var stdout, stderr bytes.Buffer
	finished := make(chan int, 1)
	go func() {
		params := `{"session_id":"lane@local","input":"work"}`
		finished <- run([]string{"-socket", socket, "turn.run", params}, &stdout, &stderr)
	}()
	server := <-serverReady
	hello := waitRequest(t, requests)
	if hello.Method != "session.hello" {
		t.Fatalf("first method = %s", hello.Method)
	}
	if err = server.Result(hello, struct{}{}); err != nil {
		t.Fatal(err)
	}
	runRequest := waitRequest(t, requests)
	if runRequest.Method != "turn.run" {
		t.Fatalf("second method = %s", runRequest.Method)
	}
	select {
	case code := <-finished:
		t.Fatalf("returned before terminal with exit %d", code)
	default:
	}
	terminal := protocol.TurnResult{Outcome: "completed", Result: "terminal"}
	if err = server.Result(runRequest, terminal); err != nil {
		t.Fatal(err)
	}
	if code := <-finished; code != 0 || stdout.String() != `{"outcome":"completed","result":"terminal"}`+"\n" {
		t.Fatalf("terminal exit %d: %s / %s", code, stdout.String(), stderr.String())
	}
}

func waitRequest(t *testing.T, requests <-chan *rpc.Request) *rpc.Request {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("request timed out")
		return nil
	}
}
