# Contract: Durable Lane Input Ledger and Spool

This contract replaces volatile busy-lane input and applies to all ten
products. The lane engine, not a product driver, owns admission, ordering,
recovery, and retirement.

## 1. Admission Transaction

For exact input bytes `B`:

1. validate caller, lane visibility/ownership, content size, and total quota;
2. generate receipt and spool-object IDs;
3. exclusive no-follow create under the private spool root;
4. write `B`, fsync, verify identity/size/SHA-256, fsync parent;
5. commit a receipt with lane-local sequence and state `Queued` (a
   `Prepared` transition may be represented within the same engine operation);
6. only after commit, return caller acceptance with receipt ID and sequence.

Failure before step 5 is never caller-accepted. Recovery may remove an exact
orphan spool object after proving it has no committed receipt.

## 2. Dispatch Transaction

1. select the earliest eligible `Queued` receipt;
2. select `Steer` only when the product declares it and an exact native turn is
   running; otherwise assign/start the next daemon turn;
3. commit `Dispatching`, target turn, and stable dispatch-attempt ID;
4. open the spool object no-follow and re-verify identity, length, and digest;
5. call `Steer` or `StartTurn` with the receipt ID;
6. on exact native acceptance, commit `Injected` plus
   `NativeAcceptanceRef`/native turn reference;
7. securely retire the exact spool object and later mark metadata `Retired`
   under the documented retention policy.

`ErrUnsupportedSteer` moves the same receipt back to ordered queueing for the
next turn. It does not create another receipt or alter sequence.

## 3. Crash Matrix

| Crash point | Recovery result |
|---|---|
| Before spool fsync | no accepted receipt; incomplete exact object removed |
| After spool fsync, before receipt commit | unacknowledged orphan removed after exact proof |
| After receipt commit, before dispatch intent | receipt remains queued |
| After `Dispatching`, before native I/O | driver recovery may prove no native acceptance and requeue exact receipt |
| After native I/O, before durable native ack | `Ambiguous`; no replay unless native idempotency is proven |
| After `Injected`, before spool removal | exact object removed during recovery; no redelivery |
| During removal with changed identity/type | cleanup debt; unrelated object preserved |

## 4. Ordering

- Receipt sequence is unique and strictly increasing per lane.
- One receipt may be associated with only one daemon turn at a time.
- Steering preserves receipt order relative to other accepted input.
- A native protocol's internal queue does not replace the daemon ledger; native
  acceptance is recorded as evidence after shared admission.
- Terminal result collection and input receipt retirement are distinct; old
  terminal turns remain collectable while later queued input is accepted.

## 5. Bounds and Privacy

- Per-input maximum is no larger than the current remote-lane input bound.
- Per-lane and host total bytes/counts are explicit configuration constants.
- Bodies never enter the JSON daemon catalog, roster, doctor output, logs,
  diagnostics, or federation capability frames.
- Digest, size, state, sequence, and redacted failure category may be exposed in
  status/receipt output.
- Federation carries an already bounded lane input to the destination, whose
  daemon performs its own spool transaction before acknowledging acceptance.

## 6. Ambiguity Resolution

`Ambiguous` is terminal for automatic dispatch. An operator may:

- query status and native evidence;
- explicitly retire/abandon the receipt;
- explicitly mark it proven injected when a product-specific authoritative
  query returns the exact operation ID;
- submit a new input, which gets a new receipt/sequence.

There is no generic force-replay action. Product-specific idempotent replay
requires a reviewed capability and acceptance test.

## 7. Existing-Four Migration

Claude, Codex, Grok, and Qwen busy input must enter the same ledger. Existing
product-specific native delivery remains behind drivers, but `laneActor.pending`
is no longer an acceptance authority. Restart tests prove no acknowledged input
is lost for any product.
