package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

const productionSupervisorMaintenanceVersion = "test-supervisor-maintenance-window"

func TestMain(main *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == "supervisor" && os.Args[2] == "run" &&
		os.Args[3] == "--plugin-version" && os.Args[4] == productionSupervisorMaintenanceVersion {
		os.Exit(runProductionSupervisorMaintenanceHelper())
	}
	os.Exit(main.Run())
}

func TestProductionSupervisorMustBeOperatorStoppedAndStaleSocketIsRetirable(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "asmig-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dataRoot := filepath.Join(root, "legacy-state", "claude-code-peer")
	agentsRoot := filepath.Join(root, "legacy-state", "agent-sessions", "agents")
	runtimeRoot := filepath.Join(root, "runtime", "ccp")
	profileKey := "non-default-profile"
	statePath := filepath.Join(dataRoot, "profiles", profileKey, "supervisor.json")
	controlSocket := filepath.Join(runtimeRoot, "supervisor-"+profileKey+".sock")
	appServerSocket := filepath.Join(root, "profiles", profileKey, "app-server-control", "app-server-control.sock")
	for _, directory := range []string{filepath.Dir(statePath), agentsRoot, runtimeRoot, filepath.Dir(appServerSocket)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(root, "legacy-bin", "agent-session-runtime")
	copyProductionSupervisorMaintenanceExecutable(t, executable)
	command := exec.Command(executable, "supervisor", "run", "--plugin-version", productionSupervisorMaintenanceVersion)
	command.Env = []string{
		"CLAUDE_PEER_DATA_DIR=" + dataRoot,
		"CLAUDE_PEER_SUPERVISOR_SOCKET=" + controlSocket,
		"CLAUDE_PEER_APP_SERVER_SOCKET=" + appServerSocket,
		"CLAUDE_PEER_PROFILE_KEY=" + profileKey,
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	t.Cleanup(func() {
		if productionSupervisorMaintenanceResponsive(controlSocket) {
			stopProductionSupervisorMaintenanceProcess(controlSocket)
		}
		select {
		case <-processDone:
		case <-time.After(time.Second):
			_ = command.Process.Kill()
		}
	})
	waitProductionSupervisorMaintenanceStatus(t, controlSocket, true)

	sources := []LegacyInventorySource{
		{ID: "bridge-state", Kind: "state", Path: dataRoot, Target: true, MaxDepth: 5},
		{ID: "host-agent-state", Kind: "state", Path: agentsRoot, Target: true, MaxDepth: 4},
		{ID: "bridge-runtime", Kind: "runtime", Path: runtimeRoot, Target: true, MaxDepth: 3},
	}
	live, err := collectProductionFirstMigration(ctx, sources, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	liveCandidate := findLegacyCandidate(t, live.Candidates, "legacy-supervisor-"+profileKey)
	if liveCandidate.Classification != LegacyClassificationActiveManagedBlocker {
		t.Fatalf("responsive supervisor classification = %q", liveCandidate.Classification)
	}
	if _, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: live.Candidates}); err == nil {
		t.Fatal("responsive legacy supervisor did not block unified admission")
	}
	if !productionSupervisorMaintenanceResponsive(controlSocket) {
		t.Fatal("read-only inspection signalled the live legacy supervisor")
	}

	stopProductionSupervisorMaintenanceProcess(controlSocket)
	waitProductionSupervisorMaintenanceStatus(t, controlSocket, false)
	select {
	case err := <-processDone:
		if err != nil {
			t.Fatalf("operator-stopped supervisor exited: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operator-stopped supervisor did not exit")
	}
	if info, err := os.Lstat(controlSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected stable stale socket, info=%v err=%v", info, err)
	}

	stopped, err := collectProductionFirstMigration(ctx, sources, time.Now().Add(time.Second).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	staleCandidate := findLegacyCandidate(t, stopped.Candidates, "legacy-supervisor-"+profileKey)
	if staleCandidate.Classification != LegacyClassificationStale || staleCandidate.EndpointStatus != "absent" ||
		staleCandidate.SourcePath != statePath {
		t.Fatalf("operator-stopped supervisor = %+v", staleCandidate)
	}
	report, err := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: stopped.Candidates})
	if err != nil || !report.LegacyAbsenceVerified || len(report.Blockers) != 0 || len(report.Debt) != 0 {
		t.Fatalf("operator-stopped supervisor quiescence = %+v, %v", report, err)
	}
	if stopped.PriorAuthority.Candidate.CandidateID != "" {
		t.Fatalf("operator-stopped migration fabricated prior authority: %+v", stopped.PriorAuthority)
	}
}

func findLegacyCandidate(t *testing.T, candidates []LegacyRuntimeCandidate, candidateID string) LegacyRuntimeCandidate {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.CandidateID == candidateID {
			return candidate
		}
	}
	t.Fatalf("candidate %q absent from %+v", candidateID, candidates)
	return LegacyRuntimeCandidate{}
}

func runProductionSupervisorMaintenanceHelper() int {
	dataRoot := os.Getenv("CLAUDE_PEER_DATA_DIR")
	controlSocket := os.Getenv("CLAUDE_PEER_SUPERVISOR_SOCKET")
	appServerSocket := os.Getenv("CLAUDE_PEER_APP_SERVER_SOCKET")
	profileKey := os.Getenv("CLAUDE_PEER_PROFILE_KEY")
	statePath := filepath.Join(dataRoot, "profiles", profileKey, "supervisor.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return 2
	}
	listener, err := net.Listen("unix", controlSocket)
	if err != nil {
		return 3
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	defer func() { _ = listener.Close() }()
	process := procinfo.Read(os.Getpid())
	executable, err := os.Executable()
	if err != nil || process.Status != procinfo.Known {
		return 4
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		return 5
	}
	digest := sha256.Sum256(body)
	runtimeIdentity := "sha256:" + hex.EncodeToString(digest[:])
	state := productionLegacySupervisorState{
		AppServerSocket: appServerSocket, ControlSocket: controlSocket, Implementation: "go",
		PID: os.Getpid(), PluginVersion: productionSupervisorMaintenanceVersion, ProcStart: process.Start,
		ProfileKey: profileKey, RuntimeIdentity: runtimeIdentity, StartedAt: time.Now().UnixMilli(),
	}
	stateBody, err := json.Marshal(state)
	if err != nil || os.WriteFile(statePath, stateBody, 0o600) != nil {
		return 6
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			return 7
		}
		requestBody, err := bufio.NewReaderSize(connection, 64<<10).ReadBytes('\n')
		var request map[string]any
		if err != nil || json.Unmarshal(requestBody, &request) != nil {
			_ = connection.Close()
			continue
		}
		response := map[string]any{
			"ok": true, "ready": true, "pid": os.Getpid(), "procStart": process.Start,
			"pluginVersion": productionSupervisorMaintenanceVersion, "runtimeIdentity": runtimeIdentity,
			"implementation": "go", "controlSocket": controlSocket, "appServerSocket": appServerSocket,
			"appServerConnected": false, "appServerPid": 0, "appServerProcStart": "", "shimCount": 0,
		}
		_ = json.NewEncoder(connection).Encode(response)
		_ = connection.Close()
		if request["action"] == "stop" {
			return 0
		}
	}
}

func copyProductionSupervisorMaintenanceExecutable(t *testing.T, target string) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, body, 0o700); err != nil {
		t.Fatal(err)
	}
}

func waitProductionSupervisorMaintenanceStatus(t *testing.T, socket string, responsive bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if productionSupervisorMaintenanceResponsive(socket) == responsive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("supervisor responsive=%t did not become %t", productionSupervisorMaintenanceResponsive(socket), responsive)
}

func productionSupervisorMaintenanceResponsive(socket string) bool {
	connection, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = connection.Write([]byte("{\"action\":\"status\"}\n"))
	var response struct {
		OK bool `json:"ok"`
	}
	return json.NewDecoder(connection).Decode(&response) == nil && response.OK
}

func stopProductionSupervisorMaintenanceProcess(socket string) {
	connection, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
	if err != nil {
		return
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = connection.Write([]byte("{\"action\":\"stop\"}\n"))
	_, _ = bufio.NewReader(connection).ReadBytes('\n')
}
