# Runtime lifecycle debt discovered during Qwen acceptance

**Status**: transferred to a separate Spec Kit feature and pull request  
**Qwen source boundary**: `ed752b47db7dab2290a2c446c633944ca8a4bae8`  
**Release effect**: blocks the v0.2.4 release/tag gates; does not block merging the reviewed Qwen
feature boundary into `develop`

## Observation

During macOS upgrade validation, one native profile was served concurrently by two responsive Agent
Sessions supervisors:

- the pre-upgrade supervisor used the previous installed runtime and a socket below the native
  Darwin temporary root;
- the successor used the current installed runtime and a socket below `/tmp`;
- the independently operated federation agent also remained on its pre-upgrade executable.

After the operator closed every real Codex peer, process and session-state inspection found no live
Codex shim or remote Codex TUI. The legacy supervisor nevertheless continued to report one live shim.
The successor's fail-closed quiescence gate therefore refused replacement indefinitely. The refusal
was safe, but the supported upgrade path could not make progress from observable live state.

## Root-cause class

The runtime has multiple long-lived Agent Sessions authorities with overlapping durable ownership,
different restart triggers, different runtime-root selection histories, and independently loaded
versions. Correctness consequently depends on cross-process inventories being current across an
upgrade. A stale scalar held by an obsolete process can deadlock the process responsible for safely
retiring it. Separately restarting the federation agent leaves the same version-skew class open even
if supervisor reconciliation succeeds.

The Qwen peer, lane, composition, install, rollback, and federation contracts were not the cause.
Qwen acceptance merely exercised the installation and cross-product lifecycle deeply enough to make
the existing runtime defect visible.

## Disposition

An interim patch that added per-shim PID/start/socket inventory and legacy-child reconciliation was
rejected from this feature. It increased the cross-process protocol whose drift caused the defect
and did not eliminate independently versioned host, lane-manager, supervisor, and federation-agent
roles.

The durable repair is assigned to a new Spec Kit feature on a separate branch. Its starting design
constraint is one versioned per-user Agent Sessions runtime authority for Agent Sessions-owned
long-lived roles. Vendor executables remain external, and vendor-mandated stdio MCP children may
remain only as stateless connectors. The successor specification, plan, tasks, implementation, and
upgrade evidence must be reviewed independently rather than retrofitted into this Qwen spec.

No supervisor-repair implementation from the abandoned line is included in the Qwen boundary named
above. No tag or release is authorized by this note.
