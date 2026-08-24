# Qwen acceptance evidence template

Use this template for every live Linux, macOS, installation, lifecycle, crash, and federation
evidence report under this directory. A report is evidence for one exact source tree and one clearly
bounded cell; it is not a retrospective summary assembled from memory.

Linux and macOS reports for the same requirement MUST use the same commit and tree. Cross-builds are
not runtime evidence. Preserve raw command output separately when useful, but keep this report
self-contained enough for a reviewer to reproduce the identity, authority, mutation, and cleanup
claims without reading terminal scrollback.

## Evidence rules

- Record exact values. Do not abbreviate commit, tree, session, instance, process-start, strong-start,
  socket, or artifact identities in an authoritative assertion.
- Select processes for observation or signalling by exact PID plus process start and, when available,
  strong start. Recheck that identity immediately before a signal. A name, PID alone, command-text
  match, or `ps | head` result is not authority.
- Before removing a file or directory, recheck its physical path, filesystem type, owner, device,
  inode, mode, and the exact state that makes it attributable. A changed or unverifiable item is
  durable cleanup debt, not permission to delete it.
- Record the actual exit code of every credited command. Pipelines MUST use `pipefail` or capture the
  producer's status directly.
- Use state predicates to establish readiness and completion. Fixed sleeps and retries are not proof.
  Timed residue samples remain required after the terminal predicate is observed.
- A first genuine RED stops broader cells until its root cause is understood and a class-closing
  regression is available. Do not turn a RED green by editing state, widening cleanup, suppressing a
  finding, or increasing a timeout without causal evidence.
- Disclose every discarded, failed, or confounded attempt. Only the explicitly credited attempt may
  support the verdict.

## Credential and profile safety

Never copy, print, log, hash, diff, or otherwise expose a credential value, bearer token, API key,
cookie, OAuth payload, secure-storage value, or full credential file. Do not use a command that dumps
an environment or profile wholesale when either may contain secrets.

Permitted evidence is limited to:

- credential/provider configuration state as `ready`, `unknown`, or `unready`;
- non-secret provider or authentication-method labels explicitly returned by the readiness contract;
- existence/absence, filesystem metadata, and key names for credential-bearing stores, without file
  content or a content hash;
- secure-store metadata read without requesting the secret value;
- hashes of files proven non-secret before hashing, such as public manifests, settings with no secret
  fields, skills, installed binaries, and test-owned transcripts.

An authentication-file symlink protects its external target from write-through only while the
native client preserves the link. It is not proof that no credential copy exists: a client may
atomically replace the symlink with a regular credential file inside the test profile. After any
authenticated native launch, treat the entire supplied test profile as credential-bearing, keep it
owner-only, inspect credential paths by metadata only, and delete the validated dedicated profile
after extracting non-secret evidence. Never hash either the external credential target or a
materialized test copy.

For a Qwen profile, record every selector's literal value or explicit absence, the resolved physical
profile path, and whether selection is default or an explicit supported override. Do not infer an
unset value from shell defaults. A nondefault profile must show the same explicit selector on resume.
The exact `agent-sessions` plugin/skill identity must be verified in that selected profile before a
managed launch; integration in another profile is not credit.

## Per-cell report template

Copy the following sections into the evidence file for the cell. Replace every `<...>` marker or
write `not applicable — <reason>`; do not silently omit a field.

### 1. Cell identity and verdict

| Field | Exact value |
| --- | --- |
| Requirement/task/cell | `<ID and short name>` |
| Attempt ID | `<unique ID>` |
| Credited attempt | `<yes/no>` |
| Verdict | `<GREEN / RED-PRODUCT / RED-REGRESSION / BLOCKED-ENVIRONMENT / BLOCKED-AUTH / HARNESS-CONFOUNDED / NOT-RUN>` |
| Started/finished UTC | `<RFC 3339 timestamps>` |
| Host role | `<local / source / destination / hub>` |
| OS/architecture | `<exact OS release, kernel, architecture>` |
| Host ID/name | `<Agent Sessions host identity>` |
| Operator boundary | `<isolated roots, authorized mutations, prohibited mutations>` |
| First genuine RED | `<none, or exact first failing assertion and stop point>` |

State the expected invariant, the operation performed, and the observed outcome in two or three
sentences. A GREEN verdict must name the terminal predicate that completed the cell.

### 2. Source, signature, and installed identity

| Field | Exact value |
| --- | --- |
| Commit SHA | `<full SHA>` |
| Tree SHA | `<full SHA>` |
| Parent SHA | `<full SHA>` |
| Commit subject | `<subject>` |
| Signature result | `<cryptographic result, signing key fingerprint, signer>` |
| Trust-store result | `<trusted/untrusted/unknown; separate from cryptographic validity>` |
| Branch/detached state | `<value>` |
| Worktree baseline | `<clean, or exact pre-existing tracked/untracked paths>` |
| Installed version | `<exact version>` |
| Installed binary hashes | `<path and SHA-256 for every exercised Agent Sessions executable>` |
| Plugin identity/build | `<name, version/build, selected-profile install path>` |
| Hub/agent versions | `<exact host IDs, versions, executable hashes>` |

Any source or tree change invalidates later evidence. Do not describe an installed process as being
at the checked-out commit unless its executable version and hash corroborate that claim.

### 3. Toolchain and native clients

| Component | Exact path | Exact version/build | Notes |
| --- | --- | --- | --- |
| Go | `<path>` | `<version>` | `<GOOS/GOARCH if relevant>` |
| Repository-managed linter | `<path selected by Makefile>` | `<version>` | `<not a different system linter>` |
| Git | `<path>` | `<version>` | `<signature backend>` |
| Qwen Code | `<path>` | `<package and native version>` | `<minimum/live-contract result>` |
| Codex | `<path>` | `<version>` | `<if involved>` |
| Claude Code | `<path>` | `<version>` | `<if involved>` |
| Grok | `<path>` | `<version>` | `<if involved>` |
| Other required runtime | `<path>` | `<version>` | `<Node, shell, etc.>` |

Record native-client auto-updates as a boundary change. Evidence spanning two native versions is not
one homogeneous run and must be split or rejected.

### 4. Selected profile and non-secret state

| Item | Before value/absence | Resolved physical path | Safe baseline evidence | Expected mutation? |
| --- | --- | --- | --- | --- |
| Qwen profile selector CLI option | `<literal/absent>` | `<path/n/a>` | `<how corroborated>` | `<yes/no>` |
| Qwen profile environment selector | `<literal/absent>` | `<path/n/a>` | `<how corroborated>` | `<yes/no>` |
| Qwen active/default profile | `<default/explicit>` | `<path>` | `<device/inode/owner/mode>` | `<yes/no>` |
| Agent Sessions plugin manifest | `<present/absent>` | `<path>` | `<SHA-256, size, mtime>` | `<yes/no>` |
| MCP registration | `<present/absent>` | `<path or native inventory>` | `<non-secret identity only>` | `<yes/no>` |
| Four lane skills | `<exact inventory/absent>` | `<root>` | `<hashes>` | `<yes/no>` |
| Non-secret settings | `<present/absent>` | `<path>` | `<SHA-256, size, mtime>` | `<yes/no>` |
| Credential/provider state | `<ready/unknown/unready>` | `<store label only>` | `<method/provider label; no value>` | `no Agent Sessions mutation` |
| Secure storage | `<present/absent>` | `<store label only>` | `<metadata only>` | `no Agent Sessions mutation` |
| Native transcript inventory | `<count and exact test-owned IDs>` | `<root>` | `<non-secret hashes/mtimes>` | `<cell-specific>` |

List every secret-shaped field or file intentionally excluded from reads/hashes. Record exactly:

```text
Credential values read: NO
Credential values printed/logged: NO
Credential files copied: NO
Credential or provider configuration mutated by Agent Sessions: NO
Owner-wide permission/authentication settings broadened: NO
```

When the native client legitimately changes counters, cache, or project bookkeeping, identify the
native writer and list only non-secret changed key names. Do not restore concurrent owner state or
call an expected native mutation “unchanged.”

### 5. Ownership baseline before mutation

Record test-owned and preserved controls separately. Counts alone are insufficient; attach the exact
identity rows below or point to a bounded raw artifact whose SHA-256 is recorded.

#### 5.1 Processes

| Scope | Product/role | PID | Process start | Strong start | UID | Executable and SHA-256 | Parent/PGID/session | Cwd | Expected disposition |
| --- | --- | ---: | --- | --- | ---: | --- | --- | --- | --- |
| owned | `<role>` | `<pid>` | `<exact>` | `<exact/unsupported>` | `<uid>` | `<path, hash>` | `<exact>` | `<path>` | `<survive/exit/be killed>` |
| preserved | `<role>` | `<pid>` | `<exact>` | `<exact/unsupported>` | `<uid>` | `<path, hash>` | `<exact>` | `<path>` | `must survive` |

For a planned signal, also record the exact target identity immediately before the signal, the signal,
the sender's authorization, and whether escalation was required. Never use a loose command-text match.

#### 5.2 Sockets and listeners

| Scope | Path/address | `lstat` type | Device/inode | UID/mode | Owning PID/start | Listener/connection state | Expected disposition |
| --- | --- | --- | --- | --- | --- | --- | --- |
| owned | `<path>` | `<socket>` | `<exact>` | `<exact>` | `<exact>` | `<LISTEN/connected/private>` | `<remove/preserve>` |
| preserved | `<path>` | `<type>` | `<exact>` | `<exact>` | `<exact>` | `<state>` | `must survive` |

Inventory all host listeners relevant to the cell. For federation, record listener ownership on hub,
source, and destination, plus outbound connections. A Qwen lane must not introduce an undocumented
network listener or fallback transport.

#### 5.3 Registry rows, keys, preparations, and artifacts

| Scope | Path/catalog | Type | Device/inode | SHA-256 or exact revision | Session/host/product/instance | PID/start/socket | Status/groups/permission | Expected disposition |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| owned | `<path>` | `<row/key/preparation/file/dir>` | `<exact>` | `<non-secret hash/revision>` | `<exact IDs>` | `<exact>` | `<exact>` | `<remove/change/preserve>` |
| preserved | `<path>` | `<type>` | `<exact>` | `<non-secret hash/revision>` | `<exact IDs>` | `<exact>` | `<exact>` | `must survive unchanged` |

Include private input/event files, launch settings, lifecycle roots, stable/backend sockets, native
session rows, plugin rows, worktrees, pending notices, and cleanup-debt records applicable to the cell.

#### 5.4 Lane inventory

| Product | Active/all counts | Lane + native IDs | Status/turn | Manager PID/start | Worker PID/start/strong-start | Tool/MCP roots | Owner/persistent/notify | Groups | Cleanup debt |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen | `<n/n>` | `<full IDs>` | `<exact>` | `<exact/none>` | `<exact/none>` | `<exact paths/none>` | `<exact>` | `<exact>` | `<none/exact debt>` |
| Codex | `<n/n>` | `<controls>` | `<exact>` | `<n/a>` | `<exact/none>` | `<exact/none>` | `<exact>` | `<exact>` | `<none/exact debt>` |
| Claude | `<n/n>` | `<controls>` | `<exact>` | `<exact/none>` | `<exact/none>` | `<exact/none>` | `<exact>` | `<exact>` | `<none/exact debt>` |
| Grok | `<n/n>` | `<controls>` | `<exact>` | `<exact/none>` | `<exact/none>` | `<exact/none>` | `<exact>` | `<exact>` | `<none/exact debt>` |

#### 5.5 Preserved-state checklist

- [ ] Unrelated owner processes and native sessions have exact identities recorded.
- [ ] Host agent, hub, supervisor, App Server, and service-row identities are recorded.
- [ ] Unrelated rows, keys, sockets, listeners, lifecycle roots, worktrees, and lane records are recorded.
- [ ] Owner settings and selected-profile non-secret files have safe hashes/metadata recorded.
- [ ] Unrelated test-owned and owner-owned transcript identities are recorded without secret content.
- [ ] Credential stores have metadata-only baselines; no secret value or credential-file hash was read.
- [ ] Every intentionally seeded stale/changed-type control has an exact type, owner, device, inode,
      and expected disposition.
- [ ] The filesystem root for destructive work is a validated dedicated path, not a symlink, mount,
      workspace root, home directory, or unresolved variable.

### 6. Operation and assertions

For each credited operation record:

| Step | Exact command/API request | stdin/payload SHA-256 or safe summary | Started UTC | Exit/response | State predicate | Evidence artifact |
| --- | --- | --- | --- | ---: | --- | --- |
| `<n>` | `<exact argv or typed request>` | `<non-secret>` | `<time>` | `<code/type>` | `<predicate and observed value>` | `<path + SHA-256>` |

Then record:

- exact native and Agent Sessions session/turn/message IDs;
- expected and observed permission launch preference plus effective initial native mode;
- exact groups, source-parent anchor, and destination-child anchor;
- message recipients and correlated delivery/acknowledgment IDs;
- collection cursor/debt before and after each wait;
- native archive/unarchive and compensation state;
- every filesystem/process mutation attributable to the operation;
- the first failed assertion, if any, before attempting further work.

For a remote cell, duplicate Sections 2–5 for source and destination, and record the exact hub route,
capability advertisement, target placement, terminal notice, and emitted `Collect:` line. Execute that
line byte-for-byte and record its SHA-256; a corrected or augmented command does not test the emitted
instruction.

### 7. Terminal and timed cleanup samples

Define `t=return` as the monotonic instant when the credited command/API returns after its terminal
state predicate. Record the actual monotonic offset of every sample; do not claim an exact delay from
a wall-clock timestamp.

| Sample | Actual monotonic offset | Attributable live processes | Rows/keys/preparations | Sockets/listeners | Temp/input/event/settings files | Worktrees | Pending notices/collection debt | Preserved controls | Verdict |
| --- | ---: | --- | --- | --- | --- | --- | --- | --- | --- |
| return | `<0.xxx s>` | `<exact IDs or zero>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact survivors>` | `<pass/fail>` |
| +1 s | `<>=1.000 s>` | `<exact IDs or zero>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact survivors>` | `<pass/fail>` |
| +5 s | `<>=5.000 s>` | `<exact IDs or zero>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact survivors>` | `<pass/fail>` |
| +10 s | `<>=10.000 s>` | `<exact IDs or zero>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact survivors>` | `<pass/fail>` |
| +30 s | `<>=30.000 s>` | `<exact IDs or zero>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact>` | `<exact survivors>` | `<pass/fail>` |

Success requires zero attributable live process, row, socket, key, temporary setting, input/event
file, helper, worktree, or pending notice, and no unrecorded cleanup debt. An expected retained durable
archived record is not residue only when the contract explicitly requires it and its normalized state
is recorded. Every unrelated control must be byte-for-byte or identity-equivalent intact at every
sample.

### 8. Post-cell nonmutation and exact deltas

| Item | Before | After | Exact delta | Writer/authority | Classification |
| --- | --- | --- | --- | --- | --- |
| Processes | `<baseline>` | `<final>` | `<exact>` | `<component>` | `<expected/unexpected>` |
| Sockets/listeners | `<baseline>` | `<final>` | `<exact>` | `<component>` | `<expected/unexpected>` |
| Rows/keys/preparations | `<baseline>` | `<final>` | `<exact>` | `<component>` | `<expected/unexpected>` |
| Lane catalogs/debt | `<baseline>` | `<final>` | `<exact>` | `<component>` | `<expected/unexpected>` |
| Qwen profile | `<safe evidence>` | `<safe evidence>` | `<non-secret keys only>` | `<Qwen/Agent Sessions/unknown>` | `<expected/RED/debt>` |
| Owner/global settings | `<hash/metadata>` | `<hash/metadata>` | `<exact>` | `<writer/unknown>` | `<preserved/concurrent/unexpected>` |
| Credential stores | `<metadata only>` | `<metadata only>` | `<exact>` | `<unknown if changed>` | `<preserved/RED>` |
| Native transcripts | `<inventory>` | `<inventory>` | `<exact test-owned change>` | `<native client>` | `<expected/unexpected>` |

Do not restore or overwrite a concurrent owner change. If an owner file changes outside the test
window or by a separately identified owner process, report it as concurrent evidence with timestamps;
do not call it byte-identical and do not attribute it to the cell without proof.

### 9. RED, blocker, and confound record

Complete this section for every non-GREEN attempt and retain it even after a later pass.

| Field | Exact evidence |
| --- | --- |
| Classification | `<product defect / regression / platform difference / environment / auth / toolchain skew / harness error / confounded>` |
| Triggering conditions | `<exact preconditions and operation>` |
| First failing invariant | `<expected versus observed>` |
| Raw diagnostic | `<bounded verbatim diagnostic and source>` |
| Scope stopped | `<cells not started after first genuine RED>` |
| State preserved | `<artifacts/processes/logs retained for RCA>` |
| Causal mechanism | `<proved RCA or pending>` |
| Why prior tests missed it | `<evidence, not speculation>` |
| Class-closing regression | `<test path/name or not yet available>` |
| Cleanup/boundary result | `<exact collateral and debt check>` |
| Retry disposition | `<not run / discarded / separately credited attempt ID>` |

A harness-confounded attempt must say precisely which assertion became vacuous or which wrong identity,
timing assumption, pipeline status, stale version, prompt choice, or manual intervention invalidated
it. Mark every assertion from that attempt `NO CREDIT`. A later clean rerun gets a new attempt ID and
does not erase the confound.

### 10. Reviewer conclusion

- [ ] Commit, tree, signature, installed executable, and plugin identities agree.
- [ ] Linux and macOS reports use the same commit/tree and equivalent cell contract.
- [ ] Exact authority was rechecked before every signal, mutation, archive, and deletion.
- [ ] Required return/+1/+5/+10/+30 samples are complete.
- [ ] Owned state reached the documented terminal condition with no unrecorded debt.
- [ ] Every preserved control survived.
- [ ] Credential values were never read, copied, printed, logged, or hashed.
- [ ] Profile selection and selected-profile integration were proved by value or explicit absence.
- [ ] Every rejected, RED, blocked, and confounded attempt is disclosed and not credited.
- [ ] Any genuine RED has an evidence-backed RCA and class-closing regression before a later GREEN is
      accepted.
- [ ] The report identifies raw evidence artifacts and their non-secret SHA-256 values.

Reviewer: `<name/session>`
Review UTC: `<timestamp>`
Decision: `<accepted / rejected / more evidence required>`
Reason: `<concise evidence-based reason>`

## Two-platform comparison summary

Each paired Linux/macOS acceptance set should finish with this table in one of the two reports or a
small index report that links both.

| Field | Linux | macOS | Match/difference classification |
| --- | --- | --- | --- |
| Commit/tree | `<full values>` | `<full values>` | `<must match>` |
| Signature/key | `<exact>` | `<exact>` | `<must match>` |
| Agent Sessions versions/hashes | `<exact>` | `<exact>` | `<platform binaries may hash differently>` |
| Go/linter/native versions | `<exact>` | `<exact>` | `<recorded difference>` |
| Qwen live contract | `<exact>` | `<exact>` | `<equivalent/genuine difference>` |
| Profile selection/integration | `<exact>` | `<exact>` | `<equivalent/genuine difference>` |
| Permission mapping | `<exact>` | `<exact>` | `<must satisfy same class>` |
| Lifecycle/terminal result | `<exact>` | `<exact>` | `<must satisfy same contract>` |
| Cleanup samples | `<return/+1/+5/+10/+30>` | `<return/+1/+5/+10/+30>` | `<both zero-residue>` |
| Preserved-state result | `<exact>` | `<exact>` | `<both preserve controls>` |
| Platform-specific observation | `<none/details>` | `<none/details>` | `<product/platform/RCA>` |
| Final verdict | `<verdict>` | `<verdict>` | `<both GREEN required>` |

A genuine platform difference is documented and tested; it is not a waiver. Neither platform can be
credited from the other's run.
