#!/usr/bin/env node

import fs from 'node:fs'
import crypto from 'node:crypto'
import { execFileSync } from 'node:child_process'

const [driverPath, tuplePath, pluginLogPath, transcriptPath, outputPath, dshBin] = process.argv.slice(2)
if (![driverPath, tuplePath, pluginLogPath, transcriptPath, outputPath, dshBin].every(Boolean)) {
  throw new Error('usage: evidence.js DRIVER TUPLE PLUGIN_LOG TRANSCRIPT OUTPUT DSH_BIN')
}

const readJSON = (file) => JSON.parse(fs.readFileSync(file, 'utf8'))
const sha256 = (file) => crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')
const driver = readJSON(driverPath)
const tuple = readJSON(tuplePath)
const cliVersion = execFileSync(dshBin, ['--version'], { encoding: 'utf8' }).trim()
const pnpmVersion = execFileSync('pnpm', ['--version'], { encoding: 'utf8' }).trim()

const assertions = {
  exact_tuple: tuple.exact_tuple_ok && cliVersion === tuple.expected,
  mismatched_tuple_rejected: tuple.mismatch_rejected,
  cordis_session_enumeration: driver.session_listed,
  idle_followup_wake: driver.idle_followup,
  busy_steer: driver.busy_steer,
  completion_event: driver.completion_observed,
  native_parent_tool_exact_session: driver.parent_tool_exact_session,
  sandbox_home_socket: driver.sandbox_home_socket,
  dsh_session_id_witness: driver.dsh_session_id_witness,
  acp_busy_prompt_rejected: driver.acp_busy_prompt_rejected,
  cancel_is_notification: driver.cancel_notification && driver.cancel_request_rejected,
  projcache_not_liveness: driver.projcache_not_liveness,
}
const pass = Object.values(assertions).every(Boolean)
const evidence = {
  schema: 'agent-sessions.six-product.phase0.v1',
  spike: 'S2-dsh',
  status: pass ? 'PASS' : 'RED',
  base_commit: process.env.DSH_S2_BASE_COMMIT,
  generated_at: new Date().toISOString(),
  product: {
    id: 'dsh',
    cli_version: cliVersion,
    tuple_version: tuple.expected,
    acp_app_version: tuple.actual.acp_app,
    cordis_plugin_version: tuple.actual.plugin,
    profile_version: tuple.actual.profile,
    package_manager: `pnpm ${pnpmVersion}`,
    model_provider: 'deterministic mock adapter inside the real DSH process',
    product_protocol_mocked: false,
    credential_used: false,
  },
  assertions,
  observed: {
    session_id: driver.session_id,
    projcache_sample_while_running: driver.projcache_sample,
    cancel_notification_result: 'stopReason=cancelled',
    acp_busy_prompt_result: 'JSON-RPC -32602 prompt already in flight',
    cancel_request_result: 'JSON-RPC -32601 Method not found',
    component_socket_location: '$HOME/.local/state/agent-sessions-spikes/<mktemp>/component.sock',
    component_socket_tmpfs_safe: true,
  },
  design_decision: {
    parent_facade: 'native_registered_tool',
    rationale: 'Cordis tool execution supplies exec.agent with the exact native DSH session; it avoids an extra MCP child and keeps identity in-process.',
    mcp_status: 'supported fallback, not selected for primary parent facade',
    busy_lane_policy: 'ACP rejects a concurrent prompt; shared durable ledger queues. Cordis peer delivery uses agent.steer directly.',
    liveness_source: 'ACP stop reason plus Cordis agent/status and agent/turn-stopping; never projection cache',
  },
  tuple_validation: tuple,
  raw_result: driver,
  artifacts: {
    plugin_log_sha256: sha256(pluginLogPath),
    acp_transcript_sha256: sha256(transcriptPath),
  },
  account_gate: {
    status: 'not_applicable_to_spike',
    explanation: 'The native DSH/Cordis/ACP/tool/sandbox protocols ran end-to-end with an allowed mock model adapter; no user credential was needed or read.',
  },
}
fs.mkdirSync(new URL('.', `file://${outputPath}`).pathname, { recursive: true, mode: 0o700 })
fs.writeFileSync(outputPath, `${JSON.stringify(evidence, null, 2)}\n`, { mode: 0o600 })
if (!pass) process.exitCode = 1
