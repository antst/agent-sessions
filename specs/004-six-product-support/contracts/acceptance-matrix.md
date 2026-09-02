# Contract: Product Acceptance Matrix

## 1. Capability Rows

Every descriptor expands into cells for every declared capability. A product
cannot be `general` unless all applicable cells pass on both Linux and macOS.

### PEER cells

1. wrapper prepare/exec and exact attachment evidence;
2. external discovery and group isolation;
3. outbound direct/multicast/group message;
4. idle inbound message visibly wakes a model turn;
5. busy inbound message steers or durably queues with truthful receipt;
6. native rename reflected externally;
7. kill/resume preserves exact native ID and reconnects;
8. daemon restart reconnect;
9. component or native-registry/server crash behavior;
10. unmanaged bare native launch remains opt-out.

### LANE cells

1. doctor/presence/version/features;
2. start/run/wait/collect;
3. busy steer and unsupported-steer queue fallback;
4. interrupt and timeout;
5. exact resume and daemon restart recovery;
6. archive/idempotent archive/cleanup debt;
7. auto-archive and persistent policy;
8. local message and terminal-notice delivery;
9. federated lifecycle and destination capability enforcement;
10. crash at each ledger/native-I/O boundary.
11. when `DeferredSessionBinding` is declared: fresh Open remains unbound and
    creates no native session/job; first StartTurn binds atomically with exact
    native acceptance; unbound restart requeues by LaneID; possible write stays
    ambiguous until authoritative reconciliation; resume and bound recovery
    always use the exact immutable native ID.

### PARENT cells

1. exact product-native session identity from registered tool/context;
2. false model-supplied identity rejected;
3. list peers and send message;
4. launch same-product and cross-product lane;
5. receive visible terminal notice;
6. skills/instructions/commands installed once;
7. permission prompt/allowlist behavior;
8. rename/resume retains parent authority.

### INSTALL/OPERATIONS cells

1. absent product skip;
2. exact install/update/rollback/removal;
3. user-modified native registration preservation;
4. doctor/roster/catalog projection;
5. release archive inventory and prebuilt install;
6. systemd/launchd environment and service restart;
7. secret-free diagnostics/evidence.

## 2. Product-Specific Mandatory Cells

- OpenCode: plugin reconnect, exact rename route, `noReply`, queued prompt idle
  semantics, permission reply.
- Kilo: two isolated authenticated server/full-attach pairs, zero cross-delivery,
  `/tui/*` peer route versus session lane route, background-process/MCP parity,
  and explicit rejection of `attach --mini` as a managed peer surface.
- Pi: RPC ready-version negotiation, `agent_settled`, native
  `PI_SESSION_ID` cross-check, restricted default tool allowlist.
- OMP: native interjection envelope retains Agent Sessions framing, extension
  env identity, foreground/async spawn behavior, explicit approval mapping.
- CodeBuddy: product-owned peer registry plus socket-to-PID/executable/ancestry
  correlation, constant-CSRF-header semantics, stale-row/port-reuse rejection,
  cross-target isolation, daemon restart re-discovery; separately, authenticated
  Agent Sessions-owned lane-server handling and offline OpenAPI drift. Tencent
  model path is the only permitted pending cell and keeps support experimental.
- DSH lane: exact native tuple/pnpm, ACP new and exact resume, real turn,
  interrupt, cancel-as-notification, archive, and truthful busy rejection.

## 3. Shared Gates

Every candidate commit runs:

- focused contract/unit/integration tests;
- `./scripts/test` normal and race;
- `go vet` and repository-managed lint;
- four supported builds;
- install/removal transaction tests;
- live product cells applicable to available credentials;
- real Linux and physical macOS acceptance;
- catalog/projection drift check;
- exact commit/tree/toolchain/native-version evidence manifest.

## 4. Credit Rules

- Mock model providers are allowed; mocked product protocol implementations do
  not earn real-product credit.
- Fixed sleeps without state predicates, skipped assertions, loose process
  matching, TTY scraping, or transcript-only identity do not earn credit.
- A red first genuine cell stops wider scope until root cause is classified.
- CodeBuddy's account-gated cell is reported as pending, never as pass.
- DSH is credited only for the exact installed tuple recorded by evidence.
