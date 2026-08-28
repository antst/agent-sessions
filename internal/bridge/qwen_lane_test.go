package bridge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQwenLaneUsageAdvertisesEveryParsedSharedAndPermissionOption(t *testing.T) {
	usage := qwenLaneUsage()
	for _, option := range []string{
		"-g, --group", "--inherit-groups", "--no-inherit-groups", "--qwen-home",
		"--yolo", "--no-yolo", "--approval-mode", "--persistent", "--notify",
		"--no-notify", "--auto-archive-after", "--no-auto-archive",
	} {
		if !strings.Contains(usage, option) {
			t.Errorf("usage omits %s", option)
		}
	}
}

func TestParseQwenLaneArgsPermissionAndGroupContract(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		preference string
		mode       string
		groups     []string
		wantErr    string
	}{
		{name: "native default", args: []string{"start", "--name", "q", "-g", "one"}, preference: "native_default", groups: []string{"one"}},
		{name: "no yolo", args: []string{"start", "--name", "q", "--no-yolo"}, preference: "non_yolo", mode: "default"},
		{name: "yolo", args: []string{"start", "--name", "q", "--yolo"}, preference: "yolo", mode: "yolo"},
		{name: "native plan", args: []string{"start", "--name", "q", "--approval-mode", "plan", "--group", "one", "-g", "two"}, preference: "native:plan", mode: "plan", groups: []string{"one", "two"}},
		{name: "wrapper conflict", args: []string{"start", "--name", "q", "--yolo", "--no-yolo"}, wantErr: "repeated or contradictory"},
		{name: "wrapper native conflict", args: []string{"start", "--name", "q", "--yolo", "--approval-mode", "yolo"}, wantErr: "repeated or contradictory"},
		{name: "repeated native", args: []string{"start", "--name", "q", "--approval-mode", "plan", "--approval-mode", "plan"}, wantErr: "repeated or contradictory"},
		{name: "unknown mode", args: []string{"start", "--name", "q", "--approval-mode", "future"}, wantErr: "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseQwenLaneArgs(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parsed.launchPreference != test.preference || parsed.permissionMode != test.mode ||
				strings.Join(parsed.groupOptions.groups, ",") != strings.Join(test.groups, ",") {
				t.Fatalf("parsed = %+v", parsed)
			}
		})
	}
}

func TestParseQwenLaneArgsSelectorsAndExitContract(t *testing.T) {
	for _, args := range [][]string{
		{"resume", "thread", "--qwen-home", "/tmp/qwen"},
		{"wait", "thread", "--timeout", "1.5"},
		{"status", "thread"}, {"interrupt", "thread"}, {"archive", "thread"},
		{"list", "--all"}, {"list", "--mine"}, {"doctor", "--json"},
	} {
		if _, err := parseQwenLaneArgs(args); err != nil {
			t.Errorf("parse %q: %v", args, err)
		}
	}
	options, err := parseQwenLaneArgs([]string{"start", "--name", "q", "--yolo", "--no-yolo"})
	if err == nil {
		err = validateQwenLaneCommandOptions(options)
	}
	if err == nil {
		t.Fatal("usage conflict was accepted")
	}
}

func TestQwenLaneStateSelectionStatusAndExactlyOnceCollection(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "data"), runtimeDir: filepath.Join(root, "runtime"), claudeRoot: filepath.Join(root, "claude"), codexHome: filepath.Join(root, "codex")}
	if err := os.MkdirAll(filepath.Join(profileDataRoot(paths), "qwen-lanes"), 0o700); err != nil {
		t.Fatal(err)
	}
	threadID := "11111111-2222-4333-8444-555555555555"
	turn := newQwenLaneTurn("return ok")
	turn.Status, turn.Result, turn.Outcome, turn.TerminalRevision = "completed", "ok", "completed", randomID()
	turn.CompletedAt = time.Now().UnixMilli()
	state := qwenLaneState{
		Version: 1, ContractVersion: 1, Type: "qwen-peer-lane", Name: "qwen-test", ThreadID: threadID,
		Cwd: root, Status: "idle", LaunchPreference: "native_default", CurrentNativeMode: "unknown",
		NativeArchiveState: "active", Turns: []qwenLaneTurn{turn}, LatestTurnID: turn.ID,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := writeQwenLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveQwenLaneState(paths, "qwen-test")
	if err != nil || resolved.ThreadID != threadID {
		t.Fatalf("resolve = %+v, %v", resolved, err)
	}
	status := qwenLaneStatusEvent(resolved)
	if status["product"] != "qwen" || status["current_native_mode"] != "unknown" {
		t.Fatalf("status = %#v", status)
	}
	if err := acknowledgeQwenLaneTurn(paths, threadID, turn.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := readQwenLaneState(paths, threadID)
	if err != nil || !updated.Turns[0].Collected || updated.CollectedTurnID != turn.ID {
		t.Fatalf("updated = %+v, %v", updated, err)
	}
	if debt := firstQwenLaneDebt(updated); debt != "" {
		t.Fatalf("collection debt = %s", debt)
	}
}

func TestQwenLaneJSONEventsRemainMachineReadable(t *testing.T) {
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	if err := emitQwenLaneReady(qwenLaneState{
		Name: "q", ThreadID: "11111111-2222-4333-8444-555555555555", Cwd: "/tmp", MessagingSocket: "/tmp/q.sock",
		LaunchPreference: "native:plan", InitialNativeMode: "plan", CurrentNativeMode: "plan",
	}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	os.Stdout = old
	var output bytes.Buffer
	_, _ = output.ReadFrom(reader)
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil || event["product"] != "qwen" || event["initial_native_mode"] != "plan" {
		t.Fatalf("event = %q, %#v, %v", output.String(), event, err)
	}
}

func TestQwenLaneTerminalNoticeIsExactAndDeduplicated(t *testing.T) {
	threadID, turnID := randomID(), randomID()
	state := qwenLaneState{
		Name: "qwen-notice", ThreadID: threadID, NotifyTarget: "session:parent-session",
		ParentHostID: "source-host", ParentAgentRuntimeDir: "/private/runtime",
		Groups: []string{
			"shared", "session:source-host/parent-session", "session:destination-host/" + threadID,
		},
	}
	turn := qwenLaneTurn{ID: turnID, Status: "completed", Outcome: "completed", Exit: 0}
	queueQwenLaneTerminalNotice(&state, turn)
	queueQwenLaneTerminalNotice(&state, turn)
	if len(state.Notices) != 1 {
		t.Fatalf("terminal notices = %+v", state.Notices)
	}
	notice := state.Notices[0]
	wantedCollect := "agent-sessions lane --host destination-host --product qwen -- wait " + threadID
	if notice.TurnID != turnID || notice.Target != "session:parent-session" ||
		!strings.Contains(notice.Message, "status=completed outcome=completed exit=0") ||
		!strings.Contains(notice.Message, "Collect: "+wantedCollect) {
		t.Fatalf("terminal notice = %+v", notice)
	}
}
