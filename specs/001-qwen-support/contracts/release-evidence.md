# Release Evidence Contract

## Authoritative producer

The final v0.2.4 evidence producer is `.github/workflows/ci.yml` running against the exact signed
`main` release commit. A local rehearsal report, copied terminal transcript, or manually assembled
release asset is not final release evidence.

The workflow must consume the same authoritative executable and plugin inventory as local packaging.
It must not carry a separate hard-coded product list. Before any tag or release publication it must
prove that all four platform archives contain the declared eleven executables and four plugin
payloads.

## Artifact identity

The workflow emits one byte-stable JSON file:

```text
agent-sessions-v0.2.4-release-evidence.json
```

The immutable workflow artifact containing it is named:

```text
agent-sessions-v0.2.4-release-evidence-<full-commit-sha>
```

The workflow artifact uses a 90-day retention period. The same JSON bytes are later attached to the
GitHub release for the lifetime of that release. `SHA256SUMS` includes the JSON file as well as every
platform archive. Before tag creation, the release procedure must refuse if local or remote
`refs/tags/v0.2.4` already exists. After that exact signed tag is created, the tag-triggered job must
require it, verify its signature and target, and refuse an existing GitHub release or same-named
release asset on that target release rather than replace either. Asset names may repeat on historical
releases because GitHub scopes assets to their owning release. The triggering tag itself is required state, not a
publication collision. After publication, the signed tag's recorded digest detects any out-of-band
asset change.

## Required JSON schema

[`release-evidence.schema.json`](release-evidence.schema.json) is the normative Draft 2020-12 JSON
Schema for the document. The evidence producer and every fixture/release validator must validate
against that checked-in schema rather than a separately transcribed field list. Schema v1 rejects
unknown fields at every object level and defines every nested field name, type, required list, enum,
cardinality, digest format, platform key, gate key, executable, plugin identifier, and archive path.

The document bytes are the RFC 8785 JSON Canonicalization Scheme serialization followed by exactly
one LF byte, with no secret values. Its top-level shape is:

```json
{
  "schema_version": 1,
  "release_version": "0.2.4",
  "intended_tag": "v0.2.4",
  "commit_sha": "<40 lowercase hex>",
  "tree_sha": "<40 lowercase hex>",
  "artifact": {},
  "workflow": {},
  "toolchains": {"linux": {}, "macos": {}},
  "native_clients": {"linux": {}, "macos": {}},
  "gates": {"linux": {}, "macos": {}},
  "archives": {},
  "package_inventory": {}
}
```

The schema is authoritative for representation. These cross-field and byte-level invariants are
validated in addition because JSON Schema does not express them completely:

- `commit_sha` identifies the checked-out signed release commit and `tree_sha` equals its actual tree.
- `artifact.workflow_artifact_name` ends with that exact `commit_sha`.
- `workflow.run_url` identifies `workflow.run_id` and the run checked out `commit_sha`.
- Every archive's `source_commit` equals `commit_sha`; its object key, `platform`, and exact filename
  identify the same platform; its SHA-256 and byte size match the rebuilt file.
- Every gate URL belongs to the recorded workflow run and every referenced evidence artifact exists
  in that run with the recorded SHA-256.
- The exact package inventory from the schema exists in every archive at the declared paths.
- RFC 8785 serialization of the parsed value plus one LF reproduces the evidence bytes exactly.
- The exact-commit evidence run and tag-triggered release rebuild use version `0.2.4` from
  `deploy/peer-federator/VERSION`; the signed tag is independently required to equal `v0.2.4`.
  Building any platform archive twice from the same commit, version, toolchain, and authoritative
  inventory produces byte-identical output and the SHA-256 recorded by the evidence artifact.

Changing any field, type, enum, inventory member, or canonicalization rule requires a new
`schema_version` and a new checked-in schema; v1 permits no unversioned extensions.

## Signed-tag binding and publication

The signed annotated `v0.2.4` tag is created only after the exact-commit workflow run succeeds and
the pre-tag local/remote absence check passes. Its
annotation contains these exact trailers:

```text
Agent-Sessions-Evidence-Run: <run URL>
Agent-Sessions-Evidence-Artifact: agent-sessions-v0.2.4-release-evidence-<full-commit-sha>
Agent-Sessions-Evidence-SHA256: <64 lowercase hex>
```

The tag must point to `commit_sha`. The tag-triggered release job requires that exact existing signed
tag, downloads the exact workflow artifact by run identity, verifies its SHA-256 and
commit/tree/tag/schema fields, verifies the four rebuilt archives against the recorded authoritative
inventory, refuses an already-created GitHub release or colliding asset, and uploads the unchanged
JSON file, archives, and `SHA256SUMS` to the new GitHub release.

Any changed evidence byte, missing platform result, failed or skipped required gate, inventory drift,
tag/commit mismatch, unavailable workflow artifact, or rebuilt archive hash mismatch stops
publication. A new source/tree change invalidates the evidence and restarts the release sequence.
