package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

const unifiedLaneCompositionEnvironment = "AGENT_SESSIONS_UNIFIED_LANE_COMPOSITION"

type unifiedLaneWorker struct {
	command   *exec.Cmd
	pid       int
	procStart string
}

type unifiedLaneAdapter struct {
	mu         sync.Mutex
	product    string
	workers    map[string]unifiedLaneWorker
	dispatches map[string]int
	reconnects map[string]int
}

func (adapter *unifiedLaneAdapter) Dispatch(
	_ context.Context,
	lane LaneRecord,
	turn LaneTurnRecord,
) (LaneDispatchResult, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	worker, ok := adapter.workers[lane.LaneSessionID]
	if !ok || !unifiedLaneWorkerLive(worker) {
		return LaneDispatchResult{}, ErrAttachmentEvidenceChanged
	}
	adapter.dispatches[turn.TurnID]++
	return LaneDispatchResult{
		NativeActor:        map[string]any{"pid": worker.pid, "proc_start": worker.procStart, "product": adapter.product},
		NativeTurnIdentity: map[string]any{"turn_id": turn.TurnID, "native_session_id": lane.LaneSessionID},
		DispatchState:      LaneDispatchRunning,
	}, nil
}

func (adapter *unifiedLaneAdapter) ReconnectTurn(
	_ context.Context,
	lane LaneRecord,
	turn LaneTurnRecord,
) (LaneReconnectResult, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	worker, ok := adapter.workers[lane.LaneSessionID]
	if !ok || !unifiedLaneWorkerLive(worker) {
		return LaneReconnectResult{}, ErrAttachmentEvidenceChanged
	}
	adapter.reconnects[turn.TurnID]++
	result := LaneReconnectResult{
		NativeActor:        map[string]any{"pid": worker.pid, "proc_start": worker.procStart, "product": adapter.product},
		NativeTurnIdentity: cloneAttachmentEvidence(turn.NativeTurnIdentity), DispatchState: LaneDispatchRunning,
	}
	if adapter.product != "codex" {
		result.DispatchState, result.TerminalOutcome = LaneDispatchInterrupted, LaneTerminalInterrupted
		result.ResultReference = map[string]any{
			"restart_outcome": "evidence-approved-interrupted", "collectable": true,
			"resumable": true, "native_evidence": true,
		}
	}
	return result, nil
}

func (*unifiedLaneAdapter) InterruptTurn(context.Context, LaneRecord, LaneTurnRecord) error {
	return nil
}

func (*unifiedLaneAdapter) CollectTurn(
	_ context.Context,
	_ LaneRecord,
	turn LaneTurnRecord,
) (LaneTerminalResult, error) {
	return LaneTerminalResult{
		TerminalOutcome: turn.TerminalOutcome, ResultReference: cloneAttachmentEvidence(turn.ResultReference),
		NativeTurnIdentity: cloneAttachmentEvidence(turn.NativeTurnIdentity),
	}, nil
}

func (*unifiedLaneAdapter) Archive(context.Context, LaneRecord) error { return nil }
func (*unifiedLaneAdapter) Cleanup(context.Context, LaneRecord) error { return nil }

func (adapter *unifiedLaneAdapter) count(turnID string) (int, int) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.dispatches[turnID], adapter.reconnects[turnID]
}

type unifiedLaneAttachmentAdapter struct{}

func (*unifiedLaneAttachmentAdapter) PrepareInteractive(
	_ context.Context,
	request AttachmentPrepareRequest,
) (NativeLaunchPlan, error) {
	return NativeLaunchPlan{
		Executable: request.Product, Cwd: request.Cwd,
		ExpectedNativeActor: cloneAttachmentEvidence(request.ExpectedNativeActor),
	}, nil
}

func (*unifiedLaneAttachmentAdapter) Corroborate(
	_ context.Context,
	_ AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	return cloneAttachmentEvidence(evidence), nil
}

func (*unifiedLaneAttachmentAdapter) Reconnect(
	_ context.Context,
	record AttachmentRecord,
) (map[string]any, error) {
	return cloneAttachmentEvidence(record.NativeActor), nil
}

func TestUnifiedLaneComposition(t *testing.T) {
	if os.Getenv(unifiedLaneCompositionEnvironment) != "1" {
		t.Skip("run through scripts/test-unified-lane-composition")
	}
	ctx := context.Background()
	root := t.TempDir()
	token := "unified-lane-composition-" + filepath.Base(root)
	products := []string{"codex", "claude", "grok", "qwen"}
	workers := startUnifiedLaneWorkers(t, token, products)
	adapters := make(map[string]*unifiedLaneAdapter, len(products))
	laneAdapters := make(map[string]LaneAdapter, len(products))
	for _, product := range products {
		adapter := &unifiedLaneAdapter{
			product: product, workers: workers, dispatches: make(map[string]int), reconnects: make(map[string]int),
		}
		adapters[product], laneAdapters[product] = adapter, adapter
	}

	state, err := OpenStateStore(filepath.Join(root, "state"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	paths := ProductionPaths{
		ConfigurationRoot: filepath.Join(root, "config"), ConfigurationFile: filepath.Join(root, "config", "config.json"),
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), ControlEndpoint: filepath.Join(root, "run", "daemon.sock"),
	}
	attachmentAdapter := &unifiedLaneAttachmentAdapter{}
	attachmentAdapters := map[string]AttachmentAdapter{
		"codex": attachmentAdapter, "claude": attachmentAdapter, "grok": attachmentAdapter, "qwen": attachmentAdapter,
	}
	first := newUnifiedLaneRuntime(t, paths, state, attachmentAdapters, laneAdapters, "generation-one", 72001)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first daemon: %v", err)
	}
	parents := attachUnifiedLaneParents(t, first, products)
	cells := startUnifiedLaneCells(t, first, products, parents, workers)
	assertUnifiedLaneCensus(t, token, len(cells))

	if err := first.Stop(ctx); err != nil {
		t.Fatalf("stop first daemon: %v", err)
	}
	second := newUnifiedLaneRuntime(t, paths, state, attachmentAdapters, laneAdapters, "generation-two", 72002)
	if err := second.Start(ctx); err != nil {
		t.Fatalf("restart daemon during active turns: %v", err)
	}
	if second.Generation() <= first.Generation() {
		t.Fatalf("daemon generation did not advance: first=%d second=%d", first.Generation(), second.Generation())
	}
	verifyUnifiedLaneCells(t, second, cells, parents, adapters, first.Generation(), second.Generation())
	assertUnifiedLaneCensus(t, token, len(cells))
	assertNoUnifiedLaneObsoleteArtifacts(t, root)
	if err := second.Stop(ctx); err != nil {
		t.Fatalf("stop successor daemon: %v", err)
	}
}

type unifiedLaneCell struct {
	parent, target, laneSessionID, turnID string
}

func newUnifiedLaneRuntime(
	t *testing.T,
	paths ProductionPaths,
	state *StateStore,
	attachmentAdapters map[string]AttachmentAdapter,
	laneAdapters map[string]LaneAdapter,
	identity string,
	pid int,
) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, State: state,
		Configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion, HostID: "composition-host", HostName: "composition-host",
			StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
		},
		RuntimeVersion: "0.3.0", RuntimeIdentity: "sha256:" + identity, PID: pid,
		ProcStart: identity, StrongStart: identity + "-strong", ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: timeNowIncrementing(), AttachmentAdapters: attachmentAdapters, LaneAdapters: laneAdapters,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func attachUnifiedLaneParents(t *testing.T, runtime *Runtime, products []string) map[string]AttachmentRecord {
	t.Helper()
	result := make(map[string]AttachmentRecord, len(products))
	for index, product := range products {
		sessionID := fmt.Sprintf("parent-%s-%d", product, index)
		prepared, capability, err := runtime.attachmentRegistry().Prepare(context.Background(), AttachmentPrepareRequest{
			Product: product, Kind: "interactive", Cwd: "/workspace/" + product, Name: product + "-parent",
			Groups:         []string{"composition", "session:composition-host/" + sessionID},
			PermissionMode: "default", ExpectedNativeActor: map[string]any{"pid": 73000 + index, "proc_start": "parent-" + product},
		})
		if err != nil {
			t.Fatalf("prepare %s parent: %v", product, err)
		}
		attached, err := runtime.attachmentRegistry().Adopt(context.Background(), AttachmentAdoptRequest{
			AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: sessionID,
			NativeActor: map[string]any{"pid": 73000 + index, "proc_start": "parent-" + product},
		})
		if err != nil {
			t.Fatalf("adopt %s parent: %v", product, err)
		}
		result[product] = attached
	}
	return result
}

func startUnifiedLaneCells(
	t *testing.T,
	runtime *Runtime,
	products []string,
	parents map[string]AttachmentRecord,
	workers map[string]unifiedLaneWorker,
) []unifiedLaneCell {
	t.Helper()
	cells := make([]unifiedLaneCell, 0, len(products)*len(products))
	for _, parent := range products {
		for _, target := range products {
			laneID, turnID := "lane-"+parent+"-to-"+target, "turn-"+parent+"-to-"+target
			if _, ok := workers[laneID]; !ok {
				t.Fatalf("missing vendor worker for %s", laneID)
			}
			lane, turn, err := runtime.laneEngine().Start(context.Background(), LaneStartRequest{
				LaneSessionID: laneID, TurnID: turnID, SourceAttachmentID: parents[parent].AttachmentID,
				Product: target, Name: laneID, Cwd: "/workspace/composition", Groups: []string{"composition"},
				InheritParentGroups: true, PermissionMode: "default", InputReference: map[string]any{"input_id": turnID},
			})
			if err != nil || lane.State != LaneStateRunning || turn.DispatchState != LaneDispatchRunning {
				t.Fatalf("start %s->%s: lane=%+v turn=%+v err=%v", parent, target, lane, turn, err)
			}
			cells = append(cells, unifiedLaneCell{parent: parent, target: target, laneSessionID: laneID, turnID: turnID})
		}
	}
	return cells
}

func startUnifiedLaneWorkers(t *testing.T, token string, products []string) map[string]unifiedLaneWorker {
	t.Helper()
	workers := make(map[string]unifiedLaneWorker, len(products)*len(products))
	for _, parent := range products {
		for _, target := range products {
			laneID := "lane-" + parent + "-to-" + target
			command := exec.Command(os.Args[0], "-test.run=^TestUnifiedPeerVendorHelper$", "--", token, "vendor", laneID)
			command.Env = append(os.Environ(), "AGENT_SESSIONS_UNIFIED_VENDOR_HELPER=1")
			if err := command.Start(); err != nil {
				t.Fatalf("start %s vendor lane worker: %v", laneID, err)
			}
			identity := waitUnifiedPeerIdentity(t, command.Process.Pid)
			workers[laneID] = unifiedLaneWorker{command: command, pid: command.Process.Pid, procStart: identity.Start}
		}
	}
	t.Cleanup(func() {
		for _, worker := range workers {
			_ = worker.command.Process.Kill()
			_ = worker.command.Wait()
		}
	})
	return workers
}

func unifiedLaneWorkerLive(worker unifiedLaneWorker) bool {
	identity := procinfo.Read(worker.pid)
	return identity.Status == procinfo.Known && identity.Start == worker.procStart
}

func verifyUnifiedLaneCells(
	t *testing.T,
	runtime *Runtime,
	cells []unifiedLaneCell,
	parents map[string]AttachmentRecord,
	adapters map[string]*unifiedLaneAdapter,
	acceptedGeneration uint64,
	recoveredGeneration uint64,
) {
	t.Helper()
	for _, cell := range cells {
		turn, err := runtime.laneEngine().ReadTurn(context.Background(), cell.laneSessionID, cell.turnID)
		if err != nil {
			t.Fatalf("read %s->%s recovered turn: %v", cell.parent, cell.target, err)
		}
		if cell.target == "codex" {
			if turn.DispatchState != LaneDispatchRunning || turn.TerminalOutcome != "" {
				t.Fatalf("Codex %s->%s did not continue: %+v", cell.parent, cell.target, turn)
			}
			_, completeErr := runtime.laneEngine().Complete(context.Background(), LaneTerminalRequest{
				LaneSessionID: cell.laneSessionID, TurnID: cell.turnID, Outcome: LaneTerminalCompleted,
				NativeTurnIdentity: turn.NativeTurnIdentity, ResultReference: map[string]any{"continued": true},
			})
			if completeErr != nil {
				t.Fatalf("complete continued %s->%s: %v", cell.parent, cell.target, completeErr)
			}
		} else if turn.TerminalOutcome != LaneTerminalInterrupted ||
			turn.ResultReference["restart_outcome"] != "evidence-approved-interrupted" {
			t.Fatalf("%s->%s restart outcome = %+v", cell.parent, cell.target, turn)
		}
		collection, err := runtime.laneEngine().Collect(context.Background(), LaneCollectRequest{
			LaneSessionID: cell.laneSessionID, TurnID: cell.turnID, SourceAttachmentID: parents[cell.parent].AttachmentID,
		})
		if err != nil || collection.CollectionRevision != 1 || collection.Outcome == "" {
			t.Fatalf("collect %s->%s = %+v, %v", cell.parent, cell.target, collection, err)
		}
		dispatches, reconnects := adapters[cell.target].count(cell.turnID)
		if dispatches != 1 || reconnects != 1 {
			t.Fatalf("%s->%s dispatch/reconnect = %d/%d, want 1/1", cell.parent, cell.target, dispatches, reconnects)
		}
		worker := adapters[cell.target].workers[cell.laneSessionID]
		restartOutcome := "continued"
		if cell.target != "codex" {
			restartOutcome = "evidence-approved-interrupted"
		}
		interrupted := cell.target != "codex"
		evidence, marshalErr := json.Marshal(map[string]any{
			"type":                     "unified.lane_composition.cell",
			"contract_version":         1,
			"parent_product":           cell.parent,
			"target_product":           cell.target,
			"parent_attachment_id":     parents[cell.parent].AttachmentID,
			"parent_session_id":        parents[cell.parent].SessionID,
			"lane_session_id":          cell.laneSessionID,
			"turn_id":                  cell.turnID,
			"accepted_generation":      acceptedGeneration,
			"recovered_generation":     recoveredGeneration,
			"native_pid":               worker.pid,
			"native_proc_start":        worker.procStart,
			"active_before_restart":    true,
			"worker_survived_restart":  true,
			"restart_outcome":          restartOutcome,
			"terminal_outcome":         collection.Outcome,
			"collectable":              true,
			"resumable":                interrupted,
			"native_evidence_required": interrupted,
			"collection_revision":      collection.CollectionRevision,
			"dispatch_count":           dispatches,
			"reconnect_count":          reconnects,
			"redispatch_count":         dispatches - 1,
		})
		if marshalErr != nil {
			t.Fatalf("marshal %s->%s evidence: %v", cell.parent, cell.target, marshalErr)
		}
		t.Log(string(evidence))
	}
}

func assertUnifiedLaneCensus(t *testing.T, token string, want int) {
	t.Helper()
	processes, err := procinfo.List()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, process := range processes {
		arguments, argsErr := procinfo.Args(process.PID)
		if argsErr != nil || !containsUnifiedLaneArgument(arguments, token) {
			continue
		}
		seen++
		joined := " " + strings.Join(arguments, " ") + " "
		for _, forbidden := range []string{" supervisor ", " shim ", "lane-manager", "qwen-host", "grok-host", "lane-watch"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("obsolete Agent Sessions process survived: %s", joined)
			}
		}
	}
	if seen != want {
		t.Fatalf("vendor lane worker census = %d, want %d", seen, want)
	}
}

func containsUnifiedLaneArgument(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
}

func assertNoUnifiedLaneObsoleteArtifacts(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := strings.ToLower(entry.Name())
		for _, forbidden := range []string{"supervisor", "shim", "lane-manager", "qwen-host", "grok-host", "lane-watch"} {
			if strings.Contains(name, forbidden) {
				return fmt.Errorf("obsolete runtime artifact %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
