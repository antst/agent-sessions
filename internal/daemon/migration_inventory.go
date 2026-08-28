package daemon

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	legacyStateInventoryDepth   = 5
	legacyRuntimeInventoryDepth = 3
)

// LegacyInventorySources returns the closed set of Agent Sessions-owned
// legacy roots shipped on the requested platform. Every result is an exact
// root or service file; callers must not expand the inventory to home or
// process-name scans.
func LegacyInventorySources(options LegacyInventoryOptions) ([]LegacyInventorySource, error) { //nolint:gocyclo // The platform source allowlist remains explicit and closed.
	if options.UID < 0 {
		return nil, fmt.Errorf("legacy inventory has invalid uid %d", options.UID)
	}
	if options.Platform != "linux" && options.Platform != "darwin" {
		return nil, fmt.Errorf("legacy inventory has unsupported platform %q", options.Platform)
	}
	for name, path := range map[string]string{
		"home":        options.HomeDir,
		"state home":  options.StateHome,
		"system temp": options.SystemTempDir,
	} {
		if !legacyInventoryRootIsExact(path) {
			return nil, fmt.Errorf("legacy inventory %s is not an exact absolute root", name)
		}
	}
	if options.RuntimeDir != "" && !legacyInventoryRootIsExact(options.RuntimeDir) {
		return nil, fmt.Errorf("legacy inventory runtime dir is not an exact absolute root")
	}
	if options.Platform == "linux" && options.RuntimeDir == "" {
		return nil, fmt.Errorf("legacy Linux inventory requires an exact runtime dir")
	}

	uid := strconv.Itoa(options.UID)
	sources := []LegacyInventorySource{
		legacyInventorySource("bridge-state", "state", filepath.Join(options.StateHome, "claude-code-peer"), legacyStateInventoryDepth),
		legacyInventorySource("host-agent-state", "state", filepath.Join(options.StateHome, "agent-sessions", "agents"), legacyStateInventoryDepth),
	}

	switch options.Platform {
	case "linux":
		sources = append(sources,
			legacyInventorySource("bridge-xdg-runtime", "runtime", filepath.Join(options.RuntimeDir, "codex-claude-peer-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("bridge-temp-runtime", "runtime", filepath.Join(options.SystemTempDir, "codex-claude-peer-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("bridge-compact-runtime", "runtime", filepath.Join(options.SystemTempDir, "ccp-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("federator-xdg-runtime", "runtime", filepath.Join(options.RuntimeDir, "peer-federator"), legacyRuntimeInventoryDepth),
			legacyInventorySource("federator-temp-runtime", "runtime", filepath.Join(options.SystemTempDir, "peer-federator-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("grok-xdg-runtime", "runtime", filepath.Join(options.RuntimeDir, "agent-sessions-grok-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("grok-temp-runtime", "runtime", filepath.Join(options.SystemTempDir, "agent-sessions-grok-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("grok-compact-runtime", "runtime", filepath.Join(options.SystemTempDir, "asg-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("systemd-host-agent", "service", filepath.Join(options.HomeDir, ".config", "systemd", "user", "peer-federator-agent.service"), 0),
			legacyInventorySource("systemd-host-agent-env", "configuration", filepath.Join(options.HomeDir, ".config", "peer-federator", "agent.env"), 0),
		)
	case "darwin":
		sources = append(sources,
			legacyInventorySource("bridge-compact-runtime", "runtime", filepath.Join("/tmp", "ccp-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("bridge-tmp-runtime", "runtime", filepath.Join("/tmp", "codex-claude-peer-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("bridge-darwin-compact-runtime", "runtime", filepath.Join(options.SystemTempDir, "ccp-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("bridge-darwin-runtime", "runtime", filepath.Join(options.SystemTempDir, "codex-claude-peer-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("federator-temp-runtime", "runtime", filepath.Join(options.SystemTempDir, "peer-federator-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("grok-darwin-runtime", "runtime", filepath.Join(options.SystemTempDir, "agent-sessions-grok-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("grok-compact-runtime", "runtime", filepath.Join("/tmp", "asg-"+uid), legacyRuntimeInventoryDepth),
			legacyInventorySource("launchd-host-agent", "service", filepath.Join(options.HomeDir, "Library", "LaunchAgents", "net.antst.peer-federator.agent.plist"), 0),
		)
		sources = deduplicateLegacyInventorySources(sources)
	}

	seenPaths := make(map[string]struct{}, len(sources)+len(options.RecordedRuntimeRoots))
	for _, source := range sources {
		seenPaths[source.Path] = struct{}{}
	}
	recordedIndexes := make(map[string]int)
	for _, path := range options.RecordedRuntimeRoots {
		if options.Platform != "darwin" {
			return nil, fmt.Errorf("recorded TMPDIR roots are supported only for Darwin inventory")
		}
		id, allowed := legacyRecordedRuntimeSourceID(path, uid)
		if !legacyInventoryRootIsExact(path) || !allowed {
			return nil, fmt.Errorf("recorded legacy runtime root %q is outside the shipped closed list", path)
		}
		if _, exists := seenPaths[path]; exists {
			continue
		}
		recordedIndexes[id]++
		if recordedIndexes[id] > 1 {
			id += "-" + strconv.Itoa(recordedIndexes[id])
		}
		sources = append(sources, legacyInventorySource(id, "recorded_runtime", path, legacyRuntimeInventoryDepth))
		seenPaths[path] = struct{}{}
	}
	for _, source := range sources {
		if legacyInventoryPathIsExcluded(source.Path) {
			return nil, fmt.Errorf("legacy inventory source %q crosses an excluded hub or vendor boundary", source.Path)
		}
	}

	return sources, nil
}

func deduplicateLegacyInventorySources(sources []LegacyInventorySource) []LegacyInventorySource {
	seen := make(map[string]struct{}, len(sources))
	result := make([]LegacyInventorySource, 0, len(sources))
	for _, source := range sources {
		if _, duplicate := seen[source.Path]; duplicate {
			continue
		}
		seen[source.Path] = struct{}{}
		result = append(result, source)
	}
	return result
}

// ClassifyLegacyCandidate reconciles exact durable, process, endpoint, and
// service evidence. It deliberately treats path, PID, executable name, and
// scalar counts as non-authoritative unless all role-specific identities
// corroborate one another.
func ClassifyLegacyCandidate(evidence LegacyCandidateEvidence) (LegacyRuntimeCandidate, error) {
	evidenceRevision := evidence.EvidenceRevision
	if evidenceRevision == 0 {
		evidenceRevision = 1
	}
	lastObservedAt := evidence.ObservedAt
	if lastObservedAt == 0 {
		lastObservedAt = 1
	}
	candidate := LegacyRuntimeCandidate{
		SchemaVersion:           MigrationSchemaVersion,
		CandidateID:             evidence.CandidateID,
		Kind:                    evidence.Kind,
		SourcePath:              evidence.SourcePath,
		SourceRevision:          evidence.SourceRevision,
		ArtifactRevision:        evidence.ArtifactRevision,
		ArtifactIdentity:        evidence.ArtifactIdentity,
		ReportedVersion:         evidence.ReportedVersion,
		RuntimeIdentity:         evidence.RuntimeIdentity,
		PID:                     evidence.PID,
		ProcStart:               evidence.ProcStart,
		StrongStart:             evidence.StrongStart,
		ProcessStatus:           evidence.Process.Status,
		EndpointPath:            evidence.Endpoint.Path,
		EndpointStatus:          evidence.Endpoint.Status,
		EndpointType:            evidence.Endpoint.Type,
		EndpointOwnerUID:        evidence.Endpoint.OwnerUID,
		EndpointOwnerPID:        evidence.Endpoint.OwnerPID,
		EndpointOwnerStart:      evidence.Endpoint.OwnerProcStart,
		EndpointRuntimeIdentity: evidence.Endpoint.RuntimeIdentity,
		ServiceManager:          evidence.Service.Manager,
		ServiceUnit:             evidence.Service.Unit,
		ServiceStatus:           evidence.Service.Status,
		ServiceExecutable:       evidence.Service.Executable,
		ServiceArgvRole:         evidence.Service.ArgvRole,
		ProcessExecutable:       evidence.Process.Executable,
		ProcessArgvRole:         evidence.Process.ArgvRole,
		RelatedSessionIDs:       append([]string(nil), evidence.RelatedSessionIDs...),
		RelatedLaneIDs:          append([]string(nil), evidence.RelatedLaneIDs...),
		ReportedActiveCount:     evidence.ReportedActiveCount,
		EvidenceRevision:        evidenceRevision,
		LastObservedAt:          lastObservedAt,
	}

	switch {
	case legacyExcludedCandidateKind(evidence.Kind):
		candidate.Classification = LegacyClassificationExcluded
	case evidence.Kind == "host_agent_service_job":
		candidate.Classification = classifyLegacyService(evidence.Service)
	case legacyProcessCandidateKind(evidence.Kind):
		candidate.Classification = classifyLegacyProcess(evidence)
	default:
		candidate.Classification = LegacyClassificationUnknown
	}

	if err := candidate.Validate(); err != nil {
		return LegacyRuntimeCandidate{}, fmt.Errorf("classify legacy candidate: %w", err)
	}
	return candidate, nil
}

func legacyInventorySource(id, kind, path string, maxDepth int) LegacyInventorySource {
	return LegacyInventorySource{ID: id, Kind: kind, Path: path, Target: true, MaxDepth: maxDepth}
}

func legacyInventoryRootIsExact(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	volume := filepath.VolumeName(path)
	return path != string(filepath.Separator) && path != volume+string(filepath.Separator)
}

func legacyRecordedBridgeRuntime(path, uid string) bool {
	base := filepath.Base(path)
	return base == "ccp-"+uid || base == "codex-claude-peer-"+uid
}

func legacyRecordedRuntimeSourceID(path, uid string) (string, bool) {
	if legacyRecordedBridgeRuntime(path, uid) {
		return "recorded-bridge-runtime", true
	}
	if filepath.Base(path) == "peer-federator-"+uid {
		return "recorded-federator-runtime", true
	}
	return "", false
}

func legacyInventoryPathIsExcluded(path string) bool {
	if strings.Contains(path, "agent-sessions-hub") ||
		strings.Contains(path, "peer-federator-hub") ||
		strings.Contains(path, "peer-federator.hub") {
		return true
	}
	for _, component := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		switch component {
		case ".codex", ".claude", ".grok", ".qwen":
			return true
		}
	}
	return false
}

func legacyExcludedCandidateKind(kind string) bool {
	switch kind {
	case "central_hub", "native_codex", "native_claude", "native_grok", "native_qwen":
		return true
	default:
		return false
	}
}

func legacyProcessCandidateKind(kind string) bool {
	switch kind {
	case "supervisor", "shim", "claude_host", "grok_host", "qwen_host",
		"claude_lane_manager", "grok_lane_manager", "qwen_lane_manager",
		"remote_lane_watch", "host_federation_agent", "supervisor_socket_artifact",
		"host_agent_socket_artifact":
		return true
	default:
		return false
	}
}

func classifyLegacyProcess(evidence LegacyCandidateEvidence) string { //nolint:gocyclo // Evidence conflict cases stay explicit and fail closed.
	process := evidence.Process
	endpoint := evidence.Endpoint
	if process.Status == "absent" && endpoint.Status == "absent" {
		return LegacyClassificationStale
	}
	if process.Status == "unknown" || endpoint.Status == "unknown" {
		return LegacyClassificationUnknown
	}
	if process.Status == "absent" || endpoint.Status == "absent" {
		return LegacyClassificationConflicting
	}
	if process.Status != "known" || endpoint.Status != "responsive" {
		return LegacyClassificationUnknown
	}

	if legacyProcessEvidenceIncomplete(evidence) {
		return LegacyClassificationUnknown
	}
	if process.PID != evidence.PID || process.ProcStart != evidence.ProcStart ||
		process.StrongStart != evidence.StrongStart || process.ArgvRole != evidence.Kind ||
		!legacyProcessExecutableMatchesRole(evidence.Kind, process.Executable) ||
		endpoint.Type != "unix" || endpoint.OwnerPID != evidence.PID ||
		endpoint.OwnerProcStart != evidence.ProcStart || endpoint.RuntimeIdentity != evidence.RuntimeIdentity {
		return LegacyClassificationConflicting
	}
	if len(evidence.RelatedSessionIDs)+len(evidence.RelatedLaneIDs) > 0 {
		return LegacyClassificationActiveManagedBlocker
	}
	return LegacyClassificationLiveLegacyAuthority
}

func legacyProcessExecutableMatchesRole(kind, executable string) bool {
	base := filepath.Base(executable)
	if base == "agent-session-runtime" {
		return true
	}
	switch kind {
	case "claude_host":
		return base == "claude-peer"
	case "remote_lane_watch", "host_federation_agent":
		return base == "peer-federator"
	default:
		return false
	}
}

func legacyProcessEvidenceIncomplete(evidence LegacyCandidateEvidence) bool {
	process := evidence.Process
	endpoint := evidence.Endpoint
	return evidence.PID <= 0 || strings.TrimSpace(evidence.ProcStart) == "" ||
		strings.TrimSpace(evidence.StrongStart) == "" || strings.TrimSpace(evidence.RuntimeIdentity) == "" ||
		process.PID <= 0 || strings.TrimSpace(process.ProcStart) == "" ||
		strings.TrimSpace(process.StrongStart) == "" || !migrationAbsoluteCleanPath(process.Executable) ||
		strings.TrimSpace(process.ArgvRole) == "" || !migrationAbsoluteCleanPath(endpoint.Path) ||
		strings.TrimSpace(endpoint.Type) == "" || endpoint.OwnerUID < 0 || endpoint.OwnerPID <= 0 ||
		strings.TrimSpace(endpoint.OwnerProcStart) == "" || strings.TrimSpace(endpoint.RuntimeIdentity) == ""
}

func classifyLegacyService(service LegacyServiceObservation) string {
	if service.Status == "absent" {
		return LegacyClassificationStale
	}
	if service.Status == "unknown" || service.Status == "" {
		return LegacyClassificationUnknown
	}
	if service.Status != "loaded" {
		return LegacyClassificationUnknown
	}
	if !migrationAbsoluteCleanPath(service.Executable) || service.ArgvRole != "agent" {
		return LegacyClassificationUnknown
	}
	switch service.Manager {
	case "systemd-user":
		if service.Unit != "peer-federator-agent.service" {
			return LegacyClassificationConflicting
		}
	case "launchd-user":
		if service.Unit != "net.antst.peer-federator.agent" {
			return LegacyClassificationConflicting
		}
	default:
		return LegacyClassificationUnknown
	}
	if filepath.Base(service.Executable) != "peer-federator" {
		return LegacyClassificationConflicting
	}
	return LegacyClassificationLiveLegacyAuthority
}
