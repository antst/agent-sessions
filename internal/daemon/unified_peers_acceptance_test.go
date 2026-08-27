package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

const unifiedPeerAcceptanceEnvironment = "AGENT_SESSIONS_UNIFIED_PEERS_ACCEPTANCE"

type unifiedPeerProcess struct {
	product, sessionID string
	command            *exec.Cmd
	pid                int
	procStart          string
}

type unifiedPeerAdapter struct {
	mu         sync.Mutex
	processes  map[string]unifiedPeerProcess
	deliveries map[string]map[string]int
}

func (adapter *unifiedPeerAdapter) PrepareInteractive(_ context.Context, request AttachmentPrepareRequest) (NativeLaunchPlan, error) {
	return NativeLaunchPlan{
		Executable: request.Product, Cwd: request.Cwd,
		ExpectedNativeActor: cloneAttachmentEvidence(request.ExpectedNativeActor),
	}, nil
}

func (adapter *unifiedPeerAdapter) Corroborate(_ context.Context, record AttachmentRecord, evidence map[string]any) (map[string]any, error) {
	process, ok := adapter.processes[record.Product]
	if !ok || unifiedPeerString(evidence["session_id"]) != process.sessionID || unifiedPeerInt(evidence["pid"]) != process.pid ||
		unifiedPeerString(evidence["proc_start"]) != process.procStart {
		return nil, ErrAttachmentEvidenceChanged
	}
	identity := procinfo.Read(process.pid)
	if identity.Status != procinfo.Known || identity.Start != process.procStart {
		return nil, ErrAttachmentEvidenceChanged
	}
	return cloneAttachmentEvidence(evidence), nil
}

func unifiedPeerString(value any) string {
	text, _ := value.(string)
	return text
}

func unifiedPeerInt(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case float64:
		return int(number)
	case json.Number:
		integer, _ := number.Int64()
		return int(integer)
	default:
		return 0
	}
}

func (adapter *unifiedPeerAdapter) Reconnect(ctx context.Context, record AttachmentRecord) (map[string]any, error) {
	return adapter.Corroborate(ctx, record, record.NativeActor)
}

func (adapter *unifiedPeerAdapter) Deliver(ctx context.Context, destination AttachmentRecord, frame federation.AgentFrame) error {
	if _, err := adapter.Corroborate(ctx, destination, destination.NativeActor); err != nil {
		return err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.deliveries[destination.SessionID] == nil {
		adapter.deliveries[destination.SessionID] = make(map[string]int)
	}
	adapter.deliveries[destination.SessionID][frame.MessageID]++
	return nil
}

func (adapter *unifiedPeerAdapter) count(sessionID, messageID string) int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.deliveries[sessionID][messageID]
}

func TestUnifiedPeersAcceptance(t *testing.T) {
	if os.Getenv(unifiedPeerAcceptanceEnvironment) != "1" {
		t.Skip("run through scripts/test-unified-peers")
	}
	ctx := context.Background()
	root := t.TempDir()
	token := "unified-peer-census-" + filepath.Base(root)
	processes := startUnifiedPeerProcesses(t, token)
	adapter := &unifiedPeerAdapter{processes: processes, deliveries: make(map[string]map[string]int)}
	assertUnifiedPeerProcessCensus(t, token, processes)

	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(root, "install"), releaseinstall.RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeUnifiedPeerTreeWritable(layout.Root) })
	service := &unifiedPeerRoleService{}
	hooks := &unifiedPeerRoleHooks{adapter: adapter}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Install(ctx, unifiedPeerRelease(t, root, "0.3.0-a", "a")); err != nil {
		t.Fatalf("install initial host release: %v", err)
	}

	paths := unifiedPeerRuntimePaths(root)
	state, err := OpenStateStore(paths.StateRoot, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	first := newUnifiedPeerRuntime(t, paths, state, adapter, "0.3.0-a", "sha256:first", 71001, "daemon-first")
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first daemon generation: %v", err)
	}
	attachments := attachUnifiedPeers(t, first, adapter)
	assertUnifiedPeerGroups(t, first, attachments)

	before := DeliveryRequest{
		MessageID: "accepted-before-upgrade", SourceAttachmentID: attachments["codex"].AttachmentID,
		Operation: DeliveryOperationSend, Targets: []string{attachmentAddress(attachments["claude"])}, Content: "accepted once",
	}
	accepted, err := first.deliveryEngine().Accept(ctx, before)
	if err != nil || accepted.State != DeliveryStateDelivered || adapter.count(processes["claude"].sessionID, before.MessageID) != 1 {
		t.Fatalf("pre-upgrade acceptance = %+v, err=%v", accepted, err)
	}

	identities := unifiedPeerIdentitySnapshot(processes)
	hooks.requireLive = true
	if _, err := engine.Install(ctx, unifiedPeerRelease(t, root, "0.3.0-b", "b")); err != nil {
		t.Fatalf("upgrade host release with active peers: %v", err)
	}
	assertUnifiedPeerIdentities(t, processes, identities)
	if service.restarts != 2 || hooks.readyWithLivePeers != 1 {
		t.Fatalf("installer transitions: restarts=%d live_ready=%d", service.restarts, hooks.readyWithLivePeers)
	}

	if err := first.Stop(ctx); err != nil {
		t.Fatalf("stop first daemon generation: %v", err)
	}
	second := newUnifiedPeerRuntime(t, paths, state, adapter, "0.3.0-b", "sha256:second", 71002, "daemon-second")
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start successor daemon generation: %v", err)
	}
	if second.Generation() <= first.Generation() {
		t.Fatalf("successor generation %d did not advance from %d", second.Generation(), first.Generation())
	}
	assertUnifiedPeerIdentities(t, processes, identities)
	assertUnifiedPeerProcessCensus(t, token, processes)
	assertRecoveredUnifiedPeers(t, second, attachments)

	replayed, err := second.deliveryEngine().Accept(ctx, before)
	if err != nil || !reflect.DeepEqual(replayed, accepted) || adapter.count(processes["claude"].sessionID, before.MessageID) != 1 {
		t.Fatalf("accepted replay duplicated after restart: replay=%+v err=%v count=%d", replayed, err, adapter.count(processes["claude"].sessionID, before.MessageID))
	}
	acceptUnifiedPeerPostRestartMessages(t, second, attachments, adapter)
	assertBareUnifiedPeerIsInactive(t, second, processes["bare"])
	if err := second.Stop(ctx); err != nil {
		t.Fatalf("stop successor daemon generation: %v", err)
	}
}

func TestUnifiedPeerVendorHelper(t *testing.T) {
	if os.Getenv("AGENT_SESSIONS_UNIFIED_VENDOR_HELPER") != "1" {
		t.Skip("acceptance subprocess only")
	}
	select {}
}

func startUnifiedPeerProcesses(t *testing.T, token string) map[string]unifiedPeerProcess {
	t.Helper()
	sessions := map[string]string{
		"codex": "10000000-0000-4000-8000-000000000001", "claude": "20000000-0000-4000-8000-000000000002",
		"grok": "30000000-0000-4000-8000-000000000003", "qwen": "40000000-0000-4000-8000-000000000004",
		"bare": "50000000-0000-4000-8000-000000000005",
	}
	result := make(map[string]unifiedPeerProcess, len(sessions))
	for product, sessionID := range sessions {
		command := exec.Command(os.Args[0], "-test.run=^TestUnifiedPeerVendorHelper$", "--", token, product, sessionID)
		command.Env = append(os.Environ(), "AGENT_SESSIONS_UNIFIED_VENDOR_HELPER=1")
		if err := command.Start(); err != nil {
			t.Fatalf("start %s vendor fixture: %v", product, err)
		}
		identity := waitUnifiedPeerIdentity(t, command.Process.Pid)
		result[product] = unifiedPeerProcess{product: product, sessionID: sessionID, command: command, pid: command.Process.Pid, procStart: identity.Start}
	}
	t.Cleanup(func() {
		for _, process := range result {
			_ = process.command.Process.Kill()
			_ = process.command.Wait()
		}
	})
	return result
}

func waitUnifiedPeerIdentity(t *testing.T, pid int) procinfo.Info {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		identity := procinfo.Read(pid)
		if identity.Status == procinfo.Known && identity.Start != "" {
			return identity
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("vendor fixture %d did not publish a process identity", pid)
	return procinfo.Info{}
}

func unifiedPeerRuntimePaths(root string) ProductionPaths {
	return ProductionPaths{
		ConfigurationRoot: filepath.Join(root, "config"), ConfigurationFile: filepath.Join(root, "config", "config.json"),
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), ControlEndpoint: filepath.Join(root, "run", "daemon.sock"),
	}
}

func newUnifiedPeerRuntime(
	t *testing.T,
	paths ProductionPaths,
	state *StateStore,
	adapter *unifiedPeerAdapter,
	version, identity string,
	pid int,
	procStart string,
) *Runtime {
	t.Helper()
	attachmentAdapters := map[string]AttachmentAdapter{"codex": adapter, "claude": adapter, "grok": adapter, "qwen": adapter}
	deliveryAdapters := map[string]DeliveryAdapter{"codex": adapter, "claude": adapter, "grok": adapter, "qwen": adapter}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, State: state,
		Configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion, HostID: "acceptance-host", HostName: "acceptance-host",
			StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
		},
		RuntimeVersion: version, RuntimeIdentity: identity, PID: pid, ProcStart: procStart,
		StrongStart: procStart + "-strong", ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: unifiedPeerClock(), AttachmentAdapters: attachmentAdapters, DeliveryAdapters: deliveryAdapters,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func unifiedPeerClock() func() time.Time {
	now := time.UnixMilli(20000)
	return func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}
}

func attachUnifiedPeers(t *testing.T, runtime *Runtime, adapter *unifiedPeerAdapter) map[string]AttachmentRecord {
	t.Helper()
	groups := map[string][]string{"codex": {"shared"}, "claude": {"shared"}, "grok": {"isolated"}, "qwen": {"isolated", "shared"}}
	attachments := make(map[string]AttachmentRecord, 4)
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		process := adapter.processes[product]
		evidence := map[string]any{"session_id": process.sessionID, "pid": process.pid, "proc_start": process.procStart}
		prepared, capability, err := runtime.attachmentRegistry().Prepare(context.Background(), AttachmentPrepareRequest{
			Product: product, Kind: "interactive", Cwd: "/workspace/" + product, Name: product + "-peer",
			Groups: groups[product], PermissionMode: "default", ExpectedNativeActor: evidence,
		})
		if err != nil {
			t.Fatalf("prepare %s attachment: %v", product, err)
		}
		attached, err := runtime.attachmentRegistry().Adopt(context.Background(), AttachmentAdoptRequest{
			AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: process.sessionID, NativeActor: evidence,
		})
		if err != nil {
			t.Fatalf("adopt %s attachment: %v", product, err)
		}
		attachments[product] = attached
	}
	return attachments
}

func assertUnifiedPeerGroups(t *testing.T, runtime *Runtime, attachments map[string]AttachmentRecord) {
	t.Helper()
	peers, err := runtime.deliveryEngine().Discover(context.Background(), attachments["codex"].AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(peers))
	for _, peer := range peers {
		got = append(got, peer.SessionID)
	}
	slices.Sort(got)
	want := []string{attachments["claude"].SessionID, attachments["qwen"].SessionID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("shared-group discovery = %q, want %q", got, want)
	}
	if _, err := runtime.deliveryEngine().Accept(context.Background(), DeliveryRequest{
		MessageID: "isolated-rejected", SourceAttachmentID: attachments["codex"].AttachmentID,
		Operation: DeliveryOperationSend, Targets: []string{attachmentAddress(attachments["grok"])}, Content: "must not cross groups",
	}); !errors.Is(err, ErrDeliveryUnauthorized) {
		t.Fatalf("isolated direct delivery error = %v", err)
	}
}

func assertRecoveredUnifiedPeers(t *testing.T, runtime *Runtime, before map[string]AttachmentRecord) {
	t.Helper()
	for product, prior := range before {
		recovered, err := runtime.attachmentRegistry().Select(context.Background(), AttachmentSelector{SessionID: prior.SessionID})
		if err != nil || recovered.Product != product || recovered.AttachmentID != prior.AttachmentID ||
			!reflect.DeepEqual(recovered.Groups, prior.Groups) || !reflect.DeepEqual(recovered.NativeActor, prior.NativeActor) {
			t.Fatalf("recovered %s attachment = %+v, err=%v", product, recovered, err)
		}
	}
}

func acceptUnifiedPeerPostRestartMessages(t *testing.T, runtime *Runtime, attachments map[string]AttachmentRecord, adapter *unifiedPeerAdapter) {
	t.Helper()
	multicast := DeliveryRequest{
		MessageID: "post-restart-multicast", SourceAttachmentID: attachments["codex"].AttachmentID,
		Operation: DeliveryOperationMulticast,
		Targets:   []string{attachmentAddress(attachments["claude"]), attachmentAddress(attachments["qwen"])}, Content: "two peers",
	}
	if _, err := runtime.deliveryEngine().Accept(context.Background(), multicast); err != nil {
		t.Fatal(err)
	}
	for _, product := range []string{"claude", "qwen"} {
		if count := adapter.count(attachments[product].SessionID, multicast.MessageID); count != 1 {
			t.Fatalf("%s multicast count = %d", product, count)
		}
	}
	broadcast := DeliveryRequest{
		MessageID: "post-restart-broadcast", SourceAttachmentID: attachments["qwen"].AttachmentID,
		Operation: DeliveryOperationBroadcast, Group: "isolated", Content: "isolated group",
	}
	if _, err := runtime.deliveryEngine().Accept(context.Background(), broadcast); err != nil {
		t.Fatal(err)
	}
	if count := adapter.count(attachments["grok"].SessionID, broadcast.MessageID); count != 1 {
		t.Fatalf("Grok isolated broadcast count = %d", count)
	}
}

func assertBareUnifiedPeerIsInactive(t *testing.T, runtime *Runtime, bare unifiedPeerProcess) {
	t.Helper()
	if _, err := runtime.attachmentRegistry().Select(context.Background(), AttachmentSelector{SessionID: bare.sessionID}); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("bare vendor session became attached: %v", err)
	}
	params := json.RawMessage(`{"name":"list_peers","arguments":{}}`)
	decision := runtime.mcpService().Forward(context.Background(), controlPrincipal{}, MCPForwardRequest{Method: "tools/call", Params: params})
	if decision.Error != nil || !strings.Contains(string(decision.Result), "inactive outside an attested peer session") {
		t.Fatalf("bare MCP decision = %+v", decision)
	}
}

func unifiedPeerIdentitySnapshot(processes map[string]unifiedPeerProcess) map[string]string {
	result := make(map[string]string, len(processes))
	for product, process := range processes {
		result[product] = fmt.Sprintf("%d/%s/%s", process.pid, process.procStart, process.sessionID)
	}
	return result
}

func assertUnifiedPeerIdentities(t *testing.T, processes map[string]unifiedPeerProcess, before map[string]string) {
	t.Helper()
	for product, process := range processes {
		identity := procinfo.Read(process.pid)
		got := fmt.Sprintf("%d/%s/%s", process.pid, identity.Start, process.sessionID)
		if identity.Status != procinfo.Known || got != before[product] {
			t.Fatalf("%s identity changed: got %q want %q status=%v", product, got, before[product], identity.Status)
		}
	}
}

func assertUnifiedPeerProcessCensus(t *testing.T, token string, processes map[string]unifiedPeerProcess) {
	t.Helper()
	listed, err := procinfo.List()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, 0, len(processes))
	for _, process := range listed {
		arguments, argsErr := procinfo.Args(process.PID)
		if argsErr == nil && slices.Contains(arguments, token) {
			got = append(got, process.PID)
			joined := strings.Join(arguments, " ")
			for _, forbidden := range []string{" supervisor ", " shim ", "qwen-host", "grok-host", "lane-manager"} {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("acceptance process retained legacy authority role: %s", joined)
				}
			}
		}
	}
	want := make([]int, 0, len(processes))
	for _, process := range processes {
		want = append(want, process.pid)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("exact vendor process census = %v, want %v", got, want)
	}
}

type unifiedPeerRoleService struct{ restarts int }

func (service *unifiedPeerRoleService) Restart(context.Context) error { service.restarts++; return nil }
func (*unifiedPeerRoleService) Stop(context.Context) error            { return nil }
func (*unifiedPeerRoleService) Disable(context.Context) error         { return nil }
func (*unifiedPeerRoleService) Start(context.Context) error           { return nil }
func (*unifiedPeerRoleService) Verify(context.Context) error          { return nil }

type unifiedPeerRoleHooks struct {
	adapter            *unifiedPeerAdapter
	requireLive        bool
	readyWithLivePeers int
}

func (*unifiedPeerRoleHooks) Prepare(context.Context, releaseinstall.InstallRequest) error {
	return nil
}
func (hooks *unifiedPeerRoleHooks) Ready(_ context.Context, release releaseinstall.InstalledRelease) error {
	if !hooks.requireLive {
		return nil
	}
	for _, process := range hooks.adapter.processes {
		identity := procinfo.Read(process.pid)
		if identity.Status != procinfo.Known || identity.Start != process.procStart {
			return fmt.Errorf("%s vendor process changed during release %s readiness", process.product, release.ReleaseID)
		}
	}
	hooks.readyWithLivePeers++
	return nil
}
func (*unifiedPeerRoleHooks) Commit(context.Context) error   { return nil }
func (*unifiedPeerRoleHooks) Rollback(context.Context) error { return nil }
func (*unifiedPeerRoleHooks) Remove(context.Context) error   { return nil }

func unifiedPeerRelease(t *testing.T, root, version, seed string) releaseinstall.InstallRequest {
	t.Helper()
	contentIdentity := "sha256:" + strings.Repeat(seed, 64)
	source := filepath.Join(root, "source-"+version)
	if err := os.MkdirAll(filepath.Join(source, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "bin", "agent-sessions"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"version": version, "content_identity": contentIdentity})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return releaseinstall.InstallRequest{Version: version, ContentIdentity: contentIdentity, SourceRoot: source, Executable: "agent-sessions"}
}

func makeUnifiedPeerTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
