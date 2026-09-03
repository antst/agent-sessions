package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productruntime"
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

func (driver *recordingMessageTestDriver) SendMessage(_ context.Context, ref productruntime.NativeSessionRef, message string) error {
	driver.refs = append(driver.refs, ref)
	driver.messages = append(driver.messages, message)
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
	joined := make(chan liveSessionReport, 1)
	server, err := startLivePresenceServer(ctx, shortDaemonTestRoot(t), func(report liveSessionReport) {
		joined <- report
	}, func(liveSessionReport) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	calls := 0
	startLiveSessionClient(ctx, server.listener.Addr().String(), liveSessionReport{
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
	err = coordinator.deliverLaneMessage(context.Background(), actor, "delivery", "wrapped")
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
		func(report liveSessionReport) { coordinator.joinLiveSession(runtime, report) },
		func(report liveSessionReport) { coordinator.leaveLiveSession(runtime, report) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = presence.Close() })
	coordinator.presence = presence
	delivered := make(chan struct{}, 2)
	_ = startLiveSessionClient(ctx, presence.listener.Addr().String(), liveSessionReport{
		UUID: "parent-id", Name: "parent-name", Product: "claude",
	}, func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
		if method != "message.deliver" {
			t.Fatalf("delivery method = %q", method)
		}
		delivered <- struct{}{}
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
		case <-delivered:
		case <-time.After(2 * time.Second):
			t.Fatalf("send to %q was not delivered", target)
		}
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
