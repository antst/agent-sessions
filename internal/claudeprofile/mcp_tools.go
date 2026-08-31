package claudeprofile

// ProjectMCPServerName is the public server name used by a project-scoped
// Agent Sessions .mcp.json declaration. Managed Claude sessions disable that
// project entry and use the origin-qualified installed plugin instead: an
// arbitrary repository must not be able to inherit approval by declaring a
// different server under the public name.
const ProjectMCPServerName = "agent_sessions"

// ManagedAgentSessionsMCPTools returns the exact origin-qualified Claude
// permission identifiers for the process-attested Agent Sessions tools made
// available by the installed plugin to managed Claude peers and lanes. Keep
// this list narrow: launcher-owned approval is a convenience for the managed
// lifecycle, not a global Claude policy change.
func ManagedAgentSessionsMCPTools() []string {
	return []string{
		"mcp__plugin_agent-sessions_agent_sessions__list_peers",
		"mcp__plugin_agent-sessions_agent_sessions__send_message",
		"mcp__plugin_agent-sessions_agent_sessions__broadcast",
		"mcp__plugin_agent-sessions_agent_sessions__lane",
	}
}
