package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/qwen"
)

func TestLaneModeRejectsArguments(t *testing.T) {
	t.Setenv(host.TokenEnv, "token")
	if err := run(context.Background(), []string{"mcp"}); err == nil || err.Error() != "lane mode accepts no arguments" {
		t.Fatalf("run = %v", err)
	}
}

func TestMCPStartsOneResidentPeerBeforeFirstToolCall(t *testing.T) {
	root, socket := t.TempDir(), filepath.Join(t.TempDir(), "bus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	inputFile := filepath.Join(root, "input.jsonl")
	if err := os.WriteFile(inputFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(host.SocketEnv, socket)
	t.Setenv(host.LocalKeyEnv, "")
	t.Setenv(host.TokenEnv, "")
	t.Setenv(host.SessionIDEnv, "c4b4334f-f2b7-4ad8-9b1c-f3ef6f46c644")
	t.Setenv(host.NameEnv, "Qwen Peer Runtime")
	t.Setenv(host.GroupsEnv, `["qwen-peer-cells"]`)
	t.Setenv(qwen.InputFileEnv, inputFile)
	t.Setenv("QWEN_HOME", root)
	requests := make(chan map[string]any)
	go serveBus(listener, requests)

	input, writeInput := io.Pipe()
	var output bytes.Buffer
	served := make(chan error, 1)
	go func() { served <- serveMCP(context.Background(), input, &output) }()
	_, _ = fmt.Fprintln(writeInput, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	_, _ = fmt.Fprintln(writeInput, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	hello := <-requests
	params := hello["params"].(map[string]any)
	if hello["method"] != "session.hello" || params["session_id"] != "c4b4334f-f2b7-4ad8-9b1c-f3ef6f46c644" || params["name"] != "Qwen Peer Runtime" || fmt.Sprint(params["groups"]) != "[qwen-peer-cells]" {
		t.Fatalf("hello after initialize/list = %#v", hello)
	}
	_, _ = fmt.Fprintln(writeInput, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"agent_sessions","arguments":{"action":"list","arguments":{}}}}`)
	action := <-requests
	if action["method"] != "session.list" || action["id"] == hello["id"] {
		t.Fatalf("action did not use resident connection: %#v", action)
	}
	_ = writeInput.Close()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	seen := map[float64]bool{}
	for scanner := bufio.NewScanner(bytes.NewReader(output.Bytes())); scanner.Scan(); {
		var response map[string]any
		if json.Unmarshal(scanner.Bytes(), &response) != nil || response["error"] != nil {
			t.Fatalf("MCP response = %s", scanner.Bytes())
		}
		seen[response["id"].(float64)] = true
		if response["id"] == float64(3) && !strings.Contains(string(scanner.Bytes()), `"structuredContent":{"hosts":[],"sessions":[]}`) {
			t.Fatalf("tools/call response = %s", scanner.Bytes())
		}
	}
	if !seen[1] || !seen[2] || !seen[3] {
		t.Fatalf("MCP response ids = %#v: %s", seen, output.String())
	}
}

func serveBus(listener net.Listener, requests chan<- map[string]any) {
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	defer connection.Close()
	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		requests <- request
		result := `{}`
		if request["method"] == "session.list" {
			result = `{"sessions":[],"hosts":[]}`
		}
		_, _ = fmt.Fprintf(connection, `{"jsonrpc":"2.0","id":%v,"result":%s}`+"\n", request["id"], result)
	}
}
