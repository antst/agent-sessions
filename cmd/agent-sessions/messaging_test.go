package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/productruntime"
	grokproduct "github.com/antst/agent-sessions/internal/products/grok"
	ompproduct "github.com/antst/agent-sessions/internal/products/omp"
)

type passiveMessageTestDriver struct{}

func (*passiveMessageTestDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{}
}
func (*passiveMessageTestDriver) Open(context.Context, productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	return productruntime.NativeSessionRef{}, nil
}
func (*passiveMessageTestDriver) StartTurn(context.Context, productruntime.NativeSessionRef, productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	return productruntime.NativeTurnRef{}, nil
}
func (*passiveMessageTestDriver) WaitTurn(context.Context, productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	return productruntime.NativeTerminal{}, nil
}
func (*passiveMessageTestDriver) Steer(context.Context, productruntime.NativeTurnRef, productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
}
func (*passiveMessageTestDriver) Interrupt(context.Context, productruntime.NativeTurnRef) error {
	return nil
}
func (*passiveMessageTestDriver) Archive(context.Context, productruntime.NativeSessionRef) error {
	return nil
}

type recordingMessageTestDriver struct {
	passiveMessageTestDriver
	refs     []productruntime.NativeSessionRef
	messages []string
	err      error
}

type cwdRecordingLaneDriver struct {
	passiveMessageTestDriver
	opened  chan productruntime.LaneOpenRequest
	release chan struct{}
}

func (driver *cwdRecordingLaneDriver) Open(_ context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	driver.opened <- request
	return productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: request.LaneID, Generation: 1}, nil
}

func (*cwdRecordingLaneDriver) StartTurn(_ context.Context, session productruntime.NativeSessionRef, _ productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	return productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: "turn"}, nil
}

func (driver *cwdRecordingLaneDriver) WaitTurn(context.Context, productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	<-driver.release
	return productruntime.NativeTerminal{Outcome: productruntime.TurnCompleted}, nil
}

func TestLiveSessionDispatchExposesExactlyTheFirstClassV1Methods(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	report := livepresence.Report{UUID: "missing-source", Product: "future", Groups: []string{"team"}, Info: map[string]string{}}
	supported := map[string]string{
		"peers.list":     `{}`,
		"message.send":   `{"target":"peer","message":"hello"}`,
		"lane.doctor":    `{"product":"codex","arguments":["--json"]}`,
		"lane.list":      `{"product":"codex","arguments":["--mine"]}`,
		"lane.start":     `{"product":"codex","arguments":["--name","worker"],"input":"work"}`,
		"lane.run":       `{"product":"codex","arguments":["--name","worker"],"input":"work"}`,
		"lane.resume":    `{"product":"codex","arguments":["worker"],"input":"work"}`,
		"lane.steer":     `{"product":"codex","arguments":["worker"],"input":"work"}`,
		"lane.wait":      `{"product":"codex","arguments":["worker"]}`,
		"lane.status":    `{"product":"codex","arguments":["worker"]}`,
		"lane.interrupt": `{"product":"codex","arguments":["worker"]}`,
		"lane.archive":   `{"product":"codex","arguments":["worker"]}`,
	}
	for method, params := range supported {
		_, err := coordinator.handleLiveSessionCall(context.Background(), runtime, report, "request", method, json.RawMessage(params))
		var rpcErr *livepresence.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == livepresence.NotPermitted {
			t.Fatalf("supported method %s was refused: %v", method, err)
		}
	}
	_, err := coordinator.handleLiveSessionCall(context.Background(), runtime, report, "request", "lane.status",
		json.RawMessage(`{"product":"codex","arguments":["worker"],"cwd":"relative"}`))
	var invalid *livepresence.RPCError
	if !errors.As(err, &invalid) || invalid.Code != livepresence.InvalidParams {
		t.Fatalf("relative lane cwd = %v", err)
	}
	for _, method := range []string{"tool.call", "tools/call", "lane.collect", "broadcast", "identity"} {
		_, err := coordinator.handleLiveSessionCall(context.Background(), runtime, report, "request", method, json.RawMessage(`{}`))
		var rpcErr *livepresence.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != livepresence.NotPermitted {
			t.Fatalf("legacy method %s = %v", method, err)
		}
	}
}

func TestLiveMessageSendRequiresExactlyOneSelector(t *testing.T) {
	for _, input := range []string{
		`{"target":"peer","message":"hello"}`,
		`{"targets":["one","two"],"message":"hello"}`,
		`{"group":"team","message":"hello"}`,
	} {
		if _, err := decodeLiveMessageSend(json.RawMessage(input)); err != nil {
			t.Fatalf("valid message.send %s: %v", input, err)
		}
	}
	for _, input := range []string{
		`{"message":"hello"}`,
		`{"target":"one","targets":["two"],"message":"hello"}`,
		`{"target":"one","group":"team","message":"hello"}`,
		`{"targets":[],"message":"hello"}`,
		`{"group":"","message":"hello"}`,
	} {
		if _, err := decodeLiveMessageSend(json.RawMessage(input)); err == nil {
			t.Fatalf("invalid message.send accepted: %s", input)
		}
	}
}

func (driver *recordingMessageTestDriver) SendMessage(_ context.Context, ref productruntime.NativeSessionRef, message productruntime.NativeMessage) error {
	driver.refs = append(driver.refs, ref)
	driver.messages = append(driver.messages, message.Body)
	return driver.err
}

func TestMessagingToolsDeliverLaneThroughOneNativePathWithoutReadingItsState(t *testing.T) {
	for _, state := range []string{"running", "idle"} {
		for _, target := range []string{"worker", "lane-id", "native-id"} {
			t.Run(state+"/"+target, func(t *testing.T) {
				runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtime.Close() })
				parentGroup := "session:" + runtime.HostID() + "/parent"
				laneGroup := "session:" + runtime.HostID() + "/lane-id"
				activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
					ID: "parent", Product: "codex", NativeSessionID: "native-parent",
					Cwd: "/work", PermissionMode: "bypassPermissions",
				})
				coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
				driver := &recordingMessageTestDriver{}
				coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"claude": driver})
				if err != nil {
					t.Fatal(err)
				}
				coordinator.lanesLoaded = true
				actor := &laneActor{
					id: "lane-id", product: "claude", nativeID: "native-id", nativeGeneration: 7, name: "worker",
					parentID: "parent", cwd: "/work",
					groups:     []string{parentGroup, laneGroup},
					permission: "default", state: state, turnID: "existing-turn", nativeTurnID: "existing-native-turn",
				}
				coordinator.lanes["lane-id"] = actor

				listed, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "list_peers", map[string]any{})
				if err != nil {
					t.Fatal(err)
				}
				peers := listed.Data.(map[string]any)["peers"].([]map[string]any)
				if len(peers) != 1 || peers[0]["id"] != "lane-id" || peers[0]["session_id"] != "native-id" || peers[0]["name"] != "worker" {
					t.Fatalf("advertised peers = %#v", peers)
				}
				_, err = coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
					"target": target, "message": "mid-flight correction",
				})
				wantRef := productruntime.NativeSessionRef{LaneID: "lane-id", NativeSessionID: "native-id", Generation: 7}
				if err != nil || !reflect.DeepEqual(driver.refs, []productruntime.NativeSessionRef{wantRef}) ||
					len(driver.messages) != 1 || !strings.Contains(driver.messages[0], "mid-flight correction") {
					t.Fatalf("delivery refs=%+v messages=%q err=%v", driver.refs, driver.messages, err)
				}
				if actor.state != state || actor.turnID != "existing-turn" || actor.nativeTurnID != "existing-native-turn" {
					t.Fatalf("message mutated lane turn state: %+v", actor)
				}
			})
		}
	}
}

func TestPluginLaneMessageUsesPresenceOnceAndReturnsProductErrorVerbatim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	joined := make(chan livepresence.Report, 1)
	server, err := startLivePresenceServer(ctx, shortDaemonTestRoot(t), func(report livepresence.Report) {
		joined <- report
	}, func(livepresence.Report) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	calls := 0
	livepresence.StartClient(ctx, server.listener.Addr().String(), livepresence.Report{
		UUID: "native-id", Name: "worker", Groups: []string{"shared"}, Product: "qwen",
	}, func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		calls++
		if method != "message.deliver" || !strings.Contains(string(params), `"message_id":"delivery"`) ||
			!strings.Contains(string(params), `"body":"wrapped"`) {
			t.Fatalf("presence delivery method=%q params=%s", method, params)
		}
		return nil, errors.New("product rejected exact delivery")
	})
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin lane presence did not join")
	}
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	coordinator.presence = server
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{
		"qwen": &passiveMessageTestDriver{},
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := &laneActor{
		id: "lane-id", nativeID: "native-id", nativeGeneration: 4, product: "qwen",
		state: "running", turnID: "untouched", nativeTurnID: "untouched-native",
	}
	err = coordinator.deliverLaneMessage(context.Background(), actor, productruntime.NativeMessage{
		ID: "delivery", Body: "wrapped", From: productruntime.NativeMessageSource{
			UUID: "parent", Name: "parent", Product: "codex", Groups: []string{"shared"},
		},
	})
	if err == nil || err.Error() != "product rejected exact delivery" || calls != 1 {
		t.Fatalf("presence calls=%d err=%v", calls, err)
	}
	if actor.state != "running" || actor.turnID != "untouched" || actor.nativeTurnID != "untouched-native" {
		t.Fatalf("presence delivery mutated lane state: %+v", actor)
	}
}

func TestLaneCanListAndMessageParentThroughItsPrivateAnchor(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), root)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	presence, err := startLivePresenceServer(ctx, root,
		func(report livepresence.Report) { coordinator.joinLiveSession(runtime, report) },
		func(report livepresence.Report) { coordinator.leaveLiveSession(runtime, report) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = presence.Close() })
	coordinator.presence = presence
	delivered := make(chan productruntime.NativeMessage, 2)
	_ = livepresence.StartClient(ctx, presence.listener.Addr().String(), livepresence.Report{
		UUID: "parent-id", Name: "parent-name", Product: "claude",
	}, func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "message.deliver" {
			t.Fatalf("delivery method = %q", method)
		}
		var delivery productruntime.NativeMessage
		if err := json.Unmarshal(params, &delivery); err != nil {
			t.Fatalf("decode delivery: %v", err)
		}
		delivered <- delivery
		return json.RawMessage(`{}`), nil
	})
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, active, _ := runtime.Attachments().ActiveAttachment("parent-id"); active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parent presence did not join")
		}
		time.Sleep(5 * time.Millisecond)
	}
	parentAnchor := "session:" + runtime.HostID() + "/parent-id"
	coordinator.lanes["lane-id"] = &laneActor{
		id: "lane-id", nativeID: "lane-native", name: "lane-name", product: "qwen",
		parentID: "parent-id", groups: []string{parentAnchor, parentAnchor + "/lane-native"}, state: "running",
	}
	listed, err := coordinator.callLocalTool(context.Background(), runtime, "lane-id", "list_peers", map[string]any{})
	if err != nil || !strings.Contains(listed.Text, "parent-name") || !strings.Contains(listed.Text, parentAnchor) {
		t.Fatalf("lane-visible parent = %q, %v", listed.Text, err)
	}
	for _, target := range []string{"parent-name", "parent-id"} {
		if _, err := coordinator.callLocalTool(context.Background(), runtime, "lane-id", "send_message", map[string]any{
			"target": target, "message": "from lane",
		}); err != nil {
			t.Fatalf("send to %q: %v", target, err)
		}
		select {
		case delivery := <-delivered:
			if delivery.Body != "from lane" || delivery.From.UUID != "lane-native" || delivery.From.Name != "lane-name" {
				t.Fatalf("lane delivery = %#v", delivery)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("send to %q was not delivered", target)
		}
	}
	if _, err := coordinator.callLocalTool(context.Background(), runtime, "lane-id", "send_message", map[string]any{
		"group": parentAnchor, "message": "to parent group",
	}); err != nil {
		t.Fatalf("group send: %v", err)
	}
	select {
	case delivery := <-delivered:
		if delivery.Body != "to parent group" {
			t.Fatalf("group delivery = %#v", delivery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("group send was not delivered")
	}
	empty, err := coordinator.callLocalTool(context.Background(), runtime, "lane-id", "send_message", map[string]any{
		"group": parentAnchor + "/lane-native", "message": "only me",
	})
	if err != nil || len(empty.Data.(map[string]any)["deliveries"].([]federationpkg.DeliveryResult)) != 0 {
		t.Fatalf("private group send = %#v, %v", empty, err)
	}
	if got := coordinator.attachmentDisplayName(runtime, daemonpkg.ManagedAttachment{ID: "parent-id"}); got != "parent-name" {
		t.Fatalf("peer display name changed = %q", got)
	}
}

func TestUnifiedRenameNeverCreatesADurableDaemonAlias(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: "peer", Product: "claude", NativeSessionID: "native",
	})
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	_, err = coordinator.callLocalTool(context.Background(), runtime, "peer", "rename_session", map[string]any{
		"name": "Builder Corrected",
	})
	if err == nil || !strings.Contains(err.Error(), "native rename driver") {
		t.Fatalf("rename error = %v", err)
	}
	current, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(current.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"attachments"`) || strings.Contains(string(encoded), `"name"`) {
		t.Fatalf("live peer leaked into durable catalog: %s", encoded)
	}
}

func TestClaudePeerNameComesFromTheProductTranscriptAtQueryTime(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", profile)
	project := filepath.Join(profile, "projects", "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-1111-4111-8111-111111111111"
	if err := os.WriteFile(filepath.Join(project, id+".jsonl"), []byte(
		`{"sessionId":"`+id+`","type":"custom-title","customTitle":"native-name"}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := newPresenceTestRuntime(t)
	runtime.Attachments().ReportLive("source", "source", "codex", []string{"project"}, map[string]string{}, false)
	runtime.Attachments().ReportLive(id, "launch-name", "claude", []string{"project"}, map[string]string{}, false)
	runtime.Attachments().ReportLive("22222222-2222-4222-8222-222222222222", "hello-name", "claude", []string{"project"}, map[string]string{}, false)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })

	if got := coordinator.attachmentDisplayName(runtime, daemonpkg.ManagedAttachment{
		ID: id, NativeSessionID: id, Product: "claude",
	}); got != "native-name" {
		t.Fatalf("Claude display name = %q", got)
	}
	if got := coordinator.attachmentDisplayName(runtime, daemonpkg.ManagedAttachment{
		ID: "22222222-2222-4222-8222-222222222222", NativeSessionID: "22222222-2222-4222-8222-222222222222", Product: "claude",
	}); got != "hello-name" {
		t.Fatalf("Claude hello fallback name = %q", got)
	}
	listed, err := coordinator.callLocalTool(context.Background(), runtime, "source", "list_peers", map[string]any{})
	if err != nil || !strings.Contains(listed.Text, "native-name") {
		t.Fatalf("Claude peer list = %q, %v", listed.Text, err)
	}
	_, err = coordinator.callLocalTool(context.Background(), runtime, "source", "send_message", map[string]any{
		"target": "native-name", "message": "hello",
	})
	if err == nil || err.Error() != "live session channel is unavailable" {
		t.Fatalf("send-by-native-name did not select the live Claude peer: %v", err)
	}
}

func TestOMPHasAnOnDemandProductTitleResolver(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	if coordinator.liveTitleResolvers[ompproduct.ProductID] == nil {
		t.Fatal("OMP live title resolver was not composed")
	}
	calls := 0
	coordinator.liveTitleResolvers[ompproduct.ProductID] = func(attachments []daemonpkg.ManagedAttachment) map[string]string {
		calls++
		if len(attachments) != 2 {
			t.Fatalf("OMP resolver attachments = %#v", attachments)
		}
		return map[string]string{"omp-session": "native-name"}
	}
	runtime := newPresenceTestRuntime(t)
	runtime.Attachments().ReportLive("omp-session", "launch-name", ompproduct.ProductID, []string{"project"}, map[string]string{}, false)
	runtime.Attachments().ReportLive("missing", "hello-name", ompproduct.ProductID, []string{"project"}, map[string]string{}, false)
	attachments := []daemonpkg.ManagedAttachment{
		{ID: "omp-session", NativeSessionID: "omp-session", Product: ompproduct.ProductID},
		{ID: "missing", NativeSessionID: "missing", Product: ompproduct.ProductID},
	}
	names := coordinator.attachmentDisplayNames(runtime, attachments)
	if names["omp-session"] != "native-name" || names["missing"] != "hello-name" || calls != 1 {
		t.Fatalf("OMP display names = %#v, resolver calls = %d", names, calls)
	}
	names = coordinator.attachmentDisplayNames(runtime, attachments)
	if names["omp-session"] != "native-name" || names["missing"] != "hello-name" || calls != 2 {
		t.Fatalf("second OMP query names = %#v, resolver calls = %d", names, calls)
	}
}

func TestGrokHasAnOnDemandProductTitleResolver(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	if coordinator.liveTitleResolvers[grokproduct.ProductID] == nil {
		t.Fatal("Grok live title resolver was not composed")
	}
	coordinator.liveTitleResolvers[grokproduct.ProductID] = func(attachments []daemonpkg.ManagedAttachment) map[string]string {
		for _, attachment := range attachments {
			if attachment.NativeSessionID == "grok-session" {
				return map[string]string{attachment.ID: "native-name"}
			}
		}
		return map[string]string{}
	}
	runtime := newPresenceTestRuntime(t)
	runtime.Attachments().ReportLive("grok-session", "launch-name", grokproduct.ProductID, []string{"project"}, map[string]string{}, false)
	runtime.Attachments().ReportLive("missing", "hello-name", grokproduct.ProductID, []string{"project"}, map[string]string{}, false)
	if got := coordinator.attachmentDisplayName(runtime, daemonpkg.ManagedAttachment{
		ID: "grok-session", NativeSessionID: "grok-session", Product: grokproduct.ProductID,
	}); got != "native-name" {
		t.Fatalf("Grok display name = %q", got)
	}
	if got := coordinator.attachmentDisplayName(runtime, daemonpkg.ManagedAttachment{
		ID: "missing", NativeSessionID: "missing", Product: grokproduct.ProductID,
	}); got != "hello-name" {
		t.Fatalf("Grok hello fallback name = %q", got)
	}
}

func TestToolsOnlyConnectorsUseTheirLiveSessionAsTheCaller(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	for _, product := range []string{"codex", "grok"} {
		t.Run(product, func(t *testing.T) {
			sessionID := product + "-session"
			activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
				ID: sessionID, Product: product, NativeSessionID: sessionID, Groups: []string{"review"},
			})
			payload, marshalErr := json.Marshal(connectorToolEnvelope{
				SourceID: sessionID, RequestID: "mcp-" + product, Name: "identity", Arguments: map[string]any{},
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			response, callErr := coordinator.handleConnectorTool(context.Background(), runtime, daemonpkg.ControlRequest{Payload: payload})
			if callErr != nil {
				t.Fatal(callErr)
			}
			var result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if json.Unmarshal(response, &result) != nil || result.IsError || len(result.Content) != 1 ||
				!strings.Contains(result.Content[0].Text, sessionID) {
				t.Fatalf("connector tool response = %s", response)
			}
		})
	}
}

func TestPresenceInvocationCwdIsExplicitAndConnectorUsesTheProductAttachmentCwd(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: "session", Product: "claude", NativeSessionID: "native", Cwd: "/stored",
	})
	runtime.Attachments().ReportLive("session", "session", "claude", nil, map[string]string{"cwd": "/stored"}, false)
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))

	emptyCwd := ""
	withoutCwd, err := coordinator.callLiveTool(context.Background(), runtime,
		"session", "without-cwd", "identity", map[string]any{}, &emptyCwd)
	if err != nil || !strings.Contains(string(withoutCwd), `"cwd":""`) {
		t.Fatalf("live call without cwd = %s, %v", withoutCwd, err)
	}

	liveCwd := "/live-call"
	live, err := coordinator.callLiveTool(context.Background(), runtime,
		"session", "request", "identity", map[string]any{}, &liveCwd)
	if err != nil || !strings.Contains(string(live), `"cwd":"/live-call"`) {
		t.Fatalf("live connector result = %s, %v", live, err)
	}

	controlPayload, err := json.Marshal(connectorToolEnvelope{
		SourceID: "session", RequestID: "request", Name: "identity", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolsOnly, err := coordinator.handleConnectorTool(context.Background(), runtime, daemonpkg.ControlRequest{Payload: controlPayload})
	if err != nil || !strings.Contains(string(toolsOnly), `"cwd":"/stored"`) {
		t.Fatalf("tools-only connector result = %s, %v", toolsOnly, err)
	}
	stored, active, err := runtime.Attachments().ActiveAttachment("session")
	if err != nil || !active || stored.Cwd != "/stored" {
		t.Fatalf("stored attachment changed = %+v, active=%v, err=%v", stored, active, err)
	}
}

func TestConnectorLaneStartUsesTheProductAttachmentCwd(t *testing.T) {
	root := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	productCwd := t.TempDir()
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: "codex-session", Product: "codex", NativeSessionID: "codex-session", Cwd: productCwd,
		Groups: []string{"team"}, PermissionMode: "bypassPermissions",
	})
	runtime.Attachments().ReportLive("codex-session", "codex-session", "codex", []string{"team"}, map[string]string{"cwd": productCwd}, false)
	native := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(native, []byte("#!/bin/sh\nprintf '%s\\n' 'claude fixture'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", native)
	driver := &cwdRecordingLaneDriver{opened: make(chan productruntime.LaneOpenRequest, 1), release: make(chan struct{})}
	coordinator := newHostCoordinator(context.Background(), root)
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"claude": driver})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(connectorToolEnvelope{
		SourceID: "codex-session", RequestID: "lane-start", Name: "lane", Arguments: map[string]any{
			"product": "claude", "command": "start", "arguments": []string{"--name", "worker"}, "input": "work",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := coordinator.handleConnectorTool(context.Background(), runtime, daemonpkg.ControlRequest{Payload: payload})
	if err != nil || strings.Contains(string(response), `"isError":true`) {
		t.Fatalf("connector lane start = %s, %v", response, err)
	}
	select {
	case request := <-driver.opened:
		if request.Cwd != productCwd {
			t.Fatalf("lane cwd = %q, want product attachment cwd %q", request.Cwd, productCwd)
		}
	case <-time.After(time.Second):
		t.Fatal("connector lane start did not reach the driver")
	}
	coordinator.mu.Lock()
	var done chan struct{}
	for _, actor := range coordinator.lanes {
		done = actor.done
	}
	coordinator.mu.Unlock()
	close(driver.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connector lane test turn did not finish")
	}
}
