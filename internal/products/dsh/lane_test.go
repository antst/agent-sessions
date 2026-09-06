package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/procinfo"
	"github.com/antst/sessionbus/internal/productruntime"
)

type testSupervisor struct {
	mu      sync.Mutex
	command productruntime.NativeCommand
	done    chan struct{}
	once    sync.Once
}

func newTestSupervisor() *testSupervisor { return &testSupervisor{done: make(chan struct{})} }

func (supervisor *testSupervisor) Start(_ context.Context, command productruntime.NativeCommand) (productruntime.OwnedProcessRef, error) {
	supervisor.mu.Lock()
	supervisor.command = command
	supervisor.mu.Unlock()
	return productruntime.OwnedProcessRef{Process: procinfo.Identity{PID: 42, Start: "1", StrongStart: "1"}, ProcessGroup: 42}, nil
}

func (supervisor *testSupervisor) Signal(context.Context, productruntime.OwnedProcessRef, productruntime.ProcessSignal) error {
	supervisor.once.Do(func() { close(supervisor.done) })
	return nil
}

func (supervisor *testSupervisor) Wait(ctx context.Context, _ productruntime.OwnedProcessRef) (productruntime.ProcessExit, error) {
	select {
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	case <-supervisor.done:
		return productruntime.ProcessExit{}, nil
	}
}

type testPresence struct {
	mu         sync.Mutex
	waited     []string
	calls      []string
	supervisor *testSupervisor
}

func (presence *testPresence) WaitLane(_ context.Context, sessionID string) error {
	presence.mu.Lock()
	presence.waited = append(presence.waited, sessionID)
	presence.mu.Unlock()
	return nil
}

func (presence *testPresence) CallLane(_ context.Context, sessionID, _ string, method string, params any) (json.RawMessage, error) {
	presence.mu.Lock()
	presence.calls = append(presence.calls, sessionID+":"+method)
	presence.mu.Unlock()
	switch method {
	case "lane.turn.start":
		body, _ := json.Marshal(params)
		var request struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(body, &request)
		return json.RawMessage(`{"native_message_id":"native-` + request.Mode + `"}`), nil
	case "lane.turn.wait":
		return json.RawMessage(`{"outcome":"completed","result":"DSH_OK","reason":{"kind":"completed"}}`), nil
	case "lane.turn.interrupt":
		return json.RawMessage(`{}`), nil
	case "lane.session.archive":
		presence.supervisor.once.Do(func() { close(presence.supervisor.done) })
		return json.RawMessage(`{}`), nil
	default:
		return nil, errors.New("unexpected DSH method")
	}
}

func TestNativeLaneUsesOnePresenceConnectionAndProductIdentity(t *testing.T) {
	supervisor := newTestSupervisor()
	presence := &testPresence{supervisor: supervisor}
	driver, err := NewLaneDriver(LaneConfig{
		Executable: "/usr/bin/dsh", Profile: ManagedProfile, Generation: 7,
		Processes: supervisor, Presence: presence, StartupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := driver.Capabilities(); got != (productruntime.LaneCapabilitySet{Steer: true, DurableResume: true, CallerSuppliedSessionID: true}) {
		t.Fatalf("capabilities = %#v", got)
	}
	ref, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "session-native", Name: "native lane", Cwd: "/work",
		PermissionMode: permissionmode.Default, Arguments: []string{"--model", "deepseek/deepseek-v4-flash"},
		Environment: []string{"AGENT_SESSIONS_PRODUCT=dsh", "AGENT_SESSIONS_SESSION_ID=session-native"}, Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref != (productruntime.NativeSessionRef{LaneID: "session-native", NativeSessionID: "session-native", Generation: 7}) {
		t.Fatalf("native ref = %#v", ref)
	}
	if !reflect.DeepEqual(presence.waited, []string{"session-native"}) {
		t.Fatalf("presence waits = %v", presence.waited)
	}
	wantEnv := []productruntime.EnvVar{
		{Name: "AGENT_SESSIONS_PRODUCT", Value: "dsh"},
		{Name: "AGENT_SESSIONS_SESSION_ID", Value: "session-native"},
		{Name: cwdEnvironment, Value: "/work"},
		{Name: permissionPresetEnv, Value: "workspace-write-noninteractive"},
		{Name: modelProviderEnv, Value: "deepseek"},
		{Name: modelIDEnvironment, Value: "deepseek-v4-flash"},
		{Name: reasoningEffortEnv, Value: "high"},
	}
	if supervisor.command.Path != "/usr/bin/dsh" || !reflect.DeepEqual(supervisor.command.Args, []string{"--profile", ManagedProfile}) ||
		supervisor.command.Cwd != "/work" || !reflect.DeepEqual(supervisor.command.Env, wantEnv) {
		t.Fatalf("native command = %#v", supervisor.command)
	}

	turn, err := driver.StartTurn(context.Background(), ref, productruntime.TurnStartRequest{Prompt: "run"})
	if err != nil || turn.NativeTurnID != "native-followup" {
		t.Fatalf("StartTurn() = %#v, %v", turn, err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "DSH_OK" || terminal.ExitLike != 0 {
		t.Fatalf("WaitTurn() = %#v, %v", terminal, err)
	}
	accepted, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{Prompt: "steer"})
	if err != nil || accepted.NativeSessionID != "session-native" || accepted.NativeMessageID != "native-steer" {
		t.Fatalf("Steer() = %#v, %v", accepted, err)
	}
	if err := driver.Interrupt(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"session-native:lane.turn.start", "session-native:lane.turn.wait", "session-native:lane.turn.start",
		"session-native:lane.turn.interrupt", "session-native:lane.session.archive",
	}
	if !reflect.DeepEqual(presence.calls, wantCalls) {
		t.Fatalf("presence calls = %v", presence.calls)
	}
}

func TestNativeLaneResumeAndPermissionStayInvocationOwned(t *testing.T) {
	for _, test := range []struct {
		mode   permissionmode.Mode
		preset string
	}{
		{permissionmode.Default, "workspace-write-noninteractive"},
		{permissionmode.BypassPermissions, "danger-full-access"},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			supervisor := newTestSupervisor()
			presence := &testPresence{supervisor: supervisor}
			driver, err := NewLaneDriver(LaneConfig{Executable: "dsh", Profile: ManagedProfile, Processes: supervisor, Presence: presence})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: ProductID, LaneID: "session-resume", ResumeNativeID: "session-resume", Cwd: "/next", PermissionMode: test.mode,
			})
			if err != nil || ref.LaneID != ref.NativeSessionID {
				t.Fatalf("resume = %#v, %v", ref, err)
			}
			got := map[string]string{}
			for _, variable := range supervisor.command.Env {
				got[variable.Name] = variable.Value
			}
			if got[resumeEnvironment] != "1" || got[permissionPresetEnv] != test.preset {
				t.Fatalf("resume environment = %v", got)
			}
			supervisor.once.Do(func() { close(supervisor.done) })
		})
	}
}

func TestNativeLaneRejectsUnrepresentableOptionsAndIdentity(t *testing.T) {
	supervisor := newTestSupervisor()
	driver, err := NewLaneDriver(LaneConfig{Executable: "dsh", Profile: ManagedProfile, Processes: supervisor, Presence: &testPresence{supervisor: supervisor}})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []productruntime.LaneOpenRequest{
		{ProductID: ProductID, LaneID: "one", ResumeNativeID: "two", Cwd: "/work", PermissionMode: permissionmode.Default},
		{ProductID: ProductID, LaneID: "one", Cwd: "/work", PermissionMode: permissionmode.Default, Arguments: []string{"--model", "missing-provider"}},
		{ProductID: ProductID, LaneID: "one", Cwd: "/work", PermissionMode: permissionmode.Default, Arguments: []string{"--agent", "other"}},
	} {
		if _, err := driver.Open(context.Background(), request); err == nil {
			t.Fatalf("invalid request accepted: %#v", request)
		}
	}
}
