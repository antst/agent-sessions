package servicecontrol

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDarwinUserAgentUsesExactDomainLabelAndPlist(t *testing.T) {
	runner := &recordingRunner{}
	service := NewDarwinForUser(runner, 501, "/Users/tester")
	for _, operation := range []func(context.Context) error{service.Enable, service.Restart, service.Stop, service.Start, service.Disable} {
		if err := operation(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	target := "gui/501/" + darwinLabel
	plist := "/Users/tester/Library/LaunchAgents/" + darwinLabel + ".plist"
	want := []recordedCommand{
		{name: "launchctl", arguments: []string{"bootstrap", "gui/501", plist}},
		{name: "launchctl", arguments: []string{"enable", target}},
		{name: "launchctl", arguments: []string{"kickstart", "-k", target}},
		{name: "launchctl", arguments: []string{"bootout", target}},
		{name: "launchctl", arguments: []string{"bootstrap", "gui/501", plist}},
		{name: "launchctl", arguments: []string{"enable", target}},
		{name: "launchctl", arguments: []string{"kickstart", "-k", target}},
		{name: "launchctl", arguments: []string{"bootout", target}},
		{name: "launchctl", arguments: []string{"disable", target}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("launchd commands = %#v, want %#v", runner.commands, want)
	}
	for _, command := range runner.commands {
		joined := strings.Join(command.arguments, " ")
		if strings.Contains(joined, "agent-sessions-hub") || strings.Contains(joined, "com.apple") {
			t.Fatalf("launchd operation touched unrelated agent: %q", joined)
		}
	}
}

func TestDarwinUpgradeValidatesBeforeBootoutAndPreservesLoadedAgentOnFailure(t *testing.T) {
	runner := &recordingRunner{}
	service := NewDarwinForUser(runner, 502, "/Users/release")
	sentinel := errors.New("candidate invalid")
	if err := service.Upgrade(context.Background(), func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("failed validation error = %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("failed validation mutated loaded agent: %#v", runner.commands)
	}
	if err := service.Upgrade(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 4 || runner.commands[0].arguments[0] != "bootout" || runner.commands[1].arguments[0] != "bootstrap" {
		t.Fatalf("validated upgrade sequence = %#v", runner.commands)
	}
}

func TestDarwinServiceAssetRunsAtLoadSurvivesWakeAndTargetsOnlyDaemon(t *testing.T) {
	body := readRepositoryAsset(t, "deploy/agent-sessions/launchd/net.antst.agent-sessions.plist")
	for _, literal := range []string{
		"<key>Label</key>", darwinLabel, "<key>RunAtLoad</key>", "<key>KeepAlive</key>",
		"<key>WorkingDirectory</key>", "@HOME@", "<key>EnvironmentVariables</key>",
		"@SERVICE_PATH@", "CODEX_PEER_CODEX_BIN", "@CODEX_BIN@",
		"CLAUDE_PEER_CLAUDE_BIN", "@CLAUDE_BIN@", "GROK_PEER_GROK_BIN", "@GROK_BIN@",
		"QWEN_PEER_QWEN_BIN", "@QWEN_BIN@", "agent-sessions", "daemon",
	} {
		if !strings.Contains(body, literal) {
			t.Errorf("launchd asset omits %q", literal)
		}
	}
	for _, forbidden := range []string{"agent-sessions-hub", "killall", "pkill"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("launchd asset contains unrelated/destructive directive %q", forbidden)
		}
	}
}
