# Products

Agent Sessions supports eight interactive peer products and nine worker-lane products. All product
sessions remain native: the product issues or accepts the session ID, owns the title and history,
and performs resume, prompting, interruption, and archive behavior through its own interfaces.

| Product | Peer alias | Lane alias | Catalog version policy |
| --- | --- | --- | --- |
| Codex | `codex-peer` | `codex-peer-lane` | minimum 0.151.0 |
| Claude Code | `claude-peer` | `claude-peer-lane` | minimum 2.1.252 |
| Grok Build | `grok-peer` | `grok-peer-lane` | minimum 1.0.5; accepted on 1.0.13 |
| Qwen Code | `qwen-peer` | `qwen-peer-lane` | minimum 0.22.0; validated on 0.22.3 and 0.23.0 |
| OpenCode | `opencode-peer` | `opencode-peer-lane` | exact 1.18.25 |
| Kilo Code | `kilo-peer` | `kilo-peer-lane` | exact 7.5.6 |
| Pi | `pi-peer` | `pi-peer-lane` | exact 0.84.4 |
| Oh My Pi | `omp-peer` | `omp-peer-lane` | exact 18.0.11 |
| DeepSeek Harness | — | `dsh-peer-lane` | exact 0.1.2-rc.1 |

The installer detects available products independently. A missing product does not prevent the
other integrations from being installed.

## Resume selectors

Every peer wrapper accepts one uniform `--resume [NATIVE_SELECTOR]` option. Agent Sessions only
translates the flag; it passes the optional selector verbatim and never lists or matches product
sessions in a peer resume path:

| Product | Resume behavior |
| --- | --- |
| Codex | `codex --remote … resume [SELECTOR]`; the remote client owns name/ID resolution, duplicate handling, and its bare native picker. |
| Claude | The optional selector passes to Claude's native resume flag unchanged. |
| Grok, Qwen | The optional selector passes to the product's native resume flag unchanged. Native name/title lookup is scoped to the current working directory or project, so resume must use the original cwd. |
| OpenCode, Kilo, Pi, OMP | The optional selector passes to the translated native resume flag unchanged. The product alone decides whether it is a valid ID, path, name, or picker request. |

Lane resume always resolves to one exact product session ID. Offline candidates are returned only
after the product confirms that ID still exists.

The standing real-product matrix requires one `--cwd` that already exists and has been approved
through every installed product's own trust prompt. Prefer a dedicated empty directory: products
may index native session history there and load project configuration or hooks under their native
defaults. The matrix never writes trust configuration or drives a trust prompt.

## Invocation-owned choices

Start and resume take their complete launch selections from the current invocation. If model,
agent, effort, permission, cwd, or groups are omitted, Agent Sessions sends no remembered value;
the product applies its own default. Product-specific options remain available as native arguments
unless the wrapper reserves them for a uniform translation.

Uniform opt-in options use the descriptor argument table:

| Wrapper option | Native products/surfaces |
| --- | --- |
| `--model VALUE` | Native product model flag or prompt field. Omission sends nothing. |
| `--agent NAME` | Claude, Grok, OpenCode, and Kilo. Other products reject before launch because they have no native agent selector. |
| `--effort VALUE` or `--reasoning-effort VALUE` | Codex, Claude, Grok, Pi, and OMP; OpenCode/Kilo lanes through their prompt variant; DSH when `--model` is also supplied. Qwen and unsupported peer surfaces reject before launch. |
| `--yolo` / `--no-yolo` | Translated to each product's native permissive or normal permission surface. Unsupported widening is never synthesized. |
| `--cd PATH` | The current invocation directory. Relative paths resolve from the caller's invocation cwd. |

OpenCode and Kilo pass optional agent/model/variant values on each prompt, not to their server
process. Grok applies effort through its native session mode. DSH selects its product-owned model
and reasoning effort together. No wrapper stores or mirrors a product default.

### Qwen 0.23.0 approval policy

Qwen owns the approval decision for actions prompted by a delivered peer message. In 0.23.0,
`yolo` acts; `auto` and the omitted-mode default block tool actions triggered solely by a
cross-session message; `auto-edit` and `default` prompt the human in the TUI; and `plan` rejects
the action. These are Qwen product rules, not Agent Sessions routing rules. Qwen documents the
five native modes in `lib/bundled/qc-helper/docs/features/approval-mode.md:3-31`, declares `auto`
as the default in `lib/bundled/qc-helper/docs/configuration/settings.md:374`, and implements the
cross-session authorization boundary in `lib/chunks/chunk-4F7GQGXB.js:50826-50840`.

On Qwen 0.23.0, a lane must use `permission_mode=bypass` (`--yolo`) to act on a steer. A
default-mode lane still completes its turn truthfully, but Qwen's policy prevents it from taking
the requested tool action.

## Native mechanics

- Codex lanes use the daemon-owned Codex App Server connection.
- Claude lanes use the native stream-JSON session and product replay acknowledgements.
- Grok lanes share the native leader/primary bootstrap with Grok peers.
- Qwen lanes use Qwen's ACP session surface.
- OpenCode and Kilo lanes use their native server, session, prompt, and event APIs.
- Pi and OMP lanes use their native JSONL RPC modes.
- DSH lanes run the installed Agent Sessions DSH profile as a native protocol-v1 client over the
  presence connection.

Adapters do not reproduce a product store or model its lifecycle. Where a native capability is
absent, the wrapper either supplies the smallest proven translation or rejects the operation
truthfully.
