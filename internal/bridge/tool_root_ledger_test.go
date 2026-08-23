//go:build linux || darwin

package bridge

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestToolRootLedgerReusesPIDOnlyAfterStrongStartProvesOldRootStale(t *testing.T) {
	for _, product := range []string{"grok", "qwen"} {
		t.Run(product, func(t *testing.T) {
			observed := map[int]procinfo.Info{}
			ledger := prepareTestToolRootLedger(t, product, observed, func(toolRootProcessIdentity) error {
				t.Fatal("registration must not retire a process")
				return nil
			})
			oldRoot := toolRootProcessIdentity{PID: 2101, ProcStart: "same-display-start", StrongStart: "kernel-start-a"}
			observed[oldRoot.PID] = knownToolRootProcess(oldRoot)
			if err := ledger.register(oldRoot); err != nil {
				t.Fatalf("register original root: %v", err)
			}

			newRoot := toolRootProcessIdentity{PID: oldRoot.PID, ProcStart: oldRoot.ProcStart, StrongStart: "kernel-start-b"}
			observed[newRoot.PID] = knownToolRootProcess(newRoot)
			if err := ledger.register(newRoot); err != nil {
				t.Fatalf("reuse PID after exact strong-start mismatch: %v", err)
			}
			snapshot, err := ledger.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Roots) != 1 || snapshot.Roots[0] != newRoot {
				t.Fatalf("roots after PID reuse = %+v; want only %+v", snapshot.Roots, newRoot)
			}
		})
	}
}

func TestToolRootLedgerAdmissionClosureSurvivesReopen(t *testing.T) {
	observed := map[int]procinfo.Info{}
	config := testToolRootLedgerConfig(t, "qwen", observed, func(toolRootProcessIdentity) error { return nil })
	ledger, err := prepareToolRootLedger(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.closeAdmission(); err != nil {
		t.Fatalf("close admission: %v", err)
	}

	root := toolRootProcessIdentity{PID: 2201, ProcStart: "root-start", StrongStart: "root-strong-start"}
	observed[root.PID] = knownToolRootProcess(root)
	if err := ledger.register(root); err == nil {
		t.Fatal("closed ledger admitted a detached root")
	}
	reopened, err := openToolRootLedger(config)
	if err != nil {
		t.Fatalf("reopen closed ledger: %v", err)
	}
	if err := reopened.register(root); err == nil {
		t.Fatal("reopened ledger forgot durable admission closure")
	}
	snapshot, err := reopened.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AdmissionOpen {
		t.Fatal("snapshot reports admission open after cleanup barrier")
	}
}

func TestToolRootLedgerCrashRetryRetainsCleanupDebt(t *testing.T) {
	root := toolRootProcessIdentity{PID: 2301, ProcStart: "root-start", StrongStart: "root-strong-start"}
	observed := map[int]procinfo.Info{root.PID: knownToolRootProcess(root)}
	retireFailure := errors.New("injected retire crash")
	config := testToolRootLedgerConfig(t, "qwen", observed, func(identity toolRootProcessIdentity) error {
		if identity != root {
			t.Fatalf("retire identity = %+v; want %+v", identity, root)
		}
		return retireFailure
	})
	ledger, err := prepareToolRootLedger(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.register(root); err != nil {
		t.Fatal(err)
	}
	if err := ledger.closeAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.reconcileCleanup(); !errors.Is(err, retireFailure) {
		t.Fatalf("cleanup error = %v; want %v", err, retireFailure)
	}
	snapshot, err := ledger.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CleanupState != "debt" || len(snapshot.Roots) != 1 || snapshot.Roots[0] != root {
		t.Fatalf("failed cleanup did not retain exact debt: %+v", snapshot)
	}

	// Simulate a new manager after the process that attempted cleanup crashed.
	config.RetireRoot = func(identity toolRootProcessIdentity) error {
		if identity != root {
			t.Fatalf("retried retire identity = %+v; want %+v", identity, root)
		}
		observed[root.PID] = procinfo.Info{Status: procinfo.Absent}
		return nil
	}
	reopened, err := openToolRootLedger(config)
	if err != nil {
		t.Fatalf("reopen cleanup debt: %v", err)
	}
	if err := reopened.reconcileCleanup(); err != nil {
		t.Fatalf("retry cleanup debt: %v", err)
	}
	snapshot, err = reopened.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CleanupState != "clean" || len(snapshot.Roots) != 0 {
		t.Fatalf("cleanup retry did not converge: %+v", snapshot)
	}
}

func TestToolRootLedgerPreservesUnrelatedProcessAfterPIDReuse(t *testing.T) {
	owned := toolRootProcessIdentity{PID: 2401, ProcStart: "same-display-start", StrongStart: "owned-strong-start"}
	unrelated := toolRootProcessIdentity{PID: owned.PID, ProcStart: owned.ProcStart, StrongStart: "unrelated-strong-start"}
	observed := map[int]procinfo.Info{owned.PID: knownToolRootProcess(owned)}
	retireCalls := 0
	ledger := prepareTestToolRootLedger(t, "grok", observed, func(identity toolRootProcessIdentity) error {
		retireCalls++
		t.Fatalf("attempted to retire unrelated process through stale identity: %+v", identity)
		return nil
	})
	if err := ledger.register(owned); err != nil {
		t.Fatal(err)
	}
	observed[owned.PID] = knownToolRootProcess(unrelated)
	if err := ledger.closeAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.reconcileCleanup(); err != nil {
		t.Fatalf("reconcile reused PID: %v", err)
	}
	if retireCalls != 0 {
		t.Fatalf("retire calls = %d; want zero", retireCalls)
	}
	snapshot, err := ledger.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CleanupState != "clean" || len(snapshot.Roots) != 0 {
		t.Fatalf("stale ownership did not converge without collateral cleanup: %+v", snapshot)
	}
}

func prepareTestToolRootLedger(
	t *testing.T,
	product string,
	observed map[int]procinfo.Info,
	retire func(toolRootProcessIdentity) error,
) *toolRootLedger {
	t.Helper()
	ledger, err := prepareToolRootLedger(testToolRootLedgerConfig(t, product, observed, retire))
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func testToolRootLedgerConfig(
	t *testing.T,
	product string,
	observed map[int]procinfo.Info,
	retire func(toolRootProcessIdentity) error,
) toolRootLedgerConfig {
	t.Helper()
	manager := toolRootProcessIdentity{PID: 2001, ProcStart: "manager-start", StrongStart: "manager-strong-start"}
	worker := toolRootProcessIdentity{PID: 2002, ProcStart: "worker-start", StrongStart: "worker-strong-start"}
	observed[manager.PID] = knownToolRootProcess(manager)
	observed[worker.PID] = knownToolRootProcess(worker)
	return toolRootLedgerConfig{
		Version:          1,
		Product:          product,
		ManagerIdentity:  manager,
		WorkerIdentity:   worker,
		CapabilityDigest: strings.Repeat("a", 64),
		IntentRevision:   "intent-" + product,
		Root:             filepath.Join(t.TempDir(), "tool-root-ledger"),
		ObserveProcess: func(pid int) procinfo.Info {
			if info, ok := observed[pid]; ok {
				return info
			}
			return procinfo.Info{Status: procinfo.Absent}
		},
		RetireRoot: retire,
	}
}

func knownToolRootProcess(identity toolRootProcessIdentity) procinfo.Info {
	return procinfo.Info{
		Status: procinfo.Known, Start: identity.ProcStart, StrongStart: identity.StrongStart,
	}
}
