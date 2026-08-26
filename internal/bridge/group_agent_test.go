package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestRemoteLaneCollectionPointerCarriesSourceAgentRuntime(t *testing.T) {
	groups := []string{"project", "session:destination/child", "session:source/parent"}
	got := laneCollectionPointer("claude", "child", "source", "/tmp/source runtime", groups)
	want := "peer-federator lane -runtime-dir '/tmp/source runtime' --host destination --product claude -- wait child"
	if got != want {
		t.Fatalf("remote collection pointer = %q, want %q", got, want)
	}
	legacy := laneCollectionPointer("claude", "child", "source", "", groups)
	if strings.Contains(legacy, "-runtime-dir") {
		t.Fatalf("legacy collection pointer invented a runtime directory: %q", legacy)
	}
}

func TestRemoteQwenCollectionPointerIsVerbatimAndSourceScoped(t *testing.T) {
	groups := []string{"project", "session:destination/qwen-child", "session:source/qwen-parent"}
	got := laneCollectionPointer("qwen", "qwen-child", "source", "/tmp/qwen source", groups)
	want := "peer-federator lane -runtime-dir '/tmp/qwen source' --host destination --product qwen -- wait qwen-child"
	if got != want {
		t.Fatalf("remote Qwen collection pointer = %q, want %q", got, want)
	}
}

func TestManagedPeerUsesSingleAgentRegistryCarrierAndGroupedDelivery(t *testing.T) {
	root := shortSocketTestRoot(t, "ga-")
	configDir := filepath.Join(root, "claude")
	runtimeDir := filepath.Join(root, "agent-runtime")
	peerRuntimeDir := filepath.Join(root, "peer-runtime")
	stateDir := filepath.Join(root, "agent-state")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(peerRuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataDir)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- federator.RunAgent(ctx, federator.AgentOptions{
			HostID: "test-host", HostName: "test-host", ClaudeConfigDir: configDir,
			RuntimeDir: runtimeDir, StateDir: stateDir, ScanInterval: 20 * time.Millisecond,
			Logger: log.New(io.Discard, "", 0),
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("host agent: %v", err)
		}
	})
	if !waitForCondition(2*time.Second, func() bool {
		_, err := federator.ReadAgentStatus(runtimeDir)
		return err == nil
	}) {
		t.Fatal("host agent did not become ready")
	}
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	for _, sessionID := range []string{"source-session", "target-session"} {
		if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
			SessionID: sessionID, Product: "codex", Groups: []string{"project"}, GroupsSpecified: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	sourceSocket := filepath.Join(root, "source.sock")
	sourceListener, err := net.Listen("unix", sourceSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceListener.Close() }()
	go drainTestListener(sourceListener)
	sourceRegistration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: "source-session", Product: "codex",
		Name: "source", PID: os.Getpid(), ProcStart: readProcStart(os.Getpid()), Socket: sourceSocket,
	}
	if _, err := federator.RegisterPeer(runtimeDir, sourceRegistration); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = federator.UnregisterPeer(runtimeDir, sourceRegistration) }()
	for _, product := range mcpLaneProductIDs() {
		childID := product + "-child"
		state, _, err := resolveLaneGroupState(childID, product, laneGroupOptions{
			groups: []string{"child-extra"}, groupsSpecified: true,
			parentSessionID: "source-session", parentHostID: "test-host",
		}, product == "grok", true)
		if err != nil {
			t.Fatalf("resolve %s child groups: %v", product, err)
		}
		want := []string{"child-extra", "session:test-host/" + childID, "session:test-host/source-session"}
		if !equalStringSlices(state.Groups, want) {
			t.Fatalf("%s default child groups = %v, want %v", product, state.Groups, want)
		}
		inherited, _, err := resolveLaneGroupState(childID, product, laneGroupOptions{
			parentSessionID: "source-session", parentHostID: "test-host",
			inheritParentGroups: true, inheritGroupsSpecified: true,
		}, product == "grok", false)
		if err != nil {
			t.Fatalf("inherit %s parent groups: %v", product, err)
		}
		if !containsString(inherited.Groups, "project") {
			t.Fatalf("%s explicit inheritance omitted parent project: %v", product, inherited.Groups)
		}
	}

	target := newDaemon(map[string]string{
		"session-id": "target-session", "cwd": root, "name": "target", "entrypoint": "codex",
		"data-dir": filepath.Join(root, "data"), "claude-config-dir": configDir,
		"codex-home": filepath.Join(root, "codex"), "runtime-dir": peerRuntimeDir,
		"agent-runtime-dir": runtimeDir,
	})
	if err := target.start(); err != nil {
		t.Fatal(err)
	}
	defer target.shutdown()
	paths := resolveNativePaths()
	writeTestActiveCodexLane(t, paths, "source-session")
	if err := writeJSONAtomic(filepath.Join(dataDir, "sessions", sessionKey("source-session"), "state.json"), map[string]any{
		"sessionId": "source-session", "name": "source", "socketPath": sourceSocket,
		"groupProtocol": federator.GroupProtocolVersion, "agentRuntimeDir": runtimeDir,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := federator.ReadAgentStatus(runtimeDir)
	if err != nil || status.LocalPeers != 2 {
		t.Fatalf("registered peers = %d, %v", status.LocalPeers, err)
	}
	listed, err := callNativePeerTool("list_peers", map[string]any{}, "source-session")
	if err != nil || !strings.Contains(stringValue(listed["text"]), "target") {
		t.Fatalf("grouped MCP list = %#v, %v", listed, err)
	}
	if _, err := callNativePeerTool("send_message", map[string]any{
		"session_id": "source-session", "targets": []string{"target"}, "message": "GROUPED_MCP_SEND_OK",
	}, "source-session"); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(time.Second, func() bool {
		entries, _ := os.ReadDir(target.pendingDir)
		return len(entries) == 1
	}) {
		t.Fatal("grouped MCP send was not queued")
	}
	if _, err := callNativePeerTool("broadcast", map[string]any{
		"session_id": "source-session", "group": "project", "message": "GROUPED_BROADCAST_OK",
	}, "source-session"); err != nil {
		t.Fatal(err)
	}
	fakeRuntime := filepath.Join(root, "mcp-runtime")
	if err := os.WriteFile(fakeRuntime, []byte("#!/bin/sh\nprintf 'role=%s session=%s product=%s\\n' \"$1\" \"$AGENT_SESSIONS_SESSION_ID\" \"$AGENT_SESSIONS_PRODUCT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousRuntime := mcpLaneRuntimeExecutable
	mcpLaneRuntimeExecutable = func() (string, error) { return fakeRuntime, nil }
	t.Cleanup(func() { mcpLaneRuntimeExecutable = previousRuntime })
	laneResult, err := callNativePeerTool("lane", map[string]any{
		"session_id": "source-session", "product": "claude", "command": "doctor",
		"arguments": []any{"--json"},
	}, "source-session")
	if err != nil {
		t.Fatal(err)
	}
	laneData, _ := laneResult["data"].(map[string]any)
	if intValue(laneData["exit"]) != 0 ||
		!strings.Contains(stringValue(laneData["stdout"]), "role=claude-lane session=source-session product=codex") {
		t.Fatalf("attested MCP lane result = %#v", laneResult)
	}

	result, err := federator.RouteAgentFrame(runtimeDir, "source-session", federator.AgentFrame{
		Version: federator.AgentFrameVersion, Type: "send", MessageID: "group-message-1",
		Targets: []string{"target"}, Content: "GROUPED_DELIVERY_OK", Summary: "test grouped delivery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deliveries) != 1 || result.Deliveries[0].Status != "accepted" {
		t.Fatalf("delivery result = %+v", result.Deliveries)
	}
	if !waitForCondition(time.Second, func() bool {
		entries, _ := os.ReadDir(target.pendingDir)
		return len(entries) == 3
	}) {
		t.Fatal("grouped message was not queued by the target adapter")
	}
	entries, _ := os.ReadDir(target.pendingDir)
	seen := map[string]bool{}
	for _, entry := range entries {
		body, readErr := os.ReadFile(filepath.Join(target.pendingDir, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var item map[string]any
		if json.Unmarshal(body, &item) != nil || stringValue(item["fromSession"]) != "source-session" ||
			stringValue(item["fromProduct"]) != "codex" {
			t.Fatalf("queued grouped item = %s", body)
		}
		seen[stringValue(item["message"])] = true
	}
	for _, message := range []string{"GROUPED_MCP_SEND_OK", "GROUPED_BROADCAST_OK", "GROUPED_DELIVERY_OK"} {
		if !seen[message] {
			t.Fatalf("queued grouped messages = %v; missing %s", seen, message)
		}
	}
	registryEntries, err := os.ReadDir(filepath.Join(configDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	var registryName, keyName string
	for _, entry := range registryEntries {
		switch {
		case strings.HasSuffix(entry.Name(), ".json"):
			if registryName != "" {
				t.Fatalf("multiple Claude registry rows: %v", registryEntries)
			}
			registryName = entry.Name()
		case strings.HasSuffix(entry.Name(), ".key"):
			if keyName != "" {
				t.Fatalf("multiple Claude service keys: %v", registryEntries)
			}
			keyName = entry.Name()
		default:
			t.Fatalf("unexpected Claude registry artifact %s", entry.Name())
		}
	}
	if registryName == "" || keyName == "" {
		t.Fatalf("Claude registry entries = %v; want one host-agent row and its key", registryEntries)
	}
	registryBody, err := os.ReadFile(filepath.Join(configDir, "sessions", registryName))
	if err != nil {
		t.Fatal(err)
	}
	var registry map[string]any
	if json.Unmarshal(registryBody, &registry) != nil || !boolValue(registry["agentService"]) {
		t.Fatalf("Claude registry row is not the host agent: %s", registryBody)
	}
	keyBody, err := os.ReadFile(filepath.Join(configDir, "sessions", keyName))
	var key struct {
		PeerToken string `json:"peerToken"`
		ProcStart string `json:"procStart"`
	}
	if err != nil || json.Unmarshal(keyBody, &key) != nil || len(key.PeerToken) != 32 || key.ProcStart == "" {
		t.Fatalf("invalid projected host-agent key %s: %s, %v", keyName, keyBody, err)
	}
}

func TestAllLaneTargetParsersAcceptSharedGroupLayer(t *testing.T) {
	codex, err := parseLaneArgs([]string{"start", "--name", "c", "--group", "one", "--group", "two", "--inherit-groups", "-"})
	if err != nil || !codex.groupOptions.groupsSpecified || !codex.groupOptions.inheritParentGroups {
		t.Fatalf("Codex group options = %+v, %v", codex.groupOptions, err)
	}
	claude, err := parseClaudeLaneArgs([]string{"start", "--name", "c", "--group", "one", "--no-inherit-groups", "-"})
	if err != nil || !claude.groupOptions.groupsSpecified || !claude.groupOptions.inheritGroupsSpecified || claude.groupOptions.inheritParentGroups {
		t.Fatalf("Claude group options = %+v, %v", claude.groupOptions, err)
	}
	grok, err := parseGrokLaneArgs([]string{"start", "--name", "g", "--group", "one", "--inherit-groups", "-"})
	if err != nil || !grok.groupOptions.groupsSpecified || !grok.groupOptions.inheritParentGroups {
		t.Fatalf("Grok group options = %+v, %v", grok.groupOptions, err)
	}
	qwen, err := parseQwenLaneArgs([]string{"start", "--name", "q", "--group", "one", "--inherit-groups", "-"})
	if err != nil || !qwen.groupOptions.groupsSpecified || !qwen.groupOptions.inheritParentGroups {
		t.Fatalf("Qwen group options = %+v, %v", qwen.groupOptions, err)
	}
}

func TestRemoteParentContextSurvivesEveryTargetLaunchLayer(t *testing.T) {
	parent := federator.ParentContext{
		HostID: "source-host", SessionID: "source-session", Product: "claude",
		Groups: []string{"project", "session:source-host/source-session"}, AgentRuntimeDir: "/source/agent-runtime",
	}
	body, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(remoteParentEnvironment, string(body))
	t.Setenv(peerSessionIDEnvironment, "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("AGENT_SESSIONS_GROK_SESSION_ID", "")

	localOwner := laneOwner{PID: os.Getpid(), ProcStart: readProcStart(os.Getpid()), SessionID: "destination-local-parent"}
	common := laneCommonOptions{command: "start", persistent: true}
	codex := withLaneResolvedParent(laneOptions{laneCommonOptions: common}, localOwner)
	claude := withClaudeLaneResolvedParent(claudeLaneOptions{laneCommonOptions: common}, localOwner)
	grok := withGrokLaneResolvedParent(grokLaneOptions{laneCommonOptions: common}, localOwner)
	qwen := withQwenLaneResolvedParent(qwenLaneOptions{laneCommonOptions: common}, localOwner)
	for product, state := range map[string]laneGroupOptions{
		"codex": codex.groupOptions, "claude": claude.groupOptions, "grok": grok.groupOptions, "qwen": qwen.groupOptions,
	} {
		if state.parentSessionID != parent.SessionID || state.parentHostID != parent.HostID ||
			state.parentAgentRuntimeDir != parent.AgentRuntimeDir ||
			!equalStringSlices(state.parentGroups, parent.Groups) {
			t.Fatalf("%s remote parent = %+v", product, state)
		}
	}
	if codex.ownerSessionID != "" || claude.ownerSessionID != "" || grok.ownerSessionID != "" || qwen.ownerSessionID != "" {
		t.Fatalf("remote communication parent became a local lifecycle owner: codex=%q claude=%q grok=%q qwen=%q",
			codex.ownerSessionID, claude.ownerSessionID, grok.ownerSessionID, qwen.ownerSessionID)
	}
}

func TestEveryParentProductComposesWithEveryTargetGroupLayer(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	procStart := readProcStart(os.Getpid())
	socket := filepath.Join(root, "parent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go drainTestListener(listener)
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Setenv(remoteParentEnvironment, "")
	t.Setenv("CODEX_THREAD_ID", "")

	for _, parentProduct := range mcpLaneProductIDs() {
		parentProduct := parentProduct
		t.Run("parent-"+parentProduct, func(t *testing.T) {
			parentID := "local-" + parentProduct + "-parent"
			projectGroup := "project-" + parentProduct
			if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
				SessionID: parentID, Product: parentProduct, Groups: []string{projectGroup}, GroupsSpecified: true,
			}); err != nil {
				t.Fatal(err)
			}
			registration := federator.PeerRegistration{
				Version: federator.GroupProtocolVersion, SessionID: parentID, Product: parentProduct,
				Name: parentID, PID: os.Getpid(), ProcStart: procStart, Socket: socket,
			}
			if parentProduct == "qwen" {
				registration.QwenCapabilityDigest = "sha256:" + strings.Repeat("a", 64)
			}
			if _, err := federator.RegisterPeer(runtimeDir, registration); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = federator.UnregisterPeer(runtimeDir, registration) })
			t.Setenv(peerSessionIDEnvironment, parentID)
			t.Setenv("AGENT_SESSIONS_PRODUCT", parentProduct)
			owner := laneOwner{PID: os.Getpid(), ProcStart: procStart, SessionID: parentID, PermissionMode: "default"}

			for _, targetProduct := range mcpLaneProductIDs() {
				targetProduct := targetProduct
				t.Run("target-"+targetProduct, func(t *testing.T) {
					childID := parentProduct + "-to-" + targetProduct
					launch := resolvedTargetLaunch(targetProduct, false, laneGroupOptions{}, owner)
					assertResolvedTargetParent(t, launch, parentID, os.Getpid(), procStart, true)
					state, _, err := resolveLaneGroupState(childID, targetProduct, launch.groups, targetProduct == "grok", true)
					if err != nil {
						t.Fatal(err)
					}
					assertContainsGroup(t, state.Groups, "session:test-host/"+childID)
					assertContainsGroup(t, state.Groups, "session:test-host/"+parentID)
					if containsString(state.Groups, projectGroup) {
						t.Fatalf("default child inherited %q: %v", projectGroup, state.Groups)
					}

					inherited := resolvedTargetLaunch(targetProduct, false, laneGroupOptions{
						inheritParentGroups: true, inheritGroupsSpecified: true,
					}, owner)
					inheritedState, _, err := resolveLaneGroupState(childID+"-inherited", targetProduct, inherited.groups, targetProduct == "grok", true)
					if err != nil {
						t.Fatal(err)
					}
					assertContainsGroup(t, inheritedState.Groups, projectGroup)

					persistent := resolvedTargetLaunch(targetProduct, true, laneGroupOptions{}, owner)
					assertResolvedTargetParent(t, persistent, parentID, 0, "", false)
				})
			}
		})
	}
}

func TestCorroboratedNativeParentWinsLeakedCodexThreadID(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	procStart := readProcStart(os.Getpid())
	socket := filepath.Join(root, "grok-parent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go drainTestListener(listener)

	const parentID = "corroborated-grok-parent"
	if _, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: parentID, Product: "grok", Groups: []string{"grok-project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: parentID, Product: "grok",
		Name: parentID, PID: os.Getpid(), ProcStart: procStart, Socket: socket,
	}
	if _, err := federator.RegisterPeer(runtimeDir, registration); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = federator.UnregisterPeer(runtimeDir, registration) })
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Setenv(remoteParentEnvironment, "")
	t.Setenv(peerSessionIDEnvironment, "")
	t.Setenv("AGENT_SESSIONS_PRODUCT", "")
	t.Setenv("CODEX_THREAD_ID", "unrelated-outer-codex-thread")
	owner := laneOwner{PID: os.Getpid(), ProcStart: procStart, SessionID: parentID, PermissionMode: "bypassPermissions"}

	for _, targetProduct := range mcpLaneProductIDs() {
		launch := resolvedTargetLaunch(targetProduct, false, laneGroupOptions{}, owner)
		if launch.groups.parentErr != nil {
			t.Fatalf("%s target rejected corroborated Grok parent: %v", targetProduct, launch.groups.parentErr)
		}
		assertResolvedTargetParent(t, launch, parentID, os.Getpid(), procStart, true)
	}
}

func TestRemoteParentTargetThenNestedLaneUsesImmediateParent(t *testing.T) {
	root, runtimeDir := startLaneContextTestAgent(t)
	procStart := readProcStart(os.Getpid())
	socket := filepath.Join(root, "nested-parent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go drainTestListener(listener)
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Setenv(peerSessionIDEnvironment, "")
	t.Setenv("AGENT_SESSIONS_PRODUCT", "")
	t.Setenv("CODEX_THREAD_ID", "")
	remote := federator.ParentContext{
		HostID: "remote-host", SessionID: "remote-parent", Product: "claude",
		Groups: []string{"remote-project", "session:remote-host/remote-parent"},
	}
	remoteBody, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(remoteParentEnvironment, string(remoteBody))

	for _, firstTarget := range mcpLaneProductIDs() {
		firstTarget := firstTarget
		t.Run("first-target-"+firstTarget, func(t *testing.T) {
			childID := "remote-to-" + firstTarget
			first := resolvedTargetLaunch(firstTarget, true, laneGroupOptions{}, laneOwner{})
			if first.groups.parentSessionID != remote.SessionID || first.groups.parentHostID != remote.HostID ||
				!equalStringSlices(first.groups.parentGroups, remote.Groups) {
				t.Fatalf("remote parent context = %+v", first.groups)
			}
			// Remote-parent group attestation is covered inside the federator,
			// where a real remote peer can be installed in the agent snapshot.
			// Seed the child catalog row here so this bridge test stays focused on
			// worker-environment and immediate-parent
			// composition rather than constructing a second federated host.
			childPreference, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
				SessionID: childID, Product: firstTarget,
				AlwaysApprove: firstTarget == "grok", AlwaysApproveSpecified: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertContainsGroup(t, childPreference.EffectiveGroups, "session:test-host/"+childID)

			assertTargetWorkerDropsRemoteParent(t, firstTarget, childID, string(remoteBody))
			registration := federator.PeerRegistration{
				Version: federator.GroupProtocolVersion, SessionID: childID, Product: firstTarget,
				Name: childID, PID: os.Getpid(), ProcStart: procStart, Socket: socket,
			}
			if firstTarget == "grok" {
				registration.PermissionMode = "bypassPermissions"
			}
			if firstTarget == "qwen" {
				registration.QwenCapabilityDigest = "sha256:" + strings.Repeat("a", 64)
			}
			if _, err := federator.RegisterPeer(runtimeDir, registration); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = federator.UnregisterPeer(runtimeDir, registration) })
			t.Setenv(remoteParentEnvironment, "")
			t.Setenv(peerSessionIDEnvironment, childID)
			t.Setenv("AGENT_SESSIONS_PRODUCT", firstTarget)
			childOwner := laneOwner{PID: os.Getpid(), ProcStart: procStart, SessionID: childID, PermissionMode: "default"}

			for _, nestedTarget := range mcpLaneProductIDs() {
				nested := resolvedTargetLaunch(nestedTarget, false, laneGroupOptions{}, childOwner)
				assertResolvedTargetParent(t, nested, childID, os.Getpid(), procStart, true)
				assertContainsGroup(t, nested.groups.parentGroups, "session:test-host/"+childID)
				if nested.groups.parentSessionID == remote.SessionID || nested.groups.parentHostID == remote.HostID {
					t.Fatalf("%s nested target retained remote grandparent: %+v", nestedTarget, nested.groups)
				}
			}
		})
	}
}

type targetLaunchProjection struct {
	groups         laneGroupOptions
	ownerPID       int
	ownerProcStart string
	ownerSessionID string
	notifyTarget   string
}

func resolvedTargetLaunch(product string, persistent bool, groups laneGroupOptions, owner laneOwner) targetLaunchProjection {
	switch product {
	case "codex":
		got := withLaneResolvedParent(laneOptions{laneCommonOptions: laneCommonOptions{command: "start", persistent: persistent, groupOptions: groups}}, owner)
		return targetLaunchProjection{got.groupOptions, got.ownerPID, got.ownerProcStart, got.ownerSessionID, got.notifyTarget}
	case "claude":
		got := withClaudeLaneResolvedParent(claudeLaneOptions{laneCommonOptions: laneCommonOptions{command: "start", persistent: persistent, groupOptions: groups}}, owner)
		return targetLaunchProjection{got.groupOptions, got.ownerPID, got.ownerProcStart, got.ownerSessionID, got.notifyTarget}
	case "grok":
		got := withGrokLaneResolvedParent(grokLaneOptions{laneCommonOptions: laneCommonOptions{command: "start", persistent: persistent, groupOptions: groups}}, owner)
		return targetLaunchProjection{got.groupOptions, got.ownerPID, got.ownerProcStart, got.ownerSessionID, got.notifyTarget}
	case "qwen":
		got := withQwenLaneResolvedParent(qwenLaneOptions{laneCommonOptions: laneCommonOptions{command: "start", persistent: persistent, groupOptions: groups}}, owner)
		return targetLaunchProjection{got.groupOptions, got.ownerPID, got.ownerProcStart, got.ownerSessionID, got.notifyTarget}
	default:
		panic("unsupported test target product: " + product)
	}
}

func assertResolvedTargetParent(t *testing.T, got targetLaunchProjection, sessionID string, pid int, procStart string, notify bool) {
	t.Helper()
	wantOwnerSession := ""
	if pid > 0 {
		wantOwnerSession = sessionID
	}
	if got.groups.parentSessionID != sessionID || got.groups.parentHostID != "test-host" ||
		got.ownerPID != pid || got.ownerProcStart != procStart || got.ownerSessionID != wantOwnerSession {
		t.Fatalf("resolved target parent = %+v", got)
	}
	wantNotify := ""
	if notify {
		wantNotify = "session:" + sessionID
	}
	if got.notifyTarget != wantNotify {
		t.Fatalf("notify target = %q, want %q", got.notifyTarget, wantNotify)
	}
}

func assertContainsGroup(t *testing.T, groups []string, want string) {
	t.Helper()
	if !containsString(groups, want) {
		t.Fatalf("groups = %v; missing %q", groups, want)
	}
}

func assertTargetWorkerDropsRemoteParent(t *testing.T, product, sessionID, remoteBody string) {
	t.Helper()
	input := []string{remoteParentEnvironment + "=" + remoteBody, peerSessionIDEnvironment + "=remote-parent", "AGENT_SESSIONS_PRODUCT=claude"}
	var environment []string
	switch product {
	case "codex":
		// The Codex supershim owns its detached App Server environment. Its
		// persistentRuntimeEnvironment unit test covers the same scrub contract.
		return
	case "claude":
		environment = claudeLaneWorkerEnv(input, sessionID, claudeprofile.Source{
			ConfigRoot: "/shared", ConfigEnvSet: true, ConfigEnvValue: "/shared",
			SecureConfig: "/secure", SecureEnvSet: true,
		})
	case "grok":
		environment = grokLaneWorkerEnvironment(input, strings.Repeat("a", 64), grokLaneState{SessionID: sessionID}, "/tmp/grok-shell", "/bin/bash")
	case "qwen":
		environment = qwenLaneWorkerEnvironment(input, qwenLaneState{ThreadID: sessionID}, strings.Repeat("a", 64))
	}
	joined := "\n" + strings.Join(environment, "\n") + "\n"
	if strings.Contains(joined, "\n"+remoteParentEnvironment+"=") ||
		!strings.Contains(joined, "\n"+peerSessionIDEnvironment+"="+sessionID+"\n") ||
		!strings.Contains(joined, "\nAGENT_SESSIONS_PRODUCT="+product+"\n") {
		t.Fatalf("%s worker environment retained a remote grandparent or omitted child identity: %q", product, environment)
	}
}

func startLaneContextTestAgent(t *testing.T) (string, string) {
	t.Helper()
	root := shortSocketTestRoot(t, "gc-")
	runtimeDir := filepath.Join(root, "agent-runtime")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- federator.RunAgent(ctx, federator.AgentOptions{
			HostID: "test-host", HostName: "test-host", ClaudeConfigDir: filepath.Join(root, "claude"),
			RuntimeDir: runtimeDir, StateDir: filepath.Join(root, "state"), ScanInterval: 20 * time.Millisecond,
			Logger: log.New(io.Discard, "", 0),
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("host agent: %v", err)
		}
	})
	if !waitForCondition(2*time.Second, func() bool {
		_, err := federator.ReadAgentStatus(runtimeDir)
		return err == nil
	}) {
		t.Fatal("host agent did not become ready")
	}
	return root, runtimeDir
}

// shortSocketTestRoot avoids including the test name in Unix socket paths.
// Darwin limits sockaddr_un.sun_path to 104 bytes, and testing.T.TempDir uses
// the full test name even when TMPDIR itself is already compact.
func shortSocketTestRoot(t *testing.T, pattern string) string {
	t.Helper()
	return testutil.ShortSocketRoot(t, pattern, filepath.Join("agent-runtime", "agent.sock"))
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func drainTestListener(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func waitForCondition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}
