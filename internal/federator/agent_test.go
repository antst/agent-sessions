package federator

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnreadyDiscoveredQwenLauncherIsNotAdvertised(t *testing.T) {
	bin := t.TempDir()
	launcher := filepath.Join(bin, "qwen-peer-lane")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	previous := evaluateQwenLaneReadiness
	evaluateQwenLaneReadiness = func(got string) error {
		if got != launcher {
			t.Fatalf("Qwen readiness executable = %q, want %q", got, launcher)
		}
		return errors.New("Qwen profile is not ready")
	}
	t.Cleanup(func() { evaluateQwenLaneReadiness = previous })
	options := AgentOptions{EnableRemoteLanes: true}
	if err := configureLaneExecutables(&options); err != nil {
		t.Fatalf("implicit unready Qwen launcher should be omitted, not fatal: %v", err)
	}
	if options.QwenLaneExecutable != "" {
		t.Fatalf("unready Qwen launcher remained configured: %q", options.QwenLaneExecutable)
	}
	agent := &agent{options: options}
	if hostHasCapability(Host{Capabilities: agent.laneCapabilities()}, CapabilityQwenLane) {
		t.Fatal("unready Qwen capability was advertised")
	}
}
