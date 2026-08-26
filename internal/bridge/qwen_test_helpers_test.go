package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	qwenTestFakeProcessEnv = "AGENT_SESSIONS_TEST_QWEN_PROCESS"
	qwenTestRecordEnv      = "AGENT_SESSIONS_TEST_QWEN_RECORD"
	qwenTestReadyEnv       = "AGENT_SESSIONS_TEST_QWEN_READY"
	qwenTestStopEnv        = "AGENT_SESSIONS_TEST_QWEN_STOP"
	qwenTestSessionIDEnv   = "AGENT_SESSIONS_TEST_QWEN_SESSION_ID"
	qwenTestVersionEnv     = "AGENT_SESSIONS_TEST_QWEN_VERSION"
	qwenTestExitAfterStart = "AGENT_SESSIONS_TEST_QWEN_EXIT_AFTER_START"
)

const (
	qwenTestPollInterval = 5 * time.Millisecond
	// Match the product's bounded lifecycle operations while allowing
	// race-instrumented subprocess and filesystem transitions on loaded hosts.
	// Polls and event waits return immediately once the transition completes.
	qwenTestLifecycleTimeout = 10 * time.Second
)

type qwenTestPrivatePaths struct {
	Root       string
	Executable string
	Records    string
	Events     string
	Input      string
	Ready      string
	Stop       string
}

// newQwenTestPrivatePaths creates the regular files used by the fake native
// process up front. This mirrors the production contract: the runtime
// directory is private and protocol paths cannot be substituted with FIFOs,
// directories, or symlinks between setup and launch.
func newQwenTestPrivatePaths(t *testing.T) qwenTestPrivatePaths {
	t.Helper()
	root := filepath.Join(t.TempDir(), "qwen-private")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := qwenTestPrivatePaths{
		Root:       root,
		Executable: filepath.Join(root, "qwen"),
		Records:    filepath.Join(root, "records.jsonl"),
		Events:     filepath.Join(root, "events.jsonl"),
		Input:      filepath.Join(root, "input.jsonl"),
		Ready:      filepath.Join(root, "ready"),
		Stop:       filepath.Join(root, "stop"),
	}
	for _, path := range []string{paths.Records, paths.Events, paths.Input} {
		qwenTestCreatePrivateFile(t, path, nil)
	}
	return paths
}

func qwenTestCreatePrivateFile(t *testing.T, path string, body []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		if _, err := file.Write(body); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func qwenTestAssertPrivatePath(t *testing.T, path string, directory bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory {
		t.Fatalf("unsafe Qwen test path %s: mode=%s", path, info.Mode())
	}
	want := os.FileMode(0o600)
	if directory {
		want = 0o700
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("Qwen test path %s mode = %04o, want %04o", path, got, want)
	}
}

type qwenTestRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type qwenTestRPCMessage struct {
	JSONRPC string            `json:"jsonrpc,omitempty"`
	ID      any               `json:"id,omitempty"`
	Method  string            `json:"method,omitempty"`
	Params  json.RawMessage   `json:"params,omitempty"`
	Result  json.RawMessage   `json:"result,omitempty"`
	Error   *qwenTestRPCError `json:"error,omitempty"`
}

// qwenTestRPCPeer is a concurrency-safe newline-delimited JSON-RPC endpoint.
// Keeping one decoder for the lifetime of the stream avoids losing bytes that
// json.Decoder buffered past an individual message.
type qwenTestRPCPeer struct {
	decoder *json.Decoder
	encoder *json.Encoder
	writeMu sync.Mutex
}

func newQwenTestRPCPeer(reader io.Reader, writer io.Writer) *qwenTestRPCPeer {
	return &qwenTestRPCPeer{decoder: json.NewDecoder(reader), encoder: json.NewEncoder(writer)}
}

func (p *qwenTestRPCPeer) read() (qwenTestRPCMessage, error) {
	var message qwenTestRPCMessage
	err := p.decoder.Decode(&message)
	return message, err
}

func (p *qwenTestRPCPeer) write(message any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.encoder.Encode(message)
}

func (p *qwenTestRPCPeer) respond(id, result any) error {
	return p.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (p *qwenTestRPCPeer) respondError(id any, code int, message string) error {
	return p.write(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": qwenTestRPCError{Code: code, Message: message},
	})
}

func (p *qwenTestRPCPeer) notify(method string, params any) error {
	return p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

type qwenTestPollPredicate func() (bool, error)

// qwenTestPoll evaluates predicate immediately and thereafter on a bounded
// ticker. It reports the last transient error on timeout, making lifecycle
// failures actionable without using fixed sleeps.
func qwenTestPoll(t *testing.T, timeout time.Duration, description string, predicate qwenTestPollPredicate) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(qwenTestPollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		ok, err := predicate()
		if ok {
			return
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-deadline.C:
			if lastErr != nil {
				t.Fatalf("timed out waiting for %s: %v", description, lastErr)
			}
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func qwenTestWaitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	qwenTestPoll(t, timeout, "path "+path, func() (bool, error) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("unexpected mode %s", info.Mode())
		}
		return true, nil
	})
}

func qwenTestWaitForProcessStart(t *testing.T, pid int, timeout time.Duration) string {
	t.Helper()
	var start string
	qwenTestPoll(t, timeout, fmt.Sprintf("Qwen process %d identity", pid), func() (bool, error) {
		var err error
		start, err = captureProcessStart(pid)
		return err == nil && start != "", err
	})
	return start
}

func qwenTestWaitForCommand(t *testing.T, done <-chan error, timeout time.Duration) error {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		t.Fatal("timed out waiting for fake Qwen process exit")
		return nil
	}
}

func qwenTestReadJSONL(path string) ([]map[string]any, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(body), "\n")
	rows := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func qwenTestWaitForJSONL(t *testing.T, path string, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	var matched map[string]any
	qwenTestPoll(t, timeout, "matching JSONL record in "+path, func() (bool, error) {
		rows, err := qwenTestReadJSONL(path)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if match(row) {
				matched = row
				return true, nil
			}
		}
		return false, nil
	})
	return matched
}

type fakeQwenProcess struct {
	Paths qwenTestPrivatePaths
}

func newFakeQwenProcess(t *testing.T) fakeQwenProcess {
	t.Helper()
	paths := newQwenTestPrivatePaths(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec " + qwenTestShellQuote(testBinary) + " -test.run='^TestQwenFakeProcess$' -- \"$@\"\n"
	qwenTestCreatePrivateFile(t, paths.Executable, []byte(script))
	if err := os.Chmod(paths.Executable, 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeQwenProcess{Paths: paths}
}

func (f fakeQwenProcess) command(args ...string) *exec.Cmd {
	command := exec.Command(f.Paths.Executable, args...)
	command.Env = qwenTestEnvironment(os.Environ(), map[string]string{
		qwenTestFakeProcessEnv: "1",
		qwenTestRecordEnv:      f.Paths.Records,
		qwenTestReadyEnv:       f.Paths.Ready,
		qwenTestStopEnv:        f.Paths.Stop,
	})
	return command
}

func (f fakeQwenProcess) installEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv(qwenTestFakeProcessEnv, "1")
	t.Setenv(qwenTestRecordEnv, f.Paths.Records)
	t.Setenv(qwenTestReadyEnv, f.Paths.Ready)
	t.Setenv(qwenTestStopEnv, f.Paths.Stop)
}

func TestQwenFoundationHelpers(t *testing.T) {
	fake := newFakeQwenProcess(t)
	fake.installEnvironment(t)
	qwenTestAssertPrivatePath(t, fake.Paths.Root, true)
	qwenTestWaitForPath(t, fake.Paths.Records, time.Second)
	if start := qwenTestWaitForProcessStart(t, os.Getpid(), time.Second); start == "" {
		t.Fatal("current process has no start identity")
	}
	done := make(chan error, 1)
	done <- nil
	if err := qwenTestWaitForCommand(t, done, time.Second); err != nil {
		t.Fatal(err)
	}

	input := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var output bytes.Buffer
	peer := newQwenTestRPCPeer(input, &output)
	message, err := peer.read()
	if err != nil || message.Method != "initialize" {
		t.Fatalf("read Qwen helper RPC = %#v, %v", message, err)
	}
	if err := peer.respond(message.ID, map[string]any{"ready": true}); err != nil {
		t.Fatal(err)
	}
	if err := peer.respondError(message.ID, -32601, "not found"); err != nil {
		t.Fatal(err)
	}
	if err := peer.notify("session/update", map[string]any{"ready": true}); err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(output.Bytes(), []byte("\n")); lines != 3 {
		t.Fatalf("Qwen helper RPC wrote %d lines, want 3", lines)
	}
}

func TestQwenFakeInteractiveExitsWhenTestParentDies(t *testing.T) {
	fake := newFakeQwenProcess(t)
	launcher := strings.Join([]string{
		qwenTestShellQuote(fake.Paths.Executable), "--session-id", "11111111-2222-4333-8444-555555555555",
		">/dev/null 2>&1 & child=$!;",
		"while [ ! -f " + qwenTestShellQuote(fake.Paths.Ready) + " ]; do",
		"kill -0 \"$child\" 2>/dev/null || exit 70; sleep 0.01; done;",
		"printf '%s\\n' \"$child\"",
	}, " ")
	command := exec.Command("/bin/sh", "-c", launcher)
	command.Env = qwenTestEnvironment(os.Environ(), map[string]string{
		qwenTestFakeProcessEnv: "1",
		qwenTestRecordEnv:      fake.Paths.Records,
		qwenTestReadyEnv:       fake.Paths.Ready,
		qwenTestStopEnv:        fake.Paths.Stop,
	})
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 1 {
		t.Fatalf("fake Qwen child PID = %q, %v", body, err)
	}
	process, _ := os.FindProcess(pid)
	t.Cleanup(func() { _ = process.Kill() })
	qwenTestPoll(t, qwenTestLifecycleTimeout, "orphaned fake Qwen helper exit", func() (bool, error) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true, nil
		}
		return false, err
	})
}

func qwenTestEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func qwenTestShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// TestQwenFakeProcess turns the current package test binary into a deterministic
// Qwen stand-in. Tests invoke it through newFakeQwenProcess rather than relying
// on a shell-specific fake installed elsewhere on the machine.
func TestQwenFakeProcess(_ *testing.T) {
	if os.Getenv(qwenTestFakeProcessEnv) != "1" {
		return
	}
	args := qwenFakeNativeArgs(os.Args)
	qwenFakeRecord(map[string]any{
		"kind": "start", "pid": os.Getpid(), "args": args,
		"env": map[string]any{
			"QWEN_HOME":                      os.Getenv("QWEN_HOME"),
			"QWEN_RUNTIME_DIR":               os.Getenv("QWEN_RUNTIME_DIR"),
			"QWEN_CODE_SYSTEM_SETTINGS_PATH": os.Getenv("QWEN_CODE_SYSTEM_SETTINGS_PATH"),
			"QWEN_SANDBOX":                   os.Getenv("QWEN_SANDBOX"),
		},
	})
	if qwenFakeHasArg(args, "--version", "-v") {
		_, _ = fmt.Fprintln(os.Stdout, qwenFakeVersion())
		os.Exit(0)
	}
	if qwenFakeHasArg(args, "--acp") {
		runQwenFakeACP(args)
		os.Exit(0)
	}
	runQwenFakeInteractive(args)
	os.Exit(0)
}

func qwenFakeNativeArgs(args []string) []string {
	for index, argument := range args {
		if argument == "--" {
			return append([]string(nil), args[index+1:]...)
		}
	}
	return nil
}

func qwenFakeHasArg(args []string, wanted ...string) bool {
	for _, argument := range args {
		for _, candidate := range wanted {
			if argument == candidate {
				return true
			}
		}
	}
	return false
}

func qwenFakeArg(args []string, names ...string) string {
	for index, argument := range args {
		for _, name := range names {
			if argument == name && index+1 < len(args) {
				return args[index+1]
			}
			if value, found := strings.CutPrefix(argument, name+"="); found {
				return value
			}
		}
	}
	return ""
}

func qwenFakeVersion() string {
	if version := os.Getenv(qwenTestVersionEnv); version != "" {
		return version
	}
	return "0.21.15"
}

func runQwenFakeACP(args []string) {
	peer := newQwenTestRPCPeer(os.Stdin, os.Stdout)
	for {
		message, err := peer.read()
		if err != nil {
			return
		}
		qwenFakeRecord(map[string]any{"kind": "rpc_request", "message": message})
		if message.Method == "" {
			continue
		}
		sessionID := qwenFakeSessionID(args)
		switch message.Method {
		case "initialize":
			_ = peer.respond(message.ID, map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "qwen-code", "version": qwenFakeVersion()},
				"agentCapabilities": map[string]any{
					"loadSession":         true,
					"sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}},
					"mcpCapabilities":     map[string]any{"stdio": true},
				},
				"authMethods": []any{},
			})
		case "session/new", "session/resume":
			var params map[string]any
			_ = json.Unmarshal(message.Params, &params)
			if supplied, _ := params["sessionId"].(string); supplied != "" {
				sessionID = supplied
			}
			_ = peer.respond(message.ID, map[string]any{
				"sessionId": sessionID,
				"modes": map[string]any{
					"currentModeId": qwenFakeInitialMode(args),
					"availableModes": []any{
						map[string]any{"id": "default"}, map[string]any{"id": "plan"}, map[string]any{"id": "yolo"},
					},
				},
			})
		case "session/prompt":
			var params map[string]any
			_ = json.Unmarshal(message.Params, &params)
			promptBody, _ := json.Marshal(params["prompt"])
			if strings.Contains(string(promptBody), "BLOCK_QWEN_PROMPT") {
				cancel, cancelErr := peer.read()
				if cancelErr != nil || cancel.Method != "session/cancel" || cancel.ID != nil {
					return
				}
				qwenFakeRecord(map[string]any{"kind": "rpc_request", "message": cancel})
				_ = peer.respond(message.ID, map[string]any{"stopReason": "cancelled"})
				continue
			}
			_ = peer.notify("session/update", map[string]any{
				"sessionId": sessionID,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "fake Qwen answer"},
				},
			})
			_ = peer.respond(message.ID, map[string]any{"stopReason": "end_turn"})
		case "session/cancel":
			if message.ID != nil {
				_ = peer.respond(message.ID, map[string]any{})
			}
		case "session/set_mode":
			_ = peer.respond(message.ID, map[string]any{})
		default:
			_ = peer.respondError(message.ID, -32601, "fake Qwen method not found: "+message.Method)
		}
	}
}

func qwenFakeInitialMode(args []string) string {
	if mode := qwenFakeArg(args, "--approval-mode"); mode != "" {
		return mode
	}
	return "default"
}

func runQwenFakeInteractive(args []string) {
	// A killed or interrupted `go test` process must not leave this helper
	// occupying the operator's Qwen profile. The shell wrapper execs this test
	// binary directly, so a parent change is authoritative test-harness death.
	parentPID := os.Getppid()
	writer, err := qwenFakeEventWriter(args)
	if err != nil {
		qwenFakeRecord(map[string]any{"kind": "event_open_error", "error": err.Error()})
		os.Exit(71)
	}
	start := map[string]any{
		"type": "system", "subtype": "session_start", "session_id": qwenFakeSessionID(args),
		"data": map[string]any{
			"session_id": qwenFakeSessionID(args), "cwd": qwenFakeCWD(),
			"protocol_version": 2, "version": qwenFakeVersion(),
			"supported_events": []string{"system", "user", "assistant", "stream_event", "result", "control_request", "control_response"},
		},
	}
	if err := qwenFakeWriteJSON(writer, start); err != nil {
		_ = writer.Close()
		os.Exit(72)
	}
	defer func() { _ = writer.Close() }()
	qwenFakeRecord(map[string]any{"kind": "session_start", "event": start})
	qwenFakeWriteMarker(os.Getenv(qwenTestReadyEnv), "ready\n")
	if os.Getenv(qwenTestExitAfterStart) == "1" {
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ticker := time.NewTicker(qwenTestPollInterval)
	defer ticker.Stop()
	inputPath := qwenFakeArg(args, "--input-file")
	var input qwenFakeInputCursor
	for {
		if os.Getppid() != parentPID {
			return
		}
		if inputPath != "" {
			if records, readErr := input.read(inputPath); readErr == nil {
				for _, record := range records {
					qwenFakeRecord(map[string]any{"kind": "input", "record": record})
					if record["type"] == "submit" {
						text, _ := record["text"].(string)
						_ = qwenFakeWriteJSON(writer, map[string]any{"type": "user", "message": text})
						_ = qwenFakeWriteJSON(writer, map[string]any{"type": "assistant", "message": "fake Qwen answer"})
						_ = qwenFakeWriteJSON(writer, map[string]any{"type": "result", "subtype": "success"})
					}
				}
			}
		}
		if stop := os.Getenv(qwenTestStopEnv); stop != "" {
			if info, statErr := os.Lstat(stop); statErr == nil && info.Mode().IsRegular() {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func qwenFakeSessionID(args []string) string {
	if sessionID := os.Getenv(qwenTestSessionIDEnv); sessionID != "" {
		return sessionID
	}
	if sessionID := qwenFakeArg(args, "--session-id", "--resume", "-r"); sessionID != "" {
		return sessionID
	}
	return "11111111-2222-4333-8444-555555555555"
}

func qwenFakeCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

type qwenFakeWriteCloser struct {
	io.Writer
	close func() error
}

func (w qwenFakeWriteCloser) Close() error { return w.close() }

func qwenFakeEventWriter(args []string) (io.WriteCloser, error) {
	if path := qwenFakeArg(args, "--json-file"); path != "" {
		return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	}
	if rawFD := qwenFakeArg(args, "--json-fd"); rawFD != "" {
		fd, err := strconv.Atoi(rawFD)
		if err != nil || fd < 3 {
			return nil, fmt.Errorf("invalid --json-fd %q", rawFD)
		}
		file := os.NewFile(uintptr(fd), "qwen-json-fd")
		if file == nil {
			return nil, fmt.Errorf("unavailable --json-fd %d", fd)
		}
		return file, nil
	}
	return qwenFakeWriteCloser{Writer: io.Discard, close: func() error { return nil }}, nil
}

func qwenFakeWriteJSON(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = writer.Write(body)
	return err
}

type qwenFakeInputCursor struct {
	info   os.FileInfo
	offset int
}

func (c *qwenFakeInputCursor) read(path string) ([]map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Qwen input path is not regular: %s", info.Mode())
	}
	if c.info == nil || !os.SameFile(c.info, info) || int(info.Size()) < c.offset {
		c.offset = 0
	}
	c.info = info
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if c.offset > len(body) {
		c.offset = 0
	}
	tail := body[c.offset:]
	lastNewline := strings.LastIndexByte(string(tail), '\n')
	if lastNewline < 0 {
		return nil, nil
	}
	complete := tail[:lastNewline]
	c.offset += lastNewline + 1
	var records []map[string]any
	for _, line := range strings.Split(string(complete), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

var qwenFakeRecordMu sync.Mutex

func qwenFakeRecord(value any) {
	path := os.Getenv(qwenTestRecordEnv)
	if path == "" {
		return
	}
	qwenFakeRecordMu.Lock()
	defer qwenFakeRecordMu.Unlock()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_ = json.NewEncoder(file).Encode(value)
}

func qwenFakeWriteMarker(path, value string) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = io.WriteString(file, value)
	_ = file.Close()
}

func TestQwenTestHelpersFakeACP(t *testing.T) {
	fake := newFakeQwenProcess(t)
	command := fake.command("--acp")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	peer := newQwenTestRPCPeer(stdout, stdin)
	if err := peer.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	response, err := peer.read()
	if err != nil || response.ID == nil || len(response.Result) == 0 {
		t.Fatalf("fake Qwen initialize response = %+v, %v", response, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := qwenTestWaitForCommand(t, done, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	qwenTestWaitForJSONL(t, fake.Paths.Records, time.Second, func(row map[string]any) bool {
		return row["kind"] == "rpc_request"
	})
}

func TestQwenTestHelpersPrivatePaths(t *testing.T) {
	paths := newQwenTestPrivatePaths(t)
	qwenTestAssertPrivatePath(t, paths.Root, true)
	for _, path := range []string{paths.Records, paths.Events, paths.Input} {
		qwenTestAssertPrivatePath(t, path, false)
	}
}
