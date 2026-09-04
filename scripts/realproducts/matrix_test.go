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

	"github.com/antst/agent-sessions/internal/livepresence"
)

func TestMatrixRawV1ClientHandlesBusyDeliveryAndRecordsEvidence(t *testing.T) {
	socketPath, serverDone := matrixTestServer(t, func(connection net.Conn) error {
		decoder, encoder := json.NewDecoder(connection), json.NewEncoder(connection)
		hello, err := decodeMatrixFrame(decoder)
		var report struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		}
		if err != nil || json.Unmarshal(hello.Params, &report) != nil || report.UUID == "" || report.Name != "matrix-source" ||
			string(hello.ID) != `"session.hello"` || hello.Method != "session.hello" {
			return fmt.Errorf("unexpected hello: %+v/%v", hello, err)
		}
		_ = encoder.Encode(livepresence.Success(hello.ID, json.RawMessage(`{}`)))
		send, err := decodeMatrixFrame(decoder)
		if err != nil || string(send.ID) != `"session.send"` || send.Method != "message.send" {
			return fmt.Errorf("unexpected send: %+v/%v", send, err)
		}
		deliver := livepresence.Frame{JSONRPC: "2.0", ID: json.RawMessage(`"delivery-1"`), Method: "message.deliver", Params: json.RawMessage(
			`{"message_id":"message-in","from":{"uuid":"native-in","name":"peer","product":"codex","groups":["matrix-group"]},"body":"busy"}`)}
		_ = encoder.Encode(deliver)
		busy, err := decodeMatrixFrame(decoder)
		if err != nil || string(busy.ID) != `"delivery-1"` || busy.Error == nil || busy.Error.Code != livepresence.Busy ||
			busy.Error.Message != "Session busy" || string(busy.Error.Data) != `{"uuid":"`+report.UUID+`"}` {
			return fmt.Errorf("unexpected busy response: %+v/%v", busy, err)
		}
		_ = encoder.Encode(livepresence.Success(send.ID, json.RawMessage(
			`{"message_id":"message-1","deliveries":[{"target":"missing","session_id":"native-1","delivery_id":"delivery-1","status":"accepted"}]}`)))
		_, err = decodeMatrixFrame(decoder)
		return err
	})
	runner := matrixRunner{config: matrixOptions{presenceSocket: socketPath, evidenceDir: t.TempDir()}, runID: "matrix-test"}
	response, evidence, err := runner.sendV1(context.Background(), "codex", "direct", "matrix-group", "matrix-source", map[string]any{"target": "missing", "message": "marker"})
	if serverErr := <-serverDone; serverErr != io.EOF {
		t.Fatal(serverErr)
	}
	if err != nil || requireAcceptedSessions(response, []string{"native-1"}) != nil {
		t.Fatalf("raw response/error = %+v, %v", response, err)
	}
	wire := readMatrixWire(t, evidence)
	if wire.Error != "" || len(wire.Frames) != 6 {
		t.Fatalf("wire evidence = %+v", wire)
	}
	var sequence []string
	for _, frame := range wire.Frames {
		sequence = append(sequence, frame.Direction+":"+frame.Frame.Method)
	}
	if got, want := strings.Join(sequence, ","), "send:session.hello,receive:,send:message.send,receive:message.deliver,send:,receive:"; got != want {
		t.Fatalf("wire sequence = %q, want %q", got, want)
	}
}

func decodeMatrixFrame(decoder *json.Decoder) (livepresence.Frame, error) {
	var frame livepresence.Frame
	err := decoder.Decode(&frame)
	return frame, err
}

func matrixTestServer(t *testing.T, serve func(net.Conn) error) (string, <-chan error) {
	listener, err := net.Listen("unix", matrixTestSocket(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			err = serve(connection)
			_ = connection.Close()
		}
		done <- err
	}()
	return listener.Addr().String(), done
}

func readMatrixWire(t *testing.T, path string) matrixWireEvidence {
	body, err := os.ReadFile(path)
	var evidence matrixWireEvidence
	if err != nil || json.Unmarshal(body, &evidence) != nil {
		t.Fatalf("wire evidence = %s/%v", body, err)
	}
	return evidence
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
	response := livepresence.Frame{JSONRPC: "2.0", Result: result}
	if err := requireAcceptedSessions(response, []string{"native-a", "native-b"}); err != nil {
		t.Fatal(err)
	}
	if err := requireAcceptedSessions(response, []string{"native-a"}); err == nil {
		t.Fatal("delivery assertion accepted an extra native id")
	}
	response.Error = &livepresence.RPCError{Code: -32002, Message: "Session busy"}
	if err := requireAcceptedSessions(response, []string{"native-a", "native-b"}); err == nil {
		t.Fatal("delivery assertion accepted an RPC error")
	}
}

func TestMatrixLanePromptAndLostStartRecoveryAreBounded(t *testing.T) {
	prompt, expected := matrixLanePrompt("12345678-deadbeef", "matrix-lane", "matrix-group")
	if strings.Contains(prompt, expected) || !strings.Contains(prompt, "agent_sessions lane tool") {
		t.Fatalf("lane prompt exposed derived marker or omitted tool authority: %q / %q", prompt, expected)
	}
	connector, logPath := matrixTestConnector(t, "sequence", 5*time.Second)
	connector.frames = append(connector.frames, matrixWireFrame{Direction: "send", Frame: livepresence.Frame{ID: []byte(`"lost-start"`), Method: "tools/call"}})
	if err := connector.recoverLane("parent", "matrix-lane"); err != nil {
		t.Fatal(err)
	}
	rows, err := connector.listLanes("parent", false)
	if closeErr := connector.Close(); err != nil || closeErr != nil || len(rows) != 0 {
		t.Fatalf("final list/close = %+v, %v/%v", rows, err, closeErr)
	}
	body, _ := os.ReadFile(logPath)
	want := "list --mine --all\ninterrupt matrix-lane\nwait matrix-lane --timeout 30\narchive matrix-lane\nlist --mine\n"
	if string(body) != want || len(connector.frames) != 11 || connector.frames[0].Direction != "send" {
		t.Fatalf("lost-response recovery/evidence = %q / %+v", body, connector.frames)
	}
	ids := map[string]bool{}
	for _, frame := range connector.frames[1:] {
		if frame.Direction == "send" {
			id := string(frame.Frame.ID)
			if ids[id] {
				t.Fatalf("duplicate connector id %s", id)
			}
			ids[id] = true
		}
	}
	if _, err := namedLane([]map[string]any{{"name": "same"}, {"name": "same"}}, "same"); err == nil {
		t.Fatal("duplicate lane name was not ambiguous")
	}
}

func TestMatrixConnectorTimeoutKillsAndJoinsOneProcess(t *testing.T) {
	connector, _ := matrixTestConnector(t, "hang", 50*time.Millisecond)
	started := time.Now()
	if _, _, err := connector.call("parent", map[string]any{"product": "codex", "command": "list", "arguments": []string{"--mine"}}); err == nil {
		t.Fatal("hung connector call succeeded")
	}
	if err := connector.Close(); err == nil || time.Since(started) > time.Second {
		t.Fatalf("connector timeout close = %v after %s", err, time.Since(started))
	}
}

func matrixTestConnector(t *testing.T, mode string, timeout time.Duration) (*matrixConnector, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	logPath := filepath.Join(t.TempDir(), "calls")
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=TestMatrixConnectorProcess")
	command.Env = append(os.Environ(), "MATRIX_CONNECTOR_HELPER="+mode, "MATRIX_CONNECTOR_LOG="+logPath)
	input, _ := command.StdinPipe()
	output, _ := command.StdoutPipe()
	connector := &matrixConnector{command: command, input: input, output: json.NewDecoder(output)}
	command.Stderr = &connector.stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return connector, logPath
}

func TestMatrixConnectorProcess(t *testing.T) {
	mode := os.Getenv("MATRIX_CONNECTOR_HELPER")
	if mode == "" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	listed := false
	for {
		var request livepresence.Frame
		if decoder.Decode(&request) != nil {
			return
		}
		if mode == "hang" {
			select {}
		}
		var call struct {
			Arguments struct {
				Command   string
				Arguments []string
			} `json:"arguments"`
		}
		if json.Unmarshal(request.Params, &call) != nil {
			os.Exit(2)
		}
		line := strings.TrimSpace(call.Arguments.Command+" "+strings.Join(call.Arguments.Arguments, " ")) + "\n"
		file, _ := os.OpenFile(os.Getenv("MATRIX_CONNECTOR_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = file.WriteString(line)
		_ = file.Close()
		result := map[string]any{"type": "ok"}
		if call.Arguments.Command == "list" {
			result = map[string]any{"lanes": []map[string]any{}}
			if !listed {
				result["lanes"] = []map[string]any{{"name": "matrix-lane", "state": "running"}}
				listed = true
			}
		}
		body, _ := json.Marshal(map[string]any{"structuredContent": result})
		_ = encoder.Encode(livepresence.Success(request.ID, body))
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
	listener, err := net.Listen("unix", matrixTestSocket(t))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, missing := range []string{"agent-sessions", "tmux", "socket", "tmux-list"} {
		t.Run("missing "+missing, func(t *testing.T) {
			base, bin := t.TempDir(), t.TempDir()
			for _, name := range []string{"agent-sessions", "tmux"} {
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
			arguments := []string{"--standing-matrix", "--repository-root", repositoryRoot(t), "--cwd", cwd, "--temporary-root", temporary}
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

func TestMatrixDeliveryRequiresTheRenderedSourceEnvelope(t *testing.T) {
	const runID = "1788480387-3a1ce53dd5a8"
	seen := map[string]bool{}
	for _, cell := range []string{"03", "04", "05"} {
		token := matrixReceiptToken(runID, cell)
		message := matrixDeliveryMessage(token)
		if message != "Matrix delivery "+token+"." || strings.Contains(message, "\n") {
			t.Fatalf("delivery message = %q", message)
		}
		for _, product := range matrixProductInventory() {
			source := matrixEnvelopeSource(token, product.id)
			if seen[source] || len(source) > 24 {
				t.Fatalf("source is not compact and unique: %q", source)
			}
			seen[source] = true
			marker := matrixEnvelopeMarker(product, source)
			if !paneHasMarkerThenReady("chrome\n"+marker+"\ncollapsed body", marker, "", "") {
				t.Fatalf("%s envelope marker did not match", product.id)
			}
			if paneHasMarkerThenReady("assistant: "+token+"\n"+message, marker, "", "") {
				t.Fatalf("%s accepted a model echo without the source envelope", product.id)
			}
		}
	}
}

func TestMatrixQwenQuotaIsAnOwnerEnvironmentFailure(t *testing.T) {
	const pane = "✕ Quota exhausted: Your token-plan 1-week quota has been\n" +
		"exhausted. (cause: insufficient_quota: 429 Your token-plan 1-week quota has been exhausted.) (Press Ctrl+Y to retry)\n" +
		"Auto mode   Type your message"
	wantVerbatim := "Quota exhausted: Your token-plan 1-week quota has been\n" +
		"exhausted. (cause: insufficient_quota: 429 Your token-plan 1-week quota has been exhausted.) (Press Ctrl+Y to retry)"
	products := matrixProductInventory()
	detail := matrixOwnerEnvironmentDetail(products[3], pane)
	if detail != "OWNER-ENVIRONMENT quota: "+wantVerbatim {
		t.Fatalf("quota detail = %q", detail)
	}
	for _, test := range []struct {
		product matrixProduct
		pane    string
	}{
		{products[0], pane},
		{products[3], "Quota exhausted without a 429"},
		{products[3], "insufficient_quota: 429 without product heading"},
		{products[3], "Quota exhausted: unrelated (Press Ctrl+Y to retry)\ninsufficient_quota: 429 outside the quota block"},
	} {
		if got := matrixOwnerEnvironmentDetail(test.product, test.pane); got != "" {
			t.Fatalf("unexpected classification for %s/%q: %q", test.product.id, test.pane, got)
		}
	}
}

func TestMatrixCodexSubmitDelayAndCancellation(t *testing.T) {
	log, fake := filepath.Join(t.TempDir(), "keys"), filepath.Join(t.TempDir(), "tmux")
	writeMatrixTestExecutable(t, fake, `[ "$1" = has-session ] && exit 0; printf '%s\n' "$*" >>"$MATRIX_KEYS"`)
	t.Setenv("MATRIX_KEYS", log)
	runner := matrixRunner{config: matrixOptions{tmux: fake}}
	tui := &matrixTUI{tmuxName: "owned", pane: "owned:0.0", submitDelay: matrixProductInventory()[0].nativeSubmitDelay}
	if started := time.Now(); runner.sendTUIInput(context.Background(), tui, "codex-complete") != nil || time.Since(started) < codexTUISubmitDelay {
		t.Fatalf("completed Codex send took %s", time.Since(started))
	}
	_ = os.Truncate(log, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := runner.sendTUIInput(ctx, tui, "codex-input")
	cancel()
	if body, _ := os.ReadFile(log); err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) || len(strings.Split(strings.TrimSpace(string(body)), "\n")) != 1 {
		t.Fatalf("cancelled Codex send = %v, calls %q", err, body)
	}
	tui.submitDelay = matrixProductInventory()[1].nativeSubmitDelay
	if err := runner.sendTUIInput(context.Background(), tui, "claude-input"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(log)
	if calls := strings.Split(strings.TrimSpace(string(body)), "\n"); len(calls) != 3 || !strings.HasSuffix(calls[2], " Enter") {
		t.Fatalf("non-Codex calls = %q", body)
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

func matrixTestSocket(t *testing.T) string {
	id, err := matrixUUID()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(shortTemporaryRoot(), "mx-"+id+".sock")
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
			for _, name := range []string{"codex", "codex-peer"} {
				writeMatrixTestExecutable(t, filepath.Join(bin, name), "exit 0")
			}
			cwd, temporary, evidence := filepath.Join(base, "cwd"), filepath.Join(base, "temporary"), filepath.Join(base, "evidence")
			for _, path := range []string{cwd, temporary, evidence} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("unix", matrixTestSocket(t))
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
