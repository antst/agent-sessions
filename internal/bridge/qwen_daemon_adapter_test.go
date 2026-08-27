package bridge

import (
	"context"
	"errors"
	"reflect"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestQwenDaemonAdapterRequiresReadinessDualOutputAndExactAncestry(t *testing.T) {
	client := &qwenDaemonTestClient{session: qwenDaemonSession{
		SessionID: "qwen-session-1", Name: "qwen-review", Cwd: "/work", Profile: "profile-qwen",
		PID: 7101, ProcStart: "start-7101", ParentPID: 7001,
		EventPath: "/tmp/qwen-event", InputPath: "/tmp/qwen-input", ReadinessPath: "/tmp/qwen-ready",
		Ready: true, DualOutput: true,
	}}
	adapter := newQwenDaemonAdapter(client)
	record := daemonpkg.AttachmentRecord{
		SessionID: "qwen-session-1", Product: "qwen", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-qwen"},
		NativeActor: map[string]any{
			"session_id": "qwen-session-1", "pid": 7101, "proc_start": "start-7101", "parent_pid": 7001,
			"event_path": "/tmp/qwen-event", "input_path": "/tmp/qwen-input", "readiness_path": "/tmp/qwen-ready",
			"dual_output": true,
		},
	}
	actor, err := adapter.Corroborate(context.Background(), record, record.NativeActor)
	if err != nil {
		t.Fatalf("corroborate Qwen actor: %v", err)
	}
	if actor["event_path"] != "/tmp/qwen-event" || actor["input_path"] != "/tmp/qwen-input" {
		t.Fatalf("Qwen actor = %#v", actor)
	}

	client.session.DualOutput = false
	if _, err := adapter.Reconnect(context.Background(), record); !errors.Is(err, ErrQwenDualOutputUnavailable) {
		t.Fatalf("missing Qwen dual output error = %v", err)
	}
	client.session.DualOutput = true
	client.session.ParentPID = 7999
	if _, err := adapter.Reconnect(context.Background(), record); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed Qwen ancestry error = %v", err)
	}
	client.session.ParentPID = 7001
	client.session.Ready = false
	if _, err := adapter.Reconnect(context.Background(), record); !errors.Is(err, ErrQwenReadinessUnavailable) {
		t.Fatalf("missing Qwen readiness error = %v", err)
	}
}

func TestQwenDaemonAdapterUsesNativeInputAndEventAndReconnectsWithoutHost(t *testing.T) {
	client := &qwenDaemonTestClient{session: qwenDaemonSession{
		SessionID: "qwen-session-2", Name: "qwen-live", Cwd: "/work", Profile: "profile-qwen",
		PID: 7201, ProcStart: "start-7201", ParentPID: 7002,
		EventPath: "/tmp/qwen-event-2", InputPath: "/tmp/qwen-input-2", ReadinessPath: "/tmp/qwen-ready-2",
		Ready: true, DualOutput: true,
	}}
	adapter := newQwenDaemonAdapter(client)
	selected, err := adapter.ResolveSelection(context.Background(), "qwen-live")
	if err != nil || selected.SessionID != "qwen-session-2" {
		t.Fatalf("resolve Qwen selection: selected=%#v err=%v", selected, err)
	}
	record := daemonpkg.AttachmentRecord{
		AttachmentID: "attachment-qwen", SessionID: "qwen-session-2", Product: "qwen", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-qwen"},
		NativeActor: map[string]any{
			"session_id": "qwen-session-2", "pid": 7201, "proc_start": "start-7201", "parent_pid": 7002,
			"event_path": "/tmp/qwen-event-2", "input_path": "/tmp/qwen-input-2", "readiness_path": "/tmp/qwen-ready-2",
			"dual_output": true,
		},
	}
	frame := federation.AgentFrame{Version: federation.AgentFrameVersion, Type: "send", MessageID: "qwen-message", Content: "hello"}
	if err := adapter.Deliver(context.Background(), record, frame); err != nil {
		t.Fatalf("deliver Qwen frame: %v", err)
	}
	if client.inputPath != "/tmp/qwen-input-2" || len(client.frames) != 1 || !reflect.DeepEqual(client.frames[0], frame) {
		t.Fatalf("Qwen input path=%q frames=%#v", client.inputPath, client.frames)
	}
	if _, err := adapter.Reconnect(context.Background(), record); err != nil {
		t.Fatalf("reconnect Qwen actor: %v", err)
	}
	if client.hostLaunches != 0 {
		t.Fatalf("Qwen reconnect launched %d obsolete host process(es)", client.hostLaunches)
	}
}

type qwenDaemonTestClient struct {
	session      qwenDaemonSession
	ambiguous    bool
	hostLaunches int
	inputPath    string
	frames       []federation.AgentFrame
}

func (client *qwenDaemonTestClient) PrepareInteractive(_ context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "qwen", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *qwenDaemonTestClient) ResolveSession(_ context.Context, selector string) (qwenDaemonSession, bool, error) {
	if client.ambiguous {
		return qwenDaemonSession{}, true, nil
	}
	if selector != client.session.Name && selector != client.session.SessionID {
		return qwenDaemonSession{}, false, daemonpkg.ErrAttachmentNotFound
	}
	return client.session, false, nil
}

func (client *qwenDaemonTestClient) ObserveSession(_ context.Context, _ daemonpkg.AttachmentRecord, _ int) (qwenDaemonSession, error) {
	return client.session, nil
}

func (client *qwenDaemonTestClient) InspectSession(_ context.Context, _ string) (qwenDaemonSession, error) {
	return client.session, nil
}

func (client *qwenDaemonTestClient) WriteInput(_ context.Context, path string, frame federation.AgentFrame) error {
	client.inputPath = path
	client.frames = append(client.frames, frame)
	return nil
}
