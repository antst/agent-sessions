#!/bin/sh
set -eu

node -e '
  const net = require("node:net")
  const socketPath = process.env.AGENT_SESSIONS_COMPONENT_SOCKET
  const sessionId = process.env.DSH_SESSION_ID
  if (!socketPath) throw new Error("AGENT_SESSIONS_COMPONENT_SOCKET is unset")
  if (!sessionId) throw new Error("DSH_SESSION_ID is unset")
  const client = net.connect(socketPath)
  let buffer = ""
  client.setEncoding("utf8")
  client.on("connect", () => client.write(JSON.stringify({
    op: "sandbox-probe",
    session_id: sessionId,
  }) + "\n"))
  client.on("data", (chunk) => {
    buffer += chunk
    if (!buffer.includes("\n")) return
    const response = JSON.parse(buffer.slice(0, buffer.indexOf("\n")))
    if (!response.ok) throw new Error(response.error)
    if (response.result.session_id !== sessionId) throw new Error("session mismatch")
    process.stdout.write(`DSH_SESSION_ID=${sessionId}\n`)
    process.stdout.write(`HOME_SOCKET=${socketPath}\n`)
    process.stdout.write("SANDBOX_SOCKET_OK\n")
    client.end()
  })
  client.on("error", (error) => { throw error })
  setTimeout(() => { throw new Error("socket response timeout") }, 5000).unref()
'
