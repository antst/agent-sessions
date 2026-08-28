package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

const (
	legacyOwnedPendingPayloadCanary = "T084_OWNED_PENDING_PAYLOAD_REQUIRED_58d7"
	legacyMetadataOnlyCanary        = "T084_METADATA_ONLY_RESULT_ERROR_FORBIDDEN_4ac1"
)

func TestProductionLegacyAdoptsNoStatePendingInboxAndWakeExactlyOnce(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_100_000)
	base := productionDeliveryAdoptionBase(t, observedAt)
	base.Sessions = []LegacySessionRecord{{
		SessionID: "pending-session", Product: "codex", Kind: federation.SessionKindInteractive,
		PermissionMode: "default", UpdatedAt: observedAt,
	}}
	bridgeRoot := filepath.Join(t.TempDir(), "claude-code-peer")
	item := map[string]any{
		"id": "pending-message", "message": legacyOwnedPendingPayloadCanary,
		"from": "remote-host/source-session", "fromSession": "source-session",
		"sentAt": "2026-08-27T12:00:00Z", "receivedAt": "2026-08-27T12:00:01Z",
		"summary": "bounded summary",
	}
	pending := filepath.Join(
		bridgeRoot, "sessions", sessionkey.FromID("pending-session"), "inbox", "pending",
		fmt.Sprintf("%016d-pending-message.json", observedAt),
	)
	writeProductionAdoptionJSON(t, pending, item)
	// The missing state.json is intentional: pending inbox discovery is an
	// independent owned-record family, not a child of live shim state.
	if _, err := os.Stat(filepath.Join(bridgeRoot, "sessions", sessionkey.FromID("pending-session"), "state.json")); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly has state.json: %v", err)
	}
	writeProductionAdoptionJSON(t, filepath.Join(
		bridgeRoot, "profiles", "profile-a", "wakes", sessionkey.FromID("pending-session"),
		sessionkey.FromID("pending-message")+".json",
	), map[string]any{
		"sessionId": "pending-session", "messageId": "pending-message", "state": "queued",
		"fingerprint": "fingerprint-pending", "item": item, "updatedAt": observedAt,
		"error": legacyMetadataOnlyCanary, "result": legacyMetadataOnlyCanary,
	})
	sources := []LegacyInventorySource{{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5}}
	adoption, err := productionLegacyBridgeAdoption(ctx, base, sources, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(adoption.Deliveries) != 1 || adoption.Deliveries[0].MessageID != "pending-message" ||
		adoption.Deliveries[0].Content != legacyOwnedPendingPayloadCanary ||
		adoption.Deliveries[0].DestinationResults[base.HostID+"/pending-session"] != DeliveryDestinationPending {
		t.Fatalf("accepted pending adoption = %+v", adoption.Deliveries)
	}
	if len(adoption.DeliveryCursors) != 1 || adoption.DeliveryCursors[0].MessageID != "pending-message" ||
		adoption.Sessions[0].DeliveryCursor != "pending-message" || len(adoption.DeliveryNotices) != 2 {
		t.Fatalf("pending cursor/notices = %+v / %+v / %+v", adoption.Sessions, adoption.DeliveryCursors, adoption.DeliveryNotices)
	}
	encoded, err := json.Marshal(adoption)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), legacyOwnedPendingPayloadCanary) || strings.Contains(string(encoded), legacyMetadataOnlyCanary) {
		t.Fatalf("owned payload or metadata-only boundary changed: %s", encoded)
	}

	plan, err := StageLegacyAdoption(ctx, adoption)
	if err != nil {
		t.Fatal(err)
	}
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "unified"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	migrationID := "migration-pending-delivery"
	selectProductionDeliveryMigration(t, ctx, state, migrationID)
	firstCommit, err := CommitLegacyAdoption(ctx, state, migrationID, plan)
	if err != nil {
		t.Fatal(err)
	}
	if retry, retryErr := CommitLegacyAdoption(ctx, state, migrationID, plan); retryErr != nil ||
		!retry.AlreadyCommitted || retry.StateRevision != firstCommit.StateRevision {
		t.Fatalf("idempotent pending adoption = %+v, %v", retry, retryErr)
	}
	commitProductionDeliveryMigration(t, ctx, state, migrationID)

	registry := productionAdoptionAttachmentRegistry(t, state, base.HostID)
	managed, err := registry.LookupManaged(ctx, AttachmentLookupRequest{Product: "codex", SessionID: "pending-session"})
	if err != nil || managed.SessionID != "pending-session" || managed.Product != "codex" || managed.Live {
		t.Fatalf("adopted dormant session is not exactly resumable: %+v, %v", managed, err)
	}
	target := attachDeliveryLaneTestParticipant(
		t, registry, "codex", "pending", "pending-session", "/workspace", []string{"migration"}, 9101,
	)
	adapter := &deliveryTestAdapter{counts: make(map[string]int)}
	engine, err := NewDeliveryEngine(DeliveryEngineOptions{
		State: state, Attachments: registry, Adapters: map[string]DeliveryAdapter{"codex": adapter},
		Now: attachmentTestClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := engine.Read(ctx, "pending-message")
	if err != nil || delivered.State != DeliveryStateDelivered || adapter.deliveriesTo(target.SessionID) != 1 {
		t.Fatalf("resumed adopted delivery = %+v, calls=%d, err=%v", delivered, adapter.callCount(), err)
	}
	if _, err := NewDeliveryEngine(DeliveryEngineOptions{
		State: state, Attachments: registry, Adapters: map[string]DeliveryAdapter{"codex": adapter},
		Now: attachmentTestClock(),
	}); err != nil || adapter.deliveriesTo(target.SessionID) != 1 {
		t.Fatalf("restart duplicated adopted delivery: calls=%d err=%v", adapter.callCount(), err)
	}
}

func TestProductionLegacyWakeOutcomesKeepTerminalMetadataAndDebtWithoutContent(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_200_000)
	base := productionDeliveryAdoptionBase(t, observedAt)
	for _, id := range []string{"grok-queued", "grok-terminal", "grok-ambiguous"} {
		base.Sessions = append(base.Sessions, LegacySessionRecord{
			SessionID: id, Product: "grok", Kind: federation.SessionKindInteractive,
			PermissionMode: "default", UpdatedAt: observedAt,
		})
	}
	bridgeRoot := filepath.Join(t.TempDir(), "claude-code-peer")
	for _, fixture := range []struct {
		session, message, delivery, body string
	}{
		{"grok-queued", "message-queued", "queued", "queued-owned-payload"},
		{"grok-terminal", "message-terminal", "actor_accepted", legacyMetadataOnlyCanary},
		{"grok-ambiguous", "message-ambiguous", "in_flight", legacyMetadataOnlyCanary},
	} {
		writeProductionAdoptionJSON(t, filepath.Join(
			bridgeRoot, "profiles", "profile-grok", "grok-wakes", sessionkey.FromID(fixture.session),
			sessionkey.FromID(fixture.message)+".json",
		), map[string]any{
			"sessionId": fixture.session, "messageId": fixture.message, "delivery": fixture.delivery,
			"fingerprint": "fingerprint-" + fixture.message, "updatedAt": observedAt,
			"item":  map[string]any{"id": fixture.message, "message": fixture.body, "from": "remote/source"},
			"error": legacyMetadataOnlyCanary, "result": legacyMetadataOnlyCanary,
		})
	}
	adoption, err := productionLegacyBridgeAdoption(ctx, base, []LegacyInventorySource{{
		ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5,
	}}, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(adoption.Deliveries) != 1 || adoption.Deliveries[0].MessageID != "message-queued" ||
		len(adoption.DeliveryNotices) != 3 || len(adoption.DeliveryCursors) != 3 {
		t.Fatalf("wake delivery projection = deliveries=%+v notices=%+v cursors=%+v", adoption.Deliveries, adoption.DeliveryNotices, adoption.DeliveryCursors)
	}
	if len(adoption.Debt) != 1 || adoption.Debt[0].ResourceKind != "legacy_delivery_ambiguous" ||
		!strings.Contains(adoption.Debt[0].ResourceIdentity, "message-ambiguous") {
		t.Fatalf("ambiguous wake debt = %+v", adoption.Debt)
	}
	if _, err := StageLegacyAdoption(ctx, adoption); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(adoption)
	if strings.Contains(string(encoded), legacyMetadataOnlyCanary) {
		t.Fatalf("terminal/ambiguous wake retained item, result, or error content: %s", encoded)
	}
}

func TestProductionLegacyNoStateInboxWithUnknownSessionIsScopedDebt(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_250_000)
	base := productionDeliveryAdoptionBase(t, observedAt)
	bridgeRoot := filepath.Join(t.TempDir(), "claude-code-peer")
	writeProductionAdoptionJSON(t, filepath.Join(
		bridgeRoot, "sessions", sessionkey.FromID("unknown-session"), "inbox", "pending",
		fmt.Sprintf("%016d-unknown-message.json", observedAt),
	), map[string]any{"id": "unknown-message", "message": "must-not-be-lost", "from": "remote/source"})
	adoption, err := productionLegacyBridgeAdoption(ctx, base, []LegacyInventorySource{{
		ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5,
	}}, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(adoption.Deliveries) != 0 || len(adoption.Debt) != 1 ||
		adoption.Debt[0].ResourceKind != "legacy_pending_inbox" ||
		adoption.Debt[0].CauseCode != "session_identity_unresolved" ||
		adoption.Debt[0].ResourceIdentity != sessionkey.FromID("unknown-session") {
		t.Fatalf("unknown no-state inbox was silently omitted or guessed: %+v", adoption)
	}
	if _, err := StageLegacyAdoption(ctx, adoption); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptedDispatchClaimRestartBecomesDebtWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_300_000)
	request := productionDeliveryAdoptionBase(t, observedAt)
	request.Sessions = []LegacySessionRecord{{
		SessionID: "claimed-session", Product: "claude", Kind: federation.SessionKindInteractive,
		PermissionMode: "default", DeliveryCursor: "claimed-message", UpdatedAt: observedAt,
	}}
	destination := request.HostID + "/claimed-session"
	request.Deliveries = []DeliveryRecord{{
		RecordHeader: productionLegacyMigrationHeader(observedAt), MessageID: "claimed-message",
		SourceHostID: "remote-host", SourceSessionID: "remote-session", Operation: DeliveryOperationSend,
		RequestedTargets: []string{destination}, ResolvedDestinations: []string{destination}, Content: "claimed-payload",
		State: DeliveryStateAccepted, DestinationResults: map[string]string{destination: DeliveryDestinationPending},
		AcceptedRevision: 1, AcceptedAt: observedAt, RequestDigest: "claimed-digest",
		AdoptedSourceRevision: "sha256:claimed-source",
	}}
	request.DeliveryCursors = []LegacyDeliveryCursor{{
		SessionID: "claimed-session", Product: "claude", MessageID: "claimed-message",
		SourceRevision: "sha256:claimed-source", UpdatedAt: observedAt,
	}}
	request.DeliveryNotices = []LegacyDeliveryNotice{{
		NoticeID: "claimed-notice", SessionID: "claimed-session", Product: "claude", MessageID: "claimed-message",
		SourceKind: "pending_inbox", SourceState: DeliveryStateAccepted, Disposition: legacyDeliveryDispositionPending,
		SourceRevision: "sha256:claimed-source", UpdatedAt: observedAt,
	}}
	plan, err := StageLegacyAdoption(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "unified"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	migrationID := "migration-claimed-delivery"
	selectProductionDeliveryMigration(t, ctx, state, migrationID)
	if _, err := CommitLegacyAdoption(ctx, state, migrationID, plan); err != nil {
		t.Fatal(err)
	}
	commitProductionDeliveryMigration(t, ctx, state, migrationID)
	claimed := cloneDeliveryRecord(plan.Snapshot.Deliveries[0])
	claimed.DestinationResults[destination] = deliveryDestinationDispatching
	claimed.Revision++
	if _, err := state.compareAndSwapDeliveryCatalog(ctx, 0, deliveryCatalog{Records: []DeliveryRecord{claimed}}); err != nil {
		t.Fatal(err)
	}
	registry := productionAdoptionAttachmentRegistry(t, state, request.HostID)
	attachDeliveryLaneTestParticipant(t, registry, "claude", "claimed", "claimed-session", "/workspace", []string{"migration"}, 9201)
	adapter := &deliveryTestAdapter{counts: make(map[string]int)}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := NewDeliveryEngine(DeliveryEngineOptions{
			State: state, Attachments: registry, Adapters: map[string]DeliveryAdapter{"claude": adapter},
			Now: attachmentTestClock(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if adapter.callCount() != 0 {
		t.Fatalf("ambiguous dispatch claim was redispatched %d times", adapter.callCount())
	}
	debtID := quiescenceRecordID("adopted-delivery-debt", "claimed-message", destination, "delivery_dispatch_outcome_ambiguous")
	debt, _, err := state.ReadDebt(ctx, debtID)
	if err != nil || debt.ResourceIdentity != "claimed-message/"+destination ||
		debt.CauseCode != "delivery_dispatch_outcome_ambiguous" {
		t.Fatalf("dispatch ambiguity debt = %+v, %v", debt, err)
	}
}

func TestProductionLegacyPreparationAndUncollectedTurnAreMetadataOnlyExactDebt(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_400_000)
	base := productionDeliveryAdoptionBase(t, observedAt)
	root := t.TempDir()
	agentsRoot := filepath.Join(root, "agents")
	preparationID := "prepared-attachment"
	writeProductionAdoptionJSON(t, filepath.Join(
		agentsRoot, base.HostID, "claude-peer-preparations", sessionkey.FromID(preparationID)+".json",
	), map[string]any{
		"version": 1, "product": "claude", "committed": true,
		"registration": map[string]any{
			"version": 1, "session_id": "prepared-session", "attachment_id": preparationID,
			"product": "claude", "started_at": observedAt,
			"pid": 4401, "proc_start": "adapter-start", "adapter_strong_start": "adapter-strong",
			"lifecycle_pid":        4402,
			"lifecycle_proc_start": "lifecycle-start", "lifecycle_strong_start": "lifecycle-strong",
			"lifecycle_root": "/state/agent-sessions/lifecycle",
		},
		"product_payload": map[string]any{"transcript": legacyMetadataOnlyCanary},
		"cleanup_debt": []map[string]any{{
			"version": 1, "product": "claude", "debt_id": "prepared-cleanup",
			"revision": "sha256:cleanup", "operation": "cleanup",
			"owner_kind": "service_key", "owner_id": "prepared-owner", "observation_state": "identity_changed",
			"expected_path": "/profiles/claude/service.key", "expected_pid": 4401,
			"expected_start": "adapter-start", "expected_strong_start": "adapter-strong",
			"expected_digest": "sha256:service-key", "attempts": 2, "updated_at": observedAt,
			"terminal_when_clean": "prepared_removed",
			"last_error":          legacyMetadataOnlyCanary,
		}},
	})
	bridgeRoot := filepath.Join(root, "bridge")
	writeProductionAdoptionJSON(t, filepath.Join(
		bridgeRoot, "profiles", "profile-qwen", "qwen-lanes", "pending-turn.json",
	), map[string]any{
		"threadId": "pending-turn-lane", "name": "pending turn", "cwd": "/workspace",
		"parentHostId": base.HostID, "parentSessionId": "parent-session", "status": "idle",
		"createdAt": observedAt - 10, "updatedAt": observedAt, "collectedTurnId": "previous-turn",
		"turns": []map[string]any{{
			"id": "terminal-uncollected", "status": "completed", "outcome": "completed", "collected": false,
			"messageId": "accepted-message", "qwenSessionId": "native-qwen-session",
			"terminalRevision": "terminal-revision-7",
			"completedAt":      observedAt, "prompt": legacyMetadataOnlyCanary, "result": legacyMetadataOnlyCanary,
		}},
	})
	sources := []LegacyInventorySource{
		{ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true, MaxDepth: 5},
		{ID: "host-agent-state", Kind: "state", Path: agentsRoot, Target: true, MaxDepth: 5},
		{ID: "systemd-host-agent-env", Kind: "configuration", Path: filepath.Join(root, "missing-agent.env")},
		{ID: "systemd-host-agent", Kind: "service", Path: filepath.Join(root, "missing-agent.service")},
	}
	adoption, err := productionLegacyBridgeAdoption(ctx, base, sources, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(adoption.Preparations) != 1 || adoption.Preparations[0].PreparationID != preparationID ||
		!adoption.Preparations[0].Committed || len(adoption.Preparations[0].CleanupDebtIDs) != 1 ||
		adoption.Preparations[0].CleanupDebtIDs[0] != "prepared-cleanup" ||
		adoption.Preparations[0].AdapterPID != 4401 || adoption.Preparations[0].LifecyclePID != 4402 ||
		adoption.Preparations[0].AdapterStrongStart != "adapter-strong" ||
		adoption.Preparations[0].LifecycleStrongStart != "lifecycle-strong" {
		t.Fatalf("prepared launch metadata = %+v", adoption.Preparations)
	}
	var turnDebt *DebtRecord
	var cleanupDebt *DebtRecord
	for index := range adoption.Debt {
		if adoption.Debt[index].CauseCode == "uncollected_or_nonterminal_turn" {
			t.Fatalf("terminal uncollected work collapsed into lossy lane debt: %+v", adoption.Debt[index])
		}
		if adoption.Debt[index].ResourceKind == "legacy_terminal_turn" {
			turnDebt = &adoption.Debt[index]
		}
		if adoption.Debt[index].DebtID == "prepared-cleanup" {
			cleanupDebt = &adoption.Debt[index]
		}
	}
	if cleanupDebt == nil || !strings.Contains(cleanupDebt.CauseDetail, "owner_kind=service_key") ||
		!strings.Contains(cleanupDebt.CauseDetail, "expected_path=/profiles/claude/service.key") ||
		!strings.Contains(cleanupDebt.CauseDetail, "expected_pid=4401") {
		t.Fatalf("closed-list cleanup provenance = %+v", cleanupDebt)
	}
	if turnDebt == nil || turnDebt.ResourceIdentity != "qwen/pending-turn-lane/terminal-uncollected/accepted-message/native-qwen-session" ||
		!strings.Contains(turnDebt.CauseDetail, "collection_cursor=previous-turn") ||
		!strings.Contains(turnDebt.CauseDetail, "result_reference=terminal-revision-7") {
		t.Fatalf("terminal uncollected turn debt = %+v", turnDebt)
	}
	if _, err := StageLegacyAdoption(ctx, adoption); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(adoption)
	if strings.Contains(string(encoded), legacyMetadataOnlyCanary) {
		t.Fatalf("preparation or uncollected turn retained content/error: %s", encoded)
	}
}

func TestProductionLegacyStoppedConfigurationProjectsClosedKnownValues(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_500_000)
	base := productionDeliveryAdoptionBase(t, observedAt)
	root := t.TempDir()
	envPath := filepath.Join(root, "peer-federator", "agent.env")
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := strings.Join([]string{
		"PEER_FEDERATOR_HUB=hub.example.test:7443",
		"PEER_FEDERATOR_HOST=legacy-config-host",
		"PEER_FEDERATOR_NAME=legacy-workstation",
		"PEER_FEDERATOR_ENABLE_REMOTE_LANES=true",
		"PEER_FEDERATOR_CODEX_LANE=/opt/agent-sessions/codex-peer-lane",
		"QWEN_PEER_QWEN_BIN=qwen",
		"QWEN_HOME=/profiles/qwen",
		"CLAUDE_CONFIG_DIR=/profiles/claude",
		"UNRELATED_SECRET=" + legacyMetadataOnlyCanary,
	}, "\n") + "\n"
	if err := os.WriteFile(envPath, []byte(environment), 0o600); err != nil {
		t.Fatal(err)
	}
	adoption, err := productionLegacyBridgeAdoption(ctx, base, []LegacyInventorySource{{
		ID: "systemd-host-agent-env", Kind: "configuration", Path: envPath,
	}}, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	configuration := adoption.Configuration
	if configuration == nil || adoption.HostID != "legacy-config-host" || configuration.HostID != "legacy-config-host" ||
		configuration.HostName != "legacy-workstation" ||
		configuration.HubAddress != "hub.example.test:7443" || !configuration.RemoteLanesEnabled ||
		configuration.LaneExecutables["codex"] != "/opt/agent-sessions/codex-peer-lane" ||
		configuration.ProductOverrides["qwen"].Executable != "qwen" ||
		configuration.ProfileSelections["qwen_home"] != "/profiles/qwen" {
		t.Fatalf("stopped host configuration = %+v", configuration)
	}
	if _, err := StageLegacyAdoption(ctx, adoption); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(adoption)
	if strings.Contains(string(encoded), legacyMetadataOnlyCanary) {
		t.Fatalf("unknown configuration value was retained: %s", encoded)
	}
	changedEnvironment := strings.Replace(environment, legacyMetadataOnlyCanary, "CHANGED_UNKNOWN_SECRET", 1)
	if err := os.WriteFile(envPath, []byte(changedEnvironment), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := productionLegacyBridgeAdoption(ctx, base, []LegacyInventorySource{{
		ID: "systemd-host-agent-env", Kind: "configuration", Path: envPath,
	}}, observedAt+1)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SourceRevision != adoption.SourceRevision || changed.Configuration.SourceRevision != configuration.SourceRevision {
		t.Fatalf("unknown configuration changed metadata identity: first=%+v second=%+v", adoption.Configuration, changed.Configuration)
	}

	plistPath := filepath.Join(root, "net.antst.peer-federator.agent.plist")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>net.antst.peer-federator.agent</string>
<key>ProgramArguments</key><array>
<string>/opt/agent-sessions/peer-federator</string><string>agent</string>
	<string>--host</string><string>legacy-config-host</string>
<string>--name</string><string>darwin-workstation</string>
<string>--hub</string><string>hub.example.test:7443</string>
<string>--enable-remote-lanes</string>
</array>
<key>EnvironmentVariables</key><dict>
<key>QWEN_HOME</key><string>/profiles/darwin-qwen</string>
<key>UNRELATED_SECRET</key><string>` + legacyMetadataOnlyCanary + `</string>
</dict></dict></plist>`
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	darwin, err := productionLegacyLaunchdHostConfiguration(plistPath, observedAt)
	if err != nil || darwin.HostID != "legacy-config-host" || !darwin.RemoteLanesEnabled ||
		darwin.ProfileSelections["qwen_home"] != "/profiles/darwin-qwen" {
		t.Fatalf("launchd configuration = %+v, %v", darwin, err)
	}
	darwinJSON, _ := json.Marshal(darwin)
	if strings.Contains(string(darwinJSON), legacyMetadataOnlyCanary) {
		t.Fatalf("launchd projection retained unknown environment: %s", darwinJSON)
	}

	plan, err := StageLegacyAdoption(ctx, adoption)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "unified")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const migrationID = "migration-stopped-configuration"
	selectProductionDeliveryMigration(t, ctx, state, migrationID)
	if _, err := CommitLegacyAdoption(ctx, state, migrationID, plan); err != nil {
		t.Fatal(err)
	}
	commitProductionDeliveryMigration(t, ctx, state, migrationID)
	paths := ProductionPaths{StateRoot: stateRoot, RuntimeRoot: filepath.Join(root, "runtime")}
	runtime := &Runtime{options: RuntimeOptions{
		State: state, Paths: paths,
		Configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion, HostID: base.HostID, HostName: "synthetic-name",
			StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
		},
	}}
	if err := runtime.applyAdoptedHostConfiguration(ctx); err != nil {
		t.Fatal(err)
	}
	effective := runtime.options.Configuration
	if effective.HostID != "legacy-config-host" || effective.HostName != "legacy-workstation" ||
		effective.HubAddress != "hub.example.test:7443" || !effective.RemoteLanesEnabled ||
		effective.ProductOverrides["qwen"].Profile != "/profiles/qwen" {
		t.Fatalf("runtime did not apply exact adopted configuration before routing: %+v", effective)
	}
}

func TestProductionLegacyDormantHostWithoutConfigurationIsDebtButNoAgentIsClean(t *testing.T) {
	ctx := context.Background()
	const observedAt = int64(1_800_000_600_000)
	base := productionDeliveryAdoptionBase(t, observedAt)
	root := t.TempDir()
	agentsRoot := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(agentsRoot, base.HostID), 0o700); err != nil {
		t.Fatal(err)
	}
	sources := []LegacyInventorySource{
		{ID: "host-agent-state", Kind: "state", Path: agentsRoot, Target: true, MaxDepth: 5},
		{ID: "systemd-host-agent-env", Kind: "configuration", Path: filepath.Join(root, "missing-agent.env")},
		{ID: "systemd-host-agent", Kind: "service", Path: filepath.Join(root, "missing-agent.service")},
	}
	dormant, err := productionLegacyBridgeAdoption(ctx, base, sources, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if dormant.Configuration != nil || len(dormant.Debt) != 1 ||
		dormant.Debt[0].ResourceKind != "legacy_host_configuration" ||
		dormant.Debt[0].CauseCode != "legacy_host_configuration_missing" {
		t.Fatalf("manual/stopped host missing exact config = %+v", dormant)
	}
	if _, err := StageLegacyAdoption(ctx, dormant); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(agentsRoot, base.HostID)); err != nil {
		t.Fatal(err)
	}
	clean, err := productionLegacyBridgeAdoption(ctx, base, sources, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Configuration != nil || len(clean.Debt) != 0 {
		t.Fatalf("true no-agent estate gained synthetic config debt: %+v", clean)
	}
}

func productionDeliveryAdoptionBase(t *testing.T, observedAt int64) LegacyAdoptionRequest {
	t.Helper()
	request, err := productionSupervisorOnlyAdoption(nil, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func selectProductionDeliveryMigration(
	t *testing.T,
	ctx context.Context,
	state *StateStore,
	migrationID string,
) {
	t.Helper()
	const candidateID = "candidate-verified-absent"
	journal := MigrationTransaction{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID,
		FromVersions: []string{"legacy-test"}, TargetRuntimeIdentity: "sha256:adoption-test-runtime",
		State: MigrationStateAdopting, Candidates: []string{candidateID},
		MaintenanceWindowState:    MaintenanceWindowLegacyAbsenceVerified,
		VerifiedAbsentAuthorities: []string{candidateID}, Revision: 1,
		StartedAt: 1_800_000_000_000, UpdatedAt: 1_800_000_000_000,
	}
	if _, err := state.CompareAndSwapMigration(ctx, 0, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectCurrentMigration(ctx, 0, MigrationCurrent{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID,
	}); err != nil {
		t.Fatal(err)
	}
}

func commitProductionDeliveryMigration(t *testing.T, ctx context.Context, state *StateStore, migrationID string) {
	t.Helper()
	if err := CommitFirstMigrationAuthority(
		ctx, state, migrationID, "sha256:adoption-test-runtime", 7, 1_800_000_000_001,
	); err != nil {
		t.Fatal(err)
	}
}

func productionAdoptionAttachmentRegistry(t *testing.T, state *StateStore, hostID string) *AttachmentRegistry {
	t.Helper()
	actor := &attachmentTestAdapter{}
	registry, err := NewAttachmentRegistry(AttachmentRegistryOptions{
		State: state, Generation: 9, HostID: hostID, Now: attachmentTestClock(),
		Capability: func() (string, error) { return "migration-delivery-capability", nil },
		Adapters: map[string]AttachmentAdapter{
			"codex": actor, "claude": actor, "grok": actor, "qwen": actor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func writeProductionAdoptionJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
