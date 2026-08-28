package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/antst/agent-sessions/internal/testutil"
)

func TestLegacyInventorySourcesCloseEveryShippedLinuxRoot(t *testing.T) {
	home := "/home/tester"
	state := "/state/tester"
	runtime := "/run/user/501"
	temp := "/tmp"
	sources, err := LegacyInventorySources(LegacyInventoryOptions{
		Platform: "linux", UID: 501, HomeDir: home, StateHome: state,
		RuntimeDir: runtime, SystemTempDir: temp,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []legacyInventorySourceExpectation{
		{"bridge-state", "state", filepath.Join(state, "claude-code-peer"), true},
		{"host-agent-state", "state", filepath.Join(state, "agent-sessions", "agents"), true},
		{"bridge-xdg-runtime", "runtime", filepath.Join(runtime, "codex-claude-peer-501"), true},
		{"bridge-temp-runtime", "runtime", filepath.Join(temp, "codex-claude-peer-501"), true},
		{"bridge-compact-runtime", "runtime", filepath.Join(temp, "ccp-501"), true},
		{"federator-xdg-runtime", "runtime", filepath.Join(runtime, "peer-federator"), true},
		{"federator-temp-runtime", "runtime", filepath.Join(temp, "peer-federator-501"), true},
		{"grok-xdg-runtime", "runtime", filepath.Join(runtime, "agent-sessions-grok-501"), true},
		{"grok-temp-runtime", "runtime", filepath.Join(temp, "agent-sessions-grok-501"), true},
		{"grok-compact-runtime", "runtime", filepath.Join(temp, "asg-501"), true},
		{"systemd-host-agent", "service", filepath.Join(home, ".config", "systemd", "user", "peer-federator-agent.service"), true},
		{"systemd-host-agent-env", "configuration", filepath.Join(home, ".config", "peer-federator", "agent.env"), true},
	}
	assertLegacyInventorySources(t, sources, want)
	assertLegacyInventoryIsBounded(t, home, sources)
	assertLegacyInventoryExcludes(t, sources, "peer-federator-hub.service", "agent-sessions-hub", ".codex", ".claude", ".grok", ".qwen")
}

func TestLegacyInventorySourcesCloseEveryShippedDarwinRootAndRecordedTMPDIRSpelling(t *testing.T) {
	home := "/Users/tester"
	state := "/Users/tester/Library/Application Support/state"
	darwinTemp := "/private/var/folders/aa/bb/T"
	recorded := []string{
		"/private/var/folders/recorded/one/ccp-501",
		"/private/var/folders/recorded/two/codex-claude-peer-501",
		"/private/var/folders/recorded/three/peer-federator-501",
	}
	sources, err := LegacyInventorySources(LegacyInventoryOptions{
		Platform: "darwin", UID: 501, HomeDir: home, StateHome: state,
		SystemTempDir: darwinTemp, RecordedRuntimeRoots: recorded,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []legacyInventorySourceExpectation{
		{"bridge-state", "state", filepath.Join(state, "claude-code-peer"), true},
		{"host-agent-state", "state", filepath.Join(state, "agent-sessions", "agents"), true},
		{"bridge-compact-runtime", "runtime", "/tmp/ccp-501", true},
		{"bridge-tmp-runtime", "runtime", "/tmp/codex-claude-peer-501", true},
		{"bridge-darwin-compact-runtime", "runtime", filepath.Join(darwinTemp, "ccp-501"), true},
		{"bridge-darwin-runtime", "runtime", filepath.Join(darwinTemp, "codex-claude-peer-501"), true},
		{"federator-temp-runtime", "runtime", filepath.Join(darwinTemp, "peer-federator-501"), true},
		{"grok-darwin-runtime", "runtime", filepath.Join(darwinTemp, "agent-sessions-grok-501"), true},
		{"grok-compact-runtime", "runtime", "/tmp/asg-501", true},
		{"launchd-host-agent", "service", filepath.Join(home, "Library", "LaunchAgents", "net.antst.peer-federator.agent.plist"), true},
		{"recorded-bridge-runtime", "recorded_runtime", recorded[0], true},
		{"recorded-bridge-runtime-2", "recorded_runtime", recorded[1], true},
		{"recorded-federator-runtime", "recorded_runtime", recorded[2], true},
	}
	assertLegacyInventorySources(t, sources, want)
	assertLegacyInventoryIsBounded(t, home, sources)
	assertLegacyInventoryExcludes(t, sources, "net.antst.peer-federator.hub", "agent-sessions-hub", ".codex", ".claude", ".grok", ".qwen")
}

func TestProductionDarwinSupervisorEndpointInventoryFindsOrphanAcrossThreeRoots(t *testing.T) {
	root := testutil.ShortSocketRoot(t, "mdi-", filepath.Join("recorded-temp", "ccp-501", "supervisor-new-profile.sock"))
	uid := strconv.Itoa(os.Getuid())
	roots := []string{
		filepath.Join(root, "system-temp", "ccp-"+uid),
		filepath.Join(root, "system-temp", "codex-claude-peer-"+uid),
		filepath.Join(root, "recorded-temp", "ccp-"+uid),
	}
	for _, runtimeRoot := range roots {
		if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	orphanEndpoint := filepath.Join(roots[0], "supervisor-old-profile.sock")
	recordedEndpoint := filepath.Join(roots[2], "supervisor-new-profile.sock")
	listeners := make([]*net.UnixListener, 0, 2)
	for _, endpoint := range []string{orphanEndpoint, recordedEndpoint} {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		listeners = append(listeners, listener)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()

	sources := []LegacyInventorySource{
		{ID: "bridge-darwin-compact-runtime", Kind: "runtime", Path: roots[0], Target: true, MaxDepth: 3},
		{ID: "bridge-darwin-runtime", Kind: "runtime", Path: roots[1], Target: true, MaxDepth: 3},
		{ID: "recorded-bridge-runtime", Kind: "recorded_runtime", Path: roots[2], Target: true, MaxDepth: 3},
	}
	runtimeIdentity := "sha256:orphan-runtime"
	probe := productionLegacySupervisorEndpointProbe{
		Observe: func(endpoint string) (productionLegacySupervisorStatus, controlPeerEvidence, error) {
			if endpoint != orphanEndpoint {
				t.Fatalf("record-backed endpoint was not deduplicated: %s", endpoint)
			}
			return productionLegacySupervisorStatus{
					PID: 7123, ProcStart: "start-7123", ControlSocket: endpoint, ProfileKey: "old-profile",
					PluginVersion: "0.2.4", RuntimeIdentity: runtimeIdentity, Ready: true, ShimCount: 1,
				}, controlPeerEvidence{
					UID: os.Getuid(), PID: 7123, ProcStart: "start-7123", StrongStart: "strong-7123",
				}, nil
		},
		Executable: func(int) (string, error) { return "/opt/agent-sessions/agent-session-runtime", nil },
		Arguments: func(int) ([]string, error) {
			return []string{"/opt/agent-sessions/agent-session-runtime", "supervisor", "run", "--plugin-version", "0.2.4"}, nil
		},
		RuntimeIdentity: func(string) (string, error) { return runtimeIdentity, nil },
	}
	candidates, err := productionLegacyOrphanSupervisors(
		context.Background(), sources, 100, map[string]struct{}{recordedEndpoint: {}}, probe,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].EndpointPath != orphanEndpoint ||
		candidates[0].Classification != LegacyClassificationActiveManagedBlocker ||
		candidates[0].Kind != "supervisor_socket_artifact" || candidates[0].PID != 7123 ||
		candidates[0].RuntimeIdentity != runtimeIdentity {
		t.Fatalf("orphan supervisor candidates = %+v", candidates)
	}
	identity, present, err := observeProductionLegacyPath(orphanEndpoint)
	if err != nil || !present {
		t.Fatalf("observe orphan identity: present=%v err=%v", present, err)
	}
	if candidates[0].SourceRevision != productionLegacySocketIdentityRevision(identity) {
		t.Fatalf("orphan source revision = %q", candidates[0].SourceRevision)
	}
	failedProbe := probe
	failedProbe.Observe = func(string) (productionLegacySupervisorStatus, controlPeerEvidence, error) {
		return productionLegacySupervisorStatus{}, controlPeerEvidence{}, syscall.ECONNREFUSED
	}
	stale, err := productionLegacyOrphanSupervisor(orphanEndpoint, "old-profile", 101, failedProbe)
	if err != nil || stale.Kind != "supervisor_socket_artifact" ||
		stale.Classification != LegacyClassificationStale || stale.SourceRevision != candidates[0].SourceRevision {
		t.Fatalf("stable failed-probe socket artifact = %+v, %v", stale, err)
	}
	hungProbe := probe
	hungProbe.Observe = func(string) (productionLegacySupervisorStatus, controlPeerEvidence, error) {
		return productionLegacySupervisorStatus{}, controlPeerEvidence{
			UID: os.Getuid(), PID: 8123, ProcStart: "start-8123", StrongStart: "strong-8123",
		}, io.ErrUnexpectedEOF
	}
	hung, err := productionLegacyOrphanSupervisor(orphanEndpoint, "old-profile", 102, hungProbe)
	if err != nil || hung.Classification != LegacyClassificationUnknown || hung.PID != 8123 || hung.EndpointOwnerPID != 8123 ||
		hung.ProcessStatus != "known" || hung.EndpointStatus != "unknown" {
		t.Fatalf("connected supervisor with failed status = %+v, %v", hung, err)
	}
	timeoutProbe := probe
	timeoutProbe.Observe = func(string) (productionLegacySupervisorStatus, controlPeerEvidence, error) {
		return productionLegacySupervisorStatus{}, controlPeerEvidence{}, context.DeadlineExceeded
	}
	timedOut, err := productionLegacyOrphanSupervisor(orphanEndpoint, "old-profile", 103, timeoutProbe)
	if err != nil || timedOut.Classification != LegacyClassificationUnknown {
		t.Fatalf("non-refusal dial failure = %+v, %v", timedOut, err)
	}
}

func TestLegacyCandidateClassificationRequiresExactRoleSpecificEvidence(t *testing.T) {
	tests := []struct {
		kind       string
		sessionIDs []string
		laneIDs    []string
		want       string
	}{
		{kind: "supervisor", want: "live_legacy_authority"},
		{kind: "shim", sessionIDs: []string{"codex-session"}, want: "active_managed_blocker"},
		{kind: "claude_host", sessionIDs: []string{"claude-session"}, want: "active_managed_blocker"},
		{kind: "grok_host", sessionIDs: []string{"grok-session"}, want: "active_managed_blocker"},
		{kind: "qwen_host", sessionIDs: []string{"qwen-session"}, want: "active_managed_blocker"},
		{kind: "claude_lane_manager", laneIDs: []string{"claude-lane"}, want: "active_managed_blocker"},
		{kind: "grok_lane_manager", laneIDs: []string{"grok-lane"}, want: "active_managed_blocker"},
		{kind: "qwen_lane_manager", laneIDs: []string{"qwen-lane"}, want: "active_managed_blocker"},
		{kind: "remote_lane_watch", laneIDs: []string{"remote-lane"}, want: "active_managed_blocker"},
		{kind: "host_federation_agent", want: "live_legacy_authority"},
		{kind: "host_agent_service_job", want: "live_legacy_authority"},
	}
	for index, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			evidence := exactLegacyCandidateEvidence(test.kind, index+100)
			evidence.RelatedSessionIDs = append([]string(nil), test.sessionIDs...)
			evidence.RelatedLaneIDs = append([]string(nil), test.laneIDs...)
			if test.kind == "host_agent_service_job" {
				evidence.PID, evidence.ProcStart, evidence.StrongStart = 0, "", ""
				evidence.Process, evidence.Endpoint = LegacyProcessObservation{}, LegacyEndpointObservation{}
				evidence.Service = LegacyServiceObservation{
					Status: "loaded", Manager: "systemd-user", Unit: "peer-federator-agent.service",
					Executable: "/opt/agent-sessions/peer-federator", ArgvRole: "agent",
				}
			}
			candidate, err := ClassifyLegacyCandidate(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Kind != test.kind || candidate.Classification != test.want ||
				!reflect.DeepEqual(candidate.RelatedSessionIDs, test.sessionIDs) ||
				!reflect.DeepEqual(candidate.RelatedLaneIDs, test.laneIDs) {
				t.Fatalf("candidate = %#v, want kind=%q classification=%q sessions=%v lanes=%v",
					candidate, test.kind, test.want, test.sessionIDs, test.laneIDs)
			}
			if candidate.PID != evidence.PID || candidate.ProcStart != evidence.ProcStart ||
				candidate.StrongStart != evidence.StrongStart || candidate.EndpointPath != evidence.Endpoint.Path ||
				candidate.RuntimeIdentity != evidence.RuntimeIdentity || candidate.SourcePath != evidence.SourcePath {
				t.Fatalf("exact candidate evidence was not retained: %#v", candidate)
			}
			if test.kind == "host_agent_service_job" &&
				(candidate.ServiceManager != evidence.Service.Manager || candidate.ServiceUnit != evidence.Service.Unit) {
				t.Fatalf("exact service evidence was not retained: %#v", candidate)
			}
		})
	}
}

func TestLegacyCandidateClassificationNeverTrustsPathPIDNameOrScalarCountAlone(t *testing.T) {
	exact := exactLegacyCandidateEvidence("supervisor", 401)
	tests := []struct {
		name string
		edit func(*LegacyCandidateEvidence)
		want string
	}{
		{name: "proven absent owner makes stale count stale", edit: func(e *LegacyCandidateEvidence) {
			e.Process.Status, e.Endpoint.Status, e.ReportedActiveCount = "absent", "absent", 99
		}, want: "stale"},
		{name: "recycled pid conflicts", edit: func(e *LegacyCandidateEvidence) {
			e.Process.ProcStart = "replacement-start"
		}, want: "conflicting"},
		{name: "changed endpoint identity conflicts", edit: func(e *LegacyCandidateEvidence) {
			e.Endpoint.OwnerPID++
		}, want: "conflicting"},
		{name: "unknown process is unknown", edit: func(e *LegacyCandidateEvidence) {
			e.Process.Status = "unknown"
		}, want: "unknown"},
		{name: "path pid and process name alone remain unknown", edit: func(e *LegacyCandidateEvidence) {
			e.RuntimeIdentity, e.ProcStart, e.StrongStart = "", "", ""
			e.Endpoint = LegacyEndpointObservation{Path: e.Endpoint.Path, Status: "present"}
			e.Process = LegacyProcessObservation{Status: "known", PID: e.PID, Executable: "agent-session-runtime"}
		}, want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := exact
			test.edit(&evidence)
			candidate, err := ClassifyLegacyCandidate(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Classification != test.want {
				t.Fatalf("classification = %q, want %q; candidate=%#v", candidate.Classification, test.want, candidate)
			}
		})
	}
}

func TestLegacyCandidateClassificationExplicitlyExcludesHubAndNativeVendors(t *testing.T) {
	for _, test := range []struct{ kind, source string }{
		{kind: "central_hub", source: "/service/peer-federator-hub.service"},
		{kind: "native_codex", source: "/vendor/codex"},
		{kind: "native_claude", source: "/vendor/claude"},
		{kind: "native_grok", source: "/vendor/grok"},
		{kind: "native_qwen", source: "/vendor/qwen"},
	} {
		evidence := exactLegacyCandidateEvidence(test.kind, 900)
		evidence.SourcePath = test.source
		candidate, err := ClassifyLegacyCandidate(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Classification != "excluded" {
			t.Fatalf("%s classification = %q, want excluded", test.kind, candidate.Classification)
		}
	}
}

type legacyInventorySourceExpectation struct {
	id, kind, path string
	target         bool
}

func assertLegacyInventorySources(t *testing.T, sources []LegacyInventorySource, want []legacyInventorySourceExpectation) {
	t.Helper()
	got := make([]legacyInventorySourceExpectation, 0, len(sources))
	for _, source := range sources {
		got = append(got, legacyInventorySourceExpectation{source.ID, source.Kind, source.Path, source.Target})
	}
	sort.Slice(got, func(i, j int) bool { return got[i].id+got[i].path < got[j].id+got[j].path })
	sort.Slice(want, func(i, j int) bool { return want[i].id+want[i].path < want[j].id+want[j].path })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy inventory sources = %#v, want %#v", got, want)
	}
}

func assertLegacyInventoryIsBounded(t *testing.T, home string, sources []LegacyInventorySource) {
	t.Helper()
	for _, source := range sources {
		clean := filepath.Clean(source.Path)
		if clean == filepath.Clean(home) || clean == filepath.Dir(filepath.Clean(home)) ||
			strings.Contains(source.Path, "**") || source.MaxDepth < 0 || source.MaxDepth > 5 {
			t.Fatalf("legacy source is a broad scan: %#v", source)
		}
		if source.Kind == "service" && source.MaxDepth != 0 {
			t.Fatalf("service source is not an exact file: %#v", source)
		}
	}
}

func assertLegacyInventoryExcludes(t *testing.T, sources []LegacyInventorySource, fragments ...string) {
	t.Helper()
	for _, source := range sources {
		for _, fragment := range fragments {
			if strings.Contains(source.Path, fragment) {
				t.Fatalf("legacy host inventory included excluded path %q in %#v", fragment, source)
			}
		}
	}
}

func exactLegacyCandidateEvidence(kind string, suffix int) LegacyCandidateEvidence {
	pid := 5000 + suffix
	procStart := "proc-start-" + strconv.Itoa(suffix)
	strongStart := "strong-start-" + strconv.Itoa(suffix)
	endpoint := "/runtime/legacy/" + kind + ".sock"
	return LegacyCandidateEvidence{
		CandidateID: "candidate-" + kind, Kind: kind, SourcePath: "/state/legacy/" + kind + ".json",
		RuntimeIdentity: "sha256:legacy-" + kind, PID: pid, ProcStart: procStart, StrongStart: strongStart,
		Process: LegacyProcessObservation{
			Status: "known", PID: pid, ProcStart: procStart, StrongStart: strongStart,
			Executable: "/opt/agent-sessions/agent-session-runtime", ArgvRole: kind,
		},
		Endpoint: LegacyEndpointObservation{
			Status: "responsive", Path: endpoint, Type: "unix", OwnerUID: 501,
			OwnerPID: pid, OwnerProcStart: procStart, RuntimeIdentity: "sha256:legacy-" + kind,
		},
	}
}
