package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPreparedLaunchCreatesExactNamedOwner(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := canonicalLaunchDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-000000000101"
	methods := []string{}
	var methodsMu sync.Mutex
	var published map[string]any
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
		case "thread/start":
			params, _ := request["params"].(map[string]any)
			ephemeral, hasEphemeral := params["ephemeral"].(bool)
			if stringValue(params["cwd"]) != canonicalRoot || !hasEphemeral || ephemeral || stringValue(params["serviceName"]) != "codex-peer" ||
				stringValue(params["approvalPolicy"]) != "never" || stringValue(params["sandbox"]) != "danger-full-access" {
				return nil, errors.New("wrong thread/start parameters")
			}
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": canonicalRoot}, "approvalPolicy": "never"}, nil
		case "thread/name/set":
			params, _ := request["params"].(map[string]any)
			if stringValue(params["threadId"]) != threadID || stringValue(params["name"]) != "exact-peer" {
				return nil, errors.New("wrong thread/name/set parameters")
			}
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected App Server method")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket, func(request map[string]any) map[string]any {
		switch stringValue(request["action"]) {
		case "register_prepared":
			published = request
			commitPreparedOwnerForTest(resolveNativePaths(), threadID)
			return map[string]any{"state": map[string]any{"sessionId": threadID}}
		default:
			return map[string]any{}
		}
	})
	started := readProcStart(os.Getpid())
	got, err := startPreparedLaunchNative([]string{
		"--cwd", root, "--name", "exact peer", "--name-source", "explicit", "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", started, "--approval-policy", "never", "--sandbox", "danger-full-access",
	})
	if err != nil || got != threadID {
		t.Fatalf("prepared launch = %q, %v", got, err)
	}
	owner := readInteractiveOwner(resolveNativePaths(), threadID)
	if owner == nil || !owner.Pending || owner.OwnerPID != os.Getpid() || owner.OwnerProcStart != started ||
		owner.Cwd != canonicalRoot || owner.Name != "exact-peer" || owner.NameSource != "explicit" {
		t.Fatalf("prepared owner = %#v", owner)
	}
	if stringValue(published["action"]) != "register_prepared" || stringValue(published["sessionId"]) != threadID ||
		stringValue(published["name"]) != "exact-peer" || stringValue(published["permissionMode"]) != "bypassPermissions" ||
		stringValue(published["status"]) != "idle" {
		t.Fatalf("prepared publication = %#v", published)
	}
	methodsMu.Lock()
	gotMethods := strings.Join(methods, ",")
	methodsMu.Unlock()
	if gotMethods != "thread/start,thread/name/set" {
		t.Fatalf("prepared methods = %s", gotMethods)
	}
}

func TestPreparedLaunchGeneratesNameAndMaterializesRollout(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000102"
	gotName := ""
	var published map[string]any
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root}, "approvalPolicy": "never"}, nil
		case "thread/name/set":
			params, _ := request["params"].(map[string]any)
			gotName = stringValue(params["name"])
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected App Server method")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket, func(request map[string]any) map[string]any {
		switch stringValue(request["action"]) {
		case "register_prepared":
			published = request
			commitPreparedOwnerForTest(resolveNativePaths(), threadID)
			return map[string]any{"state": map[string]any{"sessionId": threadID}}
		default:
			return map[string]any{}
		}
	})
	started := readProcStart(os.Getpid())
	if _, err := startPreparedLaunchNative([]string{
		"--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", started,
	}); err != nil {
		t.Fatal(err)
	}
	want := defaultPeerName(root, threadID)
	owner := readInteractiveOwner(resolveNativePaths(), threadID)
	if gotName != want || owner == nil || owner.Name != want || owner.NameSource != "generated" ||
		stringValue(published["name"]) != want || stringValue(published["nameSource"]) != "generated" ||
		stringValue(published["permissionMode"]) != "bypassPermissions" {
		t.Fatalf("generated name = %q, owner=%#v, publication=%#v, want %q", gotName, owner, published, want)
	}
}

func TestPreparedLaunchRejectsEffectiveApprovalMismatch(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000101"
	deleted := false
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root}, "approvalPolicy": "on-request"}, nil
		case "thread/delete":
			deleted = true
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected method after policy mismatch")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	_, err := startPreparedLaunchNative([]string{
		"--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", readProcStart(os.Getpid()),
		"--approval-policy", "never", "--sandbox", "danger-full-access",
	})
	if err == nil || !strings.Contains(err.Error(), `applied approval policy "on-request", expected "never"`) || !deleted {
		t.Fatalf("effective approval mismatch = deleted=%v err=%v", deleted, err)
	}
	if owner := readInteractiveOwner(resolveNativePaths(), threadID); owner != nil {
		t.Fatalf("policy mismatch wrote owner: %#v", owner)
	}
}

func TestPreparedLaunchRejectsMismatchedPublicationState(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-00000000010a"
	deleted := false
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root}, "approvalPolicy": "on-request"}, nil
		case "thread/name/set":
			return map[string]any{}, nil
		case "thread/delete":
			deleted = true
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected App Server method")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket, func(request map[string]any) map[string]any {
		if stringValue(request["action"]) == "abort_prepared" {
			deleted = true
			removeInteractiveOwnerIfMatching(resolveNativePaths(), threadID, nil)
			return map[string]any{"aborted": true}
		}
		return map[string]any{"state": map[string]any{"sessionId": "00000000-0000-0000-0000-00000000010b"}}
	})
	_, err := startPreparedLaunchNative([]string{
		"--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err == nil || !strings.Contains(err.Error(), "mismatched state") || !deleted {
		t.Fatalf("mismatched publication = deleted=%v err=%v", deleted, err)
	}
	if owner := readInteractiveOwner(resolveNativePaths(), threadID); owner != nil {
		t.Fatalf("mismatched publication retained owner: %#v", owner)
	}
}

func TestPreparedLaunchRollsBackNameFailure(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000103"
	deleted := false
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": threadID}, "approvalPolicy": "on-request"}, nil
		case "thread/name/set":
			return nil, errors.New("name rejected")
		case "thread/delete":
			deleted = true
			return map[string]any{}, nil
		default:
			return nil, errors.New("unexpected App Server method")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	_, err := startPreparedLaunchNative([]string{
		"--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err == nil || !strings.Contains(err.Error(), "name rejected") || !deleted {
		t.Fatalf("rollback = deleted=%v err=%v", deleted, err)
	}
	if owner := readInteractiveOwner(resolveNativePaths(), threadID); owner != nil {
		t.Fatalf("failed launch retained owner: %#v", owner)
	}
}

func TestPreparedLaunchAttachmentPromotesOwner(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	threadID := "00000000-0000-0000-0000-000000000104"
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "request", OwnerPID: os.Getpid(), OwnerProcStart: readProcStart(os.Getpid()),
		Pending: true, Cwd: root, Name: "prepared", NameSource: "launch",
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	attached, err := markPreparedLaunchAttached(paths, threadID)
	if err != nil || attached == nil || attached.Pending {
		t.Fatalf("attach = %#v, %v", attached, err)
	}
	owner := readInteractiveOwner(paths, threadID)
	if owner == nil || owner.Pending || owner.Name != "prepared" {
		t.Fatalf("promoted owner = %#v", owner)
	}
}

func TestAbortPreparedLaunchDeletesOnlyMatchingPendingOwner(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "attached"}[attached], func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-00000000010c"
			deleted := false
			_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
				switch stringValue(request["method"]) {
				case "initialize":
					return map[string]any{}, nil
				case "thread/delete":
					deleted = true
					return map[string]any{}, nil
				default:
					return map[string]any{}, nil
				}
			})
			paths := nativePaths{
				profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"),
				appServerSock: socket,
			}
			record := interactiveOwnerRecord{
				ThreadID: threadID, RequestID: "abort-request", OwnerPID: os.Getpid(),
				OwnerProcStart: readProcStart(os.Getpid()), Pending: !attached, Prepared: true, DeleteOnAbort: true,
			}
			if err := writeInteractiveOwnerRecord(paths, record); err != nil {
				t.Fatal(err)
			}
			supervisor := &nativeSupervisor{
				paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
				activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
			}
			result, err := supervisor.abortPreparedLaunch(map[string]any{
				"sessionId": threadID, "requestId": record.RequestID,
				"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
			})
			if err != nil {
				t.Fatal(err)
			}
			preserved, _ := result["preserved"].(bool)
			aborted, _ := result["aborted"].(bool)
			if attached {
				if deleted || !preserved || readInteractiveOwner(paths, threadID) == nil {
					t.Fatalf("attached abort = deleted=%v result=%#v owner=%#v", deleted, result, readInteractiveOwner(paths, threadID))
				}
			} else if !deleted || !aborted || readInteractiveOwner(paths, threadID) != nil {
				t.Fatalf("pending abort = deleted=%v result=%#v owner=%#v", deleted, result, readInteractiveOwner(paths, threadID))
			}
			if supervisor.client != nil {
				supervisor.client.close()
			}
		})
	}
}

func TestAbortPreparedResumeBeforePublicationRemovesOnlyOwner(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-00000000010a"
	methods := []string{}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if method := stringValue(request["method"]); method != "initialize" {
			methods = append(methods, method)
		}
		return map[string]any{}, nil
	})
	paths := nativePaths{profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"), appServerSock: socket}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "resume-before-publication", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true,
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
	}
	result, err := supervisor.abortPreparedLaunch(map[string]any{
		"sessionId": threadID, "requestId": record.RequestID,
		"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
	})
	if err != nil || readInteractiveOwner(paths, threadID) != nil || len(methods) != 0 {
		t.Fatalf("resume prepublication abort = result=%#v err=%v owner=%#v methods=%v", result, err, readInteractiveOwner(paths, threadID), methods)
	}
	if aborted, _ := result["aborted"].(bool); !aborted {
		t.Fatalf("resume prepublication abort result = %#v", result)
	}
}

func TestAbortPreparedLaunchPreservesUncorroboratedOwner(t *testing.T) {
	for _, test := range []struct {
		name      string
		ownerPID  int
		procStart string
		wantError bool
	}{
		{name: "unknown", ownerPID: os.Getpid(), wantError: true},
		{name: "stale", ownerPID: 1 << 30, procStart: "definitely-stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-00000000010e"
			deleted := false
			_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
				switch stringValue(request["method"]) {
				case "initialize":
					return map[string]any{}, nil
				case "thread/delete":
					deleted = true
					return map[string]any{}, nil
				default:
					return map[string]any{}, nil
				}
			})
			paths := nativePaths{profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"), appServerSock: socket}
			record := interactiveOwnerRecord{
				ThreadID: threadID, RequestID: "uncorroborated-abort", OwnerPID: test.ownerPID,
				OwnerProcStart: test.procStart, Pending: true, Prepared: true,
			}
			if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), record); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(interactiveOwnerPath(paths, threadID))
			if err != nil {
				t.Fatal(err)
			}
			supervisor := &nativeSupervisor{
				paths: paths, done: make(chan struct{}),
				shims:       map[string]map[string]any{threadID: {"sessionId": threadID}},
				activeTurns: map[string]string{threadID: "turn"}, subscribed: map[string]bool{threadID: true}, retired: map[string]bool{},
			}
			result, abortErr := supervisor.abortPreparedLaunch(map[string]any{
				"sessionId": threadID, "requestId": record.RequestID,
				"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
			})
			if (abortErr != nil) != test.wantError {
				t.Fatalf("abort error = %v, wantError=%v", abortErr, test.wantError)
			}
			if preserved, _ := result["preserved"].(bool); !preserved || deleted {
				t.Fatalf("uncorroborated abort = result=%#v deleted=%v", result, deleted)
			}
			after, err := os.ReadFile(interactiveOwnerPath(paths, threadID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("uncorroborated abort mutated owner: err=%v before=%q after=%q", err, before, after)
			}
			if len(supervisor.shims) != 1 || !supervisor.subscribed[threadID] || supervisor.activeTurns[threadID] != "turn" {
				t.Fatalf("uncorroborated abort mutated transport: shims=%#v subscribed=%#v turns=%#v", supervisor.shims, supervisor.subscribed, supervisor.activeTurns)
			}
		})
	}
}

func TestFailedPreparedAbortCannotBePromotedAndReaperRetries(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-00000000010f"
	deleteFails := true
	deleted := false
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/delete":
			if deleteFails {
				return nil, errors.New("delete unavailable")
			}
			deleted = true
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"), appServerSock: socket}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "failed-abort", OwnerPID: os.Getpid(),
		OwnerProcStart: readProcStart(os.Getpid()), Pending: true, Prepared: true, DeleteOnAbort: true,
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{}, retired: map[string]bool{},
	}
	_, err := supervisor.abortPreparedLaunch(map[string]any{
		"sessionId": threadID, "requestId": record.RequestID,
		"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
	})
	if err == nil || !strings.Contains(err.Error(), "delete unavailable") {
		t.Fatalf("failed abort error = %v", err)
	}
	aborting := readInteractiveOwner(paths, threadID)
	if aborting == nil || !aborting.Pending || !aborting.Aborting {
		t.Fatalf("failed abort record = %#v", aborting)
	}
	if promoted, promoteErr := markPreparedLaunchAttached(paths, threadID); promoteErr == nil || promoted == nil || !promoted.Pending || !promoted.Aborting {
		t.Fatalf("late SessionStart promotion = record=%#v err=%v", promoted, promoteErr)
	}
	aborting.OwnerPID = 1 << 30
	aborting.OwnerProcStart = "definitely-stale"
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), aborting); err != nil {
		t.Fatal(err)
	}
	deleteFails = false
	supervisor.reconcileExitedInteractiveOwners()
	if !deleted || readInteractiveOwner(paths, threadID) != nil {
		t.Fatalf("stale abort retry = deleted=%v owner=%#v", deleted, readInteractiveOwner(paths, threadID))
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestPreparedLaunchReaperDeletesOnlyDefinitelyStalePendingOwner(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000105"
	deleted := false
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/delete":
			deleted = true
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{profileRoot: filepath.Join(root, "profile"), appServerSock: socket}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "stale-request", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", Pending: true, Prepared: true, DeleteOnAbort: true,
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), record); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{paths: paths, done: make(chan struct{})}
	supervisor.reconcileExitedInteractiveOwners()
	if !deleted || readInteractiveOwner(paths, threadID) != nil {
		t.Fatalf("stale pending launch = deleted=%v owner=%#v", deleted, readInteractiveOwner(paths, threadID))
	}
}

func TestPreparedLaunchReaperUnpublishesBeforeDeleteRetry(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-00000000010d"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/delete":
			return nil, errors.New("delete unavailable")
		default:
			return map[string]any{}, nil
		}
	})
	paths := nativePaths{profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"), appServerSock: socket}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "stale-delete-retry", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", Pending: true, Prepared: true, DeleteOnAbort: true,
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), record); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{threadID: {"sessionId": threadID}},
		activeTurns: map[string]string{threadID: "turn"}, subscribed: map[string]bool{threadID: true}, retired: map[string]bool{},
	}
	supervisor.reconcileExitedInteractiveOwners()
	if readInteractiveOwner(paths, threadID) == nil {
		t.Fatal("failed delete discarded the durable owner needed for retry")
	}
	if len(supervisor.shims) != 0 || len(supervisor.activeTurns) != 0 || supervisor.subscribed[threadID] {
		t.Fatalf("failed delete retained publication: shims=%#v turns=%#v subscribed=%#v", supervisor.shims, supervisor.activeTurns, supervisor.subscribed)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestCommittedZeroTurnLaunchRetainsTakeoverProofWithoutArchiving(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000110"
	methods := []string{}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods = append(methods, method)
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		default:
			return nil, fmt.Errorf("committed zero-turn thread must not call %s", method)
		}
	})
	paths := nativePaths{profileRoot: filepath.Join(root, "profile"), dataRoot: filepath.Join(root, "state"), appServerSock: socket}
	record := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "committed-zero-turn", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", Pending: true, Prepared: true, ParkOnAbort: true,
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), record); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{
		paths: paths, done: make(chan struct{}), shims: map[string]map[string]any{threadID: {"sessionId": threadID}},
		activeTurns: map[string]string{}, subscribed: map[string]bool{threadID: true}, retired: map[string]bool{}, releasing: map[string]int64{},
	}
	supervisor.reconcileExitedInteractiveOwners()
	supervisor.reconcileExitedInteractiveOwners()
	if readInteractiveOwner(paths, threadID) == nil || len(supervisor.shims) != 0 {
		t.Fatalf("committed zero-turn cleanup = owner=%#v shims=%#v", readInteractiveOwner(paths, threadID), supervisor.shims)
	}
	if len(methods) != 0 {
		t.Fatalf("committed zero-turn methods = %v", methods)
	}
	if supervisor.client != nil {
		supervisor.client.close()
	}
}

func TestPreparedLaunchRejectsUncorroboratedOwner(t *testing.T) {
	root := t.TempDir()
	_, err := startPreparedLaunchNative([]string{
		"--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", "wrong",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot corroborate") {
		t.Fatalf("uncorroborated owner error = %v", err)
	}
}

func TestParseLaunchOptionsRejectsMissingValue(t *testing.T) {
	if _, err := parseLaunchOptions([]string{"--cwd"}); err == nil {
		t.Fatal("expected missing-value error")
	}
}

func TestCanonicalLaunchDirectoryRejectsLineBreak(t *testing.T) {
	root := t.TempDir()
	newline := filepath.Join(root, "line\nbreak")
	if err := os.Mkdir(newline, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalLaunchDirectory(newline); err == nil || !strings.Contains(err.Error(), "line break") {
		t.Fatalf("newline cwd error = %v", err)
	}
}

func TestPreparedResumeBindsExactUUIDFromAuthoritativeRead(t *testing.T) {
	root := t.TempDir()
	requestedRoot := t.TempDir()
	requestedRoot, err := filepath.EvalSymlinks(requestedRoot)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-000000000115"
	var published map[string]any
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root, "name": "existing-peer", "source": "cli"}}, nil
		case "thread/list", "thread/loaded/list":
			return map[string]any{"data": []any{}}, nil
		default:
			return nil, errors.New("unexpected exact UUID method")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket, func(request map[string]any) map[string]any {
		switch stringValue(request["action"]) {
		case "register_prepared":
			published = request
			return map[string]any{"state": map[string]any{"sessionId": threadID}}
		case "abort_prepared":
			removeInteractiveOwnerIfMatching(resolveNativePaths(), threadID, nil)
			return map[string]any{"aborted": true}
		default:
			return map[string]any{}
		}
	})
	started := readProcStart(os.Getpid())
	got, effectiveCwd, err := bindPreparedResumeNative([]string{
		"--target", threadID, "--cwd", requestedRoot, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", started,
		"--cwd-explicit", "false", "--approval-policy", "never", "--sandbox", "danger-full-access",
	})
	if err != nil || got != threadID || effectiveCwd != requestedRoot {
		t.Fatalf("exact resume bind = id=%q cwd=%q err=%v", got, effectiveCwd, err)
	}
	owner := readInteractiveOwner(resolveNativePaths(), threadID)
	if owner == nil || !owner.Pending || !owner.Prepared || owner.DeleteOnAbort || owner.OwnerPID != os.Getpid() || owner.Name != "existing-peer" || owner.Cwd != requestedRoot {
		t.Fatalf("exact resume owner = %#v", owner)
	}
	if stringValue(published["action"]) != "register_prepared" || stringValue(published["sessionId"]) != threadID ||
		stringValue(published["cwd"]) != requestedRoot || stringValue(published["approvalPolicy"]) != "never" ||
		stringValue(published["sandbox"]) != "danger-full-access" {
		t.Fatalf("exact resume publication = %#v", published)
	}
}

func TestPreparedResumeTakesOverLoadedZeroTurnOwnerWithoutArchiving(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000126"
	methods := []string{}
	var publishedOwner *interactiveOwnerRecord
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods = append(methods, method)
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root, "name": "zero-turn", "source": "cli"}}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{threadID}}, nil
		default:
			return nil, fmt.Errorf("zero-turn takeover called destructive method %s", method)
		}
	})
	setPreparedLaunchTestEnv(t, root, socket, func(request map[string]any) map[string]any {
		if stringValue(request["action"]) != "register_prepared" {
			return map[string]any{}
		}
		publishedOwner = readInteractiveOwner(resolveNativePaths(), threadID)
		return map[string]any{"state": map[string]any{"sessionId": threadID}}
	})
	paths := resolveNativePaths()
	stale := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "exited-zero-turn", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", Pending: true, Prepared: true, ParkOnAbort: true,
		Cwd: root, Name: "zero-turn", UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), stale); err != nil {
		t.Fatal(err)
	}
	got, _, err := bindPreparedResumeNative([]string{
		"--target", threadID, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", readProcStart(os.Getpid()), "--cwd-explicit", "false",
	})
	if err != nil || got != threadID {
		t.Fatalf("zero-turn takeover = id=%q err=%v", got, err)
	}
	if publishedOwner == nil || !publishedOwner.Pending || !publishedOwner.Prepared || !publishedOwner.ResumeLoaded ||
		publishedOwner.RequestID == stale.RequestID || publishedOwner.OwnerPID != os.Getpid() {
		t.Fatalf("zero-turn takeover owner = %#v", publishedOwner)
	}
	if strings.Contains(strings.Join(methods, ","), "thread/archive") || strings.Contains(strings.Join(methods, ","), "thread/unarchive") {
		t.Fatalf("zero-turn takeover archived the thread: %v", methods)
	}
}

func TestPreparedResumeDetachesLoadedStaleOwnerBeforeImplicitCwdChange(t *testing.T) {
	migrationRoot := t.TempDir()
	threadRoot := filepath.Join(migrationRoot, "codex-messaging")
	requestedRoot := filepath.Join(migrationRoot, "agent-sessions")
	if err := os.Mkdir(threadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(threadRoot, requestedRoot); err != nil {
		t.Fatal(err)
	}
	canonicalRequestedRoot, err := canonicalLaunchDirectory(requestedRoot)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "00000000-0000-0000-0000-00000000012a"
	methods := []string{}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		method := stringValue(request["method"])
		if method != "initialize" {
			methods = append(methods, method)
		}
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": threadRoot, "source": "cli"}}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{threadID}}, nil
		default:
			return nil, fmt.Errorf("unexpected loaded cwd-override method %s", method)
		}
	})
	actions := []string{}
	var published map[string]any
	setPreparedLaunchTestEnv(t, requestedRoot, socket, func(request map[string]any) map[string]any {
		action := stringValue(request["action"])
		actions = append(actions, action)
		switch action {
		case "detach_stale_prepared":
			return map[string]any{"detached": true}
		case "register_prepared":
			published = request
			commitPreparedOwnerForTest(resolveNativePaths(), threadID)
			return map[string]any{"state": map[string]any{"sessionId": threadID}}
		default:
			return map[string]any{}
		}
	})
	paths := resolveNativePaths()
	stale := interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "exited-loaded-cwd", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", Pending: true, Prepared: true, ParkOnAbort: true, Aborting: true,
		Cwd: threadRoot, UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), stale); err != nil {
		t.Fatal(err)
	}
	got, effectiveCwd, err := bindPreparedResumeNative([]string{
		"--target", threadID, "--cwd", requestedRoot, "--cwd-explicit", "false",
		"--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err != nil || got != threadID || effectiveCwd != canonicalRequestedRoot {
		t.Fatalf("loaded migrated cwd resume = id=%q cwd=%q err=%v", got, effectiveCwd, err)
	}
	owner := readInteractiveOwner(paths, threadID)
	if strings.Join(actions, ",") != "detach_stale_prepared,register_prepared" ||
		owner == nil || !owner.ResumeLoaded || owner.Cwd != canonicalRequestedRoot || stringValue(published["cwd"]) != canonicalRequestedRoot {
		t.Fatalf("loaded cwd migration actions=%v owner=%#v publication=%#v", actions, owner, published)
	}
	if strings.Contains(strings.Join(methods, ","), "thread/archive") || strings.Contains(strings.Join(methods, ","), "thread/unarchive") {
		t.Fatalf("loaded cwd migration archived the thread: %v", methods)
	}
}

func TestPreparedResumeUsesCurrentCwdWhenSavedCwdIsMissing(t *testing.T) {
	migrationRoot := t.TempDir()
	threadRoot := filepath.Join(migrationRoot, "codex-messaging")
	requestedRoot := filepath.Join(migrationRoot, "agent-sessions")
	if err := os.Mkdir(threadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(threadRoot, requestedRoot); err != nil {
		t.Fatal(err)
	}
	canonicalRequestedRoot, err := canonicalLaunchDirectory(requestedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(threadRoot); !os.IsNotExist(err) {
		t.Fatalf("saved cwd still exists after rename: %v", err)
	}
	threadID := "00000000-0000-0000-0000-000000000114"
	var published map[string]any
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "cwd": threadRoot, "source": "cli"}}, nil
		case "thread/list", "thread/loaded/list":
			return map[string]any{"data": []any{}}, nil
		default:
			return nil, errors.New("unexpected migrated-cwd method")
		}
	})
	setPreparedLaunchTestEnv(t, requestedRoot, socket, func(request map[string]any) map[string]any {
		switch stringValue(request["action"]) {
		case "register_prepared":
			published = request
			commitPreparedOwnerForTest(resolveNativePaths(), threadID)
			return map[string]any{"state": map[string]any{"sessionId": threadID}}
		default:
			return map[string]any{}
		}
	})
	got, effectiveCwd, err := bindPreparedResumeNative([]string{
		"--target", threadID, "--cwd", requestedRoot, "--cwd-explicit", "false",
		"--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err != nil || got != threadID || effectiveCwd != canonicalRequestedRoot {
		t.Fatalf("migrated cwd resume = id=%q cwd=%q err=%v", got, effectiveCwd, err)
	}
	owner := readInteractiveOwner(resolveNativePaths(), threadID)
	if owner == nil || owner.Cwd != canonicalRequestedRoot || stringValue(published["cwd"]) != canonicalRequestedRoot {
		t.Fatalf("migrated cwd owner=%#v publication=%#v", owner, published)
	}
	if _, err := os.Lstat(threadRoot); !os.IsNotExist(err) {
		t.Fatalf("resume recreated the missing saved cwd: %v", err)
	}
}

func TestPreparedResumeRejectsLoadedOrArchivedTargetBeforeOwnerWrite(t *testing.T) {
	for _, test := range []struct {
		name     string
		archived bool
		loaded   bool
		want     string
	}{
		{name: "loaded", loaded: true, want: "already loaded"},
		{name: "archived", archived: true, want: "is archived"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			threadID := "00000000-0000-0000-0000-000000000121"
			_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
				switch stringValue(request["method"]) {
				case "initialize":
					return map[string]any{}, nil
				case "thread/read":
					return map[string]any{"thread": map[string]any{"id": threadID, "cwd": root, "source": "cli"}}, nil
				case "thread/list":
					if test.archived {
						return map[string]any{"data": []map[string]any{{"id": threadID}}}, nil
					}
					return map[string]any{"data": []any{}}, nil
				case "thread/loaded/list":
					if test.loaded {
						return map[string]any{"data": []string{threadID}}, nil
					}
					return map[string]any{"data": []string{}}, nil
				default:
					return nil, errors.New("unexpected method")
				}
			})
			setPreparedLaunchTestEnv(t, root, socket)
			_, _, err := bindPreparedResumeNative([]string{
				"--target", threadID, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()),
				"--owner-proc-start", readProcStart(os.Getpid()),
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resume conflict error = %v", err)
			}
			if owner := readInteractiveOwner(resolveNativePaths(), threadID); owner != nil {
				t.Fatalf("resume conflict wrote owner: %#v", owner)
			}
		})
	}
}

func TestPreparedResumeNameResolutionUsesNewestAppServerMatch(t *testing.T) {
	root := t.TempDir()
	firstID := "00000000-0000-0000-0000-000000000116"
	secondID := "00000000-0000-0000-0000-000000000117"
	name := "duplicate-resume"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			params, _ := request["params"].(map[string]any)
			if archived, _ := params["archived"].(bool); archived {
				return map[string]any{"data": []any{}}, nil
			}
			return map[string]any{"data": []map[string]any{
				{"id": secondID, "name": name, "cwd": root, "source": "cli"},
				{"id": firstID, "name": name, "cwd": root, "source": "cli"},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	started := readProcStart(os.Getpid())
	got, _, err := bindPreparedResumeNative([]string{
		"--target", name, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", started,
	})
	if err != nil || got != secondID {
		t.Fatalf("newest named resume = %q, %v", got, err)
	}
	if readInteractiveOwner(resolveNativePaths(), firstID) != nil || readInteractiveOwner(resolveNativePaths(), secondID) == nil {
		t.Fatal("named resolution did not bind only the newest App Server match")
	}
}

func TestPreparedResumeAppServerMatchIgnoresCorruptHistoricalIndex(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000128"
	name := "live-index-independent"
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	// Exceed the historical index scanner's explicit 4 MiB line limit. The
	// live App Server result must win before this fallback is opened.
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), bytes.Repeat([]byte("x"), 4*1024*1024+1), 0600); err != nil {
		t.Fatal(err)
	}
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			params, _ := request["params"].(map[string]any)
			if archived, _ := params["archived"].(bool); archived {
				return map[string]any{"data": []any{}}, nil
			}
			return map[string]any{"data": []map[string]any{{
				"id": threadID, "name": name, "cwd": root, "source": "cli",
			}}}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []any{}}, nil
		default:
			return nil, fmt.Errorf("unexpected App Server method %s", stringValue(request["method"]))
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	got, _, err := bindPreparedResumeNative([]string{
		"--target", name, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err != nil || got != threadID {
		t.Fatalf("live name resolution with corrupt historical index = %q, %v", got, err)
	}
}

func TestPreparedResumeRejectsExactSubagentBeforeOwnerWrite(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000124"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": root, "source": "subAgent",
				"parentThreadId": "00000000-0000-0000-0000-000000000125",
			}}, nil
		default:
			return nil, errors.New("subagent resume performed an unexpected App Server request")
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	_, _, err := bindPreparedResumeNative([]string{
		"--target", threadID, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err == nil || !strings.Contains(err.Error(), "is not an interactive root") {
		t.Fatalf("exact subagent resume error = %v", err)
	}
	if owner := readInteractiveOwner(resolveNativePaths(), threadID); owner != nil {
		t.Fatalf("exact subagent resume wrote owner: %#v", owner)
	}
}

func TestPreparedResumeNameResolutionSkipsNewerSubagent(t *testing.T) {
	root := t.TempDir()
	rootID := "00000000-0000-0000-0000-000000000126"
	subagentID := "00000000-0000-0000-0000-000000000127"
	name := "shared-root-name"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			params, _ := request["params"].(map[string]any)
			if archived, _ := params["archived"].(bool); archived {
				return map[string]any{"data": []any{}}, nil
			}
			return map[string]any{"data": []map[string]any{
				{"id": subagentID, "name": name, "cwd": root, "source": "subAgent", "parentThreadId": rootID},
				{"id": rootID, "name": name, "cwd": root, "source": "cli"},
			}}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []any{}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	got, _, err := bindPreparedResumeNative([]string{
		"--target", name, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err != nil || got != rootID {
		t.Fatalf("named root selection = %q, %v", got, err)
	}
	if readInteractiveOwner(resolveNativePaths(), subagentID) != nil || readInteractiveOwner(resolveNativePaths(), rootID) == nil {
		t.Fatal("named resolution bound the subagent instead of the interactive root")
	}
}

func TestPreparedResumeSynchronouslyReleasesExitedLoadedOwner(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000122"
	loaded := true
	var loadedMu sync.Mutex
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/loaded/list":
			loadedMu.Lock()
			defer loadedMu.Unlock()
			if loaded {
				return map[string]any{"data": []string{threadID}}, nil
			}
			return map[string]any{"data": []string{}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", socket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "exited-owner", OwnerPID: 1 << 30,
		OwnerProcStart: "definitely-stale", UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	actions := make(chan string, 1)
	supervisorSocket := startAuthorizationControlServer(t, func(request map[string]any) map[string]any {
		action := stringValue(request["action"])
		actions <- action
		if action == "release_stale_interactive" && stringValue(request["sessionId"]) == threadID {
			loadedMu.Lock()
			loaded = false
			loadedMu.Unlock()
		}
		return map[string]any{"released": true}
	})
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
	paths = resolveNativePaths()
	client, err := dialLaunchAppServer(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if staleOwner, err := releaseStaleLoadedInteractiveOwner(client, paths, threadID, false); err != nil || staleOwner != nil {
		t.Fatal(err)
	}
	select {
	case action := <-actions:
		if action != "release_stale_interactive" {
			t.Fatalf("release action = %q", action)
		}
	case <-time.After(time.Second):
		t.Fatal("loaded exited owner was not synchronously released")
	}
}

func TestPreparedResumeDoesNotReleaseUncorroboratedLoadedOwner(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000123"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{threadID}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", socket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	if err := writeJSONAtomic(interactiveOwnerPath(paths, threadID), interactiveOwnerRecord{
		ThreadID: threadID, RequestID: "unknown-owner", OwnerPID: os.Getpid(),
		OwnerProcStart: "", UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	client, err := dialLaunchAppServer(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if _, err := releaseStaleLoadedInteractiveOwner(client, paths, threadID, false); err == nil || !strings.Contains(err.Error(), "cannot currently corroborate") {
		t.Fatalf("unknown loaded owner release error = %v", err)
	}
	if owner := readInteractiveOwner(paths, threadID); owner == nil || owner.RequestID != "unknown-owner" {
		t.Fatalf("unknown loaded owner was mutated: %#v", owner)
	}
}

func TestPreparedResumeNameResolutionUsesNewestIndexedRollout(t *testing.T) {
	root := t.TempDir()
	firstID := "00000000-0000-0000-0000-000000000120"
	secondID := "00000000-0000-0000-0000-000000000121"
	name := "duplicate-index-resume"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/read":
			params, _ := request["params"].(map[string]any)
			return map[string]any{"thread": map[string]any{
				"id": params["threadId"], "name": name, "cwd": root, "source": "cli",
			}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	older, _ := json.Marshal(map[string]any{
		"id": firstID, "thread_name": name, "updated_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	})
	newer, _ := json.Marshal(map[string]any{
		"id": secondID, "thread_name": name, "updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	contents := append(append(older, '\n'), append(newer, '\n')...)
	if err := os.WriteFile(filepath.Join(root, "codex", "session_index.jsonl"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	started := readProcStart(os.Getpid())
	got, _, err := bindPreparedResumeNative([]string{
		"--target", name, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", started,
	})
	if err != nil || got != secondID {
		t.Fatalf("newest indexed resume = %q, %v", got, err)
	}
}

func TestPreparedResumeFindsZeroTurnNameThroughExactIndexRead(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000118"
	name := "zero-turn-resume"
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": threadID, "name": name, "cwd": root, "source": "cli"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	setPreparedLaunchTestEnv(t, root, socket)
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]any{"id": threadID, "thread_name": name, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(filepath.Join(root, "codex", "session_index.jsonl"), append(row, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	started := readProcStart(os.Getpid())
	got, _, err := bindPreparedResumeNative([]string{
		"--target", name, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()), "--owner-proc-start", started,
	})
	if err != nil || got != threadID {
		t.Fatalf("zero-turn name bind = %q, %v", got, err)
	}
}

func TestPreparedResumeKeepsIndexedNameAfterPromptReplacesTitle(t *testing.T) {
	root := t.TempDir()
	threadID := "00000000-0000-0000-0000-000000000128"
	name := "used-named-peer"
	var published map[string]any
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{
				"id": threadID, "name": "first real prompt", "cwd": root, "source": "cli",
			}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	setPreparedLaunchTestEnv(t, root, socket, func(request map[string]any) map[string]any {
		if stringValue(request["action"]) == "register_prepared" {
			published = request
			return map[string]any{"state": map[string]any{"sessionId": threadID}}
		}
		return map[string]any{}
	})
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]any{
		"id": threadID, "thread_name": name, "updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err := os.WriteFile(filepath.Join(root, "codex", "session_index.jsonl"), append(row, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	got, _, err := bindPreparedResumeNative([]string{
		"--target", name, "--cwd", root, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--owner-proc-start", readProcStart(os.Getpid()),
	})
	if err != nil || got != threadID {
		t.Fatalf("used named resume = %q, %v", got, err)
	}
	owner := readInteractiveOwner(resolveNativePaths(), threadID)
	if owner == nil || owner.Name != name || owner.NameSource != "codex" || stringValue(published["name"]) != name {
		t.Fatalf("indexed name was not preserved: owner=%#v publication=%#v", owner, published)
	}
}

func setPreparedLaunchTestEnv(t *testing.T, root, socket string, handlers ...func(map[string]any) map[string]any) {
	t.Helper()
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", socket)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	supervisorSocket := startAuthorizationControlServer(t, func(request map[string]any) map[string]any {
		if len(handlers) > 0 {
			return handlers[0](request)
		}
		switch stringValue(request["action"]) {
		case "register_prepared":
			commitPreparedOwnerForTest(paths, stringValue(request["sessionId"]))
			return map[string]any{"state": map[string]any{"sessionId": stringValue(request["sessionId"])}}
		case "abort_prepared":
			removeInteractiveOwnerIfMatching(paths, stringValue(request["sessionId"]), nil)
			return map[string]any{"aborted": true}
		default:
			return map[string]any{}
		}
	})
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)
}

func commitPreparedOwnerForTest(paths nativePaths, threadID string) {
	owner := readInteractiveOwner(paths, threadID)
	if owner == nil {
		return
	}
	owner.DeleteOnAbort = false
	owner.ParkOnAbort = true
	_ = writeJSONAtomic(interactiveOwnerPath(paths, threadID), owner)
}
