package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestProductionDarwinRecordedFederatorRootRecoversLiveAndRetiresStableStaleAgentSocket(t *testing.T) {
	root := shortMigrationAgentSocketRoot(t)
	uid := os.Getuid()
	home := filepath.Join(root, "home")
	stateHome := filepath.Join(root, "state")
	currentTemp := filepath.Join(root, "current-tmp")
	oldTemp := filepath.Join(root, "prior-tmp")
	oldRuntime := filepath.Join(oldTemp, "peer-federator-"+strconv.Itoa(uid))
	oldBridgeRuntime := filepath.Join(oldTemp, "ccp-"+strconv.Itoa(uid))
	agentState := filepath.Join(stateHome, "agent-sessions", "agents", "old-host")
	supervisorProfile := filepath.Join(stateHome, "claude-code-peer", "profiles", "recorded")
	servicePath := filepath.Join(home, "Library", "LaunchAgents", "net.antst.peer-federator.agent.plist")
	for _, directory := range []string{agentState, supervisorProfile, currentTemp, oldRuntime, filepath.Dir(servicePath)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentState, "sessions.json"), []byte(`{"version":2,"sessions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, []byte("<plist version=\"1.0\"></plist>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	supervisorState, err := json.Marshal(productionLegacySupervisorState{
		ControlSocket: filepath.Join(oldBridgeRuntime, "supervisor-recorded.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(supervisorProfile, "supervisor.json"), supervisorState, 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "bin", "peer-federator")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("legacy executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeIdentity, err := hostExecutableIdentity(executable)
	if err != nil {
		t.Fatal(err)
	}

	recordedRoots, err := productionRecordedRuntimeRoots(stateHome, uid)
	if err != nil || !slices.Contains(recordedRoots, oldRuntime) {
		t.Fatalf("derive prior TMPDIR federation root from durable provenance: roots=%v err=%v", recordedRoots, err)
	}
	sources, err := LegacyInventorySources(LegacyInventoryOptions{
		Platform: "darwin", UID: uid, HomeDir: home, StateHome: stateHome,
		SystemTempDir: currentTemp, RecordedRuntimeRoots: recordedRoots,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source, found := legacyInventorySourceByID(sources, "recorded-federator-runtime"); !found || source.Path != oldRuntime {
		t.Fatalf("prior TMPDIR federation root missing from closed inventory: %+v", sources)
	}

	endpoint := filepath.Join(oldRuntime, "agent.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(endpoint)
	})
	probe := productionLegacyInspectionProbe{
		ObserveAgent: func(context.Context, string) (productionLegacyAgentObservation, error) {
			return productionLegacyAgentObservation{
				Status: productionLegacyAgentStatus{
					RuntimeVersion: "0.2.4", ProtocolVersion: federation.ProtocolVersion,
					HostID: "old-host", HostName: "old host", RuntimeDir: oldRuntime, StateDir: agentState,
				},
				Peer: controlPeerEvidence{
					UID: uid, PID: 4401, ProcStart: "start-4401", StrongStart: "strong-4401",
				},
				Executable: executable, Identity: runtimeIdentity,
			}, nil
		},
		ServiceLoaded:   func(context.Context, string, string) (bool, error) { return true, nil },
		ServiceActivity: func(context.Context, string, string) (string, error) { return "inactive", nil },
		Supervisors: func(context.Context, []LegacyInventorySource, LegacyInventorySource, int64) ([]LegacyRuntimeCandidate, error) {
			return nil, nil
		},
		RemoteWatches: func([]LegacyRuntimeCandidate, int64) ([]LegacyRuntimeCandidate, error) { return nil, nil },
	}
	live, err := collectProductionFirstMigrationWithProbe(context.Background(), sources, 100, probe)
	if err != nil {
		t.Fatal(err)
	}
	liveAgent := legacyCandidateByKind(t, live.Candidates, "host_federation_agent")
	if liveAgent.Classification != LegacyClassificationActiveManagedBlocker || liveAgent.EndpointPath != endpoint {
		t.Fatalf("prior TMPDIR live agent = %+v", liveAgent)
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	probe.ObserveAgent = func(context.Context, string) (productionLegacyAgentObservation, error) {
		return productionLegacyAgentObservation{}, syscall.ECONNREFUSED
	}
	stopped, err := collectProductionFirstMigrationWithProbe(context.Background(), sources, 101, probe)
	if err != nil {
		t.Fatal(err)
	}
	stale := legacyCandidateByKind(t, stopped.Candidates, "host_agent_socket_artifact")
	identity, present, err := observeProductionLegacyPath(endpoint)
	if err != nil || !present {
		t.Fatalf("observe stable stale agent socket: present=%v err=%v", present, err)
	}
	if stale.Classification != LegacyClassificationStale || stale.ProcessStatus != "absent" ||
		stale.EndpointStatus != "absent" || stale.SourcePath != endpoint || stale.EndpointPath != endpoint ||
		stale.SourceRevision != productionLegacySocketIdentityRevision(identity) {
		t.Fatalf("revision-bound stale agent socket = %+v", stale)
	}
	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: stopped.Candidates})
	if err != nil || !report.LegacyAbsenceVerified || len(report.Debt) != 0 {
		t.Fatalf("stable stale socket quiescence = %+v, %v", report, err)
	}

	lifecycle := &productionLegacyRetirementLifecycle{
		retirementArtifacts: make(map[string]productionLegacyArtifactAttestation),
	}
	observed, err := lifecycle.ReattestEndpoint(context.Background(), stale)
	if err != nil || observed.EndpointStatus != "absent" {
		t.Fatalf("reattest stale socket = %+v, %v", observed, err)
	}
	if err := lifecycle.RetireEndpoint(context.Background(), stale); err != nil {
		t.Fatalf("retire revision-bound stale socket: %v", err)
	}
	if _, err := os.Lstat(endpoint); !os.IsNotExist(err) {
		t.Fatalf("retired stale agent socket still exists: %v", err)
	}
}

func TestProductionLegacyAgentSocketAmbiguityRemainsDebt(t *testing.T) {
	root := shortMigrationAgentSocketRoot(t)
	runtimeRoot := filepath.Join(root, "prior-tmp", "peer-federator-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(runtimeRoot, "agent.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(endpoint) })

	tests := []struct {
		name            string
		observe         productionLegacyAgentObservation
		observeErr      error
		withService     bool
		serviceActivity func(context.Context, string, string) (string, error)
	}{
		{name: "timeout", observeErr: context.DeadlineExceeded},
		{name: "temporary resource exhaustion", observeErr: syscall.EAGAIN},
		{name: "permission denied", observeErr: syscall.EACCES},
		{name: "unknown failure", observeErr: errors.New("unclassified dial failure")},
		{
			name: "refusal with partial process identity",
			observe: productionLegacyAgentObservation{Peer: controlPeerEvidence{
				UID: os.Getuid(), PID: 4401, ProcStart: "start-4401",
			}},
			observeErr: syscall.ECONNREFUSED,
		},
		{
			name: "refusal while service is active", observeErr: syscall.ECONNREFUSED, withService: true,
			serviceActivity: func(context.Context, string, string) (string, error) { return "active", nil },
		},
		{
			name: "refusal with unobservable service", observeErr: syscall.ECONNREFUSED, withService: true,
			serviceActivity: func(context.Context, string, string) (string, error) {
				return "unknown", errors.New("service probe unavailable")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sources := []LegacyInventorySource{
				{ID: "recorded-federator-runtime", Kind: "recorded_runtime", Path: runtimeRoot, Target: true, MaxDepth: 3},
			}
			if test.withService {
				sources = append(sources, LegacyInventorySource{
					ID: "launchd-host-agent", Kind: "service",
					Path: filepath.Join(root, "LaunchAgents", "net.antst.peer-federator.agent.plist"), Target: true,
				})
			}
			serviceActivity := test.serviceActivity
			if serviceActivity == nil {
				serviceActivity = func(context.Context, string, string) (string, error) { return "absent", nil }
			}
			probe := productionLegacyInspectionProbe{
				ObserveAgent: func(context.Context, string) (productionLegacyAgentObservation, error) {
					return test.observe, test.observeErr
				},
				ServiceLoaded:   func(context.Context, string, string) (bool, error) { return false, nil },
				ServiceActivity: serviceActivity,
				RemoteWatches:   func([]LegacyRuntimeCandidate, int64) ([]LegacyRuntimeCandidate, error) { return nil, nil },
			}
			inspection, err := collectProductionFirstMigrationWithProbe(context.Background(), sources, 200, probe)
			if err != nil {
				t.Fatal(err)
			}
			candidate := legacyCandidateByID(t, inspection.Candidates, "source-recorded-federator-runtime")
			if candidate.Classification != LegacyClassificationUnknown || candidate.SourceRevision != "unresponsive-agent-endpoint" {
				t.Fatalf("ambiguous socket candidate = %+v", candidate)
			}
			report, quiescenceErr := EvaluateLegacyQuiescence(
				context.Background(), LegacyQuiescenceRequest{Candidates: inspection.Candidates},
			)
			if quiescenceErr == nil || len(report.Debt) == 0 || report.LegacyAbsenceVerified {
				t.Fatalf("ambiguous socket did not remain debt: %+v, %v", report, quiescenceErr)
			}
			if info, statErr := os.Lstat(endpoint); statErr != nil || info.Mode()&os.ModeSocket == 0 {
				t.Fatalf("read-only ambiguous inspection mutated socket: info=%v err=%v", info, statErr)
			}
		})
	}
}

func legacyInventorySourceByID(sources []LegacyInventorySource, id string) (LegacyInventorySource, bool) {
	for _, source := range sources {
		if source.ID == id {
			return source, true
		}
	}
	return LegacyInventorySource{}, false
}

func shortMigrationAgentSocketRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "mas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func legacyCandidateByKind(t *testing.T, candidates []LegacyRuntimeCandidate, kind string) LegacyRuntimeCandidate {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.Kind == kind {
			return candidate
		}
	}
	t.Fatalf("legacy candidate kind %q missing: %+v", kind, candidates)
	return LegacyRuntimeCandidate{}
}

func legacyCandidateByID(t *testing.T, candidates []LegacyRuntimeCandidate, candidateID string) LegacyRuntimeCandidate {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.CandidateID == candidateID {
			return candidate
		}
	}
	t.Fatalf("legacy candidate %q missing: %+v", candidateID, candidates)
	return LegacyRuntimeCandidate{}
}
