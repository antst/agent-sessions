package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestMatrixCWDPreflightDoesNotWrite(t *testing.T) {
	tests := []struct{ name, cwd, temporary, evidence string }{
		{"temporary equal", "cwd", "cwd", "evidence"},
		{"temporary below", "cwd", "cwd/temp", "evidence"},
		{"temporary above", "temp/cwd", "temp", "evidence"},
		{"evidence equal", "cwd", "temp", "cwd"},
		{"evidence below", "cwd", "temp", "cwd/evidence"},
		{"evidence above", "evidence/cwd", "temp", "evidence"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			cwd, temporary, evidence := filepath.Join(base, test.cwd), filepath.Join(base, test.temporary), filepath.Join(base, test.evidence)
			for _, path := range []string{cwd, temporary, evidence} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := matrixTreeSnapshot(t, base)
			_, err := parseMatrixOptions([]string{"--standing-matrix", "--repository-root", repositoryRoot(t), "--cwd", cwd, "--temporary-root", temporary, "--evidence-dir", evidence})
			if err == nil || before != matrixTreeSnapshot(t, base) {
				t.Fatalf("preflight error/snapshot = %v/%q, want error/%q", err, matrixTreeSnapshot(t, base), before)
			}
		})
	}
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"", filepath.Join(base, "missing"), file, filepath.Join(base, "link")} {
		evidence := filepath.Join(base, "must-not-exist")
		_, err := parseMatrixOptions([]string{"--standing-matrix", "--repository-root", repositoryRoot(t), "--cwd", cwd, "--temporary-root", os.TempDir(), "--evidence-dir", evidence})
		if _, statErr := os.Lstat(evidence); err == nil || !os.IsNotExist(statErr) {
			t.Fatalf("cwd %q error/evidence = %v/%v", cwd, err, statErr)
		}
	}
	if matrixPathsOverlap(filepath.Join(base, "cwd"), filepath.Join(base, "cwd-other")) {
		t.Fatal("component-disjoint prefix siblings overlap")
	}
}
func matrixTreeSnapshot(t *testing.T, root string) string {
	var entries []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		body, _ := os.ReadFile(path)
		entries = append(entries, fmt.Sprintf("%s:%s:%x", relative, info.Mode(), body))
		return nil
	})
	return strings.Join(entries, "\n")
}
func TestMatrixTUIUsesApprovedCWDAndDiagnosesTrust(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "tmux")
	writeMatrixTestExecutable(t, fake, `case "$1" in
has-session) exit 1;;
capture-pane) [ "$MATRIX_CAPTURE_FAIL" = 1 ] && exit 4; printf '%s\n' "$MATRIX_PANE";;
esac`)
	cwd := filepath.Join(t.TempDir(), `odd ' cwd`)
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := matrixRunner{config: matrixOptions{cwd: cwd, tmux: fake, evidenceDir: t.TempDir()}, runID: "test", active: map[string]*matrixTUI{}}
	for _, product := range matrixProductInventory() {
		product.peerExecutable = "/native/" + product.id + "-peer"
		for _, resume := range []bool{false, true} {
			_, command, err := runner.startTUI(context.Background(), product, "matrix-name", "matrix-group", "label", resume)
			if err != nil || len(command) < 7 || command[5] != "-c" || command[6] != cwd {
				t.Fatalf("%s resume=%t command/error = %q/%v", product.id, resume, command, err)
			}
		}
	}
	product := matrixProduct{id: "codex", nativeTrustPrompt: "Do you trust the contents of this directory?"}
	tui := &matrixTUI{tmuxName: "owned", pane: "owned:0.0"}
	t.Setenv("MATRIX_PANE", product.nativeTrustPrompt)
	evidence := map[string]any{}
	err := runner.diagnoseAttachFailure(product, "identity", tui, evidence, fmt.Errorf("attach failed"))
	if err == nil || !strings.Contains(err.Error(), "trust "+cwd+" once via codex's own prompt") || evidence["pane_evidence"] == nil {
		t.Fatalf("trust diagnosis/evidence = %v/%v", err, evidence)
	}
	t.Setenv("MATRIX_PANE", "login required")
	err = runner.diagnoseAttachFailure(matrixProduct{id: "grok"}, "identity", tui, map[string]any{}, fmt.Errorf("attach failed"))
	if err == nil || strings.Contains(err.Error(), "trust ") || !strings.Contains(err.Error(), "pane evidence") {
		t.Fatalf("generic diagnosis = %v", err)
	}
	t.Setenv("MATRIX_CAPTURE_FAIL", "1")
	err = runner.diagnoseAttachFailure(product, "identity", tui, map[string]any{}, fmt.Errorf("attach failed"))
	if err == nil || !strings.Contains(err.Error(), "capture unattached codex TUI") {
		t.Fatalf("capture diagnosis = %v", err)
	}
}
func writeMatrixTestExecutable(t *testing.T, path, body string) {
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}
func TestMatrixSignalOwnsAmbiguousStart(t *testing.T) {
	if os.Getenv("MATRIX_SIGNAL_HELPER") == "1" {
		if err := runStandingMatrix(context.Background(), strings.Fields(os.Getenv("MATRIX_ARGS")), io.Discard); err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("helper result = %v", err)
		}
		return
	}
	for _, test := range []struct {
		name   string
		signal syscall.Signal
		second bool
	}{{"interrupt", syscall.SIGINT, false}, {"terminate", syscall.SIGTERM, false}, {"second signal", syscall.SIGTERM, true}} {
		t.Run(test.name, func(t *testing.T) {
			base, bin := t.TempDir(), filepath.Join(t.TempDir(), "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			marker, starts := filepath.Join(base, "live"), filepath.Join(base, "starts")
			writeMatrixTestExecutable(t, filepath.Join(bin, "tmux"), `case "$1" in
has-session) [ -s "$MATRIX_MARKER" ];;
new-session) printf x >"$MATRIX_MARKER"; printf x >>"$MATRIX_STARTS"; while :; do :; done;;
capture-pane) printf 'starting\n';;
kill-session) [ "$MATRIX_SECOND" = 1 ] && kill -TERM "$PPID"; : >"$MATRIX_MARKER";;
esac`)
			writeMatrixTestExecutable(t, filepath.Join(bin, "agent-sessions"), `printf '%s\n' '{"schema":"agent-sessions.roster.v1","host":{"id":"pdev"},"local":[]}'`)
			for _, name := range []string{"python3", "codex", "codex-peer"} {
				writeMatrixTestExecutable(t, filepath.Join(bin, name), "exit 0")
			}
			cwd, temporary, evidence := filepath.Join(base, "cwd"), filepath.Join(base, "temporary"), filepath.Join(base, "evidence")
			for _, path := range []string{cwd, temporary, evidence} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("unix", filepath.Join(base, "presence.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			args := strings.Join([]string{"--standing-matrix", "--repository-root", repositoryRoot(t), "--cwd", cwd, "--temporary-root", temporary, "--evidence-dir", evidence, "--matrix-timeout", "5s"}, " ")
			command := exec.Command(os.Args[0], "-test.run=^TestMatrixSignalOwnsAmbiguousStart$")
			command.Env = append(os.Environ(), "MATRIX_SIGNAL_HELPER=1", "MATRIX_ARGS="+args, "MATRIX_MARKER="+marker, "MATRIX_STARTS="+starts, fmt.Sprintf("MATRIX_SECOND=%d", map[bool]int{true: 1}[test.second]), "AGENT_SESSIONS_PRESENCE_SOCKET="+listener.Addr().String(), "PATH="+bin)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if info, statErr := os.Stat(marker); statErr == nil && info.Size() != 0 {
					break
				} else if time.Now().After(deadline) {
					t.Fatal("fake tmux start was not reached")
				}
			}
			if err := command.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			startBody, _ := os.ReadFile(starts)
			if test.second {
				if err == nil {
					t.Fatal("second signal was swallowed")
				}
			} else if err != nil || string(startBody) != "x" {
				t.Fatalf("helper/start count = %v/%q", err, startBody)
			} else if info, statErr := os.Stat(marker); statErr != nil || info.Size() != 0 {
				t.Fatalf("exact session marker = %v/%v", info, statErr)
			}
		})
	}
}
