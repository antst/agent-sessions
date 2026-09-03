package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestCodexNativePreservesLaunchResolveDeliveryAndArchiveProtocol(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	workspace := codexNativeTestDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c001"
	var methodsMu sync.Mutex
	methods := []string{}
	archived := false
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methodsMu.Lock()
			methods = append(methods, method)
			methodsMu.Unlock()
		}
		switch method {
		case "initialize", "plugin/list", "config/mcpServer/reload", "thread/name/set", "thread/settings/update", "thread/unsubscribe", "thread/delete":
			return map[string]any{}, nil
		case "thread/archive":
			archived = true
			return map[string]any{}, nil
		case "thread/unarchive":
			if !archived {
				return nil, errors.New("thread is not archived")
			}
			archived = false
			return map[string]any{}, nil
		case "thread/list":
			params, _ := request["params"].(map[string]any)
			if archived && boolValue(params["archived"]) {
				return map[string]any{"data": []any{map[string]any{"id": threadID}}}, nil
			}
			return map[string]any{"data": []any{}}, nil
		case "thread/start":
			params, _ := request["params"].(map[string]any)
			if stringValue(params["cwd"]) != workspace || stringValue(params["approvalPolicy"]) != "never" ||
				stringValue(params["sandbox"]) != "danger-full-access" || stringValue(params["historyMode"]) != "legacy" {
				return nil, errors.New("start parameters changed")
			}
			return map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": workspace, "source": "appServer", "status": map[string]any{"type": "idle"},
			}, "approvalPolicy": "never"}, nil
		case "thread/resume":
			params, _ := request["params"].(map[string]any)
			if stringValue(params["threadId"]) != threadID || !boolValue(params["excludeTurns"]) || stringValue(params["cwd"]) != workspace {
				return nil, errors.New("resume parameters changed")
			}
			return map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": workspace, "source": "appServer", "status": map[string]any{"type": "idle"},
			}, "cwd": workspace, "approvalPolicy": "never"}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "name": "exact-peer", "cwd": workspace, "source": "appServer", "status": map[string]any{"type": "idle"},
			}}, nil
		case "turn/start":
			params, _ := request["params"].(map[string]any)
			input, _ := params["input"].([]any)
			if stringValue(params["threadId"]) != threadID || len(input) != 1 {
				return nil, errors.New("turn input changed")
			}
			return map[string]any{"turn": map[string]any{"id": "turn-c001", "status": "inProgress"}}, nil
		default:
			return nil, errors.New("unexpected native method " + method)
		}
	})
	startedNative := 0
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
		Start: func(context.Context, string, []string, []string) error {
			startedNative++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	if startedNative != 0 {
		t.Fatal("live App Server was started again")
	}
	if err := native.ReloadMCPServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid, procStart, evidenceSocket := native.AppServerEvidence()
	if pid != os.Getpid() || procStart == "" || evidenceSocket != socket {
		t.Fatalf("App Server evidence = %d/%q/%q", pid, procStart, evidenceSocket)
	}

	started, err := native.StartThread(context.Background(), CodexStartRequest{
		Cwd: workspace, Name: "exact peer", NameSource: "explicit",
		ApprovalPolicy: "never", Sandbox: "danger-full-access",
	})
	if err != nil || started.ID != threadID || started.Name != "exact-peer" || started.ApprovalPolicy != "never" {
		t.Fatalf("started thread = %+v, %v", started, err)
	}
	if prepared, err := native.PrepareLaneThread(context.Background(), threadID, workspace, "never", "danger-full-access"); err != nil || prepared.ID != threadID {
		t.Fatalf("prepare already-loaded lane thread = %+v, %v", prepared, err)
	}
	resolved, err := native.ResolveThread(context.Background(), threadID)
	if err != nil || resolved.ID != threadID || resolved.Name != "exact-peer" {
		t.Fatalf("resolved thread = %+v, %v", resolved, err)
	}
	if delivery, err := native.SendMessage(context.Background(), threadID, "exact content"); err != nil || delivery != "started" {
		t.Fatalf("delivery = %q, %v", delivery, err)
	}
	if err := native.ArchiveThread(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	if prepared, err := native.PrepareLaneThread(context.Background(), threadID, workspace, "never", "danger-full-access"); err != nil || prepared.ID != threadID {
		t.Fatalf("prepare archived lane thread = %+v, %v", prepared, err)
	}
	if err := native.DeleteThread(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	methodsMu.Lock()
	gotMethods := append([]string(nil), methods...)
	methodsMu.Unlock()
	wantMethods := []string{"plugin/list", "config/mcpServer/reload", "thread/start", "thread/name/set", "thread/resume", "thread/settings/update", "thread/read", "thread/settings/update", "thread/read", "thread/read", "turn/start", "thread/archive", "thread/unsubscribe", "thread/list", "thread/unarchive", "thread/resume", "thread/settings/update", "thread/delete"}
	if !reflect.DeepEqual(gotMethods, wantMethods) {
		t.Fatalf("native method sequence = %v, want %v", gotMethods, wantMethods)
	}
}

func TestCodexNativeListsEveryProductMatchAndResolvesOnlyExactID(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exactID := "00000000-0000-0000-0000-00000000c011"
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		params, _ := request["params"].(map[string]any)
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			if stringValue(params["threadId"]) != exactID {
				return nil, errors.New("thread/read target changed")
			}
			return map[string]any{"thread": map[string]any{
				"id": exactID, "name": "exact-id", "cwd": home, "source": "appServer",
			}}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)

	exact, err := native.ResolveThread(context.Background(), exactID)
	if err != nil || exact.ID != exactID || exact.Name != "exact-id" {
		t.Fatalf("exact resolution = %+v, %v", exact, err)
	}
	if _, err := native.ResolveThread(context.Background(), "shared-name"); err == nil || !strings.Contains(err.Error(), "exact thread UUID") {
		t.Fatalf("daemon accepted name selection: %v", err)
	}
}

func TestCodexLaneRecoveryRejectsPreferredHistoricalTurnWhenDifferentTurnIsActive(t *testing.T) {
	threadID := "00000000-0000-0000-0000-00000000c0aa"
	native := &CodexNative{activeTurns: map[string]string{threadID: "turn-b"}}
	if _, err := native.ResolveLaneTurnID(context.Background(), threadID, "turn-a"); err == nil ||
		!strings.Contains(err.Error(), "does not match durable turn") {
		t.Fatalf("active B versus preferred A error = %v", err)
	}
}

func TestCodexLaneRecoveryAcceptsPreferredTurnCompletedDuringDowntimeWithoutCompetingActive(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c0ab"
	turnID := "turn-completed-during-downtime"
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []map[string]any{{
				"id": turnID, "status": "completed", "completedAt": int64(2), "items": []any{map[string]any{
					"type": "agent_message", "phase": "final_answer", "text": "downtime terminal result",
				}},
			}}}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", stringValue(request["method"]))
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
		Start: func(context.Context, string, []string, []string) error {
			return errors.New("live app server must not restart")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	resolved, err := native.ResolveLaneTurnID(context.Background(), threadID, turnID)
	if err != nil || resolved != turnID {
		t.Fatalf("completed preferred recovery = %q, %v", resolved, err)
	}
	result, err := native.WaitLaneTurn(context.Background(), threadID, resolved)
	if err != nil || result.TurnID != turnID || result.Outcome != "completed" || result.Result != "downtime terminal result" {
		t.Fatalf("completed preferred collection = %+v, %v", result, err)
	}
}

func TestCodexNativeWaitReadsProductClosureExactlyOnce(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		turn        map[string]any
		wantOutcome string
		wantResult  string
		wantError   string
	}{
		{
			name: "closed completed turn",
			turn: map[string]any{
				"id": "turn-closed-completed", "status": "completed", "completedAt": int64(2),
				"items": []any{map[string]any{
					"type": "agent_message", "phase": "final_answer", "text": "completed after event",
				}},
			},
			wantOutcome: "completed",
			wantResult:  "completed after event",
		},
		{
			name:        "closed interrupted turn",
			turn:        map[string]any{"id": "turn-closed-interrupted", "status": "interrupted", "completedAt": int64(2)},
			wantOutcome: "interrupted",
		},
		{
			name:      "pre-closure interrupted projection",
			turn:      map[string]any{"id": "turn-open-interrupted", "status": "interrupted"},
			wantError: "not product-closed",
		},
		{
			name: "closed completed turn without final answer",
			turn: map[string]any{
				"id": "turn-completed-no-answer", "status": "completed", "completedAt": int64(2),
			},
			wantError: "has no final answer",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
			socket := filepath.Join(home, "app-server.sock")
			threadID := "00000000-0000-0000-0000-00000000c0ac"
			turnID := stringValue(test.turn["id"])
			var callsMu sync.Mutex
			calls := 0
			startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
				switch stringValue(request["method"]) {
				case "initialize":
					return map[string]any{}, nil
				case "thread/turns/list":
					params, _ := request["params"].(map[string]any)
					if stringValue(params["threadId"]) != threadID || intValue(params["limit"]) != 100 ||
						stringValue(params["sortDirection"]) != "desc" || stringValue(params["itemsView"]) != "full" || params["cursor"] != nil {
						return nil, errors.New("exact turn read parameters changed")
					}
					callsMu.Lock()
					calls++
					callsMu.Unlock()
					return map[string]any{"data": []any{test.turn}}, nil
				default:
					return nil, fmt.Errorf("unexpected method %s", stringValue(request["method"]))
				}
			})
			native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
				CodexBinary: executable, CodexHome: home, SocketPath: socket,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(native.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, err := native.WaitLaneTurn(ctx, threadID, turnID)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("wait error = %v, want %q", err, test.wantError)
				}
			} else if err != nil || result.Outcome != test.wantOutcome || result.Result != test.wantResult {
				t.Fatalf("wait result = %+v, %v; want outcome %q result %q", result, err, test.wantOutcome, test.wantResult)
			}
			callsMu.Lock()
			gotCalls := calls
			callsMu.Unlock()
			if gotCalls != 1 {
				t.Fatalf("thread/turns/list calls = %d, want 1", gotCalls)
			}
		})
	}
}

func TestCodexNativePostRestartIdleLaneResumesWithoutUnarchive(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	workspace := codexNativeTestDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c101"
	methods := []string{}
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods = append(methods, method)
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/unarchive":
			return nil, errors.New("thread already has an active writer")
		case "thread/resume":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": workspace, "source": "appServer", "status": map[string]any{"type": "idle"},
			}, "cwd": workspace, "approvalPolicy": "never"}, nil
		case "thread/settings/update":
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected native method " + method)
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	thread, err := native.PrepareLaneThread(context.Background(), threadID, workspace, "never", "danger-full-access")
	if err != nil || thread.ID != threadID || thread.Status != "idle" {
		t.Fatalf("post-restart idle prepare = %+v, %v", thread, err)
	}
	if containsString(methods, "thread/unarchive") {
		t.Fatalf("post-restart idle prepare called unarchive: %v", methods)
	}
}

func TestCodexNativeUnarchivesOnlyWhenProductReportsArchivedMembership(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	workspace := codexNativeTestDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c102"
	methods := []string{}
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods = append(methods, method)
		}
		switch method {
		case "initialize", "thread/unarchive", "thread/settings/update":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []any{map[string]any{"id": threadID}}}, nil
		case "thread/resume":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": workspace, "source": "appServer", "status": map[string]any{"type": "idle"},
			}, "cwd": workspace, "approvalPolicy": "never"}, nil
		default:
			return nil, errors.New("unexpected native method " + method)
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	thread, err := native.PrepareLaneThread(context.Background(), threadID, workspace, "never", "danger-full-access")
	if err != nil || thread.ID != threadID {
		t.Fatalf("archived lane prepare = %+v, %v", thread, err)
	}
	want := []string{"thread/list", "thread/unarchive", "thread/resume", "thread/settings/update"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("archived lane methods = %v, want %v", methods, want)
	}
}

func TestCodexNativeLazilyStartsSupportedAppServerAndLeavesItRunning(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", filepath.Join("app-server-control", "app-server-control.sock")))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = canonicalExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	socketRoot := filepath.Join(home, "app-server-control")
	if err := os.Mkdir(socketRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketRoot, "app-server-control.sock")
	startCalls := 0
	var fake *fakeAppServer
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
		Start: func(_ context.Context, binary string, args, _ []string) error {
			startCalls++
			if binary != executable || !reflect.DeepEqual(args, []string{"app-server", "daemon", "start"}) {
				return errors.New("lazy start command changed")
			}
			fake = startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
				if stringValue(request["method"]) != "initialize" {
					return nil, errors.New("unexpected request")
				}
				return map[string]any{}, nil
			})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if startCalls != 1 {
		t.Fatalf("lazy start calls = %d", startCalls)
	}
	native.Close()
	connection, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatalf("closing daemon client stopped native App Server: %v", err)
	}
	_ = connection.Close()
	if fake == nil || strings.TrimSpace(socket) == "" {
		t.Fatal("lazy App Server fixture was not created")
	}
}

func TestCodexNativeReopensOnceOnOperationAfterAppServerReplacement(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home, "app-server.sock")
	threadID := "00000000-0000-0000-0000-00000000c044"
	operationEntered := make(chan struct{})
	releaseOldOperation := make(chan struct{})
	first := startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			close(operationEntered)
			<-releaseOldOperation
			return map[string]any{"thread": map[string]any{"id": threadID}}, nil
		default:
			return nil, errors.New("unexpected old App Server operation")
		}
	})
	starts := 0
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
		Start: func(context.Context, string, []string, []string) error {
			starts++
			return errors.New("replacement was already running")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	oldClient := native.clientSnapshot()
	failed := make(chan error, 1)
	go func() {
		_, resolveErr := native.ResolveThread(context.Background(), threadID)
		failed <- resolveErr
	}()
	select {
	case <-operationEntered:
	case <-time.After(time.Second):
		t.Fatal("operation did not reach old App Server")
	}
	oldClient.close()
	first.close()
	close(releaseOldOperation)
	if err := <-failed; err == nil {
		t.Fatal("operation crossing App Server replacement was replayed or reported successful")
	}
	select {
	case <-oldClient.done:
	case <-time.After(time.Second):
		t.Fatal("old App Server connection did not close")
	}
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "name": "replacement", "cwd": home, "source": "appServer",
			}}, nil
		default:
			return nil, fmt.Errorf("unexpected replacement method %s", stringValue(request["method"]))
		}
	})
	if _, _, gotSocket, err := native.RefreshAppServerEvidence(context.Background()); err != nil || gotSocket != socket {
		t.Fatalf("refresh replacement evidence socket = %q, err = %v", gotSocket, err)
	}
	if native.clientSnapshot() == oldClient {
		t.Fatal("refresh retained the closed App Server observer")
	}
	thread, err := native.ResolveThread(context.Background(), threadID)
	if err != nil || thread.ID != threadID || thread.Name != "replacement" {
		t.Fatalf("replacement operation = %+v, %v", thread, err)
	}
	if starts != 0 {
		t.Fatalf("running replacement started %d extra App Servers", starts)
	}
}

func TestCodexNativeRecoversActiveTurnAfterDaemonRestart(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c002"
	turnID := "turn-c002"
	socket := filepath.Join(home, "app-server.sock")
	var methodsMu sync.Mutex
	methods := []string{}
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methodsMu.Lock()
			methods = append(methods, method)
			methodsMu.Unlock()
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/resume", "thread/read":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": "/workspace", "source": "appServer", "status": map[string]any{"type": "active"},
			}}, nil
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": turnID, "status": "inProgress"}}}, nil
		case "turn/steer":
			params, _ := request["params"].(map[string]any)
			if stringValue(params["threadId"]) != threadID || stringValue(params["expectedTurnId"]) != turnID {
				return nil, errors.New("steer did not use the recovered active turn")
			}
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected native method " + method)
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	if _, err := native.ReattachThread(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	resolvedTurn, err := native.ResolveLaneTurnID(context.Background(), threadID, turnID)
	if err != nil || resolvedTurn != turnID {
		t.Fatalf("resolved recovered turn = %q, %v", resolvedTurn, err)
	}
	if delivery, err := native.SendMessage(context.Background(), threadID, "after restart"); err != nil || delivery != "steered" {
		t.Fatalf("delivery = %q, %v", delivery, err)
	}
	methodsMu.Lock()
	gotMethods := append([]string(nil), methods...)
	methodsMu.Unlock()
	wantMethods := []string{"thread/resume", "thread/turns/list", "thread/turns/list", "thread/read", "turn/steer"}
	if !reflect.DeepEqual(gotMethods, wantMethods) {
		t.Fatalf("native method sequence = %v, want %v", gotMethods, wantMethods)
	}
}

func TestCodexNativeLaneTurnUsesAppServerAndCollectsFinalAnswer(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c003"
	turnID := "turn-c003"
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		params, _ := request["params"].(map[string]any)
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "turn/start":
			config, _ := params["config"].(map[string]any)
			features, _ := config["features"].(map[string]any)
			tools, _ := config["tools"].(map[string]any)
			if stringValue(params["threadId"]) != threadID || stringValue(params["model"]) != "lane-model" ||
				stringValue(params["approvalPolicy"]) != "never" ||
				fmt.Sprint(params["sandboxPolicy"]) != "map[type:dangerFullAccess]" ||
				boolValue(features["code_mode_host"]) || !boolValue(features["custom"]) || boolValue(tools["web_search"]) {
				return nil, errors.New("lane turn/start parameters changed")
			}
			return map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}}, nil
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{
				"id": turnID, "status": "completed", "completedAt": int64(2), "items": []any{map[string]any{
					"type": "agent_message", "phase": "final_answer", "text": "native lane result",
				}},
			}}}, nil
		default:
			return nil, errors.New("unexpected native method " + method)
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	started, err := native.StartLaneTurn(context.Background(), CodexLaneTurnRequest{
		ThreadID: threadID, Prompt: "do work", Model: "lane-model",
		ApprovalPolicy: "never", Sandbox: "danger-full-access",
		Arguments: []string{"--config", "features.custom=true", "--no-web"},
	})
	if err != nil || started != turnID {
		t.Fatalf("start lane turn = %q, %v", started, err)
	}
	// Product closure arrived before the waiter registered. The absence of the
	// exact active turn sends WaitLaneTurn directly to its one product read.
	native.observeNotification(codexTurnCompletedNotification(t, threadID, turnID, "completed"))
	result, err := native.WaitLaneTurn(context.Background(), threadID, turnID)
	if err != nil || result.Outcome != "completed" || result.Result != "native lane result" {
		t.Fatalf("wait lane turn = %+v, %v", result, err)
	}
}

func TestCodexNativeWaitCancellationDoesNotInterruptProductTurn(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c004"
	turnID := "turn-c004"
	interrupts := 0
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": turnID, "status": "inProgress"}}}, nil
		case "turn/interrupt":
			interrupts++
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected native method " + stringValue(request["method"]))
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	native.mu.Lock()
	native.activeTurns[threadID] = turnID
	native.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := native.WaitLaneTurn(ctx, threadID, turnID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait error = %v", err)
	}
	if interrupts != 0 {
		t.Fatalf("process-local wait cancellation sent %d native interrupts", interrupts)
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if len(native.turnWaiters) != 0 {
		t.Fatalf("canceled wait left waiters = %#v", native.turnWaiters)
	}
}

func TestCodexNativeWaitWakesOnlyForExactProductCompletion(t *testing.T) {
	home := codexNativeCanonicalDirectory(t, testutil.ShortSocketRoot(t, "cn-", "app-server.sock"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000c005"
	turnID := "turn-c005"
	var readsMu sync.Mutex
	reads := 0
	socket := filepath.Join(home, "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/turns/list":
			readsMu.Lock()
			reads++
			readsMu.Unlock()
			return map[string]any{"data": []any{map[string]any{
				"id": turnID, "status": "interrupted", "completedAt": int64(2),
			}}}, nil
		default:
			return nil, errors.New("unexpected native method " + stringValue(request["method"]))
		}
	})
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: executable, CodexHome: home, SocketPath: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	native.mu.Lock()
	native.activeTurns[threadID] = turnID
	native.mu.Unlock()

	resultCh := make(chan CodexLaneTurnResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, waitErr := native.WaitLaneTurn(context.Background(), threadID, turnID)
		resultCh <- result
		errCh <- waitErr
	}()
	waitForCodexTurnWaiter(t, native, codexLaneTurnKey{threadID: threadID, turnID: turnID})

	native.observeNotification(codexTurnCompletedNotification(t, threadID, "another-turn", "completed"))
	native.observeNotification(codexTurnCompletedNotification(t, "another-thread", turnID, "completed"))
	native.mu.Lock()
	active := native.activeTurns[threadID]
	waiters := len(native.turnWaiters)
	native.mu.Unlock()
	readsMu.Lock()
	readCount := reads
	readsMu.Unlock()
	if active != turnID || waiters != 1 || readCount != 0 {
		t.Fatalf("unrelated completion changed active=%q waiters=%d reads=%d", active, waiters, readCount)
	}

	// The real App Server uses turn/completed with status interrupted after an
	// exact turn/interrupt; the same exact closure wakes the waiter.
	native.observeNotification(codexTurnCompletedNotification(t, threadID, turnID, "interrupted"))
	result := <-resultCh
	if waitErr := <-errCh; waitErr != nil || result.Outcome != "interrupted" || result.TurnID != turnID {
		t.Fatalf("interrupted wait = %+v, %v", result, waitErr)
	}
	readsMu.Lock()
	readCount = reads
	readsMu.Unlock()
	if readCount != 1 {
		t.Fatalf("exact product reads = %d, want 1", readCount)
	}
}

func waitForCodexTurnWaiter(t *testing.T, native *CodexNative, key codexLaneTurnKey) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		native.mu.Lock()
		registered := native.turnWaiters[key] != nil
		native.mu.Unlock()
		if registered {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Codex turn waiter was not registered")
}

func codexTurnCompletedNotification(t *testing.T, threadID, turnID, status string) rpcNotification {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"threadId": threadID,
		"turn": map[string]any{
			"id": turnID, "status": status, "completedAt": int64(2),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rpcNotification{Method: "turn/completed", Params: params}
}

func TestCodexNativeProjectsNestedThreadStartedIdentity(t *testing.T) {
	var observed CodexNativeEvent
	native := &CodexNative{config: CodexNativeConfig{OnEvent: func(event CodexNativeEvent) { observed = event }}}
	params, err := json.Marshal(map[string]any{
		"thread": map[string]any{
			"id":  "00000000-0000-0000-0000-00000000c023",
			"cwd": "/native/cwd",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	native.observeNotification(rpcNotification{Method: "thread/started", Params: params})
	if observed.Kind != "thread/started" || observed.ThreadID != "00000000-0000-0000-0000-00000000c023" {
		t.Fatalf("thread/started event = %+v", observed)
	}
}

func TestCodexNativeLiveFreshPreparation(t *testing.T) {
	if os.Getenv("AGENT_SESSIONS_CODEX_LIVE") != "1" {
		t.Skip("set AGENT_SESSIONS_CODEX_LIVE=1 for an authenticated local App Server probe")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	native, err := OpenCodexNative(context.Background(), CodexNativeConfig{
		CodexBinary: binary, CodexHome: filepath.Join(home, ".codex"), Environment: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(native.Close)
	thread, err := native.StartThread(context.Background(), CodexStartRequest{
		Cwd: codexNativeTestDirectory(t), Name: "agent-sessions-live-probe",
		NameSource: "explicit", ApprovalPolicy: "never", Sandbox: "danger-full-access",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.DeleteThread(context.Background(), thread.ID) })
	if !validSessionID(thread.ID) || thread.Cwd == "" {
		t.Fatalf("live prepared thread = %+v", thread)
	}
}

func codexNativeTestDirectory(t *testing.T) string {
	t.Helper()
	return codexNativeCanonicalDirectory(t, t.TempDir())
}

func codexNativeCanonicalDirectory(t *testing.T, root string) string {
	t.Helper()
	path, err := pathidentity.ExistingDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
