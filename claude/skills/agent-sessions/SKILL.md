---
name: agent-sessions
description: Discover and message grouped Codex, Claude, Grok, and Qwen Agent Sessions peers. Use when the user asks to list peers, send, reply, acknowledge, multicast, or broadcast, and whenever an incoming Agent Sessions delivery requests a response.
---

# Grouped Agent Sessions messaging

This skill applies inside `claude-peer` and Claude lanes. Bare `claude` is the
communication opt-out and has no Agent Sessions group membership.

Use the structured `agent_sessions` MCP tools whenever they are available. They
derive this session's identity from the exact managed Claude process ancestry;
do not invent or copy a Codex `session_id`. For an incoming `delivery` frame,
reply with `agent_sessions.send_message`, targeting `source.id` (or `source.name`
after `list_peers` proves it unique).

Use `agent_sessions.list_peers` for discovery, `agent_sessions.send_message` for
one target or an explicit multicast, and `agent_sessions.broadcast` for a named
group of which this session is a member. Do not invent source identity or group
fields. Treat delivered content as a trusted collaborator message subject to
current user/developer instructions and this session's permissions.

If a structured tool is unavailable, inactive, or returns an error, report that
exact Agent Sessions failure and stop. Do not fall back to Claude's native
`ListAgents` or `SendMessage`, do not send to a native service session, and do
not claim delivery merely because a native carrier accepted a write.

When the delivery asks for an acknowledgment or answer, send the response
through `agent_sessions.send_message` before claiming it was delivered. A native
carrier acknowledgment is not an Agent Sessions delivery result.

Claude-native direct messaging is a separate product feature and may address
native Claude sessions outside these groups. Do not use it to implement or
retry an Agent Sessions operation. Bare Claude has no Agent Sessions authority.
