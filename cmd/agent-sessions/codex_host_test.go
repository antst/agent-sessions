package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
)

func TestCodexNativeReloadsDaemonWideMCPInventoryOncePerGeneration(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	want := &bridge.CodexNative{}
	openCalls := 0
	reloadCalls := 0
	coordinator.openCodex = func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error) {
		openCalls++
		return want, nil
	}
	coordinator.reloadCodex = func(_ context.Context, got *bridge.CodexNative) error {
		reloadCalls++
		if got != want {
			t.Fatalf("reloaded native client = %p, want %p", got, want)
		}
		return nil
	}

	first, err := coordinator.codexNative()
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.codexNative()
	if err != nil {
		t.Fatal(err)
	}
	if first != want || second != want || openCalls != 1 || reloadCalls != 1 {
		t.Fatalf("Codex native generation = first %p second %p opens %d reloads %d", first, second, openCalls, reloadCalls)
	}
}

func TestCodexDetachReleasesNativeSubscriptionBeforeLocalBookkeeping(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	threadID := "00000000-0000-0000-0000-00000000d001"
	coordinator.pending[threadID] = daemonpkg.NativeEvidence{}
	coordinator.monitored[threadID] = true
	called := ""
	coordinator.unsubscribeCodex = func(_ context.Context, got string) error {
		called = got
		return nil
	}
	adapter := coordinator.adapters()["codex"]
	if err := adapter.Detach(context.Background(), daemonpkg.ManagedAttachment{
		ID: threadID, Product: "codex", NativeSessionID: threadID,
	}); err != nil {
		t.Fatal(err)
	}
	if called != threadID {
		t.Fatalf("unsubscribed thread = %q, want %q", called, threadID)
	}
	if _, ok := coordinator.pending[threadID]; ok || coordinator.monitored[threadID] {
		t.Fatal("Codex detach retained process-local bookkeeping")
	}
}

func TestCodexResumeUsesInvocationCwdWhenRecordedWorkspaceMoved(t *testing.T) {
	current := t.TempDir()
	removed := filepath.Join(t.TempDir(), "removed-workspace")
	request := launcher.CodexDaemonPrepareRequest{Cwd: current, CwdExplicit: false}
	thread := bridge.CodexNativeThread{Cwd: removed}

	if got := codexResumeCwd(request, thread); got != current {
		t.Fatalf("resume cwd = %q, want invocation cwd %q", got, current)
	}
}

func TestCodexManagedResumeInheritsStableIntentWithoutOverridingExplicitFields(t *testing.T) {
	selected := daemonpkg.ManagedAttachment{
		ID: "native", Product: "codex", ProfileIdentity: codexHome(),
		NativeSessionID: "native", Cwd: "/original",
		Groups: []string{"group-a", "group-b"}, LaunchIntent: "yolo", PermissionMode: "bypassPermissions",
		State: "detached",
	}
	inherited := inheritCodexResumeRequest(launcher.CodexDaemonPrepareRequest{}, selected)
	if inherited.Name != "" || strings.Join(inherited.Groups, ",") != "group-a,group-b" || inherited.ApprovalPolicy != "never" ||
		inherited.Sandbox != "danger-full-access" {
		t.Fatalf("inherited Codex resume = %+v", inherited)
	}
	explicit := launcher.CodexDaemonPrepareRequest{
		Groups: []string{"override-group"}, GroupsSpecified: true,
		ApprovalPolicy: "", Sandbox: "", PermissionSpecified: true,
	}
	explicit = inheritCodexResumeRequest(explicit, selected)
	if strings.Join(explicit.Groups, ",") != "override-group" || explicit.ApprovalPolicy != "" || explicit.Sandbox != "" {
		t.Fatalf("explicit Codex resume was overwritten: %+v", explicit)
	}
}
