# Sessionbus repository split plan

Status: plan only. This document authorizes no deletion, history rewrite, tag,
push, package publication, or clone reset by itself.

## 1. End state

The split produces three repositories with one direction of dependency:

```text
github.com/antst/sessionbus-peers  -->  github.com/antst/sessionbus/bus/sdk/go
@sessionbus/dsh                   -->  @sessionbus/kit
sessionbus daemon                 -->  github.com/antst/sessionbus/bus/sdk/go/protocol
```

No SDK depends on daemon code. No peer imports a daemon-internal package. The
repositories are:

| Repository | License | Owned surface |
| --- | --- | --- |
| `antst/sessionbus` / Forgejo `ai/sessionbus` | GPL-3.0-only, except the two SDK subtrees below | Daemon, wire authority, Go and JavaScript kits, reference binaries, and design documents. |
| `antst/sessionbus-peers` / Forgejo `ai/sessionbus-peers` | MIT | Shared wrapper host and MCP packages, all product wrappers and peer commands, product facts, skills, plugins, and product installers. |
| `antst/sessionbus-dsh` / Forgejo `ai/sessionbus-dsh` | Existing package license; this split makes no new license claim | The already separate DSH package. Its history is squashed with the other repositories, but its source is not moved through either Go repository. |

The canonical GitHub repositories and their Forgejo mirrors have identical
`main` and `develop` tips after the rewrite. Each tip is a root commit with no
parent; `main` and `develop` point to that same commit. Historical development
remains reachable only through the Forgejo `legacy-*` branches described in
Section 3.

## 2. File inventory

This inventory is an allowlist. During execution, every tracked input path is
classified as `sessionbus`, `sessionbus-peers`, `sessionbus-dsh`, or
`archive-only`. An unclassified path aborts the split; it is never silently
copied or discarded.

### 2.1 `sessionbus`

The daemon repository contains only these paths, plus generated sums and the
new license files named here:

| Target path | Source and treatment |
| --- | --- |
| `LICENSE` | Replace the current root license with GPL-3.0-only text. |
| `.gitignore`, `.golangci.yml` | Retain, then narrow to the retained tree and the two Go modules. |
| `go.mod`, `go.sum` | Retain as module `github.com/antst/sessionbus`; remove every legacy dependency and add an exact requirement on the tagged Go SDK module. No local `replace` is committed. |
| `go.work`, `go.work.sum` | Create the repository-development workspace for `.` and `./bus/sdk/go`. |
| `bus/architecture_test.go` | Retain and rewrite as the post-split daemon/SDK boundary guard. |
| `bus/cmd/sessionbus/**` | Retain the daemon command. |
| `bus/cmd/sessionbus-call/**` | Retain the one-shot reference caller. |
| `bus/cmd/example-peer/**` | Retain the protocol reference worker; it is a bus conformance binary, not a product wrapper. |
| `bus/internal/conn/**` | Retain as daemon-only connection ownership. |
| `bus/internal/daemon/**` | Retain as daemon state, routing, row, launch, and lifecycle ownership. |
| `bus/internal/structuredprocess/**` | Retain as daemon process-group ownership. |
| `bus/sdk/go/**` | Retain as the independent Go SDK module and add its module, license, protocol, RPC, and socket-default files described in Section 4. |
| `bus/sdk/js/**` | Retain as the JavaScript kit and add its own MIT license. |
| `bus/package.json` and lockfile, if generated | Retain as the `@sessionbus/kit` npm package root. Its publish allowlist contains only JavaScript SDK files, the shared schema/fixtures needed at runtime, and the SDK license. |
| `bus/docs/PROTOCOL.md` | Retain the generated protocol. |
| `docs/designs/**` | Retain all signed design documents, including this plan. |
| `.github/workflows/ci.yml`, `.github/workflows/npm-publish.yml`, `.github/workflows/pkg-pr-new.yml` | Regenerate for this repository only; details are in Section 7. |
| `.forgejo/workflows/ci.yml` | Regenerate for this repository only. |

Current paths `bus/internal/protocol/**`, `bus/internal/rpc/**`, and
`bus/internal/stateroot/**` are consumed by the SDK today. They do not remain
at those locations after the moves in Section 4. No `wrappers/**`, product
command, product facts file, product name, or product installer remains here.

### 2.2 `sessionbus-peers`

The peers repository is module `github.com/antst/sessionbus-peers`. Its initial
tree is assembled from the stamped product tips, not from whichever single
branch happens to be checked out when the split starts.

The assembly manifest assigns one source owner to every overlapping path:

| Source owner | Paths selected from that owner's frozen stamped tip |
| --- | --- |
| Universal bus | No peer path; it supplies only the public SDK dependency version. |
| Shared host | `wrappers/host/**`. |
| Shared MCP | `wrappers/mcp/**`. |
| Claude | `wrappers/claude/**`, `cmd/claude-peer/**`, `docs/products/claude.md`, `claude/**`, and `.claude-plugin/**`. |
| Codex | `wrappers/codex/**`, `cmd/codex-peer/**`, `docs/products/codex.md`, `.codex-plugin/**`, `hooks/**`, `scripts/codex-mcp`, and `scripts/test-codex-mcp`. |
| Grok | `wrappers/grok/**`, `cmd/grok-peer/**`, `docs/products/grok.md`, and `grok/**`. |
| Qwen | `wrappers/qwen/**`, `cmd/qwen-peer/**`, `docs/products/qwen.md`, `docs/products/qwen-0.23.0-help.txt`, and `qwen/**`. |
| OpenCode | `wrappers/opencode/**`, `cmd/opencode-peer/**`, `docs/products/opencode.md`, and the active OpenCode package rehomed as `opencode/**`. |
| Facts-only products | `docs/products/kilo.md`, `docs/products/pi.md`, and `docs/products/omp.md`; no legacy implementation accompanies them. |
| Final integration tip | `skills/**` and `.agents/plugins/marketplace.json`, but only after that tip is proven to descend every stamped product/shared tip and its full-tree gate is green. |

If two frozen tips contain the same target path, the assembler hashes the
bytes and requires equality unless the table gives that path a single product
owner. A mismatch in a shared file is a merge finding; last-writer-wins is
forbidden. The manifest records source repository, source ref, full source
commit, source path, target path, mode, and blob ID for every selected file.

| Target path | Source and treatment |
| --- | --- |
| `LICENSE` | New MIT license text. |
| `README.md` | New peers-only installation and development README; do not copy the old root README. |
| `.gitignore`, `.golangci.yml` | Copy and narrow to the peers tree. |
| `go.mod`, `go.sum` | New peers module requiring the exact released `github.com/antst/sessionbus/bus/sdk/go` version. No `replace`. |
| `wrappers/host/**`, `wrappers/mcp/**` | Move the shared host and MCP packages intact. |
| `wrappers/claude/**`, `wrappers/codex/**`, `wrappers/grok/**`, `wrappers/qwen/**`, `wrappers/opencode/**` | Move the stamped product packages intact. Later product packages use the same location. |
| `wrappers/README.md` | Move and update module/repository links. |
| `cmd/claude-peer/**`, `cmd/codex-peer/**`, `cmd/grok-peer/**`, `cmd/qwen-peer/**`, `cmd/opencode-peer/**` | Move every built product peer command. No non-`-peer` command is copied. |
| `docs/products/**` | Move all product facts and captured help text. Add the historical-citation header from Section 6 to every facts Markdown file. |
| `claude/**` | Move the Claude marketplace package, MCP configuration, commands, skills, references, and preflight scripts. |
| `grok/**` | Move the Grok plugin, MCP configuration, native entry, and skills. |
| `qwen/**` | Move the Qwen plugin, MCP configuration, skills, and test data owned by the wrapper. |
| `.claude-plugin/**`, `.codex-plugin/**` | Move the active marketplace/plugin manifests. |
| `hooks/**`, `skills/**` | Move the active product hook and shared Sessionbus/lane skills. |
| `.agents/plugins/marketplace.json` | Move the Sessionbus Codex plugin marketplace entry. `.agents/skills/speckit-*` is not moved. |
| `scripts/codex-mcp`, `scripts/test-codex-mcp` | Move the active Codex installer and its transaction test. |
| `opencode/**` | Move the active files now under `integrations/opencode/**` into this non-legacy package root: `README.md`, `package.json`, `install.mjs`, `install.test.mjs`, `sessionbus.mjs`, `sessionbus.test.mjs`, `commands/**`, and `skills/**`. Update the npm repository directory and SDK dependency. The old `integrations/` path itself is not retained. |
| `.github/workflows/ci.yml`, `.github/workflows/pkg-pr-new.yml`, `.forgejo/workflows/ci.yml` | New peers-only workflows. The package preview publishes `./opencode`, not an old integration path. |

Product-specific root assets that exist only as development preferences, such
as `.grok/settings.json`, are not install artifacts and are archive-only unless
a product facts file and installer both name them before the freeze. The
preflight classification manifest makes that decision visible rather than
letting a wildcard copy them accidentally.

The initial peers tree includes only products with stamped implementation
tips. Facts for products not yet implemented may be retained under
`docs/products`, but their legacy implementations are not promoted into this
repository.

### 2.3 `sessionbus-dsh`

The DSH repository is already separate. Its audited implementation tip has the
following complete source inventory, which is copied into its root commit:

```text
.github/workflows/pkg-pr-new.yml
.gitignore
README.md
bin.mjs
docs/PROPOSAL.md
install.mjs
install.test.mjs
package.json
package.test.cjs
plugin.cjs
plugin.test.cjs
rename.test.mjs
```

The package remains `@sessionbus/dsh`, its repository URL remains
`https://github.com/antst/sessionbus-dsh.git`, and its exact dependency on
`@sessionbus/kit` is advanced only after that kit version exists. Its preview
workflow remains repository-local and is repointed from implementation
branches to the post-split development policy.

### 2.4 Archive-only input

The following paths are preserved by the Forgejo legacy refs but copied to no
new root commit:

- `internal/**` in full;
- the legacy command at `d9cd0f3:cmd/agent-sessions/main.go` and every other
  file in that command directory;
- the legacy hub rooted at `d9cd0f3:cmd/agent-sessions-hub/main.go`, plus
  `cmd/ingress/**` and `cmd/verbatim-writer/**`;
- `integrations/**` in full after the active OpenCode package files have been
  rehomed as listed above;
- `Makefile`, `scripts/install-host`, and the old root `README.md`;
- `deploy/**`, `examples/**`, the old top-level non-design/non-product docs,
  `.specify/**`, `specs/**`, and `.agents/skills/speckit-*/**`;
- old release, federation, service, migration, real-product, packaging, and
  removal scripts not explicitly retained in Section 2.2;
- old root commands, plugins, and helpers not explicitly retained above.

This is deletion from the new histories, not deletion of the historical
objects. The archive verification in Section 3 must pass before any root commit
is created.

## 3. Preserve every branch before rewriting history

No staging-tree rewrite begins until this phase is complete.

1. Freeze merges and package publications. Record the UTC freeze time.
2. Fetch `refs/heads/*` and `refs/tags/*` without pruning from both GitHub and
   Forgejo for `sessionbus` and `sessionbus-dsh`. Write a manifest containing
   repository, remote, full ref, and full object ID. Sign or checksum the
   manifest and store it outside all three working trees.
3. Treat GitHub heads as the canonical source heads. Map a source branch
   `<name>` to `legacy-<name-with-each-slash-replaced-by-hyphen>`. Thus `main`,
   `develop`, and `impl/universal-phase1-c5b` become `legacy-main`,
   `legacy-develop`, and `legacy-impl-universal-phase1-c5b`. Reject the entire
   operation if normalization produces a collision or an existing legacy ref
   points to a different object.
4. A Forgejo-only head, or a Forgejo head whose same-named GitHub head points to
   a different object, is also preserved before overwrite as
   `legacy-forgejo-<normalized-name>`. This prevents the mirror's current
   divergent `main` or `develop` from becoming unreachable while reserving the
   unqualified `legacy-*` name for the canonical GitHub head.
5. Push explicit object IDs, never moving local branch names:

   ```text
   <full-object-id>:refs/heads/<legacy-name>
   ```

   Push Sessionbus refs only to Forgejo `ai/sessionbus`, and DSH refs only to
   Forgejo `ai/sessionbus-dsh`. Use atomic batches small enough for the server;
   do not force an existing legacy ref.
6. Fetch the two Forgejo repositories into fresh verification clones. Compare
   every legacy ref to the freeze manifest and run `git fsck --full`. Record
   tags separately and verify the rewrite does not move or delete them.
7. Classify every commit-qualified facts citation before validating it. A
   citation to a path from the pre-split Sessionbus tree must resolve with
   `git rev-parse <abbrev>^{commit}` in the verified `ai/sessionbus` legacy
   clone. A product-source citation is external—for example OpenCode source
   commit `1674747`—and must instead have a manifest row containing product,
   canonical external repository URL, cited abbreviation, full commit ID, and
   frozen clone/archive checksum. Resolve it in that frozen external clone.
   Never infer that an unknown object belongs to Sessionbus. Ambiguous,
   missing, or unclassified citations stop the split.

Only the owner may lift the freeze after all new repositories pass Section 8.

## 4. SDK extraction and current forbidden-import inventory

There are currently zero product-wrapper or product-command imports of
`github.com/antst/sessionbus/bus/internal/...`; all such production files use
`github.com/antst/sessionbus/bus/sdk/go`. The architecture test already checks
that boundary. The work is inside the Go SDK itself and in its tests:

The complete current import inventory is:

```text
bus/internal/protocol:
  bus/sdk/go/action.go
  bus/sdk/go/caller.go
  bus/sdk/go/caller_test.go
  bus/sdk/go/peer.go
  bus/sdk/go/peer_test.go
  bus/sdk/go/schema.go
  bus/sdk/go/worker.go
  bus/sdk/go/worker_test.go

bus/internal/rpc:
  bus/sdk/go/client.go
  bus/sdk/go/caller_test.go
  bus/sdk/go/peer.go
  bus/sdk/go/peer_test.go
  bus/sdk/go/worker.go
  bus/sdk/go/worker_test.go

bus/internal/stateroot:
  bus/sdk/go/client.go

bus/internal/daemon:
  bus/sdk/go/peer_test.go

relative reads under bus/internal/protocol:
  bus/sdk/go/caller_test.go
  bus/sdk/go/worker_test.go
  bus/sdk/js/caller.test.js
  bus/sdk/js/schema.js
  bus/sdk/js/schema.test.js
  bus/sdk/js/worker.test.js
```

| Current dependency | Current SDK consumers | Move/rewrite |
| --- | --- | --- |
| `bus/internal/protocol` | `bus/sdk/go/action.go`, `caller.go`, `peer.go`, `schema.go`, `worker.go`, and their tests | Move the full wire package—types, errors, frame codec, validation, schema, three fixture files, and tests—to `bus/sdk/go/protocol`. The daemon imports this MIT package. The root `sessionkit` package continues to alias the public application types so peers need not import `protocol` directly. |
| `bus/internal/rpc` | `bus/sdk/go/client.go`, `peer.go`, `worker.go`, and SDK tests | Move to `bus/sdk/go/internal/rpc`. It is kit transport, not daemon transport. Rewrite root-module tests that need a client to use public `sessionkit.Client` instead of reaching into SDK internals. |
| `bus/internal/stateroot` | `bus/sdk/go/client.go`; daemon composition also calls it today | Move the default socket/root calculation to `bus/sdk/go/internal/stateroot`, keep `sessionkit.Socket()` as the only public surface, and change `bus/cmd/sessionbus` to call that public surface. |
| `bus/internal/daemon` from `bus/sdk/go/peer_test.go` | Real-daemon EOF/admission integration rows | Move those rows to a root-module daemon/SDK integration test. The SDK module tests use only local fake sockets and SDK-owned internals, so `GOWORK=off go mod graph` for the SDK contains no root daemon module. |
| `../../internal/protocol/*.json` from Go and JavaScript tests, and `schema.js` | Shared schema/lifecycle/caller fixtures | Point both kits at the new canonical copies under `bus/sdk/go/protocol`. Include the runtime schema in the npm `files` allowlist and enforce byte identity in both language suites. No generated copy may drift silently. |

The resulting Go SDK subtree is exact in shape:

```text
bus/sdk/go/
  LICENSE
  go.mod
  go.sum
  action.go, caller.go, client.go, peer.go, schema.go, worker.go
  *_test.go
  protocol/
    frame.go, schema.go, types.go, validate.go, *_test.go
    session.schema.json
    session.fixtures.json
    session-lifecycle.fixtures.json
    caller-sugar.fixtures.json
  internal/rpc/
    rpc.go, rpc_test.go
  internal/stateroot/
    stateroot.go, stateroot_test.go
```

All files in that module, including generated Go source, begin with
`// SPDX-License-Identifier: MIT`. `bus/sdk/go/LICENSE` contains the MIT text.
All JavaScript SDK source begins with `// SPDX-License-Identifier: MIT`, and
`bus/sdk/js/LICENSE` contains the MIT text. JSON files cannot carry comments;
their license is established by the nearest SDK license and the npm manifest.
All retained daemon Go source begins with
`// SPDX-License-Identifier: GPL-3.0-only` and is covered by the root GPLv3
license.

The moves are gated by these negative checks:

- no file in `sessionbus-peers` contains a Go import with prefix
  `github.com/antst/sessionbus/bus/internal/`;
- no package in the SDK module imports `github.com/antst/sessionbus` outside
  its own module path;
- `cd bus/sdk/go && GOWORK=off go list -m all` contains no daemon module;
- the npm pack manifest contains no daemon, wrapper, or product file;
- the daemon module imports no `github.com/antst/sessionbus-peers` path.

## 5. Module and workspace shape

### 5.1 `sessionbus`

Both module ecosystems use the one fixed first split prerelease:
`v0.1.0-pre.2` for Go and `0.1.0-pre.2` for npm. Placeholders are forbidden in
candidate manifests, module files, package files, tags, and workflows.

The root module is:

```text
module github.com/antst/sessionbus
go 1.24
require github.com/antst/sessionbus/bus/sdk/go v0.1.0-pre.2
```

The SDK module is:

```text
module github.com/antst/sessionbus/bus/sdk/go
go 1.24
```

The committed workspace is:

```text
go 1.24
use (
  .
  ./bus/sdk/go
)
```

Neither `go.mod` contains a local `replace`. The SDK version is tagged with the
standard submodule tag `bus/sdk/go/v0.1.0-pre.2` on the same root
commit. The initial root commit, both branch updates, and that tag are pushed
atomically, so the root module's exact requirement is resolvable as soon as the
new history is visible. Development commands test once through `go.work` and
once with `GOWORK=off` in each module.

### 5.2 `sessionbus-peers`

The peers root has one module:

```text
module github.com/antst/sessionbus-peers
go 1.24
require github.com/antst/sessionbus/bus/sdk/go v0.1.0-pre.2
```

Every old `github.com/antst/sessionbus/wrappers/...` import changes to
`github.com/antst/sessionbus-peers/wrappers/...`; SDK imports keep their public
Sessionbus path. No `go.work` is committed to this repository. A developer who
needs an unreleased cross-repository change creates an untracked parent
workspace containing the two repository roots and the SDK submodule; CI and
release gates always run without that overlay.

### 5.3 `sessionbus-dsh`

DSH remains an npm-only CommonJS package. Its `package.json` pins exact
`@sessionbus/kit` version `0.1.0-pre.2`. It has no filesystem or Git dependency
on either sibling repository.

## 6. Facts and immutable history

Every Markdown facts file under `docs/products` receives this header directly
below its title:

> Historical source note: citations to pre-split Sessionbus paths resolve in
> the Forgejo `ai/sessionbus` repository through its `legacy-*` branches.
> Citations to product source resolve in the external repository and full
> commit recorded by the split archive manifest. Host evidence paths are
> immutable external artifacts, not repository paths.

Captured blocks and sealed evidence paths remain byte-for-byte unchanged. A
post-split guard permits former identifiers only in the already-ruled shapes:
captured fenced blocks in product facts, immutable evidence-path tokens, and
commit-qualified historical paths. The guard scans both repositories and is
updated for the new module/repository paths; the peers repository owns the
product-facts half, while the daemon repository owns the design-document half.

The archive manifest records which legacy ref makes each Sessionbus citation
reachable and which frozen external clone resolves each product-source
citation. The initial peers commit message names the manifest checksum so the
new root is auditable without pretending its parentless history contains the
old citations.

## 7. CI, publishing, and guard relocation

### 7.1 `sessionbus`

- GitHub and Forgejo CI build `bus/cmd/sessionbus`,
  `bus/cmd/sessionbus-call`, and `bus/cmd/example-peer` on supported targets.
- Run root-module test, race, vet, and lint gates; run the same gates separately
  in `bus/sdk/go` with `GOWORK=off`; run a workspace integration gate; and run
  `npm test --prefix bus` on Node 24.
- Replace the current monolithic release workflow with daemon-only artifacts.
  It must not reference removed commands, old release scripts, product
  packages, or product evidence.
- Keep `.github/workflows/npm-publish.yml` on `kit-v*`, Node 24, npm >=11.5.1,
  tag/version equality, tests, and provenance. Repoint schema/file allowlists
  after the SDK move. No token secret is introduced.
- Keep pkg.pr.new for `./bus`, but assert the packed file list contains only the
  MIT JavaScript kit/license/schema surface.
- Replace the pre-split architecture guard with repository-local checks for:
  no peers import/path, no product token in bus source, independent SDK module
  graph, SPDX/license coverage, former-brand exceptions, and generated
  protocol byte identity.

### 7.2 `sessionbus-peers`

- New GitHub and Forgejo CI run format, lint, build, test, race, and vet for the
  peers module on Linux and Darwin, plus every product-specific fixture gate.
- Run the import guard that rejects all daemon internals and accepts only the
  public Go SDK path for Sessionbus Go dependencies.
- Run product installer/plugin tests, the former-brand guard, facts-header and
  historical-citation reachability checks, SPDX/license coverage, and package
  manifest/repository URL checks.
- pkg.pr.new publishes only current package roots, initially `./opencode`.
  Product installers and manifests point to `antst/sessionbus-peers`; none
  points to the daemon repository as its own source repository.
- The initial root has no release workflow for `*-peer` binaries or
  `@sessionbus/opencode`. Its README and product install/facts documents say
  explicitly that binaries are built from source and the OpenCode package is a
  pkg.pr.new preview. Stable binary and npm release jobs are a separately
  reviewed later deliverable; this split does not invent them or imply that an
  initial release artifact exists.

### 7.3 `sessionbus-dsh`

- Repoint pkg.pr.new triggers to the post-split branch policy and keep publish
  root `.`.
- Test with the exact published `@sessionbus/kit` dependency; never use a
  sibling filesystem dependency in CI.
- Add a repository URL/package-file guard so the package cannot regress to a
  monorepo path.
- The initial root retains preview publication only. Its README says that
  `@sessionbus/dsh` is a pkg.pr.new preview until a separately reviewed trusted
  publishing workflow exists; the split does not claim a registry release of
  DSH itself.

## 8. Exact execution sequence

Each numbered gate completes before the next begins.

0. **Amend and sign the retained design.** On the design review branch, update
   every current-authority clause in
   `docs/designs/UNIVERSAL-SESSION-PROTOCOL.md` that names
   `bus/internal/protocol` or `bus/internal/rpc`. The wire/schema authority
   becomes `bus/sdk/go/protocol`; SDK transport becomes
   `bus/sdk/go/internal/rpc`; the module and generation clauses match Sections
   4 and 5. Regenerate `bus/docs/PROTOCOL.md`, obtain the architect's split
   signature, sync the signed bytes to the universal branch, and make that
   signed commit the freeze input. Historical commit-qualified passages are
   left historical. No tree assembly starts from a design that contradicts
   the target paths.
1. **Freeze and archive.** Execute Section 3 for both existing repositories.
   Verify every legacy ref and citation from fresh Forgejo clones. Save the
   branch/tag/archive/external-source manifest and checksum outside the
   worktrees.
2. **Create clean staging clones.** Clone the canonical GitHub repositories into
   new, empty parent directories. Do not reuse a developer worktree, installed
   binary prefix, or host evidence directory. Record all stamped source-tip
   object IDs used to assemble the daemon and peers trees.
3. **Assemble the SDK first.** Perform Section 4 in the Sessionbus staging
   clone. Add licenses/SPDX headers, split integration tests out of the SDK,
   generate the two module sums and workspace sum, and prove both SDKs contain
   no daemon dependency or packed daemon file.
4. **Assemble the daemon tree.** Apply the Section 2.1 allowlist, rewrite daemon
   protocol/socket imports to the SDK-owned surfaces, regenerate protocol docs,
   and regenerate daemon-only CI/guards. Produce a complete input-path
   classification report.
5. **Assemble the peers tree.** Export the stamped wrapper/product paths into a
   separate empty repository, apply the Section 2.2 path moves and import
   rewrite, add the facts headers, licenses, CI, and guards, and generate its
   classification report. Verify no source content was selected from the
   legacy implementations.
6. **Assemble DSH.** Export exactly Section 2.3 from its frozen implementation
   tip, update only the released kit pin and post-split CI/repository metadata,
   and generate its classification report.
7. **Create candidate root commits locally.** For each repository, create one
   orphan commit with the intended tree. Verify `git rev-list --count` is `1`,
   `git cat-file -p <commit>` has no `parent` line, the tree matches the
   classification manifest, and `main` and `develop` candidate refs name the
   same commit.
8. **Run isolated gates.** Run every repository's Section 7 gates in fresh
   clones. Before either version exists remotely, construct a read-only local
   Go module proxy from the candidate SDK tree with exact
   `v0.1.0-pre.2.info`, `.mod`, `.zip`, and `list` entries. With `GOWORK=off`,
   `GOPROXY=file://<candidate-proxy>`, and `GOSUMDB=off`, download and test the
   root and peers modules; reject any filesystem `replace`. Run `npm pack
   ./bus`, unpack that exact `@sessionbus/kit` tarball into an isolated DSH
   fixture's `node_modules/@sessionbus/kit`, and test DSH with registry access
   disabled. Do the analogous packed-kit consumer test for the OpenCode
   preview. Record SHA-256 for the proxy files and npm tarball. No gate may
   rely on the pre-split worktree or a published candidate.
9. **Preflight remote leases.** Re-read GitHub and Forgejo `main` and `develop`
   object IDs and compare them with the freeze manifest. Any movement aborts;
   do not refresh the lease silently. Prove both new peers repositories are
   truly empty: no heads and no tags. Prove tags
   `bus/sdk/go/v0.1.0-pre.2` and `kit-v0.1.0-pre.2` are absent from both
   Sessionbus remotes, and prove npm has no `@sessionbus/kit@0.1.0-pre.2`.
10. **Publish Sessionbus atomically.** Push its root commit to `main` and
    `develop` with explicit `--force-with-lease=<ref>:<frozen-oid>` and push the
    SDK submodule tag `bus/sdk/go/v0.1.0-pre.2` in the same atomic transaction.
    Repeat to Forgejo. Verify both branches, the tag, the root tree, and a
    normal-proxy `GOWORK=off` SDK consumer from fresh clones before continuing.
11. **Publish and verify the JavaScript kit.** Only after Step 10 is green,
    push `kit-v0.1.0-pre.2` to Forgejo first and verify the tag object, then
    push that exact tag object to GitHub to trigger trusted publishing. The
    existing workflow verifies tag/package equality, Node 24 and npm
    >=11.5.1, tests the package, and publishes
    `@sessionbus/kit@0.1.0-pre.2` with provenance. Wait until
    `npm view @sessionbus/kit@0.1.0-pre.2` returns the expected version,
    repository, provenance, and tarball integrity; download it and compare its
    SHA-512/integrity and file list with the Step 8 candidate. This npm publish
    is the first irreversible operation.
12. **Publish peers atomically.** After the kit registry verification, create
    `main` and `develop` at the one peers root commit on the truly empty GitHub
    and Forgejo repositories in atomic pushes. A nonempty target is an abort,
    not permission to force a seed away. Verify fresh clones build with
    `GOWORK=off` against the published SDK tag and test the OpenCode preview
    against the published kit. Do not create a binary or OpenCode release tag.
13. **Publish DSH atomically.** After the kit registry verification, push its
    one root commit to `main` and `develop` on GitHub and Forgejo with leases
    against the frozen heads. From fresh clones, install the registry
    `@sessionbus/kit@0.1.0-pre.2`, run DSH tests, and verify its pkg.pr.new
    preview. Do not create a DSH registry release tag.
14. **Cross-repository acceptance.** Run the daemon protocol/CLI suite, peers
    unit/race/vet/build and package tests, npm pack-file checks, DSH tests, facts
    citation checks against Forgejo, and all three former-brand/repository URL
    guards. Compare GitHub and Forgejo root-tree hashes for each repository.
15. **Retire heads, verify, and relock.** From the freeze manifest, delete
    every old non-`main`/non-`develop` GitHub branch with an explicit
    object-ID lease. On Forgejo, delete the corresponding old unprefixed heads;
    keep every verified `legacy-*` head. Do not move or delete historical tags.
    Re-fetch all remotes and prove no old development head survives under an
    unprefixed name. A failed deletion stops the sequence but never permits
    deleting its legacy ref. Immediately after remote verification, the owner
    disables force-push on `main` and `develop` in all three GitHub and Forgejo
    repositories, restores required checks/reviews, and confirms no temporary
    rewrite credential or bypass remains. Clone migration is not allowed to
    keep this rewrite window open.
16. **Start only from fresh clones.** Contributors archive old clones as
    read-only evidence and create fresh clones from the correct new GitHub
    repository, adding the corresponding Forgejo mirror. They verify clean
    tracked and untracked inventories, build/test once, and create any
    cross-repository development workspace outside the repositories. No hard
    reset or clean command is prescribed for an old clone: untracked obsolete
    source can survive a reset, and unpushed work belongs to its owner. The
    owner lifts the merge freeze only after the new-clone receipts are recorded.

## 9. Owner-only prerequisites and final actions

Before Step 0, the owner must:

- create or confirm `antst/sessionbus-peers` and Forgejo
  `ai/sessionbus-peers` are truly empty, with no branch, tag, or seed commit;
- keep temporary force-push authorization enabled for the six target branch
  names per hosting service—twelve refs total: `main` and `develop` in three
  repositories on GitHub and Forgejo—through the Step 15 retirement and
  verification, then re-lock immediately;
- authorize/provide the final GPL-3.0-only and MIT license texts and confirm the
  fixed Go/npm prerelease `0.1.0-pre.2`;
- configure repository secrets, trusted npm publishing, pkg.pr.new access,
  branch checks, and mirrors for the new repository boundaries;
- name the canonical stamped product-tip manifest used for peers assembly.

During Step 15, the owner must re-lock force-push, re-enable required reviews
and status checks, verify mirror/default-branch settings, and explicitly close
the rewrite window before contributors migrate clones. If the owner cannot
confirm those actions, the repositories remain frozen even when their code
gates are green.

## 10. Abort and rollback rule

Before a force-push, rollback is local: discard the candidate root commits and
leave all remotes untouched. After a force-push but before Step 11, do not
improvise a partial history restoration: stop, preserve the observed remote
refs, and have the owner choose either to complete the reviewed transaction or
restore the frozen object IDs from verified `legacy-*` refs using the same
explicit leases.

Publishing `@sessionbus/kit@0.1.0-pre.2` in Step 11 is irreversible: npm may
permit deprecation, but the version can never be reused or made unpublished by
this plan. It is deliberately the last irreversible boundary before the peers
and DSH roots are exposed. A failure after it freezes all repositories; the
owner may deprecate that exact package and issue a new reviewed prerelease, but
must not rewrite bytes under the published version or pretend rollback removed
it. Peer binaries, `@sessionbus/opencode`, and `@sessionbus/dsh` are previews in
the initial roots and therefore add no further release boundary here.
