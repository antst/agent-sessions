package bridge

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

func TestQwenDoctorProjectionReportsEvidenceWithoutOverclaimingSessionState(t *testing.T) {
	report := qwenreadiness.Report{
		Ready: true, ResolvedExecutable: "/opt/qwen", Version: "0.21.15", MinimumVersion: "0.21.15",
		MinimumVersionOK: true, IntegrationReady: true, CredentialConfigurationState: qwenreadiness.StateUnknown,
		WorkspaceTrust: qwenreadiness.StateReady, ACPContract: qwenreadiness.StateReady, ArchiveContract: qwenreadiness.StateReady,
		ParserContracts: map[qwenreadiness.ParserContract]qwenreadiness.State{
			qwenreadiness.ParserDualOutput: qwenreadiness.StateReady, qwenreadiness.ParserNativeDefault: qwenreadiness.StateReady,
			qwenreadiness.ParserDefault: qwenreadiness.StateReady, qwenreadiness.ParserYolo: qwenreadiness.StateReady,
			qwenreadiness.ParserPlan: qwenreadiness.StateReady,
		},
	}
	event := qwenLaneDoctorEvent(report, qwenLaneOptions{launchPreference: "native:plan", permissionMode: "plan", permissionModeSet: true})
	if event["type"] != "lane.doctor" || event["contract_version"] != qwenLaneContractVersion ||
		event["requested_initial_mode"] != "native:plan" || event["expected_initial_mode"] != "plan" ||
		event["current_native_mode"] != "unknown" || event["auth_state"] != qwenreadiness.StateUnknown {
		t.Fatalf("Qwen doctor event = %#v", event)
	}
	body, _ := json.Marshal(event)
	lower := strings.ToLower(string(body))
	for _, forbidden := range []string{"access_token", "refresh_token", "api_key", "logged_in", "effective_mode"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("Qwen doctor overclaims or exposes %q: %s", forbidden, body)
		}
	}
}

func TestQwenDoctorProjectionCarriesEverySessionFreeEvidenceClass(t *testing.T) {
	profile := qwenprofile.Identity{
		QwenHomeSet: true, QwenHome: "/profiles/qwen", QwenRuntimeSet: true,
		QwenRuntimeDir: "/runtime/qwen", Fingerprint: strings.Repeat("f", 64),
	}
	integration := qwenreadiness.IntegrationEvidence{
		ID: "agent-sessions", Version: qwenreadiness.IntegrationVersion,
		ManifestDigest: strings.Repeat("a", 64), ProfileFingerprint: profile.Fingerprint, Ready: true,
	}
	parsers := allReadyQwenParserContracts()
	issues := []qwenreadiness.Issue{{Code: "credential_configuration", Message: "provider selection is unavailable"}}
	report := qwenreadiness.Report{
		Ready: false, Executable: "qwen", ResolvedExecutable: "/opt/qwen", Version: "0.22.0",
		MinimumVersion: qwenreadiness.MinimumVersion, MinimumVersionOK: true, PackageIdentityOK: true,
		Profile: profile, ParserContracts: parsers, ACPContract: qwenreadiness.StateReady,
		ArchiveContract: qwenreadiness.StateReady, WorkspaceTrust: qwenreadiness.StateReady,
		Integration: integration, IntegrationReady: true,
		CredentialConfigurationState: qwenreadiness.StateUnknown, Issues: issues,
	}
	event := qwenLaneDoctorEvent(report, qwenLaneOptions{launchPreference: "native_default"})
	checks := map[string]any{
		"ok": false, "qwen_available": true, "qwen_path": "/opt/qwen", "qwen_version": "0.22.0",
		"minimum_version": qwenreadiness.MinimumVersion, "minimum_version_ok": true,
		"package_identity_ok": true, "profile": profile, "integration": integration,
		"integration_ready": true, "parser_contracts": parsers, "workspace_trust": qwenreadiness.StateReady,
		"interactive_contract": qwenreadiness.StateReady, "acp_contract": qwenreadiness.StateReady,
		"archive_contract": qwenreadiness.StateReady, "auth_state": qwenreadiness.StateUnknown,
		"issues": issues,
	}
	for field, want := range checks {
		if got := event[field]; !reflect.DeepEqual(got, want) {
			t.Errorf("doctor %s = %#v, want %#v", field, got, want)
		}
	}
}

func TestQwenDoctorProjectionPreservesCredentialStateWithoutLoginClaim(t *testing.T) {
	for _, state := range []qwenreadiness.State{
		qwenreadiness.StateReady, qwenreadiness.StateUnknown, qwenreadiness.StateUnready,
	} {
		report := qwenreadiness.Report{
			ParserContracts:              allReadyQwenParserContracts(),
			CredentialConfigurationState: state,
		}
		event := qwenLaneDoctorEvent(report, qwenLaneOptions{launchPreference: "native_default"})
		if event["auth_state"] != state || event["current_native_mode"] != "unknown" {
			t.Fatalf("credential state %q projection = %#v", state, event)
		}
		body, _ := json.Marshal(event)
		for _, forbidden := range []string{"logged_in", "access_token", "refresh_token", "effective_mode"} {
			if strings.Contains(strings.ToLower(string(body)), forbidden) {
				t.Fatalf("credential state %q overclaim contains %q: %s", state, forbidden, body)
			}
		}
	}
}

func TestQwenDoctorInitialModeProjectionMatchesLaunchRequest(t *testing.T) {
	tests := []struct {
		argv       []string
		preference string
		mode       string
	}{
		{argv: []string{"doctor", "--json"}, preference: "native_default", mode: "default"},
		{argv: []string{"doctor", "--json", "--yolo"}, preference: "yolo", mode: "yolo"},
		{argv: []string{"doctor", "--json", "--no-yolo"}, preference: "non_yolo", mode: "default"},
		{argv: []string{"doctor", "--json", "--approval-mode", "plan"}, preference: "native:plan", mode: "plan"},
	}
	for _, test := range tests {
		options, err := parseQwenLaneArgs(test.argv)
		if err != nil {
			t.Fatal(err)
		}
		event := qwenLaneDoctorEvent(qwenreadiness.Report{ParserContracts: allReadyQwenParserContracts()}, options)
		if event["requested_initial_mode"] != test.preference || event["expected_initial_mode"] != test.mode {
			t.Fatalf("doctor %q mode projection = %#v", test.argv, event)
		}
	}
}

func allReadyQwenParserContracts() map[qwenreadiness.ParserContract]qwenreadiness.State {
	return map[qwenreadiness.ParserContract]qwenreadiness.State{
		qwenreadiness.ParserDualOutput:    qwenreadiness.StateReady,
		qwenreadiness.ParserNativeDefault: qwenreadiness.StateReady,
		qwenreadiness.ParserDefault:       qwenreadiness.StateReady,
		qwenreadiness.ParserYolo:          qwenreadiness.StateReady,
		qwenreadiness.ParserPlan:          qwenreadiness.StateReady,
	}
}

func TestQwenDoctorProjectionDoesNotClaimInteractiveReadyWhenParserEvidenceFails(t *testing.T) {
	report := qwenreadiness.Report{ParserContracts: map[qwenreadiness.ParserContract]qwenreadiness.State{
		qwenreadiness.ParserDualOutput: qwenreadiness.StateReady, qwenreadiness.ParserNativeDefault: qwenreadiness.StateUnready,
	}}
	event := qwenLaneDoctorEvent(report, qwenLaneOptions{launchPreference: "native_default"})
	if event["interactive_contract"] != qwenreadiness.StateUnready || event["expected_initial_mode"] != "default" {
		t.Fatalf("unready Qwen doctor event = %#v", event)
	}
}

func TestQwenDoctorParserAcceptsOnlyProfilePermissionAndCWDLaunchOptions(t *testing.T) {
	parsed, err := parseQwenLaneArgs([]string{"doctor", "--json", "--qwen-home", "/tmp/qwen", "-C", "/tmp", "--no-yolo"})
	if err != nil || parsed.qwenHome != "/tmp/qwen" || parsed.cwd != "/tmp" || parsed.permissionMode != "default" {
		t.Fatalf("Qwen doctor options = %+v, %v", parsed, err)
	}
	if _, err := parseQwenLaneArgs([]string{"doctor", "--name", "invalid"}); err == nil {
		t.Fatal("Qwen doctor accepted a session-mutating name")
	}
}
