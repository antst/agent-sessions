package bridge

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGrokNativeLeaderBootstrapOwnsTheOneLeaderArgv(t *testing.T) {
	root := t.TempDir()
	bootstrap, err := NewGrokNativeLeaderBootstrap(
		"/usr/bin/grok", root, filepath.Join(root, "leader.sock"), []string{"PATH=/bin"}, io.Discard,
		"default", []string{"--allow", "MCPTool(agent_sessions__*)"},
	)
	if err != nil {
		t.Fatal(err)
	}
	command := bootstrap.Command()
	want := []string{
		"/usr/bin/grok", "--permission-mode", "default", "--allow", "MCPTool(agent_sessions__*)", "agent", "leader", "--leader-socket",
		filepath.Join(root, "leader.sock"), "--relay-on-demand", "--no-auto-update",
	}
	if !reflect.DeepEqual(command.Args, want) || command.Dir != root || !reflect.DeepEqual(command.Env, []string{"PATH=/bin"}) {
		t.Fatalf("command args=%q dir=%q env=%q", command.Args, command.Dir, command.Env)
	}
	for _, argument := range command.Args {
		if argument == "--no-exit-on-disconnect" {
			t.Fatal("leader bootstrap restored orphan-preserving flag")
		}
	}
	if command.Stdout != io.Discard || command.Stderr != io.Discard {
		t.Fatal("leader diagnostics were not shared")
	}
	_ = os.Remove(filepath.Join(root, "leader.sock"))
}

func TestGrokNativeLeaderReceivesInvocationYoloInsteadOfDefaultPolicy(t *testing.T) {
	root := t.TempDir()
	bootstrap, err := NewGrokNativeLeaderBootstrap(
		"/usr/bin/grok", root, filepath.Join(root, "leader.sock"), []string{"PATH=/bin"}, io.Discard,
		"bypassPermissions", []string{"--allow", "MCPTool(agent_sessions__*)"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/grok", "--permission-mode", "bypassPermissions", "agent", "leader", "--leader-socket",
		filepath.Join(root, "leader.sock"), "--relay-on-demand", "--no-auto-update",
	}
	if got := bootstrap.Command().Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("yolo leader argv = %q, want %q", got, want)
	}
}

func TestGrokNativeSessionTitleUsesGlobalNoLeaderRoster(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wrapper := filepath.Join(root, "grok")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$GROK_FAKE_TEST_BINARY\" -test.run='^TestGrokFakeProcess$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "requests.jsonl")
	environment := append(os.Environ(),
		"AGENT_SESSIONS_GROK_FAKE_PROCESS=1",
		"GROK_FAKE_TEST_BINARY="+testBinary,
		"GROK_FAKE_RECORD="+record,
		"GROK_FAKE_GENERATED_SESSION_ID="+testGrokNativeID,
		"GROK_FAKE_SESSION_TITLE=global lane",
		"GROK_FAKE_ACTIVITY=dormant",
	)
	name, ok := GrokNativeSessionTitle(context.Background(), wrapper, "", environment, io.Discard, testGrokNativeID)
	if !ok || name != "global lane" {
		t.Fatalf("global Grok title = %q, ok=%v", name, ok)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	requests := string(body)
	if !strings.Contains(requests, `"--no-leader"`) || strings.Contains(requests, `"--leader-socket"`) ||
		!strings.Contains(requests, `"method":"_x.ai/sessions/list"`) {
		t.Fatalf("global roster trace = %s", requests)
	}
}
