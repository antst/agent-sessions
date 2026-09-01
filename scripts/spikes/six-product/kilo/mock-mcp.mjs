#!/usr/bin/env node

import { createInterface } from "node:readline"

const lines = createInterface({ input: process.stdin, crlfDelay: Infinity })

function send(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`)
}

lines.on("line", (line) => {
  let request
  try {
    request = JSON.parse(line)
  } catch {
    return
  }
  if (request.id === undefined) return
  if (request.method === "initialize") {
    send({
      jsonrpc: "2.0",
      id: request.id,
      result: {
        protocolVersion: request.params?.protocolVersion ?? "2025-06-18",
        capabilities: { tools: {} },
        serverInfo: { name: "agent-sessions-kilo-spike", version: "1" },
      },
    })
    return
  }
  if (request.method === "tools/list") {
    send({ jsonrpc: "2.0", id: request.id, result: { tools: [] } })
    return
  }
  if (request.method === "ping") {
    send({ jsonrpc: "2.0", id: request.id, result: {} })
    return
  }
  send({
    jsonrpc: "2.0",
    id: request.id,
    error: { code: -32601, message: `unsupported method ${String(request.method)}` },
  })
})
