package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/sessionkey"
)

type productionLegacyLaneFamily struct {
	directory string
	product   string
	idField   string
}

type productionLegacyAdoptionAccumulator struct {
	revisions      []string
	sessionIndexes map[string]int
	nameIndexes    map[string]int
	seenLanes      map[string]struct{}
	seenTurns      map[string]struct{}
	seenNotices    map[string]struct{}
	seenDebt       map[string]struct{}
}

// productionLegacyBridgeAdoption projects only Agent Sessions-owned metadata
// from the exact bridge record families already used by inventory. Prompt,
// result, notice message, error, transcript, credential, and vendor profile
// bodies never enter the projection or its source revision.
func productionLegacyBridgeAdoption(
	ctx context.Context,
	request LegacyAdoptionRequest,
	sources []LegacyInventorySource,
	observedAt int64,
) (LegacyAdoptionRequest, error) {
	bridgeRoot := productionLegacySourcePath(sources, "bridge-state")
	accumulator := newProductionLegacyAdoptionAccumulator(request)
	if bridgeRoot != "" {
		if err := productionLegacyAdoptSessionDirectory(
			ctx, filepath.Join(bridgeRoot, "sessions"), observedAt, &request, accumulator,
		); err != nil {
			return LegacyAdoptionRequest{}, err
		}
		if err := productionLegacyAdoptProfileSessionRecords(
			ctx, bridgeRoot, &request, accumulator,
		); err != nil {
			return LegacyAdoptionRequest{}, err
		}
	}
	var profiles []os.DirEntry
	if bridgeRoot != "" {
		var err error
		profiles, err = readProductionLegacyDirectory(filepath.Join(bridgeRoot, "profiles"))
		if err != nil && !os.IsNotExist(err) {
			return LegacyAdoptionRequest{}, err
		}
	}
	families := []productionLegacyLaneFamily{
		{directory: "lanes", product: "codex", idField: "sessionId"},
		{directory: "claude-lanes", product: "claude", idField: "sessionId"},
		{directory: "grok-lanes", product: "grok", idField: "sessionId"},
		{directory: "qwen-lanes", product: "qwen", idField: "threadId"},
	}
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return LegacyAdoptionRequest{}, err
		}
		if !profile.IsDir() {
			continue
		}
		profileRoot := filepath.Join(bridgeRoot, "profiles", profile.Name())
		if err := productionLegacyAdoptProfileSessionRecords(
			ctx, profileRoot, &request, accumulator,
		); err != nil {
			return LegacyAdoptionRequest{}, err
		}
		for _, family := range families {
			directory := filepath.Join(profileRoot, family.directory)
			if err := productionLegacyAdoptLaneDirectory(
				ctx, directory, family, observedAt, &request, accumulator,
			); err != nil {
				return LegacyAdoptionRequest{}, err
			}
		}
	}
	if err := productionLegacyAdoptDeliveriesAndPreparations(
		ctx, bridgeRoot, sources, observedAt, &request, accumulator,
	); err != nil {
		return LegacyAdoptionRequest{}, err
	}
	return accumulator.finish(request), nil
}

func newProductionLegacyAdoptionAccumulator(
	request LegacyAdoptionRequest,
) *productionLegacyAdoptionAccumulator {
	result := &productionLegacyAdoptionAccumulator{
		revisions: []string{request.SourceRevision}, sessionIndexes: make(map[string]int), nameIndexes: make(map[string]int),
		seenLanes: make(map[string]struct{}),
		seenTurns: make(map[string]struct{}), seenNotices: make(map[string]struct{}), seenDebt: make(map[string]struct{}),
	}
	for index, session := range request.Sessions {
		result.sessionIndexes[legacySessionKey(session.Product, session.SessionID)] = index
	}
	for index, name := range request.Names {
		result.nameIndexes[legacySessionKey(name.Product, name.SessionID)] = index
	}
	for _, lane := range request.Lanes {
		result.seenLanes[lane.LaneSessionID] = struct{}{}
	}
	for _, turn := range request.Turns {
		result.seenTurns[laneTurnKey(turn.LaneSessionID, turn.TurnID)] = struct{}{}
	}
	for _, notice := range request.Notices {
		result.seenNotices[notice.NoticeID] = struct{}{}
	}
	for _, debt := range request.Debt {
		result.seenDebt[debt.DebtID] = struct{}{}
	}
	return result
}

func (accumulator *productionLegacyAdoptionAccumulator) finish(
	request LegacyAdoptionRequest,
) LegacyAdoptionRequest {
	if len(accumulator.revisions) == 1 {
		return request
	}
	sort.Strings(accumulator.revisions[1:])
	digest := sha256.Sum256([]byte(strings.Join(accumulator.revisions, "\x00")))
	request.SourceRevision = "sha256:" + hex.EncodeToString(digest[:])
	return request
}

func (accumulator *productionLegacyAdoptionAccumulator) mergeSession(
	request *LegacyAdoptionRequest,
	session LegacySessionRecord,
	name *LegacySessionName,
) error {
	key := legacySessionKey(session.Product, session.SessionID)
	if index, exists := accumulator.sessionIndexes[key]; exists {
		current := request.Sessions[index]
		if current.Kind != session.Kind {
			return fmt.Errorf("legacy bridge session %q changes kind", key)
		}
		if session.UpdatedAt >= current.UpdatedAt {
			if session.PermissionMode != "" {
				current.PermissionMode = session.PermissionMode
			}
			current.UpdatedAt = session.UpdatedAt
		}
		if session.ExplicitGroups != nil {
			current.ExplicitGroups = append([]string(nil), session.ExplicitGroups...)
		}
		if session.InheritedGroups != nil {
			current.InheritedGroups = append([]string(nil), session.InheritedGroups...)
		}
		if session.ParentSessionID != "" || session.ParentHostID != "" {
			current.ParentSessionID, current.ParentHostID = session.ParentSessionID, session.ParentHostID
			current.InheritParentGroups = session.InheritParentGroups
		}
		request.Sessions[index] = current
	} else {
		accumulator.sessionIndexes[key] = len(request.Sessions)
		request.Sessions = append(request.Sessions, session)
	}
	if name == nil {
		return nil
	}
	if index, exists := accumulator.nameIndexes[key]; exists {
		if name.UpdatedAt >= request.Names[index].UpdatedAt {
			request.Names[index] = *name
		}
		return nil
	}
	accumulator.nameIndexes[key] = len(request.Names)
	request.Names = append(request.Names, *name)
	return nil
}

func productionLegacyAdoptSessionDirectory(
	ctx context.Context,
	directory string,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
) error {
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
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name(), "state.json")
		record, err := readProductionLegacyProjection(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			continue // Inventory retains exact malformed-record debt.
		}
		session, name, ok := productionLegacySessionProjection(record, observedAt)
		if !ok {
			continue
		}
		revision, err := productionLegacyMetadataRevision(record)
		if err != nil {
			return err
		}
		if err := accumulator.mergeSession(request, session, name); err != nil {
			return err
		}
		accumulator.revisions = append(accumulator.revisions, revision)
	}
	return nil
}

func productionLegacySessionProjection(
	record map[string]json.RawMessage,
	observedAt int64,
) (LegacySessionRecord, *LegacySessionName, bool) {
	sessionID := productionLegacyRawString(record, "sessionId")
	product := productionLegacySessionProduct(productionLegacyRawString(record, "entrypoint"))
	if !durableRecordID.MatchString(sessionID) || product == "" {
		return LegacySessionRecord{}, nil, false
	}
	updatedAt := productionLegacyRawInt64(record, "updatedAt")
	if updatedAt <= 0 {
		updatedAt = observedAt
	}
	permission := productionLegacyRawString(record, "permissionMode")
	if permission == "" {
		permission = "default"
	}
	session := LegacySessionRecord{
		SessionID: sessionID, Product: product, Kind: "interactive", PermissionMode: permission,
		UpdatedAt: updatedAt,
	}
	if _, ok := record["explicitGroups"]; ok {
		session.ExplicitGroups = sortedUniqueStrings(productionLegacyRawStrings(record, "explicitGroups"))
	}
	if _, ok := record["inheritedGroups"]; ok {
		session.InheritedGroups = sortedUniqueStrings(productionLegacyRawStrings(record, "inheritedGroups"))
	} else if _, ok := record["groups"]; ok {
		session.InheritedGroups = productionLegacyInheritedGroups(
			sortedUniqueStrings(productionLegacyRawStrings(record, "groups")), session.ExplicitGroups,
		)
	}
	session.ParentSessionID = productionLegacyRawString(record, "parentSessionId")
	session.ParentHostID = productionLegacyRawString(record, "parentHostId")
	session.InheritParentGroups = productionLegacyRawBool(record, "inheritParentGroups")
	nameValue := strings.TrimSpace(productionLegacyRawString(record, "name"))
	if nameValue == "" {
		return session, nil, true
	}
	nameSource := productionLegacyRawString(record, "nameSource")
	if nameSource == "" {
		nameSource = "legacy_bridge"
	}
	name := &LegacySessionName{
		SessionID: sessionID, Product: product, Kind: "interactive", Name: nameValue,
		NameSource: nameSource, UpdatedAt: updatedAt,
	}
	return session, name, true
}

func productionLegacySessionProduct(entrypoint string) string {
	switch entrypoint {
	case "codex":
		return "codex"
	case "claude":
		return "claude"
	case "grok":
		return "grok"
	case "qwen":
		return "qwen"
	default:
		return ""
	}
}

func productionLegacyAdoptProfileSessionRecords(
	ctx context.Context,
	profileRoot string,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
) error {
	for _, family := range []struct {
		directory string
		timeField string
		name      bool
	}{
		{directory: "interactive-owners", timeField: "updatedAt", name: true},
		{directory: "retired", timeField: "retiredAt"},
	} {
		if err := productionLegacyAdoptProfileSessionRecordDirectory(
			ctx, filepath.Join(profileRoot, family.directory), family.timeField, family.name,
			request, accumulator,
		); err != nil {
			return err
		}
	}
	return nil
}

func productionLegacyAdoptProfileSessionRecordDirectory(
	ctx context.Context,
	directory string,
	timeField string,
	includeName bool,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
) error {
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
		path := filepath.Join(directory, entry.Name())
		record, err := readProductionLegacyProjection(path)
		if err != nil {
			continue // Inventory retains exact malformed-record debt.
		}
		sessionID := productionLegacyRawString(record, "threadId")
		updatedAt := productionLegacyRawInt64(record, timeField)
		if !durableRecordID.MatchString(sessionID) || updatedAt <= 0 ||
			entry.Name() != sessionkey.FromID(sessionID)+".json" {
			continue
		}
		session := LegacySessionRecord{
			SessionID: sessionID, Product: "codex", Kind: "interactive",
			PermissionMode: "default", UpdatedAt: updatedAt,
		}
		if _, exists := accumulator.sessionIndexes[legacySessionKey("codex", sessionID)]; exists {
			session.PermissionMode = ""
		}
		name := productionLegacyProfileSessionName(record, sessionID, updatedAt, includeName)
		revision, err := productionLegacyMetadataRevision(record)
		if err != nil {
			return err
		}
		if err := accumulator.mergeSession(request, session, name); err != nil {
			return err
		}
		accumulator.revisions = append(accumulator.revisions, revision)
	}
	return nil
}

func productionLegacyProfileSessionName(
	record map[string]json.RawMessage,
	sessionID string,
	updatedAt int64,
	include bool,
) *LegacySessionName {
	if !include {
		return nil
	}
	value := strings.TrimSpace(productionLegacyRawString(record, "name"))
	if value == "" {
		return nil
	}
	source := productionLegacyRawString(record, "nameSource")
	if source == "" {
		source = "legacy_owner"
	}
	return &LegacySessionName{
		SessionID: sessionID, Product: "codex", Kind: "interactive",
		Name: value, NameSource: source, UpdatedAt: updatedAt,
	}
}

func productionLegacyAdoptLaneDirectory(
	ctx context.Context,
	directory string,
	family productionLegacyLaneFamily,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
) error {
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
		path := filepath.Join(directory, entry.Name())
		record, err := readProductionLegacyProjection(path)
		if err != nil {
			continue // Inventory already reports this exact malformed record as debt.
		}
		revision, err := productionLegacyMetadataRevision(record)
		if err != nil {
			return err
		}
		lane, turns, notices, debt, ok := productionLegacyLaneProjection(record, family, revision, observedAt)
		if family.product == "codex" && !productionLegacyCodexLaneStatusKnown(productionLegacyRawString(record, "status")) {
			ok = false
		}
		if ok {
			if err := accumulator.append(request, revision, lane, turns, notices, debt); err != nil {
				return err
			}
		} else if family.product == "codex" {
			debt := productionLegacyIncompleteLaneDebt(record, family, path, revision, observedAt)
			if _, duplicate := accumulator.seenDebt[debt.DebtID]; !duplicate {
				accumulator.seenDebt[debt.DebtID] = struct{}{}
				request.Debt = append(request.Debt, debt)
			}
			accumulator.revisions = append(accumulator.revisions, revision)
		}
	}
	return nil
}

func productionLegacyIncompleteLaneDebt(
	record map[string]json.RawMessage,
	family productionLegacyLaneFamily,
	path string,
	revision string,
	observedAt int64,
) DebtRecord {
	identity := productionLegacyRawString(record, family.idField)
	if !durableRecordID.MatchString(identity) {
		identity = productionLegacyRawString(record, "threadId")
	}
	if !durableRecordID.MatchString(identity) {
		identity = quiescenceRecordID("legacy-lane-record", path)
	}
	updatedAt := productionLegacyRawInt64(record, "updatedAt")
	if updatedAt <= 0 {
		updatedAt = observedAt
	}
	return DebtRecord{
		RecordHeader: productionLegacyMigrationHeader(updatedAt),
		DebtID:       quiescenceRecordID("legacy-lane-projection-debt", family.product, path),
		Operation:    "migration_reconcile", ResourceKind: "legacy_lane_metadata",
		ResourceIdentity: family.product + "/" + identity, ExpectedRevision: revision,
		CauseCode:       "incomplete_legacy_lane_projection",
		RetryPredicate:  "repair or explicitly retire the exact preserved Agent Sessions-owned lane record",
		ProhibitedScope: "do not infer lane identity or content from vendor transcripts, profiles, content logs, or process names",
	}
}

func (accumulator *productionLegacyAdoptionAccumulator) append(
	request *LegacyAdoptionRequest,
	revision string,
	lane LaneRecord,
	turns []LaneTurnRecord,
	notices []LaneNotice,
	debt []DebtRecord,
) error {
	if _, duplicate := accumulator.seenLanes[lane.LaneSessionID]; duplicate {
		return fmt.Errorf("legacy bridge adoption repeats lane %q", lane.LaneSessionID)
	}
	accumulator.seenLanes[lane.LaneSessionID] = struct{}{}
	request.Lanes = append(request.Lanes, lane)
	for _, turn := range turns {
		key := laneTurnKey(turn.LaneSessionID, turn.TurnID)
		if _, duplicate := accumulator.seenTurns[key]; duplicate {
			return fmt.Errorf("legacy bridge adoption repeats turn %q", key)
		}
		accumulator.seenTurns[key] = struct{}{}
		request.Turns = append(request.Turns, turn)
	}
	for _, notice := range notices {
		if _, duplicate := accumulator.seenNotices[notice.NoticeID]; duplicate {
			return fmt.Errorf("legacy bridge adoption repeats notice %q", notice.NoticeID)
		}
		accumulator.seenNotices[notice.NoticeID] = struct{}{}
		request.Notices = append(request.Notices, notice)
	}
	for _, item := range debt {
		if _, duplicate := accumulator.seenDebt[item.DebtID]; duplicate {
			continue
		}
		accumulator.seenDebt[item.DebtID] = struct{}{}
		request.Debt = append(request.Debt, item)
	}
	accumulator.revisions = append(accumulator.revisions, revision)
	return nil
}

func productionLegacyLaneProjection(
	record map[string]json.RawMessage,
	family productionLegacyLaneFamily,
	sourceRevision string,
	observedAt int64,
) (LaneRecord, []LaneTurnRecord, []LaneNotice, []DebtRecord, bool) {
	laneID := productionLegacyRawString(record, family.idField)
	name := productionLegacyRawString(record, "name")
	cwd := productionLegacyRawString(record, "cwd")
	parentHostID := productionLegacyRawString(record, "parentHostId")
	parentSessionID := productionLegacyRawString(record, "parentSessionId")
	if !durableRecordID.MatchString(laneID) || strings.TrimSpace(name) == "" ||
		!migrationAbsoluteCleanPath(cwd) || strings.TrimSpace(parentHostID) == "" ||
		strings.TrimSpace(parentSessionID) == "" {
		return LaneRecord{}, nil, nil, nil, false
	}
	createdAt := productionLegacyRawInt64(record, "createdAt")
	updatedAt := productionLegacyRawInt64(record, "updatedAt")
	if createdAt <= 0 {
		createdAt = observedAt
	}
	if updatedAt < createdAt {
		updatedAt = createdAt
	}
	header := RecordHeader{
		SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: 1,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	status := productionLegacyRawString(record, "status")
	state := LaneStateIdle
	archiveRevision := uint64(0)
	if status == "archived" || productionLegacyRawString(record, "nativeArchiveState") == "archived" {
		state, archiveRevision = LaneStateArchived, 1
	}
	groups := sortedUniqueStrings(productionLegacyRawStrings(record, "groups"))
	explicit := sortedUniqueStrings(productionLegacyRawStrings(record, "explicitGroups"))
	parentGroups := productionLegacyInheritedGroups(groups, explicit)
	permission := productionLegacyRawString(record, "permissionMode")
	if permission == "" {
		permission = "default"
	}
	lane := LaneRecord{
		RecordHeader: header, LaneSessionID: laneID, Name: name, Product: family.product,
		ParentHostID: parentHostID, ParentSessionID: parentSessionID, ParentGroups: parentGroups,
		InheritParentGroups: productionLegacyRawBool(record, "inheritParentGroups"), Groups: groups,
		PermissionMode: permission, Cwd: cwd, State: state, ArchiveRevision: archiveRevision,
		NativeActor: productionLegacyLaneNativeActor(record, family.product, laneID),
	}
	turns, notices, incomplete := productionLegacyCollectedTurns(record, lane, sourceRevision, header)
	if collected := productionLegacyRawString(record, "collectedTurnId"); collected != "" {
		lane.CollectionCursor = collected + ":1"
	}
	debt := productionLegacyLaneDebt(record, lane, sourceRevision, header, incomplete)
	for _, item := range debt {
		lane.CleanupDebtIDs = append(lane.CleanupDebtIDs, item.DebtID)
	}
	return lane, turns, notices, debt, true
}

func productionLegacyCollectedTurns(
	record map[string]json.RawMessage,
	lane LaneRecord,
	sourceRevision string,
	header RecordHeader,
) ([]LaneTurnRecord, []LaneNotice, bool) {
	var rawTurns []map[string]json.RawMessage
	if body := record["turns"]; len(body) != 0 {
		_ = json.Unmarshal(body, &rawTurns)
	}
	noticeIDs := productionLegacyNoticeIDs(record)
	var turns []LaneTurnRecord
	var notices []LaneNotice
	incomplete := false
	for _, raw := range rawTurns {
		turnID := productionLegacyRawString(raw, "id")
		status := productionLegacyRawString(raw, "status")
		outcome, terminal := productionLegacyTerminalOutcome(status, productionLegacyRawString(raw, "outcome"))
		if !durableRecordID.MatchString(turnID) || !terminal || !productionLegacyRawBool(raw, "collected") {
			incomplete = true
			continue
		}
		collectedAt := productionLegacyRawInt64(raw, "collectedAt")
		if collectedAt <= 0 {
			collectedAt = productionLegacyRawInt64(raw, "completedAt")
		}
		if collectedAt <= 0 {
			collectedAt = header.UpdatedAt
		}
		turnHeader := header
		if createdAt := productionLegacyRawInt64(raw, "createdAt"); createdAt > 0 {
			turnHeader.CreatedAt = createdAt
		}
		turnHeader.UpdatedAt = collectedAt
		if turnHeader.UpdatedAt < turnHeader.CreatedAt {
			turnHeader.UpdatedAt = turnHeader.CreatedAt
		}
		noticeID := noticeIDs[turnID]
		if !durableRecordID.MatchString(noticeID) {
			noticeID = quiescenceRecordID("legacy-terminal-notice", lane.LaneSessionID, turnID)
		}
		requestDigest := productionLegacyRawString(raw, "requestDigest")
		if requestDigest == "" {
			requestDigest = "sha256:" + productionDigest([]byte(sourceRevision+"\x00"+lane.LaneSessionID+"\x00"+turnID))
		}
		turns = append(turns, LaneTurnRecord{
			RecordHeader: turnHeader, TurnID: turnID, LaneSessionID: lane.LaneSessionID,
			ParentContextRevision: 1, RequestDigest: requestDigest, DispatchState: LaneDispatchCollected,
			NativeTurnIdentity: productionLegacyTurnNativeIdentity(raw), TerminalOutcome: outcome,
			TerminalNoticeID: noticeID, CollectionRevision: 1, CollectedAt: collectedAt,
		})
		notices = append(notices, LaneNotice{
			RecordHeader: turnHeader, NoticeID: noticeID, LaneSessionID: lane.LaneSessionID, TurnID: turnID,
			ParentHostID: lane.ParentHostID, ParentSessionID: lane.ParentSessionID, Outcome: outcome,
		})
	}
	return turns, notices, incomplete
}

func productionLegacyNoticeIDs(record map[string]json.RawMessage) map[string]string {
	result := make(map[string]string)
	var notices []map[string]json.RawMessage
	if body := record["notices"]; len(body) != 0 {
		_ = json.Unmarshal(body, &notices)
	}
	for _, notice := range notices {
		turnID := productionLegacyRawString(notice, "turnId")
		noticeID := productionLegacyRawString(notice, "id")
		if durableRecordID.MatchString(turnID) && durableRecordID.MatchString(noticeID) {
			result[turnID] = noticeID
		}
	}
	return result
}

func productionLegacyLaneDebt(
	record map[string]json.RawMessage,
	lane LaneRecord,
	sourceRevision string,
	header RecordHeader,
	incompleteTurn bool,
) []DebtRecord {
	var reasons []string
	turnDebt, otherIncomplete := productionLegacyPendingTurnDebt(record, lane, sourceRevision, header)
	if incompleteTurn && otherIncomplete {
		reasons = append(reasons, "uncollected_or_nonterminal_turn")
	}
	if productionLegacyRawString(record, "cleanupError") != "" {
		reasons = append(reasons, "legacy_cleanup_error")
	}
	if body := record["cleanupDebt"]; len(body) != 0 && string(body) != "null" && string(body) != "[]" {
		reasons = append(reasons, "legacy_cleanup_debt")
	}
	result := append([]DebtRecord(nil), turnDebt...)
	for _, reason := range sortedUniqueStrings(reasons) {
		result = append(result, DebtRecord{
			RecordHeader: header,
			DebtID:       quiescenceRecordID("legacy-lane-debt", lane.LaneSessionID, reason), Operation: "migration_reconcile",
			ResourceKind: "legacy_lane", ResourceIdentity: lane.Product + "/" + lane.LaneSessionID,
			ExpectedRevision: sourceRevision, CauseCode: reason,
			RetryPredicate:  "reobserve exact Agent Sessions-owned lane metadata",
			ProhibitedScope: "do not read or mutate vendor transcripts, credentials, profiles, or native history",
		})
	}
	return result
}

// productionLegacyPendingTurnDebt preserves the exact accepted terminal turn
// identity, bounded native/result references, and collection cursor when a
// legacy collector did not durably acknowledge it. Migration must surface
// this scoped work instead of collapsing it into a lossy lane-level reason or
// consulting a vendor transcript/content log to guess whether it was handled.
func productionLegacyPendingTurnDebt(
	record map[string]json.RawMessage,
	lane LaneRecord,
	sourceRevision string,
	header RecordHeader,
) ([]DebtRecord, bool) {
	var rawTurns []map[string]json.RawMessage
	if body := record["turns"]; len(body) != 0 {
		_ = json.Unmarshal(body, &rawTurns)
	}
	collectionCursor := productionLegacyRawString(record, "collectedTurnId")
	var result []DebtRecord
	otherIncomplete := false
	for _, raw := range rawTurns {
		turnID := productionLegacyRawString(raw, "id")
		_, terminal := productionLegacyTerminalOutcome(
			productionLegacyRawString(raw, "status"), productionLegacyRawString(raw, "outcome"),
		)
		if terminal && productionLegacyRawBool(raw, "collected") {
			continue
		}
		if !terminal || !durableRecordID.MatchString(turnID) {
			otherIncomplete = true
			continue
		}
		updatedAt := productionLegacyRawInt64(raw, "completedAt")
		if updatedAt <= 0 {
			updatedAt = header.UpdatedAt
		}
		identity := productionLegacyPendingTurnIdentity(raw)
		resource := lane.Product + "/" + lane.LaneSessionID + "/" + turnID
		if identity != "" {
			resource += "/" + identity
		}
		detail := "turn_id=" + turnID
		if durableRecordID.MatchString(collectionCursor) {
			detail += "; collection_cursor=" + collectionCursor
		} else if collectionCursor != "" {
			detail += "; collection_cursor_state=invalid"
		}
		if reference := productionLegacyPendingTurnResultReference(raw); reference != "" {
			detail += "; result_reference=" + reference
		}
		result = append(result, DebtRecord{
			RecordHeader: productionLegacyMigrationHeader(updatedAt),
			DebtID:       quiescenceRecordID("legacy-terminal-turn-debt", lane.LaneSessionID, turnID),
			Operation:    "migration_collect_terminal_turn", ResourceKind: "legacy_terminal_turn",
			ResourceIdentity: resource, ExpectedRevision: sourceRevision,
			CauseCode: "terminal_turn_collection_pending", CauseDetail: detail,
			RetryPredicate:  "reconcile the exact Agent Sessions turn and collection cursor without redispatch",
			ProhibitedScope: "do not read vendor transcripts, prompt/result content, native history, or content logs",
		})
	}
	return result, otherIncomplete
}

func productionLegacyPendingTurnIdentity(record map[string]json.RawMessage) string {
	var identities []string
	seen := make(map[string]struct{})
	for _, key := range []string{"messageId", "qwenSessionId", "nativeTurnId", "turnId"} {
		if value := productionLegacyRawString(record, key); durableRecordID.MatchString(value) {
			if _, duplicate := seen[value]; !duplicate {
				seen[value] = struct{}{}
				identities = append(identities, value)
			}
		}
	}
	return strings.Join(identities, "/")
}

func productionLegacyPendingTurnResultReference(record map[string]json.RawMessage) string {
	for _, key := range []string{"nativeResultId", "resultId", "terminalRevision"} {
		if value := productionLegacyRawString(record, key); durableRecordID.MatchString(value) {
			return value
		}
	}
	return ""
}

func productionLegacyTerminalOutcome(status, outcome string) (string, bool) {
	for _, value := range []string{outcome, status} {
		switch value {
		case "completed", "success":
			return LaneDispatchCompleted, true
		case "interrupted", "cancelled", "canceled", "timed_out":
			return LaneDispatchInterrupted, true
		case "failed", "error":
			return LaneDispatchFailed, true
		}
	}
	return "", false
}

func productionLegacyLaneNativeActor(
	record map[string]json.RawMessage,
	product string,
	laneID string,
) map[string]any {
	result := map[string]any{"product": product, "legacy_lane_id": laneID}
	for _, key := range []string{"threadId", "sessionId", "qwenSessionId", "grokSessionId"} {
		if value := productionLegacyRawString(record, key); value != "" {
			result[key] = value
		}
	}
	return result
}

func productionLegacyTurnNativeIdentity(record map[string]json.RawMessage) map[string]any {
	result := make(map[string]any)
	for _, key := range []string{"id", "messageId", "qwenSessionId", "terminalRevision"} {
		if value := productionLegacyRawString(record, key); value != "" {
			result[key] = value
		}
	}
	return result
}

func productionLegacyInheritedGroups(groups, explicit []string) []string {
	explicitSet := make(map[string]struct{}, len(explicit))
	for _, group := range explicit {
		explicitSet[group] = struct{}{}
	}
	var inherited []string
	for _, group := range groups {
		if _, ok := explicitSet[group]; !ok {
			inherited = append(inherited, group)
		}
	}
	return inherited
}

func productionLegacyRawString(record map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(record[key], &value)
	return value
}

func productionLegacyRawStrings(record map[string]json.RawMessage, key string) []string {
	var value []string
	_ = json.Unmarshal(record[key], &value)
	return value
}

func productionLegacyRawBool(record map[string]json.RawMessage, key string) bool {
	var value bool
	_ = json.Unmarshal(record[key], &value)
	return value
}

func productionLegacyRawInt64(record map[string]json.RawMessage, key string) int64 {
	var value int64
	_ = json.Unmarshal(record[key], &value)
	return value
}
