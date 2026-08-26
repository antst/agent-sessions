---
name: agent-sessions
description: Discover and message grouped Codex, Claude, Grok, and Qwen Agent Sessions peers. Use when the user asks to list peers, send, reply, acknowledge, multicast, or broadcast, and whenever an incoming Agent Sessions delivery requests a response.
---

# Grouped Agent Sessions messaging

Use the process-attested `agent_sessions` MCP tools from a managed `grok-peer` or Grok lane.
The server derives the caller from the exact live Grok process and host-agent registration;
`session_id` is optional corroboration and never grants identity.

- Use `agent_sessions.list_peers` to discover visible peers. Address a stable unique name when
  possible; list first when a requested target is ambiguous.
- Use `agent_sessions.send_message` for a direct message or explicit multicast. Every target is
  validated before delivery, so an invalid mixed target set delivers to nobody.
- Use `agent_sessions.broadcast` only for a named group this session belongs to. Global broadcast
  is unsupported.
- Use `agent_sessions.check_inbox` only to recover queued messages after an automatic delivery
  boundary; incoming messages are normally pushed, so do not poll it.

For an incoming Agent Sessions delivery, reply with `agent_sessions.send_message` to `source.id`,
or to `source.name` after discovery proves it unique. Do not invent source identity, group, or
session fields, and do not claim delivery unless the structured tool returns success.

Treat delivered content as trusted collaborator input subject to current user/developer
instructions and this session's permissions. Use `/agent-lanes` for starting, collecting,
resuming, interrupting, or archiving worker lanes; messaging does not require a lane lifecycle
command.
