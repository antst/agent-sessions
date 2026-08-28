package daemon

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/federation"
)

const (
	legacyDeliveryDispositionPending   = "pending"
	legacyDeliveryDispositionTerminal  = "terminal"
	legacyDeliveryDispositionAmbiguous = "ambiguous"
)

// LegacyDeliveryCursor is the metadata-only progress boundary for one
// dormant session. It carries no message, result, transcript, or error body.
type LegacyDeliveryCursor struct {
	SessionID      string `json:"session_id"`
	Product        string `json:"product"`
	MessageID      string `json:"message_id"`
	SourceRevision string `json:"source_revision"`
	UpdatedAt      int64  `json:"updated_at"`
}

// LegacyDeliveryNotice retains one bridge-owned inbox or wake outcome without
// retaining its payload or diagnostic error text. Disposition, rather than a
// vendor-content probe, determines whether migration may replay the message.
type LegacyDeliveryNotice struct {
	NoticeID       string `json:"notice_id"`
	SessionID      string `json:"session_id"`
	Product        string `json:"product"`
	MessageID      string `json:"message_id"`
	SourceKind     string `json:"source_kind"`
	SourceState    string `json:"source_state"`
	Disposition    string `json:"disposition"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	SourceRevision string `json:"source_revision"`
	UpdatedAt      int64  `json:"updated_at"`
}

// LegacyPreparationRecord is bounded provenance for an Agent Sessions-owned
// host-agent launch journal. Product payloads, artifact bodies, capabilities,
// and cleanup error text are deliberately absent.
type LegacyPreparationRecord struct {
	PreparationID        string   `json:"preparation_id"`
	SessionID            string   `json:"session_id"`
	Product              string   `json:"product"`
	AdapterPID           int      `json:"adapter_pid,omitempty"`
	AdapterProcStart     string   `json:"adapter_proc_start,omitempty"`
	AdapterStrongStart   string   `json:"adapter_strong_start,omitempty"`
	AdapterSocket        string   `json:"adapter_socket,omitempty"`
	LifecyclePID         int      `json:"lifecycle_pid,omitempty"`
	LifecycleProcStart   string   `json:"lifecycle_proc_start,omitempty"`
	LifecycleStrongStart string   `json:"lifecycle_strong_start,omitempty"`
	LifecycleRoot        string   `json:"lifecycle_root,omitempty"`
	Committed            bool     `json:"committed"`
	CleanupDebtIDs       []string `json:"cleanup_debt_ids,omitempty"`
	SourceRevision       string   `json:"source_revision"`
	UpdatedAt            int64    `json:"updated_at"`
}

// LegacyHostConfiguration is the stopped federation agent's exact non-secret
// configuration projection. The source remains revision-bound; no environment
// variable outside this closed field set is copied.
type LegacyHostConfiguration struct {
	HostID             string                     `json:"host_id"`
	HostName           string                     `json:"host_name"`
	HubAddress         string                     `json:"hub_address,omitempty"`
	RemoteLanesEnabled bool                       `json:"remote_lanes_enabled"`
	LaneExecutables    map[string]string          `json:"lane_executables,omitempty"`
	ProductOverrides   map[string]ProductOverride `json:"product_overrides,omitempty"`
	ProfileSelections  map[string]string          `json:"profile_selections,omitempty"`
	SourceKind         string                     `json:"source_kind"`
	SourceRevision     string                     `json:"source_revision"`
	UpdatedAt          int64                      `json:"updated_at"`
}

func validateLegacyDeliveries(snapshot LegacyAdoptionSnapshot) error { //nolint:gocyclo // Each durable delivery link fails independently.
	sessions := make(map[string]LegacySessionRecord, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		sessions[legacySessionKey(session.Product, session.SessionID)] = session
	}
	deliveries := make(map[string]struct{}, len(snapshot.Deliveries))
	for _, delivery := range snapshot.Deliveries {
		if err := validateLegacyDelivery(delivery, snapshot.HostID, sessions); err != nil {
			return err
		}
		if _, duplicate := deliveries[delivery.MessageID]; duplicate {
			return fmt.Errorf("legacy adoption repeats delivery %q", delivery.MessageID)
		}
		deliveries[delivery.MessageID] = struct{}{}
	}
	cursors := make(map[string]struct{}, len(snapshot.DeliveryCursors))
	for _, cursor := range snapshot.DeliveryCursors {
		key := legacySessionKey(cursor.Product, cursor.SessionID)
		if _, exists := sessions[key]; !exists || !durableRecordID.MatchString(cursor.MessageID) ||
			strings.TrimSpace(cursor.SourceRevision) == "" || cursor.UpdatedAt <= 0 {
			return fmt.Errorf("legacy delivery cursor for %q is incomplete", key)
		}
		if _, duplicate := cursors[key]; duplicate {
			return fmt.Errorf("legacy adoption repeats delivery cursor for %q", key)
		}
		cursors[key] = struct{}{}
	}
	notices := make(map[string]struct{}, len(snapshot.DeliveryNotices))
	for _, notice := range snapshot.DeliveryNotices {
		key := legacySessionKey(notice.Product, notice.SessionID)
		if _, exists := sessions[key]; !exists || !durableRecordID.MatchString(notice.NoticeID) ||
			!durableRecordID.MatchString(notice.MessageID) || strings.TrimSpace(notice.SourceKind) == "" ||
			strings.TrimSpace(notice.SourceState) == "" || strings.TrimSpace(notice.SourceRevision) == "" ||
			notice.UpdatedAt <= 0 || !validLegacyDeliveryDisposition(notice.Disposition) {
			return fmt.Errorf("legacy delivery notice %q is incomplete", notice.NoticeID)
		}
		if _, duplicate := notices[notice.NoticeID]; duplicate {
			return fmt.Errorf("legacy adoption repeats delivery notice %q", notice.NoticeID)
		}
		notices[notice.NoticeID] = struct{}{}
		if notice.Disposition == legacyDeliveryDispositionPending {
			if _, exists := deliveries[notice.MessageID]; !exists {
				return fmt.Errorf("pending legacy delivery notice %q lacks accepted work", notice.NoticeID)
			}
		}
	}
	preparations := make(map[string]struct{}, len(snapshot.Preparations))
	debt := make(map[string]struct{}, len(snapshot.Debt))
	for _, item := range snapshot.Debt {
		debt[item.DebtID] = struct{}{}
	}
	for _, preparation := range snapshot.Preparations {
		if !durableRecordID.MatchString(preparation.PreparationID) ||
			!durableRecordID.MatchString(preparation.SessionID) || strings.TrimSpace(preparation.SourceRevision) == "" ||
			preparation.UpdatedAt <= 0 || preparation.AdapterPID <= 1 || preparation.LifecyclePID <= 1 ||
			strings.TrimSpace(preparation.AdapterProcStart) == "" || strings.TrimSpace(preparation.LifecycleProcStart) == "" ||
			strings.TrimSpace(preparation.AdapterStrongStart) == "" || strings.TrimSpace(preparation.LifecycleStrongStart) == "" ||
			len(preparation.AdapterProcStart) > 4096 || len(preparation.LifecycleProcStart) > 4096 ||
			len(preparation.AdapterStrongStart) > 4096 || len(preparation.LifecycleStrongStart) > 4096 ||
			preparation.AdapterSocket != "" && !migrationAbsoluteCleanPath(preparation.AdapterSocket) ||
			preparation.LifecycleRoot != "" && !migrationAbsoluteCleanPath(preparation.LifecycleRoot) ||
			len(preparation.AdapterSocket) > 4096 || len(preparation.LifecycleRoot) > 4096 {
			return fmt.Errorf("legacy preparation %q is incomplete", preparation.PreparationID)
		}
		if !isLegacyPreparationProduct(preparation.Product) {
			return fmt.Errorf("legacy preparation %q has unsupported product %q", preparation.PreparationID, preparation.Product)
		}
		if _, duplicate := preparations[preparation.PreparationID]; duplicate {
			return fmt.Errorf("legacy adoption repeats preparation %q", preparation.PreparationID)
		}
		preparations[preparation.PreparationID] = struct{}{}
		for _, debtID := range preparation.CleanupDebtIDs {
			if _, exists := debt[debtID]; !exists {
				return fmt.Errorf("legacy preparation %q lacks cleanup debt %q", preparation.PreparationID, debtID)
			}
		}
	}
	if snapshot.Configuration != nil {
		configuration := snapshot.Configuration
		if configuration.HostID != snapshot.HostID || strings.TrimSpace(configuration.HostName) == "" ||
			strings.TrimSpace(configuration.SourceKind) == "" || strings.TrimSpace(configuration.SourceRevision) == "" ||
			configuration.UpdatedAt <= 0 || configuration.HubAddress == "" ||
			len(configuration.HostName) > 4096 || len(configuration.SourceKind) > 128 ||
			len(configuration.SourceRevision) > 4096 || validateDaemonHubAddress(configuration.HubAddress) != nil {
			return errors.New("legacy host configuration is incomplete or incompatible")
		}
		for product, executable := range configuration.LaneExecutables {
			if !isFederationProduct(product) || executable == "" || len(executable) > 4096 ||
				filepath.Base(executable) != executable && !migrationAbsoluteCleanPath(executable) {
				return fmt.Errorf("legacy host configuration has invalid %q lane executable", product)
			}
		}
		for product, override := range configuration.ProductOverrides {
			if !isFederationProduct(product) || len(override.Executable) > 4096 || len(override.Profile) > 4096 || override.Executable != "" &&
				filepath.Base(override.Executable) != override.Executable && !migrationAbsoluteCleanPath(override.Executable) ||
				override.Profile != "" && !migrationAbsoluteCleanPath(override.Profile) {
				return fmt.Errorf("legacy host configuration has invalid %q product override", product)
			}
		}
		for key, value := range configuration.ProfileSelections {
			if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 4096 || !migrationAbsoluteCleanPath(value) {
				return fmt.Errorf("legacy host configuration has invalid profile selection %q", key)
			}
		}
	}
	return nil
}

//nolint:gocyclo // Replayability, destination identity, and exact pending-state checks fail independently.
func validateLegacyDelivery(
	delivery DeliveryRecord,
	hostID string,
	sessions map[string]LegacySessionRecord,
) error {
	if !validRecordHeader(delivery.RecordHeader) || !durableRecordID.MatchString(delivery.MessageID) ||
		strings.TrimSpace(delivery.SourceHostID) == "" || strings.TrimSpace(delivery.SourceSessionID) == "" ||
		delivery.Operation != DeliveryOperationSend && delivery.Operation != DeliveryOperationMulticast ||
		delivery.State != DeliveryStateAccepted || delivery.AcceptedRevision == 0 || delivery.AcceptedAt <= 0 ||
		strings.TrimSpace(delivery.RequestDigest) == "" || strings.TrimSpace(delivery.AdoptedSourceRevision) == "" || delivery.Content == "" ||
		len(delivery.Content) > federation.MaxAgentFrameBytes || len(delivery.Summary) > 64<<10 || len(delivery.SentAt) > 512 ||
		len(delivery.SourceHostID) > 4096 || len(delivery.SourceSessionID) > 4096 || len(delivery.ResolvedDestinations) == 0 {
		return fmt.Errorf("legacy delivery %q is not replayable accepted work", delivery.MessageID)
	}
	seen := make(map[string]struct{}, len(delivery.ResolvedDestinations))
	for _, destination := range delivery.ResolvedDestinations {
		parts := strings.SplitN(destination, "/", 2)
		if len(parts) != 2 || parts[0] != hostID || !durableRecordID.MatchString(parts[1]) ||
			delivery.DestinationResults[destination] != DeliveryDestinationPending {
			return fmt.Errorf("legacy delivery %q has an invalid pending destination %q", delivery.MessageID, destination)
		}
		found := false
		for _, session := range sessions {
			if session.SessionID == parts[1] {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("legacy delivery %q targets an unknown session %q", delivery.MessageID, parts[1])
		}
		if _, duplicate := seen[destination]; duplicate {
			return fmt.Errorf("legacy delivery %q repeats destination %q", delivery.MessageID, destination)
		}
		seen[destination] = struct{}{}
	}
	if len(delivery.DestinationResults) != len(seen) {
		return fmt.Errorf("legacy delivery %q has outcomes outside its accepted destination set", delivery.MessageID)
	}
	return nil
}

func validLegacyDeliveryDisposition(value string) bool {
	switch value {
	case legacyDeliveryDispositionPending, legacyDeliveryDispositionTerminal, legacyDeliveryDispositionAmbiguous:
		return true
	default:
		return false
	}
}

func isFederationProduct(product string) bool {
	switch product {
	case "codex", "claude", "grok", "qwen":
		return true
	default:
		return false
	}
}

func isLegacyPreparationProduct(product string) bool {
	return product == "claude" || product == "qwen"
}

func cloneLegacyDeliveries(records []DeliveryRecord) []DeliveryRecord {
	result := make([]DeliveryRecord, len(records))
	for index := range records {
		result[index] = cloneDeliveryRecord(records[index])
	}
	return result
}

func cloneLegacyDeliveryCursors(records []LegacyDeliveryCursor) []LegacyDeliveryCursor {
	return append([]LegacyDeliveryCursor(nil), records...)
}

func cloneLegacyDeliveryNotices(records []LegacyDeliveryNotice) []LegacyDeliveryNotice {
	return append([]LegacyDeliveryNotice(nil), records...)
}

func cloneLegacyPreparations(records []LegacyPreparationRecord) []LegacyPreparationRecord {
	result := append([]LegacyPreparationRecord(nil), records...)
	for index := range result {
		result[index].CleanupDebtIDs = append([]string(nil), result[index].CleanupDebtIDs...)
	}
	return result
}

func cloneLegacyHostConfiguration(source *LegacyHostConfiguration) *LegacyHostConfiguration {
	if source == nil {
		return nil
	}
	result := *source
	result.LaneExecutables = maps.Clone(source.LaneExecutables)
	result.ProductOverrides = maps.Clone(source.ProductOverrides)
	result.ProfileSelections = maps.Clone(source.ProfileSelections)
	return &result
}

func sortLegacyDeliveryState(snapshot *LegacyAdoptionSnapshot) {
	sort.Slice(snapshot.Deliveries, func(i, j int) bool {
		return snapshot.Deliveries[i].MessageID < snapshot.Deliveries[j].MessageID
	})
	sort.Slice(snapshot.DeliveryCursors, func(i, j int) bool {
		return legacySessionKey(snapshot.DeliveryCursors[i].Product, snapshot.DeliveryCursors[i].SessionID) <
			legacySessionKey(snapshot.DeliveryCursors[j].Product, snapshot.DeliveryCursors[j].SessionID)
	})
	sort.Slice(snapshot.DeliveryNotices, func(i, j int) bool {
		return snapshot.DeliveryNotices[i].NoticeID < snapshot.DeliveryNotices[j].NoticeID
	})
	sort.Slice(snapshot.Preparations, func(i, j int) bool {
		return snapshot.Preparations[i].PreparationID < snapshot.Preparations[j].PreparationID
	})
}
