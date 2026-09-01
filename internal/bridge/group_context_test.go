package bridge

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestQwenParentChildAnchorsInheritanceExclusionAndNotice(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	procStart := readProcStart(os.Getpid())
	socket := filepath.Join(root, "qwen-parent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go drainTestListener(listener)

	const parentID = "qwen-parent-session"
	if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: parentID, Product: "qwen", Groups: []string{"qwen-project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: parentID, Product: "qwen",
		Name: "qwen-parent", PID: os.Getpid(), ProcStart: procStart, Socket: socket,
		QwenCapabilityDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if _, err := federator.RegisterPeer(runtimeDir, registration); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = federator.UnregisterPeer(runtimeDir, registration) })
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Setenv(peerSessionIDEnvironment, parentID)
	t.Setenv("AGENT_SESSIONS_PRODUCT", "qwen")
	t.Setenv(remoteParentEnvironment, "")
	t.Setenv("CODEX_THREAD_ID", "")
	owner := laneOwner{PID: os.Getpid(), ProcStart: procStart, SessionID: parentID, PermissionMode: "default"}

	const childID = "qwen-child-session"
	launch := withQwenLaneResolvedParent(qwenLaneOptions{laneCommonOptions: laneCommonOptions{command: "start"}}, owner)
	if launch.ownerSessionID != parentID || launch.notifyTarget != "session:"+parentID {
		t.Fatalf("Qwen child ownership = %+v", launch)
	}
	plain, _, err := resolveLaneGroupState(childID, "qwen", launch.groupOptions, false, true)
	if err != nil {
		t.Fatal(err)
	}
	assertContainsGroup(t, plain.Groups, "session:test-host/"+parentID)
	assertContainsGroup(t, plain.Groups, "session:test-host/"+childID)
	if containsString(plain.Groups, "qwen-project") || containsString(plain.Groups, "unrelated-project") {
		t.Fatalf("default Qwen child inherited a project group: %v", plain.Groups)
	}

	inheritedLaunch := withQwenLaneResolvedParent(qwenLaneOptions{
		laneCommonOptions: laneCommonOptions{
			command: "start", groupOptions: laneGroupOptions{inheritParentGroups: true, inheritGroupsSpecified: true},
		},
	}, owner)
	inherited, _, err := resolveLaneGroupState(childID+"-inherited", "qwen", inheritedLaunch.groupOptions, false, true)
	if err != nil {
		t.Fatal(err)
	}
	assertContainsGroup(t, inherited.Groups, "qwen-project")
	if containsString(inherited.Groups, "unrelated-project") {
		t.Fatalf("Qwen child gained an unrelated group: %v", inherited.Groups)
	}

	turn := qwenLaneTurn{ID: randomID(), Status: "completed", Outcome: "completed", Exit: 0}
	state := qwenLaneState{
		Name: "qwen-child", ThreadID: childID, NotifyTarget: launch.notifyTarget,
		ParentSessionID: parentID, ParentHostID: "test-host", Groups: plain.Groups,
	}
	queueQwenLaneTerminalNotice(&state, turn)
	if len(state.Notices) != 1 || state.Notices[0].Target != "session:"+parentID ||
		!strings.Contains(state.Notices[0].Message, "Collect: qwen-peer-lane wait "+childID) {
		t.Fatalf("Qwen parent notice = %+v", state.Notices)
	}
}
