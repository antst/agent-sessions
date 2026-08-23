# Attestation and Permission Contract

## General peer-mode boundary

Managed Qwen behaves like native Qwen with two additions: authenticated Agent Sessions
communications and the ability to launch supported local or remote product lanes. Agent Sessions
does not replace Qwen's UI, tools, prompts, approval controls, or native session behavior.

## Operational authority

Installed Qwen skills or MCP inventory are not authority. Each Qwen Agent Sessions operation requires
all of the following:

1. raw per-launch capability unavailable in persistent profile configuration;
2. exact ancestry from the live Qwen host/worker;
3. exact PID, process start, and platform strong-start identity;
4. matching durable preparation/launch record and revision;
5. matching native Qwen session UUID and canonical cwd;
6. matching selected-profile fingerprint and installed integration;
7. matching live host-agent registration, product, instance, and delivery socket.

Any mismatch returns the common inactive/unauthorized result and conveys no roster data. A
model-supplied UUID, name, token-shaped string, or group is never sufficient. Native Qwen approval
mode is not Agent Sessions identity and cannot grant messaging, group, parent, or federation
authority.

## Native permission ownership

Agent Sessions owns only launch-time mapping and corroboration:

| Launch preference | Initial native behavior |
|---|---|
| no permission option | Preserve Qwen's native default and corroborate the effective initial mode. |
| explicit `--no-yolo` | Translate to and corroborate native `--approval-mode default`. |
| explicit `--yolo` | Request and corroborate native Qwen yolo at launch. |
| native `--approval-mode MODE` without a wrapper permission choice | Pass through unchanged, retain the exact mode for resume, and corroborate it. |

Repeated or contradictory wrapper choices, or any wrapper permission choice combined with native
`--approval-mode`, fail with exit 2 before preparation or other mutation. No precedence rule is
applied, including when two choices would request the same mode.

After publication, Qwen owns the mode. `/approval-mode`, Shift+Tab, ACP controls, or any other
supported native mechanism may enter or leave yolo. Agent Sessions does not intercept those controls,
terminate a session for using them, or add a sandbox, hook, deny list, tool guard, or input filter to
prevent them.

The durable catalog value is a launch preference used as the default for a later managed resume. It
is not the current native mode and is not a lifetime security classification. Status reports a
trustworthy current native observation when Qwen supplies one; otherwise it reports `unknown`.

## Lane permission handling

For stdio ACP lanes, the Agent Sessions manager maps the launch preference to Qwen's initial mode and
handles native protocol permission requests required to drive the lane. It does not reinterpret a
later native yolo transition as an Agent Sessions policy fault. Mode changes do not alter parent
identity, group membership, routing authorization, persistence, or cleanup ownership.

Initial-mode capability and behavior are live-probed against the selected Qwen version before
publication.

## Messaging and lane authorization

Discovery, direct send, multicast, and named-group broadcast reuse shared membership rules. Mixed
authorized/unauthorized multicast rejects atomically. Empty, wildcard, nonexistent, or
sender-nonmember broadcasts fail. Parent lane calls require the exact managed Qwen parent context;
bare Qwen and stale/exited managed processes remain inactive even if plugin surfaces are visible.

## Cleanup authority

Signaling or deletion requires re-attested exact ownership immediately before the action. Admission
closes and the root is frozen/retired before destructive cleanup. Unknown PID state, recycled PID,
changed path type, changed inode/body, unmatched native UUID, archive conflict, or catalog revision
mismatch retains durable debt and preserves the resource.
