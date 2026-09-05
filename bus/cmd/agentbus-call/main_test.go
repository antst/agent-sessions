package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/bus/internal/daemon"
	"github.com/antst/agent-sessions/bus/internal/protocol"
)

func TestOneShotResultAndError(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "agentbus.sock")
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
