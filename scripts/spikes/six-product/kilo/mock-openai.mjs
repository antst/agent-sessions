#!/usr/bin/env node

import { appendFileSync } from "node:fs"
import { createServer } from "node:http"

const host = "127.0.0.1"
const port = Number.parseInt(process.env.KILO_SPIKE_MOCK_PORT ?? "", 10)
const logPath = process.env.KILO_SPIKE_MOCK_LOG
const schemaLogPath = process.env.KILO_SPIKE_SCHEMA_LOG

if (!Number.isSafeInteger(port) || port < 0 || port > 65535) {
  throw new Error("KILO_SPIKE_MOCK_PORT must be a valid TCP port (zero requests an ephemeral port)")
}

function json(res, status, body) {
  const data = JSON.stringify(body)
  res.writeHead(status, {
    "content-type": "application/json",
    "content-length": Buffer.byteLength(data),
  })
  res.end(data)
}

function latestUserText(body) {
  const messages = Array.isArray(body?.messages) ? body.messages : []
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    if (messages[index]?.role !== "user") continue
    const content = messages[index].content
    if (typeof content === "string") return content
    if (!Array.isArray(content)) return ""
    return content
      .filter((part) => part?.type === "text" && typeof part.text === "string")
      .map((part) => part.text)
      .join("\n")
  }
  return ""
}

function messagesAfterLatestUser(body) {
  const messages = Array.isArray(body?.messages) ? body.messages : []
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    if (messages[index]?.role === "user") return messages.slice(index + 1)
  }
  return []
}

function projectDirectory(user) {
  const match = user.match(/Working directory:\s*([^\n]+)/)
  return match?.[1]?.trim() || process.cwd()
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'\\''`)}'`
}

function requestedToolCall(body, user) {
  if (messagesAfterLatestUser(body).some((message) => message?.role === "tool")) return undefined
  if (user.includes("BACKGROUND_ATTRIBUTION")) {
    return {
      id: "call_kilo_s1_background",
      name: "background_process",
      arguments: JSON.stringify({
        action: "start",
        command: `${shellQuote(process.execPath)} -e "console.log('BG_READY'); setInterval(() => {}, 1000)"`,
        workdir: projectDirectory(user),
        description: "Kilo S1 background attribution",
        ready: { pattern: "BG_READY", timeout: 5000 },
      }),
    }
  }
  return undefined
}

function logRequest(body) {
  if (!logPath) return
  const toolNames = Array.isArray(body?.tools)
    ? body.tools.map((tool) => tool?.function?.name).filter(Boolean)
    : []
  appendFileSync(
    logPath,
    `${JSON.stringify({ model: body?.model, user: latestUserText(body), toolNames })}\n`,
    { encoding: "utf8", mode: 0o600 },
  )
  if (schemaLogPath) {
    const selected = Array.isArray(body?.tools)
      ? body.tools.filter((tool) => ["background_process", "bash"].includes(tool?.function?.name))
      : []
    appendFileSync(schemaLogPath, `${JSON.stringify(selected)}\n`, {
      encoding: "utf8",
      mode: 0o600,
    })
  }
}

function writeChunk(res, id, delta, finishReason = null, usage) {
  const body = {
    id,
    object: "chat.completion.chunk",
    created: Math.floor(Date.now() / 1000),
    model: "agent-sessions-kilo-spike",
    choices: [{ index: 0, delta, finish_reason: finishReason }],
  }
  if (usage) body.usage = usage
  res.write(`data: ${JSON.stringify(body)}\n\n`)
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", `http://${host}:${port}`)
  if (req.method === "GET" && url.pathname === "/v1/models") {
    return json(res, 200, {
      object: "list",
      data: [{ id: "spike", object: "model", owned_by: "agent-sessions" }],
    })
  }
  if (req.method !== "POST" || url.pathname !== "/v1/chat/completions") {
    return json(res, 404, { error: { message: "not found", type: "not_found" } })
  }

  const chunks = []
  for await (const chunk of req) chunks.push(chunk)
  let body
  try {
    body = JSON.parse(Buffer.concat(chunks).toString("utf8"))
  } catch {
    return json(res, 400, { error: { message: "invalid json", type: "invalid_request_error" } })
  }
  logRequest(body)

  const user = latestUserText(body)
  const delay = user.includes("SLOW_TURN") ? 5000 : 50
  const toolCall = requestedToolCall(body, user)
  const answer = `MOCK_OK:${user.replaceAll(/\s+/g, " ").slice(0, 120)}`

  if (body.stream !== true) {
    await new Promise((resolve) => setTimeout(resolve, delay))
    if (toolCall) {
      return json(res, 200, {
        id: `chatcmpl-${Date.now()}`,
        object: "chat.completion",
        created: Math.floor(Date.now() / 1000),
        model: "agent-sessions-kilo-spike",
        choices: [{
          index: 0,
          message: {
            role: "assistant",
            content: null,
            tool_calls: [{
              id: toolCall.id,
              type: "function",
              function: { name: toolCall.name, arguments: toolCall.arguments },
            }],
          },
          finish_reason: "tool_calls",
        }],
        usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
      })
    }
    return json(res, 200, {
      id: `chatcmpl-${Date.now()}`,
      object: "chat.completion",
      created: Math.floor(Date.now() / 1000),
      model: "agent-sessions-kilo-spike",
      choices: [{ index: 0, message: { role: "assistant", content: answer }, finish_reason: "stop" }],
      usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
    })
  }

  res.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-cache",
    connection: "keep-alive",
  })
  const id = `chatcmpl-${Date.now()}`
  writeChunk(res, id, { role: "assistant", content: "" })
  await new Promise((resolve) => setTimeout(resolve, delay))
  if (toolCall) {
    writeChunk(res, id, {
      tool_calls: [{
        index: 0,
        id: toolCall.id,
        type: "function",
        function: { name: toolCall.name, arguments: toolCall.arguments },
      }],
    })
    writeChunk(res, id, {}, "tool_calls", { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 })
  } else {
    writeChunk(res, id, { content: answer })
    writeChunk(res, id, {}, "stop", { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 })
  }
  res.write("data: [DONE]\n\n")
  res.end()
})

server.listen(port, host, () => {
  const address = server.address()
  const actualPort = typeof address === "object" && address ? address.port : port
  process.stdout.write(`mock-openai listening on http://${host}:${actualPort}\n`)
})

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => server.close(() => process.exit(0)))
}
