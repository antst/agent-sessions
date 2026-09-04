package qwenreadiness

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const testIntegrationDigest = "9a5ce38af305bf9cf43c146c7e7f762175e4cf391bdefb526dec2c5c19cf7fd8"

func TestCheckCollectsOneCompleteSessionFreeReadinessReport(t *testing.T) {
	request, source := readyTestRequest(t)
	report, err := Check(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Ready || report.Executable != request.Executable ||
		report.ResolvedExecutable != source.executable.ResolvedExecutable || report.Version != "0.21.15" ||
		report.MinimumVersion != MinimumVersion || !report.MinimumVersionOK || !report.PackageIdentityOK {
		t.Fatalf("executable readiness = %#v", report)
	}
	if report.Profile != request.Profile || !report.IntegrationReady || report.Integration != source.integration {
		t.Fatalf("profile/integration readiness = %#v", report)
	}
	if report.ACPContract != StateReady || report.ArchiveContract != StateReady ||
		report.WorkspaceTrust != StateReady || report.CredentialConfigurationState != StateReady || len(report.Issues) != 0 {
		t.Fatalf("native readiness = %#v", report)
	}

	wantCalls := []string{
		"executable",
		"parser:dual_output",
		"parser:approval_native_default",
		"parser:approval_default",
		"parser:approval_yolo",
		"parser:approval_plan",
		"acp:initialize",
		"archive:capabilities",
		"trust",
		"integration",
		"credential_configuration",
	}
	if !reflect.DeepEqual(source.calls, wantCalls) {
		t.Fatalf("readiness probe calls = %#v, want %#v", source.calls, wantCalls)
	}
	for _, call := range source.calls {
		for _, forbidden := range []string{"session/new", "session/load", "session/resume", "session/prompt", "prompt"} {
			if strings.Contains(call, forbidden) {
				t.Fatalf("session-free readiness invoked %q", call)
			}
		}
	}
}

func TestCheckUsesPinnedPackageAndMinimumVersion(t *testing.T) {
	for _, test := range []struct {
		name      string
		packageID string
		version   string
		wantReady bool
		wantIssue string
	}{
		{name: "pinned floor", packageID: ExpectedPackage, version: "0.21.15", wantReady: true},
		{name: "newer with live contracts", packageID: ExpectedPackage, version: "0.22.0", wantReady: true},
		{name: "below floor", packageID: ExpectedPackage, version: "0.21.14", wantIssue: "version"},
		{name: "signed component", packageID: ExpectedPackage, version: "0.21.+15", wantIssue: "version"},
		{name: "dangling suffix", packageID: ExpectedPackage, version: "0.21.15-", wantIssue: "version"},
		{name: "portable overflow", packageID: ExpectedPackage, version: "4294967296.21.15", wantIssue: "version"},
		{name: "wrong package", packageID: "qwen-lookalike", version: "0.21.15", wantIssue: "package"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, source := readyTestRequest(t)
			source.executable.Package = test.packageID
			source.executable.Version = test.version
			source.acp.AgentVersion = test.version
			source.archive.QwenVersion = test.version
			report, err := Check(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if report.Ready != test.wantReady {
				t.Fatalf("ready = %t, report %#v", report.Ready, report)
			}
			if test.wantReady && (!report.MinimumVersionOK || !report.PackageIdentityOK || len(report.Issues) != 0) {
				t.Fatalf("accepted executable report = %#v", report)
			}
			if test.wantIssue != "" && !reportHasIssue(report, test.wantIssue) {
				t.Fatalf("report issues = %#v, want %q", report.Issues, test.wantIssue)
			}
		})
	}
}

func TestCheckRequestsPresenceSensitiveParserProbes(t *testing.T) {
	request, source := readyTestRequest(t)
	if _, err := Check(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := []ParserProbe{
		{
			Contract: ParserDualOutput,
			RequiredOptions: []string{
				"--session-id", "--resume", "--chat-recording", "--input-file", "--json-file", "--mcp-config",
			},
		},
		{Contract: ParserNativeDefault, ApprovalMode: PresentValue{}},
		{Contract: ParserDefault, ApprovalMode: PresentValue{Set: true, Value: "default"}},
		{Contract: ParserYolo, ApprovalMode: PresentValue{Set: true, Value: "yolo"}},
		{Contract: ParserPlan, ApprovalMode: PresentValue{Set: true, Value: "plan"}},
	}
	if !reflect.DeepEqual(source.parserProbes, want) {
		t.Fatalf("parser probes = %#v, want %#v", source.parserProbes, want)
	}
}

func TestCheckRejectsFailedParserOrInitializeOnlyACPContract(t *testing.T) {
	t.Run("parser", func(t *testing.T) {
		request, source := readyTestRequest(t)
		source.parserStates[ParserDualOutput] = StateUnready
		report, err := Check(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if report.Ready || report.ParserContracts[ParserDualOutput] != StateUnready || !reportHasIssue(report, "parser") {
			t.Fatalf("parser failure report = %#v", report)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*ACPEvidence)
	}{
		{name: "protocol", mutate: func(e *ACPEvidence) { e.ProtocolVersion = 2 }},
		{name: "agent", mutate: func(e *ACPEvidence) { e.AgentName = "lookalike" }},
		{name: "version", mutate: func(e *ACPEvidence) { e.AgentVersion = "0.21.14" }},
		{name: "load", mutate: func(e *ACPEvidence) { e.LoadSession = false }},
		{name: "list", mutate: func(e *ACPEvidence) { e.ListSessions = false }},
		{name: "resume", mutate: func(e *ACPEvidence) { e.ResumeSession = false }},
		{name: "mcp", mutate: func(e *ACPEvidence) { e.MCP = false }},
	} {
		t.Run("acp "+test.name, func(t *testing.T) {
			request, source := readyTestRequest(t)
			test.mutate(&source.acp)
			report, err := Check(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if report.Ready || report.ACPContract != StateUnready || !reportHasIssue(report, "acp") {
				t.Fatalf("ACP failure report = %#v", report)
			}
			if countTestCall(source.calls, "acp:initialize") != 1 {
				t.Fatalf("ACP calls = %#v", source.calls)
			}
		})
	}
}

func TestCheckRequiresArchiveCapabilityAndTrustedWorkspace(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		request, source := readyTestRequest(t)
		source.archive.Capabilities = []string{"session_resume"}
		report, err := Check(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if report.Ready || report.ArchiveContract != StateUnready || !reportHasIssue(report, "archive") {
			t.Fatalf("archive failure report = %#v", report)
		}
	})

	for _, state := range []State{StateUnknown, StateUnready} {
		t.Run("trust "+string(state), func(t *testing.T) {
			request, source := readyTestRequest(t)
			source.trust = state
			report, err := Check(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if report.Ready || report.WorkspaceTrust != state || !reportHasIssue(report, "trust") {
				t.Fatalf("trust failure report = %#v", report)
			}
		})
	}
}

func TestCheckRequiresExactProfileAndIntegrationIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*testEvidenceSource)
		wantIssue string
	}{
		{
			name: "profile fingerprint",
			mutate: func(source *testEvidenceSource) {
				source.integration.ProfileFingerprint = strings.Repeat("0", 64)
			},
			wantIssue: "profile",
		},
		{
			name: "integration name",
			mutate: func(source *testEvidenceSource) {
				source.integration.ID = "lookalike"
			},
			wantIssue: "integration",
		},
		{
			name: "integration version",
			mutate: func(source *testEvidenceSource) {
				source.integration.Version = "0.2.0"
			},
			wantIssue: "integration",
		},
		{
			name: "manifest inventory",
			mutate: func(source *testEvidenceSource) {
				source.integration.Ready = false
			},
			wantIssue: "integration",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, source := readyTestRequest(t)
			test.mutate(source)
			report, err := Check(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if report.Ready || report.IntegrationReady || !reportHasIssue(report, test.wantIssue) {
				t.Fatalf("identity failure report = %#v", report)
			}
		})
	}
}

func TestCheckReportsOnlyNonSecretCredentialProviderConfigurationState(t *testing.T) {
	const secret = "provider-token-must-never-appear"
	for _, state := range []State{StateReady, StateUnknown, StateUnready} {
		t.Run(string(state), func(t *testing.T) {
			request, source := readyTestRequest(t)
			source.credentialConfiguration = state
			source.secretSentinel = secret
			report, err := Check(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if report.CredentialConfigurationState != state {
				t.Fatalf("credential configuration state = %q, want %q", report.CredentialConfigurationState, state)
			}
			if report.Ready != (state == StateReady) {
				t.Fatalf("ready = %t for credential state %q", report.Ready, state)
			}
			body, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)
			for _, forbidden := range []string{secret, "provider_authenticated", "effective_initial_mode", "access_token", "api_key"} {
				if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
					t.Fatalf("readiness report exposes or overclaims %q: %s", forbidden, body)
				}
			}
		})
	}
}

type testEvidenceSource struct {
	executable              ExecutableEvidence
	parserStates            map[ParserContract]State
	parserProbes            []ParserProbe
	acp                     ACPEvidence
	archive                 ArchiveEvidence
	trust                   State
	integration             IntegrationEvidence
	credentialConfiguration State
	secretSentinel          string
	calls                   []string
}

func readyTestRequest(t *testing.T) (Request, *testEvidenceSource) {
	t.Helper()
	root := t.TempDir()
	profile, err := qwenprofile.ResolveEnvironment(testEnvironment(map[string]string{"HOME": root}))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "bin", "qwen")
	workspace := filepath.Join(root, "workspace")
	source := &testEvidenceSource{
		executable: ExecutableEvidence{
			Executable: executable, ResolvedExecutable: executable,
			Package: ExpectedPackage, Version: "0.21.15",
		},
		parserStates: map[ParserContract]State{
			ParserDualOutput: StateReady, ParserNativeDefault: StateReady, ParserDefault: StateReady,
			ParserYolo: StateReady, ParserPlan: StateReady,
		},
		acp: ACPEvidence{
			ProtocolVersion: 1, AgentName: "qwen-code", AgentVersion: "0.21.15",
			LoadSession: true, ListSessions: true, ResumeSession: true, MCP: true,
		},
		archive: ArchiveEvidence{
			ProtocolVersion: "v1", QwenVersion: "0.21.15", Workspace: workspace,
			Capabilities: []string{"session_archive"},
		},
		trust: StateReady,
		integration: IntegrationEvidence{
			ID: "agent-sessions", Version: "0.2.1", ManifestDigest: testIntegrationDigest,
			ProfileFingerprint: profile.Fingerprint, Ready: true,
		},
		credentialConfiguration: StateReady,
	}
	return Request{
		Executable: executable, Workspace: workspace, Profile: profile,
		ExpectedIntegrationVersion: "0.2.1", Source: source,
	}, source
}

func (s *testEvidenceSource) InspectExecutable(_ context.Context, executable string) (ExecutableEvidence, error) {
	s.calls = append(s.calls, "executable")
	if executable != s.executable.Executable {
		return ExecutableEvidence{}, fmt.Errorf("unexpected executable %q", executable)
	}
	return s.executable, nil
}

func (s *testEvidenceSource) ProbeParser(_ context.Context, executable string, probe ParserProbe) (State, error) {
	s.calls = append(s.calls, "parser:"+string(probe.Contract))
	s.parserProbes = append(s.parserProbes, probe)
	if executable != s.executable.Executable {
		return StateUnready, fmt.Errorf("unexpected executable %q", executable)
	}
	return s.parserStates[probe.Contract], nil
}

func (s *testEvidenceSource) InitializeACP(_ context.Context, executable string, _ qwenprofile.Identity) (ACPEvidence, error) {
	s.calls = append(s.calls, "acp:initialize")
	if executable != s.executable.Executable {
		return ACPEvidence{}, fmt.Errorf("unexpected executable %q", executable)
	}
	return s.acp, nil
}

func (s *testEvidenceSource) ProbeArchive(_ context.Context, executable, workspace string, _ qwenprofile.Identity) (ArchiveEvidence, error) {
	s.calls = append(s.calls, "archive:capabilities")
	if executable != s.executable.Executable || workspace != s.archive.Workspace {
		return ArchiveEvidence{}, fmt.Errorf("unexpected archive request executable=%q workspace=%q", executable, workspace)
	}
	return s.archive, nil
}

func (s *testEvidenceSource) InspectTrust(_ context.Context, executable, workspace string, _ qwenprofile.Identity) (State, error) {
	s.calls = append(s.calls, "trust")
	if executable != s.executable.Executable || workspace != s.archive.Workspace {
		return StateUnready, fmt.Errorf("unexpected trust request executable=%q workspace=%q", executable, workspace)
	}
	return s.trust, nil
}

func (s *testEvidenceSource) InspectIntegration(_ context.Context, _ qwenprofile.Identity) (IntegrationEvidence, error) {
	s.calls = append(s.calls, "integration")
	return s.integration, nil
}

func (s *testEvidenceSource) InspectCredentialConfiguration(_ context.Context, _ qwenprofile.Identity) (State, error) {
	s.calls = append(s.calls, "credential_configuration")
	return s.credentialConfiguration, nil
}

func testEnvironment(values map[string]string) qwenprofile.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func reportHasIssue(report Report, fragment string) bool {
	for _, issue := range report.Issues {
		if strings.Contains(strings.ToLower(issue.Code+" "+issue.Message), strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}

func countTestCall(calls []string, wanted string) int {
	count := 0
	for _, call := range calls {
		if call == wanted {
			count++
		}
	}
	return count
}
