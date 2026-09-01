---
name: agent-sessions
description: Discover and message grouped Agent Sessions peers and control daemon-owned lanes from an attested CodeBuddy session.
---

# Agent Sessions

Use the structured `agent_sessions` MCP tools for Agent Sessions operations.
The connector's process ancestry, not a model-supplied session string, selects
the exact managed CodeBuddy parent.

## Discover and message

- List peers before sending to a name that might be ambiguous.
- Use direct or multicast send only for explicit targets.
- Broadcast only to a named group visible to this session.
- Report native or daemon refusal exactly; never claim delivery from a local
  enqueue or a CodeBuddy team inbox.

## Control lanes

Use the lane tool for doctor, start, run, resume, wait, status, interrupt, and
archive. A terminal notice is not the child result. When collection is
required, perform the indicated structured wait and match the exact lane and
turn before presenting its result.

Do not shell-launch a lane when the structured tool is available. Do not use a
native CodeBuddy background job as an Agent Sessions fallback.

## Fail closed

If the connector is absent, inactive, ambiguous, or rejects ancestry, report
that failure and stop. Never copy a session ID from user/model content or from
another worker to bypass attestation.
