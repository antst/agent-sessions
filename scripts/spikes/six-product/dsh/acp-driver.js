#!/usr/bin/env node

import fs from 'node:fs'
import net from 'node:net'
import { spawn } from 'node:child_process'
import path from 'node:path'

const [dshBin, dshHome, workspace, controlSocket, pluginLog, sandboxScript, resultPath] = process.argv.slice(2)
if (![dshBin, dshHome, workspace, controlSocket, pluginLog, sandboxScript, resultPath].every(Boolean)) {
  throw new Error('usage: acp-driver.js DSH_BIN DSH_HOME WORKSPACE SOCKET LOG SANDBOX_SCRIPT RESULT')
}

const transcriptPath = `${resultPath}.transcript.jsonl`
const startedAt = Date.now()
const transcript = (direction, payload) => {
  fs.appendFileSync(transcriptPath, `${JSON.stringify({
    at_ms: Date.now() - startedAt,
    direction,
    payload,
  })}\n`, { mode: 0o600 })
}

const childEnv = {
  HOME: process.env.HOME,
  USER: process.env.USER,
  LOGNAME: process.env.LOGNAME,
  PATH: process.env.PATH,
  LANG: process.env.LANG || 'C.UTF-8',
  DSH_HOME: dshHome,
  DSH_S2_CONTROL_SOCKET: controlSocket,
  DSH_S2_LOG: pluginLog,
  DSH_S2_SANDBOX_SCRIPT: sandboxScript,
  AGENT_SESSIONS_COMPONENT_SOCKET: controlSocket,
}

const child = spawn(dshBin, ['--profile', 'acp'], {
  cwd: workspace,
  env: childEnv,
  stdio: ['pipe', 'pipe', 'pipe'],
})
child.stdout.setEncoding('utf8')
child.stderr.setEncoding('utf8')

let stdoutBuffer = ''
let stderr = ''
let nextID = 1
const pending = new Map()
const notifications = []

child.stderr.on('data', (chunk) => {
  stderr += chunk
  transcript('stderr', chunk)
})

child.stdout.on('data', (chunk) => {
  stdoutBuffer += chunk
  let newline
  while ((newline = stdoutBuffer.indexOf('\n')) >= 0) {
    const line = stdoutBuffer.slice(0, newline)
    stdoutBuffer = stdoutBuffer.slice(newline + 1)
    if (!line.trim()) continue
    const message = JSON.parse(line)
    transcript('from-dsh', message)
    if (message.id !== undefined && pending.has(message.id)) {
      const operation = pending.get(message.id)
      pending.delete(message.id)
      if (message.error) operation.reject(Object.assign(new Error(message.error.message), { rpc: message.error }))
      else operation.resolve(message.result)
    } else if (message.method) {
      notifications.push(message)
    }
  }
})

const exited = new Promise((resolve) => child.on('exit', (code, signal) => resolve({ code, signal })))

function request(method, params) {
  const id = nextID
  nextID += 1
  const message = { jsonrpc: '2.0', id, method, params }
  transcript('to-dsh', message)
  child.stdin.write(`${JSON.stringify(message)}\n`)
  return new Promise((resolve, reject) => pending.set(id, { resolve, reject }))
}

function notify(method, params) {
  const message = { jsonrpc: '2.0', method, params }
  transcript('to-dsh', message)
  child.stdin.write(`${JSON.stringify(message)}\n`)
}

async function until(label, predicate, timeout = 15000) {
  const deadline = Date.now() + timeout
  let last
  while (Date.now() < deadline) {
    last = await predicate()
    if (last) return last
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  throw new Error(`${label} timed out; last=${JSON.stringify(last)}`)
}

function control(message) {
  return new Promise((resolve, reject) => {
    const socket = net.connect(controlSocket)
    let buffer = ''
    const timer = setTimeout(() => {
      socket.destroy()
      reject(new Error(`control timeout for ${message.op}`))
    }, 5000)
    socket.setEncoding('utf8')
    socket.on('connect', () => socket.write(`${JSON.stringify(message)}\n`))
    socket.on('data', (chunk) => {
      buffer += chunk
      if (!buffer.includes('\n')) return
      clearTimeout(timer)
      const response = JSON.parse(buffer.slice(0, buffer.indexOf('\n')))
      socket.end()
      transcript('control', { request: message, response })
      if (!response.ok) reject(new Error(response.error))
      else resolve(response.result)
    })
    socket.on('error', (error) => {
      clearTimeout(timer)
      reject(error)
    })
  })
}

const readPluginEvents = () => fs.existsSync(pluginLog)
  ? fs.readFileSync(pluginLog, 'utf8').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line))
  : []

const eventsFor = (sessionID, event) => readPluginEvents()
  .filter((entry) => entry.session_id === sessionID && (!event || entry.event === event))

const statusIs = (sessionID, status) => async () => {
  try {
    const listed = await control({ op: 'list' })
    return listed.sessions.find((session) => session.session_id === sessionID && session.status === status)
  } catch (error) {
    if (error.code === 'ENOENT' || error.code === 'ECONNREFUSED') return false
    throw error
  }
}

let sessionID
const result = {
  initialized: false,
  session_listed: false,
  idle_followup: false,
  busy_steer: false,
  completion_observed: false,
  parent_facade: 'native_registered_tool',
  parent_tool_exact_session: false,
  sandbox_home_socket: false,
  dsh_session_id_witness: false,
  cancel_notification: false,
  acp_busy_prompt_rejected: false,
  cancel_request_rejected: false,
  projcache_not_liveness: false,
  projcache_sample: null,
  session_id: null,
}

try {
  const initialized = await request('initialize', {
    protocolVersion: 1,
    clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } },
    clientInfo: { name: 'agent-sessions-s2', version: '0.0.0' },
  })
  result.initialized = initialized.protocolVersion === 1

  await until('component socket', async () => fs.existsSync(controlSocket))
  const created = await request('session/new', { cwd: workspace, mcpServers: [] })
  sessionID = created.sessionId
  result.session_id = sessionID
  const listed = await until('Cordis session enumeration', async () => {
    const current = await control({ op: 'list' })
    return current.sessions.find((session) => session.session_id === sessionID)
  })
  result.session_listed = listed.status === 'idle'

  await control({ op: 'followup', session_id: sessionID, text: 'IDLE_FOLLOWUP' })
  await until('idle followup running', statusIs(sessionID, 'running'))
  await until('idle followup completion', statusIs(sessionID, 'idle'))
  result.idle_followup = eventsFor(sessionID, 'mock.request')
    .some((event) => event.user_text.includes('IDLE_FOLLOWUP'))

  await control({ op: 'followup', session_id: sessionID, text: 'BUSY_BASE' })
  await until('busy base running', statusIs(sessionID, 'running'))
  const requestCountBeforeSteer = eventsFor(sessionID, 'mock.request').length
  await control({ op: 'steer', session_id: sessionID, text: 'BUSY_STEER' })
  await until('busy steer consumed', async () => eventsFor(sessionID, 'mock.request')
    .slice(requestCountBeforeSteer)
    .some((event) => event.user_text.includes('BUSY_STEER')), 10000)
  await until('busy steer completion', statusIs(sessionID, 'idle'))
  result.busy_steer = true

  await control({ op: 'followup', session_id: sessionID, text: 'CALL_PARENT_TOOL' })
  await until('native parent tool', async () => eventsFor(sessionID, 'parent_tool.execute').length > 0)
  await until('parent tool completion', statusIs(sessionID, 'idle'))
  result.parent_tool_exact_session = eventsFor(sessionID, 'parent_tool.execute')
    .some((event) => event.session_id === sessionID && event.marker === 'NATIVE_TOOL_OK')

  await control({ op: 'followup', session_id: sessionID, text: 'SANDBOX_SOCKET_PROBE' })
  await until('sandbox socket proof', async () => eventsFor(sessionID, 'sandbox.socket').length > 0)
  await until('sandbox probe completion', statusIs(sessionID, 'idle'))
  result.sandbox_home_socket = eventsFor(sessionID, 'sandbox.socket')
    .some((event) => event.socket_path === controlSocket)
  result.dsh_session_id_witness = result.sandbox_home_socket

  const cancelPrompt = request('session/prompt', {
    sessionId: sessionID,
    prompt: [{ type: 'text', text: 'BUSY_CANCEL' }],
  })
  await until('cancel prompt running', statusIs(sessionID, 'running'))
  await until('cancel prompt native dispatch', async () => eventsFor(sessionID, 'mock.request')
    .some((event) => event.user_text.includes('BUSY_CANCEL')))
  const cachePath = path.join(dshHome, 'storages', 'session_projcache', 'sessions', `${sessionID}.json`)
  if (fs.existsSync(cachePath)) {
    const cache = JSON.parse(fs.readFileSync(cachePath, 'utf8'))
    result.projcache_sample = cache.turnBoundary ?? null
    result.projcache_not_liveness = cache.turnBoundary?.openTurnStartSeq == null
  } else {
    result.projcache_sample = { absent_while_agent_status: 'running' }
    result.projcache_not_liveness = true
  }
  try {
    await request('session/prompt', {
      sessionId: sessionID,
      prompt: [{ type: 'text', text: 'ACP_BUSY_SECOND' }],
    })
  } catch (error) {
    result.acp_busy_prompt_rejected = error.rpc?.code === -32602
  }
  notify('session/cancel', { sessionId: sessionID })
  const cancelled = await cancelPrompt
  result.cancel_notification = cancelled.stopReason === 'cancelled'
  try {
    await request('session/cancel', { sessionId: sessionID })
  } catch (error) {
    result.cancel_request_rejected = error.rpc?.code === -32601
  }
  await until('cancel convergence', statusIs(sessionID, 'idle'))

  const sessionEvents = eventsFor(sessionID, 'session.event')
  result.completion_observed = eventsFor(sessionID, 'agent.turn-stopping').length >= 3
    && sessionEvents.some((event) => event.session_event === 'turn/end')

  try { await request('session/close', { sessionId: sessionID }) } catch {}
} finally {
  child.stdin.end()
  const terminal = await Promise.race([
    exited,
    new Promise((resolve) => setTimeout(() => {
      child.kill('SIGTERM')
      resolve({ code: null, signal: 'SIGTERM-timeout' })
    }, 5000)),
  ])
  result.child_terminal = terminal
  result.stderr_lines = stderr.split('\n').filter(Boolean).slice(-20)
}

const required = [
  'initialized',
  'session_listed',
  'idle_followup',
  'busy_steer',
  'completion_observed',
  'parent_tool_exact_session',
  'sandbox_home_socket',
  'dsh_session_id_witness',
  'cancel_notification',
  'acp_busy_prompt_rejected',
  'cancel_request_rejected',
  'projcache_not_liveness',
]
result.pass = required.every((key) => result[key] === true)
result.required = required
fs.writeFileSync(resultPath, `${JSON.stringify(result, null, 2)}\n`, { mode: 0o600 })
if (!result.pass) process.exitCode = 1
