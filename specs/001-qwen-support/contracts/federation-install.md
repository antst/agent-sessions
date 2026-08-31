# Federation, Packaging, and Installation Contract

## Federation

`qwen-lane` is a current-version federation capability declared by the shared product descriptor.
A host advertises it only when its configured Qwen lane executable and operation-specific doctor are
ready. Executable presence alone is insufficient.

Remote requests:

- select one connected host explicitly;
- carry the existing hub-attested ParentContext, including source runtime directory;
- create only on the selected destination;
- add destination-child and source-parent private anchors;
- inherit other groups only on explicit request;
- never fall back locally or to another host;
- emit a collection command runnable verbatim at the source.

All hubs, agents, launchers, and plugins are current. No legacy compatibility payload is permitted.

## Package inventory

Every platform release archive contains eleven executables:

```text
agent-session-runtime
peer
codex-peer
claude-peer
grok-peer
qwen-peer
codex-peer-lane
claude-peer-lane
grok-peer-lane
qwen-peer-lane
peer-federator
```

It also contains all four product plugin/skill payloads, deployment examples, operator docs,
checksums, and exact version metadata. The prebuilt installer validates this inventory and installs
without Go or a source checkout. `.github/workflows/ci.yml` must invoke the same repository-owned
authoritative build/package entrypoint as local packaging; it must not maintain a separate executable
or plugin list. The workflow and release evidence contract are validated by repository tests so a
new product cannot pass local packaging while disappearing from published archives.

Final workflow evidence, signed-tag binding, archive digests, and GitHub release attachment follow
[`release-evidence.md`](release-evidence.md) and its normative
[`release-evidence.schema.json`](release-evidence.schema.json).

## Qwen plugin install

The staged Qwen Agent Plugin v1 payload is named `agent-sessions` and includes:

- root `plugin.json`;
- root `mcp.json`;
- `skills/agent-sessions/SKILL.md`;
- lane skills for Codex, Claude, Grok, and Qwen.

Explicit installation uses the selected native profile:

```text
QWEN_HOME=<selected-absolute-home> \
qwen extensions install <versioned-staged-plugin> --scope user --consent
```

For the default profile, `QWEN_HOME` remains unset. Installation must verify exact installed identity,
version, enabled state, recorded immutable source path, MCP name, and every skill after the native
command returns. A same-version developer source must be reconciled to the selected installed source.
When native Qwen requires uninstall/install for that reconciliation, Agent Sessions first proves the
recorded prior local source and plugin identity. A failed native install or failed exact post-install
verification restores and verifies the prior enabled plugin through Qwen's supported installer. If
the prior source cannot be proved, replacement refuses before uninstalling anything.

Rules:

- `install-qwen` is explicit and profile-scoped.
- `install-all` includes `install-qwen` for the default profile when Qwen Code is present and reports
  a skip when it is absent; explicit `install-qwen` remains strict.
- Launch never installs, updates, enables, copies, or borrows integration files.
- Missing integration produces an instruction to run the explicit install for that exact profile.
- Authentication, trust, owner settings, unrelated extensions/skills, and transcripts are preserved.
- Upgrade refuses unsafe shared-runtime replacement while an exact managed Qwen peer/lane is live.
- Upgrade never destroys a working selected-profile integration merely because its replacement fails.
- A host agent evaluates capability readiness at startup; installing a newly available integration
  requires an explicit agent restart and never signals an unrelated running agent implicitly.

## Documentation symmetry

Add `docs/QWEN-ADAPTER.md`, `docs/QWEN-LANES.md`, and `docs/QWEN-INSTALL.md`; update README,
installation, groups, federation, protocol, adapter protocol, acceptance matrix, skills, examples, and
release notes from three to four products. Genuine Qwen differences—profile selection, dual output,
ACP, native archive helper, and Qwen-owned mutable approval modes—must be explicit rather than hidden
behind generic wording. The guides must state the general peer-mode rule: the native product behaves
normally, while Agent Sessions adds authenticated communications and cross-product lane execution.
