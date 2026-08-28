//go:build linux || darwin

package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

func TestQwenLaneForcedArchiveReapsWorkerAndDetachedToolRoots(t *testing.T) {
	paths, state, worker, tool := prepareCrashedQwenLane(t, true)
	originalArchive := executeQwenArchiveTransaction
	executeQwenArchiveTransaction = func(candidate qwenLaneState, operation string) error {
		if candidate.QwenSessionID != state.QwenSessionID || operation != "archive" {
			t.Fatalf("archive transaction = %q %q", candidate.QwenSessionID, operation)
		}
		return nil
	}
	t.Cleanup(func() { executeQwenArchiveTransaction = originalArchive })
	if err := forceArchiveQwenLane(paths, state.ThreadID); err != nil {
		t.Fatal(err)
	}
	for label, process := range map[string]*grokManagedProcess{"worker": worker, "tool": tool} {
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s process survived forced archive", label)
		}
	}
	archived, err := readQwenLaneState(paths, state.ThreadID)
	if err != nil || archived.Status != "archived" || archived.NativeArchiveState != "archived" ||
		archived.ManagerPID != 0 || archived.WorkerPID != 0 || archived.ToolRegistryVersion != 0 || len(archived.CleanupDebt) != 0 {
		t.Fatalf("forced archive state = %+v, %v", archived, err)
	}
	registry, _ := qwenToolRegistryPathsForState(state)
	if _, err := os.Lstat(registry.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Qwen tool registry survived forced archive: %v", err)
	}
}

func TestQwenLaneForcedArchivePreservesRecycledWorkerPID(t *testing.T) {
	paths, state, worker, _ := prepareCrashedQwenLane(t, false)
	state.WorkerProcStart, state.WorkerStrongStart = "recycled-start", "recycled-strong-start"
	if err := writeQwenLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	registry, err := qwenToolRegistryPathsForState(state)
	if err != nil {
		t.Fatal(err)
	}
	ledgerBody, err := os.ReadFile(filepath.Join(registry.ledgerRoot, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ledger toolRootLedgerState
	if json.Unmarshal(ledgerBody, &ledger) != nil {
		t.Fatal("decode Qwen tool ledger")
	}
	ledger.WorkerIdentity = qwenLaneProcessIdentity(state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart)
	if err := writeJSONAtomic(filepath.Join(registry.ledgerRoot, "ledger.json"), ledger); err != nil {
		t.Fatal(err)
	}
	originalArchive := executeQwenArchiveTransaction
	executeQwenArchiveTransaction = func(qwenLaneState, string) error { return nil }
	t.Cleanup(func() { executeQwenArchiveTransaction = originalArchive })
	if err := forceArchiveQwenLane(paths, state.ThreadID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-worker.done:
		t.Fatal("forced archive signaled an unrelated recycled worker identity")
	default:
	}
	stopGrokManagedProcess(worker, time.Second)
}

func TestQwenLaneCleanupDebtSurvivesRestartAndRetries(t *testing.T) {
	paths, state, worker, _ := prepareCrashedQwenLane(t, false)
	registry, err := qwenToolRegistryPathsForState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry.ledgerRoot, "ledger.json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalArchive := executeQwenArchiveTransaction
	executeQwenArchiveTransaction = func(qwenLaneState, string) error { return nil }
	t.Cleanup(func() { executeQwenArchiveTransaction = originalArchive })
	firstErr := forceArchiveQwenLane(paths, state.ThreadID)
	if firstErr == nil {
		t.Fatal("corrupt durable tool ledger did not retain cleanup debt")
	}
	debt, err := readQwenLaneState(paths, state.ThreadID)
	if err != nil || debt.Status != "cleanup_debt" || len(debt.CleanupDebt) != 1 || debt.CleanupDebt[0].Attempts != 1 {
		t.Fatalf("first cleanup debt = %+v, %v", debt, err)
	}
	config, err := qwenToolRootLedgerConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	valid := toolRootLedgerState{
		Version: config.Version, Product: config.Product, ManagerIdentity: config.ManagerIdentity,
		WorkerIdentity: config.WorkerIdentity, CapabilityDigest: config.CapabilityDigest,
		IntentRevision: config.IntentRevision, AdmissionOpen: false, CleanupState: "pending",
	}
	if err := writeJSONAtomic(filepath.Join(registry.ledgerRoot, "ledger.json"), valid); err != nil {
		t.Fatal(err)
	}
	if err := reconcileQwenLaneManagers(paths); err != nil {
		t.Fatal(err)
	}
	select {
	case <-worker.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker survived cleanup-debt reconciliation")
	}
	archived, err := readQwenLaneState(paths, state.ThreadID)
	if err != nil || archived.Status != "archived" || len(archived.CleanupDebt) != 0 {
		t.Fatalf("retried cleanup = %+v, %v", archived, err)
	}
}

func prepareCrashedQwenLane(t *testing.T, registerTool bool) (nativePaths, qwenLaneState, *grokManagedProcess, *grokManagedProcess) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"state", "claude", "codex", "runtime", "qwen-home", "qwen-runtime", "workspace"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("QWEN_HOME", filepath.Join(root, "qwen-home"))
	t.Setenv("QWEN_RUNTIME_DIR", filepath.Join(root, "qwen-runtime"))
	profile, err := qwenprofile.Current()
	if err != nil {
		t.Fatal(err)
	}
	worker := startQwenCleanupProcess(t)
	workerInfo := procinfo.Read(worker.cmd.Process.Pid)
	paths := resolveNativePaths()
	threadID, launchToken := randomID(), randomID()+randomID()
	state := qwenLaneState{
		Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
		Name: "crashed-qwen", ThreadID: threadID, QwenSessionID: randomID(), Cwd: filepath.Join(root, "workspace"),
		Profile: profile, Status: "active", ManagerPID: 999999, ManagerProcStart: "stopped-manager",
		ManagerStrongStart: "stopped-manager-strong", WorkerPID: worker.cmd.Process.Pid,
		WorkerProcStart: workerInfo.Start, WorkerStrongStart: workerInfo.StrongStart,
		RuntimeDir: paths.runtimeDir, LaunchTokenHash: qwenLaneTokenHash(launchToken), LaunchPreference: "native_default",
		NativeArchiveState: "active", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeQwenLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &qwenLaneManager{paths: paths, state: state, launchToken: launchToken}
	if err := manager.prepareToolRegistry(); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSharedToolRootLedger(); err != nil {
		t.Fatal(err)
	}
	state = manager.state
	var tool *grokManagedProcess
	if registerTool {
		tool = startQwenCleanupProcess(t)
		toolInfo := procinfo.Read(tool.cmd.Process.Pid)
		config, configErr := qwenToolRootLedgerConfig(state)
		if configErr != nil {
			t.Fatal(configErr)
		}
		ledger, openErr := openToolRootLedger(config)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if registerErr := ledger.register(toolRootProcessIdentity{PID: tool.cmd.Process.Pid, ProcStart: toolInfo.Start, StrongStart: toolInfo.StrongStart}); registerErr != nil {
			t.Fatal(registerErr)
		}
	}
	t.Cleanup(func() {
		stopGrokManagedProcess(worker, time.Second)
		stopGrokManagedProcess(tool, time.Second)
	})
	return paths, state, worker, tool
}

func startQwenCleanupProcess(t *testing.T) *grokManagedProcess {
	t.Helper()
	process, err := startGrokManagedProcess(exec.Command("sleep", "60"), nil)
	if err != nil {
		t.Fatal(err)
	}
	info := procinfo.Read(process.cmd.Process.Pid)
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		stopGrokManagedProcess(process, time.Second)
		t.Fatal("cleanup process has no strong identity")
	}
	return process
}
