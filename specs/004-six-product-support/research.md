# Research: Six-Product Symmetric Support

This document is the fresh implementation research record for the released
`origin/main` base. It incorporates hands-on product evidence produced by the
visible `fable-architect` Agent Sessions peer and an independent source audit.
`[F]` means measured or source-verified fact, `[D]` means the jointly chosen
design, and `[G]` means an implementation gate that must be proven before the
dependent contract freezes.

## 1. Base and Scope

### Decision

Build from `origin/main` commit
`679fe9d3068b6362df867f8d78ce6708c4ce1342` (`v0.3.0`).

### Rationale

- [F] The apparent 83-commit `feature/unified-user-daemon-v2` history was
  squash-merged. Direct tree comparison shows it lacks later release-hardening
  changes and removes current release evidence.
- [F] `origin/main` contains the unified daemon, product-neutral durable
  attachment/lane/turn engines, release gates, and the physical macOS evidence
  used for v0.3.0.
- [D] Full support means PEER × LANE × PARENT parity, not merely a headless
  process or ACP child.

### Alternatives considered

- Continue from the dirty pre-squash feature branch: rejected because it is not
  the released source of truth and contains unrelated user work.
- Build only lane adapters first: rejected because it would create misleading
  product support and duplicate later peer/parent architecture.

## 2. Tested Native Baselines

| Product | Tested baseline | Evidence status | Release status |
|---|---:|---|---|
| OpenCode | 1.18.25 | Peer, lane, parent hands-on | General after acceptance |
| KiloCode | 7.5.6 | Peer, lane, parent and exact isolated full-attach route hands-on | General after acceptance |
| Pi Coding Agent | 0.84.4 | Peer, lane, parent hands-on | General after acceptance |
| Oh My Pi | 18.0.11 | Peer, lane, parent hands-on | General after acceptance |
| CodeBuddy | 2.143.0 | Protocol, peer wake, parent hands-on; real model reply account-gated | Experimental until Tencent cell |
| DeepSeek Harness | exact 0.1.2-alpha.3 tuple | ACP and credentialed success path hands-on | General only for exact tuple |

[D] Doctor uses both a version policy and a feature probe. A matching version
without the required behavior is not ready; a newer version is not assumed
compatible. DSH is an exact tuple rather than a semver range.

## 3. Current Architecture Findings

- [F] `daemon.AttachmentAdapter` is already a sound boundary for exact native
  evidence, but lane execution and delivery are product switches in
  `cmd/agent-sessions`.
- [F] `internal/productcatalog` and `internal/federator` contain duplicate
  product inventories.
- [F] release/install scripts repeat the four-product inventory.
- [F] input arriving during a running lane is retained in volatile
  `laneActor.pending`; daemon restart loses it.
- [F] `daemon.sock` is deliberately a bounded one-request/one-response local
  control protocol and is unsuitable for long-lived product components.
- [F] the federation hub filters capabilities through the closed product
  catalog, though the existing wire already carries product IDs and capability
  strings needed by all six products.
- [F] live unified-daemon code consumes a small subset of exports colocated in
  a much larger unreachable legacy bridge/federator implementation.

## 4. Runtime Registry

### Decision

Keep `internal/productcatalog` data-only and introduce
`internal/productruntime` as the explicit runtime capability registry. The
single composition root is `cmd/agent-sessions/product_registry.go`; package
`init` registration is forbidden.

Each runtime product may provide:

- `PeerDriver`
- `MessageDriver`
- `LaneDriver`
- `ParentAttester`
- required `DoctorProbe` for every visible product

The registry fails construction if catalog capabilities and supplied drivers
do not agree. Product switches are permitted only inside the relevant product
package and the one explicit composition root.

### Rationale

- [D] Small optional capabilities prevent a universal interface from
  inventing semantics unsupported by a product.
- [D] One explicit composition root preserves auditability and makes missing
  integrations a startup/test failure rather than an import-order side effect.
- [D] Existing products migrate through adapters or wrappers around their live
  implementations; the daemon lifecycle remains authoritative.

### Alternatives considered

- Callbacks in the data catalog: rejected because it would couple packaging,
  launcher, and daemon imports.
- `init`-based registration: rejected because availability would depend on
  hidden imports and tests could silently omit products.
- A single large `ProductAdapter`: rejected because it would force false
  symmetry and encourage product-name switches within methods.

## 5. Lane Driver and Durable Input Ledger

### Decision

Freeze a `LaneDriver` with an optional `Steer` operation before any product
owner implements it. `ErrUnsupportedSteer` causes the shared lane engine to
queue the same durable receipt for a later turn.

Accepted content uses a separate private spool and a durable metadata ledger:

1. securely write and fsync the bounded spool object;
2. commit the receipt and digest in daemon state;
3. acknowledge acceptance to the caller;
4. commit `dispatching` before any native I/O;
5. commit exact native acceptance after the native API acknowledges it;
6. retire content after proven injection or explicit terminal retirement.

Crash after native I/O but before durable acknowledgment becomes
`ambiguous`. The daemon never blindly replays unless the product protocol
proves idempotent replay for the exact operation.

### Rationale

- [F] Pi/OMP, OpenCode/Kilo, and CodeBuddy expose genuine mid-turn input.
- [F] DSH ACP rejects a second prompt while busy; the durable queue is the
  correct fallback.
- [F] the current volatile queue is already a restart-loss bug for the original
  four products.
- [D] catalog metadata remains bounded and non-content-bearing; prompt content
  receives separate retention and cleanup rules.

### Alternatives considered

- Keep `laneActor.pending`: rejected because acknowledged input can disappear.
- Store prompt bodies in the catalog: rejected because it changes the catalog's
  size/privacy contract and couples every state commit to large content.
- Replay every `dispatching` receipt after crash: rejected because native APIs
  are not uniformly idempotent and duplicate instructions are unsafe.

## 6. Component Broker and Authentication

### Decision

Add a long-lived sibling socket (`component.sock`) using shared bounded local
framing. A managed wrapper supplies an ephemeral one-time bootstrap capability;
the daemon also validates same-user kernel peer credentials, exact process
start, ancestry, and expected attachment evidence. Reconnect after a generation
change rechecks kernel/process evidence against the durable attachment. A
plugin installed without a managed wrapper is inert.

Do not add Ed25519 challenge keys.

### Rationale

- [F] the one-shot control socket has different admission, timeout, and
  idempotency semantics.
- [D] under the declared same-UID trust domain, a local attacker capable of
  reading a component's memory can also read a private key; asymmetric signing
  adds ceremony without a named mitigated adversary.
- [D] bootstrap capability plus kernel/exact-process evidence prevents
  cross-session confusion and PID-reuse mistakes while preserving restart.

### Credential handling note

Launch commands may carry an ephemeral bootstrap secret in a separately marked
redacted environment field. Durable native references, catalog rows, logs, and
diagnostics never contain that value. This is distinct from the durable
bootstrap capability identifier/hash.

## 7. Shared Transport Families

### Decision

Share only transport mechanics; keep product semantics typed:

1. `internal/localtransport` + `internal/component`: component framing,
   peer credentials, bounded replay, heartbeats, and the shared JS/TS client.
2. `internal/structuredprocess`: exact child/process-group ownership, bounded
   framed stdio, ordered writes, cancellation, and exit evidence.
3. `internal/productserver`: literal-loopback-only HTTP, no redirects or proxy,
   bounded bodies/decompression, memory-only auth, event-stream reconnect, and
   owned-server supervision.
4. Typed `pifamily` RPC above `structuredprocess`; typed DSH ACP separately.
5. Typed `opencodefamily` shared operations above `productserver`; explicit
   OpenCode/Kilo differences remain in product packages.
6. Typed CodeBuddy jobs/workers/reply client above `productserver`; no universal
   route DSL.

### Rationale

The products share framing, supervision, and local-server safety but differ in
identity, wake, completion, permission, and recovery semantics. Hiding those
differences behind untyped maps would move switches into configuration and
make safety review harder.

## 8. Product-Specific Decisions

### OpenCode

- [F] in-process plugin SDK can announce exact sessions, inject via
  `promptAsync`, observe events, inject `shell.env`, and register a parent tool.
- [D] peer uses the component broker; lane uses a loopback server with per-lane
  memory-only password and SSE completion.
- [G] acceptance confirms the exact rename route, `noReply`, and idle semantics
  when prompts queue.

### KiloCode

- [F] Kilo inherits part of OpenCode's server/plugin architecture, but TUI peer
  delivery uses `/tui/*`; server-owned lane prompts use session routes.
- [F] S1 proved two isolated authenticated server/full-attach pairs with exact
  zero-cross routing, background-process attribution, MCP, rename, and resume.
  The selected peer topology is one server plus one full attach TUI per peer;
  `attach --mini` is not `/tui/*`-messageable and is unsupported.
- [D] reuse only the operations proven identical to OpenCode.

### Pi and OMP

- [F] both expose documented structured RPC and extension APIs supporting
  session identity, idle wake, busy steering, events, abort, resume, and parent
  tools.
- [D] one shared extension client and RPC core uses a quirk table for Node/Bun,
  paths, native environment, permissions, and OMP's interjection framing.
- [F] Pi lacks a native approval prompt and therefore defaults to a restricted
  tool allowlist; OMP maps its explicit approval modes.

### CodeBuddy

- [F] each interactive worker exposes a documented per-session HTTP endpoint;
  reply wakes an idle TUI. Its registry exposes exact session/PID/URL but no
  password; the endpoint uses constant `X-CodeBuddy-Request: 1` CSRF gating.
- [D] the managed wrapper adopts the exact TUI through registry plus fresh
  socket-to-PID, executable, start, and ancestry evidence. The typed peer
  client re-discovers and re-attests after daemon restart; no sidecar exists.
- [D] an Agent Sessions-owned CodeBuddy lane server is a distinct surface: the
  daemon enables password authentication and holds that generated secret only
  in the supervised runtime client.
- [D] build the complete adapter and offline acceptance; keep support state and
  federation advertisement experimental until the Tencent-authenticated
  model-turn cell passes.

### DeepSeek Harness

- [F] the working combination is an exact 0.1.2-alpha.3 CLI/ACP/profile/plugin
  tuple; npm installation exhausted an 8 GiB environment while pnpm succeeded.
- [F] ACP busy prompt rejects, cancel is a notification, the projection cache
  is not a real-time liveness signal, the sandbox masks `/tmp`, and native
  `DSH_SESSION_ID` is available to tools.
- [D] use a Cordis protocol-driver plugin for peers and a typed ACP child for
  lanes. Enforce a durable exclusive native-session lease because DSH does not
  prevent cross-process dual ownership.
- [G] S2 proves the chosen Cordis parent facade in a real pinned profile.

## 9. Federation

### Decision

Use one uniform federation protocol 4. The exact first-frame version must match
before registration; an N+1 participant against an N hub is rejected as a
whole. Every accepted client receives the same complete roster. Lane
capability strings remain bounded opaque values and exactly one must be present;
the destination runtime registry decides whether product and capability are
ready.

Retain and document the trusted-network assumption. Authentication and
encryption are separate security work.

### Rationale

- [F] all six products fit existing `Product` and `Capabilities` wire fields;
  no additive struct field is required.
- [F] the federation path does not use `DisallowUnknownFields`.
- [D] this is an unreleased greenfield boundary, so exact version rejection is
  the forward-upgrade path and released-binary interoperation is not retained.
- [D] future range negotiation may be introduced only in an explicit
  first-frame contract that selects one complete version before registration.
- [D] destination validation remains the final authority.

### Required test debt closure

- Port command-level hub integration tests from the legacy federator to the
  live `internal/federation` hub.
- Add malformed/oversized hostile-client fuzz, exact mismatch/N+1 rejection,
  identical complete-roster, amplification, and opaque-capability tests.
- Preserve generation fencing and group/parent attestation tests.
- Fix the macOS hub service environment projection while this surface changes.

## 10. Product Catalog, Install, and Release Projection

### Decision

The Go product catalog is the only authored inventory. A staged
`agent-sessions catalog --json` command emits a deterministic projection.
Release packaging includes one `integrations/` tree. `releaseinstall.Registry`
derives install/remove plans; shell scripts consume the binary projection and
contain no product arrays.

Install transactions stage, validate, register exact applicable native
integrations, atomically switch, and roll back to captured prior identities.
DSH profile/plugin operations carry explicit ownership receipts and never
rewrite arbitrary profiles.

### Rationale

- [F] product identity currently exists in two Go catalogs and several shell
  arrays.
- [D] executable projection keeps behavior and packaging on one authored
  source while preserving deterministic release evidence.
- [D] absent products are optional integrations, not host-install failures.

## 11. Legacy Code Decision

### Decision

Phase-0 static analysis produces the exact live export and unreachable file
sets. If deletion is bounded and the complete gate stays green, remove that
set as a standalone prerequisite change. Otherwise:

- extract live session/MCP/native helpers to focused packages;
- collapse the duplicate product catalog;
- enforce no new integration imports into legacy bridge/federator code;
- file full excision as follow-up work.

In both cases, rewrite `docs/ADAPTER-PROTOCOL.md` for the unified daemon,
runtime registry, component broker, durable input ledger, and current
acceptance contract.

### Rationale

This makes legacy disposition explicit without placing an unbounded 30–40k
line deletion on the critical path. No dead code blocks the six products; its
co-location with live helpers is the relevant problem.

## 12. Truth Spikes Before Interface Freeze

Each spike uses a real pinned product binary and emits reproducible evidence;
mock model providers are allowed, mocked product protocols are not.

| Gate | Exit criteria |
|---|---|
| S0 Base/federation | historical base equals 679fe9d and decoder accepts additive fields; the protocol-3 compatibility decision is superseded by owner-authorized T125 uniform protocol 4 |
| S1 Kilo | PASS: two isolated authenticated serve+full-attach pairs receive zero cross-delivery and pass `/tui/*`, busy queue, events, background-process attribution, MCP, rename/resume; `--mini` is not peer-messageable |
| S2 DSH | PASS: exact tuple boots; Cordis enumerates sessions; real idle followup and busy steer; native registered-tool parent facade selected; HOME/XDG socket and `DSH_SESSION_ID` verified; ACP cancel is notification and projcache is not liveness |
| S3 CodeBuddy | RED reconciled: peer endpoint has no password/component/sidecar; wrapper Adopt/Refresh re-attests registry claim through socket owner and process ancestry; lane endpoint is separately AS-owned and password-authenticated; Linux negative isolation cells pass, physical macOS socket-owner cell remains |
| S4 Component identity | PASS: OpenCode/Kilo `shell.env` and Pi/OMP extension/RPC/tool contexts match authoritative native IDs; one component-v1 vocabulary works without weakening evidence; inert bootstrap and secret redaction verified |
| S5 Legacy | PASS: three legacy entrypoints are unreachable, 32 bridge exports remain live, no production file is independently deletable; extract-and-freeze with a shrinking no-new-import baseline |
| S6 Catalog projection | PASS: deterministic ten-product projection digest `9a0b001b5b92d1c0123d7ea1c62dee3ead70f545c07117cc95f1096a7fea1702`; one token grammar; exact DSH tuple; separate CodeBuddy peer/lane surfaces; drift rejected |

## 13. Verification and Ownership

- A shared OpenAI-compatible mock-provider fixture supports streaming,
  tool-call emission, slow turns, cancellation, and deterministic final text.
- Every product receives focused typed-client tests and Linux/macOS real-product
  cells for every declared capability.
- Foundation owners do not edit central catalog, daemon state, federation,
  coordinator, installer, or composition files in parallel.
- Product owners edit only their family/product/assets/fixtures. The central
  integrator owns registry composition, coordinator dispatch, catalog
  projection, and pre-agreed MCP/component hook hunks.
- Existing Grok unconditional permission widening is fixed or documented as an
  explicit narrow exception during migration; unknown session-tool entrypoints
  fail loudly rather than defaulting to a Claude label.

## 14. Explicitly Out of Scope

- Federation TLS/authentication and untrusted-network deployment.
- Windows.
- Ambient attachment of already-running pluginless/unmanaged products.
- Full deletion of the entangled legacy bridge/federator tree; S5 requires
  focused live-helper extraction and freezes new imports in this milestone.
- Copilot and other surveyed lane-only or conditional products.
- CodeBuddy's Tencent-authenticated GA model-turn credit until an account is
  available; this does not excuse any implementation or offline protocol cell.
