package pifamily

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type scriptedRPCProcess struct {
	frames                chan []byte
	mu                    sync.Mutex
	writes                []map[string]any
	sessionID             string
	streaming             bool
	answer                string
	cleaned               bool
	cleanupErr            error
	commandErrors         map[string]error
	responseErrors        map[string]string
	suppressLastResponses int
	closeOnLastResponse   bool
}

func newScriptedProcess(sessionID string) *scriptedRPCProcess {
	return &scriptedRPCProcess{frames: make(chan []byte, 64), sessionID: sessionID, answer: "native answer"}
}

func (process *scriptedRPCProcess) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-process.frames:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	}
}

func (process *scriptedRPCProcess) WriteFrame(_ context.Context, frame []byte) error {
	var request map[string]any
	if json.Unmarshal(frame, &request) != nil {
		return errors.New("bad request")
	}
	process.mu.Lock()
	process.writes = append(process.writes, request)
	command, id := request["type"].(string), request["id"].(string)
	commandErr := process.commandErrors[command]
	if commandErr != nil {
		process.mu.Unlock()
		return commandErr
	}
	streaming := process.streaming
	suppressResponse := false
	if command == "get_last_assistant_text" && process.suppressLastResponses > 0 {
		process.suppressLastResponses--
		suppressResponse = true
	}
	closeResponseStream := command == "get_last_assistant_text" && process.closeOnLastResponse
	if closeResponseStream {
		process.closeOnLastResponse = false
	}
	switch command {
	case "prompt":
		process.streaming = true
	case "abort":
		process.streaming = false
	}
	process.mu.Unlock()
	if suppressResponse {
		return nil
	}
	if responseError := process.responseErrors[command]; responseError != "" {
		process.emit(map[string]any{"type": "response", "id": id, "command": command, "success": false, "error": responseError})
		return nil
	}
	if closeResponseStream {
		close(process.frames)
		return nil
	}
	data := map[string]any{}
	switch command {
	case "get_state":
		data = map[string]any{"sessionId": process.sessionID, "sessionName": "live name", "isStreaming": streaming, "isCompacting": false}
	case "get_last_assistant_text":
		data = map[string]any{"text": process.answer}
	}
	process.emit(map[string]any{"type": "response", "id": id, "command": command, "success": true, "data": data})
	return nil
}

func (*scriptedRPCProcess) Signal(context.Context, productruntime.ProcessSignal) error { return nil }
func (*scriptedRPCProcess) Wait(context.Context) (productruntime.ProcessExit, error) {
	return productruntime.ProcessExit{}, nil
}
func (process *scriptedRPCProcess) Cleanup(context.Context) error {
	process.mu.Lock()
	process.cleaned = true
	err := process.cleanupErr
	process.mu.Unlock()
	return err
}
func (process *scriptedRPCProcess) emit(value any) {
	body, _ := json.Marshal(value)
	process.frames <- body
}
func (process *scriptedRPCProcess) commands() []string {
	process.mu.Lock()
	defer process.mu.Unlock()
	result := make([]string, 0, len(process.writes))
	for _, write := range process.writes {
		result = append(result, write["type"].(string))
	}
	return result
}
func (process *scriptedRPCProcess) messageFor(command string) string {
	process.mu.Lock()
	defer process.mu.Unlock()
	for _, write := range process.writes {
		if write["type"] == command {
			value, _ := write["message"].(string)
			return value
		}
	}
	return ""
}

type oneProcessFactory struct {
	process *scriptedRPCProcess
	command productruntime.NativeCommand
	starts  int
}

type sequenceProcessFactory struct {
	processes []*scriptedRPCProcess
	next      int
}

func (factory *sequenceProcessFactory) StartRPC(context.Context, productruntime.NativeCommand) (RPCProcess, error) {
	if factory.next >= len(factory.processes) {
		return nil, errors.New("no scripted process")
	}
	process := factory.processes[factory.next]
	factory.next++
	return process, nil
}

func (factory *oneProcessFactory) StartRPC(_ context.Context, command productruntime.NativeCommand) (RPCProcess, error) {
	factory.command = command
	factory.starts++
	return factory.process, nil
}

func familyPermission(mode permissionmode.Mode) (PermissionPolicy, error) {
	if mode != permissionmode.Default {
		return PermissionPolicy{}, productruntime.ErrUnsupportedPolicy
	}
	return NewPermissionPolicy("test", "--tools", "read"), nil
}

func TestPiLaneUsesStateReadinessAgentSettledAndExactResume(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	process := newScriptedProcess("pi-native")
	factory := &oneProcessFactory{process: process}
	driver, err := NewLaneDriver(LaneConfig{
		Quirks: quirks, Generation: 7, Processes: factory,
		MapPermission: familyPermission,
		Now:           func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: PiProductID, LaneID: "lane-pi", ResumeNativeID: "pi-native", Cwd: "/work",
		PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--mode", "rpc", "--session", "pi-native", "--tools", "read"}; !reflect.DeepEqual(factory.command.Args, want) {
		t.Fatalf("Pi resume args = %q, want %q", factory.command.Args, want)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "do work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"stopReason": "stop"}}})
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = driver.WaitTurn(waitCtx, turn)
	waitCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pi agent_end unexpectedly completed the turn: %v", err)
	}
	process.emit(map[string]any{"type": "agent_settled", "messages": []any{map[string]any{"stopReason": "stop"}}})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "native answer" || terminal.ResultDigest != sha256.Sum256([]byte("native answer")) {
		t.Fatalf("terminal = %+v", terminal)
	}
	if err := driver.Archive(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTurnRetriesCollectionFromCachedTerminalAfterCancellation(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	process := newScriptedProcess("pi-retry-terminal")
	process.suppressLastResponses = 1
	factory := &oneProcessFactory{process: process}
	driver, err := NewLaneDriver(LaneConfig{
		Quirks: quirks, Generation: 8, Processes: factory,
		MapPermission: familyPermission,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: PiProductID, LaneID: "terminal-retry", Name: "terminal retry", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "do work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{"type": "agent_settled", "messages": []any{map[string]any{"stopReason": "stop"}}})
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, err = driver.WaitTurn(firstCtx, turn)
	firstCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("post-terminal collection cancellation = %v", err)
	}
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "native answer" {
		t.Fatalf("retry from cached terminal = %+v, %v", terminal, err)
	}
	commands := process.commands()
	lastCalls := 0
	for _, command := range commands {
		if command == "get_last_assistant_text" {
			lastCalls++
		}
	}
	if lastCalls != 2 {
		t.Fatalf("result collection calls = %d, commands = %q", lastCalls, commands)
	}
	process.mu.Lock()
	process.streaming = false
	process.mu.Unlock()
	if _, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "next work", PermissionMode: permissionmode.Default}); err != nil {
		t.Fatalf("completed retry did not clear active turn: %v", err)
	}
}

func TestWaitTurnDoesNotInventEmptyResultWhenCollectionIsUnavailable(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	process := newScriptedProcess("pi-unavailable-result")
	process.closeOnLastResponse = true
	driver, err := NewLaneDriver(LaneConfig{
		Quirks: quirks, Generation: 9, Processes: &oneProcessFactory{process: process},
		MapPermission: familyPermission,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
		ProductID: PiProductID, LaneID: "result-unavailable", Name: "result unavailable", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "do work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{"type": "agent_settled", "messages": []any{map[string]any{"stopReason": "stop"}}})
	terminal, err := driver.WaitTurn(ctx, turn)
	if !errors.Is(err, productruntime.ErrUnavailable) || terminal.Outcome == productruntime.TurnCompleted || terminal.Result != "" {
		t.Fatalf("unavailable collection invented terminal = %+v, %v", terminal, err)
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer retryCancel()
	terminal, err = driver.WaitTurn(retryCtx, turn)
	if !errors.Is(err, productruntime.ErrUnavailable) || terminal.Outcome == productruntime.TurnCompleted {
		t.Fatalf("cached-terminal retry invented success = %+v, %v", terminal, err)
	}
}

func TestOMPLaneRequiresReadyPreservesSteerAndIgnoresContinuingEnd(t *testing.T) {
	quirks, _ := QuirksFor(OMPProductID)
	process := newScriptedProcess("omp-native")
	process.emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": MaxRPCFrameBytes})
	factory := &oneProcessFactory{process: process}
	driver, err := NewLaneDriver(LaneConfig{
		Quirks: quirks, Generation: 11, Processes: factory,
		MapPermission: familyPermission,
		Now:           func() time.Time { return time.Unix(200, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{ProductID: OMPProductID, LaneID: "lane-omp", Name: "native lane name", Cwd: "/work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--mode=rpc", "--name", "native lane name", "--tools", "read"}; !reflect.DeepEqual(factory.command.Args, want) {
		t.Fatalf("OMP args = %q, want %q", factory.command.Args, want)
	}
	turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{Prompt: "first", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := driver.Steer(ctx, turn, productruntime.TurnStartRequest{Prompt: "raw priority", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.NativeSessionID != "omp-native" || accepted.AcceptedAt != time.Unix(200, 0).UTC() || process.messageFor("steer") != "raw priority" {
		t.Fatalf("OMP steer = %+v, message %q", accepted, process.messageFor("steer"))
	}
	process.emit(map[string]any{"type": "agent_end", "willContinue": true, "messages": []any{}})
	process.emit(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"stopReason": "stop"}}})
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != productruntime.TurnCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}
	if got := process.commands(); !reflect.DeepEqual(got, []string{"get_state", "get_state", "prompt", "get_state", "steer", "get_last_assistant_text"}) {
		t.Fatalf("commands = %q", got)
	}
}

func TestLaneInterruptUsesCorrelatedAbortAndProductTerminalStrategy(t *testing.T) {
	for _, test := range []struct {
		productID     string
		nativeID      string
		wrongEvent    string
		terminalEvent string
		ready         bool
	}{
		{productID: PiProductID, nativeID: "pi-interrupt", wrongEvent: "agent_end", terminalEvent: "agent_settled"},
		{productID: OMPProductID, nativeID: "omp-interrupt", wrongEvent: "agent_settled", terminalEvent: "agent_end", ready: true},
	} {
		t.Run(test.productID, func(t *testing.T) {
			quirks, err := QuirksFor(test.productID)
			if err != nil {
				t.Fatal(err)
			}
			process := newScriptedProcess(test.nativeID)
			if test.ready {
				process.emit(map[string]any{
					"type": "ready", "protocolVersion": 1,
					"supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes,
				})
			}
			driver, err := NewLaneDriver(LaneConfig{
				Quirks: quirks, Generation: 12, Processes: &oneProcessFactory{process: process},
				MapPermission: familyPermission,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
				ProductID: test.productID, LaneID: "interrupt-" + test.productID, Name: "interrupt native",
				Cwd: "/work", PermissionMode: permissionmode.Default,
			})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{
				Prompt: "run until interrupted", PermissionMode: permissionmode.Default,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Interrupt(ctx, turn); err != nil {
				t.Fatal(err)
			}
			commands := process.commands()
			abortIndex := -1
			for index, command := range commands {
				if command == "abort" {
					abortIndex = index
				}
			}
			if abortIndex < 0 || abortIndex == 0 || commands[abortIndex-1] != "get_state" {
				t.Fatalf("interrupt commands = %q; want correlated state check then abort", commands)
			}

			process.emit(map[string]any{"type": test.wrongEvent, "messages": []any{map[string]any{"stopReason": "aborted"}}})
			wrongCtx, wrongCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			_, err = driver.WaitTurn(wrongCtx, turn)
			wrongCancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s accepted %s as its terminal event: %v", test.productID, test.wrongEvent, err)
			}
			process.emit(map[string]any{"type": test.terminalEvent, "messages": []any{map[string]any{"stopReason": "aborted"}}})
			terminal, err := driver.WaitTurn(ctx, turn)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Outcome != productruntime.TurnInterrupted || terminal.NativeStopReason != "aborted" || terminal.Result != "native answer" {
				t.Fatalf("interrupted terminal = %+v", terminal)
			}
		})
	}
}

func TestLaneInterruptWriteFailureRollsBackInterruptedOutcome(t *testing.T) {
	for _, productID := range []string{PiProductID, OMPProductID} {
		t.Run(productID, func(t *testing.T) {
			quirks, err := QuirksFor(productID)
			if err != nil {
				t.Fatal(err)
			}
			process := newScriptedProcess(productID + "-abort-failure")
			process.commandErrors = map[string]error{"abort": errors.New("native abort write rejected")}
			if productID == OMPProductID {
				process.emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes})
			}
			driver, err := NewLaneDriver(LaneConfig{
				Quirks: quirks, Generation: 13, Processes: &oneProcessFactory{process: process},
				MapPermission: familyPermission,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ref, err := driver.Open(ctx, productruntime.LaneOpenRequest{
				ProductID: productID, LaneID: "abort-failure-" + productID, Name: "abort failure",
				Cwd: "/work", PermissionMode: permissionmode.Default,
			})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := driver.StartTurn(ctx, ref, productruntime.TurnStartRequest{
				Prompt: "complete after rejected abort", PermissionMode: permissionmode.Default,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Interrupt(ctx, turn); !errors.Is(err, productruntime.ErrProtocol) {
				t.Fatalf("rejected abort returned %v", err)
			}
			process.emit(map[string]any{
				"type":     quirks.TerminalEvent,
				"messages": []any{map[string]any{"stopReason": "stop"}},
			})
			terminal, err := driver.WaitTurn(ctx, turn)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Outcome != productruntime.TurnCompleted || terminal.NativeStopReason != "stop" {
				t.Fatalf("abort error left an interrupted marker behind: %+v", terminal)
			}
		})
	}
}

func TestOMPHandshakeRejectsHostileFirstOrIncompatibleReadyFrame(t *testing.T) {
	quirks, _ := QuirksFor(OMPProductID)
	for _, frame := range []map[string]any{
		{"type": "response", "id": "foreign", "command": "get_state", "success": true},
		{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes / 2},
	} {
		process := newScriptedProcess("omp-native")
		process.emit(frame)
		client, err := newRPCClient(process, quirks)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = client.handshake(ctx)
		cancel()
		if !errors.Is(err, productruntime.ErrProtocol) && !errors.Is(err, productruntime.ErrIncompatible) {
			t.Fatalf("hostile frame %v returned %v", frame, err)
		}
	}
}

func TestLaneOpenFailureSurfacesSynchronousCleanupError(t *testing.T) {
	t.Run("handshake", func(t *testing.T) {
		quirks, _ := QuirksFor(OMPProductID)
		process := newScriptedProcess("omp-handshake")
		cleanupErr := errors.New("cleanup failed")
		process.cleanupErr = cleanupErr
		process.emit(map[string]any{"type": "response", "id": "foreign", "command": "get_state", "success": true})
		driver, err := NewLaneDriver(LaneConfig{
			Quirks: quirks, Generation: 1, Processes: &oneProcessFactory{process: process},
			MapPermission: familyPermission,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
			ProductID: OMPProductID, LaneID: "handshake-debt", Name: "handshake failure", Cwd: "/work", PermissionMode: permissionmode.Default,
		})
		if !errors.Is(err, productruntime.ErrProtocol) || !errors.Is(err, cleanupErr) || errors.Is(err, productruntime.ErrCleanupDebt) || !process.cleaned {
			t.Fatalf("handshake cleanup = %v, cleaned = %t", err, process.cleaned)
		}
	})

	t.Run("resume mismatch", func(t *testing.T) {
		quirks, _ := QuirksFor(PiProductID)
		process := newScriptedProcess("different-native")
		cleanupErr := errors.New("cleanup failed")
		process.cleanupErr = cleanupErr
		driver, err := NewLaneDriver(LaneConfig{
			Quirks: quirks, Generation: 1, Processes: &oneProcessFactory{process: process},
			MapPermission: familyPermission,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
			ProductID: PiProductID, LaneID: "resume-debt", ResumeNativeID: "expected-native",
			Cwd: "/work", PermissionMode: permissionmode.Default,
		})
		if !errors.Is(err, productruntime.ErrAmbiguousSession) || !errors.Is(err, cleanupErr) || errors.Is(err, productruntime.ErrCleanupDebt) || !process.cleaned {
			t.Fatalf("resume cleanup = %v, cleaned = %t", err, process.cleaned)
		}
	})

	t.Run("final native ownership collision", func(t *testing.T) {
		quirks, _ := QuirksFor(PiProductID)
		first := newScriptedProcess("shared-native")
		second := newScriptedProcess("shared-native")
		cleanupErr := errors.New("cleanup failed")
		second.cleanupErr = cleanupErr
		factory := &sequenceProcessFactory{processes: []*scriptedRPCProcess{first, second}}
		driver, err := NewLaneDriver(LaneConfig{
			Quirks: quirks, Generation: 1, Processes: factory,
			MapPermission: familyPermission,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
			ProductID: PiProductID, LaneID: "first-owner", Name: "first owner", Cwd: "/work", PermissionMode: permissionmode.Default,
		}); err != nil {
			t.Fatal(err)
		}
		_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
			ProductID: PiProductID, LaneID: "second-owner", Name: "second owner", Cwd: "/work", PermissionMode: permissionmode.Default,
		})
		if !errors.Is(err, productruntime.ErrAmbiguousSession) || !errors.Is(err, cleanupErr) || errors.Is(err, productruntime.ErrCleanupDebt) || !second.cleaned {
			t.Fatalf("collision cleanup = %v, cleaned = %t", err, second.cleaned)
		}
	})
}

func TestOMPRPCApprovalRequestFailsClosedWithoutWaitingForTerminal(t *testing.T) {
	quirks, _ := QuirksFor(OMPProductID)
	process := newScriptedProcess("omp-native")
	process.emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes})
	client, err := newRPCClient(process, quirks)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.handshake(ctx); err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{"type": "extension_ui_request", "id": "approval-1", "method": "confirm", "title": "allow tool?"})
	if _, err := client.waitTerminal(ctx); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unmediated approval request returned %v", err)
	}
}

func TestOMPRPCIgnoresOnlyProductDeclaredFireAndForgetUI(t *testing.T) {
	quirks, _ := QuirksFor(OMPProductID)
	for _, method := range []string{"cancel", "notify", "setStatus", "setWidget", "setTitle", "set_editor_text"} {
		t.Run(method, func(t *testing.T) {
			process := newScriptedProcess("omp-native")
			process.emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes})
			process.emit(map[string]any{"type": "extension_ui_request", "id": "ui-1", "method": method})
			client, err := newRPCClient(process, quirks)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			state, err := client.handshake(ctx)
			if err != nil || state.SessionID != "omp-native" {
				t.Fatalf("handshake after %s = %+v, %v", method, state, err)
			}
		})
	}

	process := newScriptedProcess("omp-native")
	process.emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes})
	client, err := newRPCClient(process, quirks)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.handshake(ctx); err != nil {
		t.Fatal(err)
	}
	process.emit(map[string]any{"type": "extension_ui_request", "id": "ui-unknown", "method": "future_method"})
	if _, err := client.waitTerminal(ctx); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("unknown UI method returned %v", err)
	}
}

func TestLaneOpenPassesProductNativeArgumentsAndName(t *testing.T) {
	for _, productID := range []string{PiProductID, OMPProductID} {
		t.Run(productID, func(t *testing.T) {
			quirks, _ := QuirksFor(productID)
			process := newScriptedProcess(productID + "-native")
			if productID == OMPProductID {
				process.emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1}, "maxFrameBytes": MaxRPCFrameBytes})
			}
			factory := &oneProcessFactory{process: process}
			driver, err := NewLaneDriver(LaneConfig{
				Quirks: quirks, Generation: 1, Processes: factory,
				MapPermission: familyPermission,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: productID, LaneID: "argument-passed", Name: "product native name", Cwd: "/work",
				PermissionMode: permissionmode.Default, Arguments: []string{"--model", "deepseek/deepseek-v4-flash"},
			})
			if err != nil {
				t.Fatal(err)
			}
			want := append(quirks.modeArguments(), "--model", "deepseek/deepseek-v4-flash", "--name", "product native name", "--tools", "read")
			if !reflect.DeepEqual(factory.command.Args, want) {
				t.Fatalf("native args = %q, want %q", factory.command.Args, want)
			}
		})
	}
}

func TestLaneOpenRejectsLifecycleOwnedNativeArgumentsBeforeStartingProcess(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	for _, argument := range []string{"--mode=rpc", "--session", "--name=other", "--extension=/tmp/x", "--approval-mode=yolo", "--tools", "--"} {
		t.Run(argument, func(t *testing.T) {
			factory := &oneProcessFactory{process: newScriptedProcess("must-not-start")}
			driver, err := NewLaneDriver(LaneConfig{Quirks: quirks, Generation: 1, Processes: factory, MapPermission: familyPermission})
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: PiProductID, LaneID: "argument-rejected", Name: "native name", Cwd: "/work",
				PermissionMode: permissionmode.Default, Arguments: []string{argument},
			})
			if !errors.Is(err, productruntime.ErrUnsupportedPolicy) || factory.starts != 0 {
				t.Fatalf("lane open = %v, process starts = %d", err, factory.starts)
			}
		})
	}
}

func TestRPCProductErrorIsReturnedVerbatim(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	process := newScriptedProcess("pi-error")
	process.responseErrors = map[string]string{"get_state": "native provider rejected sk-product-detail"}
	driver, err := NewLaneDriver(LaneConfig{
		Quirks: quirks, Generation: 1, Processes: &oneProcessFactory{process: process},
		MapPermission: familyPermission,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: PiProductID, LaneID: "native-error", Name: "native name", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if !errors.Is(err, productruntime.ErrNativeRejected) || !strings.Contains(err.Error(), "native provider rejected sk-product-detail") || strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("native product error = %v", err)
	}
}

func TestLanePromptRejectsEmptyOversizedAndNUL(t *testing.T) {
	for _, prompt := range []string{"", strings.Repeat("x", MaxPromptBytes+1), "bad\x00prompt"} {
		if _, err := lanePrompt(prompt); !errors.Is(err, productruntime.ErrProtocol) {
			t.Fatalf("invalid prompt returned %v", err)
		}
	}
}
