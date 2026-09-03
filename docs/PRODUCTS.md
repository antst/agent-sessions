# Products

Agent Sessions supports eight interactive peer products and nine worker-lane products. All product
sessions remain native: the product issues or accepts the session ID, owns the title and history,
and performs resume, prompting, interruption, and archive behavior through its own interfaces.

| Product | Peer alias | Lane alias | Catalog version policy |
| --- | --- | --- | --- |
| Codex | `codex-peer` | `codex-peer-lane` | minimum 0.151.0 |
| Claude Code | `claude-peer` | `claude-peer-lane` | minimum 2.1.252 |
| Grok Build | `grok-peer` | `grok-peer-lane` | minimum 1.0.5; accepted on 1.0.13 |
| Qwen Code | `qwen-peer` | `qwen-peer-lane` | minimum 0.22.0; accepted on 0.22.3 |
| OpenCode | `opencode-peer` | `opencode-peer-lane` | exact 1.18.25 |
| Kilo Code | `kilo-peer` | `kilo-peer-lane` | exact 7.5.6 |
| Pi | `pi-peer` | `pi-peer-lane` | exact 0.84.4 |
| Oh My Pi | `omp-peer` | `omp-peer-lane` | exact 18.0.11 |
| DeepSeek Harness | — | `dsh-peer-lane` | exact 0.1.2-alpha.5 |

The installer detects available products independently. A missing product does not prevent the
other integrations from being installed.

## Resume selectors

Every peer wrapper accepts one uniform `--resume NAME_OR_ID` option. Agent Sessions uses the
product's native selector wherever it can:

| Product | Resume behavior |
| --- | --- |
| Claude, Grok, Qwen | Name or ID passes to the native selector; the product owns duplicate handling. |
| Codex | Exact IDs pass natively. Names are resolved from product rows because Codex does not expose the native selector's chosen thread to an external parent. Ambiguity opens a picker on a terminal or prints candidates without a terminal. |
| OpenCode, Kilo | The native selector accepts an exact session ID. Name lookup uses the product session list and the shared picker. |
| Pi, OMP | The native selector accepts an exact UUID or path/prefix. Name lookup uses the product session list and the shared picker. |

Lane resume always resolves to one exact product session ID. Offline candidates are returned only
after the product confirms that ID still exists.

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
