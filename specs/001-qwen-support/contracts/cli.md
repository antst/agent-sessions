# CLI Contract

## `qwen-peer`

```text
qwen-peer [wrapper options] [native Qwen options]
qwen-peer --resume <exact-uuid-or-unique-agent-sessions-name> [wrapper options]
```

Wrapper-owned options:

| Option | Meaning |
|---|---|
| `-n, --name NAME` | Stable Agent Sessions name. |
| `-g, --group GROUP` | Repeatable explicit group. |
| `--inherit-groups` / `--no-inherit-groups` | Common immediate-parent group policy. |
| `--yolo` | Requests native Qwen yolo as the initial approval mode. |
| `--no-yolo` | Translates exactly to native `--approval-mode default` and stores that resume default. |
| `--qwen-home ABSOLUTE_PATH` | Explicit native Qwen profile/state root. |
| `--runtime-dir PATH`, `--state-dir PATH` | Existing Agent Sessions runtime/state selection. |
| `-h, --help` | Shows every wrapper option plus the native-help boundary. |

Rules:

- With no permission option, the wrapper preserves Qwen's native initial-mode behavior. `--yolo`
  requests native yolo and `--no-yolo` translates exactly to native `--approval-mode default`.
  With no wrapper permission choice, a supported native `--approval-mode MODE` passes through
  unchanged and its exact requested mode is stored as the resume default. Current mode remains
  unknown unless Qwen publishes a supported native event.
- Repeated or contradictory wrapper choices, and any combination of a wrapper permission choice
  with native `--approval-mode`, fail with exit 2 before preparation, catalog, profile, or native
  process mutation. Agent Sessions never silently chooses precedence, even when choices are
  semantically equivalent.
- After publication, native Qwen arguments, `/approval-mode`, Shift+Tab, and other supported Qwen
  controls behave normally and may enter or leave yolo. Agent Sessions does not intercept or forbid
  those changes.
- The durable launch preference is a resume default, not a claim about the current mode and not an
  Agent Sessions authorization boundary.
- `--resume` accepts exact UUID or a unique durable Agent Sessions name. The wrapper resolves names,
  then passes exact `--resume UUID` to Qwen.
- Bare/native resume picker, `--continue`, and `--fork-session` are rejected on managed paths.
- Explicit `--qwen-home` must match the durable profile on resume. Missing, relative, or mismatched
  selection fails before catalog/native mutation.
- All managed arguments are inserted before native `--`; caller content after `--` is untouched.

Exit behavior:

| Exit | Meaning |
|---|---|
| `0` | Native session exited and attributable cleanup completed. |
| `1` | Runtime/readiness/native failure; diagnostic identifies the failed precondition. |
| `2` | Invalid CLI combination, including an unsupported initial permission-mode request. |
| `130` | Interrupted session after exact cleanup. |

## `qwen-peer-lane`

Common command surface:

```text
qwen-peer-lane doctor [--json] [common/profile/permission options]
qwen-peer-lane list [--all] [--json]
qwen-peer-lane run [lane options] -- <prompt-or-command>
qwen-peer-lane start [lane options]
qwen-peer-lane resume <thread-id-or-unique-name> [lane options]
qwen-peer-lane wait <thread-id-or-unique-name>
qwen-peer-lane status <thread-id-or-unique-name>
qwen-peer-lane interrupt <thread-id-or-unique-name>
qwen-peer-lane archive <thread-id-or-unique-name>
```

Lane options reuse the common contract:

- `--name`, `-g/--group`, `--inherit-groups`, `--no-inherit-groups`
- `--persistent`, `--notify`, `--no-notify`
- `--auto-archive`, `--no-auto-archive`, `--auto-archive-delay`
- `--yolo`, `--no-yolo`
- `--qwen-home ABSOLUTE_PATH`
- `-C/--cwd`
- machine-readable JSON output where existing lane commands provide it

Remote execution remains:

```text
peer-federator lane \
  -runtime-dir <source-agent-runtime> \
  -host <destination-host> \
  -product qwen -- <qwen-peer-lane command and options>
```

The federator owns remote lifecycle flags and rejects caller-supplied lifecycle overrides exactly as
for other remote lanes. The emitted `Collect:` command includes the source runtime directory and is
runnable verbatim.

## Doctor JSON

Doctor returns a stable object with at least:

```json
{
  "ok": true,
  "product": "qwen",
  "qwen_available": true,
  "qwen_path": "/absolute/path/qwen",
  "qwen_version": "0.21.15",
  "minimum_version": "0.21.15",
  "minimum_version_ok": true,
  "profile": {"qwen_home_set": false, "qwen_runtime_dir_set": false},
  "integration_ready": true,
  "auth_state": "unknown",
  "workspace_trust": "trusted",
  "interactive_contract": "ready",
  "acp_contract": "ready",
  "archive_contract": "ready",
  "requested_initial_mode": "native_default",
  "expected_initial_mode": "default",
  "current_native_mode": "unknown",
  "issues": []
}
```

`auth_state` describes only non-secret credential/provider configuration evidence available without a
session. It may remain `unknown`; actual provider authentication and `current_native_approval_mode` are
managed-launch admission facts and are not claimed by doctor. No secret value, token, credential
body, or owner setting body is emitted.

## Help completeness

Every parsed wrapper option must be present in the corresponding help output. A table-driven test
walks all product descriptors and common option descriptors. Native Qwen root help is not used to
infer hidden-but-supported native flags.
