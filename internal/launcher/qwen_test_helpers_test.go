package launcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

const (
	qwenLauncherFakeProcessEnv = "AGENT_SESSIONS_TEST_QWEN_LAUNCHER_PROCESS"
	qwenLauncherRecordEnv      = "AGENT_SESSIONS_TEST_QWEN_LAUNCHER_RECORD"
	qwenLauncherReadyEnv       = "AGENT_SESSIONS_TEST_QWEN_LAUNCHER_READY"
	qwenLauncherStopEnv        = "AGENT_SESSIONS_TEST_QWEN_LAUNCHER_STOP"
)

const qwenLauncherPollInterval = 5 * time.Millisecond

type qwenLauncherTestPaths struct {
	Root       string
	Executable string
	Records    string
	Events     string
	Input      string
	Ready      string
	Stop       string
}

func newQwenLauncherTestPaths(t *testing.T) qwenLauncherTestPaths {
	t.Helper()
	root := filepath.Join(t.TempDir(), "qwen-private")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := qwenLauncherTestPaths{
		Root: root, Executable: filepath.Join(root, "qwen"),
		Records: filepath.Join(root, "records.jsonl"), Events: filepath.Join(root, "events.jsonl"),
		Input: filepath.Join(root, "input.jsonl"), Ready: filepath.Join(root, "ready"), Stop: filepath.Join(root, "stop"),
	}
	for _, path := range []string{paths.Records, paths.Events, paths.Input} {
		qwenLauncherCreatePrivateFile(t, path, nil)
	}
	return paths
}

func qwenLauncherCreatePrivateFile(t *testing.T, path string, body []byte) {
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

func qwenLauncherAssertPrivatePath(t *testing.T, path string, directory bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := os.FileMode(0o600)
	if directory {
		want = 0o700
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || info.Mode().Perm() != want {
		t.Fatalf("unsafe Qwen launcher test path %s: mode=%s", path, info.Mode())
	}
}

type qwenLauncherRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type qwenLauncherRPCMessage struct {
	JSONRPC string                `json:"jsonrpc,omitempty"`
	ID      any                   `json:"id,omitempty"`
	Method  string                `json:"method,omitempty"`
	Params  json.RawMessage       `json:"params,omitempty"`
	Result  json.RawMessage       `json:"result,omitempty"`
	Error   *qwenLauncherRPCError `json:"error,omitempty"`
}

type qwenLauncherRPCPeer struct {
	decoder *json.Decoder
	encoder *json.Encoder
	writeMu sync.Mutex
}

func newQwenLauncherRPCPeer(reader io.Reader, writer io.Writer) *qwenLauncherRPCPeer {
	return &qwenLauncherRPCPeer{decoder: json.NewDecoder(reader), encoder: json.NewEncoder(writer)}
}

func (p *qwenLauncherRPCPeer) read() (qwenLauncherRPCMessage, error) {
	var message qwenLauncherRPCMessage
	err := p.decoder.Decode(&message)
	return message, err
}

func (p *qwenLauncherRPCPeer) write(message any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.encoder.Encode(message)
}

func (p *qwenLauncherRPCPeer) respond(id, result any) error {
	return p.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (p *qwenLauncherRPCPeer) respondError(id any, code int, message string) error {
	return p.write(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": qwenLauncherRPCError{Code: code, Message: message},
	})
}

func (p *qwenLauncherRPCPeer) notify(method string, params any) error {
	return p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

type qwenLauncherPollPredicate func() (bool, error)

func qwenLauncherPoll(t *testing.T, timeout time.Duration, description string, predicate qwenLauncherPollPredicate) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(qwenLauncherPollInterval)
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
		case <-timer.C:
			if lastErr != nil {
				t.Fatalf("timed out waiting for %s: %v", description, lastErr)
			}
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func qwenLauncherWaitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	qwenLauncherPoll(t, timeout, "path "+path, func() (bool, error) {
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

func qwenLauncherWaitForProcessStart(t *testing.T, pid int, timeout time.Duration) string {
	t.Helper()
	var start string
	qwenLauncherPoll(t, timeout, fmt.Sprintf("Qwen process %d identity", pid), func() (bool, error) {
		start = procinfo.Read(pid).Start
		return start != "", nil
	})
	return start
}

func qwenLauncherWaitForCommand(t *testing.T, done <-chan error, timeout time.Duration) error {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		t.Fatal("timed out waiting for fake Qwen launcher process exit")
		return nil
	}
}

func qwenLauncherReadJSONL(path string) ([]map[string]any, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	for _, line := range strings.Split(string(body), "\n") {
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

func qwenLauncherWaitForJSONL(t *testing.T, path string, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	var matched map[string]any
	qwenLauncherPoll(t, timeout, "matching JSONL record in "+path, func() (bool, error) {
		rows, err := qwenLauncherReadJSONL(path)
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

type fakeQwenLauncherProcess struct {
	Paths qwenLauncherTestPaths
}

func newFakeQwenLauncherProcess(t *testing.T) fakeQwenLauncherProcess {
	t.Helper()
	paths := newQwenLauncherTestPaths(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec " + qwenLauncherShellQuote(testBinary) + " -test.run='^TestQwenLauncherFakeProcess$' -- \"$@\"\n"
	qwenLauncherCreatePrivateFile(t, paths.Executable, []byte(script))
	if err := os.Chmod(paths.Executable, 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeQwenLauncherProcess{Paths: paths}
}

func (f fakeQwenLauncherProcess) command(args ...string) *exec.Cmd {
	command := exec.Command(f.Paths.Executable, args...)
	command.Env = qwenLauncherEnvironment(os.Environ(), map[string]string{
		qwenLauncherFakeProcessEnv: "1", qwenLauncherRecordEnv: f.Paths.Records,
		qwenLauncherReadyEnv: f.Paths.Ready, qwenLauncherStopEnv: f.Paths.Stop,
	})
	return command
}

func (f fakeQwenLauncherProcess) installEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv(qwenLauncherFakeProcessEnv, "1")
	t.Setenv(qwenLauncherRecordEnv, f.Paths.Records)
	t.Setenv(qwenLauncherReadyEnv, f.Paths.Ready)
	t.Setenv(qwenLauncherStopEnv, f.Paths.Stop)
}

func TestQwenLauncherFoundationHelpers(t *testing.T) {
	fake := newFakeQwenLauncherProcess(t)
	fake.installEnvironment(t)
	qwenLauncherAssertPrivatePath(t, fake.Paths.Root, true)
	qwenLauncherWaitForPath(t, fake.Paths.Records, time.Second)
	if start := qwenLauncherWaitForProcessStart(t, os.Getpid(), time.Second); start == "" {
		t.Fatal("current process has no start identity")
	}
	done := make(chan error, 1)
	done <- nil
	if err := qwenLauncherWaitForCommand(t, done, time.Second); err != nil {
		t.Fatal(err)
	}

	input := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var output bytes.Buffer
	peer := newQwenLauncherRPCPeer(input, &output)
	message, err := peer.read()
	if err != nil || message.Method != "initialize" {
		t.Fatalf("read Qwen launcher helper RPC = %#v, %v", message, err)
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
		t.Fatalf("Qwen launcher helper RPC wrote %d lines, want 3", lines)
	}
}

func qwenLauncherEnvironment(base []string, overrides map[string]string) []string {
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

func qwenLauncherShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestQwenLauncherFakeProcess(_ *testing.T) {
	if os.Getenv(qwenLauncherFakeProcessEnv) != "1" {
		return
	}
	args := qwenLauncherNativeArgs(os.Args)
	qwenLauncherRecord(map[string]any{
		"kind": "start", "pid": os.Getpid(), "args": args,
		"env": map[string]any{
			"QWEN_HOME": os.Getenv("QWEN_HOME"), "QWEN_RUNTIME_DIR": os.Getenv("QWEN_RUNTIME_DIR"),
			"QWEN_CODE_SYSTEM_SETTINGS_PATH": os.Getenv("QWEN_CODE_SYSTEM_SETTINGS_PATH"),
			"QWEN_SANDBOX":                   os.Getenv("QWEN_SANDBOX"),
		},
	})
	for _, argument := range args {
		if argument == "--version" || argument == "-v" {
			_, _ = fmt.Fprintln(os.Stdout, "0.21.15")
			os.Exit(0)
		}
	}
	qwenLauncherWriteMarker(os.Getenv(qwenLauncherReadyEnv), "ready\n")
	stop := os.Getenv(qwenLauncherStopEnv)
	if stop == "" {
		return
	}
	ticker := time.NewTicker(qwenLauncherPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		if info, err := os.Lstat(stop); err == nil && info.Mode().IsRegular() {
			return
		}
	}
}

func qwenLauncherNativeArgs(args []string) []string {
	for index, argument := range args {
		if argument == "--" {
			return append([]string(nil), args[index+1:]...)
		}
	}
	return nil
}

var qwenLauncherRecordMu sync.Mutex

func qwenLauncherRecord(value any) {
	path := os.Getenv(qwenLauncherRecordEnv)
	if path == "" {
		return
	}
	qwenLauncherRecordMu.Lock()
	defer qwenLauncherRecordMu.Unlock()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_ = json.NewEncoder(file).Encode(value)
}

func qwenLauncherWriteMarker(path, value string) {
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

func TestQwenLauncherTestHelpers(t *testing.T) {
	fake := newFakeQwenLauncherProcess(t)
	qwenLauncherAssertPrivatePath(t, fake.Paths.Root, true)
	command := fake.command("--version")
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != "0.21.15" {
		t.Fatalf("fake Qwen --version = %q, %v", output, err)
	}
	row := qwenLauncherWaitForJSONL(t, fake.Paths.Records, time.Second, func(row map[string]any) bool {
		return row["kind"] == "start"
	})
	args, _ := row["args"].([]any)
	if len(args) != 1 || args[0] != "--version" {
		t.Fatalf("recorded fake Qwen argv = %#v", row["args"])
	}
}
