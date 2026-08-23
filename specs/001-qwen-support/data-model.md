# Data Model: Qwen Support

## Shared product descriptor

One authoritative descriptor declares each supported product.

| Field | Type | Qwen value / rule |
|---|---|---|
| `product` | enum | `qwen` |
| `label` | string | `Qwen Code` |
| `peer_executable` | basename | `qwen-peer` |
| `lane_executable` | basename | `qwen-peer-lane` |
| `lane_runtime_role` | enum | `qwen-lane` |
| `lane_manager_role` | enum | `qwen-lane-manager` |
| `federation_capability` | enum | `qwen-lane` |
| `mcp_inventory` | capability set | full managed-parent inventory |
| `stdin_option_class` | enum | Qwen lane option parser |
| `supports_native_archive` | boolean | true only after live `session_archive` probe |

Validation:

- IDs and executable basenames are unique.
- Every descriptor row is covered by resume, MCP, runtime, federation, packaging, help, skill, and
  documentation table tests.
- Product-specific behavior is referenced through callbacks; the descriptor does not contain native
  lifecycle logic.

## Qwen profile identity

Durable value that binds a managed peer or lane to Qwen-owned state.

| Field | Type | Validation |
|---|---|---|
| `qwen_home_set` | boolean | Presence-sensitive; distinguishes unset default from explicit path. |
| `qwen_home` | absolute canonical path | Required iff `qwen_home_set`; no relative path or symlink ambiguity. |
| `qwen_runtime_dir_set` | boolean | Presence-sensitive. |
| `qwen_runtime_dir` | absolute canonical path | Required iff set. |
| `profile_fingerprint` | digest | Digest of canonical value-or-absence, not credentials or settings contents. |
| `integration_id` | string | Must be `agent-sessions`. |
| `integration_version` | semver/build identity | Must match the installed Agent Sessions release. |
| `integration_manifest_digest` | digest | Exact installed manifest and required inventory. |

Relationships:

- One peer/lane references exactly one profile identity.
- Resume requires exact equality with the durable identity.
- Credentials, settings, and transcript contents are not copied into Agent Sessions state.

## Qwen readiness report

Non-secret, non-session diagnostic result.

| Field | Type | Notes |
|---|---|---|
| `executable`, `resolved_executable` | path | Exact selected binary and resolved target. |
| `version`, `minimum_version_ok` | string, boolean | Syntax/protocol floor is 0.21.15. |
| `package_identity_ok` | boolean | Selected binary belongs to the expected Qwen package. |
| `profile` | Qwen profile identity | Selected default or explicit profile. |
| `integration_ready` | boolean | Exact plugin/MCP/skills inventory. |
| `auth_state` | enum | `ready`, `unknown`, `unready`; never includes secret values. |
| `workspace_trust` | enum | `trusted`, `untrusted`, `unknown`. |
| `dual_output_contract` | result | Parser and expected launch-time `session_start` contract. |
| `acp_contract` | result | Initialize-only capability probe. |
| `archive_contract` | result | Serve help/capability and native state model. |
| `permission_contract` | result | Requested and corroborated initial native approval mode. |
| `supervisor_ready`, `runtime_ready` | boolean | Existing Agent Sessions infrastructure. |
| `issues` | ordered list | Cause-specific, machine-readable failures. |

Validation:

- Doctor creates no native session, transcript, model turn, MCP child, or integration mutation.
- `ready=true` requires every operation-specific cell; `unknown` is not ready.
- Admission fails when Qwen cannot honor or corroborate the requested initial native approval mode.
- Readiness does not promise that Qwen's mode remains fixed after publication.

## Qwen peer preparation

Product payload in the generalized durable launch transaction.

| Field | Type | Validation |
|---|---|---|
| `version` | integer | Versioned durable schema. |
| `preparation_id` | UUID | Unique transaction ID. |
| `session_id` | UUID | Expected Agent Sessions and native interactive UUID. |
| `product` | enum | Exactly `qwen`. |
| `catalog_revision` | random revision token | Exact rollback/commit CAS. |
| `profile` | Qwen profile identity | Exact resume/cleanup scope. |
| `canonical_cwd` | path | Existing canonical directory. |
| `launch_permission_preference` | tagged value | `native_default`, `non_yolo` (`--approval-mode default`), `yolo`, or `native:<mode>` for an admitted pass-through native mode; presence-sensitive. |
| `lifecycle_identity` | process identity | PID, display start, strong start, root. |
| `input_path`, `event_path` | owned path attestations | Private root, regular 0600 files, baseline/body/inode. |
| `raw_mcp_capability_digest` | digest | Never stores the raw capability in the catalog. |
| `committed` | boolean | False before native corroboration/catalog adoption. |
| `cleanup_debt` | optional debt | Retained on any ambiguous rollback or cleanup. |

State transitions:

```text
absent
  -> prepared
  -> native_starting
  -> native_corroborated
  -> committed/live
  -> retiring
  -> clean

prepared/native_starting/native_corroborated
  -> rollback_pending
  -> clean

any post-prepare state
  -> cleanup_debt
  -> (reconciliation retry) clean
```

No externally discoverable participant exists before `committed/live`.

## Managed Qwen peer

| Field | Type | Validation |
|---|---|---|
| `session_id` | UUID | Same as native Qwen interactive UUID. |
| `name` | string | Durable Agent Sessions name; unique-selector rules apply. |
| `cwd` | canonical path | Matches `session_start`. |
| `profile` | Qwen profile identity | Matches preparation and resume request. |
| `launch_permission_preference` | tagged value | Durable resume default: `native_default`, `non_yolo` (`--approval-mode default`), `yolo`, or exact admitted `native:<mode>`. |
| `initial_native_approval_mode` | string | Corroborated before publication. |
| `current_native_approval_mode` | string/unknown | Best-effort native observation; Qwen may change it normally. |
| `groups` | normalized set | Explicit plus mandatory private group. |
| `delivery_endpoint` | socket address | Private, exact, live session-stable path; `lstat` is a Unix socket, never a symlink. |
| `instance_id` | random identity | Matches host registration. |
| `lifecycle_identity` | exact process identity | Rechecked for message, signal, and cleanup operations. |
| `transport` | input/event path attestations | Exact currently owned files/cursors. |
| `status` | enum | `starting`, `idle`, `busy`, `waiting`, `retiring`. |

Authorization requires simultaneous agreement among preparation/launch record, exact process ancestry,
raw capability, structured native identity, selected profile, and live host registration. Current
native approval mode is not an Agent Sessions identity or routing input.

## Qwen lane state

Durable Agent Sessions lane record; it never conflates lane and native IDs.

| Field | Type | Validation |
|---|---|---|
| `version`, `contract_version` | integer | Qwen lane schema and common lane contract. |
| `thread_id` | UUID | Agent Sessions lane identity. |
| `qwen_session_id` | UUID | Native Qwen transcript identity. |
| `name`, `cwd`, `profile` | value | Exact durable launch context. |
| `parent_context` | Parent Context | Exact source host/session/product/instance and runtime. |
| `groups` | normalized set | Child anchor + parent anchor + optional inherited groups. |
| `persistent`, `auto_archive` | boolean | Common lane ownership policy. |
| `launch_permission_preference` | tagged value | Durable choice: `native_default`, `non_yolo` (`--approval-mode default`), `yolo`, or exact admitted `native:<mode>`. |
| `initial_native_mode` | string | ACP-corroborated at launch. |
| `current_native_mode` | string/unknown | Mutable Qwen-owned state when reliably observable. |
| `status` | enum | See transition graph below. |
| `active_turn_id` | UUID/empty | At most one active native prompt. |
| `pending_turn_ids` | ordered UUID list | Manager-owned queue; no concurrent ACP prompt calls. |
| `latest_turn_id`, `collected_turn_id` | UUID/empty | Exactly-once collection cursor. |
| `terminal_outcome`, `exit_code`, `error` | terminal result | Normalized common contract. |
| `manager`, `worker` | process identities | PID/start/strong-start and exact roots. |
| `tool_roots` | ownership-ledger refs | Detached shell process groups. |
| `native_archive_state` | enum | `active`, `archiving`, `archived`, `unarchiving`, `unknown`. |
| `archive_helper` | optional helper lease | Ephemeral serve process tree and token digest. |
| `notice` | delivery ledger | Exact parent target, attempts, sent time, debt. |
| `cleanup_debt` | debt list | Typed retryable incomplete actions. |

State transitions:

```text
starting -> idle -> active -> idle
                   |  \
                   |   -> interrupted -> idle
                   -> failed

idle/failed/interrupted/completed
  -> retiring -> native_archiving -> archived

archived
  -> native_unarchiving -> resuming -> idle

any non-archived state
  -> cleanup_debt
  -> (reconcile) prior terminal state or archived
```

Rules:

- A second prompt is queued, never sent concurrently to ACP.
- Qwen's native permission controls remain available; later mode changes are not Agent Sessions
  policy faults and do not change messaging, group, or parent authority.
- One turn has one terminal observation and one successful collection.
- Archive requires terminal turn, closed admission, retired exact worker/tool tree, and native archive
  success.
- Resume requires exact native unarchive and exact `session/resume`; failed partial resume re-archives
  or retains debt.

## Qwen turn

| Field | Type | Validation |
|---|---|---|
| `turn_id` | UUID | Unique within lane. |
| `qwen_session_id` | UUID | Matches lane native identity. |
| `request_digest` | digest | Detects duplicate/replayed requests without storing authority. |
| `status` | enum | `queued`, `active`, `completed`, `interrupted`, `failed`. |
| `agent_text` | bounded text/result reference | Aggregated from native message chunks. |
| `stop_reason` | native string | Mapped to common outcome. |
| `outcome`, `exit_code`, `error` | terminal result | Stable common contract. |
| `terminal_revision` | random revision | Exactly-once observation/collection CAS. |
| `collected_at` | timestamp/empty | Set once. |

## Ephemeral archive helper lease

| Field | Type | Validation |
|---|---|---|
| `operation` | enum | `archive` or `unarchive`. |
| `qwen_session_id` | UUID | Exact native target. |
| `profile`, `cwd` | exact context | Same as lane record. |
| `lifecycle_root` | private path | 0700, preparation-owned. |
| `server_identity` | process identity | Exact serve PID/start/strong-start. |
| `child_identities` | process identity set | Includes preheated ACP child. |
| `endpoint` | loopback URL | OS-assigned port; loopback only. |
| `token_digest` | digest | Raw bearer remains only in private memory/file. |
| `capability_version` | string | Must prove v1 `session_archive`. |
| `request_revision` | random token | Retry/idempotence correlation. |
| `state` | enum | `starting`, `ready`, `requested`, `confirmed`, `stopping`, `clean`, `debt`. |

The Agent Sessions lane state changes only after `confirmed` and `clean`.

## Tool-root ownership ledger

Shared by Grok and Qwen with product callbacks.

| Field | Type | Validation |
|---|---|---|
| `version`, `product` | value | Known descriptor and schema. |
| `manager_identity`, `worker_identity` | exact identities | Strong-start required. |
| `root_identity` | exact process identity | Detached process-group leader. |
| `raw_capability_digest` | digest | Bound to manager launch. |
| `intent_revision` | random revision | Persisted before wrapper exec. |
| `admission_open` | boolean | Must close before cleanup. |
| `created_paths` | path attestations | Private exact roots/wrappers only. |
| `cleanup_state` | enum | `owned`, `stopping`, `clean`, `debt`. |

## Qwen lifecycle debt

| Field | Type | Validation |
|---|---|---|
| `debt_id`, `revision` | UUID/random revision | Idempotent retry key. |
| `owner_kind`, `owner_id` | enum + UUID | Peer, lane, turn, tool root, or archive helper. |
| `operation` | enum | Signal, wait, unregister, unlink, rollback, archive, unarchive, notice. |
| `expected_identity` | typed proof | Exact process/path/catalog/native proof. |
| `last_observation` | typed observation | No ambiguous free-text authority. |
| `attempts`, `last_error`, `updated_at` | diagnostics | Does not weaken proof. |
| `terminal_when_clean` | state | Deterministic convergence target. |

Debt is removed only after the exact operation is proven complete. Changed or unknown identity retains
debt and preserves the artifact.
