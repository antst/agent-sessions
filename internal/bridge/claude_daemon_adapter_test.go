package bridge

import (
	"context"
	"errors"
	"reflect"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestClaudeDaemonAdapterResolvesUUIDAndUniqueName(t *testing.T) {
	client := &claudeDaemonTestClient{session: claudeDaemonSession{
		SessionID: "11111111-2222-4333-8444-555555555555", Name: "reviewer", Cwd: "/work",
		Profile: "profile-claude", PID: 5101, ProcStart: "start-5101", Socket: "/tmp/claude.sock",
		SyntheticService: true,
	}}
	adapter := newClaudeDaemonAdapter(client)
	for _, selector := range []string{"11111111-2222-4333-8444-555555555555", "reviewer"} {
		selection, err := adapter.ResolveSelection(context.Background(), selector)
		if err != nil {
			t.Fatalf("resolve Claude selector %q: %v", selector, err)
		}
		if selection.SessionID != client.session.SessionID {
			t.Fatalf("Claude selector %q resolved %#v", selector, selection)
		}
	}
	client.ambiguous = true
	if _, err := adapter.ResolveSelection(context.Background(), "reviewer"); !errors.Is(err, daemonpkg.ErrAttachmentAmbiguous) {
		t.Fatalf("ambiguous Claude name error = %v", err)
	}
}

func TestClaudeDaemonAdapterAttestsSocketProcessAndSyntheticService(t *testing.T) {
	client := &claudeDaemonTestClient{session: claudeDaemonSession{
		SessionID: "11111111-2222-4333-8444-555555555555", Name: "reviewer", Cwd: "/work",
		Profile: "profile-claude", PID: 5201, ProcStart: "start-5201", Socket: "/tmp/claude.sock",
		SyntheticService: true,
	}}
	adapter := newClaudeDaemonAdapter(client)
	record := daemonpkg.AttachmentRecord{
		SessionID: client.session.SessionID, Product: "claude", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-claude"},
		NativeActor: map[string]any{
			"session_id": client.session.SessionID, "pid": 5201, "proc_start": "start-5201",
			"socket": "/tmp/claude.sock", "synthetic_service": true,
		},
	}
	actor, err := adapter.Corroborate(context.Background(), record, record.NativeActor)
	if err != nil {
		t.Fatalf("attest Claude actor: %v", err)
	}
	if actor["socket"] != "/tmp/claude.sock" || actor["synthetic_service"] != (true) {
		t.Fatalf("Claude actor = %#v", actor)
	}

	client.session.SyntheticService = false
	if _, err := adapter.Reconnect(context.Background(), record); !errors.Is(err, ErrClaudeSyntheticServiceUnavailable) {
		t.Fatalf("missing synthetic service error = %v", err)
	}
	client.session.SyntheticService = true
	client.session.Socket = "/tmp/replaced.sock"
	if _, err := adapter.Reconnect(context.Background(), record); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed Claude socket error = %v", err)
	}
}

func TestClaudeDaemonAdapterDeliversThroughNativeSocketAndReconnects(t *testing.T) {
	client := &claudeDaemonTestClient{session: claudeDaemonSession{
		SessionID: "11111111-2222-4333-8444-555555555555", Cwd: "/work", Profile: "profile-claude",
		PID: 5301, ProcStart: "start-5301", Socket: "/tmp/claude.sock", SyntheticService: true,
	}}
	adapter := newClaudeDaemonAdapter(client)
	record := daemonpkg.AttachmentRecord{
		AttachmentID: "attachment-claude", SessionID: client.session.SessionID, Product: "claude", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-claude"},
		NativeActor: map[string]any{
			"session_id": client.session.SessionID, "pid": 5301, "proc_start": "start-5301",
			"socket": "/tmp/claude.sock", "synthetic_service": true,
		},
	}
	frame := federation.AgentFrame{Version: federation.AgentFrameVersion, Type: "send", MessageID: "claude-message", Content: "hello"}
	if err := adapter.Deliver(context.Background(), record, frame); err != nil {
		t.Fatalf("deliver Claude frame: %v", err)
	}
	if client.deliverySocket != "/tmp/claude.sock" || len(client.frames) != 1 || !reflect.DeepEqual(client.frames[0], frame) {
		t.Fatalf("Claude delivery socket=%q frames=%#v", client.deliverySocket, client.frames)
	}
	if _, err := adapter.Reconnect(context.Background(), record); err != nil {
		t.Fatalf("reconnect Claude actor: %v", err)
	}
	if client.launchCalls != 0 {
		t.Fatalf("Claude reconnect launched %d replacement process(es)", client.launchCalls)
	}
}

type claudeDaemonTestClient struct {
	session        claudeDaemonSession
	ambiguous      bool
	launchCalls    int
	deliverySocket string
	frames         []federation.AgentFrame
}

func (client *claudeDaemonTestClient) PrepareInteractive(_ context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "claude", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *claudeDaemonTestClient) ResolveSession(_ context.Context, selector string) (claudeDaemonSession, bool, error) {
	if client.ambiguous && selector == client.session.Name {
		return claudeDaemonSession{}, true, nil
	}
	if selector != client.session.Name && selector != client.session.SessionID {
		return claudeDaemonSession{}, false, daemonpkg.ErrAttachmentNotFound
	}
	return client.session, false, nil
}

func (client *claudeDaemonTestClient) InspectSession(_ context.Context, _ string) (claudeDaemonSession, error) {
	return client.session, nil
}

func (client *claudeDaemonTestClient) DeliverFrame(_ context.Context, socket string, frame federation.AgentFrame) error {
	client.deliverySocket = socket
	client.frames = append(client.frames, frame)
	return nil
}
