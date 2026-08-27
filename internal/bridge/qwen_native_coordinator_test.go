package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

func TestQwenNativeActorAdmitsDeliversAndReconstructsWithoutHostProcess(t *testing.T) {
	root := t.TempDir()
	sessionID := "66666666-6666-4666-8666-666666666666"
	actorRoot := filepath.Join(root, "runtime", "qwen-actor")
	actor := &qwenDaemonActor{
		coordinatorID: "qwen-coordinator-test", sessionID: sessionID, name: "daemon-qwen",
		cwd: root, profile: qwenprofile.Identity{Fingerprint: "profile-qwen"}, version: "0.22.0",
		actorRoot: actorRoot, inputPath: filepath.Join(actorRoot, "input.jsonl"),
		eventPath: filepath.Join(actorRoot, "events.jsonl"), recordPath: filepath.Join(root, "state", "actor.json"),
	}
	if err := actor.prepare(); err != nil {
		t.Fatalf("prepare daemon Qwen actor: %v", err)
	}
	event, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "session_start", "session_id": sessionID,
		"data": map[string]any{
			"session_id": sessionID, "cwd": root, "version": "0.22.0", "protocol_version": 2,
			"supported_events": qwenRequiredDualOutputEvents(),
		},
	})
	if err := os.WriteFile(actor.eventPath, append(event, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := procinfo.Read(os.Getpid())
	actor.mu.Lock()
	actor.ownerPID, actor.ownerProcStart, actor.parentPID = os.Getpid(), identity.Start, identity.Parent
	cursor, _, err := admitQwenDualOutput(actor.eventPath, qwenAdmissionExpectation{
		SessionID: sessionID, Cwd: root, Version: "0.22.0", ProtocolVersion: 2,
		RequiredEvents: qwenRequiredDualOutputEvents(),
	})
	if err == nil {
		actor.cursor = cursor
		actor.input, err = openQwenInputWriter(actor.inputPath)
	}
	if err == nil {
		actor.ready = true
		err = actor.persist(true)
	}
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("admit daemon Qwen actor: %v", err)
	}
	appendQwenDaemonEvents(t, actor.eventPath,
		map[string]any{"type": "user"},
		map[string]any{"type": "current_mode_update", "current_mode_id": "yolo"},
	)
	actor.mu.Lock()
	err = actor.observeAvailableEventsLocked()
	observed := actor.sessionLocked()
	actor.mu.Unlock()
	if err != nil || observed.Status != "busy" || observed.PermissionMode != "yolo" {
		t.Fatalf("observe Qwen status/mode: session=%+v err=%v", observed, err)
	}
	plan := actor.launchPlan(daemonpkg.AttachmentPrepareRequest{Intent: daemonpkg.InteractiveLaunchIntent{
		Mode: "fresh", NativeArguments: []string{"--model", "qwen-test"},
	}}, "/usr/bin/qwen")
	if plan.Executable != "/usr/bin/qwen" || plan.SessionID != "" || strings.Contains(strings.Join(plan.Arguments, " "), "qwen-host") {
		t.Fatalf("Qwen direct launch plan = %+v", plan)
	}
	frame := federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "qwen-daemon-message", Content: "hello from daemon",
	}
	if err := actor.writeFrame(frame); err != nil {
		t.Fatalf("write Qwen daemon frame: %v", err)
	}
	inputBody, err := os.ReadFile(actor.inputPath)
	if err != nil || !strings.Contains(string(inputBody), federation.AgentFrameCarrierPrefix) || !strings.Contains(string(inputBody), frame.MessageID) {
		t.Fatalf("Qwen native input body missing frame: body=%q err=%v", inputBody, err)
	}
	actor.suspend()
	recovered := recoverQwenDaemonActor(actor.recordPath)
	if recovered == nil {
		t.Fatal("recover exact daemon Qwen actor = nil")
	}
	if session, err := recovered.inspect(); err != nil || session.SessionID != sessionID || !session.DualOutput ||
		session.Status != "busy" || session.PermissionMode != "yolo" {
		t.Fatalf("reconstruct daemon Qwen actor: session=%+v err=%v", session, err)
	}
	recovered.retire()
	if _, err := os.Lstat(actor.inputPath); !os.IsNotExist(err) {
		t.Fatalf("Qwen input survived exact retirement: %v", err)
	}
	if _, err := os.Lstat(actor.eventPath); !os.IsNotExist(err) {
		t.Fatalf("Qwen event stream survived exact retirement: %v", err)
	}
}

func appendQwenDaemonEvents(t *testing.T, path string, events ...map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		body, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, writeErr := file.Write(append(body, '\n')); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestQwenDaemonProfilePreservesEnvironmentPresence(t *testing.T) {
	profile, err := qwenDaemonProfile(map[string]any{
		"profile": "fingerprint", "qwen_home_set": true, "qwen_home": "/profile",
		"qwen_runtime_dir_set": true, "qwen_runtime_dir": "/runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	environment := qwenDaemonEnvironment(profile)
	if environment["QWEN_HOME"] != "/profile" || environment["QWEN_RUNTIME_DIR"] != "/runtime" {
		t.Fatalf("Qwen daemon environment = %#v", environment)
	}
}

func TestQwenDaemonReadinessFailureIsActionable(t *testing.T) {
	err := qwenDaemonReadinessError(qwenreadiness.Report{Issues: []qwenreadiness.Issue{{
		Code: "integration_identity", Message: "extension version does not match",
	}}})
	if !strings.Contains(err.Error(), "integration_identity") {
		t.Fatalf("Qwen readiness error = %v", err)
	}
}
