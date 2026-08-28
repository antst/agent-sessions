# Data Model: Unified User Daemon

The model consolidates existing Agent Sessions-owned records. It does not copy vendor credentials,
profiles, prompts, results already owned by a vendor transcript, or native session history.

## Host Runtime Authority

The one authoritative host-side Agent Sessions daemon for an OS user on one host. The separately
deployed central hub is a different network-service entity and is not part of this record.

### Fields

- `schema_version`: supported durable state schema
- `generation`: monotonically increasing committed authority generation
- `runtime_version`: release version
- `runtime_identity`: digest/identity of the exact `agent-sessions` host binary
- `host_id`: existing stable federation host ID
- `host_name`: existing human-readable host name
- `pid`, `proc_start`, `strong_start`: exact live daemon process identity
- `control_endpoint`: the one private local Unix socket
- `service_manager`: `systemd-user` or `launchd-user`
- `service_unit`: exact unit/label identity
- `started_at`, `committed_at`: lifecycle timestamps
- `state`: `starting`, `recovering`, `ready`, `stopping`, or `debt`
- `state_revision`: optimistic revision for administrative mutation

### Rules

- One `(OS user, host)` may have at most one committed generation and one live authoritative endpoint.
- A runtime record is live only when the endpoint, PID/start identity, executable identity, generation,
  and service-manager job agree.
- A stale record never authorizes signalling or endpoint removal.

### State transitions

```text
absent -> starting -> recovering -> ready -> stopping -> absent
                     |              |
                     +-> debt <-----+
```

Only the explicit install/upgrade transaction or direct service-manager controls change daemon
lifetime. A workflow client cannot transition `absent` to `starting`.

## Daemon Configuration

Non-secret configuration applied to the host runtime authority.

### Fields

- `host_id`, `host_name`
- `hub_address`: the one configured hub endpoint, or empty for local-only operation
- `remote_lanes_enabled`
- `product_overrides`: optional native executable/profile selectors for Codex, Claude, Grok, Qwen
- `state_root`, `runtime_root`: canonical owned roots
- `revision`, `updated_at`

### Rules

- Configuration does not contain credentials.
- Product/profile selectors identify native resources; they do not create another daemon, routing
  space, or collaboration boundary.
- Existing global groups remain the only collaboration visibility/access boundary.

## Managed Attachment

The daemon-owned relationship to one managed native interactive peer or lane.

### Fields

- `attachment_id`: unpredictable launch/lifecycle identity
- `session_id`: authoritative native session ID after adoption
- `kind`: `interactive` or `lane`
- `product`: `codex`, `claude`, `grok`, or `qwen`
- `profile_identity`: canonical non-secret native profile identity
- `cwd`: canonical working directory
- `name`, `name_source`: existing display-name behavior
- `host_id`: local host identity
- `groups`: effective existing global groups, including the existing private parent/session anchors
- `permission_mode`: product-proven effective permission mode
- `native_actor`: product-specific exact process/session/channel evidence
- `connector_identity`: current kernel-attributed connector evidence, if a connector is present
- `launch_capability_hash`: hash of a daemon-issued launch capability; raw capability is never durable
- `state`: lifecycle state
- `revision`, `generation`, `created_at`, `updated_at`
- `delivery_cursor`: durable accepted-delivery progress
- `cleanup_debt_ids`: associated exact cleanup debt

### Identity rules

- `attachment_id` is unique only within the daemon and never becomes the native transcript identity.
- The global peer identity remains the existing `(host_id, session_id)` identity.
- Display names may collide. Visible ambiguity fails and requires an exact address.
- Product/profile/instance/session/host identities select exact resources but do not grant group access.

### State transitions

```text
prepared -> launching -> selecting -> attached -> detaching -> detached
    |           |           |           |
    +-> aborted +-> debt    +-> debt    +-> debt
```

Late-bound products stay in `selecting` until a supported native surface proves the selected session
ID. Only `attached` records participate in discovery or delivery.

## Native Actor Evidence

Product-specific corroboration retained by a managed attachment.

### Common fields

- native process PID plus process start/strong-start when observable
- native session/thread ID and product instance ID
- native profile and cwd
- vendor channel identity (App Server thread, Claude socket, Grok leader/ACP session, Qwen event/input)
- exact parent/ancestry or launch-capability evidence
- last verified time and verification result

### Rules

- A model-supplied ID is never sufficient.
- Missing, changed, conflicting, or unreadable required evidence makes the attachment unavailable or
  cleanup debt; it never selects another actor.
- The daemon revalidates current evidence immediately before signalling, delivery, adoption, or cleanup.

## Connector Session

An ephemeral vendor-required stdio relay connected to the daemon.

### Fields

- `connection_id`
- `role`: `mcp`
- `product`
- `peer_pid`, `peer_proc_start`, `peer_uid`: kernel-observed local client identity
- `attachment_id` or adopted `session_id`
- `protocol_version`
- `connected_at`, `last_request_at`

### Rules

- Connector sessions are not durable authority and are absent from the session catalog.
- A connector exposes only model-facing operations allowed for its attested attachment.
- Connector loss does not delete the attachment. It reconnects to the current daemon generation.
- No connector exposes daemon administration.

## Session Preferences

The existing small durable catalog entry for one managed session.

### Fields

- `session_id`, `product`, `kind`
- `explicit_groups`, `inherited_groups`
- `parent_session_id`, `parent_host_id`, `inherit_parent_groups`
- `always_approve`
- product-specific non-secret resume metadata already required by the adapter
- `revision`, `updated_at`

### Rules

- Existing group normalization, private anchors, inheritance, and resume behavior remain unchanged.
- Omitted resume options restore durable values; explicit options create a new revision.
- Vendor transcript/history is referenced by native session ID and is not stored here.

## Message Delivery

One product-neutral AgentFrame request accepted by the daemon.

### Fields

- `message_id`
- `source_host_id`, `source_session_id`, `source_attachment_revision`
- `operation`: `send`, `multicast`, or `broadcast`
- `requested_targets` or `group`
- `resolved_destinations`: exact host/session identities fixed at admission
- `content`, `summary`, `sent_at`: bounded delivery data, not diagnostic data
- `state`: delivery transaction state
- `destination_results`
- `accepted_revision`, `accepted_at`, `updated_at`

### Rules

- The daemon reconstructs source metadata from the attachment; it never trusts source fields in the
  caller payload.
- All recipients and group access are validated before multicast/broadcast acceptance.
- Acceptance is reported only after this record commits.
- Content may exist only in this bounded operation record until delivered/rejected; it never appears in
  logs, status, traces, or crash reports.
- Each destination is delivered at most once for a committed `message_id`.

### State transitions

```text
validating -> accepted -> dispatching -> delivered
                |             |            |
                |             +-> retryable
                +-> rejected               +-> rejected
```

`validating` is not durable acceptance. Restart recovery resumes only `accepted`, `dispatching`, and
`retryable` records.

## Lane

The daemon-owned durable delegated worker identity.

### Fields

- `lane_session_id`, `name`, `product`
- `parent_host_id`, `parent_session_id`, `parent_groups`, `inherit_parent_groups`
- `groups`, `permission_mode`, `cwd`, native resume metadata
- `state`
- `active_turn_id`
- `native_actor`
- `collection_cursor`, `archive_revision`
- `revision`, `created_at`, `updated_at`
- `cleanup_debt_ids`

### State transitions

```text
prepared -> idle -> running -> idle -> archiving -> archived
              |       |          |
              |       +-> interrupted
              |       +-> failed
              +-> debt           +-> debt
```

An interrupted or failed turn does not implicitly archive the lane. Resume uses the vendor-owned
transcript and a new durable turn.

## Lane Turn

One accepted unit of work for a lane.

### Fields

- `turn_id`, `lane_session_id`
- `parent_context_revision`
- `input_reference`: bounded Agent Sessions-owned input until vendor acceptance
- `dispatch_state`
- `native_turn_identity`
- `terminal_outcome`: `completed`, `interrupted`, or `failed`
- `result_reference`: bounded uncollected result when not already in vendor history
- `terminal_notice_id`
- `collection_revision`, `collected_at`
- `created_at`, `updated_at`

### State transitions

```text
prepared -> accepted -> dispatched -> running -> completed -> collected
                                 |          +-> interrupted -> collected
                                 |          +-> failed ------> collected
                                 +-> debt
```

The `accepted` transition durably records exact parent, group, permission, product, and target context.
Recovery never dispatches one accepted turn twice.

## Federation State

The embedded host-agent relationship with the existing hub.

### Fields

- `host_id`, `host_name`, `hub_address`
- `connection_generation`, `protocol_version`, `advertised_capabilities`
- `state`: `disabled`, `connecting`, `connected`, `backoff`, or `incompatible`
- `advertised_runtime_version`, `advertised_runtime_identity`
- `advertised_products`
- `remote_roster_revision`
- `last_connected_at`, `last_error_code`

### Rules

- Only products proven ready are advertised.
- Hub loss does not alter local groups, peers, or routing authority.
- Host and hub implementations are built from this repository, but their release/build identities may
  differ arbitrarily. Software-version interoperability is decided only from exact
  hub-protocol-version equality.
  Advertised capabilities gate individual remote-lane operations and do not couple releases or reject
  an otherwise protocol-matching host.
- Each service reports its own local build identity for diagnostics. Remote build metadata is not
  required for the handshake, and its presence, absence, or value is never a software-version
  interoperability input.
- A protocol-preserving host or hub upgrade does not cause lifecycle work on the other role. A
  protocol-mismatch handshake accepts no registration, message, or lane and records the exact required
  next action.
- Reconnection republishes the same existing host identity; it does not create a new namespace.
- Remote acceptance obeys the existing destination-local group check and delivery acknowledgement.

## Release Lifecycle Transaction

One staged install, upgrade, removal, or purge transaction driven by the selected host or hub role.

### Fields

- `transaction_id`, `from_release`, `to_release`, `role_selection_path`
- `role`: `host` or `hub`
- `operation`: `install`, `upgrade`, `remove`, or `purge`
- `from_generation`, `target_generation`
- `staged_manifest_identity`
- `role_mutations`: exact prior/current role-owned metadata; host connector entries never contain
  credentials and hub entries never contain remote-host state
- `service_descriptor_identity`, `readiness_hook_identity`
- `service_was_enabled`, `service_was_running`
- `phase`: `preflight`, `staged`, `connectors_prepared`, `pointer_committed`, `restarting`, `ready`,
  `rolling_back`, or `complete`
- `revision`, `last_error_code`

### Rules

- The staged release is immutable and verified before changing authority.
- Host and hub transactions share this schema and transaction engine but never share one release root,
  invocation, lock, current selection, transaction journal, service transition, rollback, or authority
  decision.
- Host hooks may prepare connector changes and apply them from the selected immutable release before
  service restart. Hub hooks may prepare only hub configuration/readiness changes.
- Only one committed authority transition is visible.
- Rollback restores the exact prior role release selection and role-owned state without inspecting credential
  values or mutating the other role.
- Normal removal preserves role configuration and durable metadata; purge requires an exact
  revision-bound plan for that role.

## Lifecycle Debt

Durable proof that an exact operation cannot complete safely yet.

### Fields

- `debt_id`, `operation`, `resource_kind`, `resource_identity`
- `expected_revision`, `observed_revision`
- `cause_code`, bounded non-secret `cause_detail`
- `retry_predicate`
- `prohibited_scope`
- `created_at`, `updated_at`, `resolved_at`

### Rules

- Debt is not permission to delete, signal, overwrite, or broaden scope.
- Retry re-observes exact current state and commits a new revision.
- Status/doctor may expose debt metadata but never message, result, prompt, transcript, or credential
  content.
