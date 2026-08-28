package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

type productionLegacyDeliveryAccumulator struct {
	deliveryIndexes map[string]int
	notices         map[string]struct{}
	cursors         map[string]int
	preparations    map[string]struct{}
	debt            map[string]struct{}
}

// productionLegacyAdoptDeliveriesAndPreparations inventories the Agent
// Sessions-owned STATE-01/STATE-02 inbox+wake ledgers and STATE-03 committed
// preparation journals. It does not enter a vendor profile, transcript,
// history database, lane content log, or result stream.
func productionLegacyAdoptDeliveriesAndPreparations(
	ctx context.Context,
	bridgeRoot string,
	sources []LegacyInventorySource,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
) error {
	state := newProductionLegacyDeliveryAccumulator(*request)
	// Configuration is the stopped host's exact identity authority. Project it
	// before destinations and preparation paths so no accepted work is bound to
	// a synthesized clean-install host identity.
	if err := productionLegacyAdoptStoppedHostConfiguration(ctx, sources, observedAt, request, accumulator, state); err != nil {
		return err
	}
	if bridgeRoot != "" {
		if err := productionLegacyAdoptPendingInboxes(ctx, bridgeRoot, observedAt, request, accumulator, state); err != nil {
			return err
		}
		if err := productionLegacyAdoptWakeLedgers(ctx, bridgeRoot, observedAt, request, accumulator, state); err != nil {
			return err
		}
	}
	if err := productionLegacyAdoptPreparationJournals(ctx, sources, observedAt, request, accumulator, state); err != nil {
		return err
	}
	for index := range request.Deliveries {
		request.Deliveries[index].RequestDigest = productionMigratedDeliveryDigest(request.Deliveries[index])
	}
	productionLegacyApplyDeliveryCursors(request)
	return nil
}

func newProductionLegacyDeliveryAccumulator(request LegacyAdoptionRequest) *productionLegacyDeliveryAccumulator {
	result := &productionLegacyDeliveryAccumulator{
		deliveryIndexes: make(map[string]int), notices: make(map[string]struct{}),
		cursors: make(map[string]int), preparations: make(map[string]struct{}), debt: make(map[string]struct{}),
	}
	for index, delivery := range request.Deliveries {
		result.deliveryIndexes[delivery.MessageID] = index
	}
	for _, notice := range request.DeliveryNotices {
		result.notices[notice.NoticeID] = struct{}{}
	}
	for index, cursor := range request.DeliveryCursors {
		result.cursors[legacySessionKey(cursor.Product, cursor.SessionID)] = index
	}
	for _, preparation := range request.Preparations {
		result.preparations[preparation.PreparationID] = struct{}{}
	}
	for _, debt := range request.Debt {
		result.debt[debt.DebtID] = struct{}{}
	}
	return result
}

//nolint:gocyclo // Every filesystem identity and envelope validation boundary becomes distinct scoped debt.
func productionLegacyAdoptPendingInboxes(
	ctx context.Context,
	bridgeRoot string,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
	state *productionLegacyDeliveryAccumulator,
) error {
	sessionsRoot := filepath.Join(bridgeRoot, "sessions")
	entries, err := readProductionLegacyDirectory(sessionsRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	byKey := make(map[string]LegacySessionRecord, len(request.Sessions))
	for _, session := range request.Sessions {
		byKey[sessionkey.FromID(session.SessionID)] = session
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		pending := filepath.Join(sessionsRoot, entry.Name(), "inbox", "pending")
		items, readErr := readProductionLegacyDirectory(pending)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return readErr
		}
		var jsonItems []os.DirEntry
		for _, item := range items {
			if !item.IsDir() && filepath.Ext(item.Name()) == ".json" {
				jsonItems = append(jsonItems, item)
			}
		}
		if len(jsonItems) == 0 {
			continue
		}
		session, known := byKey[entry.Name()]
		if !known {
			productionLegacyAppendDeliveryDebt(request, state, DebtRecord{
				RecordHeader: productionLegacyMigrationHeader(observedAt),
				DebtID:       quiescenceRecordID("legacy-inbox-debt", entry.Name()), Operation: "migration_reconcile",
				ResourceKind: "legacy_pending_inbox", ResourceIdentity: entry.Name(),
				CauseCode:       "session_identity_unresolved",
				RetryPredicate:  "match the exact Agent Sessions session key to a durable catalog or wake identity",
				ProhibitedScope: "do not infer a session from vendor transcripts, profiles, content logs, or process names",
			})
			continue
		}
		for _, item := range jsonItems {
			path := filepath.Join(pending, item.Name())
			record, readErr := readProductionLegacyProjection(path)
			if readErr != nil {
				productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
					"legacy_pending_inbox", session.SessionID, item.Name(), observedAt,
				))
				continue
			}
			messageID := productionLegacyRawString(record, "id")
			if !durableRecordID.MatchString(messageID) || !strings.HasSuffix(item.Name(), "-"+messageID+".json") {
				productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
					"legacy_pending_inbox", session.SessionID, item.Name(), observedAt,
				))
				continue
			}
			revision, revisionErr := productionLegacyMetadataRevision(record)
			if revisionErr != nil {
				return revisionErr
			}
			if err := productionLegacyAdoptDeliveryEnvelope(
				record, "pending_inbox", legacyDeliveryDispositionPending,
				revision, productionLegacyInboxTime(item.Name(), record, observedAt), session, request, state,
			); err != nil {
				productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
					"legacy_pending_inbox", session.SessionID, item.Name(), observedAt,
				))
				continue
			}
			accumulator.revisions = append(accumulator.revisions, revision)
		}
	}
	return nil
}

func productionLegacyAdoptWakeLedgers(
	ctx context.Context,
	bridgeRoot string,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
	state *productionLegacyDeliveryAccumulator,
) error {
	profiles, err := readProductionLegacyDirectory(filepath.Join(bridgeRoot, "profiles"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !profile.IsDir() {
			continue
		}
		root := filepath.Join(bridgeRoot, "profiles", profile.Name())
		for _, family := range []struct {
			directory string
			product   string
			source    string
		}{
			{directory: "wakes", product: "codex", source: "codex_wake"},
			{directory: "grok-wakes", product: "grok", source: "grok_wake"},
		} {
			if err := productionLegacyAdoptWakeDirectory(
				ctx, filepath.Join(root, family.directory), family.product, family.source, observedAt,
				request, accumulator, state,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

//nolint:gocyclo // Wake states deliberately map through independent replay, terminal, and ambiguity paths.
func productionLegacyAdoptWakeDirectory(
	ctx context.Context,
	root string,
	product string,
	sourceKind string,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
	state *productionLegacyDeliveryAccumulator,
) error {
	sessionDirectories, err := readProductionLegacyDirectory(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, directory := range sessionDirectories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !directory.IsDir() {
			continue
		}
		entries, readErr := readProductionLegacyDirectory(filepath.Join(root, directory.Name()))
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			record, readErr := readProductionLegacyProjection(filepath.Join(root, directory.Name(), entry.Name()))
			if readErr != nil {
				productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
					"legacy_wake", directory.Name(), entry.Name(), observedAt,
				))
				continue
			}
			sessionID := productionLegacyRawString(record, "sessionId")
			messageID := productionLegacyRawString(record, "messageId")
			if !durableRecordID.MatchString(sessionID) || !durableRecordID.MatchString(messageID) ||
				directory.Name() != sessionkey.FromID(sessionID) || entry.Name() != sessionkey.FromID(messageID)+".json" {
				productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
					"legacy_wake", directory.Name(), entry.Name(), observedAt,
				))
				continue
			}
			revision, revisionErr := productionLegacyWakeMetadataRevision(record)
			if revisionErr != nil {
				return revisionErr
			}
			updatedAt := productionLegacyRawInt64(record, "updatedAt")
			if updatedAt <= 0 {
				updatedAt = observedAt
			}
			session := productionLegacyEnsureDeliverySession(request, sessionID, product, updatedAt)
			stateValue, disposition := productionLegacyWakeDisposition(
				sourceKind, productionLegacyRawString(record, "state"), productionLegacyRawString(record, "delivery"),
			)
			if disposition == "" {
				productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
					"legacy_wake", sessionID, messageID, updatedAt,
				))
				continue
			}
			if disposition == legacyDeliveryDispositionPending {
				var item map[string]json.RawMessage
				if len(record["item"]) == 0 || json.Unmarshal(record["item"], &item) != nil {
					productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
						"legacy_wake", sessionID, messageID, updatedAt,
					))
					continue
				}
				if err := productionLegacyAdoptDeliveryEnvelope(
					item, sourceKind, disposition, revision, updatedAt, session, request, state,
				); err != nil {
					productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
						"legacy_wake", sessionID, messageID, updatedAt,
					))
					continue
				}
			}
			if disposition == legacyDeliveryDispositionAmbiguous {
				productionLegacyAppendDeliveryDebt(request, state, DebtRecord{
					RecordHeader: productionLegacyMigrationHeader(updatedAt),
					DebtID:       quiescenceRecordID("legacy-delivery-ambiguous", product, sessionID, messageID),
					Operation:    "migration_reconcile", ResourceKind: "legacy_delivery_ambiguous",
					ResourceIdentity: product + "/" + sessionID + "/" + messageID,
					ExpectedRevision: revision, CauseCode: "legacy_delivery_outcome_ambiguous",
					RetryPredicate:  "reconcile the exact Agent Sessions-owned wake acceptance without redispatch",
					ProhibitedScope: "do not read or copy vendor transcripts, result/error bodies, profiles, or content logs",
				})
			}
			productionLegacyAppendDeliveryNotice(request, state, LegacyDeliveryNotice{
				NoticeID:  quiescenceRecordID("legacy-delivery-notice", sourceKind, sessionID, messageID),
				SessionID: sessionID, Product: product, MessageID: messageID, SourceKind: sourceKind,
				SourceState: stateValue, Disposition: disposition,
				Fingerprint: productionLegacyRawString(record, "fingerprint"), SourceRevision: revision, UpdatedAt: updatedAt,
			})
			productionLegacyAdvanceDeliveryCursor(request, state, LegacyDeliveryCursor{
				SessionID: sessionID, Product: product, MessageID: messageID, SourceRevision: revision, UpdatedAt: updatedAt,
			})
			accumulator.revisions = append(accumulator.revisions, revision)
		}
	}
	return nil
}

//nolint:gocyclo // Exact source, payload, destination, and duplicate-envelope checks remain explicit.
func productionLegacyAdoptDeliveryEnvelope(
	record map[string]json.RawMessage,
	sourceKind string,
	disposition string,
	sourceRevision string,
	acceptedAt int64,
	session LegacySessionRecord,
	request *LegacyAdoptionRequest,
	state *productionLegacyDeliveryAccumulator,
) error {
	messageID := productionLegacyRawString(record, "id")
	content := productionLegacyRawString(record, "message")
	from := strings.TrimSpace(productionLegacyRawString(record, "from"))
	summary := productionLegacyRawString(record, "summary")
	sentAt := productionLegacyRawString(record, "sentAt")
	if !durableRecordID.MatchString(messageID) || strings.TrimSpace(content) == "" ||
		len(content) > federation.MaxAgentFrameBytes || len(summary) > 64<<10 || len(sentAt) > 512 ||
		from == "" || len(from) > 4096 || acceptedAt <= 0 {
		return errors.New("legacy delivery envelope is incomplete")
	}
	sourceHost := productionLegacyEnvelopeSourceHost(record)
	sourceSession := productionLegacyEnvelopeSourceSession(record)
	if sourceHost == "" || sourceSession == "" || len(sourceHost) > 4096 || len(sourceSession) > 4096 {
		return errors.New("legacy delivery envelope has an invalid source identity")
	}
	destination := request.HostID + "/" + session.SessionID
	delivery := DeliveryRecord{
		RecordHeader: productionLegacyMigrationHeader(acceptedAt),
		MessageID:    messageID, SourceHostID: sourceHost,
		SourceSessionID: sourceSession, Operation: DeliveryOperationSend,
		RequestedTargets: []string{destination}, ResolvedDestinations: []string{destination},
		Content: content, Summary: summary,
		SentAt: sentAt, State: DeliveryStateAccepted,
		DestinationResults: map[string]string{destination: DeliveryDestinationPending},
		AcceptedRevision:   1, AcceptedAt: acceptedAt, AdoptedSourceRevision: sourceRevision,
	}
	if index, duplicate := state.deliveryIndexes[messageID]; duplicate {
		current := request.Deliveries[index]
		if current.Content != delivery.Content || current.SourceHostID != delivery.SourceHostID ||
			current.SourceSessionID != delivery.SourceSessionID {
			return fmt.Errorf("legacy message %q has conflicting accepted envelopes", messageID)
		}
		if current.DestinationResults[destination] == DeliveryDestinationPending {
			return nil
		}
		current.Operation = DeliveryOperationMulticast
		current.RequestedTargets = append(current.RequestedTargets, destination)
		current.ResolvedDestinations = append(current.ResolvedDestinations, destination)
		current.DestinationResults[destination] = DeliveryDestinationPending
		sort.Strings(current.RequestedTargets)
		sort.Strings(current.ResolvedDestinations)
		request.Deliveries[index] = current
		return nil
	}
	state.deliveryIndexes[messageID] = len(request.Deliveries)
	request.Deliveries = append(request.Deliveries, delivery)
	productionLegacyAppendDeliveryNotice(request, state, LegacyDeliveryNotice{
		NoticeID:  quiescenceRecordID("legacy-delivery-notice", sourceKind, session.SessionID, messageID),
		SessionID: session.SessionID, Product: session.Product, MessageID: messageID, SourceKind: sourceKind,
		SourceState: DeliveryStateAccepted, Disposition: disposition, SourceRevision: sourceRevision, UpdatedAt: acceptedAt,
	})
	productionLegacyAdvanceDeliveryCursor(request, state, LegacyDeliveryCursor{
		SessionID: session.SessionID, Product: session.Product, MessageID: messageID,
		SourceRevision: sourceRevision, UpdatedAt: acceptedAt,
	})
	return nil
}

func productionLegacyWakeDisposition(sourceKind, state, delivery string) (string, string) {
	value := strings.TrimSpace(delivery)
	if value == "" {
		value = strings.TrimSpace(state)
	}
	sourceState := strings.Trim(strings.Join([]string{state, delivery}, "/"), "/")
	if sourceState == "" {
		return "", ""
	}
	if sourceKind == "grok_wake" {
		switch value {
		case "queued", "accepted":
			return sourceState, legacyDeliveryDispositionPending
		case "in_flight":
			return sourceState, legacyDeliveryDispositionAmbiguous
		case "actor_accepted", "delivered", "failed", "interrupted", "timed_out", "conflict":
			return sourceState, legacyDeliveryDispositionTerminal
		default:
			return sourceState, ""
		}
	}
	switch state {
	case "queueing", "queued":
		return sourceState, legacyDeliveryDispositionPending
	case "in_flight":
		return sourceState, legacyDeliveryDispositionAmbiguous
	case "delivered", "fallback_delivered":
		return sourceState, legacyDeliveryDispositionTerminal
	default:
		switch value {
		case "observed", "started", "steered", "failed", "interrupted", "timed_out", "conflict":
			return sourceState, legacyDeliveryDispositionTerminal
		}
	}
	return sourceState, ""
}

//nolint:gocyclo // STATE-03 provenance and each closed-list cleanup identity are validated independently.
func productionLegacyAdoptPreparationJournals(
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
	state *productionLegacyDeliveryAccumulator,
) error {
	agentsRoot := productionLegacySourcePath(sources, "host-agent-state")
	if agentsRoot == "" || request.HostID == "" {
		return nil
	}
	directory := filepath.Join(agentsRoot, request.HostID, "claude-peer-preparations")
	entries, err := readProductionLegacyDirectory(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, readErr := readProductionLegacyProjection(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
				"legacy_peer_preparation", request.HostID, entry.Name(), observedAt,
			))
			continue
		}
		var registration map[string]json.RawMessage
		if json.Unmarshal(record["registration"], &registration) != nil {
			productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
				"legacy_peer_preparation", request.HostID, entry.Name(), observedAt,
			))
			continue
		}
		sessionID := productionLegacyRawString(registration, "session_id")
		preparationID := productionLegacyRawString(registration, "attachment_id")
		if preparationID == "" {
			preparationID = sessionID
		}
		product := productionLegacyRawString(record, "product")
		if product == "" {
			product = productionLegacyRawString(registration, "product")
		}
		adapterPID := productionLegacyRawInt64(registration, "pid")
		lifecyclePID := productionLegacyRawInt64(registration, "lifecycle_pid")
		adapterStart := productionLegacyRawString(registration, "proc_start")
		lifecycleStart := productionLegacyRawString(registration, "lifecycle_proc_start")
		adapterStrong := productionLegacyRawString(registration, "adapter_strong_start")
		lifecycleStrong := productionLegacyRawString(registration, "lifecycle_strong_start")
		adapterSocket := productionLegacyRawString(registration, "socket")
		lifecycleRoot := productionLegacyRawString(registration, "lifecycle_root")
		if productionLegacyRawInt64(record, "version") != 1 ||
			productionLegacyRawInt64(registration, "version") != federation.GroupProtocolVersion ||
			productionLegacyRawString(registration, "product") != product ||
			!durableRecordID.MatchString(sessionID) || !durableRecordID.MatchString(preparationID) ||
			entry.Name() != sessionkey.FromID(preparationID)+".json" ||
			adapterPID <= 1 || adapterPID > 1<<30 || lifecyclePID <= 1 || lifecyclePID > 1<<30 ||
			strings.TrimSpace(adapterStart) == "" || strings.TrimSpace(lifecycleStart) == "" ||
			strings.TrimSpace(adapterStrong) == "" || strings.TrimSpace(lifecycleStrong) == "" ||
			len(adapterStart) > 4096 || len(lifecycleStart) > 4096 ||
			len(adapterStrong) > 4096 || len(lifecycleStrong) > 4096 ||
			adapterSocket != "" && !migrationAbsoluteCleanPath(adapterSocket) ||
			lifecycleRoot != "" && !migrationAbsoluteCleanPath(lifecycleRoot) ||
			len(adapterSocket) > 4096 || len(lifecycleRoot) > 4096 {
			productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
				"legacy_peer_preparation", request.HostID, entry.Name(), observedAt,
			))
			continue
		}
		if !isLegacyPreparationProduct(product) {
			productionLegacyAppendDeliveryDebt(request, state, productionLegacyMalformedDeliveryDebt(
				"legacy_peer_preparation", request.HostID, entry.Name(), observedAt,
			))
			continue
		}
		revision, revisionErr := productionLegacyPreparationMetadataRevision(record)
		if revisionErr != nil {
			return revisionErr
		}
		updatedAt := productionLegacyRawInt64(registration, "started_at")
		if updatedAt <= 0 {
			updatedAt = observedAt
		}
		var cleanup []map[string]json.RawMessage
		_ = json.Unmarshal(record["cleanup_debt"], &cleanup)
		debtIDs := make([]string, 0, len(cleanup)+1)
		for index, rawDebt := range cleanup {
			debtID := productionLegacyRawString(rawDebt, "debt_id")
			if !durableRecordID.MatchString(debtID) {
				debtID = quiescenceRecordID("legacy-preparation-debt", preparationID, strconv.Itoa(index))
			}
			if !productionLegacyPreparationCleanupValid(rawDebt, product) {
				debt := productionLegacyMalformedDeliveryDebt(
					"legacy_peer_preparation_cleanup", preparationID, strconv.Itoa(index), updatedAt,
				)
				debt.DebtID = debtID
				productionLegacyAppendDeliveryDebt(request, state, debt)
				debtIDs = append(debtIDs, debtID)
				continue
			}
			debt := productionLegacyPreparationDebt(rawDebt, debtID, product, preparationID, revision, updatedAt)
			productionLegacyAppendDeliveryDebt(request, state, debt)
			debtIDs = append(debtIDs, debtID)
		}
		committed := productionLegacyRawBool(record, "committed")
		if len(cleanup) == 0 {
			debtID := quiescenceRecordID("legacy-preparation-debt", preparationID, "reconcile")
			productionLegacyAppendDeliveryDebt(request, state, DebtRecord{
				RecordHeader: productionLegacyMigrationHeader(updatedAt), DebtID: debtID,
				Operation: "migration_reconcile", ResourceKind: "legacy_peer_preparation",
				ResourceIdentity: product + "/" + preparationID, ExpectedRevision: revision,
				CauseCode:       "legacy_preparation_requires_cleanup",
				RetryPredicate:  "re-attest the exact closed-list Agent Sessions lifecycle artifacts before cleanup",
				ProhibitedScope: "do not read or mutate vendor profiles, transcripts, credentials, settings, or content logs",
			})
			debtIDs = append(debtIDs, debtID)
		}
		debtIDs = sortedUniqueStrings(debtIDs)
		if _, duplicate := state.preparations[preparationID]; duplicate {
			return fmt.Errorf("legacy preparation %q is repeated", preparationID)
		}
		state.preparations[preparationID] = struct{}{}
		request.Preparations = append(request.Preparations, LegacyPreparationRecord{
			PreparationID: preparationID, SessionID: sessionID, Product: product,
			AdapterPID: int(adapterPID), AdapterProcStart: adapterStart,
			AdapterStrongStart: adapterStrong,
			AdapterSocket:      adapterSocket, LifecyclePID: int(lifecyclePID), LifecycleProcStart: lifecycleStart,
			LifecycleStrongStart: lifecycleStrong,
			LifecycleRoot:        lifecycleRoot, Committed: committed,
			CleanupDebtIDs: debtIDs, SourceRevision: revision, UpdatedAt: updatedAt,
		})
		accumulator.revisions = append(accumulator.revisions, revision)
	}
	return nil
}

//nolint:gocyclo // Closed-list cleanup provenance fields are independently bounded and validated.
func productionLegacyPreparationCleanupValid(record map[string]json.RawMessage, product string) bool {
	bounded := func(value string, maximum int) bool { return value != "" && len(value) <= maximum }
	if productionLegacyRawInt64(record, "version") != 1 || productionLegacyRawString(record, "product") != product ||
		!durableRecordID.MatchString(productionLegacyRawString(record, "debt_id")) ||
		!bounded(productionLegacyRawString(record, "revision"), 4096) ||
		!bounded(productionLegacyRawString(record, "owner_kind"), 128) ||
		!bounded(productionLegacyRawString(record, "owner_id"), 4096) ||
		!bounded(productionLegacyRawString(record, "operation"), 128) ||
		!bounded(productionLegacyRawString(record, "observation_state"), 128) ||
		productionLegacyRawInt64(record, "expected_pid") < 0 ||
		productionLegacyRawInt64(record, "attempts") < 0 || productionLegacyRawInt64(record, "updated_at") <= 0 {
		return false
	}
	for _, key := range []string{"expected_start", "expected_strong_start", "expected_digest", "terminal_when_clean"} {
		if value := productionLegacyRawString(record, key); len(value) > 4096 {
			return false
		}
	}
	if path := productionLegacyRawString(record, "expected_path"); path != "" && !migrationAbsoluteCleanPath(path) {
		return false
	}
	return true
}

func productionLegacyPreparationDebt(
	record map[string]json.RawMessage,
	debtID string,
	product string,
	preparationID string,
	sourceRevision string,
	updatedAt int64,
) DebtRecord {
	operation := productionLegacyRawString(record, "operation")
	if operation == "" {
		operation = "migration_reconcile"
	}
	resourceIdentity := product + "/" + preparationID
	if owner := productionLegacyRawString(record, "owner_id"); owner != "" {
		resourceIdentity += "/" + owner
	}
	expected := productionLegacyRawString(record, "revision")
	if expected == "" {
		expected = productionLegacyRawString(record, "expected_digest")
	}
	if expected == "" {
		expected = sourceRevision
	}
	cause := productionLegacyRawString(record, "observation_state")
	if cause == "" {
		cause = "legacy_cleanup_debt"
	}
	detail := productionLegacyCleanupDebtDetail(record)
	return DebtRecord{
		RecordHeader: productionLegacyMigrationHeader(updatedAt), DebtID: debtID, Operation: operation,
		ResourceKind: "legacy_peer_preparation", ResourceIdentity: resourceIdentity,
		ExpectedRevision: expected, CauseCode: cause,
		CauseDetail:     detail,
		RetryPredicate:  "re-attest the exact closed-list Agent Sessions lifecycle identity and retry cleanup",
		ProhibitedScope: "do not copy cleanup errors or inspect vendor profiles, transcripts, credentials, or content logs",
	}
}

func productionLegacyCleanupDebtDetail(record map[string]json.RawMessage) string {
	parts := make([]string, 0, 10)
	for _, field := range []struct {
		key   string
		label string
	}{
		{"owner_kind", "owner_kind"}, {"owner_id", "owner_id"},
		{"expected_path", "expected_path"}, {"expected_start", "expected_start"},
		{"expected_strong_start", "expected_strong_start"}, {"expected_digest", "expected_digest"},
		{"terminal_when_clean", "terminal_when_clean"},
	} {
		if value := productionLegacyRawString(record, field.key); value != "" && len(value) <= 4096 {
			parts = append(parts, field.label+"="+value)
		}
	}
	for _, field := range []struct {
		key   string
		label string
	}{{"expected_pid", "expected_pid"}, {"attempts", "attempts"}, {"updated_at", "updated_at"}} {
		if value := productionLegacyRawInt64(record, field.key); value != 0 {
			parts = append(parts, field.label+"="+strconv.FormatInt(value, 10))
		}
	}
	return strings.Join(parts, "; ")
}

func productionLegacyWakeMetadataRevision(record map[string]json.RawMessage) (string, error) {
	metadata := make(map[string]json.RawMessage, len(record))
	for key, value := range record {
		if strings.EqualFold(key, "item") || strings.EqualFold(key, "error") {
			continue
		}
		metadata[key] = value
	}
	return productionLegacyMetadataRevision(metadata)
}

func productionLegacyPreparationMetadataRevision(record map[string]json.RawMessage) (string, error) {
	metadata := make(map[string]json.RawMessage, len(record))
	for key, value := range record {
		if strings.EqualFold(key, "product_payload") {
			continue
		}
		metadata[key] = value
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return "", err
	}
	productionStripLegacyContent(value)
	productionStripLegacyPreparationErrors(value)
	body, err = json.Marshal(value)
	if err != nil {
		return "", err
	}
	return "sha256:" + productionDigest(body), nil
}

func productionStripLegacyPreparationErrors(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "last_error") {
				delete(typed, key)
				continue
			}
			productionStripLegacyPreparationErrors(child)
		}
	case []any:
		for _, child := range typed {
			productionStripLegacyPreparationErrors(child)
		}
	}
}

func productionLegacyAppendDeliveryNotice(
	request *LegacyAdoptionRequest,
	state *productionLegacyDeliveryAccumulator,
	notice LegacyDeliveryNotice,
) {
	if _, duplicate := state.notices[notice.NoticeID]; duplicate {
		for index := range request.DeliveryNotices {
			if request.DeliveryNotices[index].NoticeID == notice.NoticeID &&
				notice.UpdatedAt >= request.DeliveryNotices[index].UpdatedAt {
				request.DeliveryNotices[index] = notice
				break
			}
		}
		return
	}
	state.notices[notice.NoticeID] = struct{}{}
	request.DeliveryNotices = append(request.DeliveryNotices, notice)
}

func productionLegacyAdvanceDeliveryCursor(
	request *LegacyAdoptionRequest,
	state *productionLegacyDeliveryAccumulator,
	cursor LegacyDeliveryCursor,
) {
	key := legacySessionKey(cursor.Product, cursor.SessionID)
	if index, exists := state.cursors[key]; exists {
		current := request.DeliveryCursors[index]
		if current.UpdatedAt > cursor.UpdatedAt || current.UpdatedAt == cursor.UpdatedAt && current.MessageID >= cursor.MessageID {
			return
		}
		request.DeliveryCursors[index] = cursor
		return
	}
	state.cursors[key] = len(request.DeliveryCursors)
	request.DeliveryCursors = append(request.DeliveryCursors, cursor)
}

func productionLegacyApplyDeliveryCursors(request *LegacyAdoptionRequest) {
	bySession := make(map[string]LegacyDeliveryCursor, len(request.DeliveryCursors))
	for _, cursor := range request.DeliveryCursors {
		bySession[legacySessionKey(cursor.Product, cursor.SessionID)] = cursor
	}
	for index := range request.Sessions {
		cursor, ok := bySession[legacySessionKey(request.Sessions[index].Product, request.Sessions[index].SessionID)]
		if !ok {
			continue
		}
		request.Sessions[index].DeliveryCursor = cursor.MessageID
		if request.Sessions[index].UpdatedAt < cursor.UpdatedAt {
			request.Sessions[index].UpdatedAt = cursor.UpdatedAt
		}
	}
}

func productionLegacyEnsureDeliverySession(
	request *LegacyAdoptionRequest,
	sessionID string,
	product string,
	updatedAt int64,
) LegacySessionRecord {
	for _, session := range request.Sessions {
		if session.SessionID == sessionID && session.Product == product {
			return session
		}
	}
	kind := federation.SessionKindInteractive
	permission := "default"
	for _, lane := range request.Lanes {
		if lane.LaneSessionID == sessionID && lane.Product == product {
			kind, permission = federation.SessionKindLane, lane.PermissionMode
			break
		}
	}
	session := LegacySessionRecord{
		SessionID: sessionID, Product: product, Kind: kind, PermissionMode: permission, UpdatedAt: updatedAt,
	}
	request.Sessions = append(request.Sessions, session)
	return session
}

func productionLegacyAppendDeliveryDebt(
	request *LegacyAdoptionRequest,
	state *productionLegacyDeliveryAccumulator,
	debt DebtRecord,
) {
	if _, duplicate := state.debt[debt.DebtID]; duplicate {
		return
	}
	state.debt[debt.DebtID] = struct{}{}
	request.Debt = append(request.Debt, debt)
}

func productionLegacyMalformedDeliveryDebt(kind, sessionID, recordID string, observedAt int64) DebtRecord {
	return DebtRecord{
		RecordHeader: productionLegacyMigrationHeader(observedAt),
		DebtID:       quiescenceRecordID("legacy-delivery-debt", kind, sessionID, recordID),
		Operation:    "migration_reconcile", ResourceKind: kind,
		ResourceIdentity: sessionID + "/" + recordID, CauseCode: "malformed_owned_delivery_record",
		RetryPredicate:  "repair or remove the exact Agent Sessions-owned record and re-run inventory",
		ProhibitedScope: "do not infer payload or identity from vendor transcripts, profiles, content logs, or process names",
	}
}

func productionLegacyMigrationHeader(timestamp int64) RecordHeader {
	if timestamp <= 0 {
		timestamp = 1
	}
	return RecordHeader{
		SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: 1,
		CreatedAt: timestamp, UpdatedAt: timestamp,
	}
}

func productionLegacyInboxTime(filename string, record map[string]json.RawMessage, observedAt int64) int64 {
	if separator := strings.IndexByte(filename, '-'); separator > 0 {
		if value, err := strconv.ParseInt(filename[:separator], 10, 64); err == nil && value > 0 {
			return value
		}
	}
	for _, key := range []string{"receivedAt", "sentAt"} {
		if value := productionLegacyRawString(record, key); value != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return observedAt
}

func productionLegacyEnvelopeSourceHost(record map[string]json.RawMessage) string {
	from := strings.TrimSpace(productionLegacyRawString(record, "from"))
	if index := strings.IndexByte(from, '/'); index > 0 && durableRecordID.MatchString(from[:index]) &&
		durableRecordID.MatchString(from[index+1:]) {
		return from[:index]
	}
	return from
}

func productionLegacyEnvelopeSourceSession(record map[string]json.RawMessage) string {
	if value := strings.TrimSpace(productionLegacyRawString(record, "fromSession")); value != "" {
		return value
	}
	from := strings.TrimSpace(productionLegacyRawString(record, "from"))
	if index := strings.IndexByte(from, '/'); index > 0 && durableRecordID.MatchString(from[:index]) &&
		durableRecordID.MatchString(from[index+1:]) {
		return from[index+1:]
	}
	return from
}

func productionMigratedDeliveryDigest(record DeliveryRecord) string {
	identity := struct {
		MessageID     string   `json:"message_id"`
		SourceHost    string   `json:"source_host"`
		SourceSession string   `json:"source_session"`
		Destinations  []string `json:"destinations"`
		Content       string   `json:"content"`
		Summary       string   `json:"summary,omitempty"`
	}{
		MessageID: record.MessageID, SourceHost: record.SourceHostID, SourceSession: record.SourceSessionID,
		Destinations: append([]string(nil), record.ResolvedDestinations...), Content: record.Content, Summary: record.Summary,
	}
	sort.Strings(identity.Destinations)
	body, _ := json.Marshal(identity)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
