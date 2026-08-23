package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const (
	qwenArchiveHelperTestEnv   = "AGENT_SESSIONS_QWEN_ARCHIVE_HELPER_TEST"
	qwenArchiveHelperRecordEnv = "AGENT_SESSIONS_QWEN_ARCHIVE_HELPER_RECORD"
)

func TestQwenArchiveServeHelper(_ *testing.T) {
	if os.Getenv(qwenArchiveHelperTestEnv) != "1" {
		return
	}
	args := qwenFakeNativeArgs(os.Args)
	workspace := qwenFakeArg(args, "--workspace")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(71)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		os.Exit(72)
	}
	record := map[string]any{"childPid": child.Process.Pid, "childStart": readProcStart(child.Process.Pid), "workspace": workspace}
	if body, err := json.Marshal(record); err != nil || os.WriteFile(os.Getenv(qwenArchiveHelperRecordEnv), body, 0o600) != nil {
		os.Exit(73)
	}
	token := os.Getenv("QWEN_SERVER_TOKEN")
	authorized := func(request *http.Request) bool {
		return request.Header.Get("Authorization") == "Bearer "+token && token != ""
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/capabilities", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"protocolVersions": map[string]any{"current": "v1"}, "features": []string{"session_archive"}, "workspaceCwd": workspace,
		})
	})
	mux.HandleFunc("/sessions/archive", func(writer http.ResponseWriter, request *http.Request) {
		qwenArchiveHelperResponse(writer, request, authorized, "archived")
	})
	mux.HandleFunc("/sessions/unarchive", func(writer http.ResponseWriter, request *http.Request) {
		qwenArchiveHelperResponse(writer, request, authorized, "unarchived")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	_, _ = fmt.Fprintf(os.Stdout, "qwen serve listening on http://127.0.0.1:%d\n", listener.Addr().(*net.TCPAddr).Port)
	select {}
}

func qwenArchiveHelperResponse(writer http.ResponseWriter, request *http.Request, authorized func(*http.Request) bool, field string) {
	if !authorized(request) {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		SessionIDs []string `json:"sessionIds"`
	}
	if json.NewDecoder(request.Body).Decode(&body) != nil || len(body.SessionIDs) != 1 {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{field: body.SessionIDs})
}

func TestQwenArchiveTransactionAuthenticatesExactUUIDAndRetiresHelperTree(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"home", "runtime", "workspace"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QWEN_HOME", filepath.Join(root, "home"))
	t.Setenv("QWEN_RUNTIME_DIR", filepath.Join(root, "runtime"))
	profile, err := qwenprofile.Current()
	if err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "qwen")
	script := "#!/bin/sh\nexec " + qwenTestShellQuote(testBinary) + " -test.run='^TestQwenArchiveServeHelper$' -- \"$@\"\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(root, "helper.json")
	t.Setenv(qwenLaneExecutableEnv, executable)
	t.Setenv(qwenArchiveHelperTestEnv, "1")
	t.Setenv(qwenArchiveHelperRecordEnv, recordPath)
	state := qwenLaneState{QwenSessionID: randomID(), Cwd: filepath.Join(root, "workspace"), Profile: profile}
	for _, operation := range []string{"archive", "unarchive"} {
		if err := runQwenArchiveTransaction(state, operation); err != nil {
			t.Fatalf("%s Qwen session: %v", operation, err)
		}
		body, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if json.Unmarshal(body, &record) != nil || stringValue(record["workspace"]) != state.Cwd {
			t.Fatalf("helper record = %#v", record)
		}
		pid, start := intValue(record["childPid"]), stringValue(record["childStart"])
		if !waitForCondition(3*time.Second, func() bool { return !exactProcessIdentityMatch(pid, start) }) {
			t.Fatalf("Qwen archive helper child %d survived exact process-group retirement", pid)
		}
	}
}

func TestQwenArchiveCapabilitiesAndResultsFailClosed(t *testing.T) {
	token := "test-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"protocolVersions": map[string]any{"current": "v1"}, "features": []string{"session_archive"}, "workspaceCwd": "/workspace",
		})
	}))
	defer server.Close()
	if err := qwenValidateArchiveCapabilities(context.Background(), server.URL, token, "/workspace"); err != nil {
		t.Fatal(err)
	}
	if err := qwenValidateArchiveCapabilities(context.Background(), server.URL, "wrong", "/workspace"); err == nil {
		t.Fatal("unauthenticated Qwen archive capabilities were accepted")
	}
	id := randomID()
	if !exactSingleQwenArchiveResult([]string{id}, nil, id) || !exactSingleQwenArchiveResult(nil, []string{id}, id) {
		t.Fatal("exact Qwen archive idempotence result was rejected")
	}
	for _, result := range []struct{ success, already []string }{
		{nil, nil}, {[]string{id, randomID()}, nil}, {[]string{id}, []string{id}}, {[]string{randomID()}, nil},
	} {
		if exactSingleQwenArchiveResult(result.success, result.already, id) {
			t.Fatalf("ambiguous Qwen archive result accepted: %+v", result)
		}
	}
}

func TestQwenArchiveEnvironmentUsesExactProfileAndStripsManagedCapabilities(t *testing.T) {
	profile := qwenprofile.Identity{QwenHomeSet: true, QwenHome: "/qwen/home", QwenRuntimeSet: true, QwenRuntimeDir: "/qwen/runtime", Fingerprint: strings.Repeat("a", 64)}
	environment := qwenArchiveEnvironment([]string{
		"PATH=/bin", "QWEN_HOME=/wrong", "QWEN_RUNTIME_DIR=/wrong", "QWEN_SERVER_TOKEN=old",
		"AGENT_SESSIONS_QWEN_CAPABILITY=secret", "AGENT_SESSIONS_SESSION_ID=session", "AGENT_SESSIONS_PRODUCT=qwen",
	}, profile, "fresh-token")
	joined := strings.Join(environment, "\n")
	for _, wanted := range []string{"QWEN_HOME=/qwen/home", "QWEN_RUNTIME_DIR=/qwen/runtime", "QWEN_SERVER_TOKEN=fresh-token"} {
		if strings.Count(joined, wanted) != 1 {
			t.Fatalf("archive environment missing exact %s: %v", wanted, environment)
		}
	}
	for _, forbidden := range []string{"QWEN_HOME=/wrong", "QWEN_RUNTIME_DIR=/wrong", "QWEN_SERVER_TOKEN=old", "AGENT_SESSIONS_QWEN_CAPABILITY", "AGENT_SESSIONS_SESSION_ID", "AGENT_SESSIONS_PRODUCT"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("archive environment retained %s: %v", forbidden, environment)
		}
	}
}

func TestQwenResumeCompensationRearchivesOrRetainsTypedDebt(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "data"), runtimeDir: filepath.Join(root, "runtime"),
		claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex"),
	}
	prior := qwenLaneState{
		Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
		Name: "resume-compensation", ThreadID: randomID(), QwenSessionID: randomID(), Cwd: root,
		Status: "archived", NativeArchiveState: "archived", LaunchPreference: "native_default",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	attempted := cloneQwenLaneState(prior)
	attempted.Status, attempted.NativeArchiveState, attempted.StartupID = "starting", "active", randomID()
	attempted.Turns = append(attempted.Turns, newQwenLaneTurn("resume", 0))
	attempted.PendingTurnIDs = []string{attempted.Turns[0].ID}
	if err := writeQwenLaneState(paths, attempted); err != nil {
		t.Fatal(err)
	}

	originalArchive := executeQwenArchiveTransaction
	t.Cleanup(func() { executeQwenArchiveTransaction = originalArchive })
	resumeErr := errors.New("manager failed before readiness")
	executeQwenArchiveTransaction = func(candidate qwenLaneState, operation string) error {
		if candidate.ThreadID != prior.ThreadID || operation != "archive" {
			t.Fatalf("compensation = %s %s", candidate.ThreadID, operation)
		}
		return nil
	}
	if err := compensateQwenLaneResume(paths, prior, attempted, resumeErr); !errors.Is(err, resumeErr) {
		t.Fatalf("compensation error = %v", err)
	}
	restored, err := readQwenLaneState(paths, prior.ThreadID)
	if err != nil || restored.Status != "archived" || restored.NativeArchiveState != "archived" ||
		len(restored.Turns) != 0 || len(restored.CleanupDebt) != 0 {
		t.Fatalf("restored archive = %+v, %v", restored, err)
	}

	if err := writeQwenLaneState(paths, attempted); err != nil {
		t.Fatal(err)
	}
	compensationErr := errors.New("native archive unavailable")
	executeQwenArchiveTransaction = func(qwenLaneState, string) error { return compensationErr }
	if err := compensateQwenLaneResume(paths, prior, attempted, resumeErr); !errors.Is(err, resumeErr) || !errors.Is(err, compensationErr) {
		t.Fatalf("failed compensation error = %v", err)
	}
	debt, err := readQwenLaneState(paths, prior.ThreadID)
	if err != nil || debt.Status != "cleanup_debt" || debt.NativeArchiveState != "unknown" ||
		len(debt.CleanupDebt) != 1 || debt.CleanupDebt[0].Operation != "resume_compensation" {
		t.Fatalf("resume compensation debt = %+v, %v", debt, err)
	}
}
