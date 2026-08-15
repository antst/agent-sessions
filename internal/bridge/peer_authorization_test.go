package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSupervisorAuthorizationRequiresExplicitPeerCapability(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	supervisor := nativeSupervisor{paths: paths, retired: map[string]bool{}}
	ordinaryID := "00000000-0000-0000-0000-000000000a01"
	if supervisor.authorizedPeerThread(ordinaryID) {
		t.Fatal("ordinary App Server thread minted peer authority")
	}

	interactiveID := "00000000-0000-0000-0000-000000000a02"
	writeTestAttachedInteractiveOwner(t, paths, interactiveID)
	if !supervisor.authorizedPeerThread(interactiveID) {
		t.Fatal("exact live codex-peer owner was rejected")
	}
	owner := readInteractiveOwner(paths, interactiveID)
	owner.OwnerProcStart += "-wrong"
	if err := writeJSONAtomic(interactiveOwnerPath(paths, interactiveID), owner); err != nil {
		t.Fatal(err)
	}
	if supervisor.authorizedPeerThread(interactiveID) {
		t.Fatal("mismatched process identity authorized an interactive peer")
	}

	preparedID := "00000000-0000-0000-0000-000000000a12"
	prepared := interactiveOwnerRecord{
		ThreadID: preparedID, RequestID: "prepared", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true,
	}
	if err := writeInteractiveOwnerRecord(paths, prepared); err != nil {
		t.Fatal(err)
	}
	if !supervisor.authorizedPeerThread(preparedID) {
		t.Fatal("exact fresh prepared owner was not publishable before its first turn")
	}
	if authorizedPeerThreadNative(paths, preparedID) {
		t.Fatal("prepared owner received MCP authority before SessionStart attachment")
	}
	if _, err := supervisor.handleControl(map[string]any{
		"action": "register", "sessionId": preparedID, "cwd": root,
	}); err == nil || !strings.Contains(err.Error(), "exact launch transaction") {
		t.Fatalf("generic prepared register error = %v", err)
	}
	prepared.Prepared = false
	if err := writeInteractiveOwnerRecord(paths, prepared); err != nil {
		t.Fatal(err)
	}
	if supervisor.authorizedPeerThread(preparedID) {
		t.Fatal("ordinary pending resume owner was published before attachment")
	}
	prepared.Prepared = true
	if err := writeInteractiveOwnerRecord(paths, prepared); err != nil {
		t.Fatal(err)
	}
	if supervisor.skipLoadedThreadReconciliation(preparedID, nil) {
		t.Fatal("reconciliation removed an exact live fresh peer before its first turn")
	}

	laneID := "00000000-0000-0000-0000-000000000a03"
	writeTestActiveCodexLane(t, paths, laneID)
	if !supervisor.authorizedPeerThread(laneID) {
		t.Fatal("active Codex lane was rejected")
	}
	state, err := readLaneStateFile(paths, laneID)
	if err != nil {
		t.Fatal(err)
	}
	state.Status = "archived"
	if err := writeJSONAtomic(laneStatePath(paths, laneID), state); err != nil {
		t.Fatal(err)
	}
	if supervisor.authorizedPeerThread(laneID) {
		t.Fatal("archived Codex lane retained peer authority")
	}
}

func TestUnauthorizedSupervisorOperationsFailBeforeMutation(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		appServerSock: filepath.Join(root, "missing-app-server.sock"),
	}
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
		releasing: map[string]int64{},
	}
	threadID := "00000000-0000-0000-0000-000000000a04"
	if _, err := supervisor.handleControl(map[string]any{
		"action": "register", "sessionId": threadID, "cwd": root,
	}); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("unauthorized register error = %v", err)
	}
	if _, err := supervisor.subscribeThread(threadID); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("unauthorized subscribe error = %v", err)
	}
	if _, err := supervisor.queueWake(threadID, map[string]any{
		"id": "unauthorized-wake", "message": "do not deliver",
	}); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("unauthorized wake error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID))); !os.IsNotExist(err) {
		t.Fatalf("unauthorized operations created session state: %v", err)
	}
	if record := readWakeRecord(paths, threadID, "unauthorized-wake"); record != nil {
		t.Fatalf("unauthorized wake created a ledger entry: %+v", record)
	}
}

func TestRegisterPreparedLaunchPublishesExactTransaction(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a13"
	resumed := make(chan struct{}, 1)
	resumeEntered := make(chan struct{})
	resumeRelease := make(chan struct{})
	var resumeOnce sync.Once
	var resumeParams map[string]any
	var settingsParams map[string]any
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/resume":
			resumeParams, _ = request["params"].(map[string]any)
			resumeOnce.Do(func() { close(resumeEntered) })
			<-resumeRelease
			select {
			case resumed <- struct{}{}:
			default:
			}
			return map[string]any{
				"thread": map[string]any{"id": threadID, "cwd": root}, "approvalPolicy": "on-request",
			}, nil
		case "thread/settings/update":
			settingsParams, _ = request["params"].(map[string]any)
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		appServerSock: appSocket,
	}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "prepared-register", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true, DeleteOnAbort: true, Cwd: root,
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	shimSocket := filepath.Join(root, "shim.sock")
	listener, err := net.Listen("unix", shimSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = bufio.NewReader(connection).ReadBytes('\n')
			_ = connection.Close()
		}
	}()
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "state.json")
	if err := writeJSONAtomic(statePath, map[string]any{
		"sessionId": threadID, "pid": os.Getpid(), "socketPath": shimSocket,
		"name": "prepared", "permissionMode": "bypassPermissions", "status": "idle",
	}); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
		releasing: map[string]int64{threadID: time.Now().Add(5 * time.Second).UnixMilli()},
	}
	request := map[string]any{
		"sessionId": threadID, "requestId": record.RequestID,
		"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
		"cwd": root, "name": "prepared", "permissionMode": "default", "status": "idle",
		"approvalPolicy": "never", "sandbox": "danger-full-access",
	}
	type registerResult struct {
		result map[string]any
		err    error
	}
	registerDone := make(chan registerResult, 1)
	go func() {
		result, err := supervisor.registerPreparedLaunch(request)
		registerDone <- registerResult{result: result, err: err}
	}()
	<-resumeEntered
	archiveDone := make(chan struct{})
	go func() {
		params, _ := json.Marshal(map[string]any{"threadId": threadID})
		supervisor.handleNotification(rpcNotification{Method: "thread/archived", Params: params})
		close(archiveDone)
	}()
	select {
	case <-archiveDone:
	case <-time.After(time.Second):
		close(resumeRelease)
		<-registerDone
		t.Fatal("release notification blocked behind prepared registration")
	}
	close(resumeRelease)
	registered := <-registerDone
	result, err := registered.result, registered.err
	if err != nil {
		t.Fatalf("prepared register = result=%#v err=%v", result, err)
	}
	if !supervisor.interactiveReleasePending(threadID) || supervisor.isRetired(threadID) {
		t.Fatalf("prepared replacement lost release grace: releasing=%v retired=%v", supervisor.interactiveReleasePending(threadID), supervisor.isRetired(threadID))
	}
	select {
	case <-resumed:
	default:
		t.Fatalf("prepared register did not resume exact thread: %#v", result)
	}
	state, _ := result["state"].(map[string]any)
	if stringValue(state["sessionId"]) != threadID || stringValue(state["permissionMode"]) != "bypassPermissions" ||
		stringValue(result["approvalPolicy"]) != "never" ||
		len(supervisor.shims) != 1 || !supervisor.subscribed[threadID] {
		t.Fatalf("prepared register state=%#v shims=%#v subscribed=%#v", state, supervisor.shims, supervisor.subscribed)
	}
	if _, present := resumeParams["approvalPolicy"]; present {
		t.Fatalf("prepared resume mixed policy mutation with thread loading: %#v", resumeParams)
	}
	if _, present := resumeParams["sandbox"]; present {
		t.Fatalf("prepared resume mixed sandbox mutation with thread loading: %#v", resumeParams)
	}
	if stringValue(resumeParams["cwd"]) != root {
		t.Fatalf("prepared resume did not apply the requested cwd: %#v", resumeParams)
	}
	sandboxPolicy, _ := settingsParams["sandboxPolicy"].(map[string]any)
	if stringValue(settingsParams["approvalPolicy"]) != "never" || stringValue(sandboxPolicy["type"]) != "dangerFullAccess" {
		t.Fatalf("prepared resume did not persist effective yolo settings: %#v", settingsParams)
	}
	if committed := readInteractiveOwner(paths, threadID); committed == nil || committed.DeleteOnAbort || !committed.ParkOnAbort {
		t.Fatalf("prepared register did not commit durability: %#v", committed)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestRegisterPreparedResumeAllowsExactLoadedTakeover(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a48"
	resumeReached := false
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{threadID}}, nil
		case "thread/resume":
			resumeReached = true
			return nil, errors.New("resume takeover reached")
		default:
			return nil, fmt.Errorf("unexpected loaded-takeover method %s", stringValue(request["method"]))
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: appSocket,
	}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "loaded-takeover", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true, ResumeLoaded: true, Cwd: root,
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{}, releasing: map[string]int64{},
	}
	_, err := supervisor.registerPreparedLaunch(map[string]any{
		"sessionId": threadID, "requestId": record.RequestID, "ownerPid": record.OwnerPID,
		"ownerProcStart": record.OwnerProcStart, "cwd": root,
	})
	if err == nil || !strings.Contains(err.Error(), "resume takeover reached") || !resumeReached {
		t.Fatalf("loaded prepared registration = reached=%v err=%v", resumeReached, err)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestAttachedReplacementKeepsReleaseGuardAgainstDelayedArchive(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a36"
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize", "thread/subscribe":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		appServerSock: appSocket, supervisorSock: filepath.Join(root, "supervisor.sock"),
	}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "replacement-owner", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Prepared: true, ParkOnAbort: true,
		Cwd: root, Name: "replacement", UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	shimSocket := filepath.Join(root, "shim.sock")
	listener, err := net.Listen("unix", shimSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = bufio.NewReader(connection).ReadBytes('\n')
			_ = connection.Close()
		}
	}()
	state := map[string]any{
		"sessionId": threadID, "pid": os.Getpid(), "socketPath": shimSocket,
		"name": "replacement", "permissionMode": "default", "status": "idle",
	}
	if err := writeJSONAtomic(filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "state.json"), state); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{threadID: state},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
		releasing: map[string]int64{threadID: time.Now().Add(5 * time.Second).UnixMilli()},
	}
	if _, err := supervisor.handleControl(map[string]any{
		"action": "register", "sessionId": threadID, "cwd": root, "name": "replacement", "status": "idle",
	}); err != nil {
		t.Fatal(err)
	}
	if !supervisor.interactiveReleasePending(threadID) {
		t.Fatal("attached replacement canceled the delayed-archive guard")
	}
	params, _ := json.Marshal(map[string]any{"threadId": threadID})
	supervisor.handleNotification(rpcNotification{Method: "thread/archived", Params: params})
	if supervisor.isRetired(threadID) || len(supervisor.shims) != 1 || readInteractiveOwner(paths, threadID) == nil {
		t.Fatalf("delayed archive tore down replacement: retired=%v shims=%#v owner=%#v", supervisor.isRetired(threadID), supervisor.shims, readInteractiveOwner(paths, threadID))
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestPreparedRegisterAndAbortSerializeWithoutResidue(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a14"
	deleted := make(chan struct{}, 1)
	parked := make(chan string, 2)
	_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/resume":
			return map[string]any{
				"thread": map[string]any{"id": threadID, "cwd": root}, "approvalPolicy": "on-request",
			}, nil
		case "thread/delete":
			select {
			case deleted <- struct{}{}:
			default:
			}
			return map[string]any{}, nil
		case "thread/archive", "thread/unarchive":
			parked <- stringValue(request["method"])
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
		appServerSock: appSocket,
	}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "prepared-race", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true, DeleteOnAbort: true, Cwd: root,
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID), "state.json")
	shimSocket := filepath.Join(root, "race-shim.sock")
	listener, err := net.Listen("unix", shimSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			body, _ := bufio.NewReader(connection).ReadBytes('\n')
			_ = connection.Close()
			var message map[string]any
			if json.Unmarshal(body, &message) == nil && stringValue(message["action"]) == "shutdown" {
				_ = os.Remove(statePath)
			}
		}
	}()
	if err := writeJSONAtomic(statePath, map[string]any{
		"sessionId": threadID, "pid": os.Getpid(), "socketPath": shimSocket,
		"name": "prepared", "permissionMode": "default", "status": "idle",
	}); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
	}
	request := map[string]any{
		"sessionId": threadID, "requestId": record.RequestID,
		"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
		"cwd": root, "name": "prepared", "permissionMode": "default", "status": "idle",
	}
	lifecycleLock, err := lockLaneLifecycle(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 2)
	registerDone := make(chan error, 1)
	abortDone := make(chan error, 1)
	go func() {
		ready <- struct{}{}
		_, registerErr := supervisor.registerPreparedLaunch(request)
		registerDone <- registerErr
	}()
	go func() {
		ready <- struct{}{}
		_, abortErr := supervisor.abortPreparedLaunch(request)
		abortDone <- abortErr
	}()
	<-ready
	<-ready
	unlockLaneLifecycle(lifecycleLock)
	registerErr := <-registerDone
	abortErr := <-abortDone
	if abortErr != nil {
		t.Fatalf("abort prepared race: %v", abortErr)
	}
	if registerErr != nil && !strings.Contains(registerErr.Error(), "owner changed") {
		t.Fatalf("register prepared race: %v", registerErr)
	}
	deleteWon := false
	select {
	case <-deleted:
		deleteWon = true
	default:
	}
	parkMethods := []string{}
	for len(parked) > 0 {
		parkMethods = append(parkMethods, <-parked)
	}
	if len(parkMethods) != 0 {
		t.Fatalf("serialized zero-turn abort archived the transcript: %v", parkMethods)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(statePath); os.IsNotExist(statErr) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	owner := readInteractiveOwner(paths, threadID)
	if deleteWon {
		if owner != nil {
			t.Fatalf("serialized delete retained owner: %#v", owner)
		}
	} else if owner == nil || !owner.Pending || !owner.Prepared || !owner.ParkOnAbort || !owner.Aborting ||
		publishablePeerThreadNative(paths, threadID) {
		t.Fatalf("serialized durable abort lost its non-authorizing takeover proof: %#v", owner)
	}
	if len(supervisor.shims) != 0 || supervisor.subscribed[threadID] {
		t.Fatalf("serialized abort retained publication: shims=%#v subscribed=%#v", supervisor.shims, supervisor.subscribed)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("serialized abort retained shim state: %v", statErr)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestPreparedResumeRevalidatesLoadedAndArchivedBeforePublication(t *testing.T) {
	for _, test := range []struct {
		name     string
		method   string
		response map[string]any
		want     string
	}{
		{name: "loaded", method: "thread/loaded/list", response: map[string]any{"data": []string{"00000000-0000-0000-0000-000000000a15"}}, want: "became loaded"},
		{name: "archived", method: "thread/list", response: map[string]any{"data": []any{map[string]any{"id": "00000000-0000-0000-0000-000000000a15"}}}, want: "became archived"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-000000000a15"
			_, appSocket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
				switch method := stringValue(request["method"]); method {
				case "initialize":
					return map[string]any{}, nil
				case "thread/list", "thread/loaded/list":
					if method == test.method {
						return test.response, nil
					}
					return map[string]any{"data": []any{}}, nil
				default:
					return nil, fmt.Errorf("unexpected publication method %s", method)
				}
			})
			paths := nativePaths{
				dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: appSocket,
			}
			record := interactiveOwnerRecord{
				ThreadID: threadID, RequestID: "resume-revalidate", OwnerPID: os.Getpid(),
				OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true, Cwd: root,
			}
			if err := writeInteractiveOwnerRecord(paths, record); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(interactiveOwnerPath(paths, threadID))
			if err != nil {
				t.Fatal(err)
			}
			supervisor := &nativeSupervisor{
				paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
				activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
			}
			_, err = supervisor.registerPreparedLaunch(map[string]any{
				"sessionId": threadID, "requestId": record.RequestID,
				"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("publication revalidation error = %v, want %q", err, test.want)
			}
			after, readErr := os.ReadFile(interactiveOwnerPath(paths, threadID))
			if readErr != nil || !bytes.Equal(before, after) || len(supervisor.shims) != 0 || supervisor.subscribed[threadID] {
				t.Fatalf("revalidation mutated state: read=%v owner_changed=%v shims=%#v subscribed=%v", readErr, !bytes.Equal(before, after), supervisor.shims, supervisor.subscribed[threadID])
			}
			if supervisor.client != nil {
				supervisor.client.close()
			}
		})
	}
}

func TestUnauthorizedWakeRecoveryDoesNotCreateInbox(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	threadID := "00000000-0000-0000-0000-000000000a11"
	record := wakeRecord{
		SessionID: threadID, MessageID: "legacy-ordinary-wake", State: "queueing", Delivery: "queued",
		Item: map[string]any{"id": "legacy-ordinary-wake", "message": "must not revive"}, CreatedAt: 123,
	}
	if err := writeWakeRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	supervisor := nativeSupervisor{paths: paths, shims: map[string]map[string]any{}, retired: map[string]bool{}}
	supervisor.recoverWakeRecords()
	messages, err := consumeNativeInbox(paths, threadID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("unauthorized recovery inbox = %#v, %v", messages, err)
	}
	if current := readWakeRecord(paths, threadID, record.MessageID); current == nil || current.State != "queueing" {
		t.Fatalf("unauthorized wake journal was mutated: %+v", current)
	}
}

func TestOrdinaryNotificationsDoNotMutatePeerState(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	threadID := "00000000-0000-0000-0000-000000000a05"
	supervisor := nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
		releasing: map[string]int64{},
	}
	notifications := []struct {
		method string
		params map[string]any
	}{
		{"thread/started", map[string]any{"thread": map[string]any{
			"id": threadID, "cwd": root, "path": filepath.Join(root, "ordinary.jsonl"), "source": "cli", "createdAt": time.Now().Unix(),
		}}},
		{"thread/status/changed", map[string]any{"threadId": threadID, "status": map[string]any{"type": "active"}}},
		{"turn/started", map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn-ordinary"}}},
		{"thread/tokenUsage/updated", map[string]any{"threadId": threadID, "turnId": "turn-ordinary", "tokenUsage": map[string]any{"totalTokens": 1}}},
		{"item/completed", map[string]any{"threadId": threadID, "turnId": "turn-ordinary", "item": map[string]any{"id": "item-ordinary"}}},
		{"turn/completed", map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn-ordinary", "status": "completed"}}},
		{"thread/name/updated", map[string]any{"threadId": threadID, "threadName": "ordinary"}},
		{"thread/archived", map[string]any{"threadId": threadID}},
		{"thread/deleted", map[string]any{"threadId": threadID}},
		{"thread/unarchived", map[string]any{"threadId": threadID}},
		{"thread/closed", map[string]any{"threadId": threadID}},
	}
	for _, notification := range notifications {
		body, err := json.Marshal(notification.params)
		if err != nil {
			t.Fatal(err)
		}
		supervisor.handleNotification(rpcNotification{Method: notification.method, Params: body})
	}
	time.Sleep(25 * time.Millisecond)
	if len(supervisor.activeTurns) != 0 || len(supervisor.shims) != 0 || len(supervisor.subscribed) != 0 {
		t.Fatalf("ordinary notifications mutated memory: turns=%v shims=%v subscribed=%v", supervisor.activeTurns, supervisor.shims, supervisor.subscribed)
	}
	if _, err := os.Stat(retiredThreadPath(paths, threadID)); !os.IsNotExist(err) {
		t.Fatalf("ordinary notifications created retirement residue: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID))); !os.IsNotExist(err) {
		t.Fatalf("ordinary notifications created session state: %v", err)
	}
}

func TestReleaseNotificationsDoNotWaitOnLifecycleLock(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a39"
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
		releasing: map[string]int64{threadID: time.Now().Add(interactiveReleaseGrace).UnixMilli()},
	}
	lock, err := lockLaneLifecycle(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockLaneLifecycle(lock)
	done := make(chan struct{})
	go func() {
		supervisor.handleDestructiveThreadNotification(threadID, "archived")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("release archive notification blocked on its own lifecycle transaction")
	}
	if supervisor.isRetired(threadID) {
		t.Fatal("guarded release notification retired the thread")
	}
}

func TestReleaseUnarchiveClearsPriorRetirementWithoutWaitingOnLifecycleLock(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a40"
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	if err := markRetiredThread(paths, threadID); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{threadID: true},
		releasing: map[string]int64{threadID: time.Now().Add(interactiveReleaseGrace).UnixMilli()},
	}
	lock, err := lockLaneLifecycle(paths, threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockLaneLifecycle(lock)
	done := make(chan struct{})
	go func() {
		supervisor.handleDestructiveThreadNotification(threadID, "unarchived")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("release unarchive notification blocked on its own lifecycle transaction")
	}
	if supervisor.isRetired(threadID) || isRetiredThreadNative(paths, threadID) {
		t.Fatal("release unarchive left a durable retirement marker")
	}
}

func TestFailedUnarchiveKeepsReleaseGuardAndOwnerForRetry(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a44"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize", "thread/archive":
			return map[string]any{}, nil
		case "thread/unarchive":
			return nil, errors.New("unarchive unavailable")
		default:
			return nil, fmt.Errorf("unexpected release method %s", stringValue(request["method"]))
		}
	})
	paths := nativePaths{
		dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"), appServerSock: socket,
	}
	writeTestAttachedInteractiveOwner(t, paths, threadID)
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
		releasing: map[string]int64{},
	}
	if err := supervisor.releaseInteractiveThread(threadID); err == nil || !strings.Contains(err.Error(), "unarchive unavailable") {
		t.Fatalf("failed unarchive error = %v", err)
	}
	if !supervisor.interactiveReleasePending(threadID) || readInteractiveOwner(paths, threadID) == nil {
		t.Fatalf("ambiguous park lost retry state: release=%v owner=%#v", supervisor.releasing, readInteractiveOwner(paths, threadID))
	}
	supervisor.handleDestructiveThreadNotification(threadID, "archived")
	if supervisor.isRetired(threadID) || readInteractiveOwner(paths, threadID) == nil {
		t.Fatalf("delayed archive retired ambiguous park: retired=%v owner=%#v", supervisor.isRetired(threadID), readInteractiveOwner(paths, threadID))
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestClosedInteractiveThreadDropsAuthorityOnlyWhenUnloaded(t *testing.T) {
	for _, stillLoaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unloaded", true: "still-loaded"}[stillLoaded], func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-000000000a40"
			methods := []string{}
			_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
				method := stringValue(request["method"])
				if method != "initialize" {
					methods = append(methods, method)
				}
				switch method {
				case "initialize":
					return map[string]any{}, nil
				case "thread/loaded/list":
					if stillLoaded {
						return map[string]any{"data": []string{threadID}}, nil
					}
					return map[string]any{"data": []string{}}, nil
				default:
					return nil, fmt.Errorf("unexpected closed-thread method %s", method)
				}
			})
			paths := nativePaths{
				dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
				appServerSock: socket,
			}
			writeTestAttachedInteractiveOwner(t, paths, threadID)
			supervisor := &nativeSupervisor{
				paths: paths, done: make(chan struct{}),
				shims:       map[string]map[string]any{threadID: {"sessionId": threadID}},
				activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true},
				retired: map[string]bool{}, releasing: map[string]int64{}, closedCandidates: map[string]uint64{threadID: 1},
			}
			client, err := supervisor.ensureClient()
			if err != nil {
				t.Fatal(err)
			}
			if err := supervisor.reconcileClosedInteractiveCandidatesFromAppServer(client); err != nil {
				t.Fatal(err)
			}
			owner := readInteractiveOwner(paths, threadID)
			if stillLoaded {
				if owner == nil || len(supervisor.shims) != 1 || !supervisor.subscribed[threadID] {
					t.Fatalf("loaded closed notification removed authority: owner=%#v shims=%#v", owner, supervisor.shims)
				}
			} else if owner != nil || len(supervisor.shims) != 0 || supervisor.subscribed[threadID] {
				t.Fatalf("unloaded closed thread retained authority: owner=%#v shims=%#v", owner, supervisor.shims)
			}
			if strings.Join(methods, ",") != "thread/loaded/list" {
				t.Fatalf("closed-thread reconciliation mutated App Server: %v", methods)
			}
			if supervisor.client != nil {
				supervisor.client.close()
			}
		})
	}
}

func TestClosedThreadPreservesLaneAndUnknownOwner(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, nativePaths, string)
	}{
		{
			name: "lane",
			setup: func(t *testing.T, paths nativePaths, threadID string) {
				writeTestActiveCodexLane(t, paths, threadID)
			},
		},
		{
			name: "unknown-owner",
			setup: func(t *testing.T, paths nativePaths, threadID string) {
				if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), interactiveOwnerRecord{
					ThreadID: threadID, RequestID: "unknown-closed-owner", OwnerPID: os.Getpid(),
					OwnerProcStart: "", UpdatedAt: time.Now().UnixMilli(),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-000000000a42"
			paths := nativePaths{
				dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile"),
				appServerSock: filepath.Join(root, "missing-appserver.sock"),
			}
			test.setup(t, paths, threadID)
			supervisor := &nativeSupervisor{
				paths: paths, done: make(chan struct{}),
				shims:       map[string]map[string]any{threadID: {"sessionId": threadID}},
				activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true},
				retired: map[string]bool{}, releasing: map[string]int64{},
			}
			if completed := supervisor.reconcileClosedInteractiveThreadWithLoaded(threadID, false); test.name == "lane" && !completed {
				t.Fatal("closed lane candidate was not recognized as complete")
			} else if test.name == "unknown-owner" && completed {
				t.Fatal("unknown owner candidate was not retained for retry")
			}
			if len(supervisor.shims) != 1 || !supervisor.subscribed[threadID] {
				t.Fatalf("closed %s lost transport: shims=%#v subscribed=%#v", test.name, supervisor.shims, supervisor.subscribed)
			}
		})
	}
}

func TestClosedThreadReconciliationRetriesUnknownOwnerWithoutGoroutineFanout(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a43"
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	owner := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "unknown-closed-owner", OwnerPID: os.Getpid(),
		OwnerProcStart: "", UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), owner); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), reconcileWake: make(chan struct{}, 1),
		shims:       map[string]map[string]any{threadID: {"sessionId": threadID}},
		activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true},
		retired: map[string]bool{}, releasing: map[string]int64{}, closedCandidates: map[string]uint64{},
	}
	supervisor.queueClosedInteractiveThread(threadID)
	supervisor.queueClosedInteractiveThread(threadID)
	if supervisor.closedCandidates[threadID] != 2 || len(supervisor.reconcileWake) != 1 {
		t.Fatalf("closed notification was not bounded and deduplicated: candidates=%v wake=%d", supervisor.closedCandidates, len(supervisor.reconcileWake))
	}
	supervisor.reconcileClosedInteractiveCandidates(map[string]bool{})
	if supervisor.closedCandidates[threadID] != 2 || readInteractiveOwner(paths, threadID) == nil || len(supervisor.shims) != 1 {
		t.Fatalf("unknown closed owner was not preserved for retry: candidates=%v owner=%#v shims=%#v", supervisor.closedCandidates, readInteractiveOwner(paths, threadID), supervisor.shims)
	}
	owner.OwnerProcStart = readProcStart(os.Getpid())
	if err := writeInteractiveOwnerRecord(paths, owner); err != nil {
		t.Fatal(err)
	}
	supervisor.reconcileClosedInteractiveCandidates(map[string]bool{})
	if len(supervisor.closedCandidates) != 0 || readInteractiveOwner(paths, threadID) != nil || len(supervisor.shims) != 0 {
		t.Fatalf("corroborated unloaded close retained authority: candidates=%v owner=%#v shims=%#v", supervisor.closedCandidates, readInteractiveOwner(paths, threadID), supervisor.shims)
	}
}

func TestPreparedTurnCompletionClearsActiveStatus(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000a41"
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	if err := writeInteractiveOwnerRecord(paths, interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "prepared-completion", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true, UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{threadID: "prepared-turn"}, subscribed: map[string]bool{},
		retired: map[string]bool{}, releasing: map[string]int64{},
	}
	params, _ := json.Marshal(map[string]any{
		"threadId": threadID, "turn": map[string]any{"id": "prepared-turn", "status": "completed"},
	})
	supervisor.handleNotification(rpcNotification{Method: "turn/completed", Params: params})
	if supervisor.activeTurns[threadID] != "" {
		t.Fatalf("prepared completion retained active turn: %#v", supervisor.activeTurns)
	}
}

func TestOrdinaryHooksAreSilentAndDoNotActivateSupervisor(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "run", "missing-supervisor.sock"))
	threadID := "00000000-0000-0000-0000-000000000a06"
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		output, err := handleNativeHook(hookInput{Event: event, SessionID: threadID, Cwd: root})
		if err != nil || output != nil {
			t.Fatalf("ordinary %s hook = %#v, %v", event, output, err)
		}
	}
	paths := resolveNativePaths()
	if _, err := os.Stat(paths.supervisorSock); !os.IsNotExist(err) {
		t.Fatalf("ordinary hook activated supervisor socket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.dataRoot, "sessions", sessionKey(threadID))); !os.IsNotExist(err) {
		t.Fatalf("ordinary hook created peer state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profileDataRoot(paths), "lane-lifecycle-locks")); !os.IsNotExist(err) {
		t.Fatalf("ordinary hook created lifecycle lock residue: %v", err)
	}
}

func TestOrdinaryHookCommandWritesNoOutput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", filepath.Join(root, "run", "missing-supervisor.sock"))

	input, err := os.CreateTemp(root, "hook-input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(`{"hook_event_name":"SessionStart","session_id":"00000000-0000-0000-0000-000000000a09","cwd":"` + root + `"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin, originalStdout, originalStderr := os.Stdin, os.Stdout, os.Stderr
	restored := false
	restoreProcessIO := func() {
		if restored {
			return
		}
		restored = true
		os.Stdin, os.Stdout, os.Stderr = originalStdin, originalStdout, originalStderr
		_ = input.Close()
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
	}
	t.Cleanup(restoreProcessIO)
	os.Stdin, os.Stdout, os.Stderr = input, stdoutWrite, stderrWrite
	runHookCommand()
	restoreProcessIO()
	stdout, _ := io.ReadAll(stdoutRead)
	stderr, _ := io.ReadAll(stderrRead)
	_ = stdoutRead.Close()
	_ = stderrRead.Close()
	if !bytes.Equal(stdout, nil) || !bytes.Equal(stderr, nil) {
		t.Fatalf("ordinary hook command stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestPeerHooksNeverRenameThreadDuringTurnReconciliation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	threadID := "00000000-0000-0000-0000-000000000a35"
	paths := resolveNativePaths()
	if err := writeInteractiveOwnerRecord(paths, interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "named-owner", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Name: "stable-peer-name", NameSource: "launch",
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	actions := make(chan map[string]any, 3)
	supervisorSocket := startAuthorizationControlServer(t, func(request map[string]any) map[string]any {
		actions <- request
		return map[string]any{"state": map[string]any{"sessionId": threadID}}
	})
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	paths = resolveNativePaths()
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		if _, err := ensureHookShim(paths, hookInput{Event: event, SessionID: threadID, Cwd: root}, liveInteractiveOwnerRecord(paths, threadID)); err != nil {
			t.Fatalf("%s hook reconciliation: %v", event, err)
		}
	}
	for index := 0; index < 3; index++ {
		select {
		case request := <-actions:
			if action := stringValue(request["action"]); action != "register" {
				t.Fatalf("hook reconciliation issued %q", action)
			}
			if name := stringValue(request["name"]); name != "stable-peer-name" {
				t.Fatalf("hook registration name = %q", name)
			}
		case <-time.After(time.Second):
			t.Fatal("hook reconciliation did not register peer")
		}
	}
}

func TestPeerTurnHooksPreserveManualShimRename(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	threadID := "00000000-0000-0000-0000-000000000a36"
	paths := resolveNativePaths()
	writeTestAttachedInteractiveOwner(t, paths, threadID)
	shim := newDaemon(map[string]string{
		"session-id": threadID, "cwd": root, "name": "launch-name", "name-source": "launch",
		"data-dir": paths.dataRoot, "claude-config-dir": paths.claudeRoot,
		"codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
	})
	if err := shim.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shim.shutdown()
		time.Sleep(25 * time.Millisecond)
	})
	if err := sendUnixJSON(shim.backendSocket, map[string]any{
		"type": "control", "action": "rename", "name": "manual-name",
	}, time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if stringValue(readJSONMap(shim.stateFile)["name"]) == "manual-name" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, event := range []string{"UserPromptSubmit", "Stop", "SessionStart"} {
		if _, err := ensureHookShim(paths, hookInput{Event: event, SessionID: threadID, Cwd: root}, liveInteractiveOwnerRecord(paths, threadID)); err != nil {
			t.Fatalf("%s hook reconciliation: %v", event, err)
		}
	}
	state := readJSONMap(shim.stateFile)
	if stringValue(state["name"]) != "manual-name" || stringValue(state["nameSource"]) != "manual" {
		t.Fatalf("turn hook reverted manual rename: %#v", state)
	}
}

func TestCodexLaneHookConsumesFallbackInbox(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	threadID := "00000000-0000-0000-0000-000000000a37"
	paths := resolveNativePaths()
	if err := recordLaneState(paths, laneState{
		Type: "codex-peer-lane", ThreadID: threadID, SessionID: threadID,
		Name: "hook-lane", Cwd: root, Status: "idle", PermissionMode: "default",
	}); err != nil {
		t.Fatal(err)
	}
	shim := newDaemon(map[string]string{
		"session-id": threadID, "cwd": root, "name": "hook-lane", "name-source": "lane",
		"data-dir": paths.dataRoot, "claude-config-dir": paths.claudeRoot,
		"codex-home": paths.codexHome, "runtime-dir": paths.runtimeDir,
	})
	if err := shim.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shim.shutdown()
		time.Sleep(25 * time.Millisecond)
	})
	if err := enqueueNativeInboxItem(paths, threadID, map[string]any{
		"type": "message", "id": "lane-hook-fallback", "message": "LANE_HOOK_FALLBACK",
	}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	output, err := handleNativeHook(hookInput{Event: "UserPromptSubmit", SessionID: threadID, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	specific, _ := output["hookSpecificOutput"].(map[string]any)
	if !strings.Contains(stringValue(specific["additionalContext"]), "LANE_HOOK_FALLBACK") {
		t.Fatalf("lane hook fallback output = %#v", output)
	}
	messages, err := consumeNativeInbox(paths, threadID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("lane hook left fallback inbox = %#v, %v", messages, err)
	}
}

func TestHookRejectsMismatchedSupervisorWithoutRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	socket := startAuthorizationControlServer(t, func(_ map[string]any) map[string]any {
		return map[string]any{"runtimeIdentity": "sha256:stale", "pluginVersion": "stale"}
	})
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", socket)
	started := time.Now()
	err := ensureHookSupervisorCurrent(resolveNativePaths())
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched supervisor error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("mismatched hook attempted a blocking restart: %s", elapsed)
	}
}

func TestShimMaintenanceReapsMissingOwnerWithoutStartToken(t *testing.T) {
	d := newDaemon(map[string]string{
		"session-id": "00000000-0000-0000-0000-000000000a38", "cwd": t.TempDir(),
		"owner-pid": strconv.Itoa(1 << 30), "owner-proc-start": "", "heartbeat-ms": "50",
	})
	go d.maintenanceLoop()
	select {
	case <-d.done:
	case <-time.After(time.Second):
		t.Fatal("shim with a definitely missing tokenless owner was not reaped")
	}
}

func TestStdioMCPRequiresHostAttestationBeforeEveryTool(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	peerID := "00000000-0000-0000-0000-000000000a30"
	ordinaryID := "00000000-0000-0000-0000-000000000a31"
	supervisorSocket := startAuthorizationControlServer(t, func(_ map[string]any) map[string]any {
		return map[string]any{
			"ready": true, "appServerConnected": true, "appServerPid": os.Getppid(),
			"appServerProcStart": readProcStart(os.Getppid()),
		}
	})
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	paths := resolveNativePaths()
	writeTestAttachedInteractiveOwner(t, paths, peerID)
	peerSocket := filepath.Join(root, "peer.sock")
	listener, err := net.Listen("unix", peerSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := writeJSONAtomic(filepath.Join(paths.dataRoot, "sessions", sessionKey(peerID), "state.json"), map[string]any{
		"sessionId": peerID, "name": "attested-peer", "socketPath": peerSocket,
	}); err != nil {
		t.Fatal(err)
	}
	call := func(threadID, sessionID, turnThreadID, requested string) map[string]any {
		params, _ := json.Marshal(map[string]any{
			"name": "identity", "arguments": map[string]any{"session_id": requested},
			"_meta": map[string]any{
				"threadId":              threadID,
				"x-codex-turn-metadata": map[string]any{"session_id": sessionID, "thread_id": turnThreadID},
			},
		})
		result, err := handleNativeMCPRequest("tools/call", params, "")
		if err != nil {
			t.Fatal(err)
		}
		output, _ := result.(map[string]any)
		return output
	}
	if result := call(peerID, peerID, peerID, peerID); mcpResultIsError(result) {
		t.Fatalf("exact host-attested caller was rejected: %#v", result)
	}
	if result := call(ordinaryID, ordinaryID, ordinaryID, peerID); !mcpResultIsError(result) ||
		!strings.Contains(mcpResultText(result), "inactive outside an attested peer session") {
		t.Fatalf("ordinary caller used a live peer route: %#v", result)
	}
	if result := call(peerID, peerID, ordinaryID, peerID); !mcpResultIsError(result) ||
		!strings.Contains(mcpResultText(result), "inactive outside an attested peer session") {
		t.Fatalf("mismatched host metadata was accepted: %#v", result)
	}
	if result := call(peerID, peerID, peerID, ordinaryID); !mcpResultIsError(result) ||
		!strings.Contains(mcpResultText(result), "cannot act as") {
		t.Fatalf("model-supplied foreign route was accepted: %#v", result)
	}
}

func startAuthorizationControlServer(t *testing.T, handler func(map[string]any) map[string]any) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "supervisor.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				line, readErr := bufio.NewReader(connection).ReadBytes('\n')
				if readErr != nil {
					return
				}
				var request map[string]any
				if json.Unmarshal(line, &request) != nil {
					return
				}
				response := map[string]any{"ok": true}
				for key, value := range handler(request) {
					response[key] = value
				}
				body, _ := json.Marshal(response)
				_, _ = connection.Write(append(body, '\n'))
			}()
		}
	}()
	return socket
}

func mcpResultText(result map[string]any) string {
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 {
		return ""
	}
	return stringValue(content[0]["text"])
}

func mcpResultIsError(result map[string]any) bool {
	value, _ := result["isError"].(bool)
	return value
}

func TestStandaloneMCPProcessCannotForgeManagedHostMetadata(t *testing.T) {
	root := t.TempDir()
	peerID := "00000000-0000-0000-0000-000000000a32"
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	writeTestAttachedInteractiveOwner(t, paths, peerID)
	command := exec.Command(os.Args[0], "-test.run=^TestMCPImpersonationHelper$")
	command.Env = append(os.Environ(),
		"CODEX_MCP_IMPERSONATION_HELPER=1",
		"CODEX_MCP_IMPERSONATION_THREAD="+peerID,
		"CLAUDE_PEER_DATA_DIR="+paths.dataRoot,
		"CODEX_HOME="+paths.codexHome,
		"CLAUDE_PEER_SUPERVISOR_SOCKET="+filepath.Join(root, "missing-supervisor.sock"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("MCP impersonation helper: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "inactive outside an attested peer session") {
		t.Fatalf("standalone MCP process forged host metadata: %s", output)
	}
}

func TestPreparedAttachAndDeleteShareLifecycleTransaction(t *testing.T) {
	for _, attachFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete-first", true: "attach-first"}[attachFirst], func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-000000000a33"
			paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
			record := interactiveOwnerRecord{
				ThreadID: threadID, RequestID: "prepared-delete-race", OwnerPID: os.Getpid(),
				OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true,
			}
			if err := writeInteractiveOwnerRecord(paths, record); err != nil {
				t.Fatal(err)
			}
			supervisor := &nativeSupervisor{paths: paths, retired: map[string]bool{}, done: make(chan struct{})}
			if attachFirst {
				if _, err := markPreparedLaunchAttached(paths, threadID); err != nil {
					t.Fatal(err)
				}
			}
			supervisor.handleDestructiveThreadNotification(threadID, "deleted")
			if !attachFirst {
				attached, err := markPreparedLaunchAttached(paths, threadID)
				if err != nil || attached != nil {
					t.Fatalf("deleted pending owner was promoted: %#v, %v", attached, err)
				}
			}
			if owner := readInteractiveOwner(paths, threadID); owner != nil {
				t.Fatalf("deleted thread retained owner: %#v", owner)
			}
			if attachFirst && !readRetiredThreads(paths)[threadID] {
				t.Fatal("deleted attached peer did not retain its retirement boundary")
			}
		})
	}
}

func TestMCPImpersonationHelper(t *testing.T) {
	if os.Getenv("CODEX_MCP_IMPERSONATION_HELPER") != "1" {
		return
	}
	threadID := os.Getenv("CODEX_MCP_IMPERSONATION_THREAD")
	params, _ := json.Marshal(map[string]any{
		"name": "list_peers", "arguments": map[string]any{},
		"_meta": map[string]any{
			"threadId":              threadID,
			"x-codex-turn-metadata": map[string]any{"session_id": threadID, "thread_id": threadID},
		},
	})
	result, err := handleNativeMCPRequest("tools/call", params, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(result)
	_, _ = os.Stdout.Write(body)
}

func TestReconcileDoesNotReadResumeOrSubscribeOrdinaryLoadedThread(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	threadID := "00000000-0000-0000-0000-000000000a07"
	var methodsMu sync.Mutex
	methods := []string{}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methodsMu.Lock()
			methods = append(methods, method)
			methodsMu.Unlock()
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{threadID}}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/read", "thread/resume", "thread/subscribe":
			t.Fatalf("ordinary thread was touched through %s", method)
		}
		return map[string]any{}, nil
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", socket)
	supervisor, err := newNativeSupervisor("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.reconcile(); err != nil {
		t.Fatal(err)
	}
	methodsMu.Lock()
	got := append([]string(nil), methods...)
	methodsMu.Unlock()
	for _, forbidden := range []string{"thread/read", "thread/resume", "thread/subscribe"} {
		if containsString(got, forbidden) {
			t.Fatalf("ordinary reconciliation called %s; methods=%v", forbidden, got)
		}
	}
	if _, err := os.Stat(retiredThreadPath(supervisor.paths, threadID)); !os.IsNotExist(err) {
		t.Fatalf("ordinary reconciliation left retirement residue: %v", err)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}
