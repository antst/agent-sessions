#!/usr/bin/env node

import { createServer } from "node:net"

const server = createServer()
server.unref()
server.on("error", (error) => {
  process.stderr.write(`${error.message}\n`)
  process.exit(1)
})
server.listen(0, "127.0.0.1", () => {
  const address = server.address()
  if (typeof address !== "object" || !address) process.exit(1)
  process.stdout.write(`${address.port}\n`)
  server.close()
})
