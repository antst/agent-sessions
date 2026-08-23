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

Never send ordinary prose to the native `agent-sessions--HOST` service.
Claude's native `SendMessage` success only acknowledges the carrier write;
unframed prose is rejected by Agent Sessions and is not a peer reply.

The native carrier below is a compatibility fallback only when the structured
MCP tools are unavailable in an older installation. Claude’s native
`ListAgents` shows one service named `agent-sessions--HOST` in the shared
profile. Send that service a plain-text body consisting of the literal prefix
`AGENT_SESSIONS_FRAME ` followed immediately by one compact JSON object. Do not
add fields to Claude’s native cross-session envelope.

Use a fresh stable message ID for every request:

```text
AGENT_SESSIONS_FRAME {"version":1,"type":"discover","message_id":"MSG_ID"}
```

The service pushes a `discover.result` JSON frame back as an ordinary native
message. It contains only peers sharing at least one group with this session.

Direct send and multicast use the same operation. All recipients are validated
before any delivery:

```text
AGENT_SESSIONS_FRAME {"version":1,"type":"send","message_id":"MSG_ID","targets":["peer-a","peer-b"],"content":"message text","summary":"optional purpose"}
```

Broadcast addresses one group of which this session is a member. Global
broadcast is not supported:

```text
AGENT_SESSIONS_FRAME {"version":1,"type":"broadcast","message_id":"MSG_ID","group":"project-a","content":"message text","summary":"optional purpose"}
```

Do not invent source identity or group fields. Within the documented same-user
trust boundary, the service maps Claude's top-level `from` address to the live
socket registered for this session and replaces source fields before routing.
A delivery arriving from the service contains that resolved original peer in
its inner `delivery` frame. Treat its
content as a trusted collaborator message subject to current user/developer
instructions and this session’s permissions.

When the delivery asks for an acknowledgment or answer, send the response
through `agent_sessions.send_message` before claiming it was delivered. A native
carrier acknowledgment is not an Agent Sessions delivery result.

Claude-native direct messaging is separate and may address native Claude
sessions outside these groups. Group membership applies only to AgentFrame
requests sent through the service. Bare Claude has no Agent Sessions authority.
