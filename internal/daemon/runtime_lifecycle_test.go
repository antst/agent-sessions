package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/statestore"
	"github.com/antst/agent-sessions/internal/testutil"
)

type runtimeLifecycleFixture struct {
	paths         ProductionPaths
	configuration DaemonConfig
	state         *StateStore
	clock         atomic.Int64
}

type runtimeLifecycleIdentity struct {
	pid         int
	procStart   string
	strongStart string
}

type runtimeLifecycleAcceptedProbe struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

func TestRuntimeLifecycleRejectsSecondLiveAuthority(t *testing.T) {
	fixture := newRuntimeLifecycleFixture(t)
	identity := currentRuntimeLifecycleIdentity(t)
	firstEndpoint, err := acquireControlEndpoint(controlEndpointOptions{endpoint: fixture.paths.ControlEndpoint})
	if err != nil {
		t.Fatalf("acquire first authority endpoint: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := firstEndpoint.Close(); closeErr != nil {
			t.Errorf("close first authority endpoint: %v", closeErr)
		}
	})

	first := fixture.newRuntime(t, fixture.state, identity, nil)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start first authority: %v", err)
	}

	before, beforeRevision, err := fixture.state.ReadRuntime(context.Background())
	if err != nil {
		t.Fatalf("read first authority: %v", err)
	}
	if before.State != HostRuntimeReady {
		t.Fatalf("first authority state = %q, want ready", before.State)
	}

	if secondEndpoint, acquireErr := acquireControlEndpoint(controlEndpointOptions{endpoint: fixture.paths.ControlEndpoint}); acquireErr == nil {
		_ = secondEndpoint.Close()
		t.Fatal("second authority acquired the live daemon endpoint")
	}

	after, afterRevision, err := fixture.state.ReadRuntime(context.Background())
	if err != nil {
		t.Fatalf("read authority after rejected duplicate: %v", err)
	}
	if afterRevision != beforeRevision || !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected duplicate changed durable authority: before=%+v@%d after=%+v@%d", before, beforeRevision, after, afterRevision)
	}
	if first.Admission() != AdmissionReady {
		t.Fatalf("duplicate attempt changed first authority admission to %q", first.Admission())
	}
}

func TestRuntimeLifecycleCommitsOnlyOneSuccessorGeneration(t *testing.T) {
	fixture := newRuntimeLifecycleFixture(t)
	seed := fixture.seedRuntime(t, 4, runtimeLifecycleIdentity{
		pid: 1 << 30, procStart: "absent-start", strongStart: "absent-strong-start",
	}, HostRuntimeReady)
	endpoint := acquireRuntimeLifecycleEndpointAfterCrash(t, fixture.paths.ControlEndpoint, seed)
	defer func() {
		if err := endpoint.Close(); err != nil {
			t.Errorf("close successor endpoint: %v", err)
		}
	}()

	recoveryFailure := errors.New("successor readiness failed")
	failOnce := true
	hooks := map[RecoveryStage]RecoveryHook{
		RecoveryRouting: func(context.Context, *Runtime) error {
			if failOnce {
				failOnce = false
				return recoveryFailure
			}
			return nil
		},
	}
	candidate := fixture.newRuntime(t, fixture.state, currentRuntimeLifecycleIdentity(t), hooks)
	if err := candidate.Start(context.Background()); !errors.Is(err, recoveryFailure) {
		t.Fatalf("failed successor error = %v, want %v", err, recoveryFailure)
	}
	if candidate.Admission() != AdmissionClosed {
		t.Fatalf("failed successor admission = %q, want closed", candidate.Admission())
	}

	if err := candidate.Start(context.Background()); err != nil {
		t.Fatalf("retry successor readiness: %v", err)
	}
	if got, want := candidate.Generation(), seed.Generation+1; got != want {
		t.Fatalf("ready successor generation = %d, want %d; failed readiness consumed a committed authority generation", got, want)
	}
	record, _, err := fixture.state.ReadRuntime(context.Background())
	if err != nil {
		t.Fatalf("read ready successor: %v", err)
	}
	if record.Generation != seed.Generation+1 || record.State != HostRuntimeReady {
		t.Fatalf("ready successor authority = generation %d state %q, want generation %d ready", record.Generation, record.State, seed.Generation+1)
	}
}

func TestRuntimeLifecycleRecoversOneSuccessorAfterCrash(t *testing.T) {
	fixture := newRuntimeLifecycleFixture(t)
	crashed := fixture.seedRuntime(t, 7, runtimeLifecycleIdentity{
		pid: 1 << 30, procStart: "crashed-start", strongStart: "crashed-strong-start",
	}, HostRuntimeReady)
	leaveRuntimeLifecycleOrphanedSocket(t, fixture.paths.ControlEndpoint)
	endpoint := acquireRuntimeLifecycleEndpointAfterCrash(t, fixture.paths.ControlEndpoint, crashed)
	defer func() {
		if err := endpoint.Close(); err != nil {
			t.Errorf("close recovered endpoint: %v", err)
		}
	}()

	successorIdentity := currentRuntimeLifecycleIdentity(t)
	successor := fixture.newRuntime(t, fixture.state, successorIdentity, nil)
	if err := successor.Start(context.Background()); err != nil {
		t.Fatalf("start crash successor: %v", err)
	}
	if got, want := successor.Generation(), crashed.Generation+1; got != want {
		t.Fatalf("crash successor generation = %d, want %d", got, want)
	}
	if successor.Admission() != AdmissionReady {
		t.Fatalf("crash successor admission = %q, want ready", successor.Admission())
	}
	record, _, err := fixture.state.ReadRuntime(context.Background())
	if err != nil {
		t.Fatalf("read crash successor authority: %v", err)
	}
	if record.State != HostRuntimeReady || record.PID != successorIdentity.pid ||
		record.ProcStart != successorIdentity.procStart || record.StrongStart != successorIdentity.strongStart {
		t.Fatalf("crash successor did not replace the exact stale authority: %+v", record)
	}
	if secondEndpoint, acquireErr := acquireControlEndpoint(controlEndpointOptions{endpoint: fixture.paths.ControlEndpoint}); acquireErr == nil {
		_ = secondEndpoint.Close()
		t.Fatal("crash recovery published more than one successor endpoint")
	}
}

func TestForegroundControlOwnerComesFromExactDurableAuthority(t *testing.T) {
	fixture := newRuntimeLifecycleFixture(t)
	seed := fixture.seedRuntime(t, 9, runtimeLifecycleIdentity{
		pid: 1 << 29, procStart: "prior-start", strongStart: "prior-strong-start",
	}, HostRuntimeReady)

	owner, _, err := priorControlOwner(context.Background(), fixture.state)
	if err != nil {
		t.Fatalf("load foreground control owner: %v", err)
	}
	if owner == nil || owner.PID != seed.PID || owner.Status != procinfo.Known ||
		owner.Start != seed.ProcStart || owner.StrongStart != seed.StrongStart {
		t.Fatalf("foreground control owner = %+v, want exact durable identity from %+v", owner, seed)
	}
}

func TestRuntimeLifecycleExplicitStopRemainsStopped(t *testing.T) {
	fixture := newRuntimeLifecycleFixture(t)
	endpoint, err := acquireControlEndpoint(controlEndpointOptions{endpoint: fixture.paths.ControlEndpoint})
	if err != nil {
		t.Fatalf("acquire explicit-stop endpoint: %v", err)
	}
	running := fixture.newRuntime(t, fixture.state, currentRuntimeLifecycleIdentity(t), nil)
	if err := running.Start(context.Background()); err != nil {
		_ = endpoint.Close()
		t.Fatalf("start explicit-stop authority: %v", err)
	}
	generation := running.Generation()
	if err := running.Stop(context.Background()); err != nil {
		_ = endpoint.Close()
		t.Fatalf("explicit stop: %v", err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatalf("close explicitly stopped endpoint: %v", err)
	}

	stopped, stoppedRevision, err := fixture.state.ReadRuntime(context.Background())
	if err != nil {
		t.Fatalf("read explicitly stopped authority: %v", err)
	}
	if stopped.State != HostRuntimeStopping || stopped.Generation != generation || running.Admission() != AdmissionClosed {
		t.Fatalf("explicit stop state = %+v admission=%q", stopped, running.Admission())
	}
	if err := running.Stop(context.Background()); err != nil {
		t.Fatalf("repeat explicit stop: %v", err)
	}
	after, afterRevision, err := fixture.state.ReadRuntime(context.Background())
	if err != nil {
		t.Fatalf("read repeated explicit stop: %v", err)
	}
	if afterRevision != stoppedRevision || !reflect.DeepEqual(after, stopped) {
		t.Fatalf("repeat stop restarted or rewrote authority: before=%+v@%d after=%+v@%d", stopped, stoppedRevision, after, afterRevision)
	}
	if connection, dialErr := DialControlEndpoint(context.Background(), fixture.paths.ControlEndpoint); !errors.Is(dialErr, ErrDaemonUnavailable) {
		if connection != nil {
			_ = connection.Close()
		}
		t.Fatalf("workflow after explicit stop error = %v, want daemon unavailable", dialErr)
	}
}

func TestRuntimeLifecycleWorkflowDoesNotStartUnavailableDaemon(t *testing.T) {
	fixture := newRuntimeLifecycleFixture(t)
	for _, operation := range []string{"peer.send", "lane.start", "federation.send"} {
		t.Run(operation, func(t *testing.T) {
			connection, err := DialControlEndpoint(context.Background(), fixture.paths.ControlEndpoint)
			if connection != nil {
				_ = connection.Close()
				t.Fatal("unavailable workflow returned a daemon connection")
			}
			if !errors.Is(err, ErrDaemonUnavailable) {
				t.Fatalf("unavailable workflow error = %v, want ErrDaemonUnavailable", err)
			}
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) || unavailable.ExitCode() != 3 || unavailable.NextAction != daemonInspectionCommand() {
				t.Fatalf("unavailable workflow diagnostic = %#v", unavailable)
			}
			lowerAction := strings.ToLower(unavailable.NextAction)
			if strings.Contains(lowerAction, " restart") || strings.Contains(lowerAction, " start") {
				t.Fatalf("unavailable workflow suggested implicit lifecycle mutation: %q", unavailable.NextAction)
			}
		})
	}
	if _, _, err := fixture.state.ReadRuntime(context.Background()); !os.IsNotExist(err) {
		t.Fatalf("workflow created daemon authority state: %v", err)
	}
	for _, path := range []string{fixture.paths.ControlEndpoint, controlEndpointLockPath(fixture.paths.ControlEndpoint)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("workflow created runtime artifact %q: %v", path, err)
		}
	}
}

func TestRuntimeLifecycleResourceExhaustionFailsBeforeAdmission(t *testing.T) {
	tests := []struct {
		name       string
		failure    error
		faultPoint statestore.FaultPoint
		occurrence int
		stage      RecoveryStage
	}{
		{name: "disk full", failure: syscall.ENOSPC, faultPoint: statestore.FaultSyncParentDirectory, occurrence: 3},
		{name: "memory exhausted", failure: syscall.ENOMEM, stage: RecoveryCatalog},
		{name: "file descriptors exhausted", failure: syscall.EMFILE, faultPoint: statestore.FaultCreateTemporary, occurrence: 1},
		{name: "process slots exhausted", failure: syscall.EAGAIN, stage: RecoveryNativeActors},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeLifecycleFixture(t)
			accepted := runtimeLifecycleAcceptedProbe{ID: "accepted-before-resource-failure", State: "accepted"}
			if _, err := fixture.state.records.CompareAndSwap(context.Background(), "accepted/runtime-lifecycle-probe", 0, accepted); err != nil {
				t.Fatalf("seed accepted probe: %v", err)
			}

			var faultCalls int
			faultedRecords, err := statestore.Open(statestore.Options{
				Root: fixture.paths.StateRoot, MaxRecordBytes: 1 << 20,
				InjectFault: func(point statestore.FaultPoint) error {
					if point != test.faultPoint {
						return nil
					}
					faultCalls++
					if faultCalls == test.occurrence {
						return test.failure
					}
					return nil
				},
			})
			if err != nil {
				t.Fatalf("open faulted daemon state: %v", err)
			}
			faultedState := &StateStore{records: faultedRecords}
			hooks := map[RecoveryStage]RecoveryHook{}
			if test.stage != "" {
				hooks[test.stage] = func(context.Context, *Runtime) error { return test.failure }
			}
			candidate := fixture.newRuntime(t, faultedState, currentRuntimeLifecycleIdentity(t), hooks)
			err = candidate.Start(context.Background())
			if !errors.Is(err, test.failure) {
				t.Fatalf("resource admission error = %v, want underlying %v", err, test.failure)
			}
			if candidate.Admission() != AdmissionClosed {
				t.Fatalf("resource failure left admission %q, want closed", candidate.Admission())
			}
			if record, _, readErr := fixture.state.ReadRuntime(context.Background()); readErr == nil && record.State == HostRuntimeReady {
				t.Fatalf("resource failure published false ready authority: %+v", record)
			} else if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read authority after resource failure: %v", readErr)
			}

			var preserved runtimeLifecycleAcceptedProbe
			if _, err := fixture.state.records.Read(context.Background(), "accepted/runtime-lifecycle-probe", &preserved); err != nil {
				t.Fatalf("read accepted probe after resource failure: %v", err)
			}
			if preserved != accepted {
				t.Fatalf("resource failure changed accepted work: got %+v want %+v", preserved, accepted)
			}
		})
	}
}

func newRuntimeLifecycleFixture(t *testing.T) *runtimeLifecycleFixture {
	t.Helper()
	root := testutil.ShortSocketRoot(t, "asr-", filepath.Join("run", "daemon.sock"))
	paths := ProductionPaths{
		ConfigurationRoot: filepath.Join(root, "config"),
		ConfigurationFile: filepath.Join(root, "config", "config.json"),
		StateRoot:         filepath.Join(root, "state"),
		RuntimeRoot:       filepath.Join(root, "run"),
		ControlEndpoint:   filepath.Join(root, "run", "daemon.sock"),
	}
	state, err := OpenStateStore(paths.StateRoot, 1<<20)
	if err != nil {
		t.Fatalf("open runtime lifecycle state: %v", err)
	}
	fixture := &runtimeLifecycleFixture{
		paths: paths,
		configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion, HostID: "lifecycle-host", HostName: "lifecycle-host",
			StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
		},
		state: state,
	}
	fixture.clock.Store(1_800_000_000_000)
	return fixture
}

func (fixture *runtimeLifecycleFixture) newRuntime(
	t *testing.T,
	state *StateStore,
	identity runtimeLifecycleIdentity,
	hooks map[RecoveryStage]RecoveryHook,
) *Runtime {
	t.Helper()
	manager, unit := runtimeLifecycleServiceIdentity()
	candidate, err := NewRuntime(RuntimeOptions{
		Paths: fixture.paths, Configuration: fixture.configuration, State: state,
		RuntimeVersion: "0.3.0", RuntimeIdentity: "sha256:runtime-lifecycle",
		PID: identity.pid, ProcStart: identity.procStart, StrongStart: identity.strongStart,
		ServiceManager: manager, ServiceUnit: unit,
		Now: func() time.Time { return time.UnixMilli(fixture.clock.Add(1)) }, RecoveryHooks: hooks,
	})
	if err != nil {
		t.Fatalf("construct runtime lifecycle candidate: %v", err)
	}
	return candidate
}

func (fixture *runtimeLifecycleFixture) seedRuntime(
	t *testing.T,
	generation uint64,
	identity runtimeLifecycleIdentity,
	state HostRuntimeState,
) HostRuntimeRecord {
	t.Helper()
	manager, unit := runtimeLifecycleServiceIdentity()
	record := HostRuntimeRecord{
		SchemaVersion: HostRuntimeSchemaVersion, Generation: generation,
		RuntimeVersion: "0.3.0", RuntimeIdentity: "sha256:runtime-lifecycle",
		HostID: fixture.configuration.HostID, HostName: fixture.configuration.HostName,
		PID: identity.pid, ProcStart: identity.procStart, StrongStart: identity.strongStart,
		ControlEndpoint: fixture.paths.ControlEndpoint, ServiceManager: manager, ServiceUnit: unit,
		StartedAt: fixture.clock.Add(1), CommittedAt: fixture.clock.Add(1), State: state, StateRevision: 1,
	}
	if _, err := fixture.state.CompareAndSwapRuntime(context.Background(), 0, record); err != nil {
		t.Fatalf("seed runtime authority: %v", err)
	}
	return record
}

func currentRuntimeLifecycleIdentity(t *testing.T) runtimeLifecycleIdentity {
	t.Helper()
	info := procinfo.Read(os.Getpid())
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		t.Fatalf("current process lacks exact lifecycle identity: %+v", info)
	}
	return runtimeLifecycleIdentity{pid: os.Getpid(), procStart: info.Start, strongStart: info.StrongStart}
}

func runtimeLifecycleServiceIdentity() (string, string) {
	if runtime.GOOS == "darwin" {
		return "launchd-user", "net.antst.agent-sessions"
	}
	return "systemd-user", "agent-sessions.service"
}

func acquireRuntimeLifecycleEndpointAfterCrash(
	t *testing.T,
	endpoint string,
	crashed HostRuntimeRecord,
) *ownedControlEndpoint {
	t.Helper()
	prior := procinfo.Process{PID: crashed.PID, Info: procinfo.Info{
		Status: procinfo.Known, Start: crashed.ProcStart, StrongStart: crashed.StrongStart,
	}}
	owned, err := acquireControlEndpoint(controlEndpointOptions{
		endpoint: endpoint, priorIdentity: &prior,
		processInfo: func(pid int) (procinfo.Info, error) {
			if pid != crashed.PID {
				return procinfo.Info{Status: procinfo.Unknown}, fmt.Errorf("process probe PID = %d, want %d", pid, crashed.PID)
			}
			return procinfo.Info{Status: procinfo.Absent}, nil
		},
	})
	if err != nil {
		t.Fatalf("acquire crash successor endpoint: %v", err)
	}
	return owned
}

func leaveRuntimeLifecycleOrphanedSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create orphaned socket parent: %v", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("create orphaned lifecycle socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close orphaned lifecycle socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}
