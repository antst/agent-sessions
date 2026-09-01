# Contract: One-Shot Native Launch Handoff

## 1. Purpose and authority

`NativeCommand` is an in-memory result of `PeerDriver.BuildLaunch`. Its ordinary
and sensitive environment material MUST NOT enter JSON, the control protocol,
durable state, diagnostics, logs, argv, or a regular file. The daemon transfers
the command to the exact already-running peer wrapper over a separate fixed
`launch.sock` below the private Agent Sessions runtime root. The wrapper owns
the terminal and MUST exec the native product in place.

The handoff ticket is a random selector, not authority. Authority is the exact
wrapper `{UID, ProcessIdentity}` captured from the initial `daemon.sock`
connection and freshly recaptured from kernel peer credentials on
`launch.sock`. UID, PID, start, and strong-start MUST all match, and UID MUST
equal the daemon's effective UID. The endpoint uses the same no-follow,
user-owned 0700 parent and 0600 socket rules as other local transports.

## 2. Single ephemeral record

The broker admits one product-neutral record:

```text
{ticket, attachment_id, exact_wrapper_uid_and_process_identity,
 NativeCommand, created_at, deadline, opaque_finalization_plan}
```

It is generation-local and memory-only. The complete command uses one path for
raw component bootstrap values, Kilo server credentials, and future sensitive
native environment entries. There is no product-specific secret side channel.

Production bounds are:

- 64 pending or claimed entries;
- 256 KiB per encoded command and 1 MiB aggregate;
- 256 arguments, 256 ordinary environment entries, 32 sensitive entries;
- 128 bytes per environment name and 64 KiB per individual field;
- 30 seconds to consume and 10 seconds for the binary handshake;
- five seconds for an exact rollback attempt.

Environment names, counts, lengths, NULs, and duplicates are validated before
staging. A sensitive value appearing in path, cwd, argv, or ordinary env makes
the command invalid. Stage admission failure after attachment preparation MUST
be followed by exact rollback by the coordinator.

`NewFinalizationPlan` accepts two capabilities but exposes neither field:
destructive `RollbackFunc` for a proven zero-byte `go`, and
`AmbiguousFinalizer` for a partial/possible `go`. Classification consumes the
plan into one of three compile-time variants. The ambiguous variant has no
rollback field or method; it can only reconcile live adoption versus proven
absence and record cleanup debt when neither is provable.

## 3. Binary protocol

The protocol is a four-byte big-endian length followed by a bounded custom
binary frame. Every body begins with `ASLH`, uint16 contract version `1`, and a
one-byte kind. It never uses JSON, gob, or base64.

```text
wrapper -> daemon: claim(ticket)
daemon  -> wrapper: command(path,cwd,args,ordinary_env,sensitive_env) | error(category)
wrapper -> daemon: ack(sha256(exact_command_frame))
daemon  -> wrapper: go | error(category)
```

The digest is correlation only. It MUST NOT be logged or persisted. Stable
errors contain no native command, ticket, digest, or credential detail.

Claim changes `pending -> claimed` atomically. Exactly one claimant succeeds.
A prepared attachment has at most one pending or claimed record; staging a
second selector for the same attachment fails closed. The control response
cache replays the original pending selector without re-running Stage.
A claimed entry NEVER returns to pending: disconnect, invalid ACK, timeout, or
shutdown destroys the command and rolls the prepared attachment back. A retry
may reuse only the same still-pending ticket returned by the idempotent control
cache. After claim, retry requires a fresh prepare and fresh secret.
During the bounded interval between a full `go` write and completed in-memory
settlement, replay reports `claimed`; after settlement it reports `stale`.
Both are terminal/burned results and neither makes the selector dispatchable.

The byte transport reports the exact framed byte count for `go`. The broker
classifies and settles it as follows:

- full frame: consume the record with no cleanup callback;
- zero bytes: invoke the destructive rollback capability;
- partial/possible write: invoke only the ambiguous finalizer, never rollback
  and never replay.

The unified record, attachment reservation, and aggregate capacity remain
present through write classification AND through the selected callback. A
second Stage cannot race an old rollback/debt handoff. The command is cleared
when resolution begins, while its accounting/reservation remains until the
callback returns. Close and expiry close active claims; a claim already writing
`go` settles from the exact write count. The public Close boundary waits for all
active handlers and callback settlement and returns with no entry or capacity
remaining. Finalizers MUST NOT re-enter Broker lifecycle methods.

Client socket FDs are CLOEXEC and closed before exec. The exported production
`ConsumeAndExec` is bound to `chdir` + `syscall.Exec` image replacement; the
injectable callback exists only as an unexported test seam. The wrapper
constructs the child environment deterministically from the ordered envelope
alone, never merges ambient environment and never calls `os.Setenv`. A short or
truncated `go` fails closed before exec. Only the final native exec environment
may contain the sensitive values.

## 4. Cleanup and recovery

- Pending/claimed-before-GO expiry, broker shutdown, malformed handoff, or
  disconnect invokes the record's exact rollback at most once.
- Expiry/shutdown concurrent with `go` uses the same full/zero/partial write
  classification; partial/possible writes can only hand off reconciliation and
  cleanup debt.
- Rollback calls the daemon AttachmentEngine; product cleanup ambiguity becomes
  durable cleanup debt through that existing authority.
- A complete `go` transfers lifecycle responsibility to normal attachment
  adoption and owner-death reconciliation.
- Daemon restart drops every ephemeral handoff. A pre-restart ticket fails
  closed against the successor broker; stale prepared attachments reconcile by
  the existing generation/rollback rules.
- No handoff body or credential is retained to make an ambiguous claim
  replayable.

## 5. Threat model

The mechanism prevents accidental persistence or logging, different-UID
consumption, stale ticket/PID reuse, wrong-wrapper targeting, duplicate
consumption, unbounded frames, and symlink/socket substitution within the
existing local-transport rules.

The project retains its same-UID trust boundary. Root/kernel compromise,
malicious same-UID ptrace or `/proc` memory inspection, daemon/product process
compromise, and the final native product reading its required environment are
out of scope. Go cannot guarantee erasure of immutable string copies; bounded
binary buffers are zeroed best-effort and all references are dropped promptly.

## 6. Required acceptance cells

1. Exact initiating wrapper consumes once; replay and concurrent claim fail.
2. Foreign PID, stale start, and simulated PID reuse do not consume the pending
   ticket; an exact subsequent claimant may still consume it.
3. Disconnect after claim never re-pends and rolls back exactly once.
4. Expiry and shutdown drain bounded state and converge through rollback/debt.
   Close does not return while a callback, entry, or capacity reservation
   remains.
5. Wrong version, malformed/trailing/oversized frames, bad digest, duplicate
   env, NUL, and secret-in-public-field all fail closed.
6. A sentinel is absent from control JSON, state, logs, stdout/stderr, argv,
   ambient wrapper environment, and every regular state-root file. An injected
   exec seam observes it only in the exact final child environment.
7. No launch socket FD survives to the exec seam; exec failure remains typed
   and the coordinator can roll back the exact attachment. The exported API is
   statically bound to native syscall image replacement.
8. Linux and macOS compile/unit coverage is mandatory. Physical platform credit
   requires the existing release acceptance gate; cross-compilation alone is
   explicitly no credit.
9. A blocked `go` retains the exact attachment reservation and aggregate
   capacity. Full/zero/partial outcomes select consume/rollback/ambiguous
   exactly once; partial never reaches rollback. Close-versus-expire is
   convergent and cannot cross-contaminate a replacement prepare.
10. Partial body reads and decoded temporary byte buffers are zeroed on every
    error; a wrapper receiving a truncated `go` never invokes exec.

## 7. Rejected mechanisms

- `ExtraFiles`/inherited pipe would require the daemon to spawn the wrapper,
  breaking wrapper-owned TTY and PID-preserving exec topology.
- SCM_RIGHTS pipe/socketpair transfer would alter the cached JSON control
  framing and adds ancillary-data, CLOEXEC, and retry ambiguity.
- A binary tail on `daemon.sock` couples credentials to its cached response
  lifecycle.
- `memfd` is Linux-only, seekable/replayable, and has no macOS equivalent.
