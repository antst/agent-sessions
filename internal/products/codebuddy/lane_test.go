package codebuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestLaneDispatchStreamSteerStopRespawnArchiveAndSecretSplit(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	deps := productruntime.HostDeps{
		Generation: 7, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"first": []byte("do work"), "steer": []byte("new direction"), "next": []byte("continue")}},
	}
	driver, err := NewLaneDriver(config, deps)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane-1", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reference.NativeSessionID != "" || reference.Generation != 7 {
		t.Fatalf("lane ref = %#v", reference)
	}
	if capabilities := driver.Capabilities(); !capabilities.Steer || !capabilities.DurableResume || !capabilities.DeferredSessionBinding {
		t.Fatalf("lane capabilities do not describe deferred binding: %#v", capabilities)
	}
	command := supervisor.commands[0]
	if argumentValue(command.Args, "--auth") != "password" || argumentValue(command.Args, "--session-id") != "" || len(command.SensitiveEnv) != 1 {
		t.Fatalf("owned server command = %#v", command)
	}
	for _, variable := range command.Env {
		if variable.Name == SessionIDEnv {
			t.Fatalf("fresh lane exported invented native session identity: %#v", command.Env)
		}
	}
	if encoded, err := json.Marshal(command); err == nil || strings.Contains(string(encoded), supervisor.native.password) {
		t.Fatalf("secret-bearing command serialized: %s, %v", encoded, err)
	}
	turn, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "first", PermissionMode: permissionmode.Default})
	if err != nil || turn.NativeSessionID != "native-job-session-1" || turn.NativeTurnID != "job-1" {
		t.Fatalf("start turn = %#v, %v", turn, err)
	}
	reference = turn.NativeSessionRef
	acceptance, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "steer", PermissionMode: permissionmode.Default})
	if err != nil || acceptance.NativeMessageID != "" {
		t.Fatalf("steer = %#v, %v", acceptance, err)
	}
	if err := driver.Interrupt(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "native result" {
		t.Fatalf("terminal = %#v, %v", terminal, err)
	}

	supervisor.native.mu.Lock()
	supervisor.native.replySaved = true
	supervisor.native.getCount = 0
	supervisor.native.mu.Unlock()
	next, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "next", PermissionMode: permissionmode.Default})
	if err != nil || next.NativeTurnID == "" || next.NativeTurnID == turn.NativeTurnID || next.NativeTurnID == "job-1" {
		t.Fatalf("respawn turn = %#v, %v", next, err)
	}
	if err := driver.Interrupt(context.Background(), turn); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("old turn aliased the respawned job: %v", err)
	}
	if _, err := driver.WaitTurn(context.Background(), next); err != nil {
		t.Fatalf("respawn wait: %v", err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatalf("idempotent archive: %v", err)
	}
	supervisor.native.mu.Lock()
	archived := supervisor.native.archived
	supervisor.native.mu.Unlock()
	if !archived {
		t.Fatal("native job was not archived")
	}
}

func TestLaneArchiveStopsServerAfterDeletingJob(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 3, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "archive-retry", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "turn", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	reference = turn.NativeSessionRef
	if _, err := driver.WaitTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatalf("archive: %v", err)
	}
	supervisor.native.mu.Lock()
	deleteCount := supervisor.native.deleteCount
	supervisor.native.mu.Unlock()
	if deleteCount != 1 {
		t.Fatalf("delete count = %d", deleteCount)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatalf("idempotent archive: %v", err)
	}
	supervisor.native.mu.Lock()
	deleteCount = supervisor.native.deleteCount
	supervisor.native.mu.Unlock()
	if deleteCount != 1 {
		t.Fatalf("confirmed job was deleted %d times", deleteCount)
	}
}

func TestLaneNativeIOOnOneLaneDoesNotBlockAnotherLane(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var nativeA *fakeNativeServer
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/api/v1/jobs" && native == nativeA {
			once.Do(func() { close(entered) })
			<-release
		}
		defaultNativeHandler(native, response, request)
	}
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 4, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"a": []byte("a"), "b": []byte("b")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	open := func(id string) productruntime.NativeSessionRef {
		ref, openErr := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: id, Cwd: "/work", PermissionMode: permissionmode.Default})
		if openErr != nil {
			t.Fatal(openErr)
		}
		return ref
	}
	refA := open("lane-a")
	nativeA = supervisor.native
	refB := open("lane-b")
	type startResult struct {
		turn productruntime.NativeTurnRef
		err  error
	}
	resultA := make(chan startResult, 1)
	go func() {
		turn, startErr := driver.StartTurn(context.Background(), refA, productruntime.TurnStartRequest{ReceiptID: "a", PermissionMode: permissionmode.Default})
		resultA <- startResult{turn: turn, err: startErr}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("lane-a dispatch did not enter native server")
	}
	resultB := make(chan startResult, 1)
	go func() {
		turn, startErr := driver.StartTurn(context.Background(), refB, productruntime.TurnStartRequest{ReceiptID: "b", PermissionMode: permissionmode.Default})
		resultB <- startResult{turn: turn, err: startErr}
	}()
	var startedB startResult
	select {
	case startedB = <-resultB:
		if startedB.err != nil {
			t.Fatal(startedB.err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("lane-b was blocked by lane-a native I/O")
	}
	close(release)
	startedA := <-resultA
	if startedA.err != nil {
		t.Fatal(startedA.err)
	}
	if _, err := driver.WaitTurn(context.Background(), startedA.turn); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.WaitTurn(context.Background(), startedB.turn); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), startedA.turn.NativeSessionRef); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), startedB.turn.NativeSessionRef); err != nil {
		t.Fatal(err)
	}
}

func TestLaneConcurrentWaitIsIdempotentAndCannotClobberNextRespawn(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 8, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"first": []byte("first"), "next": []byte("next")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "double-wait", Cwd: "/work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	first, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "first", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	reference = first.NativeSessionRef
	type waitResult struct {
		terminal productruntime.NativeTerminal
		err      error
	}
	results := make(chan waitResult, 2)
	for range 2 {
		go func() {
			terminal, waitErr := driver.WaitTurn(context.Background(), first)
			results <- waitResult{terminal: terminal, err: waitErr}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil || result.terminal.Result != "native result" {
			t.Fatalf("duplicate wait = %#v, %v", result.terminal, result.err)
		}
	}
	supervisor.native.mu.Lock()
	supervisor.native.replySaved = true
	supervisor.native.getCount = 0
	supervisor.native.mu.Unlock()
	next, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "next", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	if terminal, err := driver.WaitTurn(context.Background(), first); err != nil || terminal.Result != "native result" {
		t.Fatalf("old terminal was not idempotent: %#v, %v", terminal, err)
	}
	runtime := driver.lanes[reference.LaneID]
	runtime.stateMu.Lock()
	jobUpdatedAt := runtime.job.UpdatedAt
	_, nextStillActive := runtime.active[next.NativeTurnID]
	runtime.stateMu.Unlock()
	if jobUpdatedAt != 4 || !nextStillActive {
		t.Fatalf("old wait clobbered respawn: updatedAt=%d active=%v", jobUpdatedAt, nextStillActive)
	}
	if _, err := driver.WaitTurn(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
}

func TestLaneSavedReplyRespawnsExactlyAndReturnsAcceptance(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	deps := productruntime.HostDeps{Generation: 2, OwnedProcesses: supervisor, Receipts: memoryReceiptReader{values: map[string][]byte{"first": []byte("first"), "busy": []byte("busy")}}}
	driver, _ := NewLaneDriver(config, deps)
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane", Cwd: "/work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "first", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	reference = turn.NativeSessionRef
	supervisor.native.mu.Lock()
	supervisor.native.replySaved = true
	supervisor.native.mu.Unlock()
	acceptance, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "busy", PermissionMode: permissionmode.Default})
	if err != nil || acceptance.NativeSessionID != turn.NativeSessionID || acceptance.NativeMessageID != "" {
		t.Fatalf("saved steer acceptance = %#v, %v", acceptance, err)
	}
	supervisor.native.mu.Lock()
	updatedAt := supervisor.native.job.UpdatedAt
	supervisor.native.mu.Unlock()
	if updatedAt != 4 {
		t.Fatalf("saved reply was not consumed by exact respawn: updatedAt=%d", updatedAt)
	}
	if _, err := driver.WaitTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
}

func TestLaneSavedReplyRespawnFailureIsAmbiguousAndNeverUnsupported(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/api/v1/jobs/job-1/respawn" {
			http.Error(response, "injected respawn failure", http.StatusServiceUnavailable)
			return
		}
		defaultNativeHandler(native, response, request)
	}
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	deps := productruntime.HostDeps{Generation: 3, OwnedProcesses: supervisor, Receipts: memoryReceiptReader{values: map[string][]byte{"first": []byte("first"), "busy": []byte("busy")}}}
	driver, _ := NewLaneDriver(config, deps)
	unbound, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "saved-ambiguous", Cwd: "/work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), unbound, productruntime.TurnStartRequest{ReceiptID: "first", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.native.mu.Lock()
	supervisor.native.replySaved = true
	supervisor.native.mu.Unlock()
	if _, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "busy", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrAmbiguousSession) || errors.Is(err, productruntime.ErrUnsupportedSteer) {
		t.Fatalf("post-save respawn failure category = %v", err)
	}
	supervisor.native.mu.Lock()
	requests := append([]string(nil), supervisor.native.requests...)
	supervisor.native.mu.Unlock()
	var replies, respawns int
	for _, request := range requests {
		if request == "POST /api/v1/jobs/job-1/reply" {
			replies++
		}
		if request == "POST /api/v1/jobs/job-1/respawn" {
			respawns++
		}
	}
	if replies != 1 || respawns != 1 {
		t.Fatalf("saved input was retried: replies=%d respawns=%d", replies, respawns)
	}
	runtime := driver.lanes[turn.LaneID]
	runtime.stateMu.Lock()
	runtime.active = make(map[string]turnObservation)
	runtime.stateMu.Unlock()
	if err := driver.Archive(context.Background(), turn.NativeSessionRef); err != nil {
		t.Fatal(err)
	}
}

func TestLaneTerminalWaitAndSavedSteerSerializeWithoutOrphanRespawn(t *testing.T) {
	t.Run("terminal-wins-before-native-write", func(t *testing.T) {
		supervisor := newFakeOwnedSupervisor()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodGet && request.URL.Path == "/api/v1/jobs/job-1" {
				once.Do(func() { close(entered) })
				<-release
				native.mu.Lock()
				job := native.job
				native.mu.Unlock()
				job.State, job.Detail, job.UpdatedAt, job.FirstTerminalAt = "done", "old terminal", 3, 3
				writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"job": job}})
				return
			}
			defaultNativeHandler(native, response, request)
		}
		driver, _ := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
			Generation: 17, OwnedProcesses: supervisor,
			Receipts: memoryReceiptReader{values: map[string][]byte{"first": []byte("first"), "busy": []byte("busy")}},
		})
		unbound, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "wait-wins", Cwd: "/work", PermissionMode: permissionmode.Default})
		if err != nil {
			t.Fatal(err)
		}
		turn, err := driver.StartTurn(context.Background(), unbound, productruntime.TurnStartRequest{ReceiptID: "first", PermissionMode: permissionmode.Default})
		if err != nil {
			t.Fatal(err)
		}
		supervisor.native.mu.Lock()
		supervisor.native.replySaved = true
		supervisor.native.mu.Unlock()
		waitResult := make(chan error, 1)
		go func() {
			terminal, waitErr := driver.WaitTurn(context.Background(), turn)
			if waitErr == nil && terminal.Result != "old terminal" {
				waitErr = fmt.Errorf("terminal result = %q", terminal.Result)
			}
			waitResult <- waitErr
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("wait did not reach terminal poll")
		}
		steerResult := make(chan error, 1)
		go func() {
			_, steerErr := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "busy", PermissionMode: permissionmode.Default})
			steerResult <- steerErr
		}()
		close(release)
		if err := <-waitResult; err != nil {
			t.Fatal(err)
		}
		if err := <-steerResult; !errors.Is(err, productruntime.ErrStale) {
			t.Fatalf("steer wrote after terminal commit: %v", err)
		}
		supervisor.native.mu.Lock()
		requests := append([]string(nil), supervisor.native.requests...)
		supervisor.native.mu.Unlock()
		for _, request := range requests {
			if request == "POST /api/v1/jobs/job-1/reply" || request == "POST /api/v1/jobs/job-1/respawn" {
				t.Fatalf("terminal winner left a native write: %#v", requests)
			}
		}
		if err := driver.Archive(context.Background(), turn.NativeSessionRef); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("saved-respawn-wins-before-terminal-proof", func(t *testing.T) {
		supervisor := newFakeOwnedSupervisor()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/api/v1/jobs/job-1/reply" {
				once.Do(func() { close(entered) })
				<-release
			}
			defaultNativeHandler(native, response, request)
		}
		driver, _ := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
			Generation: 18, OwnedProcesses: supervisor,
			Receipts: memoryReceiptReader{values: map[string][]byte{"first": []byte("first"), "busy": []byte("busy")}},
		})
		unbound, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "steer-wins", Cwd: "/work", PermissionMode: permissionmode.Default})
		if err != nil {
			t.Fatal(err)
		}
		turn, err := driver.StartTurn(context.Background(), unbound, productruntime.TurnStartRequest{ReceiptID: "first", PermissionMode: permissionmode.Default})
		if err != nil {
			t.Fatal(err)
		}
		supervisor.native.mu.Lock()
		supervisor.native.replySaved = true
		supervisor.native.mu.Unlock()
		steerResult := make(chan error, 1)
		go func() {
			_, steerErr := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "busy", PermissionMode: permissionmode.Default})
			steerResult <- steerErr
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("steer did not reach saved reply")
		}
		waitResult := make(chan error, 1)
		go func() {
			_, waitErr := driver.WaitTurn(context.Background(), turn)
			waitResult <- waitErr
		}()
		close(release)
		if err := <-steerResult; err != nil {
			t.Fatalf("saved respawn winner was not accepted: %v", err)
		}
		if err := <-waitResult; err != nil {
			t.Fatal(err)
		}
		runtime := driver.lanes[turn.LaneID]
		runtime.stateMu.Lock()
		active := len(runtime.active)
		runtime.stateMu.Unlock()
		if active != 0 {
			t.Fatalf("respawn winner left %d active observations", active)
		}
		if err := driver.Archive(context.Background(), turn.NativeSessionRef); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLaneExactResumeAndRecoveryRejectSubstitution(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	config.Recovery = RecoveryRequestSourceFunc(func(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error) {
		return productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane-r", ResumeNativeID: "native-r", Cwd: "/work", PermissionMode: permissionmode.Default}, nil
	})
	deps := productruntime.HostDeps{Generation: 9, OwnedProcesses: supervisor, Receipts: memoryReceiptReader{values: map[string][]byte{"x": []byte("x")}}}
	driver, _ := NewLaneDriver(config, deps)
	reference, err := driver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: ProductID, LaneID: "lane-r", PriorNativeSessionID: "native-r", PriorGeneration: 8,
	})
	if err != nil || reference.NativeSessionID != "native-r" || supervisor.native.resumeID != "native-r" {
		t.Fatalf("recovery = %#v, %v, resumed=%q", reference, err, supervisor.native.resumeID)
	}
	command := supervisor.commands[0]
	if argumentValue(command.Args, "--session-id") != "native-r" {
		t.Fatalf("resume omitted exact product-owned session: %#v", command.Args)
	}
	foundSessionEnv := false
	for _, variable := range command.Env {
		foundSessionEnv = foundSessionEnv || variable.Name == SessionIDEnv && variable.Value == "native-r"
	}
	if !foundSessionEnv {
		t.Fatalf("resume omitted exact product-owned session environment: %#v", command.Env)
	}
	if _, err := driver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: ProductID, LaneID: "lane-r", PriorNativeSessionID: "other-native", PriorGeneration: 8,
	}); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("substitution error = %v", err)
	}
	_ = driver.Archive(context.Background(), reference)
}

func TestLaneInMemoryRecoveryReconcilesExactLiveJob(t *testing.T) {
	for _, test := range []struct {
		name     string
		missing  bool
		session  string
		cwd      string
		category error
	}{
		{name: "missing", missing: true, category: productruntime.ErrStale},
		{name: "rotated-session", session: "attacker-session", cwd: "/work", category: productruntime.ErrProtocol},
		{name: "rotated-cwd", session: "native-job-session-1", cwd: "/attacker", category: productruntime.ErrProtocol},
	} {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newFakeOwnedSupervisor()
			driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
				Generation: 16, OwnedProcesses: supervisor,
				Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			unbound, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: ProductID, LaneID: "recover-live", Cwd: "/work", PermissionMode: permissionmode.Default,
			})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := driver.StartTurn(context.Background(), unbound, productruntime.TurnStartRequest{ReceiptID: "turn", PermissionMode: permissionmode.Default})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := driver.WaitTurn(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/api/v1/jobs/job-1" {
					if test.missing {
						http.NotFound(response, request)
						return
					}
					native.mu.Lock()
					job := native.job
					native.mu.Unlock()
					job.SessionID, job.Cwd = test.session, test.cwd
					writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"job": job}})
					return
				}
				defaultNativeHandler(native, response, request)
			}
			_, err = driver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
				ProductID: ProductID, LaneID: turn.LaneID, PriorNativeSessionID: turn.NativeSessionID, PriorGeneration: 15,
			})
			if !errors.Is(err, productruntime.ErrUnsupportedRecovery) || !errors.Is(err, test.category) {
				t.Fatalf("healthy but substituted recovery error = %v", err)
			}
			if err := driver.Archive(context.Background(), turn.NativeSessionRef); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLaneRejectsInvalidFirstDispatchIdentityWithoutClaimingABinding(t *testing.T) {
	for _, test := range []struct {
		name      string
		sessionID string
		cwd       string
		marker    string
	}{
		{name: "missing-product-session", sessionID: "", cwd: "/work", marker: "exact"},
		{name: "whitespace-product-session", sessionID: " product-session ", cwd: "/work", marker: "exact"},
		{name: "wrong-cwd", sessionID: "product-session", cwd: "/other", marker: "exact"},
		{name: "missing-marker", sessionID: "product-session", cwd: "/work", marker: "missing"},
		{name: "wrong-marker", sessionID: "product-session", cwd: "/work", marker: "wrong"},
	} {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newFakeOwnedSupervisor()
			supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && request.URL.Path == "/api/v1/jobs" {
					var body DispatchJobRequest
					_ = json.NewDecoder(request.Body).Decode(&body)
					responseMarker := body.Name
					if test.marker == "missing" {
						responseMarker = ""
					} else if test.marker == "wrong" {
						responseMarker = "attacker-marker"
					}
					writeJSON(response, http.StatusOK, map[string]any{"data": AgentJob{
						ID: "job-1", SessionID: test.sessionID, State: "working", Name: responseMarker,
						Cwd: test.cwd, StartedAt: 1, UpdatedAt: 2,
					}})
					return
				}
				defaultNativeHandler(native, response, request)
			}
			driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
				Generation: 5, OwnedProcesses: supervisor,
				Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: ProductID, LaneID: "lane-bound", Cwd: "/work/./", PermissionMode: permissionmode.Default,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "turn", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrAmbiguousSession) {
				t.Fatalf("unprovable dispatch error = %v", err)
			}
			runtime := driver.lanes[reference.LaneID]
			runtime.stateMu.Lock()
			if runtime.ref.NativeSessionID != "" || runtime.pending == nil {
				t.Fatalf("invalid response falsely bound the lane: ref=%#v pending=%#v", runtime.ref, runtime.pending)
			}
			// The hostile server deliberately supplies no exact reconciliation
			// candidate. Clear only the test fixture's ambiguity record so the
			// owned process can be shut down without claiming native cleanup.
			runtime.pending = nil
			runtime.stateMu.Unlock()
			if err := driver.Archive(context.Background(), reference); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLaneOpenIsUnboundAndDoesNotClaimLiveSession(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	var liveCalls, jobCalls int
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/sessions/live" {
			liveCalls++
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"sessionId": "wrong-live-session", "writerOccupied": false}})
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/v1/jobs") {
			jobCalls++
		}
		defaultNativeHandler(native, response, request)
	}
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 11, OwnedProcesses: supervisor, Receipts: memoryReceiptReader{values: map[string][]byte{"x": []byte("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "expected-live", Cwd: "/work", PermissionMode: permissionmode.Default})
	if err != nil || reference.NativeSessionID != "" {
		t.Fatalf("fresh open falsely claimed native authority: %#v, %v", reference, err)
	}
	if liveCalls != 0 || jobCalls != 0 {
		t.Fatalf("fresh open created or consulted native session state: live=%d jobs=%d", liveCalls, jobCalls)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
}

func TestLaneDirectFirstDispatchRejectsPreexistingJobIdentity(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	preexisting := AgentJob{
		ID: "job-1", SessionID: "old-product-session", State: "working", Name: "old-job",
		Cwd: "/work", StartedAt: 1, UpdatedAt: 1,
	}
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/jobs":
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"jobs": []AgentJob{preexisting}}})
			return
		case "POST /api/v1/jobs":
			var body DispatchJobRequest
			_ = json.NewDecoder(request.Body).Decode(&body)
			writeJSON(response, http.StatusOK, map[string]any{"data": AgentJob{
				ID: preexisting.ID, SessionID: "new-product-session", State: "working", Name: body.Name,
				Cwd: body.Cwd, StartedAt: 2, UpdatedAt: 2,
			}})
			return
		}
		defaultNativeHandler(native, response, request)
	}
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 14, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "reused-job", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "turn", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("pre-existing native job ID was accepted: %v", err)
	}
	runtime := driver.lanes[reference.LaneID]
	runtime.stateMu.Lock()
	if runtime.ref.NativeSessionID != "" || runtime.job != nil || runtime.pending == nil {
		t.Fatalf("pre-existing identity falsely bound: ref=%#v job=%#v pending=%#v", runtime.ref, runtime.job, runtime.pending)
	}
	// Test-only teardown; no new exact native job was proven.
	runtime.pending = nil
	runtime.stateMu.Unlock()
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
}

func TestLaneFirstDispatchReconcilesOneExactPossibleWriteWithoutReplay(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	var mu sync.Mutex
	var listCalls, postCalls int
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/jobs":
			mu.Lock()
			listCalls++
			var jobs []AgentJob
			native.mu.Lock()
			if native.job.ID != "" {
				jobs = append(jobs, native.job)
			}
			native.mu.Unlock()
			mu.Unlock()
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"jobs": jobs}})
			return
		case "POST /api/v1/jobs":
			var body DispatchJobRequest
			_ = json.NewDecoder(request.Body).Decode(&body)
			job := AgentJob{
				ID: "job-1", SessionID: "product-generated-session", State: "working", Name: body.Name,
				Cwd: body.Cwd, StartedAt: 1, UpdatedAt: 2,
			}
			native.mu.Lock()
			native.job = job
			native.mu.Unlock()
			mu.Lock()
			postCalls++
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte("{\"data\":"))
			return
		}
		defaultNativeHandler(native, response, request)
	}
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 12, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	unbound, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "reconciled", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), unbound, productruntime.TurnStartRequest{ReceiptID: "turn", PermissionMode: permissionmode.Default})
	if err != nil || turn.NativeSessionID != "product-generated-session" || turn.NativeTurnID != "job-1" {
		t.Fatalf("reconciled turn = %#v, %v", turn, err)
	}
	mu.Lock()
	gotLists, gotPosts := listCalls, postCalls
	mu.Unlock()
	if gotLists != 2 || gotPosts != 1 {
		t.Fatalf("reconciliation calls: lists=%d posts=%d", gotLists, gotPosts)
	}
	if _, err := driver.WaitTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), turn.NativeSessionRef); err != nil {
		t.Fatal(err)
	}
}

func TestLaneAmbiguousFirstDispatchNeverReplaysOrAdoptsArbitraryJob(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	var mu sync.Mutex
	var postCalls int
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/jobs":
			// Neither an unrelated job nor a missing job is authority to bind the
			// lane. The exact receipt marker must identify one new candidate.
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"jobs": []AgentJob{{
				ID: "unrelated", SessionID: "unrelated-session", State: "working", Name: "somebody-else",
				Cwd: "/work", StartedAt: 1, UpdatedAt: 1,
			}}}})
			return
		case "POST /api/v1/jobs":
			mu.Lock()
			postCalls++
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte("{\"data\":"))
			return
		}
		defaultNativeHandler(native, response, request)
	}
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 13, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work"), "other": []byte("other")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "ambiguous", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := func(receipt string) error {
		_, startErr := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: receipt, PermissionMode: permissionmode.Default})
		return startErr
	}
	if err := start("turn"); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("possible write was not quarantined: %v", err)
	}
	if err := start("turn"); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("same receipt retry escaped ambiguity: %v", err)
	}
	if err := start("other"); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("different receipt crossed ambiguous dispatch: %v", err)
	}
	mu.Lock()
	gotPosts := postCalls
	mu.Unlock()
	if gotPosts != 1 {
		t.Fatalf("ambiguous dispatch was replayed %d times", gotPosts)
	}
	if err := driver.Archive(context.Background(), reference); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("ambiguous native cleanup falsely succeeded: %v", err)
	}
	runtime := driver.lanes[reference.LaneID]
	runtime.stateMu.Lock()
	if runtime.ref.NativeSessionID != "" || runtime.job != nil {
		t.Fatalf("unrelated job was adopted: ref=%#v job=%#v", runtime.ref, runtime.job)
	}
	// Test-only teardown: discarding this ambiguity record does not assert that
	// the native write did or did not happen; it only permits owned-process exit.
	runtime.pending = nil
	runtime.stateMu.Unlock()
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
}

func TestLaneRejectsSubstitutedJobWhileWaiting(t *testing.T) {
	for _, test := range []struct {
		name      string
		sessionID string
		cwd       string
	}{
		{name: "session", sessionID: "attacker-session", cwd: "/work"},
		{name: "cwd", sessionID: "native-job-session-1", cwd: "/attacker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newFakeOwnedSupervisor()
			supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/api/v1/jobs/job-1" {
					writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"job": AgentJob{
						ID: "job-1", SessionID: test.sessionID, State: "done", Detail: "attacker result", Cwd: test.cwd, StartedAt: 1, UpdatedAt: 3,
					}}})
					return
				}
				defaultNativeHandler(native, response, request)
			}
			driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
				Generation: 6, OwnedProcesses: supervisor,
				Receipts: memoryReceiptReader{values: map[string][]byte{"turn": []byte("work")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "wait-bound", Cwd: "/work", PermissionMode: permissionmode.Default})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := driver.StartTurn(context.Background(), reference, productruntime.TurnStartRequest{ReceiptID: "turn", PermissionMode: permissionmode.Default})
			if err != nil {
				t.Fatal(err)
			}
			reference = turn.NativeSessionRef
			if terminal, err := driver.WaitTurn(context.Background(), turn); !errors.Is(err, productruntime.ErrProtocol) || terminal.Result != "" {
				t.Fatalf("substituted wait result = %#v, %v", terminal, err)
			}
			// The hostile response deliberately leaves the turn active; clear only the
			// test fixture's local observation so exact owned cleanup can be exercised.
			runtime := driver.lanes[reference.LaneID]
			runtime.stateMu.Lock()
			runtime.active = make(map[string]turnObservation)
			runtime.stateMu.Unlock()
			if err := driver.Archive(context.Background(), reference); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLaneDurableResumeRequiresExactRecoverySource(t *testing.T) {
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	config.Recovery = nil
	if _, err := NewLaneDriver(config, productruntime.HostDeps{
		Generation: 1, OwnedProcesses: newFakeOwnedSupervisor(), Receipts: memoryReceiptReader{values: map[string][]byte{"x": []byte("x")}},
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("durable resume was advertised without recovery truth: %v", err)
	}
}

func TestLaneRecoveryRejectsResumeCwdSubstitution(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/api/v1/jobs/resume" {
			writeJSON(response, http.StatusOK, map[string]any{"data": AgentJob{
				ID: "job-1", SessionID: "native-r", State: "done", Cwd: "/attacker", StartedAt: 1, UpdatedAt: 1,
			}})
			return
		}
		defaultNativeHandler(native, response, request)
	}
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	config.Recovery = RecoveryRequestSourceFunc(func(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error) {
		return productruntime.LaneOpenRequest{ProductID: ProductID, LaneID: "lane-r", ResumeNativeID: "native-r", Cwd: "/work", PermissionMode: permissionmode.Default}, nil
	})
	driver, err := NewLaneDriver(config, productruntime.HostDeps{
		Generation: 10, OwnedProcesses: supervisor, Receipts: memoryReceiptReader{values: map[string][]byte{"x": []byte("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: ProductID, LaneID: "lane-r", PriorNativeSessionID: "native-r", PriorGeneration: 9,
	}); !errors.Is(err, productruntime.ErrUnsupportedRecovery) || !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("resume cwd substitution error = %v", err)
	}
}

func TestPermissionMapperNeverWidensImplicitly(t *testing.T) {
	permission, err := MapPermission(permissionmode.Default)
	if err != nil || permission.Mode != "default" || len(permission.Env) != 0 {
		t.Fatalf("default mapping = %#v, %v", permission, err)
	}
	if _, err := MapPermission(permissionmode.BypassPermissions); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("implicit bypass error = %v", err)
	}
	permission, err = MapLanePermission(permissionmode.BypassPermissions, true)
	if err != nil || permission.Mode != "bypassPermissions" || len(permission.Env) != 1 || permission.Env[0].Name != SandboxBypassEnv {
		t.Fatalf("explicit sandbox bypass = %#v, %v", permission, err)
	}
}
