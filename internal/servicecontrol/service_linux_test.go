package servicecontrol

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLinuxUserServiceUsesExactUnitForEnableAndLifecycle(t *testing.T) {
	runner := &recordingRunner{}
	service := NewLinux(runner)
	for _, operation := range []func(context.Context) error{service.Enable, service.Start, service.Stop, service.Restart, service.Disable} {
		if err := operation(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	want := []recordedCommand{
		{name: "systemctl", arguments: []string{"--user", "daemon-reload"}},
		{name: "systemctl", arguments: []string{"--user", "enable", linuxUnit}},
		{name: "systemctl", arguments: []string{"--user", "start", linuxUnit}},
		{name: "systemctl", arguments: []string{"--user", "stop", linuxUnit}},
		{name: "systemctl", arguments: []string{"--user", "restart", linuxUnit}},
		{name: "systemctl", arguments: []string{"--user", "stop", linuxUnit}},
		{name: "systemctl", arguments: []string{"--user", "disable", linuxUnit}},
		{name: "systemctl", arguments: []string{"--user", "daemon-reload"}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("systemd commands = %#v, want %#v", runner.commands, want)
	}
	for _, command := range runner.commands {
		joined := strings.Join(command.arguments, " ")
		if strings.Contains(joined, "agent-sessions-hub") || strings.Contains(joined, "claude") || strings.Contains(joined, "codex") {
			t.Fatalf("systemd operation touched unrelated service: %q", joined)
		}
	}
}

func TestLinuxUpgradeValidatesBeforeReloadAndPreservesRunningServiceOnFailure(t *testing.T) {
	runner := &recordingRunner{}
	service := NewLinux(runner)
	sentinel := errors.New("candidate invalid")
	if err := service.Upgrade(context.Background(), func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("failed validation error = %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("failed validation mutated service: %#v", runner.commands)
	}
	if err := service.Upgrade(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []recordedCommand{
		{name: "systemctl", arguments: []string{"--user", "daemon-reload"}},
		{name: "systemctl", arguments: []string{"--user", "restart", linuxUnit}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("upgrade commands = %#v, want %#v", runner.commands, want)
	}
}

func TestLinuxServiceAssetStopsTheWholeDaemonOwnedProcessTree(t *testing.T) {
	body := readRepositoryAsset(t, "deploy/agent-sessions/systemd/user/agent-sessions.service")
	for _, literal := range []string{"Type=simple", "Restart=on-failure", "agent-sessions daemon"} {
		if !strings.Contains(body, literal) {
			t.Errorf("systemd asset omits %q", literal)
		}
	}
	for _, forbidden := range []string{"KillMode=process", "pkill", "killall", "agent-sessions-hub"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("systemd asset contains unrelated/destructive directive %q", forbidden)
		}
	}
}

func TestLinuxHubAssetUsesPerUserHubEnvironmentWithoutAddingNetworkTrust(t *testing.T) {
	body := readRepositoryAsset(t, "deploy/agent-sessions-hub/systemd/user/agent-sessions-hub.service")
	for _, literal := range []string{
		"Type=simple", "EnvironmentFile=-%h/.config/agent-sessions/hub.env",
		"agent-sessions-hub", "Restart=on-failure",
	} {
		if !strings.Contains(body, literal) {
			t.Errorf("systemd hub asset omits %q", literal)
		}
	}
	for _, forbidden := range []string{"TLS", "AUTH_TOKEN", "CODEX_PEER_CODEX_BIN", "QWEN_PEER_QWEN_BIN"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("systemd hub asset contains out-of-scope or host-only setting %q", forbidden)
		}
	}
}
