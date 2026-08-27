# Contract: Durable Storage, Recovery, and Legacy Migration

## Canonical owned roots

### Configuration

- Linux/macOS default: `${XDG_CONFIG_HOME:-$HOME/.config}/agent-sessions/config.json`
- Contains non-secret daemon, host, hub, product selector, and feature configuration
- Preserved by normal removal

### Durable state

- Default: `${XDG_STATE_HOME:-$HOME/.local/state}/agent-sessions/`

```text
agent-sessions/
├── runtime.json
├── install/
│   ├── current.json
│   └── transactions/<id>.json
├── migration/
│   ├── current.json
│   ├── candidates/<id>.json
│   └── debt/<id>.json
├── catalog/
│   ├── sessions.json
│   └── names/<session-key>.json
├── attachments/<attachment-id>/
│   ├── state.json
│   ├── deliveries/<message-id>.json
│   └── debt/<id>.json
├── lanes/<product>/<session-key>/
│   ├── state.json
│   ├── turns/<turn-id>.json
│   ├── notices/<notice-id>.json
│   └── debt/<id>.json
└── federation/state.json
```

Exact directory sharding may change during implementation, but one schema and runtime authority own all
paths. Product/profile identities remain record fields, not separate runtime roots or daemon instances.

### Ephemeral runtime

- Linux: `$XDG_RUNTIME_DIR/agent-sessions/`
- macOS: `/tmp/agent-sessions-$UID/`
- Contains only the daemon lock, one Unix socket, and exact ephemeral record needed to corroborate them
- Removed on verified shutdown/recovery; never stores credentials or accepted work

## Storage rules

- Agent Sessions records have an explicit schema version and resource revision.
- Writes use same-directory temporary files, file sync, atomic rename, and parent-directory sync where
  required for crash durability.
- Compare-and-swap mutations verify the expected revision immediately before commit.
- File permissions are `0600`; owned directories are `0700`.
- Readers reject symlinks, changed file types, wrong UID, unbounded files, malformed records, and
  unsupported schema versions.
- A single corrupted entity becomes exact debt when isolation is safe; corruption of runtime authority,
  catalog security rules, or install/migration journal fails daemon readiness.
- Accepted message/turn content exists only in its bounded operation record and never in logs/status.
- Vendor transcript content is referenced by native ID and never copied into this root.

## Recovery ordering

On every daemon start:

1. validate runtime root and acquire the single-daemon lock;
2. load and validate configuration and runtime schema;
3. recover incomplete install or migration journal before opening admission;
4. open the session catalog and shared group rules;
5. load attachments, deliveries, lanes, turns, notices, and cleanup debt;
6. reconcile exact native actors and vendor channels through each adapter;
7. restore local routing and only ready product capabilities;
8. reconnect the configured hub and republish the same host identity;
9. write the committed runtime generation and open admission;
10. publish the synthetic Claude service row only if required and fully corroborated.

Recovery never redispatches a turn or delivery whose durable state is ambiguous. It records retryable
debt until the exact prior outcome is established.

## Legacy source inventory

The first migration knows the exact prior layouts shipped by Agent Sessions, including:

- `${XDG_STATE_HOME:-$HOME/.local/state}/claude-code-peer/`
  - `native-runtime-path`
  - `sessions/`
  - `profiles/*/supervisor.json`
  - `interactive-owners/`, `retired/`
  - Codex/Claude/Grok/Qwen lane state, turns, notices, wakes, logs, worktrees, and debt
  - Grok launch records and per-session delivery state
- `${XDG_STATE_HOME:-$HOME/.local/state}/agent-sessions/agents/<host>/`
  - host session catalog
  - name projection
  - launch preparation journals
  - cleanup debt and agent service records
- historical runtime roots shipped on Linux and macOS
  - `$XDG_RUNTIME_DIR/codex-claude-peer-$UID`
  - `/tmp/ccp-$UID`
  - prior Darwin `TMPDIR` spellings
  - peer-federator runtime roots and `agent.sock`
- legacy systemd/launchd host-agent jobs and durable service records

The inventory is a closed list derived from shipped releases. It does not search arbitrary process
names or broad home-directory patterns.

## Candidate evidence

Each process/endpoint candidate records:

- exact source record and revision;
- runtime implementation/version identity;
- PID plus process start/strong-start;
- executable and argv contract;
- Unix socket path, type, owner, and responsive status identity;
- profile/product/session relationships;
- service-manager unit/label identity where applicable.

Classification requires corroborating sources. These never suffice alone:

- scalar shim/peer/lane counts;
- process name or substring;
- PID liveness;
- socket/path existence or shape;
- model-supplied ID;
- a stale service file.

## Migration phases

### 1. Inventory

Read all known legacy durable sources and live exact endpoints without mutation. Produce a bounded
candidate list and adoption plan.

### 2. Classify blockers

- `active_managed_blocker`: an exact managed peer or lane is still active; name it and require the
  operator to close it.
- `quiescent_authority`: an exact legacy Agent Sessions authority is live but owns no active managed
  peer or lane and may be stopped through its supported lifecycle.
- `stale`: exact alleged owner is proven absent; stale count/path cannot block.
- `unknown` or `conflicting`: fail closed as retryable migration debt.

No state changes occur while blockers remain. Migration does not transfer a live attachment, peer, or
lane turn from a legacy authority.

### 3. Stop the old authority

After the global peer/lane quiescence gate passes, close admission where supported and stop every exact
legacy Agent Sessions authority through its supported lifecycle. Re-attest its process and endpoint
identity immediately before the stop and verify exit. Do not stop native vendor processes. The old and
new host authorities are never concurrently accepting work.

### 4. Adopt durable state

Copy/transform only Agent Sessions-owned records into a staged new schema:

- session preferences, names, global groups, parent context, permissions;
- dormant session metadata and completed/interrupted lane history, but no live attachment;
- accepted messages and delivery cursors;
- lanes, turns, terminal states, collection cursors, notices, archive state;
- host/hub configuration, capability policy, cleanup debt;
- install and migration provenance.

Do not copy vendor transcripts, credentials, settings, caches, or native history databases.

Validate the complete staged state before selecting it.

### 5. Commit new authority

Atomically select the staged state/release generation and start the service. The daemon remains
`recovering` and does not accept work until the legacy retirement gates complete. There are no live
legacy managed attachments to reconstruct.

### 6. Retire legacy authorities and endpoints

Immediately re-attest each stopped legacy process record, service, socket, lock, and disposable file.
Stop/remove only the exact matching Agent Sessions target. A changed identity becomes debt and prevents
ready status. Preserve unrelated native processes and all vendor data.

### 7. Ready and retain provenance

After exactly one daemon endpoint remains and every adopted record validates, commit `ready`. Retain a
bounded migration result and unresolved debt metadata. Legacy state roots may be retained as
revision-bound migration provenance until explicit cleanup; they are never active authority.

## Migration rollback

Before the new authority becomes ready:

- stop the candidate daemon if it started;
- restore the exact prior release/state pointer;
- restore prior connector and service-manager state;
- restart a previously usable stopped legacy authority only when its exact identity and journal
  revision still match;
- report any changed identity as debt rather than guessing.

After the new generation is ready, migration does not silently roll back to split authorities. Later
failures use normal daemon recovery.

## Restart reconciliation

### Attachments

- reconstruct exact vendor actor evidence;
- mark attachments unavailable when required evidence is absent or ambiguous;
- retain durable preferences for later native resume;
- never create a managed attachment from plugin presence alone.

### Deliveries

- resume committed `accepted`, `dispatching`, or `retryable` records by message ID;
- never retry a destination already durably delivered;
- preserve ambiguous outcomes as debt.

### Lane turns

- reconnect through a supported native identity when possible;
- otherwise commit exactly one evidence-approved `interrupted` terminal outcome;
- never dispatch one accepted turn twice;
- preserve the idempotent collection cursor.

### Federation

- reconnect using the same host identity;
- preserve local routing during hub outage;
- republish only live/ready local peers and products;
- do not fall back to a second local carrier or agent.

## Cleanup debt

Cleanup debt records the exact expected/observed identity and prohibited mutation scope. Reconciliation
retries only after fresh observation. A debt record cannot authorize broad directory removal, PID
signalling, registry repair, or vendor-state edits.

## Normal removal and explicit purge

Normal removal preserves this durable state root and configuration root. It removes only installed
release/service/connector artifacts and verified ephemeral runtime files.

Explicit purge accepts a revision-bound plan listing exact Agent Sessions-owned paths. It refuses:

- any symlink/hard-link ambiguity or changed file type;
- wrong ownership or root escape;
- a running service or active managed attachment/lane;
- any vendor profile, credential, transcript, native session, or unenumerated path.

Interrupted purge records remaining targets and is idempotent.
