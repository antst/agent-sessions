package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	claudeproduct "github.com/antst/agent-sessions/internal/products/claude"
	grokproduct "github.com/antst/agent-sessions/internal/products/grok"
	kiloproduct "github.com/antst/agent-sessions/internal/products/kilocode"
	ompproduct "github.com/antst/agent-sessions/internal/products/omp"
	opencodeproduct "github.com/antst/agent-sessions/internal/products/opencode"
	piproduct "github.com/antst/agent-sessions/internal/products/pi"
	qwenproduct "github.com/antst/agent-sessions/internal/products/qwen"
)

func TestHostCoordinatorComposesProductLaneDriversAndProductCandidateLookups(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	if _, ok := coordinator.laneDrivers.ByProduct(qwenproduct.ProductID); !ok {
		t.Fatal("Qwen lane driver is absent from the production registry")
	}
	if _, ok := coordinator.laneDrivers.ByProduct(claudeproduct.ProductID); !ok {
		t.Fatal("Claude lane driver is absent from the production registry")
	}
	if _, ok := coordinator.laneDrivers.ByProduct(grokproduct.ProductID); !ok || coordinator.candidateResolvers[grokproduct.ProductID] == nil {
		t.Fatal("Grok lane driver or product-global candidate resolver is absent from the production registry")
	}
	for _, product := range []struct {
		id          string
		environment string
	}{
		{id: opencodeproduct.ProductID, environment: "OPENCODE_PEER_OPENCODE_BIN"},
		{id: kiloproduct.ProductID, environment: "KILO_PEER_KILO_BIN"},
	} {
		t.Run(product.id, func(t *testing.T) {
			if _, ok := coordinator.laneDrivers.ByProduct(product.id); !ok {
				t.Fatalf("%s lane driver is absent from the production registry", product.id)
			}
			executable := filepath.Join(t.TempDir(), product.id)
			body := "#!/bin/sh\nprintf '%s\\n' '[{\"id\":\"ses_" + product.id + "\",\"title\":\"reviewer-" + product.id + "\",\"directory\":\"/work/" + product.id + "\",\"updated\":17}]'\n"
			if err := os.WriteFile(executable, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv(product.environment, executable)
			resolver := coordinator.candidateResolvers[product.id]
			entry, ok := resolver(context.Background(), daemonpkg.ManagedAttachment{}, daemonpkg.LaneCandidate{
				Product: product.id, NativeSessionID: "ses_" + product.id,
			})
			if !ok || entry.Name != "reviewer-"+product.id {
				t.Fatalf("product-confirmed candidate = %+v, ok=%v", entry, ok)
			}
			if _, ok := resolver(context.Background(), daemonpkg.ManagedAttachment{}, daemonpkg.LaneCandidate{
				Product: product.id, NativeSessionID: "ses_stale",
			}); ok {
				t.Fatalf("stale %s candidate was accepted without a product row", product.id)
			}
		})
	}
	for _, product := range []struct {
		id          string
		executable  string
		runtime     string
		scopeArg    string
		environment string
	}{
		{id: piproduct.ProductID, executable: "pi", runtime: "node", scopeArg: "$5", environment: "PI_PEER_PI_BIN"},
		{id: ompproduct.ProductID, executable: "omp", runtime: "bun", scopeArg: "$4", environment: "OMP_PEER_OMP_BIN"},
	} {
		t.Run(product.id, func(t *testing.T) {
			if _, ok := coordinator.laneDrivers.ByProduct(product.id); !ok {
				t.Fatalf("%s lane driver is absent from the production registry", product.id)
			}
			packageRoot := t.TempDir()
			binDir := filepath.Join(packageRoot, "bin")
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(binDir, product.executable)
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			toolsDir := t.TempDir()
			runtime := filepath.Join(toolsDir, product.runtime)
			body := "#!/bin/sh\n[ \"" + product.scopeArg + "\" = all ] || exit 23\nprintf '%s\\n' '[{\"id\":\"" + product.id + "-native\",\"title\":\"reviewer-" + product.id + "\",\"directory\":\"/work/" + product.id + "\",\"modified\":\"now\"}]'\n"
			if err := os.WriteFile(runtime, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv(product.environment, executable)
			resolver := coordinator.candidateResolvers[product.id]
			entry, ok := resolver(context.Background(), daemonpkg.ManagedAttachment{Cwd: t.TempDir()}, daemonpkg.LaneCandidate{
				Product: product.id, NativeSessionID: product.id + "-native",
			})
			if !ok || entry.Name != "reviewer-"+product.id {
				t.Fatalf("%s product-confirmed candidate = %+v, ok=%v", product.id, entry, ok)
			}
			if _, ok := resolver(context.Background(), daemonpkg.ManagedAttachment{Cwd: t.TempDir()}, daemonpkg.LaneCandidate{
				Product: product.id, NativeSessionID: product.id + "-stale",
			}); ok {
				t.Fatalf("stale %s candidate was accepted without a product row", product.id)
			}
		})
	}
}

func TestCodexLauncherOwnsPresenceForExactChildLifetime(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", stateRoot)
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	joined := make(chan liveSessionReport, 1)
	left := make(chan liveSessionReport, 1)
	server, err := startLivePresenceServer(serverCtx, stateRoot,
		func(report liveSessionReport) { joined <- report },
		func(report liveSessionReport) { left <- report }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()

	original := connectorNativeAdapters[connectorProductCodex]
	defer func() { connectorNativeAdapters[connectorProductCodex] = original }()
	delivered := make(chan liveSessionReport, 1)
	connectorNativeAdapters[connectorProductCodex] = func(_ context.Context, report liveSessionReport, _ string, body string) error {
		if !strings.Contains(body, "hello") || !strings.Contains(body, `from-session="parent"`) {
			t.Errorf("delivered body = %q", body)
		}
		delivered <- report
		return nil
	}

	launch := launcher.CodexNativeLaunch{
		Executable: "/bin/sh", Arguments: []string{"-c", "sleep 1"},
		ThreadID: "00000000-0000-0000-0000-00000000c001", Name: "reviewer", Groups: []string{"project"},
	}
	done := make(chan error, 1)
	go func() { done <- runCodexNativePeer(context.Background(), launch) }()

	select {
	case report := <-joined:
		if report.UUID != launch.ThreadID || report.Name != launch.Name || report.Product != "codex" || len(report.Groups) != 1 || report.Groups[0] != "project" {
			t.Fatalf("live report = %+v", report)
		}
		params, _ := json.Marshal(map[string]any{
			"message_id": "message-1", "body": "hello",
			"from": map[string]any{"uuid": "parent", "name": "parent", "product": "claude", "groups": []string{"project"}},
		})
		if _, err := server.Call(context.Background(), report.UUID, "test", "message.deliver", json.RawMessage(params)); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Codex child did not become live")
	}
	select {
	case report := <-delivered:
		if report.UUID != launch.ThreadID {
			t.Fatalf("delivery targeted %q", report.UUID)
		}
	case <-time.After(time.Second):
		t.Fatal("live delivery did not target the Codex thread")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case report := <-left:
		if report.UUID != launch.ThreadID {
			t.Fatalf("departed report = %+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("Codex presence outlived its child")
	}
}

func TestCodexNativeReloadsDaemonWideMCPInventoryOncePerGeneration(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	want := &bridge.CodexNative{}
	openCalls := 0
	reloadCalls := 0
	var onEvent func(bridge.CodexNativeEvent)
	coordinator.openCodex = func(_ context.Context, config bridge.CodexNativeConfig) (*bridge.CodexNative, error) {
		openCalls++
		onEvent = config.OnEvent
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
	if onEvent == nil {
		t.Fatal("Codex native title observer was not composed")
	}
}

func TestCodexNativeNameEventUpdatesOnlyTheLiveCodexPeer(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	runtime := newPresenceTestRuntime(t)
	coordinator.publishRuntime(runtime)
	runtime.Attachments().ReportLive("codex-thread", "launch-name", "codex", []string{"project"}, map[string]string{}, false)
	runtime.Attachments().ReportLive("claude-session", "claude-name", "claude", []string{"project"}, map[string]string{}, false)

	coordinator.observeCodexNativeEvent(bridge.CodexNativeEvent{
		Kind: "thread/name/updated", ThreadID: "codex-thread", Name: "native-name",
	})
	if title, ok, err := runtime.Attachments().LiveNativeTitle("codex-thread"); err != nil || !ok || title != "native-name" {
		t.Fatalf("Codex native title = %q, ok=%v, err=%v", title, ok, err)
	}
	coordinator.observeCodexNativeEvent(bridge.CodexNativeEvent{
		Kind: "thread/name/updated", ThreadID: "claude-session", Name: "wrong-product",
	})
	if title, ok, err := runtime.Attachments().LiveNativeTitle("claude-session"); err != nil || !ok || title != "claude-name" {
		t.Fatalf("Claude title changed by Codex event: %q, ok=%v, err=%v", title, ok, err)
	}
	runtime.Attachments().ForgetLive("codex-thread")
	coordinator.observeCodexNativeEvent(bridge.CodexNativeEvent{
		Kind: "thread/name/updated", ThreadID: "codex-thread", Name: "late-name",
	})
	if title, ok, err := runtime.Attachments().LiveNativeTitle("codex-thread"); err != nil || ok || title != "" {
		t.Fatalf("departed Codex title was revived: %q, ok=%v, err=%v", title, ok, err)
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
