// Package qwenreadiness owns the single session-free Qwen readiness engine.
package qwenreadiness

import (
	"context"
	"fmt"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const (
	// MinimumVersion is the oldest Qwen Code release whose contracts are supported.
	MinimumVersion = "0.21.15"
	// ExpectedPackage is the authoritative native package identity.
	ExpectedPackage = "@qwen-code/qwen-code"
	// IntegrationVersion is the exact Agent Sessions Qwen plugin contract
	// shipped by this release. Interactive and lane admission share it so their
	// readiness checks cannot drift from one another.
	IntegrationVersion = "0.4.0"
)

// State describes whether a probed readiness contract is usable.
type State string

const (
	// StateReady means the contract was positively corroborated.
	StateReady State = "ready"
	// StateUnknown means the probe could not make an authoritative determination.
	StateUnknown State = "unknown"
	// StateUnready means the contract was authoritatively rejected.
	StateUnready State = "unready"
)

// ParserContract names one presence-sensitive native CLI parser contract.
type ParserContract string

const (
	// ParserDualOutput covers mutually exclusive structured-output options.
	ParserDualOutput ParserContract = "dual_output"
	// ParserNativeDefault covers omission of the approval-mode option.
	ParserNativeDefault ParserContract = "approval_native_default"
	// ParserDefault covers an explicitly constrained native approval mode.
	ParserDefault ParserContract = "approval_default"
	// ParserYolo covers the native yolo approval mode.
	ParserYolo ParserContract = "approval_yolo"
	// ParserPlan covers the native plan approval mode.
	ParserPlan ParserContract = "approval_plan"
)

// PresentValue preserves the distinction between an omitted and empty option.
type PresentValue struct {
	Set   bool
	Value string
}

// ParserProbe describes a session-free parser request.
type ParserProbe struct {
	Contract        ParserContract
	RequiredOptions []string
	ApprovalMode    PresentValue
}

// ExecutableEvidence identifies the selected native Qwen executable and package.
type ExecutableEvidence struct {
	Executable         string
	ResolvedExecutable string
	Package            string
	Version            string
}

// ACPEvidence records initialize-only ACP identity and capability evidence.
type ACPEvidence struct {
	ProtocolVersion int
	AgentName       string
	AgentVersion    string
	LoadSession     bool
	ListSessions    bool
	ResumeSession   bool
	MCP             bool
}

// ArchiveEvidence records native archive-control capability evidence.
type ArchiveEvidence struct {
	ProtocolVersion string
	QwenVersion     string
	Workspace       string
	Capabilities    []string
}

// IntegrationEvidence identifies the exact Agent Sessions plugin installation.
type IntegrationEvidence struct {
	ID                 string
	Version            string
	ManifestDigest     string
	ProfileFingerprint string
	Ready              bool
}

// EvidenceSource supplies non-session readiness probes for one selected profile.
type EvidenceSource interface {
	// InspectExecutable resolves and identifies the selected executable.
	InspectExecutable(context.Context, string) (ExecutableEvidence, error)
	// ProbeParser exercises a parser contract without opening a session.
	ProbeParser(context.Context, string, ParserProbe) (State, error)
	// InitializeACP performs only the ACP initialize exchange.
	InitializeACP(context.Context, string, qwenprofile.Identity) (ACPEvidence, error)
	// ProbeArchive inspects the native archive-control capability.
	ProbeArchive(context.Context, string, string, qwenprofile.Identity) (ArchiveEvidence, error)
	// InspectTrust checks the selected workspace's native trust state.
	InspectTrust(context.Context, string, string, qwenprofile.Identity) (State, error)
	// InspectIntegration checks exact plugin identity in the selected profile.
	InspectIntegration(context.Context, qwenprofile.Identity) (IntegrationEvidence, error)
	// InspectCredentialConfiguration checks provider configuration without reading secrets.
	InspectCredentialConfiguration(context.Context, qwenprofile.Identity) (State, error)
}

// Request contains all immutable inputs to one readiness evaluation.
type Request struct {
	Executable                 string
	Workspace                  string
	Profile                    qwenprofile.Identity
	ExpectedIntegrationVersion string
	Source                     EvidenceSource
}

// Issue is one machine-readable reason readiness was not established.
type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Report is the complete readiness result shared by admission and diagnostics.
type Report struct {
	Ready                        bool                     `json:"ready"`
	Executable                   string                   `json:"executable"`
	ResolvedExecutable           string                   `json:"resolved_executable"`
	Version                      string                   `json:"version"`
	MinimumVersion               string                   `json:"minimum_version"`
	MinimumVersionOK             bool                     `json:"minimum_version_ok"`
	PackageIdentityOK            bool                     `json:"package_identity_ok"`
	Profile                      qwenprofile.Identity     `json:"profile"`
	ParserContracts              map[ParserContract]State `json:"parser_contracts"`
	ACPContract                  State                    `json:"acp_contract"`
	ArchiveContract              State                    `json:"archive_contract"`
	WorkspaceTrust               State                    `json:"workspace_trust"`
	Integration                  IntegrationEvidence      `json:"integration"`
	IntegrationReady             bool                     `json:"integration_ready"`
	CredentialConfigurationState State                    `json:"credential_configuration_state"`
	Issues                       []Issue                  `json:"issues"`
}

// Check runs the one session-free readiness sequence shared by launch
// admission, federation advertisement, and doctor. Evidence sources may use
// parser failures and initialize-only protocol exchanges, but must never open
// or resume a native Qwen session.
func Check(ctx context.Context, request Request) (Report, error) { //nolint:gocyclo // Readiness intentionally aggregates every independent native contract into one report.
	if request.Source == nil {
		return Report{}, fmt.Errorf("qwen readiness evidence source is nil")
	}
	if request.Executable == "" || request.Workspace == "" || request.ExpectedIntegrationVersion == "" {
		return Report{}, fmt.Errorf("qwen readiness request is incomplete")
	}

	report := Report{
		Executable: request.Executable, MinimumVersion: MinimumVersion,
		Profile: request.Profile, ParserContracts: make(map[ParserContract]State),
		ACPContract: StateUnready, ArchiveContract: StateUnready,
		WorkspaceTrust: StateUnready, CredentialConfigurationState: StateUnready,
	}
	addIssue := func(code, message string) {
		report.Issues = append(report.Issues, Issue{Code: code, Message: message})
	}

	executable, err := request.Source.InspectExecutable(ctx, request.Executable)
	if err != nil {
		addIssue("executable_probe_failed", err.Error())
	} else {
		report.ResolvedExecutable = executable.ResolvedExecutable
		report.Version = executable.Version
		report.PackageIdentityOK = executable.Package == ExpectedPackage
		report.MinimumVersionOK = productruntime.VersionAtLeast(executable.Version, MinimumVersion)
		if !report.PackageIdentityOK {
			addIssue("package_identity", "selected Qwen executable has an unexpected package identity")
		}
		if !report.MinimumVersionOK {
			addIssue("version_floor", fmt.Sprintf("Qwen version %q is below required %s", executable.Version, MinimumVersion))
		}
	}

	parserProbes := []ParserProbe{
		{Contract: ParserDualOutput, RequiredOptions: []string{
			"--session-id", "--resume", "--chat-recording", "--input-file", "--json-file", "--mcp-config",
		}},
		{Contract: ParserNativeDefault, ApprovalMode: PresentValue{}},
		{Contract: ParserDefault, ApprovalMode: PresentValue{Set: true, Value: "default"}},
		{Contract: ParserYolo, ApprovalMode: PresentValue{Set: true, Value: "yolo"}},
		{Contract: ParserPlan, ApprovalMode: PresentValue{Set: true, Value: "plan"}},
	}
	for _, probe := range parserProbes {
		state, probeErr := request.Source.ProbeParser(ctx, request.Executable, probe)
		if probeErr != nil {
			state = StateUnready
			addIssue("parser_"+string(probe.Contract), probeErr.Error())
		} else if state != StateReady {
			addIssue("parser_"+string(probe.Contract), "required Qwen parser contract is not ready")
		}
		report.ParserContracts[probe.Contract] = state
	}

	acp, err := request.Source.InitializeACP(ctx, request.Executable, request.Profile)
	switch {
	case err != nil:
		addIssue("acp_initialize", err.Error())
	case acp.ProtocolVersion == 1 && acp.AgentName == "qwen-code" &&
		productruntime.VersionAtLeast(acp.AgentVersion, MinimumVersion) && acp.LoadSession &&
		acp.ListSessions && acp.ResumeSession && acp.MCP:
		report.ACPContract = StateReady
	default:
		addIssue("acp_contract", "Qwen initialize response does not provide the required identity and capabilities")
	}

	archive, err := request.Source.ProbeArchive(ctx, request.Executable, request.Workspace, request.Profile)
	switch {
	case err != nil:
		addIssue("archive_probe", err.Error())
	case archive.ProtocolVersion == "v1" && productruntime.VersionAtLeast(archive.QwenVersion, MinimumVersion) &&
		archive.Workspace == request.Workspace && contains(archive.Capabilities, "session_archive"):
		report.ArchiveContract = StateReady
	default:
		addIssue("archive_contract", "Qwen native session archive capability is not ready")
	}

	report.WorkspaceTrust, err = request.Source.InspectTrust(ctx, request.Executable, request.Workspace, request.Profile)
	if err != nil {
		report.WorkspaceTrust = StateUnready
		addIssue("workspace_trust", err.Error())
	} else if report.WorkspaceTrust != StateReady {
		addIssue("workspace_trust", "Qwen workspace trust is not ready")
	}

	report.Integration, err = request.Source.InspectIntegration(ctx, request.Profile)
	if err != nil {
		addIssue("integration_probe", err.Error())
	} else {
		report.IntegrationReady = report.Integration.Ready && report.Integration.ID == "agent-sessions" &&
			report.Integration.Version == request.ExpectedIntegrationVersion &&
			report.Integration.ManifestDigest != "" &&
			report.Integration.ProfileFingerprint == request.Profile.Fingerprint
		if !report.IntegrationReady {
			code := "integration_identity"
			if report.Integration.ProfileFingerprint != request.Profile.Fingerprint {
				code = "profile_identity"
			}
			addIssue(code, "Qwen Agent Sessions integration does not exactly match the selected profile and release")
		}
	}

	report.CredentialConfigurationState, err = request.Source.InspectCredentialConfiguration(ctx, request.Profile)
	if err != nil {
		report.CredentialConfigurationState = StateUnready
		addIssue("credential_configuration", err.Error())
	} else if report.CredentialConfigurationState != StateReady {
		addIssue("credential_configuration", "Qwen provider configuration is not ready")
	}

	report.Ready = report.PackageIdentityOK && report.MinimumVersionOK &&
		allParserContractsReady(report.ParserContracts, parserProbes) &&
		report.ACPContract == StateReady && report.ArchiveContract == StateReady &&
		report.WorkspaceTrust == StateReady && report.IntegrationReady &&
		report.CredentialConfigurationState == StateReady && len(report.Issues) == 0
	return report, nil
}

func allParserContractsReady(states map[ParserContract]State, probes []ParserProbe) bool {
	for _, probe := range probes {
		if states[probe.Contract] != StateReady {
			return false
		}
	}
	return true
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
