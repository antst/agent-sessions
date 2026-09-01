package federator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnreadyDiscoveredQwenLauncherIsNotAdvertised(t *testing.T) {
	bin := t.TempDir()
	launcher := filepath.Join(bin, "qwen-peer-lane")
	native := filepath.Join(bin, "qwen")
	for _, executable := range []string{launcher, native} {
		if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("QWEN_PEER_QWEN_BIN", "")
	previous := evaluateQwenLaneReadiness
	evaluateQwenLaneReadiness = func(got string) error {
		if got != native {
			t.Fatalf("Qwen readiness executable = %q, want native %q (lane launcher %q)", got, native, launcher)
		}
		return errors.New("qwen profile is not ready")
	}
	t.Cleanup(func() { evaluateQwenLaneReadiness = previous })
	options := AgentOptions{EnableRemoteLanes: true}
	if err := configureLaneExecutables(&options); err != nil {
		t.Fatalf("implicit unready Qwen launcher should be omitted, not fatal: %v", err)
	}
	if options.QwenLaneExecutable != "" {
		t.Fatalf("unready Qwen launcher remained configured: %q", options.QwenLaneExecutable)
	}
	if options.QwenExecutable != "" {
		t.Fatalf("unready native Qwen executable remained configured: %q", options.QwenExecutable)
	}
	agent := &agent{options: options}
	if hostHasCapability(Host{Capabilities: agent.laneCapabilities()}, CapabilityQwenLane) {
		t.Fatal("unready Qwen capability was advertised")
	}
}

func TestConfiguredQwenLauncherRequiresSeparateNativeExecutable(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "qwen-peer-lane")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QWEN_PEER_QWEN_BIN", "")
	options := AgentOptions{EnableRemoteLanes: true, QwenLaneExecutable: launcher}
	if err := configureLaneExecutables(&options); err == nil ||
		!strings.Contains(err.Error(), "no executable native Qwen client") {
		t.Fatalf("configured Qwen lane without native executable error = %v", err)
	}
}
