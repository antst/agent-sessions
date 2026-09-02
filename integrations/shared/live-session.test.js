"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const { InactiveError, LiveSessionClient, readConfiguration } = require("./live-session.js");

test("one socket reports, calls, updates, and receives messages", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  assert.deepEqual(await client.start(), { active: true });
  assert.equal(client.report("native", "before"), true);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  assert.deepEqual(fixture.reports[0], { uuid: "native", name: "before", groups: ["team"], product: "pi" });

  const call = client.callTool("native", "tool-one", "peers.list", {});
  await until(() => fixture.requests.some((frame) => frame.method === "tool.call"));
  fixture.write({ id: fixture.requests[0].id, result: { peers: 2 } });
  assert.deepEqual(await call, { peers: 2 });

  client.updateName("native", "after");
  await until(() => fixture.requests.some((frame) => frame.method === "session.update"));
  const update = fixture.requests.find((frame) => frame.method === "session.update");
  assert.equal(update.params.name, "after");
  fixture.write({ id: update.id, result: {} });

  const delivered = new Promise((resolve) => client.once("message", resolve));
  fixture.write({ id: "daemon.message", method: "message.deliver", params: { message_id: "message", body: "hello" } });
  assert.deepEqual(await delivered, { messageID: "message", nativeSessionID: "native", body: "hello" });
  client.acceptMessage("message");
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.message"));
});

test("disconnect rejects calls and reconnect reports from scratch", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  client.report("native", "worker");
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  const pending = client.callTool("native", "tool", "peers.list", {});
  await until(() => fixture.requests.length === 1);
  fixture.socket.destroy();
  await assert.rejects(pending, /disconnected/u);
  await until(() => fixture.reports.length === 2 && client.sessions.get("native")?.ready);
});

test("inactive client never connects", async () => {
  let connects = 0;
  const client = new LiveSessionClient({ env: {}, connect: () => { connects += 1; } });
  assert.equal(readConfiguration({}).reason, "missing-presence-socket");
  assert.deepEqual(await client.start(), { active: false, reason: "missing-presence-socket" });
  await assert.rejects(client.callTool("native", "call", "peers.list", {}), InactiveError);
  assert.equal(connects, 0);
});

function env(socketPath) {
  return { AGENT_SESSIONS_PRESENCE_SOCKET: socketPath, AGENT_SESSIONS_PRODUCT_ID: "pi", AGENT_SESSIONS_GROUPS: '["team","team"]' };
}

async function server(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-live-"));
  const socketPath = path.join(root, "presence.sock");
  const reports = [], requests = [], responses = [], sockets = [];
  let socket;
  const listener = net.createServer((connection) => {
    socket = connection;
    sockets.push(connection);
    connection.setEncoding("utf8");
    connection.on("error", () => {});
    let buffer = "", reported = false;
    connection.on("data", (chunk) => {
      buffer += chunk;
      for (;;) {
        const newline = buffer.indexOf("\n");
        if (newline < 0) return;
        const frame = JSON.parse(buffer.slice(0, newline));
        buffer = buffer.slice(newline + 1);
        if (!reported) { reported = true; reports.push(frame); }
        else if (frame.method) requests.push(frame); else responses.push(frame);
      }
    });
  });
  await new Promise((resolve, reject) => { listener.once("error", reject); listener.listen(socketPath, resolve); });
  t.after(async () => {
    for (const value of sockets) value.destroy();
    await new Promise((resolve) => listener.close(resolve));
    fs.rmSync(root, { recursive: true, force: true });
  });
  return { path: socketPath, reports, requests, responses, get socket() { return socket; }, write(frame) { socket.write(`${JSON.stringify(frame)}\n`); } };
}

async function until(predicate) {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail("condition did not become true");
}
