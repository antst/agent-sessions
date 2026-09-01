package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestQwenDurableOwnerHelper(t *testing.T) {
	if os.Getenv("AGENT_SESSIONS_QWEN_DURABLE_HELPER") != "1" {
		t.Skip("helper process")
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestQwenManagedResumeInheritsStableIntentWithoutOverridingExplicitFields(t *testing.T) {
	selected := daemonpkg.ManagedAttachment{
		ID: "native", NativeSessionID: "native", Cwd: "/original",
		Groups: []string{"group-a", "group-b"}, LaunchIntent: "yolo", PermissionMode: "bypassPermissions",
	}
	inherited := inheritQwenResumeRequest(launcher.QwenDaemonPrepareRequest{}, selected)
	if inherited.SessionID != "native" || inherited.Name != "" || inherited.Cwd != "/original" ||
		strings.Join(inherited.Groups, ",") != "group-a,group-b" || inherited.LaunchPreference != "yolo" ||
		inherited.ExpectedInitialMode != "yolo" {
		t.Fatalf("inherited Qwen resume = %+v", inherited)
	}
	explicit := inheritQwenResumeRequest(launcher.QwenDaemonPrepareRequest{
		Name: "override", NameSpecified: true, Groups: []string{"override-group"}, GroupsSpecified: true,
		LaunchPreference: "non_yolo", ExpectedInitialMode: "default", PermissionSpecified: true,
	}, selected)
	if explicit.Name != "override" || strings.Join(explicit.Groups, ",") != "override-group" ||
		explicit.LaunchPreference != "non_yolo" || explicit.ExpectedInitialMode != "default" {
		t.Fatalf("explicit Qwen resume was overwritten: %+v", explicit)
	}
}

func TestDurableQwenRefreshAllowsAppendAndRejectsReplacement(t *testing.T) {
	stateRoot := t.TempDir()
	sessionID := "qwen-durable-session"
	root := filepath.Join(stateRoot, "native", "qwen", sessionID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inputPath, eventsPath := filepath.Join(root, "input.jsonl"), filepath.Join(root, "events.jsonl")
	for _, path := range []string{inputPath, eventsPath} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	input, err := federator.QwenArtifactAttestationForPath(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := federator.QwenArtifactAttestationForPath(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestQwenDurableOwnerHelper$", "--", inputPath, eventsPath, sessionID)
	command.Env = append(os.Environ(), "AGENT_SESSIONS_QWEN_DURABLE_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	var owner procinfo.Identity
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info := procinfo.Read(command.Process.Pid)
		if info.Status == procinfo.Known && info.Start != "" && info.StrongStart != "" {
			owner = procinfo.Identity{PID: command.Process.Pid, Start: info.Start, StrongStart: info.StrongStart}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if owner.PID == 0 {
		t.Fatal("helper process identity was not observable")
	}

	legacy := daemonpkg.ManagedAttachment{
		ID: sessionID, Product: "qwen", NativeSessionID: sessionID,
		Evidence: daemonpkg.NativeEvidence{
			Process: owner, ThreadID: sessionID, ArtifactPath: inputPath, RegistryPath: eventsPath,
			ArtifactRevision: input.Fingerprint + ":" + events.Fingerprint,
		},
	}
	if err := os.WriteFile(inputPath, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshed, err := refreshDurableQwenAttachment(stateRoot, legacy)
	if err != nil {
		t.Fatalf("refresh after append: %v", err)
	}
	if refreshed.ArtifactDevice == 0 || refreshed.ArtifactInode == 0 ||
		refreshed.RegistryDevice == 0 || refreshed.RegistryInode == 0 {
		t.Fatalf("refresh did not persist stable artifact identity: %+v", refreshed)
	}
	legacy.Evidence = refreshed
	coordinator := newHostCoordinator(context.Background(), stateRoot)
	if err := coordinator.submitQwenMessage(legacy, "QWEN_RESTART_INPUT_OK"); err != nil {
		t.Fatalf("re-open durable Qwen input after daemon restart: %v", err)
	}
	inputBody, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inputBody), `"type":"submit"`) || !strings.Contains(string(inputBody), `"text":"QWEN_RESTART_INPUT_OK"`) {
		t.Fatalf("re-opened Qwen input = %q", inputBody)
	}

	if err := os.Remove(inputPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := refreshDurableQwenAttachment(stateRoot, legacy); err == nil {
		t.Fatal("refresh accepted a same-path artifact replacement")
	}
}
