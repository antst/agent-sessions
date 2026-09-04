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
		if encodeErr := encoder.Encode(map[string]any{
			"jsonrpc": "2.0", "id": "send",
			"result": map[string]any{"message_id": "message-1", "deliveries": []map[string]any{{
				"target": "missing", "session_id": "native-1", "delivery_id": "delivery-1", "status": "accepted",
			}}},
		}); encodeErr != nil {
			serverDone <- encodeErr
			return
		}
		var extra json.RawMessage
		serverDone <- decoder.Decode(&extra)
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
	if serverErr := <-serverDone; serverErr != io.EOF {
		t.Fatal(serverErr)
	}
	var response matrixRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(output), &response); err != nil || requireAcceptedSessions(response, []string{"native-1"}) != nil {
		t.Fatalf("helper response = %s, %v", output, err)
	}
	body, err := os.ReadFile(evidence)
	if err != nil || !bytes.Contains(body, []byte("session.hello")) || !bytes.Contains(body, []byte("message.send")) {
		t.Fatalf("wire evidence = %s, %v", body, err)
	}
}

func TestStandingMatrixRequestDispatch(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"--standing-matrix"}, true}, {[]string{"--repository-root", "/repo", "--standing-matrix=true"}, true},
		{[]string{"--cells", "S-01"}, false}, {[]string{"--", "--standing-matrix"}, false},
	} {
		if got := standingMatrixRequested(test.args); got != test.want {
			t.Fatalf("standingMatrixRequested(%q) = %t, want %t", test.args, got, test.want)
		}
	}
}

func TestMatrixPeerArgumentsKeepWrapperIdentityExplicit(t *testing.T) {
	product := matrixProduct{displayArguments: []string{"--screen-reader"}}
	for _, test := range []struct {
		name, peerName, group string
		resume                bool
		want                  []string
	}{
		{"named fresh", "matrix-a", "matrix-g", false, []string{"-n", "matrix-a", "-g", "matrix-g", "--no-inherit-groups", "--screen-reader"}},
		{"no group", "", "", false, []string{"--no-inherit-groups", "--screen-reader"}},
		{"named resume", "matrix-a", "matrix-g", true, []string{"--resume", "matrix-a", "-g", "matrix-g", "--no-inherit-groups", "--screen-reader"}},
	} {
		got := matrixPeerArguments(product, test.peerName, test.group, test.resume)
		if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
			t.Fatalf("%s: matrixPeerArguments() = %q, want %q", test.name, got, test.want)
		}
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
	result, _ := json.Marshal(matrixSendResult{
		MessageID: "message-1",
		Deliveries: []matrixDelivery{
			{Target: "matrix-b", SessionID: "native-b", DeliveryID: "delivery-b", Status: "accepted"},
			{Target: "matrix-a", SessionID: "native-a", DeliveryID: "delivery-a", Status: "accepted"},
		},
	})
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
	path := filepath.Join(root, "docs", "specs", "NATIVE-PEER-PROTOCOL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		body string
		want bool
	}{{"message.send supports target and targets\n", false}, {"  \"group\": {\"type\": \"string\"}\n", true}} {
		if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := matrixSpecDocumentsGroup(root); err != nil || got != test.want {
			t.Fatalf("spec detection = %t/%v, want %t", got, err, test.want)
		}
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
		if err != nil || info.Size() == 0 {
			t.Fatalf("cell evidence %s: size/error = %v/%v", path, info, err)
		}
	}
	for _, status := range []string{matrixPass, matrixSkip, matrixFail} {
		if !strings.Contains(output.String(), status+" codex/") {
			t.Fatalf("output lacks %s: %s", status, output.String())
		}
	}
}

func TestMatrixCWDPreflightDoesNotWrite(t *testing.T) {
	for _, suffixes := range [][3]string{
		{"cwd", "cwd", "evidence"}, {"cwd", "cwd/temp", "evidence"}, {"temp/cwd", "temp", "evidence"},
		{"cwd", "temp", "cwd"}, {"cwd", "temp", "cwd/evidence"}, {"evidence/cwd", "temp", "evidence"},
	} {
		base := t.TempDir()
		cwd, temporary, evidence := filepath.Join(base, suffixes[0]), filepath.Join(base, suffixes[1]), filepath.Join(base, suffixes[2])
		for _, path := range []string{cwd, temporary, evidence} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		before := matrixTreeSnapshot(t, base)
		_, err := parseMatrixOptions([]string{"--standing-matrix", "--repository-root", repositoryRoot(t), "--cwd", cwd, "--temporary-root", temporary, "--evidence-dir", evidence})
		if after := matrixTreeSnapshot(t, base); err == nil || before != after {
			t.Fatalf("preflight error/snapshot = %v/%q, want error/%q", err, after, before)
		}
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

func TestMatrixTMUXPreflightRefusesExactNames(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "tmux")
	writeMatrixTestExecutable(t, fake, `printf '%s' "$MATRIX_TMUX_OUTPUT"; exit "${MATRIX_TMUX_STATUS:-0}"`)
	for _, test := range []struct {
		output string
		ok     bool
	}{
		{"no server running on /tmp/tmux-1000/default", true},
		{"error connecting to /tmp/tmux-1000/probe (No such file or directory)", true},
		{"permission denied", false}, {"probe: No such file or directory", false}, {"no server running on /tmp/socket\nextra", false},
	} {
		t.Setenv("MATRIX_TMUX_OUTPUT", test.output)
		t.Setenv("MATRIX_TMUX_STATUS", "1")
		if err := rejectExistingMatrixTMUX(fake); (err == nil) != test.ok {
			t.Fatalf("tmux preflight %q accepted=%t, want %t: %v", test.output, err == nil, test.ok, err)
		}
	}
	t.Setenv("MATRIX_TMUX_STATUS", "0")
	t.Setenv("MATRIX_TMUX_OUTPUT", "operator\nmatrix-a\nmatrix-ab\nmatrix-odd'quote \n")
	err := rejectExistingMatrixTMUX(fake)
	want := "standing matrix found existing matrix-* tmux sessions; run these exact commands first:\n" +
		"tmux kill-session -t '=matrix-a'\n" +
		"tmux kill-session -t '=matrix-ab'\n" +
		"tmux kill-session -t '=matrix-odd'\"'\"'quote '"
	if err == nil || err.Error() != want {
		t.Fatalf("preflight error = %q, want %q", err, want)
	}
}

func TestMatrixPrerequisiteFailuresPrecedeEvidenceWrite(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "presence.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, missing := range []string{"agent-sessions", "tmux", "python3", "socket", "helper", "tmux-list"} {
		t.Run("missing "+missing, func(t *testing.T) {
			base, bin := t.TempDir(), t.TempDir()
			for _, name := range []string{"agent-sessions", "tmux", "python3"} {
				if name != missing {
					body := "exit 0"
					if name == "tmux" && missing == "tmux-list" {
						body = "printf 'permission denied\\n'; exit 9"
					}
					writeMatrixTestExecutable(t, filepath.Join(bin, name), body)
				}
			}
			cwd, temporary, evidence := filepath.Join(base, "cwd"), filepath.Join(base, "temporary"), filepath.Join(base, "evidence")
			_ = os.Mkdir(cwd, 0o700)
			_ = os.Mkdir(temporary, 0o700)
			t.Setenv("PATH", bin)
			socket := listener.Addr().String()
			if missing == "socket" {
				socket = filepath.Join(base, "missing.sock")
			}
			t.Setenv("AGENT_SESSIONS_PRESENCE_SOCKET", socket)
			root := repositoryRoot(t)
			if missing == "helper" {
				root = base
			}
			arguments := []string{"--standing-matrix", "--repository-root", root, "--cwd", cwd, "--temporary-root", temporary}
			if missing != "tmux-list" {
				arguments = append(arguments, "--evidence-dir", evidence)
			}
			before := matrixTreeSnapshot(t, base)
			_, err := parseMatrixOptions(arguments)
			if after := matrixTreeSnapshot(t, base); err == nil || after != before {
				t.Fatalf("error/snapshot = %v/%q, want error/%q", err, after, before)
			}
		})
	}
}
func TestMatrixPersistenceRequiresOutputBeforeNativeReady(t *testing.T) {
	prompt, expected := matrixPersistencePrompt("1788480387-3a1ce53dd5a8")
	if strings.Contains(prompt, expected) || expected != "MXe53dd5a806" {
		t.Fatalf("prompt/expected = %q/%q", prompt, expected)
	}
	for _, product := range matrixProductInventory() {
		capture := "prompt\n" + expected + "\n" + product.nativeReadyMarker
		complete := paneHasMarkerThenReady(capture, expected, product.nativeReadyMarker, product.nativeBusyMarker)
		wrongOrder := paneHasMarkerThenReady(product.nativeReadyMarker+expected, expected, product.nativeReadyMarker, product.nativeBusyMarker)
		busy := product.nativeBusyMarker != "" && paneHasMarkerThenReady(capture+product.nativeBusyMarker, expected, product.nativeReadyMarker, product.nativeBusyMarker)
		if !complete || wrongOrder || busy {
			t.Fatalf("%s complete/wrong-order/busy = %t/%t/%t", product.id, complete, wrongOrder, busy)
		}
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
func TestMatrixDefaultSignalsLeaveExactRefusedResidue(t *testing.T) {
	if os.Getenv("MATRIX_SIGNAL_HELPER") == "1" {
		t.Fatalf("matrix returned before its default signal: %v", runStandingMatrix(context.Background(), strings.Split(os.Getenv("MATRIX_ARGS"), "\x1f"), io.Discard))
		return
	}
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			base, bin := t.TempDir(), t.TempDir()
			marker, starts, kills := filepath.Join(base, "live"), filepath.Join(base, "starts"), filepath.Join(base, "kills")
			writeMatrixTestExecutable(t, filepath.Join(bin, "tmux"), `case "$1" in
list-sessions) [ -s "$MATRIX_MARKER" ] && { IFS= read -r name <"$MATRIX_MARKER"; printf '%s\n' "$name"; }; exit 0;;
has-session) [ -s "$MATRIX_MARKER" ];;
new-session) printf matrix-residue >"$MATRIX_MARKER"; printf x >>"$MATRIX_STARTS";;
capture-pane) printf 'starting\n';;
kill-session) printf '%s\n' "$3" >>"$MATRIX_KILLS"; : >"$MATRIX_MARKER";;
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
			t.Setenv("PATH", bin)
			t.Setenv("MATRIX_MARKER", marker)
			t.Setenv("MATRIX_STARTS", starts)
			t.Setenv("MATRIX_KILLS", kills)
			t.Setenv("AGENT_SESSIONS_PRESENCE_SOCKET", listener.Addr().String())
			args := []string{"--standing-matrix", "--repository-root", repositoryRoot(t), "--cwd", cwd, "--temporary-root", temporary, "--evidence-dir", evidence, "--matrix-timeout", "5s"}
			command := exec.Command(os.Args[0], "-test.run=^TestMatrixDefaultSignalsLeaveExactRefusedResidue$")
			command.Env = append(os.Environ(), "MATRIX_SIGNAL_HELPER=1", "MATRIX_ARGS="+strings.Join(args, "\x1f"))
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
			if err := command.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
			if err == nil || !ok || !status.Signaled() || status.Signal() != signal {
				t.Fatalf("process result/status = %v/%v", err, status)
			}
			before := matrixTreeSnapshot(t, evidence)
			time.Sleep(100 * time.Millisecond)
			if after := matrixTreeSnapshot(t, evidence); after != before {
				t.Fatalf("evidence changed after signal:\n%s\n---\n%s", before, after)
			}
			nextEvidence := filepath.Join(base, "next-evidence")
			nextArgs := append(append([]string(nil), args[:len(args)-4]...), "--evidence-dir", nextEvidence, "--matrix-timeout", "5s")
			err = runStandingMatrix(context.Background(), nextArgs, io.Discard)
			if _, statErr := os.Lstat(nextEvidence); err == nil || !os.IsNotExist(statErr) || !strings.Contains(err.Error(), "tmux kill-session -t '=matrix-residue'") {
				t.Fatalf("next run error/evidence = %v/%v", err, statErr)
			}
			if body, _ := os.ReadFile(starts); string(body) != "x" {
				t.Fatalf("refused run launched another cell: %q", body)
			}
			runner := matrixRunner{config: matrixOptions{tmux: filepath.Join(bin, "tmux")}, active: map[string]*matrixTUI{"matrix-residue": {tmuxName: "matrix-residue"}}}
			runner.bestEffortCleanup()
			if body, _ := os.ReadFile(kills); string(body) != "=matrix-residue\n" {
				t.Fatalf("normal exact cleanup targets = %q", body)
			}
		})
	}
}
