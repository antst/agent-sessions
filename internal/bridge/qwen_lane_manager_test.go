package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const qwenToolWrapperExecHelperEnv = "AGENT_SESSIONS_QWEN_TOOL_EXEC_HELPER"

func TestQwenToolWrapperExecHelper(_ *testing.T) {
	if os.Getenv(qwenToolWrapperExecHelperEnv) != "1" {
		return
	}
	os.Exit(runQwenToolWrapper([]string{"-c", "printf QWEN_TOOL_WRAPPER_EXEC_OK"}))
}

func TestQwenACPClientInitializesPromptsHandlesPermissionAndUpdates(t *testing.T) {
	agentInput, clientInput := io.Pipe()
	clientOutput, agentOutput := io.Pipe()
	client := newQwenACPClient(clientInput, clientOutput)
	defer func() { _ = client.close() }()
	peer := newQwenTestRPCPeer(agentInput, agentOutput)
	updates := make(chan map[string]any, 2)
	client.setNotificationHandler(func(message map[string]any) { updates <- message })

	done := make(chan error, 1)
	go func() {
		initialize, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		if initialize.Method != "initialize" {
			done <- errors.New("first method was " + initialize.Method)
			return
		}
		if err := peer.respond(initialize.ID, map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": "qwen-code", "version": "0.21.15"},
		}); err != nil {
			done <- err
			return
		}
		prompt, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		if prompt.Method != "session/prompt" {
			done <- errors.New("second method was " + prompt.Method)
			return
		}
		if err := peer.write(map[string]any{
			"jsonrpc": "2.0", "id": 91, "method": "session/request_permission",
			"params": map[string]any{
				"sessionId": "11111111-2222-4333-8444-555555555555",
				"toolCall":  map[string]any{"toolCallId": "tool-1", "title": "test"},
				"options": []any{
					map[string]any{"optionId": "proceed_always", "kind": "allow_always"},
					map[string]any{"optionId": "proceed_once", "kind": "allow_once"},
					map[string]any{"optionId": "cancel", "kind": "reject_once"},
				},
			},
		}); err != nil {
			done <- err
			return
		}
		permission, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		var permissionBody map[string]any
		if err := json.Unmarshal(permission.Result, &permissionBody); err != nil {
			done <- err
			return
		}
		outcome := mapValue(permissionBody["outcome"])
		if outcome["optionId"] != "proceed_once" {
			done <- errors.New("permission did not select allow_once")
			return
		}
		if err := peer.notify("session/update", map[string]any{
			"sessionId": "11111111-2222-4333-8444-555555555555",
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ok"}},
		}); err != nil {
			done <- err
			return
		}
		done <- peer.respond(prompt.ID, map[string]any{"stopReason": "end_turn"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialized, err := client.request(ctx, "initialize", map[string]any{"protocolVersion": 1})
	if err != nil || intValue(initialized["protocolVersion"]) != 1 {
		t.Fatalf("initialize = %#v, %v", initialized, err)
	}
	result, err := client.request(ctx, "session/prompt", map[string]any{
		"sessionId": "11111111-2222-4333-8444-555555555555",
		"prompt":    []any{map[string]any{"type": "text", "text": "respond"}},
	})
	if err != nil || result["stopReason"] != "end_turn" {
		t.Fatalf("prompt = %#v, %v", result, err)
	}
	select {
	case update := <-updates:
		if update["method"] != "session/update" {
			t.Fatalf("update = %#v", update)
		}
	case <-ctx.Done():
		t.Fatal("missing Qwen update")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQwenACPClientResumesExactNativeSessionWithMCP(t *testing.T) {
	agentInput, clientInput := io.Pipe()
	clientOutput, agentOutput := io.Pipe()
	client := newQwenACPClient(clientInput, clientOutput)
	defer func() { _ = client.close() }()
	peer := newQwenTestRPCPeer(agentInput, agentOutput)
	nativeSessionID := randomID()
	done := make(chan error, 1)
	go func() {
		initialize, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		if initialize.Method != "initialize" {
			done <- errors.New("first method was " + initialize.Method)
			return
		}
		if err := peer.respond(initialize.ID, map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": "qwen-code", "version": "0.21.15"},
			"agentCapabilities": map[string]any{
				"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}},
				"mcpCapabilities": map[string]any{"stdio": true},
			},
		}); err != nil {
			done <- err
			return
		}
		resume, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		var params map[string]any
		if err := json.Unmarshal(resume.Params, &params); err != nil {
			done <- err
			return
		}
		servers, _ := params["mcpServers"].([]any)
		if resume.Method != "session/resume" || stringValue(params["sessionId"]) != nativeSessionID ||
			len(servers) != 1 || stringValue(mapValue(servers[0])["name"]) != "agent_sessions" {
			done <- fmt.Errorf("resume request = %#v", resume)
			return
		}
		done <- peer.respond(resume.ID, map[string]any{
			"sessionId": nativeSessionID, "modes": map[string]any{"currentModeId": "default"},
		})
	}()

	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "data"), runtimeDir: filepath.Join(root, "runtime"),
		claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex"),
	}
	state := qwenLaneState{
		Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
		Name: "resume", ThreadID: randomID(), QwenSessionID: nativeSessionID, Cwd: root,
		Status: "starting", LaunchPreference: "native_default", NativeArchiveState: "active",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeQwenLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &qwenLaneManager{
		paths: paths, state: state, launchToken: randomID() + randomID(),
		client: client, worker: &grokManagedProcess{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.initializeACP(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	latest, err := readQwenLaneState(paths, state.ThreadID)
	if err != nil || latest.QwenSessionID != nativeSessionID || latest.InitialNativeMode != "default" {
		t.Fatalf("resumed state = %+v, %v", latest, err)
	}
}

func TestQwenLaneManagerPublishesSerializesCollectsAndArchives(t *testing.T) {
	agentRuntime := useBridgeTestAgent(t)
	root := t.TempDir()
	for _, directory := range []string{"state", "claude", "codex", "runtime", "qwen-home", "qwen-runtime", "workspace"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("QWEN_HOME", filepath.Join(root, "qwen-home"))
	t.Setenv("QWEN_RUNTIME_DIR", filepath.Join(root, "qwen-runtime"))
	fake := newFakeQwenProcess(t)
	fake.installEnvironment(t)
	nativeSessionID := randomID()
	t.Setenv(qwenTestSessionIDEnv, nativeSessionID)
	t.Setenv(qwenLaneExecutableEnv, fake.Paths.Executable)

	profile, err := qwenprofile.Current()
	if err != nil {
		t.Fatal(err)
	}
	paths := resolveNativePaths()
	threadID, launchToken := randomID(), randomID()+randomID()
	turn := newQwenLaneTurn("return the fake answer", 0)
	now := time.Now().UnixMilli()
	state := qwenLaneState{
		Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
		Name: "qwen-manager-test", ThreadID: threadID, Cwd: filepath.Join(root, "workspace"), Profile: profile,
		Status: "starting", ControlSocket: qwenLaneControlSocket(paths, threadID), RuntimeDir: paths.runtimeDir,
		LaunchTokenHash: qwenLaneTokenHash(launchToken), LaunchPreference: "native_default",
		CurrentNativeMode: "unknown", NativeArchiveState: "active", Persistent: true,
		AutoArchive: false, Turns: []qwenLaneTurn{turn}, PendingTurnIDs: []string{turn.ID}, LatestTurnID: turn.ID,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := federator.ResolveSessionPreferences(agentRuntime, federator.ResolvePreferencesRequest{
		SessionID: threadID, Product: "qwen", Kind: federator.SessionKindLane,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeQwenLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &qwenLaneManager{
		paths: paths, state: state, launchToken: launchToken,
		turnNotify: make(chan struct{}, 1), done: make(chan struct{}), startupDone: make(chan struct{}),
	}
	originalArchive := executeQwenArchiveTransaction
	executeQwenArchiveTransaction = func(candidate qwenLaneState, operation string) error {
		if candidate.QwenSessionID != nativeSessionID || operation != "archive" {
			t.Fatalf("archive transaction = session %q operation %q", candidate.QwenSessionID, operation)
		}
		return nil
	}
	t.Cleanup(func() { executeQwenArchiveTransaction = originalArchive })
	if err := manager.start(); err != nil {
		t.Fatalf("start Qwen lane manager: %v", err)
	}
	qwenTestPoll(t, qwenTestLifecycleTimeout, "terminal Qwen lane turn", func() (bool, error) {
		latest, readErr := readQwenLaneState(paths, threadID)
		if readErr != nil {
			return false, readErr
		}
		return latest.Turns[0].Status == "completed", nil
	})
	latest, err := readQwenLaneState(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.QwenSessionID != nativeSessionID || latest.QwenSessionID == latest.ThreadID ||
		latest.InitialNativeMode != "default" || latest.CurrentNativeMode != "default" ||
		latest.Turns[0].Result != "fake Qwen answer" || latest.Turns[0].Exit != 0 {
		t.Fatalf("terminal Qwen state = %+v", latest)
	}
	if info, err := os.Lstat(latest.MessagingSocket); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("published Qwen endpoint = %v, %v", info, err)
	}
	if _, err := manager.handleControl(map[string]any{"action": "ack", "sessionId": threadID, "turnId": turn.ID}); err != nil {
		t.Fatal(err)
	}
	followUp := newQwenLaneTurn("follow up on the same native transcript", 0)
	if _, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": threadID, "turn": followUp, "persistent": true,
		"groups": []string{}, "explicitGroups": []string{}, "requestedInitialMode": "plan", "launchPreference": "native:plan",
	}); err != nil {
		t.Fatal(err)
	}
	qwenTestPoll(t, qwenTestLifecycleTimeout, "terminal Qwen follow-up", func() (bool, error) {
		current, readErr := readQwenLaneState(paths, threadID)
		if readErr != nil {
			return false, readErr
		}
		return len(current.Turns) == 2 && current.Turns[1].Status == "completed", nil
	})
	latest, err = readQwenLaneState(paths, threadID)
	if err != nil || latest.QwenSessionID != nativeSessionID || latest.CurrentNativeMode != "plan" || latest.LaunchPreference != "native:plan" {
		t.Fatalf("Qwen follow-up state = %+v, %v", latest, err)
	}
	if _, err := manager.handleControl(map[string]any{"action": "ack", "sessionId": threadID, "turnId": followUp.ID}); err != nil {
		t.Fatal(err)
	}
	interrupted := newQwenLaneTurn("BLOCK_QWEN_PROMPT", 0)
	if _, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": threadID, "turn": interrupted, "persistent": true,
		"groups": []string{}, "explicitGroups": []string{},
	}); err != nil {
		t.Fatal(err)
	}
	qwenTestPoll(t, qwenTestLifecycleTimeout, "active Qwen interrupt turn", func() (bool, error) {
		current, readErr := readQwenLaneState(paths, threadID)
		return readErr == nil && len(current.Turns) == 3 && current.Turns[2].Status == "active", readErr
	})
	if _, err := manager.handleControl(map[string]any{"action": "interrupt", "sessionId": threadID}); err != nil {
		t.Fatal(err)
	}
	qwenTestPoll(t, qwenTestLifecycleTimeout, "interrupted Qwen turn", func() (bool, error) {
		current, readErr := readQwenLaneState(paths, threadID)
		return readErr == nil && current.Turns[2].Status == "interrupted", readErr
	})
	latest, err = readQwenLaneState(paths, threadID)
	if err != nil || latest.Turns[2].Outcome != "interrupted" || latest.Turns[2].Exit != 130 || latest.Turns[2].TerminalRevision == "" {
		t.Fatalf("interrupted Qwen state = %+v, %v", latest, err)
	}
	if _, err := manager.handleControl(map[string]any{"action": "ack", "sessionId": threadID, "turnId": interrupted.ID}); err != nil {
		t.Fatal(err)
	}
	records, err := qwenTestReadJSONL(fake.Paths.Records)
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]int{}
	var newSession map[string]any
	for _, record := range records {
		if stringValue(record["kind"]) != "rpc_request" {
			continue
		}
		message := mapValue(record["message"])
		method := stringValue(message["Method"])
		if method == "" {
			method = stringValue(message["method"])
		}
		methods[method]++
		if method == "session/new" {
			newSession = mapValue(message["Params"])
			if len(newSession) == 0 {
				newSession = mapValue(message["params"])
			}
		}
	}
	if methods["initialize"] != 1 || methods["session/new"] != 1 || methods["session/resume"] != 0 ||
		methods["session/prompt"] != 3 || methods["session/set_mode"] != 1 || methods["session/cancel"] != 1 {
		t.Fatalf("Qwen ACP method inventory = %#v", methods)
	}
	mcpServers, _ := newSession["mcpServers"].([]any)
	if len(mcpServers) != 1 || stringValue(mapValue(mcpServers[0])["name"]) != "agent_sessions" {
		t.Fatalf("Qwen Agent Sessions MCP injection = %#v", newSession["mcpServers"])
	}
	manager.shutdown("explicit archive", true)
	archived, err := readQwenLaneState(paths, threadID)
	if err != nil || archived.Status != "archived" || archived.NativeArchiveState != "archived" ||
		archived.ManagerPID != 0 || archived.WorkerPID != 0 || archived.MessagingSocket != "" || len(archived.CleanupDebt) != 0 {
		t.Fatalf("archived Qwen lane = %+v, %v", archived, err)
	}
	if _, err := os.Lstat(state.ControlSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Qwen control socket survived archive: %v", err)
	}
}

func TestQwenToolWrapperRegistersDetachedRootBeforeBashExec(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"state", "claude", "codex", "runtime"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	info := procinfo.Read(os.Getpid())
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		t.Fatal("current test process has no strong identity")
	}
	paths := resolveNativePaths()
	threadID, launchToken := randomID(), randomID()+randomID()
	state := qwenLaneState{
		Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
		Name: "qwen-tool-test", ThreadID: threadID, Cwd: root, Status: "idle", RuntimeDir: paths.runtimeDir,
		ManagerPID: os.Getpid(), ManagerProcStart: info.Start, ManagerStrongStart: info.StrongStart,
		WorkerPID: os.Getpid(), WorkerProcStart: info.Start, WorkerStrongStart: info.StrongStart,
		LaunchTokenHash: qwenLaneTokenHash(launchToken), LaunchPreference: "native_default", NativeArchiveState: "active",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeQwenLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &qwenLaneManager{paths: paths, state: state, launchToken: launchToken}
	if err := manager.prepareToolRegistry(); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSharedToolRootLedger(); err != nil {
		t.Fatal(err)
	}
	state = manager.state
	command := exec.Command(state.ToolWrapperPath, "-test.run=^TestQwenToolWrapperExecHelper$")
	command.Args[0] = "bash"
	command.Env = qwenLaneWorkerToolEnvironment(qwenLaneWorkerEnvironment(os.Environ(), state, launchToken), state)
	command.Env = replaceTestEnvironment(command.Env, qwenToolWrapperExecHelperEnv, "1")
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "QWEN_TOOL_WRAPPER_EXEC_OK" {
		t.Fatalf("Qwen wrapper subprocess: err=%v output=%q", err, output)
	}
	config, err := qwenToolRootLedgerConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := openToolRootLedger(config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ledger.snapshot()
	if err != nil || len(snapshot.Roots) != 1 || snapshot.Roots[0].PID != command.ProcessState.Pid() || snapshot.Roots[0].StrongStart == "" {
		t.Fatalf("Qwen wrapper ledger = %+v, %v", snapshot, err)
	}
}

func TestQwenACPClientRejectsUnknownInboundRequestAndMalformedFrames(t *testing.T) {
	agentInput, clientInput := io.Pipe()
	clientOutput, agentOutput := io.Pipe()
	client := newQwenACPClient(clientInput, clientOutput)
	peer := newQwenTestRPCPeer(agentInput, agentOutput)
	if err := peer.write(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "fs/read_text_file", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	response, err := peer.read()
	if err != nil || response.Error == nil || !strings.Contains(response.Error.Message, "unsupported") {
		t.Fatalf("unsupported response = %#v, %v", response, err)
	}
	if _, err := agentOutput.Write([]byte("not-json\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("Qwen ACP client did not reject the malformed frame")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.request(ctx, "initialize", map[string]any{}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed-frame error = %v", err)
	}
}

func TestQwenACPClientCorrelatesOutOfOrderResponses(t *testing.T) {
	agentInput, clientInput := io.Pipe()
	clientOutput, agentOutput := io.Pipe()
	client := newQwenACPClient(clientInput, clientOutput)
	defer func() { _ = client.close() }()
	peer := newQwenTestRPCPeer(agentInput, agentOutput)
	done := make(chan error, 1)
	go func() {
		first, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		second, err := peer.read()
		if err != nil {
			done <- err
			return
		}
		if err := peer.respond(second.ID, map[string]any{"method": second.Method}); err != nil {
			done <- err
			return
		}
		done <- peer.respond(first.ID, map[string]any{"method": first.Method})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		method string
		body   map[string]any
		err    error
	}
	results := make(chan result, 2)
	for _, method := range []string{"first", "second"} {
		method := method
		go func() {
			body, err := client.request(ctx, method, map[string]any{})
			results <- result{method: method, body: body, err: err}
		}()
	}
	for range 2 {
		got := <-results
		if got.err != nil || stringValue(got.body["method"]) != got.method {
			t.Fatalf("out-of-order %s response = %#v, %v", got.method, got.body, got.err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQwenLaneManagerRejectsUnknownACPVersionBeforeSessionMutation(t *testing.T) {
	agentInput, clientInput := io.Pipe()
	clientOutput, agentOutput := io.Pipe()
	client := newQwenACPClient(clientInput, clientOutput)
	defer func() { _ = client.close() }()
	peer := newQwenTestRPCPeer(agentInput, agentOutput)
	go func() {
		request, err := peer.read()
		if err == nil {
			_ = peer.respond(request.ID, map[string]any{
				"protocolVersion": 1, "agentInfo": map[string]any{"name": "qwen-code", "version": "0.20.0"},
				"agentCapabilities": map[string]any{
					"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}},
					"mcpCapabilities": map[string]any{"stdio": true},
				},
			})
		}
	}()
	manager := &qwenLaneManager{
		state: qwenLaneState{ThreadID: randomID(), Cwd: t.TempDir()}, client: client, worker: &grokManagedProcess{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.initializeACP(ctx); err == nil || !strings.Contains(err.Error(), "lacks the admitted") {
		t.Fatalf("unknown Qwen ACP version error = %v", err)
	}
}

func TestQwenLaneManagerOwnerExitArchivesOwnedButNotPersistentLane(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"state", "claude", "codex", "runtime"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	paths := resolveNativePaths()
	originalArchive := executeQwenArchiveTransaction
	executeQwenArchiveTransaction = func(qwenLaneState, string) error { return nil }
	t.Cleanup(func() { executeQwenArchiveTransaction = originalArchive })
	for _, persistent := range []bool{false, true} {
		name := "owned"
		if persistent {
			name = "persistent"
		}
		t.Run(name, func(t *testing.T) {
			owner := startQwenCleanupProcess(t)
			ownerInfo := procinfo.Read(owner.cmd.Process.Pid)
			state := qwenLaneState{
				Version: qwenLaneVersion, ContractVersion: qwenLaneContractVersion, Type: "qwen-peer-lane",
				Name: name, ThreadID: randomID(), QwenSessionID: randomID(), Cwd: root, Status: "idle",
				OwnerPID: owner.cmd.Process.Pid, OwnerProcStart: ownerInfo.Start, Persistent: persistent,
				LaunchPreference: "native_default", NativeArchiveState: "active",
				CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
			}
			if err := writeQwenLaneState(paths, state); err != nil {
				t.Fatal(err)
			}
			startupDone := make(chan struct{})
			close(startupDone)
			manager := &qwenLaneManager{
				paths: paths, state: state, turnNotify: make(chan struct{}, 1), done: make(chan struct{}), startupDone: startupDone,
			}
			go manager.maintenanceLoop()
			stopGrokManagedProcess(owner, time.Second)
			if !persistent {
				qwenTestPoll(t, qwenTestLifecycleTimeout, "owned Qwen lane archive", func() (bool, error) {
					latest, err := readQwenLaneState(paths, state.ThreadID)
					return err == nil && latest.Status == "archived", err
				})
				return
			}
			select {
			case <-manager.done:
				t.Fatal("persistent Qwen lane archived when former owner exited")
			case <-time.After(400 * time.Millisecond):
			}
			manager.shutdown("explicit archive", true)
		})
	}
}
