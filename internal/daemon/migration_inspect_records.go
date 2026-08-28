package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

const maxProductionLegacyDirectoryEntries = 4096

type productionLegacyRecordDescriptor struct {
	sourcePath       string
	sourceRevision   string
	artifactRevision string
	artifactIdentity string
	kind             string
	logicalID        string
	pid              int
	procStart        string
	strongStart      string
	endpoint         string
	relatedSessions  []string
	relatedLanes     []string
	priority         int
}

type productionLegacyRecordInventory struct {
	candidates      []LegacyRuntimeCandidate
	supervisorLanes map[string][]string
}

// productionLegacyBridgeRecords reads only the Agent Sessions-owned record
// families below the exact bridge-state root. It never searches a home tree or
// treats a process name as ownership evidence.
func productionLegacyBridgeRecords(
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
	observe func(productionLegacyRecordDescriptor, []LegacyInventorySource, int64) (LegacyRuntimeCandidate, error),
) (productionLegacyRecordInventory, error) {
	result := productionLegacyRecordInventory{supervisorLanes: make(map[string][]string)}
	bridgeRoot := productionLegacySourcePath(sources, "bridge-state")
	if bridgeRoot == "" {
		return result, nil
	}
	info, err := os.Lstat(bridgeRoot)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !productionLegacyOwned(info) {
		candidate, classifyErr := productionUnknownLegacyCandidate(LegacyInventorySource{
			ID: "bridge-state", Kind: "state", Path: bridgeRoot, Target: true,
		}, observedAt, "unsafe-bridge-state")
		if classifyErr != nil {
			return result, classifyErr
		}
		result.candidates = append(result.candidates, candidate)
		return result, nil
	}
	descriptors, malformed, err := productionLegacySessionDescriptors(ctx, bridgeRoot, observedAt)
	if err != nil {
		return result, err
	}
	result.candidates = append(result.candidates, malformed...)
	rootRecords, err := productionLegacyProfileRecordCandidates(ctx, bridgeRoot, observedAt)
	if err != nil {
		return result, err
	}
	result.candidates = append(result.candidates, rootRecords...)
	profileDescriptors, profileMalformed, supervisorLanes, err := productionLegacyProfileDescriptors(
		ctx, bridgeRoot, observedAt,
	)
	if err != nil {
		return result, err
	}
	descriptors = append(descriptors, profileDescriptors...)
	result.candidates = append(result.candidates, profileMalformed...)
	result.supervisorLanes = supervisorLanes

	for _, descriptor := range deduplicateProductionLegacyDescriptors(descriptors) {
		candidate, classifyErr := observe(descriptor, sources, observedAt)
		if classifyErr != nil {
			return result, classifyErr
		}
		result.candidates = append(result.candidates, candidate)
	}
	sort.Slice(result.candidates, func(i, j int) bool {
		return result.candidates[i].CandidateID < result.candidates[j].CandidateID
	})
	return result, nil
}

func productionLegacySessionDescriptors(
	ctx context.Context,
	bridgeRoot string,
	observedAt int64,
) ([]productionLegacyRecordDescriptor, []LegacyRuntimeCandidate, error) {
	directory := filepath.Join(bridgeRoot, "sessions")
	entries, err := readProductionLegacyDirectory(directory)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var descriptors []productionLegacyRecordDescriptor
	var malformed []LegacyRuntimeCandidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name(), "state.json")
		record, artifactRevision, artifactIdentity, readErr := readProductionLegacyProjectionWithArtifact(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			candidate, classifyErr := productionMalformedRecordCandidate(path, "shim", observedAt)
			if classifyErr != nil {
				return nil, nil, classifyErr
			}
			malformed = append(malformed, candidate)
			continue
		}
		sessionID, valueErr := productionProjectionString(record, "sessionId", true)
		pid, pidErr := productionProjectionInt(record, "pid", false)
		procStart, startErr := productionProjectionString(record, "procStart", false)
		endpoint, endpointErr := productionProjectionString(record, "socketPath", false)
		entrypoint, entrypointErr := productionProjectionString(record, "entrypoint", true)
		if errors.Join(valueErr, pidErr, startErr, endpointErr, entrypointErr) != nil ||
			!durableRecordID.MatchString(sessionID) {
			candidate, classifyErr := productionMalformedRecordCandidate(path, "shim", observedAt)
			if classifyErr != nil {
				return nil, nil, classifyErr
			}
			malformed = append(malformed, candidate)
			continue
		}
		kind := productionLegacySessionKind(entrypoint)
		revision, revisionErr := productionLegacyMetadataRevision(record)
		if revisionErr != nil {
			return nil, nil, revisionErr
		}
		descriptors = append(descriptors, productionLegacyRecordDescriptor{
			sourcePath: path, sourceRevision: revision, artifactRevision: artifactRevision,
			artifactIdentity: artifactIdentity, kind: kind,
			logicalID: sessionID, pid: pid, procStart: procStart, endpoint: endpoint,
			relatedSessions: []string{sessionID}, priority: 10,
		})
	}
	return descriptors, malformed, nil
}

func productionLegacyProfileDescriptors(
	ctx context.Context,
	bridgeRoot string,
	observedAt int64,
) ([]productionLegacyRecordDescriptor, []LegacyRuntimeCandidate, map[string][]string, error) {
	profiles := filepath.Join(bridgeRoot, "profiles")
	entries, err := readProductionLegacyDirectory(profiles)
	if os.IsNotExist(err) {
		return nil, nil, map[string][]string{}, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var descriptors []productionLegacyRecordDescriptor
	var malformed []LegacyRuntimeCandidate
	supervisorLanes := make(map[string][]string)
	for _, profile := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		if !profile.IsDir() {
			continue
		}
		profileRoot := filepath.Join(profiles, profile.Name())
		families := []struct {
			directory, kind, idField, pidField, startField, strongField, endpointField string
		}{
			{"claude-lanes", "claude_lane_manager", "sessionId", "managerPid", "managerProcStart", "", "controlSocket"},
			{"grok-lanes", "grok_lane_manager", "sessionId", "managerPid", "managerProcStart", "managerStrongStart", "controlSocket"},
			{"qwen-lanes", "qwen_lane_manager", "threadId", "managerPid", "managerProcStart", "managerStrongStart", "controlSocket"},
			{"grok-launches", "grok_host", "sessionId", "hostPid", "hostProcStart", "", "controlSocket"},
		}
		for _, family := range families {
			familyDescriptors, familyMalformed, familyErr := productionLegacyDescriptorDirectory(
				ctx, filepath.Join(profileRoot, family.directory), family.kind, family.idField,
				family.pidField, family.startField, family.strongField, family.endpointField, observedAt,
			)
			if familyErr != nil {
				return nil, nil, nil, familyErr
			}
			descriptors = append(descriptors, familyDescriptors...)
			malformed = append(malformed, familyMalformed...)
		}
		lanes, laneCandidates, laneErr := productionLegacyCodexLaneRecords(
			ctx, filepath.Join(profileRoot, "lanes"), observedAt,
		)
		if laneErr != nil {
			return nil, nil, nil, laneErr
		}
		supervisorLanes[profile.Name()] = sortedUniqueStrings(lanes)
		malformed = append(malformed, laneCandidates...)
		profileRecords, recordErr := productionLegacyProfileRecordCandidates(ctx, profileRoot, observedAt)
		if recordErr != nil {
			return nil, nil, nil, recordErr
		}
		malformed = append(malformed, profileRecords...)
	}
	return descriptors, malformed, supervisorLanes, nil
}

func productionLegacyProfileRecordCandidates(
	ctx context.Context,
	profileRoot string,
	observedAt int64,
) ([]LegacyRuntimeCandidate, error) {
	owners, err := productionLegacyInteractiveOwnerCandidates(
		ctx, filepath.Join(profileRoot, "interactive-owners"), observedAt,
	)
	if err != nil {
		return nil, err
	}
	retired, err := productionLegacyRetiredRecordCandidates(
		ctx, filepath.Join(profileRoot, "retired"), observedAt,
	)
	if err != nil {
		return nil, err
	}
	return append(owners, retired...), nil
}

func productionLegacyInteractiveOwnerCandidates(
	ctx context.Context,
	directory string,
	observedAt int64,
) ([]LegacyRuntimeCandidate, error) {
	entries, err := readProductionLegacyDirectory(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []LegacyRuntimeCandidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		candidate, err := productionLegacyInteractiveOwnerCandidate(directory, entry, observedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, nil
}

func productionLegacyInteractiveOwnerCandidate(
	directory string,
	entry os.DirEntry,
	observedAt int64,
) (LegacyRuntimeCandidate, error) {
	path := filepath.Join(directory, entry.Name())
	record, artifactRevision, artifactIdentity, readErr := readProductionLegacyProjectionWithArtifact(path)
	threadID, threadErr := productionProjectionString(record, "threadId", true)
	requestID, requestErr := productionProjectionString(record, "requestId", true)
	ownerPID, pidErr := productionProjectionInt(record, "ownerPid", true)
	ownerStart, startErr := productionProjectionString(record, "ownerProcStart", true)
	updatedAt := productionLegacyRawInt64(record, "updatedAt")
	if readErr != nil || errors.Join(threadErr, requestErr, pidErr, startErr) != nil || updatedAt <= 0 ||
		!durableRecordID.MatchString(threadID) || strings.TrimSpace(requestID) == "" ||
		entry.Name() != sessionkey.FromID(threadID)+".json" {
		return productionMalformedRecordCandidate(path, "interactive_owner_record", observedAt)
	}
	revision, err := productionLegacyMetadataRevision(record)
	if err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	classification, processStatus, strongStart := productionLegacyOwnerProcessClassification(ownerPID, ownerStart)
	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion,
		CandidateID:   quiescenceRecordID("legacy-interactive-owner-record", path, threadID, requestID),
		Kind:          "interactive_owner_record", SourcePath: path, SourceRevision: revision,
		ArtifactRevision: artifactRevision, ArtifactIdentity: artifactIdentity,
		PID: ownerPID, ProcStart: ownerStart, StrongStart: strongStart, ProcessStatus: processStatus,
		EndpointStatus: "absent", RelatedSessionIDs: []string{threadID}, Classification: classification,
		EvidenceRevision: 1, LastObservedAt: observedAt,
	}
	if err := candidate.Validate(); err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	return candidate, nil
}

func productionLegacyOwnerProcessClassification(ownerPID int, ownerStart string) (string, string, string) {
	identity := procinfo.Read(ownerPID)
	switch {
	case identity.Status == procinfo.Absent:
		return LegacyClassificationStale, "absent", ""
	case identity.Status == procinfo.Known && identity.Start != ownerStart:
		return LegacyClassificationStale, "absent", ""
	case identity.Status == procinfo.Known:
		return LegacyClassificationActiveManagedBlocker, "known", identity.StrongStart
	default:
		return LegacyClassificationUnknown, "unknown", ""
	}
}

func productionLegacyRetiredRecordCandidates(
	ctx context.Context,
	directory string,
	observedAt int64,
) ([]LegacyRuntimeCandidate, error) {
	entries, err := readProductionLegacyDirectory(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []LegacyRuntimeCandidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record, artifactRevision, artifactIdentity, readErr := readProductionLegacyProjectionWithArtifact(path)
		threadID, threadErr := productionProjectionString(record, "threadId", true)
		retiredAt := productionLegacyRawInt64(record, "retiredAt")
		if readErr != nil || threadErr != nil || retiredAt <= 0 || !durableRecordID.MatchString(threadID) ||
			entry.Name() != sessionkey.FromID(threadID)+".json" {
			candidate, classifyErr := productionMalformedRecordCandidate(path, "retired_record", observedAt)
			if classifyErr != nil {
				return nil, classifyErr
			}
			result = append(result, candidate)
			continue
		}
		revision, revisionErr := productionLegacyMetadataRevision(record)
		if revisionErr != nil {
			return nil, revisionErr
		}
		candidate := LegacyRuntimeCandidate{
			SchemaVersion: MigrationSchemaVersion,
			CandidateID:   quiescenceRecordID("legacy-retired-record", path, threadID),
			Kind:          "retired_record", SourcePath: path, SourceRevision: revision,
			ArtifactRevision: artifactRevision, ArtifactIdentity: artifactIdentity,
			ProcessStatus: "absent", EndpointStatus: "absent", RelatedSessionIDs: []string{threadID},
			Classification: LegacyClassificationRetired, EvidenceRevision: 1, LastObservedAt: observedAt,
		}
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, nil
}

func productionLegacyDescriptorDirectory(
	ctx context.Context,
	directory, kind, idField, pidField, startField, strongField, endpointField string,
	observedAt int64,
) ([]productionLegacyRecordDescriptor, []LegacyRuntimeCandidate, error) {
	entries, err := readProductionLegacyDirectory(directory)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var descriptors []productionLegacyRecordDescriptor
	var malformed []LegacyRuntimeCandidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record, artifactRevision, artifactIdentity, readErr := readProductionLegacyProjectionWithArtifact(path)
		logicalID, idErr := productionProjectionString(record, idField, true)
		pid, pidErr := productionProjectionInt(record, pidField, false)
		procStart, startErr := productionProjectionString(record, startField, false)
		strongStart, strongErr := productionProjectionString(record, strongField, false)
		endpoint, endpointErr := productionProjectionString(record, endpointField, false)
		if readErr != nil || errors.Join(idErr, pidErr, startErr, strongErr, endpointErr) != nil ||
			!durableRecordID.MatchString(logicalID) {
			candidate, classifyErr := productionMalformedRecordCandidate(path, kind, observedAt)
			if classifyErr != nil {
				return nil, nil, classifyErr
			}
			malformed = append(malformed, candidate)
			continue
		}
		revision, revisionErr := productionLegacyMetadataRevision(record)
		if revisionErr != nil {
			return nil, nil, revisionErr
		}
		descriptor := productionLegacyRecordDescriptor{
			sourcePath: path, sourceRevision: revision, artifactRevision: artifactRevision,
			artifactIdentity: artifactIdentity, kind: kind,
			logicalID: logicalID, pid: pid, procStart: procStart, strongStart: strongStart,
			endpoint: endpoint, priority: 20,
		}
		if strings.Contains(kind, "lane_manager") {
			descriptor.relatedLanes = []string{logicalID}
		} else {
			descriptor.relatedSessions = []string{logicalID}
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, malformed, nil
}

func productionLegacyCodexLaneRecords( //nolint:gocyclo // Closed schema/status validation stays adjacent to provenance classification.
	ctx context.Context,
	directory string,
	observedAt int64,
) ([]string, []LegacyRuntimeCandidate, error) {
	entries, err := readProductionLegacyDirectory(directory)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var lanes []string
	var candidates []LegacyRuntimeCandidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record, artifactRevision, artifactIdentity, readErr := readProductionLegacyProjectionWithArtifact(path)
		if readErr != nil {
			candidate, classifyErr := productionMalformedRecordCandidate(path, "codex_lane_record", observedAt)
			if classifyErr != nil {
				return nil, nil, classifyErr
			}
			candidates = append(candidates, candidate)
			continue
		}
		laneID, idErr := productionProjectionString(record, "sessionId", true)
		status, statusErr := productionProjectionString(record, "status", true)
		revision, revisionErr := productionLegacyMetadataRevision(record)
		if revisionErr != nil {
			return nil, nil, revisionErr
		}
		// A Codex lane row is durable conversation metadata. It contains no
		// manager PID/start or endpoint owner identity and therefore can never
		// independently authorize cleanup or become identity debt. Live authority
		// is established only by the separately corroborated supervisor process;
		// terminal rows remain adoptable metadata after that authority exits. A
		// bounded, valid-JSON row with incomplete lane identity is likewise
		// preserved as retired source metadata; adoption records exact repair debt
		// instead of misreporting it as unknown process identity.
		relatedLaneIDs := []string(nil)
		if durableRecordID.MatchString(laneID) {
			relatedLaneIDs = []string{laneID}
		}
		candidateID := quiescenceRecordID("legacy-codex-lane-record", path)
		if durableRecordID.MatchString(laneID) {
			candidateID = quiescenceRecordID("legacy-codex-lane-record", path, laneID)
		}
		candidate := LegacyRuntimeCandidate{
			SchemaVersion: MigrationSchemaVersion,
			CandidateID:   candidateID,
			Kind:          "codex_lane_record", SourcePath: path, SourceRevision: revision,
			ArtifactRevision: artifactRevision, ArtifactIdentity: artifactIdentity,
			ProcessStatus: "absent", EndpointStatus: "absent",
			RelatedLaneIDs: relatedLaneIDs, Classification: LegacyClassificationRetired,
			EvidenceRevision: 1, LastObservedAt: observedAt,
		}
		if err := candidate.Validate(); err != nil {
			return nil, nil, err
		}
		candidates = append(candidates, candidate)
		if errors.Join(idErr, statusErr) == nil && durableRecordID.MatchString(laneID) &&
			productionLegacyCodexLaneStatusKnown(status) && productionLegacyCodexLaneStatusMayBeLive(status) {
			lanes = append(lanes, laneID)
		}
	}
	return lanes, candidates, nil
}

func productionLegacyCodexLaneStatusKnown(status string) bool {
	switch status {
	case "starting", "in_progress", "active", "running", "busy", "idle",
		"completed", "failed", "interrupted", "timed_out", "cancelled", "canceled", "archived":
		return true
	default:
		return false
	}
}

func productionLegacyCodexLaneStatusMayBeLive(status string) bool {
	switch status {
	case "starting", "in_progress", "active", "running", "busy", "idle":
		return true
	case "completed", "failed", "interrupted", "timed_out", "cancelled", "canceled", "archived":
		return false
	default:
		// Unknown durable status is not process authority. Omitting it from the
		// supervisor's related-live set prevents a metadata-only blocker while
		// preserving the row for adoption.
		return false
	}
}

func classifyProductionLegacyRecord(
	descriptor productionLegacyRecordDescriptor,
	sources []LegacyInventorySource,
	observedAt int64,
) (LegacyRuntimeCandidate, error) {
	evidence := LegacyCandidateEvidence{
		CandidateID: quiescenceRecordID("legacy-"+strings.ReplaceAll(descriptor.kind, "_", "-"), descriptor.sourcePath, descriptor.logicalID),
		Kind:        descriptor.kind, SourcePath: descriptor.sourcePath, SourceRevision: descriptor.sourceRevision,
		ArtifactRevision: descriptor.artifactRevision,
		ArtifactIdentity: descriptor.artifactIdentity,
		PID:              descriptor.pid, ProcStart: descriptor.procStart, StrongStart: descriptor.strongStart,
		RelatedSessionIDs: append([]string(nil), descriptor.relatedSessions...),
		RelatedLaneIDs:    append([]string(nil), descriptor.relatedLanes...),
		EvidenceRevision:  1, ObservedAt: observedAt,
		Process:  LegacyProcessObservation{Status: "unknown"},
		Endpoint: LegacyEndpointObservation{Status: "unknown", Path: descriptor.endpoint},
		Service:  LegacyServiceObservation{Status: "unknown"},
	}
	if descriptor.pid <= 1 {
		evidence.Process.Status = "absent"
		evidence.Endpoint = productionLegacyAbsentEndpointObservation(descriptor.endpoint)
		return ClassifyLegacyCandidate(evidence)
	}
	identity := procinfo.Read(descriptor.pid)
	switch identity.Status {
	case procinfo.Absent:
		evidence.Process.Status = "absent"
		evidence.Endpoint = productionLegacyAbsentEndpointObservation(descriptor.endpoint)
		return ClassifyLegacyCandidate(evidence)
	case procinfo.Unknown:
		return ClassifyLegacyCandidate(evidence)
	case procinfo.Known:
		if identity.Start != descriptor.procStart || descriptor.strongStart != "" && identity.StrongStart != descriptor.strongStart {
			evidence.Process.Status = "absent"
			evidence.Endpoint = productionLegacyAbsentEndpointObservation(descriptor.endpoint)
			return ClassifyLegacyCandidate(evidence)
		}
	default:
		return LegacyRuntimeCandidate{}, errors.New("legacy process probe returned an unsupported status")
	}
	if !migrationAbsoluteCleanPath(descriptor.endpoint) || !productionPathWithinLegacyRuntime(descriptor.endpoint, sources) {
		return ClassifyLegacyCandidate(evidence)
	}
	executable, err := productionProcessExecutable(descriptor.pid)
	if err != nil {
		return ClassifyLegacyCandidate(evidence)
	}
	arguments, err := procinfo.Args(descriptor.pid)
	if err != nil || !productionLegacyArgumentsMatch(descriptor.kind, descriptor.logicalID, arguments) {
		return ClassifyLegacyCandidate(evidence)
	}
	runtimeIdentity, err := hostExecutableIdentity(executable)
	if err != nil {
		return ClassifyLegacyCandidate(evidence)
	}
	peer, endpointStatus := observeProductionLegacyRecordEndpoint(descriptor.endpoint)
	evidence.RuntimeIdentity = runtimeIdentity
	evidence.StrongStart = identity.StrongStart
	evidence.Process = LegacyProcessObservation{
		Status: "known", PID: descriptor.pid, ProcStart: identity.Start, StrongStart: identity.StrongStart,
		Executable: executable, ArgvRole: descriptor.kind,
	}
	evidence.Endpoint = LegacyEndpointObservation{
		Status: endpointStatus, Path: descriptor.endpoint, Type: "unix", OwnerUID: peer.UID,
		OwnerPID: peer.PID, OwnerProcStart: peer.ProcStart, RuntimeIdentity: runtimeIdentity,
	}
	return ClassifyLegacyCandidate(evidence)
}

func observeProductionLegacyRecordEndpoint(path string) (controlPeerEvidence, string) {
	connection, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return controlPeerEvidence{}, productionLegacyEndpointAbsence(path, err)
	}
	defer func() { _ = connection.Close() }()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return controlPeerEvidence{}, "unknown"
	}
	peer, err := observeControlPeer(unixConnection)
	if err != nil || peer.UID != os.Getuid() {
		return controlPeerEvidence{}, "unknown"
	}
	return peer, "responsive"
}

func productionLegacyEndpointAbsence(path string, dialErr error) string {
	if path == "" {
		return "absent"
	}
	first, present, err := observeProductionLegacyPath(path)
	if err != nil {
		return "unknown"
	}
	if !present {
		return "absent"
	}
	if !first.ownedSocket() {
		return "unknown"
	}
	if !productionLegacyDialFailureProvesNoListener(dialErr) {
		return "unknown"
	}
	second, secondPresent, err := observeProductionLegacyPath(path)
	if err == nil && secondPresent && first.same(second) {
		return "absent"
	}
	return "unknown"
}

func productionLegacyDialFailureProvesNoListener(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func productionLegacyAbsentEndpointObservation(path string) LegacyEndpointObservation {
	peer, status := observeProductionLegacyRecordEndpoint(path)
	return LegacyEndpointObservation{
		Status: status, Path: path, Type: "unix", OwnerUID: peer.UID,
		OwnerPID: peer.PID, OwnerProcStart: peer.ProcStart,
	}
}

func productionLegacyArgumentsMatch(kind, logicalID string, arguments []string) bool {
	if len(arguments) < 2 {
		return false
	}
	if kind == "qwen_host" && arguments[1] == "qwen-host" {
		var registration struct {
			SessionID string `json:"session_id"`
		}
		encoded := productionLegacyArgumentValue(arguments[2:], "--registration-json")
		return encoded != "" && json.Unmarshal([]byte(encoded), &registration) == nil && registration.SessionID == logicalID
	}
	wantCommand := map[string]string{
		"shim": "shim", "claude_host": "shim", "grok_host": "grok-host", "qwen_host": "qwen-host",
		"claude_lane_manager": "claude-lane-manager", "grok_lane_manager": "grok-lane-manager",
		"qwen_lane_manager": "qwen-lane-manager",
	}[kind]
	if wantCommand == "" || arguments[1] != wantCommand {
		return false
	}
	return productionLegacyArgumentValue(arguments[2:], "--session-id") == logicalID
}

func productionLegacyArgumentValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func productionLegacyRemoteWatchCandidates(
	owners []LegacyRuntimeCandidate,
	observedAt int64,
) ([]LegacyRuntimeCandidate, error) {
	processes, err := procinfo.List()
	if err != nil {
		return nil, err
	}
	return productionLegacyRemoteWatchCandidatesFromProcesses(
		owners, observedAt, processes, procinfo.Args, productionProcessExecutable,
	)
}

func productionLegacyRemoteWatchCandidatesFromProcesses(
	owners []LegacyRuntimeCandidate,
	observedAt int64,
	processes []procinfo.Process,
	argumentsFor func(int) ([]string, error),
	executableFor func(int) (string, error),
) ([]LegacyRuntimeCandidate, error) {
	ownerByPID := make(map[int]LegacyRuntimeCandidate)
	for _, owner := range owners {
		if owner.ProcessStatus == "known" &&
			(len(owner.RelatedLaneIDs) != 0 || owner.Kind == "host_federation_agent") {
			ownerByPID[owner.PID] = owner
		}
	}
	if len(ownerByPID) == 0 {
		return nil, nil
	}
	byPID := make(map[int]procinfo.Process, len(processes))
	for _, process := range processes {
		byPID[process.PID] = process
	}
	var result []LegacyRuntimeCandidate
	for _, process := range processes {
		candidate, ok, err := productionLegacyRemoteWatchCandidate(
			process, byPID, ownerByPID, observedAt, argumentsFor, executableFor,
		)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, candidate)
		}
	}
	return result, nil
}

func productionLegacyRemoteWatchCandidate(
	process procinfo.Process,
	byPID map[int]procinfo.Process,
	ownerByPID map[int]LegacyRuntimeCandidate,
	observedAt int64,
	argumentsFor func(int) ([]string, error),
	executableFor func(int) (string, error),
) (LegacyRuntimeCandidate, bool, error) {
	owner, ok := productionLegacyAncestorOwner(process, byPID, ownerByPID)
	if !ok {
		return LegacyRuntimeCandidate{}, false, nil
	}
	arguments, err := argumentsFor(process.PID)
	if err != nil {
		return LegacyRuntimeCandidate{}, false, err
	}
	if len(arguments) < 2 || filepath.Base(arguments[0]) != "peer-federator" ||
		arguments[1] != "lane-watch" {
		return LegacyRuntimeCandidate{}, false, nil
	}
	executable, err := executableFor(process.PID)
	if err != nil {
		return LegacyRuntimeCandidate{}, false, err
	}
	identity, err := hostExecutableIdentity(executable)
	if err != nil {
		return LegacyRuntimeCandidate{}, false, err
	}
	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion,
		CandidateID:   quiescenceRecordID("legacy-remote-watch", owner.CandidateID, strconv.Itoa(process.PID), process.StrongStart),
		Kind:          "remote_lane_watch", SourcePath: owner.SourcePath,
		SourceRevision: owner.SourceRevision, RuntimeIdentity: identity,
		PID: process.PID, ProcStart: process.Start, StrongStart: process.StrongStart,
		ProcessStatus: "known", ProcessExecutable: executable, ProcessArgvRole: "remote_lane_watch",
		EndpointStatus: "unknown", RelatedLaneIDs: append([]string(nil), owner.RelatedLaneIDs...),
		Classification:   LegacyClassificationUnknown,
		EvidenceRevision: 1, LastObservedAt: observedAt,
	}
	if err := candidate.Validate(); err != nil {
		return LegacyRuntimeCandidate{}, false, err
	}
	return candidate, true, nil
}

func productionLegacyAncestorOwner(
	process procinfo.Process,
	byPID map[int]procinfo.Process,
	owners map[int]LegacyRuntimeCandidate,
) (LegacyRuntimeCandidate, bool) {
	parent := process.Parent
	for range 32 {
		if owner, ok := owners[parent]; ok {
			return owner, true
		}
		ancestor, ok := byPID[parent]
		if !ok || ancestor.Parent <= 1 || ancestor.Parent == parent {
			break
		}
		parent = ancestor.Parent
	}
	return LegacyRuntimeCandidate{}, false
}

func deduplicateProductionLegacyDescriptors(
	descriptors []productionLegacyRecordDescriptor,
) []productionLegacyRecordDescriptor {
	selected := make(map[string]productionLegacyRecordDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		key := descriptor.sourcePath
		if descriptor.pid > 1 && descriptor.procStart != "" {
			key = strconv.Itoa(descriptor.pid) + "\x00" + descriptor.procStart
		}
		current, exists := selected[key]
		if !exists || descriptor.priority > current.priority {
			selected[key] = descriptor
		}
	}
	result := make([]productionLegacyRecordDescriptor, 0, len(selected))
	for _, descriptor := range selected {
		result = append(result, descriptor)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].sourcePath < result[j].sourcePath })
	return result
}

func productionLegacySessionKind(entrypoint string) string {
	switch entrypoint {
	case "claude":
		return "claude_host"
	case "grok":
		return "grok_host"
	case "qwen":
		return "qwen_host"
	default:
		return "shim"
	}
}

func productionMalformedRecordCandidate(path, kind string, observedAt int64) (LegacyRuntimeCandidate, error) {
	return productionUnknownLegacyCandidate(LegacyInventorySource{
		ID: quiescenceRecordID("legacy-record", path), Kind: kind, Path: path, Target: true,
	}, observedAt, "malformed-legacy-record")
}

func productionLegacySourcePath(sources []LegacyInventorySource, id string) string {
	for _, source := range sources {
		if source.ID == id {
			return source.Path
		}
	}
	return ""
}

func readProductionLegacyDirectory(path string) ([]os.DirEntry, error) {
	return readProductionLegacyOwnedDirectory(path, maxProductionLegacyDirectoryEntries)
}

func readProductionLegacyProjection(path string) (map[string]json.RawMessage, error) {
	record := make(map[string]json.RawMessage)
	_, err := readProductionLegacyJSON(path, &record)
	return record, err
}

func readProductionLegacyProjectionWithArtifact(path string) (map[string]json.RawMessage, string, string, error) {
	record := make(map[string]json.RawMessage)
	body, identity, err := readProductionLegacyRegular(path, 1, maxProductionLegacyRecordBytes)
	if err != nil {
		return record, "", "", err
	}
	if err := decodeProductionLegacyJSON(body, &record); err != nil {
		return record, "", "", err
	}
	return record, "sha256:" + productionDigest(body), productionLegacyArtifactIdentityRevision(identity), nil
}

func productionLegacyMetadataRevision(record map[string]json.RawMessage) (string, error) {
	var value map[string]any
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return "", err
	}
	productionStripLegacyContent(value)
	metadata, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return "sha256:" + productionDigest(metadata), nil
}

func productionStripLegacyContent(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "prompt", "result", "resultframe", "message", "error":
				delete(typed, key)
			default:
				productionStripLegacyContent(child)
			}
		}
	case []any:
		for _, child := range typed {
			productionStripLegacyContent(child)
		}
	}
}

func productionProjectionString(record map[string]json.RawMessage, key string, required bool) (string, error) {
	if key == "" {
		return "", nil
	}
	body, exists := record[key]
	if !exists {
		if required {
			return "", fmt.Errorf("legacy record lacks %s", key)
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(body, &value); err != nil || required && strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("legacy record has invalid %s", key)
	}
	return value, nil
}

func productionProjectionInt(record map[string]json.RawMessage, key string, required bool) (int, error) {
	if key == "" {
		return 0, nil
	}
	body, exists := record[key]
	if !exists {
		if required {
			return 0, fmt.Errorf("legacy record lacks %s", key)
		}
		return 0, nil
	}
	var value int
	if err := json.Unmarshal(body, &value); err != nil || required && value <= 1 {
		return 0, fmt.Errorf("legacy record has invalid %s", key)
	}
	return value, nil
}
