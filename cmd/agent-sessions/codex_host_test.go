package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/bridge"
	daemonpkg "github.com/antst/sessionbus/internal/daemon"
	"github.com/antst/sessionbus/internal/launcher"
	"github.com/antst/sessionbus/internal/livepresence"
	claudeproduct "github.com/antst/sessionbus/internal/products/claude"
	grokproduct "github.com/antst/sessionbus/internal/products/grok"
	kiloproduct "github.com/antst/sessionbus/internal/products/kilocode"
	ompproduct "github.com/antst/sessionbus/internal/products/omp"
	opencodeproduct "github.com/antst/sessionbus/internal/products/opencode"
	piproduct "github.com/antst/sessionbus/internal/products/pi"
	qwenproduct "github.com/antst/sessionbus/internal/products/qwen"
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
	joined := make(chan livepresence.Report, 1)
	left := make(chan livepresence.Report, 1)
	server, err := startLivePresenceServer(serverCtx, stateRoot,
		func(report livepresence.Report) { joined <- report },
		func(report livepresence.Report) { left <- report }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()

	original := connectorNativeAdapters[connectorProductCodex]
	defer func() { connectorNativeAdapters[connectorProductCodex] = original }()
	delivered := make(chan livepresence.Report, 1)
	connectorNativeAdapters[connectorProductCodex] = func(_ context.Context, report livepresence.Report, _ string, body string) error {
		if !strings.Contains(body, "hello") || !strings.Contains(body, `from-session="parent"`) {
			t.Errorf("delivered body = %q", body)
		}
		delivered <- report
		return nil
	}

	launch := launcher.CodexNativeLaunch{
		Executable: "/bin/sh", Arguments: []string{"-c", "sleep 1"},
		Groups: []string{"project"}, Confirm: func() (launcher.CodexDaemonPrepareResult, error) {
			return launcher.CodexDaemonPrepareResult{
				ThreadID: "00000000-0000-0000-0000-00000000c001", Name: "reviewer",
			}, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- runCodexNativePeer(context.Background(), launch) }()

	select {
	case report := <-joined:
		if report.UUID != "00000000-0000-0000-0000-00000000c001" || report.Name != "reviewer" || report.Product != "codex" || len(report.Groups) != 1 || report.Groups[0] != "project" {
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
		if report.UUID != "00000000-0000-0000-0000-00000000c001" {
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
		if report.UUID != "00000000-0000-0000-0000-00000000c001" {
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
	coordinator.joinLiveSession(runtime, livepresence.Report{
		UUID: "codex-thread", Name: "launch-name", Product: "codex", Groups: []string{"project"}, Info: map[string]string{},
	})
	coordinator.joinLiveSession(runtime, livepresence.Report{
		UUID: "claude-session", Name: "claude-name", Product: "claude", Groups: []string{"project"}, Info: map[string]string{},
	})

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
	coordinator.joinLiveSession(runtime, livepresence.Report{
		UUID: "unrelated", Name: "updated-peer", Product: "qwen", Groups: []string{"project"}, Info: map[string]string{},
	})
	if title, ok, err := runtime.Attachments().LiveNativeTitle("codex-thread"); err != nil || !ok || title != "native-name" {
		t.Fatalf("unrelated report reverted Codex title: %q, ok=%v, err=%v", title, ok, err)
	}
	coordinator.leaveLiveSession(runtime, livepresence.Report{UUID: "codex-thread", Product: "codex"})
	coordinator.observeCodexNativeEvent(bridge.CodexNativeEvent{
		Kind: "thread/name/updated", ThreadID: "codex-thread", Name: "late-name",
	})
	if title, ok, err := runtime.Attachments().LiveNativeTitle("codex-thread"); err != nil || ok || title != "" {
		t.Fatalf("departed Codex title was revived: %q, ok=%v, err=%v", title, ok, err)
	}
}

func TestCodexUUIDv7UnixMilli(t *testing.T) {
	tests := []struct {
		name, id, wantError string
		want                uint64
	}{
		{name: "lowercase", id: "00000001-0002-7000-8000-000000000000", want: 1<<16 | 2},
		{name: "variant 9", id: "00000001-0002-7000-9000-000000000000", want: 1<<16 | 2},
		{name: "uppercase", id: "0000000A-000B-7000-A000-ABCDEFABCDEF", want: 10<<16 | 11},
		{name: "variant b", id: "00000001-0002-7000-b000-000000000000", want: 1<<16 | 2},
		{name: "full 48 bit", id: "ffffffff-ffff-7fff-bfff-ffffffffffff", want: 1<<48 - 1},
		{name: "length", id: "bad", wantError: "canonical 8-4-4-4-12"},
		{name: "hex", id: "00000001-0002-7000-8000-00000000000z", wantError: "hexadecimal"},
		{name: "version", id: "00000001-0002-6000-8000-000000000000", wantError: "version nibble is 6"},
		{name: "variant", id: "00000001-0002-7000-c000-000000000000", wantError: "variant bits are 11"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := codexUUIDv7UnixMilli(test.id)
			if test.wantError == "" && (err != nil || got != test.want) {
				t.Fatalf("timestamp = %d, %v; want %d", got, err, test.want)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.id) || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("error = %v; want id and %q", err, test.wantError)
			}
		})
	}
}

func TestCodexPendingLaunchMatchesOnlyEligibleFreshOrNativeResume(t *testing.T) {
	cwd := t.TempDir()
	thread := bridge.CodexNativeThread{
		ID: "00000001-0002-7000-8000-000000000000", Name: "Exact Native Name", Cwd: cwd,
	}
	for _, test := range []struct {
		name, kind, value, cwd, event, eventCwd string
		registered, started                     uint64
		captured, matches                       bool
	}{
		{name: "fresh equal millisecond", kind: launcher.CodexLaunchSelectorFresh, cwd: cwd, event: "thread/started", registered: 10, started: 10, captured: true, matches: true},
		{name: "fresh newer millisecond", kind: launcher.CodexLaunchSelectorFresh, cwd: cwd, event: "thread/started", registered: 10, started: 11, captured: true, matches: true},
		{name: "fresh older millisecond", kind: launcher.CodexLaunchSelectorFresh, cwd: cwd, event: "thread/started", registered: 10, started: 9, captured: true},
		{name: "started observed before registration", kind: launcher.CodexLaunchSelectorFresh, cwd: cwd, event: "thread/started", registered: 10, started: 11},
		{name: "fresh event cwd differs", kind: launcher.CodexLaunchSelectorFresh, cwd: cwd, event: "thread/started", eventCwd: "/other", registered: 10, started: 11, captured: true},
		{name: "old status is not fresh", kind: launcher.CodexLaunchSelectorFresh, cwd: cwd, event: "thread/status/changed", registered: 10, started: 11, captured: true},
		{name: "bare resume status", kind: launcher.CodexLaunchSelectorBare, cwd: cwd, event: "thread/status/changed", matches: true},
		{name: "exact name status", kind: launcher.CodexLaunchSelectorName, value: thread.Name, cwd: cwd, event: "thread/status/changed", matches: true},
		{name: "name is case sensitive", kind: launcher.CodexLaunchSelectorName, value: "exact native name", cwd: cwd, event: "thread/status/changed"},
		{name: "exact id started", kind: launcher.CodexLaunchSelectorID, value: thread.ID, cwd: cwd, event: "thread/started", matches: true},
		{name: "foreign cwd", kind: launcher.CodexLaunchSelectorID, value: thread.ID, cwd: t.TempDir(), event: "thread/status/changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := &pendingCodexLaunch{request: launcher.CodexDaemonPrepareRequest{SelectorKind: test.kind, Selector: test.value, Cwd: test.cwd}, registrationUnixMilli: int64(test.registered)}
			eventCwd := cwd
			if test.eventCwd != "" {
				eventCwd = test.eventCwd
			}
			observation := codexLaunchObservation{event: bridge.CodexNativeEvent{Kind: test.event, Cwd: eventCwd}, threadUnixMilli: test.started}
			if test.captured {
				observation.freshEligible = map[*pendingCodexLaunch]struct{}{record: {}}
			}
			if got := pendingCodexLaunchMatches(record, observation, thread); got != test.matches {
				t.Fatalf("match = %v, want %v", got, test.matches)
			}
		})
	}
}

func TestInvalidFreshCodexIDFailsOnlyCapturedSameCwd(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	matching := &pendingCodexLaunch{request: launcher.CodexDaemonPrepareRequest{SelectorKind: launcher.CodexLaunchSelectorFresh, Cwd: "/match"}, done: make(chan pendingCodexLaunchResult, 1)}
	foreign := &pendingCodexLaunch{request: launcher.CodexDaemonPrepareRequest{SelectorKind: launcher.CodexLaunchSelectorFresh, Cwd: "/foreign"}, done: make(chan pendingCodexLaunchResult, 1)}
	coordinator.pendingCodexLaunches["matching"], coordinator.pendingCodexLaunches["foreign"] = matching, foreign
	productID := "not-a-product-uuid"
	coordinator.observeCodexNativeEvent(bridge.CodexNativeEvent{Kind: "thread/started", ThreadID: productID, Cwd: "/match"})
	select {
	case result := <-matching.done:
		if result.err == nil || !strings.Contains(result.err.Error(), productID) || !strings.Contains(result.err.Error(), "malformed") {
			t.Fatalf("matching failure = %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("matching fresh call did not receive the invalid product id")
	}
	if coordinator.pendingCodexLaunches["matching"] != nil || coordinator.pendingCodexLaunches["foreign"] != foreign {
		t.Fatal("invalid event removed the wrong pending record")
	}
	select {
	case result := <-foreign.done:
		t.Fatalf("foreign fresh call received invalid event: %v", result.err)
	default:
	}
}

func TestCodexPendingLaunchOverlapRefusesEveryAmbiguousRegistration(t *testing.T) {
	cwd := t.TempDir()
	name := launcher.CodexDaemonPrepareRequest{Cwd: cwd, SelectorKind: launcher.CodexLaunchSelectorName, Selector: "selected"}
	if !pendingCodexLaunchesOverlap(name, name) {
		t.Fatal("same cwd and name were not recognized as duplicate")
	}
	otherName := name
	otherName.Selector = "another"
	if pendingCodexLaunchesOverlap(name, otherName) {
		t.Fatal("distinct native names were made ambiguous")
	}
	fresh := launcher.CodexDaemonPrepareRequest{Cwd: cwd, SelectorKind: launcher.CodexLaunchSelectorFresh}
	if !pendingCodexLaunchesOverlap(fresh, otherName) {
		t.Fatal("cwd-only fresh launch was allowed to overlap a name-bound launch")
	}
	foreign := name
	foreign.Cwd = t.TempDir()
	if pendingCodexLaunchesOverlap(name, foreign) {
		t.Fatal("independent working directories were treated as ambiguous")
	}
}

func TestCodexPendingLaunchNativeReadinessFailureLeavesNoRecord(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	coordinator.openCodex = func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error) {
		return nil, errors.New("native readiness failed")
	}
	_, err := coordinator.awaitCodexPendingLaunch(context.Background(), nil, launcher.CodexDaemonPrepareRequest{
		Cwd: t.TempDir(), SelectorKind: launcher.CodexLaunchSelectorFresh, PendingToken: strings.Repeat("a", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "native readiness failed") || len(coordinator.pendingCodexLaunches) != 0 {
		t.Fatalf("readiness failure = %v, pending = %v", err, coordinator.pendingCodexLaunches)
	}
}

func TestExactIDPendingLaunchReceivesItsNativeMatchFailure(t *testing.T) {
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	t.Cleanup(func() { _ = coordinator.laneProcesses.Close() })
	threadID := "00000000-0000-0000-0000-00000000c023"
	exact := &pendingCodexLaunch{
		request: launcher.CodexDaemonPrepareRequest{SelectorKind: launcher.CodexLaunchSelectorID, Selector: threadID},
		done:    make(chan pendingCodexLaunchResult, 1),
	}
	byName := &pendingCodexLaunch{
		request: launcher.CodexDaemonPrepareRequest{SelectorKind: launcher.CodexLaunchSelectorName, Selector: "same-thread"},
		done:    make(chan pendingCodexLaunchResult, 1),
	}
	coordinator.pendingCodexLaunches["exact"] = exact
	coordinator.pendingCodexLaunches["name"] = byName
	nativeFailure := errors.New("native exact read failed")
	if !coordinator.failExactCodexPendingLaunch(threadID, nativeFailure) {
		t.Fatal("exact-id pending launch did not accept its native failure")
	}
	if result := <-exact.done; !errors.Is(result.err, nativeFailure) {
		t.Fatalf("exact-id failure = %v", result.err)
	}
	if coordinator.pendingCodexLaunches["exact"] != nil || coordinator.pendingCodexLaunches["name"] != byName {
		t.Fatal("failure removed any pending launch other than the exact-id owner")
	}
	select {
	case result := <-byName.done:
		t.Fatalf("unmatched name launch received failure: %v", result.err)
	default:
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
