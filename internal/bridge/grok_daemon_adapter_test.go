package bridge

import (
	"context"
	"errors"
	"reflect"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestGrokDaemonAdapterSelectsRosterSessionAndAttestsExactLeaderOwner(t *testing.T) {
	client := &grokDaemonTestClient{session: grokDaemonSession{
		SessionID: "grok-session-1", Name: "grok-review", Cwd: "/work", Profile: "profile-grok",
		OwnerPID: 6101, OwnerProcStart: "start-6101", LeaderSessionID: "leader-1", ACPReady: true,
	}}
	adapter := newGrokDaemonAdapter(client)
	selected, err := adapter.ResolveSelection(context.Background(), "grok-review")
	if err != nil || selected.SessionID != "grok-session-1" {
		t.Fatalf("resolve Grok roster selection: selected=%#v err=%v", selected, err)
	}
	record := daemonpkg.AttachmentRecord{
		SessionID: selected.SessionID, Product: "grok", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-grok"},
		NativeActor: map[string]any{
			"session_id": "grok-session-1", "owner_pid": 6101, "owner_proc_start": "start-6101",
			"leader_session_id": "leader-1", "acp_ready": true,
		},
	}
	actor, err := adapter.Corroborate(context.Background(), record, record.NativeActor)
	if err != nil {
		t.Fatalf("corroborate Grok actor: %v", err)
	}
	if actor["leader_session_id"] != "leader-1" || actor["acp_ready"] != (true) {
		t.Fatalf("Grok actor = %#v", actor)
	}
	client.session.OwnerProcStart = "replacement-owner"
	if _, err := adapter.Reconnect(context.Background(), record); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed Grok owner error = %v", err)
	}
}

func TestGrokDaemonAdapterDeliversByACPInterjectionAndReconnectsWithoutHost(t *testing.T) {
	client := &grokDaemonTestClient{session: grokDaemonSession{
		SessionID: "grok-session-2", Name: "grok-live", Cwd: "/work", Profile: "profile-grok",
		OwnerPID: 6201, OwnerProcStart: "start-6201", LeaderSessionID: "leader-2", ACPReady: true,
	}}
	adapter := newGrokDaemonAdapter(client)
	record := daemonpkg.AttachmentRecord{
		AttachmentID: "attachment-grok", SessionID: "grok-session-2", Product: "grok", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-grok"},
		NativeActor: map[string]any{
			"session_id": "grok-session-2", "owner_pid": 6201, "owner_proc_start": "start-6201",
			"leader_session_id": "leader-2", "acp_ready": true,
		},
	}
	frame := federation.AgentFrame{Version: federation.AgentFrameVersion, Type: "send", MessageID: "grok-message", Content: "hello"}
	if err := adapter.Deliver(context.Background(), record, frame); err != nil {
		t.Fatalf("deliver Grok frame: %v", err)
	}
	if client.interjectedSession != "grok-session-2" || len(client.frames) != 1 || !reflect.DeepEqual(client.frames[0], frame) {
		t.Fatalf("Grok interjection session=%q frames=%#v", client.interjectedSession, client.frames)
	}
	if _, err := adapter.Reconnect(context.Background(), record); err != nil {
		t.Fatalf("reconnect Grok actor: %v", err)
	}
	if client.hostLaunches != 0 {
		t.Fatalf("Grok reconnect launched %d obsolete host process(es)", client.hostLaunches)
	}
}

type grokDaemonTestClient struct {
	session            grokDaemonSession
	ambiguous          bool
	hostLaunches       int
	interjectedSession string
	frames             []federation.AgentFrame
}

func (client *grokDaemonTestClient) PrepareInteractive(_ context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "grok", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *grokDaemonTestClient) ResolveSession(_ context.Context, selector string) (grokDaemonSession, bool, error) {
	if client.ambiguous {
		return grokDaemonSession{}, true, nil
	}
	if selector != client.session.Name && selector != client.session.SessionID {
		return grokDaemonSession{}, false, daemonpkg.ErrAttachmentNotFound
	}
	return client.session, false, nil
}

func (client *grokDaemonTestClient) ObserveSession(_ context.Context, _ daemonpkg.AttachmentRecord, _ int) (grokDaemonSession, error) {
	return client.session, nil
}

func (client *grokDaemonTestClient) InspectSession(_ context.Context, _ string) (grokDaemonSession, error) {
	return client.session, nil
}

func (client *grokDaemonTestClient) InterjectFrame(_ context.Context, sessionID string, frame federation.AgentFrame) error {
	client.interjectedSession = sessionID
	client.frames = append(client.frames, frame)
	return nil
}
