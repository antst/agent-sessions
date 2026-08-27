package daemon

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRuntimeRecoversInOrderBeforeOpeningAdmission(t *testing.T) {
	root := t.TempDir()
	state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	paths := ProductionPaths{
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"),
		ControlEndpoint: filepath.Join(root, "run", "daemon.sock"),
	}
	configuration := DaemonConfig{
		SchemaVersion: DaemonConfigSchemaVersion, HostID: "host", HostName: "builder",
		StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
	}
	var observed []RecoveryStage
	hooks := make(map[RecoveryStage]RecoveryHook, len(orderedRecoveryStages))
	for _, stage := range orderedRecoveryStages {
		stage := stage
		hooks[stage] = func(_ context.Context, runtime *Runtime) error {
			if runtime.Admission() != AdmissionRecovering {
				t.Fatalf("stage %s observed admission %s", stage, runtime.Admission())
			}
			observed = append(observed, stage)
			return nil
		}
	}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, Configuration: configuration, State: state,
		RuntimeVersion: "0.3.0", RuntimeIdentity: "sha256:test", PID: 42,
		ProcStart: "start", StrongStart: "strong", ServiceManager: "systemd-user",
		ServiceUnit: "agent-sessions.service", Now: func() time.Time { return time.UnixMilli(100) },
		RecoveryHooks: hooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.Admission() != AdmissionReady || runtime.Generation() != 1 {
		t.Fatalf("started runtime admission/generation = %s/%d", runtime.Admission(), runtime.Generation())
	}
	if !reflect.DeepEqual(observed, orderedRecoveryStages[:]) ||
		!reflect.DeepEqual(runtime.CompletedRecoveryStages(), orderedRecoveryStages[:]) {
		t.Fatalf("recovery order = %v / %v, want %v", observed, runtime.CompletedRecoveryStages(), orderedRecoveryStages)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.Admission() != AdmissionClosed {
		t.Fatalf("stopped runtime admission = %s", runtime.Admission())
	}
}

func TestRuntimeGenerationAdvancesAcrossCompositionRoots(t *testing.T) {
	root := t.TempDir()
	state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	paths := ProductionPaths{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), ControlEndpoint: filepath.Join(root, "run", "daemon.sock")}
	configuration := DaemonConfig{SchemaVersion: 1, HostID: "host", HostName: "builder", StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1}
	newRuntime := func() *Runtime {
		runtime, newErr := NewRuntime(RuntimeOptions{
			Paths: paths, Configuration: configuration, State: state, RuntimeVersion: "0.3.0",
			RuntimeIdentity: "sha256:test", PID: 42, ProcStart: "start", StrongStart: "strong",
			ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
			Now: func() time.Time { return time.UnixMilli(100) },
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		return runtime
	}
	first := newRuntime()
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := newRuntime()
	if err := second.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.Generation() != first.Generation()+1 {
		t.Fatalf("second generation = %d, first = %d", second.Generation(), first.Generation())
	}
}
