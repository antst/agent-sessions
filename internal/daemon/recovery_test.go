package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

func TestComposeRecoveryHooksRejectsOutOfOrderOrDuplicateStages(t *testing.T) {
	noop := func(context.Context, *Runtime) error { return nil }
	if _, err := ComposeRecoveryHooks(
		RecoveryStep{Stage: RecoveryCatalog, Run: noop},
		RecoveryStep{Stage: RecoveryLoadConfiguration, Run: noop},
	); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("out-of-order error = %v", err)
	}
	if _, err := ComposeRecoveryHooks(
		RecoveryStep{Stage: RecoveryCatalog, Run: noop},
		RecoveryStep{Stage: RecoveryCatalog, Run: noop},
	); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
	hooks, err := ComposeRecoveryHooks(
		RecoveryStep{Stage: RecoveryValidateAuthority, Run: noop},
		RecoveryStep{Stage: RecoveryCatalog, Run: noop},
	)
	if err != nil || len(hooks) != 2 {
		t.Fatalf("valid hooks = %v, %v", hooks, err)
	}
}

func TestLifecycleDebtIsBoundedCASProtectedAndReobserved(t *testing.T) {
	runtime := newRecoveryTestRuntime(t)
	ctx := context.Background()
	record, err := runtime.RecordLifecycleDebt(ctx, LifecycleDebtInput{
		DebtID: "debt-1", Operation: "attachment.reconcile", ResourceKind: "attachment",
		ResourceIdentity: "attachment-1", ExpectedRevision: "7", ObservedRevision: "8",
		CauseCode: "identity_changed", CauseDetail: strings.Repeat("detail ", diagnostics.MaxCauseDetailBytes),
		RetryPredicate: "exact actor identity matches", ProhibitedScope: "do not signal or replace actor",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(record.CauseDetail) > diagnostics.MaxCauseDetailBytes || record.ResolvedAt != 0 {
		t.Fatalf("created debt = %+v", record)
	}
	firstFailure := errors.New("still changed\nprivate tail")
	got, err := runtime.RetryLifecycleDebt(ctx, record.DebtID, func(_ context.Context, observed DebtRecord) error {
		if !reflect.DeepEqual(observed.ResourceIdentity, record.ResourceIdentity) {
			t.Fatalf("retry changed resource identity: %+v", observed)
		}
		return firstFailure
	})
	if !errors.Is(err, firstFailure) || strings.ContainsAny(got.CauseDetail, "\n\r\t") || got.Revision != 2 {
		t.Fatalf("failed retry = %+v, %v", got, err)
	}
	got, err = runtime.RetryLifecycleDebt(ctx, record.DebtID, func(context.Context, DebtRecord) error { return nil })
	if err != nil || got.ResolvedAt == 0 || got.Revision != 3 {
		t.Fatalf("resolved retry = %+v, %v", got, err)
	}
}

func newRecoveryTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	root := t.TempDir()
	state, err := OpenStateStore(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	paths := ProductionPaths{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), ControlEndpoint: filepath.Join(root, "run", "daemon.sock")}
	configuration := DaemonConfig{SchemaVersion: 1, HostID: "host", HostName: "builder", StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1}
	now := int64(100)
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, Configuration: configuration, State: state, RuntimeVersion: "0.3.0",
		RuntimeIdentity: "sha256:test", PID: 42, ProcStart: "start", StrongStart: "strong",
		ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: func() time.Time { now++; return time.UnixMilli(now) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}
