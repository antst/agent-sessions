package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

// productionLegacyRetirementLifecycle implements the T083 mutation boundary
// only with exact journal identities, supported local control frames, and
// exact user-service operations. It never scans processes or signals by name.
type productionLegacyRetirementLifecycle struct {
	paths                ProductionPaths
	state                *StateStore
	retirementMu         sync.Mutex
	retirementArtifacts  map[string]productionLegacyArtifactAttestation
	beforeArtifactUnlink func(string)
}

type productionLegacyObservedFile struct {
	identity productionLegacyFileIdentity
	present  bool
}

type productionLegacyArtifactAttestation struct {
	endpoint productionLegacyObservedFile
	source   productionLegacyObservedFile
	lock     productionLegacyObservedFile
}

func newProductionLegacyRetirementLifecycle(paths ProductionPaths, state *StateStore) (*productionLegacyRetirementLifecycle, error) {
	for _, path := range []string{paths.StateRoot, paths.RuntimeRoot, paths.ControlEndpoint} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errors.New("production legacy lifecycle requires canonical host paths")
		}
	}
	if state == nil {
		return nil, errors.New("production legacy lifecycle requires host state")
	}
	return &productionLegacyRetirementLifecycle{
		paths: paths, state: state,
		retirementArtifacts: make(map[string]productionLegacyArtifactAttestation),
	}, nil
}

func (lifecycle *productionLegacyRetirementLifecycle) attestRetirementArtifacts(
	candidate LegacyRuntimeCandidate,
) (productionLegacyArtifactAttestation, error) {
	var attestation productionLegacyArtifactAttestation
	if candidate.EndpointPath != "" {
		identity, present, err := observeProductionLegacyPath(candidate.EndpointPath)
		if err != nil {
			return productionLegacyArtifactAttestation{}, err
		}
		if present && !identity.ownedSocket() {
			return productionLegacyArtifactAttestation{}, errors.New("legacy endpoint changed filesystem type")
		}
		attestation.endpoint = productionLegacyObservedFile{identity: identity, present: present}
	}
	if candidate.Classification != LegacyClassificationExcluded && !productionLegacySocketArtifactKind(candidate.Kind) {
		identity, present, err := observeProductionLegacyRegular(candidate.SourcePath)
		if err != nil {
			return productionLegacyArtifactAttestation{}, err
		}
		attestation.source = productionLegacyObservedFile{identity: identity, present: present}
	}
	if candidate.Kind == "supervisor" {
		lockPath := filepath.Join(filepath.Dir(candidate.SourcePath), "supervisor-start.lock")
		identity, present, err := observeProductionLegacyRegular(lockPath)
		if err != nil {
			return productionLegacyArtifactAttestation{}, err
		}
		attestation.lock = productionLegacyObservedFile{identity: identity, present: present}
	}
	lifecycle.retirementMu.Lock()
	if lifecycle.retirementArtifacts == nil {
		lifecycle.retirementArtifacts = make(map[string]productionLegacyArtifactAttestation)
	}
	lifecycle.retirementArtifacts[candidate.CandidateID] = attestation
	lifecycle.retirementMu.Unlock()
	return attestation, nil
}

func (lifecycle *productionLegacyRetirementLifecycle) takeRetirementAttestation(
	candidate LegacyRuntimeCandidate,
) (productionLegacyArtifactAttestation, bool) {
	lifecycle.retirementMu.Lock()
	attestation, ok := lifecycle.retirementArtifacts[candidate.CandidateID]
	delete(lifecycle.retirementArtifacts, candidate.CandidateID)
	lifecycle.retirementMu.Unlock()
	return attestation, ok
}

func (attestation productionLegacyArtifactAttestation) same(other productionLegacyArtifactAttestation) bool {
	return attestation.endpoint.same(other.endpoint) && attestation.source.same(other.source) &&
		attestation.lock.same(other.lock)
}

func (observed productionLegacyObservedFile) same(other productionLegacyObservedFile) bool {
	return observed.present == other.present && (!observed.present || observed.identity.same(other.identity))
}

func (lifecycle *productionLegacyRetirementLifecycle) runBeforeArtifactUnlink(path string) {
	if lifecycle.beforeArtifactUnlink != nil {
		lifecycle.beforeArtifactUnlink(path)
	}
}

// ReattestProcess corroborates one exact legacy process or service definition.
func (lifecycle *productionLegacyRetirementLifecycle) ReattestProcess(
	ctx context.Context,
	candidate LegacyRuntimeCandidate,
) (LegacyRuntimeCandidate, error) {
	if err := ctx.Err(); err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	observed := candidate
	if candidate.ServiceManager != "" {
		loaded, err := productionLegacyServiceLoaded(ctx, candidate.ServiceManager, candidate.ServiceUnit)
		if err != nil {
			return LegacyRuntimeCandidate{}, err
		}
		if loaded {
			observed.ServiceStatus = "loaded"
		} else {
			observed.ServiceStatus = "absent"
		}
		if candidate.PID <= 1 {
			return observed, nil
		}
	}
	identity := procinfo.Read(candidate.PID)
	switch identity.Status {
	case procinfo.Absent:
		observed.ProcessStatus = "absent"
		return observed, nil
	case procinfo.Known:
		observed.ProcessStatus = "known"
		observed.ProcStart, observed.StrongStart = identity.Start, identity.StrongStart
	case procinfo.Unknown:
		observed.ProcessStatus = "unknown"
		return observed, nil
	}
	arguments, err := procinfo.Args(candidate.PID)
	if err != nil || len(arguments) == 0 {
		return LegacyRuntimeCandidate{}, fmt.Errorf("observe exact legacy argv: %w", err)
	}
	executable := arguments[0]
	if !filepath.IsAbs(executable) {
		return LegacyRuntimeCandidate{}, errors.New("legacy process executable is not absolute")
	}
	observed.ProcessExecutable = filepath.Clean(executable)
	if len(candidate.ProcessArguments) != 0 {
		observed.ProcessArguments = append([]string(nil), arguments[1:]...)
		observed.ProcessEnvironment = maps.Clone(candidate.ProcessEnvironment)
	}
	return observed, nil
}

// ReattestEndpoint corroborates one exact service artifact or Unix endpoint.
//
//nolint:gocyclo // Endpoint, service, and descriptor identities each fail closed at this single reattestation boundary.
func (lifecycle *productionLegacyRetirementLifecycle) ReattestEndpoint(
	ctx context.Context,
	candidate LegacyRuntimeCandidate,
) (LegacyRuntimeCandidate, error) {
	if err := ctx.Err(); err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	if candidate.EndpointPath == "" {
		observed := candidate
		if candidate.ServiceManager == "" {
			observed.ProcessStatus = "absent"
			observed.EndpointStatus = "absent"
			observed.ArtifactRevision = ""
			observed.ArtifactIdentity = ""
			attestation, err := lifecycle.attestRetirementArtifacts(candidate)
			if err != nil {
				return LegacyRuntimeCandidate{}, err
			}
			if attestation.source.present {
				record, err := openProductionLegacyAttestedRegular(candidate.SourcePath, attestation.source, false)
				if err != nil {
					return LegacyRuntimeCandidate{}, err
				}
				defer record.close()
				body, err := record.readBounded(1, maxProductionLegacyRecordBytes)
				if err != nil {
					return LegacyRuntimeCandidate{}, err
				}
				observed.ArtifactRevision = "sha256:" + productionDigest(body)
				observed.ArtifactIdentity = productionLegacyArtifactIdentityRevision(attestation.source.identity)
			}
			return observed, nil
		}
		status, err := productionLegacyServiceArtifactStatus(ctx, candidate)
		if err != nil {
			return LegacyRuntimeCandidate{}, err
		}
		observed.ServiceStatus = status
		if _, err := lifecycle.attestRetirementArtifacts(candidate); err != nil {
			return LegacyRuntimeCandidate{}, err
		}
		return observed, nil
	}
	observed := candidate
	endpointIdentity, endpointPresent, err := observeProductionLegacyPath(candidate.EndpointPath)
	if err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	if endpointPresent && !endpointIdentity.ownedSocket() {
		return LegacyRuntimeCandidate{}, errors.New("legacy endpoint is not an owned Unix socket")
	}
	if !endpointPresent {
		observed.EndpointStatus = "absent"
	}
	if endpointPresent {
		connection, err := net.DialTimeout("unix", candidate.EndpointPath, 500*time.Millisecond)
		if err != nil {
			if !productionLegacyDialFailureProvesNoListener(err) {
				return LegacyRuntimeCandidate{}, errors.New("legacy endpoint dial failure does not prove authority absence")
			}
			current, present, statErr := observeProductionLegacyPath(candidate.EndpointPath)
			if statErr != nil || !present || !current.same(endpointIdentity) {
				return LegacyRuntimeCandidate{}, errors.New("legacy endpoint changed while reattesting")
			}
			observed.EndpointStatus = "absent"
		} else {
			defer func() { _ = connection.Close() }()
			unixConnection, ok := connection.(*net.UnixConn)
			if !ok {
				return LegacyRuntimeCandidate{}, errors.New("legacy endpoint is not a Unix connection")
			}
			peer, err := observeControlPeer(unixConnection)
			if err != nil {
				return LegacyRuntimeCandidate{}, err
			}
			observed.EndpointStatus = "responsive"
			observed.EndpointType = "unix"
			observed.EndpointOwnerUID = peer.UID
			observed.EndpointOwnerPID = peer.PID
			observed.EndpointOwnerStart = peer.ProcStart
		}
		current, present, statErr := observeProductionLegacyPath(candidate.EndpointPath)
		if statErr != nil || !present || !current.same(endpointIdentity) {
			return LegacyRuntimeCandidate{}, errors.New("legacy endpoint changed while reattesting")
		}
	}
	if candidate.ServiceManager != "" {
		status, err := productionLegacyServiceArtifactStatus(ctx, candidate)
		if err != nil {
			return LegacyRuntimeCandidate{}, err
		}
		observed.ServiceStatus = status
	}
	attestation, err := lifecycle.attestRetirementArtifacts(candidate)
	if err != nil {
		return LegacyRuntimeCandidate{}, err
	}
	if attestation.endpoint.present != endpointPresent ||
		endpointPresent && !attestation.endpoint.identity.same(endpointIdentity) {
		return LegacyRuntimeCandidate{}, errors.New("legacy endpoint changed after reattestation")
	}
	return observed, nil
}

// RetireEndpoint removes only a freshly reattested Agent Sessions artifact.
func (lifecycle *productionLegacyRetirementLifecycle) RetireEndpoint( //nolint:gocyclo // Service and socket retirement share one exact-identity boundary.
	ctx context.Context,
	candidate LegacyRuntimeCandidate,
) error {
	priorAttestation, previouslyReattested := lifecycle.takeRetirementAttestation(candidate)
	observed, err := lifecycle.ReattestEndpoint(ctx, candidate)
	if err != nil {
		return err
	}
	attestation, freshlyReattested := lifecycle.takeRetirementAttestation(candidate)
	if !freshlyReattested {
		return errors.New("legacy artifacts lack a fresh filesystem reattestation")
	}
	if previouslyReattested && !priorAttestation.same(attestation) {
		return errors.New("legacy artifact inode or type changed after reattestation")
	}
	if candidate.EndpointPath == "" {
		if candidate.ServiceManager == "" {
			if !legacyRetirementArtifactsProvenAbsent(candidate, observed) {
				return errors.New("legacy record artifact is not proven free of live authority")
			}
			return lifecycle.retireProductionLegacyRecordArtifact(candidate, attestation)
		}
		if candidate.ServiceManager == "" || candidate.ServiceUnit == "" {
			return errors.New("legacy service artifact lacks exact service identity")
		}
		if !sameLegacyRetirementArtifactAuthority(candidate, observed) {
			return errors.New("legacy service artifact changed immediately before retirement")
		}
		lifecycle.runBeforeArtifactUnlink(candidate.SourcePath)
		artifact, err := openProductionLegacyAttestedRegular(candidate.SourcePath, attestation.source, false)
		if err != nil {
			return err
		}
		defer artifact.close()
		if err := validateProductionLegacyServiceArtifact(candidate, artifact); err != nil {
			return err
		}
		if err := disableProductionRetiredLegacyService(ctx, candidate.ServiceManager, candidate.ServiceUnit); err != nil {
			return err
		}
		if err := artifact.unlink(attestation.source.identity); err != nil {
			return err
		}
		if candidate.ServiceManager == "systemd-user" {
			command := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload")
			command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
			return command.Run()
		}
		return nil
	}
	if legacyRetirementArtifactsProvenAbsent(candidate, observed) {
		if productionLegacySocketArtifactKind(candidate.Kind) {
			return lifecycle.retireProductionLegacySocketArtifact(candidate, attestation)
		}
		if candidate.Kind == "supervisor" {
			return lifecycle.retireProductionLegacySupervisorArtifacts(candidate, attestation)
		}
		return lifecycle.retireProductionLegacyRecordArtifact(candidate, attestation)
	}
	if !sameLegacyRetirementArtifactAuthority(candidate, observed) {
		return errors.New("legacy endpoint identity changed immediately before retirement")
	}
	if attestation.endpoint.present {
		lifecycle.runBeforeArtifactUnlink(candidate.EndpointPath)
		if err := unlinkProductionLegacySocket(candidate.EndpointPath, attestation.endpoint.identity); err != nil {
			return err
		}
	}
	if candidate.ServiceManager == "" {
		if candidate.Kind == "supervisor" {
			return lifecycle.retireProductionLegacySupervisorArtifacts(candidate, attestation)
		}
		return lifecycle.retireProductionLegacyRecordArtifact(candidate, attestation)
	}
	lifecycle.runBeforeArtifactUnlink(candidate.SourcePath)
	artifact, artifactErr := openProductionLegacyAttestedRegular(candidate.SourcePath, attestation.source, false)
	if artifactErr == nil {
		defer artifact.close()
		if err := validateProductionLegacyServiceArtifact(candidate, artifact); err != nil {
			return err
		}
		if err := disableProductionRetiredLegacyService(ctx, candidate.ServiceManager, candidate.ServiceUnit); err != nil {
			return err
		}
		if err := artifact.unlink(attestation.source.identity); err != nil {
			return err
		}
	} else if !os.IsNotExist(artifactErr) {
		return artifactErr
	}
	if candidate.ServiceManager == "systemd-user" {
		command := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload")
		command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
		if err := command.Run(); err != nil {
			return err
		}
	}
	loaded, err := productionLegacyServiceLoaded(ctx, candidate.ServiceManager, candidate.ServiceUnit)
	if err != nil {
		return err
	}
	if loaded {
		return errors.New("legacy service job remained loaded after exact artifact retirement")
	}
	return nil
}

func (lifecycle *productionLegacyRetirementLifecycle) retireProductionLegacySocketArtifact(
	candidate LegacyRuntimeCandidate,
	attestation productionLegacyArtifactAttestation,
) error {
	if candidate.SourcePath != candidate.EndpointPath {
		return errors.New("legacy socket artifact has conflicting source and endpoint paths")
	}
	if !attestation.endpoint.present {
		return nil
	}
	if candidate.SourceRevision != productionLegacySocketIdentityRevision(attestation.endpoint.identity) {
		return errors.New("legacy socket identity changed before retirement")
	}
	lifecycle.runBeforeArtifactUnlink(candidate.EndpointPath)
	return unlinkProductionLegacySocket(candidate.EndpointPath, attestation.endpoint.identity)
}

func productionLegacySocketArtifactKind(kind string) bool {
	return kind == "supervisor_socket_artifact" || kind == "host_agent_socket_artifact"
}

func (lifecycle *productionLegacyRetirementLifecycle) retireProductionLegacyRecordArtifact(
	candidate LegacyRuntimeCandidate,
	attestation productionLegacyArtifactAttestation,
) error {
	if !productionLegacyRetirableRecordKind(candidate.Kind) {
		return fmt.Errorf("legacy candidate kind %q has no closed record-retirement contract", candidate.Kind)
	}
	if !attestation.source.present {
		return nil
	}
	lifecycle.runBeforeArtifactUnlink(candidate.SourcePath)
	record, err := openProductionLegacyAttestedRegular(candidate.SourcePath, attestation.source, false)
	if err != nil {
		return err
	}
	defer record.close()
	body, err := record.readBounded(1, maxProductionLegacyRecordBytes)
	if err != nil {
		return err
	}
	if candidate.ArtifactRevision == "" || candidate.ArtifactRevision != "sha256:"+productionDigest(body) ||
		candidate.ArtifactIdentity == "" ||
		candidate.ArtifactIdentity != productionLegacyArtifactIdentityRevision(attestation.source.identity) || !json.Valid(body) {
		return errors.New("legacy durable record schema or content identity changed before retirement")
	}
	return record.unlink(attestation.source.identity)
}

func productionLegacyRetirableRecordKind(kind string) bool {
	switch kind {
	case "shim", "claude_host", "grok_host", "qwen_host",
		"claude_lane_manager", "grok_lane_manager", "qwen_lane_manager",
		"remote_lane_watch", "interactive_owner_record":
		return true
	default:
		return false
	}
}

func (lifecycle *productionLegacyRetirementLifecycle) retireProductionLegacySupervisorArtifacts(
	candidate LegacyRuntimeCandidate,
	attestation productionLegacyArtifactAttestation,
) error {
	if filepath.Base(candidate.SourcePath) != "supervisor.json" {
		return errors.New("legacy supervisor source is not its exact durable record")
	}
	lifecycle.runBeforeArtifactUnlink(candidate.SourcePath)
	record, err := openProductionLegacyAttestedRegular(candidate.SourcePath, attestation.source, false)
	if err != nil {
		return err
	}
	defer record.close()
	var state productionLegacySupervisorState
	body, err := record.readBounded(1, maxProductionLegacyRecordBytes)
	if err != nil {
		return err
	}
	if err := decodeProductionLegacyJSON(body, &state); err != nil {
		return err
	}
	if candidate.SourceRevision != "sha256:"+productionDigest(body) ||
		state.PID != candidate.PID || state.ProcStart != candidate.ProcStart ||
		state.RuntimeIdentity != candidate.RuntimeIdentity || state.ControlSocket != candidate.EndpointPath {
		return errors.New("legacy supervisor durable record changed before retirement")
	}
	lockPath := filepath.Join(filepath.Dir(candidate.SourcePath), "supervisor-start.lock")
	lifecycle.runBeforeArtifactUnlink(lockPath)
	lock, err := openProductionLegacyRetirementLock(lockPath, attestation.lock)
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() {
			_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
			lock.close()
		}()
	}
	if err := record.unlink(attestation.source.identity); err != nil {
		return err
	}
	if lock != nil {
		if err := lock.unlink(attestation.lock.identity); err != nil {
			return err
		}
	}
	return nil
}

func openProductionLegacyRetirementLock(
	path string,
	expected productionLegacyObservedFile,
) (*productionLegacyAnchoredFile, error) {
	file, err := openProductionLegacyAttestedRegular(path, expected, true)
	if os.IsNotExist(err) && !expected.present {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.close()
		return nil, errors.New("legacy supervisor start lock is held during retirement")
	}
	return file, nil
}

func productionLegacyServiceArtifact(candidate LegacyRuntimeCandidate) error {
	artifact, err := openProductionLegacyRegular(candidate.SourcePath, false)
	if err != nil {
		return err
	}
	defer artifact.close()
	return validateProductionLegacyServiceArtifact(candidate, artifact)
}

func validateProductionLegacyServiceArtifact(
	candidate LegacyRuntimeCandidate,
	artifact *productionLegacyAnchoredFile,
) error {
	body, err := artifact.readBounded(1, maxProductionLegacyRecordBytes)
	if err != nil {
		return err
	}
	if candidate.SourceRevision != "sha256:"+productionDigest(body) {
		return errors.New("legacy service artifact content identity changed")
	}
	return nil
}

func productionLegacyServiceArtifactStatus(ctx context.Context, candidate LegacyRuntimeCandidate) (string, error) {
	artifactErr := productionLegacyServiceArtifact(candidate)
	if artifactErr != nil && !os.IsNotExist(artifactErr) {
		return "", artifactErr
	}
	loaded, err := productionLegacyServiceLoaded(ctx, candidate.ServiceManager, candidate.ServiceUnit)
	if err != nil {
		return "", err
	}
	if artifactErr == nil || loaded {
		return "loaded", nil
	}
	return "absent", nil
}

func productionLegacyServiceLoaded(ctx context.Context, manager, unit string) (bool, error) {
	var command *exec.Cmd
	switch manager {
	case "systemd-user":
		command = exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "--property=LoadState", "--value", "--no-pager") // #nosec G204 -- manager and unit use the closed service identity.
	case "launchd-user":
		command = exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), unit)) //nolint:gosec // Exact journaled label.
	default:
		return false, fmt.Errorf("unsupported legacy service manager %q", manager)
	}
	output, err := command.Output()
	if err == nil {
		if manager == "systemd-user" {
			return strings.TrimSpace(string(output)) == "loaded", nil
		}
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, err
}

func productionLegacyServiceActivity(ctx context.Context, manager, unit string) (string, error) {
	switch manager {
	case "systemd-user":
		return productionLegacySystemdServiceActivity(ctx, unit)
	case "launchd-user":
		return productionLegacyLaunchdServiceActivity(ctx, unit)
	default:
		return "unknown", fmt.Errorf("unsupported legacy service manager %q", manager)
	}
}

func productionLegacySystemdServiceActivity(ctx context.Context, unit string) (string, error) {
	command := exec.CommandContext(ctx, "systemctl", "--user", "show", unit,
		"--property=LoadState", "--property=ActiveState", "--property=MainPID", "--no-pager") // #nosec G204 -- exact closed unit.
	output, err := command.Output()
	if err != nil {
		return productionLegacyServiceActivityFallback(ctx, "systemd-user", unit)
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	if properties["LoadState"] == "not-found" {
		return "absent", nil
	}
	activeState, mainPID := properties["ActiveState"], properties["MainPID"]
	if activeState == "active" || activeState == "activating" || activeState == "reloading" ||
		activeState == "deactivating" || mainPID != "" && mainPID != "0" {
		return "active", nil
	}
	if (activeState == "inactive" || activeState == "failed") && (mainPID == "" || mainPID == "0") {
		return "inactive", nil
	}
	return "unknown", nil
}

func productionLegacyLaunchdServiceActivity(ctx context.Context, unit string) (string, error) {
	command := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), unit)) //nolint:gosec // Exact closed label.
	output, err := command.Output()
	if err != nil {
		return productionLegacyServiceActivityFallback(ctx, "launchd-user", unit)
	}
	lower := strings.ToLower(string(output))
	if strings.Contains(lower, "state = running") || strings.Contains(lower, "pid =") {
		return "active", nil
	}
	if strings.Contains(lower, "state = exited") || strings.Contains(lower, "state = waiting") {
		return "inactive", nil
	}
	return "unknown", nil
}

func productionLegacyServiceActivityFallback(ctx context.Context, manager, unit string) (string, error) {
	loaded, err := productionLegacyServiceLoaded(ctx, manager, unit)
	if err != nil {
		return "unknown", nil //nolint:nilerr // Unobservable manager state is fail-closed evidence, not an inventory failure.
	}
	if !loaded {
		return "absent", nil
	}
	return "unknown", nil
}

func disableProductionRetiredLegacyService(ctx context.Context, manager, unit string) error {
	var command *exec.Cmd
	switch manager {
	case "systemd-user":
		command = exec.CommandContext(ctx, "systemctl", "--user", "disable", unit) // #nosec G204 -- exact retired unit.
	case "launchd-user":
		command = exec.CommandContext(ctx, "launchctl", "disable", fmt.Sprintf("gui/%d/%s", os.Getuid(), unit)) //nolint:gosec // Exact retired label.
	default:
		return fmt.Errorf("unsupported retired legacy service manager %q", manager)
	}
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	return command.Run()
}

var _ LegacyRetirementLifecycle = (*productionLegacyRetirementLifecycle)(nil)
