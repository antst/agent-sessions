---
name: agent-sessions
description: Discover and message grouped Codex, Claude, and Grok Agent Sessions peers through the single host-agent service visible to Claude. Use when the user asks to list peers, send or multicast a peer message, or broadcast to one of this session's groups.
---

# Grouped Agent Sessions messaging

This skill applies inside `claude-peer` and Claude lanes. Bare `claude` is the
communication opt-out and has no Agent Sessions group membership.

Claude’s native `ListAgents` shows one service named `agent-sessions--HOST` in
the private profile. Send that service a plain-text body consisting of the
literal prefix `AGENT_SESSIONS_FRAME ` followed immediately by one compact JSON
object. The prefix prevents Claude's `SendMessage` tool from interpreting the
AgentFrame as one of Claude's own typed control objects. Do not add fields to
Claude’s native cross-session envelope.

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
