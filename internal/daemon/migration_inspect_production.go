package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const maxProductionLegacyRecordBytes = 4 << 20

type productionLegacyAgentStatus struct {
	RuntimeVersion       string   `json:"runtime_version"`
	ProtocolVersion      int      `json:"protocol_version"`
	HostID               string   `json:"host_id"`
	HostName             string   `json:"host_name"`
	Hub                  string   `json:"hub"`
	Connected            bool     `json:"connected"`
	LocalPeers           int      `json:"local_peers"`
	RemotePeers          int      `json:"remote_peers"`
	RemoteHosts          int      `json:"remote_hosts"`
	Capabilities         []string `json:"capabilities,omitempty"`
	RegistryDir          string   `json:"registry_dir"`
	RuntimeDir           string   `json:"runtime_dir"`
	StateDir             string   `json:"state_dir"`
	ClaudeConfigEnvSet   bool     `json:"claude_config_env_set"`
	ClaudeConfigEnvValue string   `json:"claude_config_env_value,omitempty"`
	ClaudeSecureConfig   string   `json:"claude_secure_config,omitempty"`
	ClaudeSecureEnvSet   bool     `json:"claude_secure_env_set"`
}

type productionLegacyCatalog struct {
	Version  int                                      `json:"version"`
	Sessions map[string]federation.SessionPreferences `json:"sessions"`
}

type productionLegacyName struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	Product   string `json:"product"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	UpdatedAt int64  `json:"updated_at"`
}

type productionLegacyAgentObservation struct {
	Status     productionLegacyAgentStatus
	Peer       controlPeerEvidence
	Executable string
	Identity   string
}

type productionLegacySupervisorState struct {
	AppServerSocket string `json:"appServerSocket"`
	ControlSocket   string `json:"controlSocket"`
	Implementation  string `json:"implementation"`
	PID             int    `json:"pid"`
	PluginVersion   string `json:"pluginVersion"`
	ProcStart       string `json:"procStart"`
	ProfileKey      string `json:"profileKey,omitempty"`
	RuntimeIdentity string `json:"runtimeIdentity"`
	StartedAt       int64  `json:"startedAt"`
}

type productionLegacySupervisorStatus struct {
	AppServerConnected bool   `json:"appServerConnected"`
	AppServerPID       int    `json:"appServerPid"`
	AppServerProcStart string `json:"appServerProcStart"`
	AppServerSocket    string `json:"appServerSocket"`
	ControlSocket      string `json:"controlSocket"`
	Implementation     string `json:"implementation"`
	OK                 bool   `json:"ok"`
	PID                int    `json:"pid"`
	PluginVersion      string `json:"pluginVersion"`
	ProcStart          string `json:"procStart"`
	ProfileKey         string `json:"profileKey,omitempty"`
	Ready              bool   `json:"ready"`
	RuntimeIdentity    string `json:"runtimeIdentity"`
	ShimCount          int    `json:"shimCount"`
}

type productionLegacyInspectionProbe struct {
	ObserveAgent    func(context.Context, string) (productionLegacyAgentObservation, error)
	ServiceLoaded   func(context.Context, string, string) (bool, error)
	ServiceActivity func(context.Context, string, string) (string, error)
	ObserveRecord   func(productionLegacyRecordDescriptor, []LegacyInventorySource, int64) (LegacyRuntimeCandidate, error)
	Supervisors     func(context.Context, []LegacyInventorySource, LegacyInventorySource, int64) ([]LegacyRuntimeCandidate, error)
	RemoteWatches   func([]LegacyRuntimeCandidate, int64) ([]LegacyRuntimeCandidate, error)
}

type productionLegacySupervisorEndpointProbe struct {
	Observe         func(string) (productionLegacySupervisorStatus, controlPeerEvidence, error)
	Executable      func(int) (string, error)
	Arguments       func(int) ([]string, error)
	RuntimeIdentity func(string) (string, error)
}

func collectProductionFirstMigration(
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
) (FirstMigrationInspection, error) {
	return collectProductionFirstMigrationWithProbe(ctx, sources, observedAt, productionLegacyInspectionProbe{
		ObserveAgent: observeProductionLegacyAgent, ServiceLoaded: productionLegacyServiceLoaded,
		ServiceActivity: productionLegacyServiceActivity,
		ObserveRecord:   classifyProductionLegacyRecord, Supervisors: productionLegacySupervisors,
		RemoteWatches: productionLegacyRemoteWatchCandidates,
	})
}

func collectProductionFirstMigrationWithProbe( //nolint:gocyclo // Closed-source reconciliation stays in one auditable collector.
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
	probe productionLegacyInspectionProbe,
) (FirstMigrationInspection, error) {
	if observedAt <= 0 {
		return FirstMigrationInspection{}, errors.New("legacy inspection requires a positive observation time")
	}
	if probe.ObserveAgent == nil || probe.ServiceLoaded == nil {
		return FirstMigrationInspection{}, errors.New("legacy inspection requires exact process and service probes")
	}
	if probe.ServiceActivity == nil {
		probe.ServiceActivity = productionLegacyServiceActivity
	}
	if probe.ObserveRecord == nil {
		probe.ObserveRecord = classifyProductionLegacyRecord
	}
	if probe.Supervisors == nil {
		probe.Supervisors = productionLegacySupervisors
	}
	if probe.RemoteWatches == nil {
		probe.RemoteWatches = productionLegacyRemoteWatchCandidates
	}
	stateRoot, serviceSource := productionLegacySpecialSources(sources)
	var candidates []LegacyRuntimeCandidate
	var adoption LegacyAdoptionRequest
	var prior LegacyPriorAuthority
	var versions []string
	seenSource := false
	agentObserved := false
	var fallbackSource LegacyInventorySource
	recordInventory, err := productionLegacyBridgeRecords(ctx, sources, observedAt, probe.ObserveRecord)
	if err != nil {
		return FirstMigrationInspection{}, err
	}
	candidates = append(candidates, recordInventory.candidates...)
	if stateRoot.Path != "" {
		supervisors, err := probe.Supervisors(ctx, sources, stateRoot, observedAt)
		if err != nil {
			return FirstMigrationInspection{}, err
		}
		for index := range supervisors {
			profile := strings.TrimPrefix(supervisors[index].CandidateID, "legacy-supervisor-")
			lanes := recordInventory.supervisorLanes[profile]
			if len(lanes) != 0 && supervisors[index].Classification == LegacyClassificationLiveLegacyAuthority {
				supervisors[index].RelatedLaneIDs = append([]string(nil), lanes...)
				supervisors[index].Classification = LegacyClassificationActiveManagedBlocker
			}
		}
		candidates = append(candidates, supervisors...)
	}

	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return FirstMigrationInspection{}, err
		}
		info, err := os.Lstat(source.Path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !productionLegacyOwned(info) {
				candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "unsafe-source")
				if classifyErr != nil {
					return FirstMigrationInspection{}, classifyErr
				}
				candidates = append(candidates, candidate)
				continue
			}
			hasEstate, estateErr := productionLegacySourceHasEstate(source.Path, info)
			if estateErr != nil {
				return FirstMigrationInspection{}, estateErr
			}
			if hasEstate {
				seenSource = true
				if fallbackSource.Path == "" {
					fallbackSource = source
				}
			}
		} else if !os.IsNotExist(err) {
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "unobservable-source")
			if classifyErr != nil {
				return FirstMigrationInspection{}, classifyErr
			}
			candidates = append(candidates, candidate)
			continue
		}
		if !productionLegacyAgentRuntimeSource(source) || info == nil || !info.IsDir() {
			continue
		}
		endpoint := filepath.Join(source.Path, "agent.sock")
		if _, err := os.Lstat(endpoint); os.IsNotExist(err) {
			continue
		} else if err != nil {
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "unobservable-agent-endpoint")
			if classifyErr != nil {
				return FirstMigrationInspection{}, classifyErr
			}
			candidates = append(candidates, candidate)
			continue
		}
		if agentObserved {
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "multiple-agent-endpoints")
			if classifyErr != nil {
				return FirstMigrationInspection{}, classifyErr
			}
			candidates = append(candidates, candidate)
			continue
		}
		agent, observeErr := probe.ObserveAgent(ctx, endpoint)
		if observeErr != nil {
			stale, proven, classifyErr := productionLegacyStaleAgentSocket(
				ctx, serviceSource, endpoint, observedAt, agent, observeErr, probe.ServiceActivity,
			)
			if classifyErr != nil {
				return FirstMigrationInspection{}, classifyErr
			}
			if proven {
				candidates = append(candidates, stale)
				continue
			}
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "unresponsive-agent-endpoint")
			if classifyErr != nil {
				return FirstMigrationInspection{}, classifyErr
			}
			candidates = append(candidates, candidate)
			continue
		}
		if !pathWithin(agent.Status.StateDir, stateRoot.Path) || filepath.Clean(agent.Status.RuntimeDir) != source.Path {
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "agent-outside-closed-inventory")
			if classifyErr != nil {
				return FirstMigrationInspection{}, classifyErr
			}
			candidates = append(candidates, candidate)
			continue
		}
		loaded := false
		if serviceSource.Path != "" {
			manager, unit := productionLegacyServiceIdentity()
			loaded, err = probe.ServiceLoaded(ctx, manager, unit)
			if err != nil {
				loaded = false
			}
		}
		candidate, err := productionLegacyAgentCandidate(agent, endpoint, serviceSource, observedAt, loaded)
		if err != nil {
			return FirstMigrationInspection{}, err
		}
		candidates = append(candidates, candidate)
		agentObserved = true
		versions = append(versions, agent.Status.RuntimeVersion)
		adoption, err = productionLegacyAdoption(agent.Status, observedAt)
		if err != nil {
			return FirstMigrationInspection{}, err
		}
		prior = LegacyPriorAuthority{
			Candidate: candidate, JournalRevision: candidate.EvidenceRevision,
			ReleaseSelection: agent.Executable, StateSelection: agent.Status.StateDir,
			ConnectorRevision: adoption.SourceRevision,
		}
		if loaded {
			prior.ServiceManager, prior.ServiceUnit = productionLegacyServiceIdentity()
		}
	}

	if !agentObserved && serviceSource.Path != "" {
		if info, err := os.Lstat(serviceSource.Path); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			seenSource = true
			body, readErr := readProductionLegacyServiceDefinition(serviceSource.Path)
			if readErr != nil {
				return FirstMigrationInspection{}, readErr
			}
			manager, unit := productionLegacyServiceIdentity()
			activity, activityErr := probe.ServiceActivity(ctx, manager, unit)
			if activityErr != nil {
				return FirstMigrationInspection{}, activityErr
			}
			classification := LegacyClassificationUnknown
			serviceStatus := activity
			switch activity {
			case "active":
				classification = LegacyClassificationLiveLegacyAuthority
				serviceStatus = "loaded"
			case "inactive", "absent":
				classification = LegacyClassificationStale
			case "unknown":
			default:
				return FirstMigrationInspection{}, fmt.Errorf("unsupported legacy service activity %q", activity)
			}
			candidate := LegacyRuntimeCandidate{
				SchemaVersion: MigrationSchemaVersion, CandidateID: "legacy-stopped-host-agent-service",
				Kind: "host_federation_agent_service", SourcePath: serviceSource.Path,
				SourceRevision: "sha256:" + productionDigest(body), ProcessStatus: "absent", EndpointStatus: "absent",
				ServiceManager: manager, ServiceUnit: unit, ServiceStatus: serviceStatus,
				Classification: classification, EvidenceRevision: 1, LastObservedAt: observedAt,
			}
			if err := candidate.Validate(); err != nil {
				return FirstMigrationInspection{}, err
			}
			candidates = append(candidates, candidate)
		}
	}
	if !seenSource {
		return FirstMigrationInspection{}, nil
	}
	watchers, err := probe.RemoteWatches(candidates, observedAt)
	if err != nil {
		return FirstMigrationInspection{}, err
	}
	candidates = append(candidates, watchers...)
	if !agentObserved {
		if len(candidates) == 0 {
			source := stateRoot
			if source.Path == "" {
				source = fallbackSource
			}
			candidate, err := productionUnknownLegacyCandidate(source, observedAt, "durable-state-without-agent-authority")
			if err != nil {
				return FirstMigrationInspection{}, err
			}
			candidates = append(candidates, candidate)
		}
		if _, quiescenceErr := EvaluateLegacyQuiescence(ctx, LegacyQuiescenceRequest{Candidates: candidates}); quiescenceErr != nil {
			return productionIncompleteInspection(candidates), nil //nolint:nilerr // Blockers and debt are a successful read-only result.
		}
		adoption, err = productionSupervisorOnlyAdoption(candidates, observedAt)
		if err != nil {
			return FirstMigrationInspection{}, err
		}
		dormant, found, err := productionDormantLegacyAgentAdoption(sources, observedAt)
		if err != nil {
			return FirstMigrationInspection{}, err
		}
		if found {
			digest := sha256.Sum256([]byte(adoption.SourceRevision + "\x00" + dormant.SourceRevision))
			dormant.SourceRevision = "sha256:" + hex.EncodeToString(digest[:])
			adoption = dormant
		}
		versions = append(versions, "legacy-stopped-metadata")
	}
	adoption, err = productionLegacyBridgeAdoption(ctx, adoption, sources, observedAt)
	if err != nil {
		return FirstMigrationInspection{}, fmt.Errorf("project production legacy bridge adoption: %w", err)
	}
	if !reflectLegacyPriorAuthorityEmpty(prior) {
		prior.ConnectorRevision = adoption.SourceRevision
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CandidateID < candidates[j].CandidateID })
	if !reflectLegacyPriorAuthorityEmpty(prior) {
		if err := prior.Validate(); err != nil {
			// A manually launched agent has no supported service rollback/stop.
			// Preserve its exact evidence as debt rather than manufacture authority.
			for index := range candidates {
				if candidates[index].CandidateID == prior.Candidate.CandidateID {
					candidates[index].Classification = LegacyClassificationUnknown
				}
			}
			return productionIncompleteInspection(candidates), nil
		}
	}
	staged, err := StageLegacyAdoption(ctx, adoption)
	if err != nil {
		return FirstMigrationInspection{}, fmt.Errorf("validate production legacy adoption: %w", err)
	}
	candidateIDs := make([]string, len(candidates))
	for index := range candidates {
		candidateIDs[index] = candidates[index].CandidateID
	}
	digest := sha256.Sum256([]byte(staged.Revision + "\x00" + strings.Join(candidateIDs, "\x00")))
	return FirstMigrationInspection{
		Required: true, MigrationID: "migration-" + hex.EncodeToString(digest[:12]),
		FromVersions: sortedUniqueStrings(versions), Candidates: candidates, PriorAuthority: prior, Adoption: adoption,
	}, nil
}

func productionLegacyAgentRuntimeSource(source LegacyInventorySource) bool {
	return strings.HasPrefix(source.ID, "federator-") ||
		strings.HasPrefix(source.ID, "recorded-federator-runtime")
}

func productionLegacyStaleAgentSocket(
	ctx context.Context,
	serviceSource LegacyInventorySource,
	endpoint string,
	observedAt int64,
	agent productionLegacyAgentObservation,
	observeErr error,
	serviceActivity func(context.Context, string, string) (string, error),
) (LegacyRuntimeCandidate, bool, error) {
	if !productionLegacyAgentRefusalProvesNoProcess(agent, observeErr) {
		return LegacyRuntimeCandidate{}, false, nil
	}
	identity, safe := productionLegacyOwnedSocketIdentity(endpoint)
	if !safe {
		return LegacyRuntimeCandidate{}, false, nil
	}
	if !productionLegacyAgentServiceProvenInactive(ctx, serviceSource, serviceActivity) {
		return LegacyRuntimeCandidate{}, false, nil
	}
	if !productionLegacySocketIdentityUnchanged(endpoint, identity) {
		return LegacyRuntimeCandidate{}, false, nil
	}
	evidence := LegacyCandidateEvidence{
		CandidateID: "legacy-host-agent-socket-" + productionDigest([]byte(endpoint))[:20],
		Kind:        "host_agent_socket_artifact", SourcePath: endpoint,
		SourceRevision: productionLegacySocketIdentityRevision(identity),
		Process:        LegacyProcessObservation{Status: "absent"},
		Endpoint: LegacyEndpointObservation{
			Status: "absent", Path: endpoint, Type: "unix", OwnerUID: os.Getuid(),
		},
		EvidenceRevision: 1, ObservedAt: observedAt,
	}
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		return LegacyRuntimeCandidate{}, false, err
	}
	return candidate, true, nil
}

func productionLegacyAgentRefusalProvesNoProcess(agent productionLegacyAgentObservation, observeErr error) bool {
	return errors.Is(observeErr, syscall.ECONNREFUSED) && agent.Peer.PID <= 1 &&
		agent.Peer.ProcStart == "" && agent.Peer.StrongStart == ""
}

func productionLegacyOwnedSocketIdentity(path string) (productionLegacyFileIdentity, bool) {
	identity, present, err := observeProductionLegacyPath(path)
	return identity, err == nil && present && identity.ownedSocket()
}

func productionLegacyAgentServiceProvenInactive(
	ctx context.Context,
	serviceSource LegacyInventorySource,
	serviceActivity func(context.Context, string, string) (string, error),
) bool {
	if serviceSource.Path == "" {
		return true
	}
	manager, unit := productionLegacyServiceIdentity()
	activity, err := serviceActivity(ctx, manager, unit)
	return err == nil && (activity == "inactive" || activity == "absent")
}

func productionLegacySocketIdentityUnchanged(path string, expected productionLegacyFileIdentity) bool {
	current, present, err := observeProductionLegacyPath(path)
	return err == nil && present && current.ownedSocket() && current.same(expected)
}

func productionLegacySourceHasEstate(path string, info os.FileInfo) (bool, error) {
	if info.Mode().IsRegular() || info.Mode()&os.ModeSocket != 0 {
		return true, nil
	}
	if !info.IsDir() {
		return false, errors.New("legacy inventory source has an unsupported filesystem type")
	}
	entries, err := readProductionLegacyOwnedDirectory(path, maxProductionLegacyDirectoryEntries)
	if err != nil {
		return false, err
	}
	return len(entries) != 0, nil
}

func productionLegacySupervisors( //nolint:gocyclo // Each bounded supervisor source is fully reconciled in place.
	ctx context.Context,
	sources []LegacyInventorySource,
	stateRoot LegacyInventorySource,
	observedAt int64,
) ([]LegacyRuntimeCandidate, error) {
	profiles := filepath.Join(stateRoot.Path, "..", "..", "claude-code-peer", "profiles")
	// host-agent-state is ${STATE_HOME}/agent-sessions/agents; resolve the
	// independently closed bridge-state source instead of relying on layout
	// arithmetic when callers inject a fixture inventory.
	for _, source := range sources {
		if source.ID == "bridge-state" {
			profiles = filepath.Join(source.Path, "profiles")
			break
		}
	}
	entries, err := readProductionLegacyOwnedDirectory(profiles, 256)
	if os.IsNotExist(err) {
		entries = nil
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	result := make([]LegacyRuntimeCandidate, 0, len(entries))
	seenEndpoints := make(map[string]struct{})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(profiles, entry.Name(), "supervisor.json")
		var state productionLegacySupervisorState
		body, err := readProductionLegacyJSON(path, &state)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			source := LegacyInventorySource{ID: "supervisor-" + entry.Name(), Kind: "state", Path: path, Target: true}
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "malformed-supervisor-state")
			if classifyErr != nil {
				return nil, classifyErr
			}
			result = append(result, candidate)
			continue
		}
		if !migrationAbsoluteCleanPath(state.ControlSocket) || !migrationAbsoluteCleanPath(state.AppServerSocket) ||
			state.PID <= 1 || state.ProcStart == "" || state.RuntimeIdentity == "" ||
			!productionPathWithinLegacyRuntime(state.ControlSocket, sources) {
			source := LegacyInventorySource{ID: "supervisor-" + entry.Name(), Kind: "state", Path: path, Target: true}
			candidate, classifyErr := productionUnknownLegacyCandidate(source, observedAt, "incomplete-supervisor-state")
			if classifyErr != nil {
				return nil, classifyErr
			}
			result = append(result, candidate)
			continue
		}
		seenEndpoints[state.ControlSocket] = struct{}{}
		status, peer, err := observeProductionLegacySupervisor(state.ControlSocket)
		if err != nil {
			identity := procinfo.Read(state.PID)
			processStatus, endpointStatus := "unknown", "unknown"
			if identity.Status == procinfo.Absent {
				if productionLegacyEndpointAbsence(state.ControlSocket, err) == "absent" {
					processStatus, endpointStatus = "absent", "absent"
				}
			}
			evidence := LegacyCandidateEvidence{
				CandidateID: "legacy-supervisor-" + entry.Name(), Kind: "supervisor", SourcePath: path,
				SourceRevision: "sha256:" + productionDigest(body), RuntimeIdentity: state.RuntimeIdentity,
				PID: state.PID, ProcStart: state.ProcStart, StrongStart: identity.StrongStart,
				Process:          LegacyProcessObservation{Status: processStatus},
				Endpoint:         LegacyEndpointObservation{Status: endpointStatus, Path: state.ControlSocket},
				EvidenceRevision: 1, ObservedAt: observedAt,
			}
			candidate, classifyErr := ClassifyLegacyCandidate(evidence)
			if classifyErr != nil {
				return nil, classifyErr
			}
			result = append(result, candidate)
			continue
		}
		executable, err := productionProcessExecutable(peer.PID)
		if err != nil {
			return nil, err
		}
		arguments, err := procinfo.Args(peer.PID)
		if err != nil || len(arguments) != 5 || filepath.Base(executable) != "agent-session-runtime" ||
			arguments[1] != "supervisor" || arguments[2] != "run" || arguments[3] != "--plugin-version" ||
			arguments[4] != state.PluginVersion {
			return nil, errors.New("legacy supervisor argv does not match its supported role")
		}
		identity, err := hostExecutableIdentity(executable)
		if err != nil {
			return nil, err
		}
		if status.PID != state.PID || peer.PID != state.PID || status.ProcStart != state.ProcStart ||
			peer.ProcStart != state.ProcStart || status.RuntimeIdentity != state.RuntimeIdentity || identity != state.RuntimeIdentity ||
			status.ControlSocket != state.ControlSocket || status.PluginVersion != state.PluginVersion ||
			!status.Ready {
			return nil, errors.New("legacy supervisor durable, kernel, executable, and endpoint identities conflict")
		}
		evidence := LegacyCandidateEvidence{
			CandidateID: "legacy-supervisor-" + entry.Name(), Kind: "supervisor", SourcePath: path,
			SourceRevision: "sha256:" + productionDigest(body), ReportedVersion: status.PluginVersion,
			RuntimeIdentity: identity, PID: peer.PID, ProcStart: peer.ProcStart, StrongStart: peer.StrongStart,
			Process: LegacyProcessObservation{
				Status: "known", PID: peer.PID, ProcStart: peer.ProcStart, StrongStart: peer.StrongStart,
				Executable: executable, ArgvRole: "supervisor",
			},
			Endpoint: LegacyEndpointObservation{
				Status: "responsive", Path: state.ControlSocket, Type: "unix", OwnerUID: peer.UID,
				OwnerPID: peer.PID, OwnerProcStart: peer.ProcStart, RuntimeIdentity: identity,
			},
			ReportedActiveCount: status.ShimCount, EvidenceRevision: 1, ObservedAt: observedAt,
		}
		candidate, err := ClassifyLegacyCandidate(evidence)
		if err != nil {
			return nil, err
		}
		// The pre-unification supervisor has no admission-drain protocol that is
		// understood by every deployed launcher. Even with zero reported shims it
		// remains a live authority capable of accepting a peer after inspection,
		// so installation requires the operator to stop it first.
		candidate.Classification = LegacyClassificationActiveManagedBlocker
		candidate.ProcessArguments = append([]string(nil), arguments[1:]...)
		result = append(result, candidate)
	}
	orphans, err := productionLegacyOrphanSupervisors(ctx, sources, observedAt, seenEndpoints,
		productionLegacySupervisorEndpointProbe{
			Observe: observeProductionLegacySupervisor, Executable: productionProcessExecutable,
			Arguments: procinfo.Args, RuntimeIdentity: hostExecutableIdentity,
		})
	if err != nil {
		return nil, err
	}
	return append(result, orphans...), nil
}

func productionLegacyOrphanSupervisors( //nolint:gocyclo // Every bounded endpoint attribute fails closed independently.
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
	seenEndpoints map[string]struct{},
	probe productionLegacySupervisorEndpointProbe,
) ([]LegacyRuntimeCandidate, error) {
	if probe.Observe == nil || probe.Executable == nil || probe.Arguments == nil || probe.RuntimeIdentity == nil {
		return nil, errors.New("legacy supervisor endpoint inventory requires exact attribution probes")
	}
	if !productionLegacyDarwinInventory(sources) {
		return nil, nil
	}
	if seenEndpoints == nil {
		seenEndpoints = make(map[string]struct{})
	}
	result := make([]LegacyRuntimeCandidate, 0)
	for _, source := range sources {
		if source.Kind != "runtime" && source.Kind != "recorded_runtime" ||
			!legacyRecordedBridgeRuntime(source.Path, strconv.Itoa(os.Getuid())) {
			continue
		}
		entries, err := readProductionLegacyOwnedDirectory(source.Path, 256)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			profileKey, canonical := productionLegacySupervisorEndpointProfile(entry.Name())
			if !canonical {
				continue
			}
			endpoint := filepath.Join(source.Path, entry.Name())
			if _, duplicate := seenEndpoints[endpoint]; duplicate {
				continue
			}
			seenEndpoints[endpoint] = struct{}{}
			candidate, err := productionLegacyOrphanSupervisor(endpoint, profileKey, observedAt, probe)
			if err != nil {
				return nil, err
			}
			result = append(result, candidate)
		}
	}
	return result, nil
}

func productionLegacyDarwinInventory(sources []LegacyInventorySource) bool {
	for _, source := range sources {
		if source.ID == "launchd-host-agent" || source.ID == "bridge-darwin-runtime" ||
			source.ID == "bridge-darwin-compact-runtime" || source.Kind == "recorded_runtime" {
			return true
		}
	}
	return false
}

func productionLegacySupervisorEndpointProfile(name string) (string, bool) {
	if name == "supervisor.sock" {
		return "", true
	}
	if !strings.HasPrefix(name, "supervisor-") || !strings.HasSuffix(name, ".sock") {
		return "", false
	}
	profileKey := strings.TrimSuffix(strings.TrimPrefix(name, "supervisor-"), ".sock")
	return profileKey, durableRecordID.MatchString(profileKey)
}

func productionLegacyOrphanSupervisor( //nolint:gocyclo // Socket, status, kernel, argv, and runtime identity each fail closed.
	endpoint string,
	profileKey string,
	observedAt int64,
	probe productionLegacySupervisorEndpointProbe,
) (LegacyRuntimeCandidate, error) {
	candidateID := "legacy-supervisor-endpoint-" + productionDigest([]byte(endpoint))[:20]
	classify := func(
		revision string,
		process LegacyProcessObservation,
		endpointObservation LegacyEndpointObservation,
	) (LegacyRuntimeCandidate, error) {
		evidence := LegacyCandidateEvidence{
			CandidateID: candidateID, Kind: "supervisor_socket_artifact", SourcePath: endpoint,
			SourceRevision: revision, PID: process.PID, ProcStart: process.ProcStart, StrongStart: process.StrongStart,
			Process: process, Endpoint: endpointObservation,
			EvidenceRevision: 1, ObservedAt: observedAt,
		}
		return ClassifyLegacyCandidate(evidence)
	}
	unknown := func(revision string) (LegacyRuntimeCandidate, error) {
		return classify(revision, LegacyProcessObservation{Status: "unknown"},
			LegacyEndpointObservation{Status: "unknown", Path: endpoint})
	}
	identity, present, err := observeProductionLegacyPath(endpoint)
	if err != nil || !present || !identity.ownedSocket() {
		return unknown("unattributed-canonical-supervisor-endpoint")
	}
	sourceRevision := productionLegacySocketIdentityRevision(identity)
	status, peer, err := probe.Observe(endpoint)
	if err != nil {
		if peer.PID > 1 {
			return classify(sourceRevision,
				LegacyProcessObservation{Status: "known", PID: peer.PID, ProcStart: peer.ProcStart, StrongStart: peer.StrongStart},
				LegacyEndpointObservation{Status: "unknown", Path: endpoint, Type: "unix", OwnerUID: peer.UID,
					OwnerPID: peer.PID, OwnerProcStart: peer.ProcStart})
		}
		if !productionLegacyDialFailureProvesNoListener(err) {
			return unknown("unobservable-canonical-supervisor-endpoint")
		}
		current, stillPresent, observeErr := observeProductionLegacyPath(endpoint)
		if observeErr == nil && stillPresent && current.ownedSocket() && current.same(identity) {
			return classify(sourceRevision, LegacyProcessObservation{Status: "absent"},
				LegacyEndpointObservation{Status: "absent", Path: endpoint, Type: "unix", OwnerUID: os.Getuid()})
		}
		return unknown("changed-canonical-supervisor-endpoint")
	}
	executable, err := probe.Executable(peer.PID)
	if err != nil {
		return unknown("unobservable-supervisor-executable")
	}
	arguments, err := probe.Arguments(peer.PID)
	if err != nil || len(arguments) != 5 || filepath.Base(executable) != "agent-session-runtime" ||
		arguments[1] != "supervisor" || arguments[2] != "run" || arguments[3] != "--plugin-version" ||
		arguments[4] != status.PluginVersion {
		return unknown("unattributed-supervisor-argv")
	}
	runtimeIdentity, err := probe.RuntimeIdentity(executable)
	if err != nil || runtimeIdentity == "" || status.PID <= 1 || status.PID != peer.PID ||
		status.ProcStart == "" || status.ProcStart != peer.ProcStart || status.RuntimeIdentity != runtimeIdentity ||
		status.ControlSocket != endpoint || status.ProfileKey != profileKey || status.PluginVersion == "" ||
		peer.UID != os.Getuid() || !status.Ready || status.ShimCount < 0 {
		return unknown("conflicting-supervisor-runtime-identity")
	}
	current, stillPresent, observeErr := observeProductionLegacyPath(endpoint)
	if observeErr != nil || !stillPresent || !current.ownedSocket() || !current.same(identity) {
		return unknown("changed-canonical-supervisor-endpoint")
	}
	evidence := LegacyCandidateEvidence{
		CandidateID: candidateID, Kind: "supervisor_socket_artifact", SourcePath: endpoint,
		SourceRevision: sourceRevision, ReportedVersion: status.PluginVersion,
		RuntimeIdentity: runtimeIdentity, PID: peer.PID, ProcStart: peer.ProcStart, StrongStart: peer.StrongStart,
		Process: LegacyProcessObservation{
			Status: "known", PID: peer.PID, ProcStart: peer.ProcStart, StrongStart: peer.StrongStart,
			Executable: executable, ArgvRole: "supervisor",
		},
		Endpoint: LegacyEndpointObservation{
			Status: "responsive", Path: endpoint, Type: "unix", OwnerUID: peer.UID,
			OwnerPID: peer.PID, OwnerProcStart: peer.ProcStart, RuntimeIdentity: runtimeIdentity,
		},
		ReportedActiveCount: status.ShimCount, EvidenceRevision: 1, ObservedAt: observedAt,
	}
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	candidate.Classification = LegacyClassificationActiveManagedBlocker
	candidate.ProcessArguments = append([]string(nil), arguments[1:]...)
	return candidate, nil
}

func productionLegacySocketIdentityRevision(identity productionLegacyFileIdentity) string {
	return productionLegacyArtifactIdentityRevision(identity)
}

func productionLegacyArtifactIdentityRevision(identity productionLegacyFileIdentity) string {
	body := fmt.Sprintf("%d:%d:%d:%d", identity.device, identity.inode, identity.mode, identity.uid)
	return "sha256:" + productionDigest([]byte(body))
}

func observeProductionLegacySupervisor(
	endpoint string,
) (productionLegacySupervisorStatus, controlPeerEvidence, error) {
	connection, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		return productionLegacySupervisorStatus{}, controlPeerEvidence{}, err
	}
	defer func() { _ = connection.Close() }()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return productionLegacySupervisorStatus{}, controlPeerEvidence{}, errors.New("legacy supervisor endpoint is not Unix")
	}
	peer, err := observeControlPeer(unixConnection)
	if err != nil || peer.UID != os.Getuid() {
		return productionLegacySupervisorStatus{}, controlPeerEvidence{}, errors.New("legacy supervisor lacks current-user kernel identity")
	}
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(connection, "{\"action\":\"status\"}\n"); err != nil {
		return productionLegacySupervisorStatus{}, peer, err
	}
	line, err := bufio.NewReaderSize(connection, maxProductionLegacyRecordBytes).ReadBytes('\n')
	if err != nil || len(line) > maxProductionLegacyRecordBytes {
		return productionLegacySupervisorStatus{}, peer, errors.New("legacy supervisor status is absent or unbounded")
	}
	var status productionLegacySupervisorStatus
	if err := decodeProductionLegacyJSON(line, &status); err != nil {
		return productionLegacySupervisorStatus{}, peer, err
	}
	return status, peer, nil
}

func productionPathWithinLegacyRuntime(path string, sources []LegacyInventorySource) bool {
	for _, source := range sources {
		if (source.Kind == "runtime" || source.Kind == "recorded_runtime") && pathWithin(path, source.Path) {
			return true
		}
	}
	return false
}

func productionDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func productionLegacySpecialSources(sources []LegacyInventorySource) (LegacyInventorySource, LegacyInventorySource) {
	var stateRoot, service LegacyInventorySource
	for _, source := range sources {
		switch source.ID {
		case "host-agent-state":
			stateRoot = source
		case "systemd-host-agent", "launchd-host-agent":
			service = source
		}
	}
	return stateRoot, service
}

func productionRecordedRuntimeRoots(stateHome string, uid int) ([]string, error) {
	profiles := filepath.Join(stateHome, "claude-code-peer", "profiles")
	entries, err := readProductionLegacyDirectory(profiles)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, errors.New("durable legacy supervisor runtime-root inventory exceeds its bound")
	}
	uidText := strconv.Itoa(uid)
	seen := make(map[string]struct{})
	var roots []string
	for _, entry := range entries {
		root, present, err := productionRecordedSupervisorRuntimeRoot(profiles, entry, uidText)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if _, duplicate := seen[root]; duplicate {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
		federatorRoot := filepath.Join(filepath.Dir(root), "peer-federator-"+uidText)
		if _, duplicate := seen[federatorRoot]; !duplicate {
			seen[federatorRoot] = struct{}{}
			roots = append(roots, federatorRoot)
		}
	}
	sort.Strings(roots)
	return roots, nil
}

func productionRecordedSupervisorRuntimeRoot(
	profiles string,
	entry os.DirEntry,
	uid string,
) (string, bool, error) {
	if entry.Type()&os.ModeSymlink != 0 {
		return "", false, errors.New("legacy supervisor profile inventory contains a symlink")
	}
	if !entry.IsDir() {
		return "", false, nil
	}
	profileRoot := filepath.Join(profiles, entry.Name())
	profileInfo, err := os.Lstat(profileRoot)
	if err != nil || profileInfo.Mode()&os.ModeSymlink != 0 || !profileInfo.IsDir() || !productionLegacyOwned(profileInfo) {
		return "", false, errors.New("legacy supervisor profile is not an owned real directory")
	}
	statePath := filepath.Join(profileRoot, "supervisor.json")
	var state productionLegacySupervisorState
	if _, err := readProductionLegacyJSON(statePath, &state); os.IsNotExist(err) {
		return "", false, nil
	} else if err != nil {
		return "", false, fmt.Errorf("read durable legacy supervisor runtime root: %w", err)
	}
	root := filepath.Dir(state.ControlSocket)
	controlName := filepath.Base(state.ControlSocket)
	if !migrationAbsoluteCleanPath(state.ControlSocket) ||
		(controlName != "supervisor.sock" && controlName != "supervisor-"+entry.Name()+".sock") ||
		!legacyRecordedBridgeRuntime(root, uid) {
		return "", false, errors.New("durable legacy supervisor control socket is outside a shipped Darwin runtime root")
	}
	return root, true, nil
}

func observeProductionLegacyAgent(ctx context.Context, endpoint string) (productionLegacyAgentObservation, error) { //nolint:gocyclo // Exact wire, kernel, argv, and state evidence are jointly checked.
	if err := ctx.Err(); err != nil {
		return productionLegacyAgentObservation{}, err
	}
	connection, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		return productionLegacyAgentObservation{}, err
	}
	defer func() { _ = connection.Close() }()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return productionLegacyAgentObservation{}, errors.New("legacy agent endpoint is not Unix")
	}
	peer, err := observeControlPeer(unixConnection)
	if err != nil || peer.UID != os.Getuid() {
		return productionLegacyAgentObservation{}, errors.New("legacy agent endpoint lacks current-user kernel identity")
	}
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(connection, "{\"type\":\"status\"}\n"); err != nil {
		return productionLegacyAgentObservation{}, err
	}
	reader := bufio.NewReaderSize(connection, maxProductionLegacyRecordBytes)
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > maxProductionLegacyRecordBytes {
		return productionLegacyAgentObservation{}, errors.New("legacy agent status response is absent or unbounded")
	}
	var response struct {
		Type  string          `json:"type"`
		Frame json.RawMessage `json:"frame"`
		Error string          `json:"error,omitempty"`
	}
	if err := decodeProductionLegacyJSON(line, &response); err != nil || response.Type != "status" || len(response.Frame) == 0 {
		return productionLegacyAgentObservation{}, errors.New("legacy agent returned an incompatible status envelope")
	}
	var status productionLegacyAgentStatus
	if err := decodeProductionLegacyJSON(response.Frame, &status); err != nil {
		return productionLegacyAgentObservation{}, err
	}
	if !durableRecordID.MatchString(status.HostID) || strings.TrimSpace(status.HostName) == "" ||
		status.ProtocolVersion != federation.ProtocolVersion || status.LocalPeers < 0 ||
		!migrationAbsoluteCleanPath(status.StateDir) || !migrationAbsoluteCleanPath(status.RuntimeDir) {
		return productionLegacyAgentObservation{}, errors.New("legacy agent status has incomplete exact identity")
	}
	executable, err := productionProcessExecutable(peer.PID)
	if err != nil {
		return productionLegacyAgentObservation{}, err
	}
	arguments, err := procinfo.Args(peer.PID)
	if err != nil || len(arguments) < 2 || filepath.Base(executable) != "peer-federator" || arguments[1] != "agent" {
		return productionLegacyAgentObservation{}, errors.New("legacy agent argv does not match its supported role")
	}
	identity, err := hostExecutableIdentity(executable)
	if err != nil {
		return productionLegacyAgentObservation{}, err
	}
	return productionLegacyAgentObservation{Status: status, Peer: peer, Executable: executable, Identity: identity}, nil
}

func productionLegacyAgentCandidate(
	agent productionLegacyAgentObservation,
	endpoint string,
	serviceSource LegacyInventorySource,
	observedAt int64,
	serviceLoaded bool,
) (LegacyRuntimeCandidate, error) {
	sourcePath := filepath.Join(agent.Status.StateDir, "sessions.json")
	sourceRevision := agent.Status.HostID + "-catalog"
	service := LegacyServiceObservation{Status: "unknown"}
	if serviceLoaded {
		manager, unit := productionLegacyServiceIdentity()
		body, err := readProductionLegacyServiceDefinition(serviceSource.Path)
		if err != nil {
			return LegacyRuntimeCandidate{}, err
		}
		sourcePath = serviceSource.Path
		sourceRevision = "sha256:" + productionDigest(body)
		service = LegacyServiceObservation{
			Status: "loaded", Manager: manager, Unit: unit, Executable: agent.Executable, ArgvRole: "agent",
		}
	}
	evidence := LegacyCandidateEvidence{
		CandidateID: "legacy-host-agent-" + agent.Status.HostID, Kind: "host_federation_agent",
		SourcePath: sourcePath, SourceRevision: sourceRevision, ReportedVersion: agent.Status.RuntimeVersion,
		RuntimeIdentity: agent.Identity, PID: agent.Peer.PID, ProcStart: agent.Peer.ProcStart,
		StrongStart: agent.Peer.StrongStart,
		Process: LegacyProcessObservation{
			Status: "known", PID: agent.Peer.PID, ProcStart: agent.Peer.ProcStart,
			StrongStart: agent.Peer.StrongStart, Executable: agent.Executable, ArgvRole: "host_federation_agent",
		},
		Endpoint: LegacyEndpointObservation{
			Status: "responsive", Path: endpoint, Type: "unix", OwnerUID: agent.Peer.UID,
			OwnerPID: agent.Peer.PID, OwnerProcStart: agent.Peer.ProcStart, RuntimeIdentity: agent.Identity,
		},
		Service: service, ReportedActiveCount: agent.Status.LocalPeers,
		EvidenceRevision: 1, ObservedAt: observedAt,
	}
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	// A responsive legacy federation agent is itself live admission authority;
	// scalar LocalPeers is diagnostic metadata, not a safe quiescence fence.
	candidate.Classification = LegacyClassificationActiveManagedBlocker
	return candidate, nil
}

func readProductionLegacyServiceDefinition(path string) ([]byte, error) {
	body, _, err := readProductionLegacyRegular(path, 1, maxProductionLegacyRecordBytes)
	return body, err
}

func productionLegacyAdoption(status productionLegacyAgentStatus, observedAt int64) (LegacyAdoptionRequest, error) {
	catalogPath := filepath.Join(status.StateDir, "sessions.json")
	var catalog productionLegacyCatalog
	body, err := readProductionLegacyJSON(catalogPath, &catalog)
	if err != nil {
		return LegacyAdoptionRequest{}, err
	}
	if catalog.Version != 2 || catalog.Sessions == nil {
		return LegacyAdoptionRequest{}, errors.New("legacy session catalog has an incompatible version")
	}
	hash := sha256.New()
	_, _ = hash.Write(body)
	sessions := make([]LegacySessionRecord, 0, len(catalog.Sessions))
	products := make([]string, 0, len(catalog.Sessions))
	for key, preference := range catalog.Sessions {
		if key != preference.SessionID || !durableRecordID.MatchString(key) || preference.UpdatedAt <= 0 {
			return LegacyAdoptionRequest{}, fmt.Errorf("legacy session catalog row %q is incompatible", key)
		}
		kind := preference.Kind
		if kind == "" {
			kind = federation.SessionKindInteractive
		}
		permission := "default"
		if preference.AlwaysApprove {
			permission = "bypassPermissions"
		}
		sessions = append(sessions, LegacySessionRecord{
			SessionID: preference.SessionID, Product: preference.Product, Kind: kind,
			ExplicitGroups:  append([]string(nil), preference.ExplicitGroups...),
			InheritedGroups: append([]string(nil), preference.InheritedGroups...),
			ParentSessionID: preference.ParentSession, ParentHostID: preference.ParentHostID,
			InheritParentGroups: preference.InheritParentGroups, PermissionMode: permission,
			UpdatedAt: preference.UpdatedAt,
		})
		products = append(products, preference.Product)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Product+sessions[i].SessionID < sessions[j].Product+sessions[j].SessionID
	})
	names, nameBodies, err := productionLegacyNames(status.StateDir, catalog.Sessions)
	if err != nil {
		return LegacyAdoptionRequest{}, err
	}
	for _, body := range nameBodies {
		_, _ = hash.Write(body)
	}
	revision := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	state := "disabled"
	if status.Hub != "" {
		state = "reconnecting"
	}
	if status.Connected {
		state = "connected"
	}
	header := RecordHeader{
		SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: 1,
		CreatedAt: observedAt, UpdatedAt: observedAt,
	}
	return LegacyAdoptionRequest{
		SourceRevision: revision, HostID: status.HostID, Sessions: sessions, Names: names,
		Hub: FederationStateRecord{
			RecordHeader: header, HostID: status.HostID, HostName: status.HostName, HubAddress: status.Hub,
			ConnectionGeneration: 1, ProtocolVersion: federation.ProtocolVersion,
			AdvertisedCapabilities: sortedUniqueStrings(status.Capabilities), State: state,
			AdvertisedRuntimeVersion: status.RuntimeVersion,
			AdvertisedProducts:       sortedUniqueStrings(products),
		},
	}, nil
}

func productionLegacyNames(
	stateDir string,
	sessions map[string]federation.SessionPreferences,
) ([]LegacySessionName, [][]byte, error) {
	directory := filepath.Join(stateDir, "session-names")
	entries, err := readProductionLegacyOwnedDirectory(directory, 4096)
	if os.IsNotExist(err) {
		return []LegacySessionName{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	names := make([]LegacySessionName, 0, len(entries))
	bodies := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var record productionLegacyName
		body, err := readProductionLegacyJSON(filepath.Join(directory, entry.Name()), &record)
		if err != nil {
			return nil, nil, err
		}
		preference, ok := sessions[record.SessionID]
		if record.Version != 1 || !ok || preference.Product != record.Product || record.Name == "" || record.UpdatedAt <= 0 {
			return nil, nil, fmt.Errorf("legacy session name %q is incompatible", entry.Name())
		}
		kind := record.Kind
		if kind == "" {
			kind = preference.Kind
		}
		if kind == "" {
			kind = federation.SessionKindInteractive
		}
		names = append(names, LegacySessionName{
			SessionID: record.SessionID, Product: record.Product, Kind: kind,
			Name: record.Name, NameSource: "legacy_catalog", UpdatedAt: record.UpdatedAt,
		})
		bodies = append(bodies, body)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Product+names[i].SessionID < names[j].Product+names[j].SessionID })
	return names, bodies, nil
}

func productionIncompleteInspection(candidates []LegacyRuntimeCandidate) FirstMigrationInspection {
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CandidateID < candidates[j].CandidateID })
	hash := sha256.New()
	for _, candidate := range candidates {
		_, _ = io.WriteString(hash, candidate.CandidateID+"\x00"+candidate.SourceRevision+"\x00")
	}
	return FirstMigrationInspection{
		Required: true, MigrationID: "migration-" + hex.EncodeToString(hash.Sum(nil)[:12]), Candidates: candidates,
	}
}

func productionSupervisorOnlyAdoption(
	candidates []LegacyRuntimeCandidate,
	observedAt int64,
) (LegacyAdoptionRequest, error) {
	hostName, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostName) == "" {
		return LegacyAdoptionRequest{}, errors.New("cannot establish the supervisor-only legacy host identity")
	}
	hostDigest := sha256.Sum256([]byte(hostName + "\x00" + strconv.Itoa(os.Getuid())))
	hostID := hex.EncodeToString(hostDigest[:16])
	hash := sha256.New()
	ordered := append([]LegacyRuntimeCandidate(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CandidateID < ordered[j].CandidateID })
	for _, candidate := range ordered {
		_, _ = io.WriteString(hash, candidate.CandidateID+"\x00"+candidate.SourceRevision+"\x00")
	}
	header := RecordHeader{
		SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: 1,
		CreatedAt: observedAt, UpdatedAt: observedAt,
	}
	return LegacyAdoptionRequest{
		SourceRevision: "sha256:" + hex.EncodeToString(hash.Sum(nil)), HostID: hostID,
		Hub: FederationStateRecord{
			RecordHeader: header, HostID: hostID, HostName: hostName, ConnectionGeneration: 1,
			ProtocolVersion: federation.ProtocolVersion, State: "disabled",
		},
	}, nil
}

func productionDormantLegacyAgentAdoption(
	sources []LegacyInventorySource,
	observedAt int64,
) (LegacyAdoptionRequest, bool, error) {
	agentsRoot := productionLegacySourcePath(sources, "host-agent-state")
	if agentsRoot == "" {
		return LegacyAdoptionRequest{}, false, nil
	}
	entries, err := readProductionLegacyDirectory(agentsRoot)
	if os.IsNotExist(err) {
		return LegacyAdoptionRequest{}, false, nil
	}
	if err != nil {
		return LegacyAdoptionRequest{}, false, err
	}
	hostName, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostName) == "" {
		return LegacyAdoptionRequest{}, false, errors.New("cannot establish dormant legacy host name")
	}
	var result LegacyAdoptionRequest
	found := false
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return LegacyAdoptionRequest{}, false, errors.New("dormant legacy host catalog inventory contains a symlink")
		}
		if !entry.IsDir() {
			continue
		}
		stateDir := filepath.Join(agentsRoot, entry.Name())
		if _, err := os.Lstat(filepath.Join(stateDir, "sessions.json")); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return LegacyAdoptionRequest{}, false, err
		}
		if found {
			return LegacyAdoptionRequest{}, false, errors.New("multiple dormant legacy host catalogs require exact operator selection")
		}
		if !durableRecordID.MatchString(entry.Name()) {
			return LegacyAdoptionRequest{}, false, errors.New("dormant legacy host catalog has an invalid host identity")
		}
		result, err = productionLegacyAdoption(productionLegacyAgentStatus{
			ProtocolVersion: federation.ProtocolVersion,
			HostID:          entry.Name(),
			HostName:        hostName,
			StateDir:        stateDir,
		}, observedAt)
		if err != nil {
			return LegacyAdoptionRequest{}, false, fmt.Errorf("adopt dormant legacy host catalog: %w", err)
		}
		found = true
	}
	return result, found, nil
}

func productionUnknownLegacyCandidate(
	source LegacyInventorySource,
	observedAt int64,
	revision string,
) (LegacyRuntimeCandidate, error) {
	evidence := LegacyCandidateEvidence{
		CandidateID: "source-" + source.ID, Kind: productionLegacyCandidateKind(source), SourcePath: source.Path,
		SourceRevision: revision, Process: LegacyProcessObservation{Status: "unknown"},
		Endpoint: LegacyEndpointObservation{Status: "unknown"}, Service: LegacyServiceObservation{Status: "unknown"},
		EvidenceRevision: 1, ObservedAt: observedAt,
	}
	return ClassifyLegacyCandidate(evidence)
}

func readProductionLegacyJSON(path string, value any) ([]byte, error) {
	body, _, err := readProductionLegacyRegular(path, 1, maxProductionLegacyRecordBytes)
	if err != nil {
		return nil, err
	}
	if err := decodeProductionLegacyJSON(body, value); err != nil {
		return nil, err
	}
	return body, nil
}

func decodeProductionLegacyJSON(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("legacy JSON has trailing content")
	}
	return nil
}

func productionProcessExecutable(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err == nil && filepath.IsAbs(path) {
			return filepath.Clean(strings.TrimSuffix(path, " (deleted)")), nil
		}
	}
	arguments, err := procinfo.Args(pid)
	if err != nil || len(arguments) == 0 || !filepath.IsAbs(arguments[0]) {
		return "", errors.New("legacy process executable is not exactly observable")
	}
	return filepath.Clean(arguments[0]), nil
}

func productionLegacyServiceIdentity() (string, string) {
	if runtime.GOOS == "darwin" {
		return "launchd-user", "net.antst.peer-federator.agent"
	}
	return "systemd-user", "peer-federator-agent.service"
}

func productionLegacyOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func sortedUniqueStrings(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(result) > 0 && result[len(result)-1] == value {
			continue
		}
		result = append(result, value)
	}
	return result
}
