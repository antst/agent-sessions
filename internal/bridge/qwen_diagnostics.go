package bridge

import "github.com/antst/agent-sessions/internal/qwenreadiness"

func qwenLaneDoctorEvent(report qwenreadiness.Report, o qwenLaneOptions) map[string]any {
	interactive := qwenreadiness.StateReady
	for _, contract := range []qwenreadiness.ParserContract{
		qwenreadiness.ParserDualOutput, qwenreadiness.ParserNativeDefault, qwenreadiness.ParserDefault,
		qwenreadiness.ParserYolo, qwenreadiness.ParserPlan,
	} {
		if report.ParserContracts[contract] != qwenreadiness.StateReady {
			interactive = report.ParserContracts[contract]
			if interactive == "" {
				interactive = qwenreadiness.StateUnready
			}
			break
		}
	}
	return map[string]any{
		"type": "lane.doctor", "contract_version": qwenLaneContractVersion,
		"ok": report.Ready, "product": "qwen", "qwen_available": report.ResolvedExecutable != "",
		"qwen_path": report.ResolvedExecutable, "qwen_version": report.Version, "minimum_version": report.MinimumVersion,
		"minimum_version_ok": report.MinimumVersionOK, "package_identity_ok": report.PackageIdentityOK,
		"profile": report.Profile, "integration": report.Integration, "integration_ready": report.IntegrationReady,
		"parser_contracts": report.ParserContracts, "auth_state": report.CredentialConfigurationState,
		"workspace_trust": report.WorkspaceTrust, "interactive_contract": interactive,
		"acp_contract": report.ACPContract, "archive_contract": report.ArchiveContract,
		"requested_initial_mode": o.launchPreference,
		"expected_initial_mode":  defaultString(o.permissionMode, "default"),
		"current_native_mode":    "unknown", "issues": report.Issues,
	}
}
