# Groups and routing

Groups are the discovery and routing boundary. Two live sessions can list or message one another
when their effective group sets intersect. Names are presentation and selectors; they do not grant
visibility or authority.

## Peer membership

Peer wrappers accept repeatable `-g NAME` or `--group NAME`. Membership is defined completely by
the current start or resume invocation. Omitting a former group removes it; passing a new one adds
it. Agent Sessions never carries groups forward.

Each peer also receives a private anchor:

```text
session:<host-id>/<native-session-id>
```

The anchor provides an exact private namespace and is derived from live identity. It is not a user
alias. A bare product invocation that does not connect to Agent Sessions remains outside this
routing plane.

## Lane membership

A lane always receives its parent's private anchor and a child anchor:

```text
session:<parent-host>/<parent-native-id>
session:<parent-host>/<parent-native-id>/<lane-native-id>
```

Lane `--group` values must belong to a namespace visible to the parent: the exact parent group or a
slash-prefixed subgroup. `--inherit-groups` copies the parent's other effective groups for that
invocation; without it only explicitly requested groups and the mandatory anchors are present.

The child anchor is derived after the product supplies the final native ID. The durable candidate
row stores only the parent primary anchor and assigned/inherited non-derived secondary groups.
Archive and resume reconstruct the same child anchor; no cache stores a second group projection.

## Addressing and delivery

`peers.list` and the Agent Sessions list tool return only currently live sessions that share a
group with the caller. Direct and multicast sends resolve against that same snapshot. Broadcast
targets one visible group. The group set displayed for a destination is the same set routing uses;
listing and delivery cannot disagree.

Names may be duplicated. An exact native session ID is unambiguous. A name selector succeeds only
according to the product or wrapper selector rules documented in [Products](PRODUCTS.md).

Message success means the destination product accepted that one live delivery. EOF removes the
destination immediately. There is no offline queue, mailbox, durable receipt, or daemon retry.

## Delegation and permission

Sharing a group does not authorize one model to override another. It only permits discovery and
transport. A receiving agent still follows its instruction hierarchy, sandbox, approval policy,
and user-established delegation. The envelope identifies the actual live sender by native UUID,
product-owned name, product, and groups.

Permission selection is separate and invocation-owned. A group cannot widen product permissions.

## Shared-group handover

A product-confirmed offline lane may be resumed by a different live parent only through a group the
two sides share. Concretely, the resuming parent's live groups must intersect the groups reconstructed
from the immutable candidate row. The new parent becomes the in-memory owner and receives its own
parent/child routing anchors; the row's historical parent field is not rewritten or served as an
answer.

A lane with a live driver session under another connected parent cannot be taken over. When a
parent connection ends, idle nonpersistent lanes are archived once. Persistent lanes remain open
but unowned and may be reattached by an eligible parent without opening a second driver session.

## Federation

The same rules apply across hosts. A hub distributes complete live rosters; each host remains the
authority for its local sessions and lanes. Private anchors contain the source host identity so
they remain distinct across the federation. See [Federation](FEDERATION.md).
