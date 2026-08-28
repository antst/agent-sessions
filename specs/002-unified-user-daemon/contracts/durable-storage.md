# Contract: Durable Storage and Recovery

This contract defines the unified daemon's own durable state. Version 0.3 is a greenfield boundary;
this contract does not describe or authorize discovery, adoption, conversion, or retirement of any
pre-unification Agent Sessions state.

## Canonical ownership

Each OS user-host has one canonical Agent Sessions configuration root, one state root, and one runtime
root. The service-manager-owned `agent-sessions` process is the sole writer authority for daemon
runtime records. Host and hub release roots, selections, locks, journals, services, removal, and purge
records are disjoint even when installed under the same prefix.

Daemon-owned records include:

- exact runtime generation and process authority;
- configuration and product overrides;
- attachments and attachment preferences;
- accepted deliveries and destination outcomes;
- lane sessions, turns, notices, collection, and archive state;
- federation connection state and remote roster revision;
- install, upgrade, removal, and purge transactions; and
- exact retryable lifecycle debt.

Vendor credentials, profiles, settings, transcripts, native history, and provider-owned bookkeeping
are never daemon state.

## Revision and identity rules

Every durable mutation uses the record's schema version and current revision. Exact authority records
also bind generation, process identity, runtime identity, service identity, and canonical path where
applicable. Writes use same-root temporary files, atomic publication, and parent-directory durability.
Unsafe paths, unsupported schemas, revision conflicts, changed identities, and incomplete records fail
closed without broad cleanup authority.

## Startup recovery order

Before opening admission, one daemon generation:

1. validates canonical roots and exclusive runtime authority;
2. loads configuration;
3. recovers unfinished unified release and daemon-owned transactions;
4. opens catalogs;
5. reconstructs attachments, lanes, turns, and lifecycle debt;
6. corroborates external native actors through their adapter contracts;
7. restores local delivery;
8. reconnects federation when configured; and
9. commits the new generation before publishing readiness.

Recovery never dispatches accepted work twice, fabricates a native identity, edits vendor history, or
starts another Agent Sessions authority.

## First-install boundary

The operator stops all pre-unification Agent Sessions processes and services and removes or archives
their Agent Sessions-owned state and installation roots before the first version-0.3 install. The
installer does not inspect the old topology and exposes no compatibility command or fallback.

After first installation, ordinary version-0.3 upgrades and rollbacks preserve this unified state
through the same durable role transaction. Removal preserves daemon configuration and metadata;
deletion requires an explicit revision-bound purge. Vendor-owned stores remain excluded from both.
