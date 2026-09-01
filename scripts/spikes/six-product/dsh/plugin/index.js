import fs from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { defineTool } from '@deepseek-ai/dsh-tools'

export const name = 'agent-sessions-s2'
export const inject = ['agents', 'llm', 'tools']

const sleep = (milliseconds, signal) => new Promise((resolve, reject) => {
  if (signal?.aborted) {
    reject(signal.reason)
    return
  }
  const timer = setTimeout(resolve, milliseconds)
  const onAbort = () => {
    clearTimeout(timer)
    reject(signal.reason)
  }
  signal?.addEventListener('abort', onAbort, { once: true })
})

function textOf(content) {
  if (!Array.isArray(content)) return ''
  return content
    .filter((block) => block && block.type === 'text')
    .map((block) => block.text)
    .join('')
}

const directives = [
  'IDLE_FOLLOWUP',
  'BUSY_BASE',
  'BUSY_STEER',
  'CALL_PARENT_TOOL',
  'SANDBOX_SOCKET_PROBE',
  'BUSY_CANCEL',
]

function latestDirective(messages) {
  for (let messageIndex = messages.length - 1; messageIndex >= 0; messageIndex -= 1) {
    const content = messages[messageIndex]?.content
    if (!Array.isArray(content)) continue
    for (let blockIndex = content.length - 1; blockIndex >= 0; blockIndex -= 1) {
      const block = content[blockIndex]
      if (block?.type !== 'text') continue
      const directive = directives.find((candidate) => block.text.includes(candidate))
      if (directive) return directive
    }
  }
  return ''
}

function hasToolResult(messages, prefix) {
  return messages.some((message) => Array.isArray(message?.content)
    && message.content.some((block) => block?.type === 'tool-result'
      && String(block.toolCallId).startsWith(prefix)))
}

function replyChunks(text) {
  return [
    { type: 'block-start', index: 0, blockType: 'text' },
    { type: 'text-delta', index: 0, text },
    { type: 'block-end', index: 0, block: { type: 'text', text } },
    { type: 'usage', usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 } },
    { type: 'finish', reason: { kind: 'stop' } },
  ]
}

function toolChunks(id, toolName, argumentsObject) {
  const argumentsText = JSON.stringify(argumentsObject)
  return [
    { type: 'block-start', index: 0, blockType: 'tool-call' },
    {
      type: 'tool-call-delta',
      index: 0,
      id,
      name: toolName,
      argumentsDelta: argumentsText,
    },
    {
      type: 'block-end',
      index: 0,
      block: { type: 'tool-call', id, name: toolName, arguments: argumentsText },
    },
    { type: 'usage', usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 } },
    { type: 'finish', reason: { kind: 'tool-calls' } },
  ]
}

function makeMockAdapter(record) {
  let callSequence = 0
  return {
    providerInfo(provider) {
      return { id: provider, name: 'Agent Sessions S2 deterministic mock' }
    },
    providerRetryPolicy() {
      return undefined
    },
    imageRequestPricing() {
      return undefined
    },
    async listModels(provider) {
      return [{ provider, id: 'mock', name: 'S2 mock', inputModalities: ['text'] }]
    },
    async resolveModel(provider, model) {
      return {
        provider,
        id: model,
        name: 'S2 mock',
        inputModalities: ['text'],
        context: { contextWindow: 100000 },
        defaultMaxTokens: 4096,
      }
    },
    async prepareCall(provider, model, signal) {
      return {
        model: await this.resolveModel(provider, model, signal),
        stream: (options) => this.stream(options),
      }
    },
    async *stream(options) {
      callSequence += 1
      const call = callSequence
      const userText = latestDirective(options.messages)
      record('mock.request', {
        call,
        session_id: options.sessionId,
        user_text: userText,
        roles: options.messages.map((message) => message.role),
      })

      if (userText.includes('BUSY_CANCEL')) {
        await sleep(10000, options.signal)
      } else if (userText.includes('BUSY_BASE')) {
        await sleep(1500, options.signal)
      }

      let chunks
      if (userText.includes('CALL_PARENT_TOOL')
        && !hasToolResult(options.messages, 'as-parent-')) {
        chunks = toolChunks(`as-parent-${call}`, 'agent_sessions_probe', {})
      } else if (userText.includes('SANDBOX_SOCKET_PROBE')
        && !hasToolResult(options.messages, 'as-sandbox-')) {
        chunks = toolChunks(`as-sandbox-${call}`, 'bash', {
          command: `bash ${JSON.stringify(process.env.DSH_S2_SANDBOX_SCRIPT)}`,
          description: 'Prove DSH sandbox session identity and HOME socket visibility',
        })
      } else {
        chunks = replyChunks(`S2_REPLY_${call}:${userText.slice(0, 120)}`)
      }

      for (const chunk of chunks) {
        options.signal?.throwIfAborted()
        record('mock.chunk', { call, type: chunk.type, reason: chunk.reason?.kind })
        yield chunk
      }
    },
  }
}

function sessionView(agent) {
  return {
    session_id: agent.id,
    status: agent.status,
    cwd: agent.session?.header?.cwd,
    seq: agent.session?.seq,
  }
}

export function apply(ctx) {
  const logPath = process.env.DSH_S2_LOG
  const socketPath = process.env.DSH_S2_CONTROL_SOCKET
  if (!logPath || !socketPath) {
    throw new Error('agent-sessions-s2 requires DSH_S2_LOG and DSH_S2_CONTROL_SOCKET')
  }

  fs.mkdirSync(path.dirname(logPath), { recursive: true, mode: 0o700 })
  fs.mkdirSync(path.dirname(socketPath), { recursive: true, mode: 0o700 })
  try { fs.unlinkSync(socketPath) } catch (error) {
    if (error.code !== 'ENOENT') throw error
  }
  let recordSequence = 0
  const record = (event, fields = {}) => {
    recordSequence += 1
    fs.appendFileSync(logPath, `${JSON.stringify({
      sequence: recordSequence,
      event,
      at: new Date().toISOString(),
      ...fields,
    })}\n`, { mode: 0o600 })
  }

  ctx.llm.registerAdapter(['as-s2-mock'], makeMockAdapter(record))
  ctx.tools.register(defineTool({
    name: 'agent_sessions_probe',
    description: 'Return the exact DSH session identity attached to this tool call.',
    parameters: {},
    output: {
      schema: {
        type: 'object',
        additionalProperties: false,
        properties: {
          session_id: { type: 'string', required: true },
          marker: { type: 'string', required: true },
        },
      },
      render: (_arguments, value) => [{
        type: 'text',
        text: `agent-sessions parent identity ${value.session_id}`,
      }],
    },
    async execute(_arguments, execution) {
      if (!execution.agent) throw new Error('agent_sessions_probe requires exec.agent')
      const result = { session_id: execution.agent.id, marker: 'NATIVE_TOOL_OK' }
      record('parent_tool.execute', result)
      return result
    },
  }))

  ctx.on('agent/created', ({ agent }) => record('agent.created', sessionView(agent)))
  ctx.on('agent/disposed', ({ agent }) => record('agent.disposed', sessionView(agent)))
  ctx.on('agent/status', ({ agent, status }) => record('agent.status', {
    ...sessionView(agent),
    status,
  }))
  ctx.on('agent/turn-stopping', ({ agent, turn }) => record('agent.turn-stopping', {
    ...sessionView(agent),
    turn,
  }))
  ctx.on('session/event', (session, event) => {
    if (event.type === 'turn/start' || event.type === 'turn/end') {
      record('session.event', {
        session_id: session.id,
        session_event: event.type,
        seq: event.seq,
        reason: event.data?.reason,
      })
    }
  })

  const server = net.createServer((connection) => {
    let buffer = ''
    connection.setEncoding('utf8')
    connection.on('data', (chunk) => {
      buffer += chunk
      let newline
      while ((newline = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, newline)
        buffer = buffer.slice(newline + 1)
        if (!line.trim()) continue
        let request
        try {
          request = JSON.parse(line)
          const agent = request.session_id ? ctx.agents.get(request.session_id) : undefined
          let result
          switch (request.op) {
            case 'list':
              result = { sessions: ctx.agents.list().map(sessionView) }
              break
            case 'followup':
              if (!agent) throw new Error('unknown session')
              agent.followup({
                content: [{ type: 'text', text: request.text }],
                source: { kind: 'plugin', plugin: name },
              })
              result = { accepted: true, status: agent.status }
              break
            case 'steer':
              if (!agent) throw new Error('unknown session')
              agent.steer({
                content: [{ type: 'text', text: request.text }],
                source: { kind: 'plugin', plugin: name },
              })
              result = { accepted: true, status: agent.status }
              break
            case 'cancel':
              if (!agent) throw new Error('unknown session')
              agent.cancel({ kind: 'user' })
              result = { accepted: true, status: agent.status }
              break
            case 'sandbox-probe':
              if (!agent) throw new Error('unknown session')
              result = {
                accepted: true,
                session_id: agent.id,
                socket_path: socketPath,
                status: agent.status,
              }
              record('sandbox.socket', result)
              break
            default:
              throw new Error(`unknown operation ${JSON.stringify(request.op)}`)
          }
          record('control.accept', { op: request.op, session_id: request.session_id })
          connection.write(`${JSON.stringify({ ok: true, result })}\n`)
        } catch (error) {
          record('control.reject', { op: request?.op, message: String(error.message) })
          connection.write(`${JSON.stringify({ ok: false, error: String(error.message) })}\n`)
        }
      }
    })
  })
  server.listen(socketPath, () => {
    fs.chmodSync(socketPath, 0o600)
    record('control.listening', { socket_path: socketPath })
  })
  ctx.effect(() => () => new Promise((resolve) => {
    server.close(() => {
      try { fs.unlinkSync(socketPath) } catch {}
      resolve()
    })
  }), 'agent-sessions-s2 control socket')
}
