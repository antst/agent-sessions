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
	"strings"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

const legacyAdoptionRecordName = "adopted"

func legacyAdoptionRecordKey(migrationID string) string {
	return "migration/" + migrationID + "/" + legacyAdoptionRecordName
}

// ErrMigrationAdoptionConflict rejects a different plan after one adoption
// snapshot has become authoritative.
var ErrMigrationAdoptionConflict = errors.New("legacy migration adoption conflicts with committed state")

// LegacySessionRecord is the product-neutral legacy session preference copied
// into the unified catalog. It contains no native transcript or credential.
type LegacySessionRecord struct {
	SessionID           string   `json:"session_id"`
	Product             string   `json:"product"`
	Kind                string   `json:"kind"`
	ExplicitGroups      []string `json:"explicit_groups,omitempty"`
	InheritedGroups     []string `json:"inherited_groups,omitempty"`
	ParentSessionID     string   `json:"parent_session_id,omitempty"`
	ParentHostID        string   `json:"parent_host_id,omitempty"`
	InheritParentGroups bool     `json:"inherit_parent_groups"`
	PermissionMode      string   `json:"permission_mode"`
	DeliveryCursor      string   `json:"delivery_cursor,omitempty"`
	UpdatedAt           int64    `json:"updated_at"`
}

// LegacySessionName is one exact product/session name projection.
type LegacySessionName struct {
	SessionID  string `json:"session_id"`
	Product    string `json:"product"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	NameSource string `json:"name_source"`
	UpdatedAt  int64  `json:"updated_at"`
}

// LegacyAdoptionRequest is already-inventoried Agent Sessions-owned metadata.
// ExcludedPaths is a deny-list boundary, never a source list to inspect.
type LegacyAdoptionRequest struct {
	SourceRevision  string                    `json:"source_revision"`
	HostID          string                    `json:"host_id"`
	Sessions        []LegacySessionRecord     `json:"sessions,omitempty"`
	Names           []LegacySessionName       `json:"names,omitempty"`
	Deliveries      []DeliveryRecord          `json:"deliveries,omitempty"`
	DeliveryCursors []LegacyDeliveryCursor    `json:"delivery_cursors,omitempty"`
	DeliveryNotices []LegacyDeliveryNotice    `json:"delivery_notices,omitempty"`
	Preparations    []LegacyPreparationRecord `json:"preparations,omitempty"`
	Configuration   *LegacyHostConfiguration  `json:"configuration,omitempty"`
	Lanes           []LaneRecord              `json:"lanes,omitempty"`
	Turns           []LaneTurnRecord          `json:"turns,omitempty"`
	Notices         []LaneNotice              `json:"notices,omitempty"`
	Hub             FederationStateRecord     `json:"hub"`
	Debt            []DebtRecord              `json:"debt,omitempty"`
	ExcludedPaths   []string                  `json:"excluded_paths,omitempty"`
}

// LegacyAdoptionSnapshot is the single atomically selected staged state.
type LegacyAdoptionSnapshot struct {
	SchemaVersion   int                       `json:"schema_version"`
	SourceRevision  string                    `json:"source_revision"`
	HostID          string                    `json:"host_id"`
	Sessions        []LegacySessionRecord     `json:"sessions,omitempty"`
	Names           []LegacySessionName       `json:"names,omitempty"`
	Deliveries      []DeliveryRecord          `json:"deliveries,omitempty"`
	DeliveryCursors []LegacyDeliveryCursor    `json:"delivery_cursors,omitempty"`
	DeliveryNotices []LegacyDeliveryNotice    `json:"delivery_notices,omitempty"`
	Preparations    []LegacyPreparationRecord `json:"preparations,omitempty"`
	Configuration   *LegacyHostConfiguration  `json:"configuration,omitempty"`
	Lanes           []LaneRecord              `json:"lanes,omitempty"`
	Turns           []LaneTurnRecord          `json:"turns,omitempty"`
	Notices         []LaneNotice              `json:"notices,omitempty"`
	Hub             FederationStateRecord     `json:"hub"`
	Debt            []DebtRecord              `json:"debt,omitempty"`
}

// LegacyAdoptionPlan is immutable staged metadata plus its content identity.
type LegacyAdoptionPlan struct {
	SchemaVersion  int                    `json:"schema_version"`
	Revision       string                 `json:"revision"`
	SourceRevision string                 `json:"source_revision"`
	Snapshot       LegacyAdoptionSnapshot `json:"snapshot"`
	ExcludedPaths  []string               `json:"excluded_paths,omitempty"`
	TargetPaths    []string               `json:"target_paths"`
}

// LegacyAdoptionResult is stable across idempotent commit retries.
type LegacyAdoptionResult struct {
	PlanRevision     string         `json:"plan_revision"`
	StateRevision    string         `json:"state_revision"`
	AdoptedCounts    map[string]int `json:"adopted_counts"`
	AlreadyCommitted bool           `json:"already_committed"`
}

// SessionCatalog is the durable product-neutral session/name projection.
type SessionCatalog struct {
	Sessions []LegacySessionRecord `json:"sessions"`
	Names    []LegacySessionName   `json:"names"`
}

type committedLegacyAdoption struct {
	SchemaVersion int                `json:"schema_version"`
	MigrationID   string             `json:"migration_id"`
	Plan          LegacyAdoptionPlan `json:"plan"`
	RolledBack    bool               `json:"rolled_back,omitempty"`
}

// StageLegacyAdoption validates and clones a complete adoption snapshot. It
// performs no state-store or source-path access.
func StageLegacyAdoption(ctx context.Context, request LegacyAdoptionRequest) (LegacyAdoptionPlan, error) {
	if err := ctx.Err(); err != nil {
		return LegacyAdoptionPlan{}, err
	}
	snapshot, err := legacyAdoptionSnapshot(request)
	if err != nil {
		return LegacyAdoptionPlan{}, err
	}
	excluded, err := validateLegacyExcludedPaths(request.ExcludedPaths)
	if err != nil {
		return LegacyAdoptionPlan{}, err
	}
	if err := rejectExcludedSnapshotReferences(snapshot, excluded); err != nil {
		return LegacyAdoptionPlan{}, err
	}
	targets := legacyAdoptionTargetPaths(snapshot)
	for _, target := range targets {
		if pathWithinAny(target, excluded) {
			return LegacyAdoptionPlan{}, fmt.Errorf("legacy adoption target %q enters excluded vendor state", target)
		}
	}
	plan := LegacyAdoptionPlan{
		SchemaVersion: MigrationSchemaVersion, SourceRevision: request.SourceRevision,
		Snapshot: snapshot, ExcludedPaths: excluded, TargetPaths: targets,
	}
	plan.Revision, err = legacyAdoptionPlanRevision(plan)
	if err != nil {
		return LegacyAdoptionPlan{}, err
	}
	return plan, nil
}

// CommitLegacyAdoption durably stages one migration-scoped aggregate snapshot
// with one CAS. The shard is not globally visible until migration/current
// selects this ID and its journal crosses the successor-authority commit CAS.
func CommitLegacyAdoption(
	ctx context.Context,
	store *StateStore,
	migrationID string,
	plan LegacyAdoptionPlan,
) (LegacyAdoptionResult, error) {
	if store == nil || store.records == nil {
		return LegacyAdoptionResult{}, errors.New("legacy adoption requires a state store")
	}
	if !durableRecordID.MatchString(migrationID) {
		return LegacyAdoptionResult{}, errors.New("legacy adoption requires an exact migration id")
	}
	if err := validateLegacyAdoptionPlan(plan); err != nil {
		return LegacyAdoptionResult{}, err
	}
	result := legacyAdoptionResult(plan)
	current, revision, err := store.readCommittedLegacyAdoption(ctx, migrationID)
	if err == nil {
		if current.RolledBack || current.Plan.Revision != plan.Revision {
			return LegacyAdoptionResult{}, ErrMigrationAdoptionConflict
		}
		result.StateRevision = legacyAdoptionStateRevision(plan.Revision, revision)
		result.AlreadyCommitted = true
		return result, nil
	}
	if !os.IsNotExist(err) {
		return LegacyAdoptionResult{}, err
	}
	if err := ensureLegacyAdoptionTargetsAbsent(ctx, store, plan.Snapshot); err != nil {
		return LegacyAdoptionResult{}, err
	}
	committed := committedLegacyAdoption{
		SchemaVersion: MigrationSchemaVersion, MigrationID: migrationID, Plan: cloneLegacyAdoptionPlan(plan),
	}
	next, err := store.records.CompareAndSwap(ctx, legacyAdoptionRecordKey(migrationID), 0, committed)
	if err != nil {
		if errors.Is(err, statestore.ErrRevisionConflict) {
			current, currentRevision, readErr := store.readCommittedLegacyAdoption(ctx, migrationID)
			if readErr == nil && !current.RolledBack && current.Plan.Revision == plan.Revision {
				result.StateRevision = legacyAdoptionStateRevision(plan.Revision, currentRevision)
				result.AlreadyCommitted = true
				return result, nil
			}
			return LegacyAdoptionResult{}, ErrMigrationAdoptionConflict
		}
		return LegacyAdoptionResult{}, err
	}
	result.StateRevision = legacyAdoptionStateRevision(plan.Revision, next)
	return result, nil
}

// LoadAdoptedState reads only the aggregate snapshot selected by the current
// migration after the successor-authority journal CAS. Pre-ready shards and
// terminally rolled-back attempts remain invisible to all state projections.
func LoadAdoptedState(ctx context.Context, store *StateStore) (LegacyAdoptionSnapshot, error) {
	if store == nil || store.records == nil {
		return LegacyAdoptionSnapshot{}, errors.New("legacy adoption requires a state store")
	}
	current, _, err := store.ReadCurrentMigration(ctx)
	if err != nil {
		return LegacyAdoptionSnapshot{}, err
	}
	journal, _, err := store.ReadMigration(ctx, current.MigrationID)
	if err != nil {
		return LegacyAdoptionSnapshot{}, err
	}
	if journal.FreshInventoryRequired || !migrationStateHasCommittedAuthority(journal.State) {
		return LegacyAdoptionSnapshot{}, os.ErrNotExist
	}
	committed, _, err := store.readCommittedLegacyAdoption(ctx, current.MigrationID)
	if err != nil {
		return LegacyAdoptionSnapshot{}, err
	}
	return cloneLegacyAdoptionSnapshot(committed.Plan.Snapshot), nil
}

// ReadSessionCatalog reads an ordinary catalog revision when present and
// otherwise projects the atomically committed migration seed.
func (store *StateStore) ReadSessionCatalog(ctx context.Context) (SessionCatalog, statestore.Revision, error) {
	var catalog SessionCatalog
	revision, err := store.records.Read(ctx, "catalog/sessions", &catalog)
	if err == nil || !os.IsNotExist(err) {
		return catalog, revision, err
	}
	snapshot, adoptionErr := LoadAdoptedState(ctx, store)
	if adoptionErr != nil {
		return SessionCatalog{}, 0, adoptionErr
	}
	return SessionCatalog{
		Sessions: cloneLegacySessions(snapshot.Sessions), Names: cloneLegacyNames(snapshot.Names),
	}, 0, nil
}

// ReadFederation reads an ordinary connection-state revision when present and
// otherwise projects the atomically committed migration seed.
func (store *StateStore) ReadFederation(ctx context.Context) (FederationStateRecord, statestore.Revision, error) {
	var state FederationStateRecord
	revision, err := store.records.Read(ctx, "federation/state", &state)
	if err == nil || !os.IsNotExist(err) {
		return state, revision, err
	}
	snapshot, adoptionErr := LoadAdoptedState(ctx, store)
	if adoptionErr != nil {
		return FederationStateRecord{}, 0, adoptionErr
	}
	return cloneFederationState(snapshot.Hub), 0, nil
}

func (store *StateStore) readCommittedLegacyAdoption(
	ctx context.Context,
	migrationID string,
) (committedLegacyAdoption, statestore.Revision, error) {
	if !durableRecordID.MatchString(migrationID) {
		return committedLegacyAdoption{}, 0, errors.New("legacy adoption migration id is invalid")
	}
	var committed committedLegacyAdoption
	revision, err := store.records.Read(ctx, legacyAdoptionRecordKey(migrationID), &committed)
	if err != nil {
		return committedLegacyAdoption{}, 0, err
	}
	if committed.SchemaVersion != MigrationSchemaVersion || committed.MigrationID != migrationID {
		return committedLegacyAdoption{}, 0, errors.New("committed legacy adoption has invalid authority")
	}
	if err := validateLegacyAdoptionPlan(committed.Plan); err != nil {
		return committedLegacyAdoption{}, 0, err
	}
	if committed.RolledBack {
		return committedLegacyAdoption{}, 0, os.ErrNotExist
	}
	return committed, revision, nil
}

func legacyAdoptionSnapshot(request LegacyAdoptionRequest) (LegacyAdoptionSnapshot, error) {
	if strings.TrimSpace(request.SourceRevision) == "" || !durableRecordID.MatchString(request.HostID) {
		return LegacyAdoptionSnapshot{}, errors.New("legacy adoption has incomplete source or host identity")
	}
	snapshot := LegacyAdoptionSnapshot{
		SchemaVersion: MigrationSchemaVersion, SourceRevision: request.SourceRevision, HostID: request.HostID,
		Sessions: cloneLegacySessions(request.Sessions), Names: cloneLegacyNames(request.Names),
		Deliveries:      cloneLegacyDeliveries(request.Deliveries),
		DeliveryCursors: cloneLegacyDeliveryCursors(request.DeliveryCursors),
		DeliveryNotices: cloneLegacyDeliveryNotices(request.DeliveryNotices),
		Preparations:    cloneLegacyPreparations(request.Preparations),
		Configuration:   cloneLegacyHostConfiguration(request.Configuration),
		Lanes:           cloneMigrationLanes(request.Lanes), Turns: cloneMigrationTurns(request.Turns),
		Notices: append([]LaneNotice(nil), request.Notices...), Hub: cloneFederationState(request.Hub),
		Debt: append([]DebtRecord(nil), request.Debt...),
	}
	sortLegacyAdoptionSnapshot(&snapshot)
	if err := validateLegacyAdoptionSnapshot(snapshot); err != nil {
		return LegacyAdoptionSnapshot{}, err
	}
	return snapshot, nil
}

//nolint:gocyclo // Each durable record family has independent exact safety invariants.
func validateLegacyAdoptionSnapshot(snapshot LegacyAdoptionSnapshot) error {
	if snapshot.SchemaVersion != MigrationSchemaVersion || strings.TrimSpace(snapshot.SourceRevision) == "" ||
		!durableRecordID.MatchString(snapshot.HostID) {
		return errors.New("legacy adoption snapshot has invalid schema, source, or host")
	}
	sessions := make(map[string]LegacySessionRecord, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		key := legacySessionKey(session.Product, session.SessionID)
		if err := validateLegacySession(session); err != nil {
			return err
		}
		if _, exists := sessions[key]; exists {
			return fmt.Errorf("legacy adoption repeats session %q", key)
		}
		sessions[key] = session
	}
	for _, session := range snapshot.Sessions {
		if session.ParentSessionID == "" {
			continue
		}
		found := false
		for _, parent := range snapshot.Sessions {
			if parent.SessionID == session.ParentSessionID {
				found = true
				break
			}
		}
		if !found || session.ParentHostID == "" {
			return fmt.Errorf("legacy session %q has an unresolved parent", session.SessionID)
		}
	}
	names := make(map[string]struct{}, len(snapshot.Names))
	for _, name := range snapshot.Names {
		key := legacySessionKey(name.Product, name.SessionID)
		if !durableRecordID.MatchString(name.SessionID) || strings.TrimSpace(name.Name) == "" ||
			strings.TrimSpace(name.NameSource) == "" || name.UpdatedAt <= 0 {
			return fmt.Errorf("legacy session name %q is incomplete", key)
		}
		session, exists := sessions[key]
		if !exists || session.Kind != name.Kind {
			return fmt.Errorf("legacy session name %q lacks its exact session", key)
		}
		if _, duplicate := names[key]; duplicate {
			return fmt.Errorf("legacy adoption repeats session name %q", key)
		}
		names[key] = struct{}{}
	}
	if err := validateLegacyLanes(snapshot); err != nil {
		return err
	}
	if err := validateLegacyDeliveries(snapshot); err != nil {
		return err
	}
	if err := validateLegacyHub(snapshot.HostID, snapshot.Hub); err != nil {
		return err
	}
	seenDebt := make(map[string]struct{}, len(snapshot.Debt))
	for _, debt := range snapshot.Debt {
		if err := debt.Validate(); err != nil {
			return fmt.Errorf("legacy adoption debt %q: %w", debt.DebtID, err)
		}
		if _, duplicate := seenDebt[debt.DebtID]; duplicate {
			return fmt.Errorf("legacy adoption repeats debt %q", debt.DebtID)
		}
		seenDebt[debt.DebtID] = struct{}{}
	}
	return nil
}

func validateLegacySession(session LegacySessionRecord) error {
	if !durableRecordID.MatchString(session.SessionID) || session.UpdatedAt <= 0 ||
		strings.TrimSpace(session.PermissionMode) == "" {
		return fmt.Errorf("legacy session %q is incomplete", session.SessionID)
	}
	if _, ok := productcatalog.ProductByID(session.Product); !ok {
		return fmt.Errorf("legacy session %q has unsupported product %q", session.SessionID, session.Product)
	}
	if session.Kind != federation.SessionKindInteractive && session.Kind != federation.SessionKindLane {
		return fmt.Errorf("legacy session %q has unsupported kind %q", session.SessionID, session.Kind)
	}
	if err := validateLegacyGroups(session.ExplicitGroups); err != nil {
		return fmt.Errorf("legacy session %q explicit groups: %w", session.SessionID, err)
	}
	if err := validateLegacyGroups(session.InheritedGroups); err != nil {
		return fmt.Errorf("legacy session %q inherited groups: %w", session.SessionID, err)
	}
	if (session.ParentSessionID == "") != (session.ParentHostID == "") ||
		(session.InheritParentGroups && session.ParentSessionID == "") {
		return fmt.Errorf("legacy session %q has incomplete parent inheritance", session.SessionID)
	}
	return nil
}

//nolint:gocyclo // Lane, turn, and notice links fail independently for actionable migration diagnostics.
func validateLegacyLanes(snapshot LegacyAdoptionSnapshot) error {
	lanes := make(map[string]LaneRecord, len(snapshot.Lanes))
	for _, lane := range snapshot.Lanes {
		if err := validateLegacyLane(lane); err != nil {
			return err
		}
		if _, duplicate := lanes[lane.LaneSessionID]; duplicate {
			return fmt.Errorf("legacy adoption repeats lane %q", lane.LaneSessionID)
		}
		lanes[lane.LaneSessionID] = lane
	}
	turns := make(map[string]LaneTurnRecord, len(snapshot.Turns))
	for _, turn := range snapshot.Turns {
		if err := validateLegacyTurn(turn); err != nil {
			return err
		}
		if _, exists := lanes[turn.LaneSessionID]; !exists {
			return fmt.Errorf("legacy turn %q lacks its lane", turn.TurnID)
		}
		key := laneTurnKey(turn.LaneSessionID, turn.TurnID)
		if _, duplicate := turns[key]; duplicate {
			return fmt.Errorf("legacy adoption repeats turn %q", key)
		}
		turns[key] = turn
	}
	notices := make(map[string]struct{}, len(snapshot.Notices))
	for _, notice := range snapshot.Notices {
		lane, laneExists := lanes[notice.LaneSessionID]
		turn, turnExists := turns[laneTurnKey(notice.LaneSessionID, notice.TurnID)]
		if !laneExists || !turnExists || !validRecordHeader(notice.RecordHeader) ||
			!durableRecordID.MatchString(notice.NoticeID) || notice.ParentHostID == "" ||
			notice.ParentSessionID == "" || notice.ParentHostID != lane.ParentHostID ||
			notice.ParentSessionID != lane.ParentSessionID || notice.Outcome != turn.TerminalOutcome ||
			turn.TerminalNoticeID != notice.NoticeID {
			return fmt.Errorf("legacy terminal notice %q lacks exact lane, turn, parent, or outcome", notice.NoticeID)
		}
		if _, duplicate := notices[notice.NoticeID]; duplicate {
			return fmt.Errorf("legacy adoption repeats notice %q", notice.NoticeID)
		}
		notices[notice.NoticeID] = struct{}{}
	}
	for _, turn := range snapshot.Turns {
		if _, exists := notices[turn.TerminalNoticeID]; !exists {
			return fmt.Errorf("legacy turn %q lacks its terminal notice", turn.TurnID)
		}
	}
	return nil
}

func validateLegacyLane(lane LaneRecord) error {
	if !validRecordHeader(lane.RecordHeader) || !durableRecordID.MatchString(lane.LaneSessionID) ||
		strings.TrimSpace(lane.Name) == "" || strings.TrimSpace(lane.ParentHostID) == "" ||
		strings.TrimSpace(lane.ParentSessionID) == "" || strings.TrimSpace(lane.PermissionMode) == "" ||
		!migrationAbsoluteCleanPath(lane.Cwd) || lane.ActiveTurnID != "" ||
		(lane.State != LaneStateIdle && lane.State != LaneStateArchived) {
		return fmt.Errorf("legacy lane %q is live or incomplete", lane.LaneSessionID)
	}
	if _, ok := productcatalog.ProductByID(lane.Product); !ok {
		return fmt.Errorf("legacy lane %q has unsupported product %q", lane.LaneSessionID, lane.Product)
	}
	if err := validateLegacyGroups(lane.ParentGroups); err != nil {
		return fmt.Errorf("legacy lane %q parent groups: %w", lane.LaneSessionID, err)
	}
	if err := validateLegacyGroups(lane.Groups); err != nil {
		return fmt.Errorf("legacy lane %q groups: %w", lane.LaneSessionID, err)
	}
	return nil
}

func validateLegacyTurn(turn LaneTurnRecord) error {
	if !validRecordHeader(turn.RecordHeader) || !durableRecordID.MatchString(turn.TurnID) ||
		!durableRecordID.MatchString(turn.LaneSessionID) || turn.DispatchState != LaneDispatchCollected ||
		!validLaneTerminalOutcome(turn.TerminalOutcome) || turn.CollectionRevision == 0 || turn.CollectedAt <= 0 ||
		!durableRecordID.MatchString(turn.TerminalNoticeID) {
		return fmt.Errorf("legacy turn %q is live, uncollected, or incomplete", turn.TurnID)
	}
	return nil
}

func validateLegacyHub(hostID string, hub FederationStateRecord) error {
	if !validRecordHeader(hub.RecordHeader) || hub.HostID != hostID || strings.TrimSpace(hub.HostName) == "" ||
		hub.ProtocolVersion != federation.ProtocolVersion || hub.ConnectionGeneration == 0 {
		return errors.New("legacy hub state has incomplete stable identity or incompatible protocol")
	}
	switch hub.State {
	case "disabled", "connecting", "reconnecting", "connected", "backoff", "incompatible":
		return nil
	default:
		return fmt.Errorf("legacy hub state %q is unsupported", hub.State)
	}
}

func validRecordHeader(header RecordHeader) bool {
	return header.SchemaVersion == HostRuntimeSchemaVersion && header.Revision > 0 && header.Generation > 0 &&
		header.CreatedAt > 0 && header.UpdatedAt >= header.CreatedAt
}

func validateLegacyGroups(groups []string) error {
	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if strings.TrimSpace(group) == "" {
			return errors.New("group is empty")
		}
		if _, duplicate := seen[group]; duplicate {
			return fmt.Errorf("group %q is repeated", group)
		}
		seen[group] = struct{}{}
	}
	return nil
}

func validateLegacyExcludedPaths(paths []string) ([]string, error) {
	result := append([]string(nil), paths...)
	for _, path := range result {
		if !migrationAbsoluteCleanPath(path) || path == string(filepath.Separator) {
			return nil, fmt.Errorf("legacy vendor exclusion %q is not an exact absolute path", path)
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, fmt.Errorf("legacy vendor exclusion %q is repeated", result[index])
		}
	}
	return result, nil
}

func rejectExcludedSnapshotReferences(snapshot LegacyAdoptionSnapshot, excluded []string) error {
	if len(excluded) == 0 {
		return nil
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return err
	}
	return walkLegacyAdoptionStrings(value, func(value string) error {
		if pathWithinAny(value, excluded) {
			return fmt.Errorf("legacy adoption metadata references excluded vendor path %q", value)
		}
		return nil
	})
}

func walkLegacyAdoptionStrings(value any, visit func(string) error) error {
	switch value := value.(type) {
	case string:
		return visit(value)
	case []any:
		for _, item := range value {
			if err := walkLegacyAdoptionStrings(item, visit); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range value {
			if err := walkLegacyAdoptionStrings(item, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func pathWithinAny(path string, roots []string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	for _, root := range roots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func legacyAdoptionTargetPaths(snapshot LegacyAdoptionSnapshot) []string {
	paths := make([]string, 0, 8+len(snapshot.Debt))
	paths = append(paths,
		"catalog/sessions", "deliveries", "delivery/cursors", "delivery/notices",
		"preparations", "federation/configuration", "federation/state", "lanes",
	)
	for _, debt := range snapshot.Debt {
		paths = append(paths, "debt/"+debt.DebtID)
	}
	sort.Strings(paths)
	return paths
}

func legacyAdoptionPlanRevision(plan LegacyAdoptionPlan) (string, error) {
	identity := plan
	identity.Revision = ""
	body, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode legacy adoption plan identity: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func validateLegacyAdoptionPlan(plan LegacyAdoptionPlan) error {
	if plan.SchemaVersion != MigrationSchemaVersion || plan.SourceRevision != plan.Snapshot.SourceRevision ||
		strings.TrimSpace(plan.Revision) == "" {
		return errors.New("legacy adoption plan has incomplete schema or provenance")
	}
	if err := validateLegacyAdoptionSnapshot(plan.Snapshot); err != nil {
		return err
	}
	wantExcluded, err := validateLegacyExcludedPaths(plan.ExcludedPaths)
	if err != nil || !equalStrings(wantExcluded, plan.ExcludedPaths) {
		return errors.New("legacy adoption plan has invalid canonical exclusions")
	}
	wantTargets := legacyAdoptionTargetPaths(plan.Snapshot)
	if !equalStrings(wantTargets, plan.TargetPaths) {
		return errors.New("legacy adoption plan target inventory changed")
	}
	if err := rejectExcludedSnapshotReferences(plan.Snapshot, plan.ExcludedPaths); err != nil {
		return err
	}
	wantRevision, err := legacyAdoptionPlanRevision(plan)
	if err != nil || wantRevision != plan.Revision {
		return errors.New("legacy adoption plan revision does not match its staged snapshot")
	}
	return nil
}

func ensureLegacyAdoptionTargetsAbsent(ctx context.Context, store *StateStore, snapshot LegacyAdoptionSnapshot) error {
	keys := make([]string, 0, 8+len(snapshot.Debt))
	keys = append(keys,
		"catalog/sessions", "deliveries", "delivery/cursors", "delivery/notices",
		"preparations", "federation/configuration", "federation/state", "lanes",
	)
	for _, debt := range snapshot.Debt {
		keys = append(keys, "debt/"+debt.DebtID)
	}
	for _, key := range keys {
		var existing json.RawMessage
		if _, err := store.records.Read(ctx, key, &existing); err == nil {
			return fmt.Errorf("%w: target record %q already exists", ErrMigrationAdoptionConflict, key)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func legacyAdoptionResult(plan LegacyAdoptionPlan) LegacyAdoptionResult {
	configurationCount := 0
	if plan.Snapshot.Configuration != nil {
		configurationCount = 1
	}
	return LegacyAdoptionResult{
		PlanRevision: plan.Revision,
		AdoptedCounts: map[string]int{
			"sessions": len(plan.Snapshot.Sessions), "names": len(plan.Snapshot.Names),
			"deliveries": len(plan.Snapshot.Deliveries), "delivery_cursors": len(plan.Snapshot.DeliveryCursors),
			"delivery_notices": len(plan.Snapshot.DeliveryNotices), "preparations": len(plan.Snapshot.Preparations),
			"configuration": configurationCount,
			"lanes":         len(plan.Snapshot.Lanes), "turns": len(plan.Snapshot.Turns),
			"notices": len(plan.Snapshot.Notices), "debt": len(plan.Snapshot.Debt),
		},
	}
}

func legacyAdoptionStateRevision(planRevision string, revision statestore.Revision) string {
	return fmt.Sprintf("%s:%d", planRevision, revision)
}

func legacySessionKey(product, sessionID string) string { return product + "\x00" + sessionID }

func sortLegacyAdoptionSnapshot(snapshot *LegacyAdoptionSnapshot) {
	sort.Slice(snapshot.Sessions, func(i, j int) bool {
		return legacySessionKey(snapshot.Sessions[i].Product, snapshot.Sessions[i].SessionID) <
			legacySessionKey(snapshot.Sessions[j].Product, snapshot.Sessions[j].SessionID)
	})
	sort.Slice(snapshot.Names, func(i, j int) bool {
		return legacySessionKey(snapshot.Names[i].Product, snapshot.Names[i].SessionID) <
			legacySessionKey(snapshot.Names[j].Product, snapshot.Names[j].SessionID)
	})
	sort.Slice(snapshot.Lanes, func(i, j int) bool { return snapshot.Lanes[i].LaneSessionID < snapshot.Lanes[j].LaneSessionID })
	sort.Slice(snapshot.Turns, func(i, j int) bool {
		return laneTurnKey(snapshot.Turns[i].LaneSessionID, snapshot.Turns[i].TurnID) <
			laneTurnKey(snapshot.Turns[j].LaneSessionID, snapshot.Turns[j].TurnID)
	})
	sort.Slice(snapshot.Notices, func(i, j int) bool { return snapshot.Notices[i].NoticeID < snapshot.Notices[j].NoticeID })
	sortLegacyDeliveryState(snapshot)
	sort.Slice(snapshot.Debt, func(i, j int) bool { return snapshot.Debt[i].DebtID < snapshot.Debt[j].DebtID })
}

func cloneLegacyAdoptionSnapshot(snapshot LegacyAdoptionSnapshot) LegacyAdoptionSnapshot {
	snapshot.Sessions = cloneLegacySessions(snapshot.Sessions)
	snapshot.Names = cloneLegacyNames(snapshot.Names)
	snapshot.Deliveries = cloneLegacyDeliveries(snapshot.Deliveries)
	snapshot.DeliveryCursors = cloneLegacyDeliveryCursors(snapshot.DeliveryCursors)
	snapshot.DeliveryNotices = cloneLegacyDeliveryNotices(snapshot.DeliveryNotices)
	snapshot.Preparations = cloneLegacyPreparations(snapshot.Preparations)
	snapshot.Configuration = cloneLegacyHostConfiguration(snapshot.Configuration)
	snapshot.Lanes = cloneMigrationLanes(snapshot.Lanes)
	snapshot.Turns = cloneMigrationTurns(snapshot.Turns)
	snapshot.Notices = append([]LaneNotice(nil), snapshot.Notices...)
	snapshot.Hub = cloneFederationState(snapshot.Hub)
	snapshot.Debt = append([]DebtRecord(nil), snapshot.Debt...)
	return snapshot
}

func cloneLegacyAdoptionPlan(plan LegacyAdoptionPlan) LegacyAdoptionPlan {
	plan.Snapshot = cloneLegacyAdoptionSnapshot(plan.Snapshot)
	plan.ExcludedPaths = append([]string(nil), plan.ExcludedPaths...)
	plan.TargetPaths = append([]string(nil), plan.TargetPaths...)
	return plan
}

func cloneLegacySessions(records []LegacySessionRecord) []LegacySessionRecord {
	result := append([]LegacySessionRecord(nil), records...)
	for index := range result {
		result[index].ExplicitGroups = append([]string(nil), result[index].ExplicitGroups...)
		result[index].InheritedGroups = append([]string(nil), result[index].InheritedGroups...)
	}
	return result
}

func cloneLegacyNames(records []LegacySessionName) []LegacySessionName {
	return append([]LegacySessionName(nil), records...)
}

func cloneMigrationLanes(records []LaneRecord) []LaneRecord {
	result := make([]LaneRecord, len(records))
	for index := range records {
		result[index] = cloneLaneRecord(records[index])
	}
	return result
}

func cloneMigrationTurns(records []LaneTurnRecord) []LaneTurnRecord {
	result := make([]LaneTurnRecord, len(records))
	for index := range records {
		result[index] = cloneLaneTurnRecord(records[index])
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
