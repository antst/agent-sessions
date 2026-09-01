package main

import (
	"context"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
)

func TestResolveGrokDaemonResumeUsesExactDetachedIDAndRejectsNamesOrLiveMatch(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "34e92a86-b699-43a8-8f3d-342abf528bf5"
	snapshot.Catalog.Attachments[sessionID] = daemonpkg.ManagedAttachment{
		ID: sessionID, Product: "grok", ProfileIdentity: grokProfileRoot(),
		NativeSessionID: sessionID, Cwd: "/workspace",
		Groups: []string{"v2-live"}, PermissionMode: "bypassPermissions", State: "detached",
	}
	snapshot.Catalog.Attachments["live-grok-id"] = daemonpkg.ManagedAttachment{
		ID: "live-grok-id", Product: "grok", ProfileIdentity: grokProfileRoot(),
		NativeSessionID: "live-grok-id", Cwd: "/workspace", State: "preparing",
	}
	snapshot.Catalog.Attachments["other-cwd-id"] = daemonpkg.ManagedAttachment{
		ID: "other-cwd-id", Product: "grok", ProfileIdentity: grokProfileRoot(),
		NativeSessionID: "other-cwd-id", Cwd: "/other", State: "detached",
	}
	if _, err := runtime.State().Commit(snapshot.Revision, snapshot.Catalog); err != nil {
		t.Fatal(err)
	}

	selected, found, err := resolveGrokDaemonResume(runtime, launcher.GrokDaemonPrepareRequest{ResumeTarget: sessionID, Cwd: "/workspace"})
	if err != nil || !found || selected.NativeSessionID != sessionID {
		t.Fatalf("id resolution = %+v, found=%v, err=%v", selected, found, err)
	}
	if _, found, err := resolveGrokDaemonResume(runtime, launcher.GrokDaemonPrepareRequest{ResumeTarget: "native-only-title"}); err != nil || found {
		t.Fatalf("unknown native title = found=%v, err=%v", found, err)
	}

	if _, _, err := resolveGrokDaemonResume(runtime, launcher.GrokDaemonPrepareRequest{ResumeTarget: "live-grok-id", Cwd: "/workspace"}); err == nil {
		t.Fatal("live managed Grok session was accepted as a resume target")
	}
}
