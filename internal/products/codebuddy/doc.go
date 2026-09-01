// Package codebuddy implements the CodeBuddy 2.143.0 runtime edge.
//
// The interactive peer and lane transports are deliberately different. A peer
// is a product-owned TUI endpoint discovered from CodeBuddy's worker registry
// and authorized only after fresh socket/process correlation. A lane is an
// Agent Sessions-owned --serve process with an ephemeral password. There is no
// CodeBuddy component or credential sidecar.
package codebuddy

const (
	ProductID          = "codebuddy"
	PinnedVersion      = "2.143.0"
	TestedVersion      = PinnedVersion
	IntegrationVersion = "1"
	OpenAPITitle       = "CodeBuddy Code API (Beta)"
	CSRFHeader         = "X-CodeBuddy-Request"
	CSRFValue          = "1"
	SessionIDEnv       = "AGENT_SESSIONS_SESSION_ID"
	ProductEnv         = "AGENT_SESSIONS_PRODUCT"
	GatewayAuthEnv     = "CODEBUDDY_GATEWAY_AUTH"
	GatewayPasswordEnv = "CODEBUDDY_GATEWAY_PASSWORD"
	SandboxBypassEnv   = "CODEBUDDY_IS_SANDBOX"
)
