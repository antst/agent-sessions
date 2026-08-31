package federator

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
)

func TestHubLossCannotBeHiddenByFullLaneOutputQueue(t *testing.T) {
	pending := &pendingLane{responses: make(chan Message, 1), failed: make(chan string, 1)}
	pending.responses <- Message{Type: "lane_stdout", Data: []byte("blocked")}
	agent := &agent{pendingLanes: map[string]*pendingLane{"request": pending}}
	agent.failPendingLanes("hub is disconnected")
	select {
	case reason := <-pending.failed:
		if reason != "hub is disconnected" {
			t.Fatalf("failure = %q", reason)
		}
	default:
		t.Fatal("hub-loss signal was dropped behind output backpressure")
	}
}

func TestFullLaneOutputQueueFailsProxyInsteadOfDroppingTerminal(t *testing.T) {
	hubSide, peerSide := net.Pipe()
	defer func() { _ = hubSide.Close() }()
	defer func() { _ = peerSide.Close() }()
	pending := &pendingLane{responses: make(chan Message, 1), failed: make(chan string, 1)}
	pending.responses <- Message{Type: "lane_stdout", Data: []byte("blocked")}
	agent := &agent{network: newWireConn(hubSide), pendingLanes: map[string]*pendingLane{"request": pending}}
	cancels := make(chan Message, 2)
	go func() {
		_ = scanMessages(peerSide, func(message Message) error {
			cancels <- message
			return nil
		})
	}()
	agent.deliverLaneResponse(Message{Type: "lane_exit", RequestID: "request"})
	agent.deliverLaneResponse(Message{Type: "lane_exit", RequestID: "request"})
	select {
	case reason := <-pending.failed:
		if reason == "" {
			t.Fatal("proxy overflow returned an empty failure")
		}
	default:
		t.Fatal("terminal frame was silently dropped behind output backpressure")
	}
	select {
	case message := <-cancels:
		if message.Type != "lane_cancel" || message.RequestID != "request" {
			t.Fatalf("cancel = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("overflow did not cancel the remote lane")
	}
	select {
	case duplicate := <-cancels:
		t.Fatalf("overflow emitted duplicate cancel: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPrepareRemoteLaneInjectsPersistentGroupedParentNotice(t *testing.T) {
	parent := groupedRemoteLaneParent("host-a", "codex")
	agent := &agent{
		options: AgentOptions{EnableRemoteLanes: true, CodexLaneExecutable: "/bin/true"},
		remote:  map[string]Peer{"host-a/source": parent},
		network: &wireConn{},
	}
	executable, args, err := agent.prepareRemoteLane(Message{
		Product: "codex", SourceID: "host-a/source", Args: []string{"start", "--name", "worker", "-"},
		ParentContext: groupedRemoteLaneParentContext(parent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if executable != "/bin/true" {
		t.Fatalf("executable = %q", executable)
	}
	want := []string{"start", "--name", "worker", "-", "--persistent", "--notify", "host-a/source"}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
	}

	_, _, err = agent.prepareRemoteLane(Message{
		Product: "codex", SourceID: "host-a/source", Args: []string{"resume", "worker", "--persistent"},
	})
	if err == nil {
		t.Fatal("caller-controlled remote lifecycle was accepted")
	}

	_, _, err = agent.prepareRemoteLane(Message{
		Product: "codex", SourceID: "host-a/source", Args: []string{"start", "--name", "worker", "--no-auto-archive", "-"},
	})
	if err == nil {
		t.Fatal("remote caller disabled the destination cleanup fuse")
	}
	for _, invalid := range []string{"0", "NaN", "+Inf", "86401"} {
		_, _, err = agent.prepareRemoteLane(Message{
			Product: "codex", SourceID: "host-a/source",
			Args: []string{"start", "--name", "worker", "--auto-archive-after", invalid, "-"},
		})
		if err == nil {
			t.Fatalf("remote caller selected invalid cleanup grace %q", invalid)
		}
	}
}

func TestPrepareRemoteGrokLaneUsesAdvertisedLauncher(t *testing.T) {
	agent := &agent{
		options: AgentOptions{EnableRemoteLanes: true, GrokLaneExecutable: "/bin/true"},
		remote:  map[string]Peer{"host-a/source": groupedRemoteLaneParent("host-a", "grok")}, network: &wireConn{},
	}
	parent := agent.remote["host-a/source"]
	executable, args, err := agent.prepareRemoteLane(Message{
		Product: "grok", SourceID: "host-a/source", Args: []string{"start", "--name", "grok-worker", "-"},
		ParentContext: groupedRemoteLaneParentContext(parent),
	})
	if err != nil || executable != "/bin/true" {
		t.Fatalf("prepare remote Grok lane = %q, %#v, %v", executable, args, err)
	}
	want := []string{"start", "--name", "grok-worker", "-", "--persistent", "--notify", "host-a/source"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("remote Grok args = %#v, want %#v", args, want)
	}
	capabilities := agent.laneCapabilities()
	if len(capabilities) != 1 || capabilities[0] != CapabilityGrokLane {
		t.Fatalf("remote Grok capabilities = %#v", capabilities)
	}
}

func TestPrepareRemoteQwenLaneUsesAdvertisedReadyLauncher(t *testing.T) {
	agent := &agent{
		options: AgentOptions{EnableRemoteLanes: true, QwenLaneExecutable: "/bin/true"},
		remote:  map[string]Peer{"host-a/source": groupedRemoteLaneParent("host-a", "qwen")}, network: &wireConn{},
	}
	parent := agent.remote["host-a/source"]
	executable, args, err := agent.prepareRemoteLane(Message{
		Product: "qwen", SourceID: "host-a/source", Args: []string{"start", "--name", "qwen-worker", "--no-yolo", "-"},
		ParentContext: groupedRemoteLaneParentContext(parent),
	})
	if err != nil || executable != "/bin/true" {
		t.Fatalf("prepare remote Qwen lane = %q, %#v, %v", executable, args, err)
	}
	want := []string{"start", "--name", "qwen-worker", "--no-yolo", "-", "--persistent", "--notify", "host-a/source"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("remote Qwen args = %#v, want %#v", args, want)
	}
	capabilities := agent.laneCapabilities()
	if len(capabilities) != 1 || capabilities[0] != CapabilityQwenLane {
		t.Fatalf("remote Qwen capabilities = %#v", capabilities)
	}
}

func TestConfiguredQwenRemoteLaneRequiresSoleReadinessEngine(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "qwen-peer-lane")
	native := filepath.Join(t.TempDir(), "qwen")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := evaluateQwenLaneReadiness
	evaluateQwenLaneReadiness = func(got string) error {
		if got != native {
			t.Fatalf("readiness executable = %q, want native %q (lane launcher %q)", got, native, executable)
		}
		return errors.New("native archive unavailable")
	}
	t.Cleanup(func() { evaluateQwenLaneReadiness = previous })
	options := AgentOptions{EnableRemoteLanes: true, QwenLaneExecutable: executable, QwenExecutable: native}
	if err := configureLaneExecutables(&options); err == nil || !strings.Contains(err.Error(), "native archive unavailable") {
		t.Fatalf("unready configured Qwen launcher = %v", err)
	}
}

func TestRemoteQwenLaneInheritsExactNativeExecutable(t *testing.T) {
	options := AgentOptions{
		RuntimeDir: "/runtime", ClaudeConfigDir: "/claude", QwenExecutable: "/opt/qwen/bin/qwen",
	}
	updates := remoteLaneEnvironmentUpdates(options, "qwen", `{"session_id":"parent"}`)
	if got := updates["QWEN_PEER_QWEN_BIN"]; got != options.QwenExecutable {
		t.Fatalf("remote Qwen executable = %q, want %q", got, options.QwenExecutable)
	}
	for _, product := range []string{"codex", "claude", "grok"} {
		if value, exists := remoteLaneEnvironmentUpdates(options, product, "{}")["QWEN_PEER_QWEN_BIN"]; exists {
			t.Fatalf("remote %s lane inherited Qwen executable %q", product, value)
		}
	}
}

func groupedRemoteLaneParent(hostID, product string) Peer {
	sessionID := "source"
	return Peer{
		ID: hostID + "/" + sessionID, HostID: hostID, SessionID: sessionID,
		GlobalID: globalSessionID(hostID, sessionID), Name: product + "-parent", DisplayName: product + "-parent",
		Entrypoint: product, PeerProtocol: GroupProtocolVersion, InstanceID: "instance-" + sessionID,
		Groups: []string{"project", privateGroupPrefix + hostID + "/" + sessionID},
	}
}

func groupedRemoteLaneParentContext(peer Peer) *ParentContext {
	return &ParentContext{
		HostID: peer.HostID, SessionID: peer.SessionID, Product: peer.Entrypoint,
		InstanceID: peer.InstanceID, Groups: append([]string(nil), peer.Groups...),
	}
}

func TestRemoteLaneExecutionRequiresExplicitEnable(t *testing.T) {
	agent := &agent{options: AgentOptions{CodexLaneExecutable: "/bin/true"}}
	if capabilities := agent.laneCapabilities(); len(capabilities) != 0 {
		t.Fatalf("disabled agent advertised remote execution: %#v", capabilities)
	}
	if _, _, err := agent.prepareRemoteLane(Message{Product: "codex", Args: []string{"list"}}); err == nil {
		t.Fatal("disabled agent accepted a remote lane command")
	}
}

func TestConfiguredRemoteLaneLauncherMustExist(t *testing.T) {
	options := AgentOptions{
		EnableRemoteLanes:   true,
		CodexLaneExecutable: filepath.Join(t.TempDir(), "missing-codex-peer-lane"),
	}
	if err := configureLaneExecutables(&options); err == nil {
		t.Fatal("invalid explicit launcher override was silently ignored")
	}
}

func TestRemoteLaneArgBounds(t *testing.T) {
	tooMany := make([]string, maxRemoteLaneArgs+1)
	if err := validateRemoteLaneArgBounds(tooMany); err == nil {
		t.Fatal("oversized argv count was accepted")
	}
	if err := validateRemoteLaneArgBounds([]string{strings.Repeat("x", maxRemoteLaneArgBytes+1)}); err == nil {
		t.Fatal("oversized argv bytes were accepted")
	}
}

func TestRemoteLaneUsesAgentClaudeCredentialNamespaceExactly(t *testing.T) {
	profile := claudeprofile.Source{
		ConfigRoot: "/shared", ConfigEnvSet: true, ConfigEnvValue: "/shared/../shared",
		SecureEnvSet: true, SecureConfig: "",
	}
	environment := claudeProfileEnvironment([]string{
		"PATH=/bin", "CLAUDE_CONFIG_DIR=/wrong", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/wrong-secure",
	}, profile)
	for _, expected := range []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/shared/../shared", "CLAUDE_SECURESTORAGE_CONFIG_DIR="} {
		found := false
		for _, entry := range environment {
			found = found || entry == expected
		}
		if !found {
			t.Fatalf("remote lane environment missing %q: %v", expected, environment)
		}
	}
	unset := claudeProfileEnvironment(environment, claudeprofile.Source{ConfigRoot: "/default"})
	for _, entry := range unset {
		if strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(entry, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") {
			t.Fatalf("unset agent namespace retained %q", entry)
		}
	}
}

func TestRemoteLaneConcurrencyIsBounded(t *testing.T) {
	agent := &agent{laneRuns: map[string]*laneRun{}}
	for index := 0; index < maxRemoteLaneRuns; index++ {
		agent.laneRuns["existing-"+strconv.Itoa(index)] = &laneRun{}
	}
	agent.startRemoteLane(Message{RequestID: "rejected"})
	if _, exists := agent.laneRuns["rejected"]; exists {
		t.Fatal("remote lane concurrency limit was exceeded")
	}
}

func TestResolveRemoteHostRequiresLiveHub(t *testing.T) {
	agent := &agent{
		remoteHosts: map[string]Host{
			"host-b": {ID: "host-b", Name: "beta", Capabilities: []string{CapabilityCodexLane}},
		},
	}
	if _, err := agent.resolveRemoteHost("host-b", CapabilityCodexLane); err == nil {
		t.Fatal("remote host resolved while hub was disconnected")
	}
}

func TestResolveRemoteQwenHostNeverFallsBack(t *testing.T) {
	networkSide, peerSide := net.Pipe()
	defer func() { _ = networkSide.Close(); _ = peerSide.Close() }()
	agent := &agent{
		network: newWireConn(networkSide),
		remoteHosts: map[string]Host{
			"selected":  {ID: "selected", Name: "selected", Capabilities: nil},
			"alternate": {ID: "alternate", Name: "alternate", Capabilities: []string{CapabilityQwenLane}},
		},
	}
	if _, err := agent.resolveRemoteHost("selected", CapabilityQwenLane); err == nil ||
		!strings.Contains(err.Error(), "does not advertise") {
		t.Fatalf("uncapable selected Qwen host = %v", err)
	}
	if _, err := agent.resolveRemoteHost("missing", CapabilityQwenLane); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("missing selected Qwen host = %v", err)
	}
	if got, err := agent.resolveRemoteHost("alternate", CapabilityQwenLane); err != nil || got.ID != "alternate" {
		t.Fatalf("explicit capable Qwen host = %+v, %v", got, err)
	}
}

func TestPrepareRemoteLaneRequiresLiveHubForEverySubcommand(t *testing.T) {
	agent := &agent{
		options: AgentOptions{EnableRemoteLanes: true, CodexLaneExecutable: "/bin/true"},
		remote:  map[string]Peer{"host-a/source": {ID: "host-a/source"}},
	}
	if _, _, err := agent.prepareRemoteLane(Message{
		Product: "codex", SourceID: "host-a/source", Args: []string{"list", "--all"},
	}); err == nil {
		t.Fatal("non-lifecycle command was accepted while the hub was disconnected")
	}
}

func TestRemoteLaneStdinDetectionMatchesNativeLifecycle(t *testing.T) {
	tests := []struct {
		product string
		args    []string
		want    bool
	}{
		{product: "codex", args: []string{"start", "--name", "worker"}, want: true},
		{product: "codex", args: []string{"run", "--name", "worker", "-"}, want: true},
		{product: "claude", args: []string{"resume", "worker"}, want: true},
		{product: "grok", args: []string{"resume", "worker"}, want: true},
		{product: "qwen", args: []string{"resume", "worker"}, want: true},
		{product: "claude", args: []string{"start", "--prompt-file", "brief.md"}, want: false},
		{product: "codex", args: []string{"start", "--name", "--prompt-file"}, want: true},
		{product: "claude", args: []string{"start", "--tools", "--prompt-file"}, want: true},
		{product: "codex", args: []string{"wait", "worker", "-"}, want: false},
	}
	for _, test := range tests {
		if got := remoteLaneReadsStdin(test.product, test.args); got != test.want {
			t.Fatalf("remoteLaneReadsStdin(%q, %q) = %v, want %v", test.product, test.args, got, test.want)
		}
	}
}

func TestAutomaticRemoteLaneSourceInferenceRejectsInheritedEnvironment(t *testing.T) {
	t.Setenv("AGENT_SESSIONS_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "stale-codex-session")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "stale-claude-session")
	t.Setenv("CLAUDE_PID", "999999")
	if got := inferRemoteLaneSourceSession(1); got != "" {
		t.Fatalf("inherited environment was attributed to %q", got)
	}
}

func TestRemoteLaneSourceResolvesLateBoundAttachmentAlias(t *testing.T) {
	const actual = "3c0c0831-9bd7-40db-9ee3-e108f315ea57"
	const attachment = "019fe660-1c86-7700-b462-6ff16de00fc5"
	want := localPeer{Peer: Peer{SessionID: actual, Name: "selected"}, AttachmentID: attachment}
	peers := map[string]localPeer{"host/" + actual: want}
	for _, sourceID := range []string{actual, attachment} {
		got, ok := localPeerBySession(peers, sourceID)
		if !ok || got.SessionID != actual || got.AttachmentID != attachment {
			t.Fatalf("source %q resolved to %+v, %v", sourceID, got, ok)
		}
	}
}

func TestPermissionModeFromProcessArgsPreservesArgumentBoundaries(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"claude", "--permission-mode", "bypassPermissions"}, want: "bypassPermissions"},
		{args: []string{"claude", "--permission-mode=bypassPermissions"}, want: "bypassPermissions"},
		{args: []string{"codex", "--ask-for-approval", "never"}, want: "bypassPermissions"},
		{args: []string{"codex", "-a", "never"}, want: "bypassPermissions"},
		{args: []string{"codex", "-a=never"}, want: "bypassPermissions"},
		{args: []string{"codex", "-anever"}, want: "bypassPermissions"},
		{args: []string{"codex", "--ask-for-approval=never"}, want: "bypassPermissions"},
		{args: []string{"codex", "--yolo"}, want: "bypassPermissions"},
		{args: []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}, want: "bypassPermissions"},
		{args: []string{"grok", "--permission-mode", "always-approve", "--permission-mode", "default"}, want: "default"},
		{args: []string{"grok", "--permission-mode=default", "--always-approve"}, want: "bypassPermissions"},
		{args: []string{"claude", "--dangerously-skip-permissions", "--permission-mode", "plan"}, want: "bypassPermissions"},
		{args: []string{"claude", "--dangerously-skip-permissions", "--dangerously-skip-permissions=false"}, want: "default"},
		{args: []string{"claude", "--dangerously-skip-permissions=false", "--permission-mode", "bypassPermissions"}, want: "bypassPermissions"},
		{args: []string{"claude", "-p", "explain --dangerously-skip-permissions"}, want: "default"},
		{args: []string{"claude", "--", "--dangerously-skip-permissions"}, want: "default"},
	}
	for _, test := range tests {
		if got := permissionModeFromProcessArgs(test.args); got != test.want {
			t.Fatalf("permissionModeFromProcessArgs(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}
