package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeRecoversAttachmentAndDeliveryAuthoritiesBeforeAdmission(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if runtime.Admission() != AdmissionReady || runtime.attachmentRegistry() == nil || runtime.deliveryEngine() == nil {
		t.Fatalf("runtime workflow authorities were not ready: admission=%q attachments=%p deliveries=%p", runtime.Admission(), runtime.attachmentRegistry(), runtime.deliveryEngine())
	}
	stages := runtime.CompletedRecoveryStages()
	if indexOfRecoveryStage(stages, RecoveryAttachments) >= indexOfRecoveryStage(stages, RecoveryRouting) {
		t.Fatalf("recovery order = %q", stages)
	}
}

func TestRuntimeLauncherPrepareAndConnectorHelloAdoptOneAttachment(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dispatch := runtimeControlDispatch(runtime)
	payload, err := json.Marshal(AttachmentPrepareRequest{
		Product: "qwen", Kind: "interactive", Cwd: "/workspace", Name: "qwen-test",
		Groups: []string{"peer-dev"}, ExpectedNativeActor: map[string]any{"pid": 71},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, failure := dispatch(context.Background(), controlPrincipal{Role: controlRoleLauncher}, controlRequest{
		Operation: "attachment.prepare", Payload: payload,
	})
	if failure != nil {
		t.Fatalf("prepare failed: %#v", failure)
	}
	var prepared AttachmentPrepareResult
	if err := json.Unmarshal(result.Result, &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Attachment.State != AttachmentStatePrepared || prepared.Capability == "" {
		t.Fatalf("prepared = %#v", prepared)
	}
	if prepared.Launch.Executable != "qwen" || prepared.Launch.Cwd != "/workspace" {
		t.Fatalf("launch plan = %#v", prepared.Launch)
	}
	authorize := authorizeForegroundHello(runtime)
	principal, rejected := authorize(context.Background(), controlPeerEvidence{UID: os.Getuid(), PID: 90, ProcStart: "connector-start"}, controlHello{
		Role: controlRoleConnector, Product: "qwen", AttachmentID: prepared.Attachment.AttachmentID,
		SessionID: "native-qwen-1", Capability: prepared.Capability, NativeActor: map[string]any{"pid": 71},
	})
	if rejected != nil {
		t.Fatalf("connector hello rejected: %#v", rejected)
	}
	if principal.AttachmentID != prepared.Attachment.AttachmentID || principal.SessionID != "native-qwen-1" {
		t.Fatalf("principal = %#v", principal)
	}
	record, err := runtime.attachmentRegistry().Select(context.Background(), AttachmentSelector{SessionID: "native-qwen-1"})
	if err != nil || record.State != AttachmentStateAttached {
		t.Fatalf("attached record = %#v, err=%v", record, err)
	}
}

func newWorkflowTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	root := t.TempDir()
	paths := ProductionPaths{
		ConfigurationRoot: filepath.Join(root, "config"), ConfigurationFile: filepath.Join(root, "config", "config.json"),
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), ControlEndpoint: filepath.Join(root, "run", "daemon.sock"),
	}
	state, err := OpenStateStore(paths.StateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &attachmentTestAdapter{}
	attachmentAdapters := map[string]AttachmentAdapter{"codex": adapter, "claude": adapter, "grok": adapter, "qwen": adapter}
	deliveryAdapter := &deliveryTestAdapter{counts: make(map[string]int)}
	deliveryAdapters := map[string]DeliveryAdapter{"codex": deliveryAdapter, "claude": deliveryAdapter, "grok": deliveryAdapter, "qwen": deliveryAdapter}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, State: state,
		Configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion, HostID: "workflow-host", HostName: "workflow-builder",
			StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
		},
		RuntimeVersion: "0.3.0-test", RuntimeIdentity: "sha256:workflow", PID: 9090,
		ProcStart: "workflow-start", StrongStart: "workflow-strong", ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: timeNowIncrementing(), AttachmentAdapters: attachmentAdapters, DeliveryAdapters: deliveryAdapters,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func timeNowIncrementing() func() time.Time {
	now := time.UnixMilli(2000)
	return func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}
}

func indexOfRecoveryStage(stages []RecoveryStage, want RecoveryStage) int {
	for index, stage := range stages {
		if stage == want {
			return index
		}
	}
	return -1
}
