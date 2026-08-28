package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

const unifiedStressEnvironment = "AGENT_SESSIONS_UNIFIED_STRESS"

type stressAdapter struct {
	mu         sync.Mutex
	deliveries map[string]map[string]int
}

func (*stressAdapter) PrepareInteractive(_ context.Context, request AttachmentPrepareRequest) (NativeLaunchPlan, error) {
	return NativeLaunchPlan{
		Executable:          request.Product,
		Cwd:                 request.Cwd,
		ExpectedNativeActor: cloneAttachmentEvidence(request.ExpectedNativeActor),
	}, nil
}

func (*stressAdapter) Corroborate(_ context.Context, record AttachmentRecord, evidence map[string]any) (map[string]any, error) {
	if !reflect.DeepEqual(record.NativeActor, evidence) {
		return nil, ErrAttachmentEvidenceChanged
	}
	return cloneAttachmentEvidence(evidence), nil
}

func (*stressAdapter) Reconnect(_ context.Context, record AttachmentRecord) (map[string]any, error) {
	return cloneAttachmentEvidence(record.NativeActor), nil
}

func (adapter *stressAdapter) Deliver(_ context.Context, destination AttachmentRecord, frame federation.AgentFrame) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.deliveries[destination.SessionID] == nil {
		adapter.deliveries[destination.SessionID] = make(map[string]int)
	}
	adapter.deliveries[destination.SessionID][frame.MessageID]++
	return nil
}

func (adapter *stressAdapter) count(sessionID, messageID string) int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.deliveries[sessionID][messageID]
}

func TestUnifiedAttachmentStress(t *testing.T) {
	if os.Getenv(unifiedStressEnvironment) != "1" {
		t.Skip("run through scripts/test-unified-stress")
	}
	const attachmentCount = 100
	ctx := context.Background()
	root := t.TempDir()
	paths := unifiedPeerRuntimePaths(root)
	state, err := OpenStateStore(paths.StateRoot, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &stressAdapter{deliveries: make(map[string]map[string]int)}
	clock := stressClock()
	first := newStressRuntime(t, paths, state, adapter, clock, "stress-a", 81001)

	endpoint, err := acquireControlEndpoint(controlEndpointOptions{endpoint: paths.ControlEndpoint})
	if err != nil {
		t.Fatalf("acquire sole stress listener: %v", err)
	}
	if info, statErr := os.Lstat(paths.ControlEndpoint); statErr != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stress control endpoint is not one Unix listener: info=%v err=%v", info, statErr)
	}
	if duplicate, duplicateErr := acquireControlEndpoint(controlEndpointOptions{endpoint: paths.ControlEndpoint}); duplicateErr == nil {
		_ = duplicate.Close()
		t.Fatal("second host listener was admitted")
	}
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first stress generation: %v", err)
	}

	attachments := attachStressPeers(t, first, adapter, attachmentCount)
	assertStressInventory(t, first, attachments)
	requests := deliverStressMessages(t, first, attachments, adapter)

	if err := first.Stop(ctx); err != nil {
		t.Fatalf("stop first stress generation: %v", err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatalf("close first stress listener: %v", err)
	}
	secondEndpoint, err := acquireControlEndpoint(controlEndpointOptions{endpoint: paths.ControlEndpoint})
	if err != nil {
		t.Fatalf("reacquire sole stress listener: %v", err)
	}
	defer func() { _ = secondEndpoint.Close() }()

	second := newStressRuntime(t, paths, state, adapter, clock, "stress-b", 81002)
	restartStarted := time.Now()
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start successor stress generation: %v", err)
	}
	if elapsed := time.Since(restartStarted); elapsed > 30*time.Second {
		t.Fatalf("100-attachment restart exceeded 30s budget: %s", elapsed)
	}
	if second.Generation() <= first.Generation() {
		t.Fatalf("stress generation did not advance: first=%d second=%d", first.Generation(), second.Generation())
	}
	assertStressRecovered(t, second, attachments)
	replayStressMessages(t, second, attachments, requests, adapter)
	if err := second.Stop(ctx); err != nil {
		t.Fatalf("stop successor stress generation: %v", err)
	}
	t.Logf(`{"type":"unified.stress.passed","attachments":%d,"products":4,"groups":3,"listeners":1,"duplicate_turns":0}`, attachmentCount)
}

func stressClock() func() time.Time {
	var milliseconds atomic.Int64
	milliseconds.Store(100_000)
	return func() time.Time { return time.UnixMilli(milliseconds.Add(1)) }
}

func newStressRuntime(
	t *testing.T,
	paths ProductionPaths,
	state *StateStore,
	adapter *stressAdapter,
	now func() time.Time,
	identity string,
	pid int,
) *Runtime {
	t.Helper()
	attachmentAdapters := make(map[string]AttachmentAdapter, 4)
	deliveryAdapters := make(map[string]DeliveryAdapter, 4)
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		attachmentAdapters[product] = adapter
		deliveryAdapters[product] = adapter
	}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths,
		State: state,
		Configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion,
			HostID:        "stress-host",
			HostName:      "stress-host",
			StateRoot:     paths.StateRoot,
			RuntimeRoot:   paths.RuntimeRoot,
			Revision:      1,
			UpdatedAt:     1,
		},
		RuntimeVersion: "stress", RuntimeIdentity: "sha256:" + identity,
		PID: pid, ProcStart: identity, StrongStart: identity + "-strong",
		ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service", Now: now,
		AttachmentAdapters: attachmentAdapters, DeliveryAdapters: deliveryAdapters,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func attachStressPeers(t *testing.T, runtime *Runtime, adapter *stressAdapter, count int) []AttachmentRecord {
	t.Helper()
	products := []string{"codex", "claude", "grok", "qwen"}
	groups := []string{"production", "development", "test"}
	result := make([]AttachmentRecord, count)
	errorsByIndex := make([]error, count)
	var workers sync.WaitGroup
	for index := range count {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			product := products[index%len(products)]
			sessionID := fmt.Sprintf("stress-session-%03d", index)
			evidence := map[string]any{"session_id": sessionID, "actor_id": fmt.Sprintf("native-actor-%03d", index)}
			prepared, capability, err := runtime.attachmentRegistry().Prepare(context.Background(), AttachmentPrepareRequest{
				Product: product, Kind: "interactive", Cwd: filepath.Join("/workspace", groups[index%len(groups)]),
				Name: fmt.Sprintf("stress-%s-%03d", product, index), NameSource: product,
				Groups: []string{groups[index%len(groups)]}, PermissionMode: "default", ExpectedNativeActor: evidence,
			})
			if err != nil {
				errorsByIndex[index] = err
				return
			}
			attached, err := runtime.attachmentRegistry().Adopt(context.Background(), AttachmentAdoptRequest{
				AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: sessionID, NativeActor: evidence,
			})
			if err != nil {
				errorsByIndex[index] = err
				return
			}
			result[index] = attached
		}(index)
	}
	workers.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("attach stress peer %d: %v", index, err)
		}
		if result[index].SessionID == "" {
			t.Fatalf("stress peer %d was not attached", index)
		}
	}
	_ = adapter
	return result
}

func assertStressInventory(t *testing.T, runtime *Runtime, attachments []AttachmentRecord) {
	t.Helper()
	if got := len(runtime.attachmentRegistry().attachedRecords()); got != len(attachments) {
		t.Fatalf("attached inventory = %d, want %d", got, len(attachments))
	}
	for index, attachment := range attachments {
		if attachment.Product != []string{"codex", "claude", "grok", "qwen"}[index%4] || len(attachment.Groups) != 1 {
			t.Fatalf("attachment %d identity/group = %+v", index, attachment)
		}
		peers, err := runtime.deliveryEngine().Discover(context.Background(), attachment.AttachmentID)
		if err != nil {
			t.Fatalf("discover from attachment %d: %v", index, err)
		}
		for _, peer := range peers {
			if len(peer.Groups) != 1 || peer.Groups[0] != attachment.Groups[0] {
				t.Fatalf("global group isolation crossed from %q to %+v", attachment.Groups[0], peer)
			}
		}
	}
	if _, err := runtime.deliveryEngine().Accept(context.Background(), DeliveryRequest{
		MessageID: "stress-cross-group-rejected", SourceAttachmentID: attachments[0].AttachmentID,
		Operation: DeliveryOperationSend, Targets: []string{attachmentAddress(attachments[1])}, Content: "must remain isolated",
	}); !errors.Is(err, ErrDeliveryUnauthorized) {
		t.Fatalf("cross-group stress delivery error = %v, want ErrDeliveryUnauthorized", err)
	}
}

func deliverStressMessages(
	t *testing.T,
	runtime *Runtime,
	attachments []AttachmentRecord,
	adapter *stressAdapter,
) []DeliveryRequest {
	t.Helper()
	requests := make([]DeliveryRequest, len(attachments))
	errorsByIndex := make([]error, len(attachments))
	var workers sync.WaitGroup
	for index, source := range attachments {
		workers.Add(1)
		go func(index int, source AttachmentRecord) {
			defer workers.Done()
			targetIndex := (index + 3) % len(attachments)
			for attachments[targetIndex].Groups[0] != source.Groups[0] {
				targetIndex = (targetIndex + 1) % len(attachments)
			}
			request := DeliveryRequest{
				MessageID: fmt.Sprintf("stress-message-%03d", index), SourceAttachmentID: source.AttachmentID,
				Operation: DeliveryOperationSend, Targets: []string{attachmentAddress(attachments[targetIndex])}, Content: "accepted exactly once",
			}
			requests[index] = request
			_, errorsByIndex[index] = runtime.deliveryEngine().Accept(context.Background(), request)
		}(index, source)
	}
	workers.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("accept stress message %d: %v", index, err)
		}
		target, selectErr := runtime.attachmentRegistry().Select(context.Background(), AttachmentSelector{
			HostID: "stress-host", SessionID: stressTargetSession(attachments, index),
		})
		if selectErr != nil || adapter.count(target.SessionID, requests[index].MessageID) != 1 {
			t.Fatalf("stress message %d delivery: target=%+v selectErr=%v count=%d", index, target, selectErr, adapter.count(target.SessionID, requests[index].MessageID))
		}
	}
	return requests
}

func stressTargetSession(attachments []AttachmentRecord, index int) string {
	source := attachments[index]
	targetIndex := (index + 3) % len(attachments)
	for attachments[targetIndex].Groups[0] != source.Groups[0] {
		targetIndex = (targetIndex + 1) % len(attachments)
	}
	return attachments[targetIndex].SessionID
}

func assertStressRecovered(t *testing.T, runtime *Runtime, attachments []AttachmentRecord) {
	t.Helper()
	for index, prior := range attachments {
		recovered, err := runtime.attachmentRegistry().Select(context.Background(), AttachmentSelector{
			HostID: prior.HostID, SessionID: prior.SessionID,
		})
		if err != nil || recovered.AttachmentID != prior.AttachmentID || recovered.Product != prior.Product ||
			!reflect.DeepEqual(recovered.NativeActor, prior.NativeActor) || !reflect.DeepEqual(recovered.Groups, prior.Groups) {
			t.Fatalf("recovered stress attachment %d = %+v, err=%v, prior=%+v", index, recovered, err, prior)
		}
	}
}

func replayStressMessages(
	t *testing.T,
	runtime *Runtime,
	attachments []AttachmentRecord,
	requests []DeliveryRequest,
	adapter *stressAdapter,
) {
	t.Helper()
	errorsByIndex := make([]error, len(requests))
	var workers sync.WaitGroup
	for index, request := range requests {
		workers.Add(1)
		go func(index int, request DeliveryRequest) {
			defer workers.Done()
			_, errorsByIndex[index] = runtime.deliveryEngine().Accept(context.Background(), request)
		}(index, request)
	}
	workers.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("replay stress message %d: %v", index, err)
		}
		sessionID := stressTargetSession(attachments, index)
		if got := adapter.count(sessionID, requests[index].MessageID); got != 1 {
			t.Fatalf("replayed stress message %d delivered %d times", index, got)
		}
	}
}
