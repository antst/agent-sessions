package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRawV1HelperSendsHelloBeforeMessage(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	socketPath := filepath.Join(t.TempDir(), "presence.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		decoder, encoder := json.NewDecoder(connection), json.NewEncoder(connection)
		var hello struct {
			JSONRPC string `json:"jsonrpc"`
			ID      string `json:"id"`
			Method  string `json:"method"`
			Params  struct {
				Protocol int      `json:"protocol"`
				Product  string   `json:"product"`
				Groups   []string `json:"groups"`
			} `json:"params"`
		}
		if decodeErr := decoder.Decode(&hello); decodeErr != nil {
			serverDone <- decodeErr
			return
		}
		if hello.JSONRPC != "2.0" || hello.ID != "hello" || hello.Method != "session.hello" ||
			hello.Params.Protocol != 1 || hello.Params.Product != "claude" ||
			!equalOrderedStrings(hello.Params.Groups, []string{"matrix-group"}) {
			serverDone <- fmt.Errorf("unexpected hello: %+v", hello)
			return
		}
		if encodeErr := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "hello", "result": map[string]any{}}); encodeErr != nil {
			serverDone <- encodeErr
			return
		}
		var send struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				Target  string `json:"target"`
				Message string `json:"message"`
			} `json:"params"`
		}
		if decodeErr := decoder.Decode(&send); decodeErr != nil {
			serverDone <- decodeErr
			return
		}
		if send.ID != "send" || send.Method != "message.send" || send.Params.Target != "missing" || send.Params.Message != "marker" {
			serverDone <- fmt.Errorf("unexpected send: %+v", send)
			return
		}
		serverDone <- encoder.Encode(map[string]any{
			"jsonrpc": "2.0", "id": "send",
			"error": map[string]any{"code": -32001, "message": "Unknown session or target"},
		})
	}()

	evidence := filepath.Join(t.TempDir(), "wire.json")
	helper := filepath.Join(repositoryRoot(t), "scripts", "realproducts", "matrix_v1.py")
	command := exec.Command(python, helper,
		"--socket", socketPath,
		"--uuid", "11111111-1111-4111-8111-111111111111",
		"--name", "matrix-source",
		"--group", "matrix-group",
		"--params", `{"target":"missing","message":"marker"}`,
		"--evidence", evidence,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v: %s", err, output)
	}
	if serverErr := <-serverDone; serverErr != nil {
		t.Fatal(serverErr)
	}
	var response matrixRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(output), &response); err != nil || response.Error == nil || response.Error.Code != -32001 {
		t.Fatalf("helper response = %s, %v", output, err)
	}
	body, err := os.ReadFile(evidence)
	if err != nil || !bytes.Contains(body, []byte("session.hello")) || !bytes.Contains(body, []byte("message.send")) {
		t.Fatalf("wire evidence = %s, %v", body, err)
	}
}

func TestStandingMatrixRequestDispatch(t *testing.T) {
	for _, args := range [][]string{
		{"--standing-matrix"},
		{"--repository-root", "/repo", "--standing-matrix=true"},
	} {
		if !standingMatrixRequested(args) {
			t.Fatalf("standingMatrixRequested(%q) = false", args)
		}
	}
	for _, args := range [][]string{
		{"--cells", "S-01"},
		{"--", "--standing-matrix"},
	} {
		if standingMatrixRequested(args) {
			t.Fatalf("standingMatrixRequested(%q) = true", args)
		}
	}
}

func TestMatrixPeerArgumentsKeepWrapperIdentityExplicit(t *testing.T) {
	product := matrixProduct{displayArguments: []string{"--screen-reader"}}
	tests := []struct {
		name, peerName, group string
		resume                bool
		want                  []string
	}{
		{
			name: "named fresh", peerName: "matrix-a", group: "matrix-g",
			want: []string{"-n", "matrix-a", "-g", "matrix-g", "--no-inherit-groups", "--screen-reader"},
		},
		{
			name: "no group", want: []string{"--no-inherit-groups", "--screen-reader"},
		},
		{
			name: "named resume", peerName: "matrix-a", group: "matrix-g", resume: true,
			want: []string{"--resume", "matrix-a", "-g", "matrix-g", "--no-inherit-groups", "--screen-reader"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := matrixPeerArguments(product, test.peerName, test.group, test.resume)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("matrixPeerArguments() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMatrixRosterAssertionsRequireExactGroups(t *testing.T) {
	var roster matrixRoster
	roster.Host.ID = "pdev"
	row := matrixRosterEntry{ID: "native-1", Groups: []string{"matrix-g", "session:pdev/native-1"}}
	if err := requireNamedGroups(roster, row, "matrix-g"); err != nil {
		t.Fatal(err)
	}
	row.Groups = []string{"session:pdev/native-1"}
	if err := requireOnlyPrivateAnchor(roster, row); err != nil {
		t.Fatal(err)
	}
	row.Groups = append(row.Groups, "unexpected")
	if err := requireOnlyPrivateAnchor(roster, row); err == nil {
		t.Fatal("private-anchor assertion accepted an extra group")
	}
}

func TestMatrixAcceptedDeliveriesRequireExactNativeIDs(t *testing.T) {
	result, err := json.Marshal(matrixSendResult{
		MessageID: "message-1",
		Deliveries: []matrixDelivery{
			{Target: "matrix-b", SessionID: "native-b", DeliveryID: "delivery-b", Status: "accepted"},
			{Target: "matrix-a", SessionID: "native-a", DeliveryID: "delivery-a", Status: "accepted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := matrixRPCResponse{JSONRPC: "2.0", Result: result}
	if err := requireAcceptedSessions(response, []string{"native-a", "native-b"}); err != nil {
		t.Fatal(err)
	}
	if err := requireAcceptedSessions(response, []string{"native-a"}); err == nil {
		t.Fatal("delivery assertion accepted an extra native id")
	}
	response.Error = &matrixRPCError{Code: -32002, Message: "Session busy"}
	if err := requireAcceptedSessions(response, []string{"native-a", "native-b"}); err == nil {
		t.Fatal("delivery assertion accepted an RPC error")
	}
}

func TestMatrixGroupPendingFollowsCheckedInSpec(t *testing.T) {
	root := t.TempDir()
	specDir := filepath.Join(root, "docs", "specs")
	if err := os.MkdirAll(specDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(specDir, "NATIVE-PEER-PROTOCOL.md")
	if err := os.WriteFile(path, []byte("message.send supports target and targets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if documented, err := matrixSpecDocumentsGroup(root); err != nil || documented {
		t.Fatalf("pre-B1 spec detection = %t, %v", documented, err)
	}
	if err := os.WriteFile(path, []byte("  \"group\": {\"type\": \"string\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if documented, err := matrixSpecDocumentsGroup(root); err != nil || !documented {
		t.Fatalf("post-B1 spec detection = %t, %v", documented, err)
	}
}

func TestMatrixReporterCountsOnlyFailuresAndWritesEveryCell(t *testing.T) {
	var output bytes.Buffer
	runner := matrixRunner{
		config: matrixOptions{evidenceDir: t.TempDir()}, output: &output,
	}
	runner.record("codex", "pass", matrixPass, "ok", nil)
	runner.record("codex", "skip", matrixSkip, "pending", nil)
	runner.record("codex", "fail", matrixFail, "red", nil)
	if runner.cellCount != 3 || runner.failures != 1 {
		t.Fatalf("cells/failures = %d/%d, want 3/1", runner.cellCount, runner.failures)
	}
	for _, cell := range []string{"pass", "skip", "fail"} {
		path := filepath.Join(runner.config.evidenceDir, "codex", cell+".json")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("cell evidence %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("cell evidence %s is empty", path)
		}
	}
	for _, status := range []string{matrixPass, matrixSkip, matrixFail} {
		if !strings.Contains(output.String(), status+" codex/") {
			t.Fatalf("output lacks %s: %s", status, output.String())
		}
	}
}
