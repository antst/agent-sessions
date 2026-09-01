"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const { ComponentClient, InactiveError, readConfiguration } = require("./client.js");
const {
  CONTRACT_REVISION,
  FrameDecoder,
  daemonRenameOperationID,
  encodeFrame,
} = require("./protocol.js");

test("a live component needs only its socket and reported identity", () => {
  assert.equal(readConfiguration({}).reason, "missing-component-socket");
  const gate = readConfiguration(componentEnv("/run/component.sock", "pi", "session-1"));
  assert.equal(gate.active, true);
  assert.equal(gate.reason, "managed-live-socket");
});

test("disconnect rejects live calls and reconnect starts fresh", async (t) => {
  const fixture = await liveServer(t);
  const client = new ComponentClient({
    env: componentEnv(fixture.socketPath, "pi", "session-live"),
    reconnectMs: 5,
  });
  t.after(() => client.stop());

  const activation = await client.start();
  assert.deepEqual(activation, { active: true, bindingID: "session-live", daemonGeneration: 0 });
  client.send("session.state", "state-before-drop", {
    native_session_id: "native", state: "idle", product_event_seq: 1,
  });
  await until(() => fixture.frames.some(({ connection, frame }) => connection === 1 && frame.id === "state-before-drop"));

  const pending = client.callTool("tool-before-drop", "peers.list", {});
  await until(() => fixture.frames.some(({ frame }) => frame.id === "tool-before-drop"));
  fixture.sockets[0].destroy();
  await assert.rejects(pending, (error) => error?.category === "unavailable");
  await until(() => fixture.connections >= 2 && client.ready);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(fixture.frames.some(({ connection, frame }) => connection === 2 && frame.id === "state-before-drop"), false);
  assert.equal(fixture.frames.some(({ connection, frame }) => connection === 2 && frame.id === "tool-before-drop"), false);

  const after = client.callTool("tool-after-drop", "peers.list", {});
  await until(() => fixture.frames.some(({ connection, frame }) => connection === 2 && frame.id === "tool-after-drop"));
  fixture.send(2, "tool.result", "tool-after-drop", {
    call_id: "tool-after-drop", success: true, result: { peers: 2 },
  });
  assert.deepEqual(await after, { peers: 2 });
});

test("native rename is one live correlated call", async (t) => {
  const fixture = await liveServer(t);
  let calls = 0;
  const client = new ComponentClient({
    env: componentEnv(fixture.socketPath, "omp", "session-rename"),
    renameSession: async ({ nativeSessionID, requestedName }) => {
      calls += 1;
      assert.equal(nativeSessionID, "native");
      return { nativeName: requestedName, productEventSeq: calls };
    },
  });
  t.after(() => client.stop());
  await client.start();

  const operationID = daemonRenameOperationID("rename-1");
  fixture.send(1, "session.rename.request", operationID, {
    native_session_id: "native", requested_name: "new title",
  });
  await until(() => fixture.frames.some(({ frame }) => frame.id === operationID && frame.type === "session.rename"));
  assert.equal(calls, 1);
  assert.deepEqual(fixture.frames.find(({ frame }) => frame.id === operationID && frame.type === "session.rename").frame.payload, {
    native_session_id: "native", native_name: "new title", product_event_seq: 1,
  });
});

test("inactive client never connects or queues", async () => {
  let connects = 0;
  const client = new ComponentClient({
    env: {},
    connect: () => { connects += 1; throw new Error("unexpected connect"); },
  });
  assert.deepEqual(await client.start(), { active: false, reason: "missing-component-socket" });
  assert.equal(client.send("session.state", "state", {}), false);
  await assert.rejects(client.callTool("call", "peers.list", {}), InactiveError);
  assert.equal(connects, 0);
});

function componentEnv(socketPath, product, attachment) {
  return {
    AGENT_SESSIONS_COMPONENT_SOCKET: socketPath,
    AGENT_SESSIONS_PRODUCT_ID: product,
    AGENT_SESSIONS_ATTACHMENT_ID: attachment,
    AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
  };
}

async function liveServer(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-live-component-"));
  const socketPath = path.join(root, "component.sock");
  const sockets = [];
  const frames = [];
  const writers = new Map();
  let connections = 0;
  const server = net.createServer((socket) => {
    socket.on("error", () => {});
    const connection = ++connections;
    sockets.push(socket);
    writers.set(connection, (type, id, payload) => {
      socket.write(encodeFrame({ version: 1, type, id, payload }));
    });
    const decoder = new FrameDecoder();
    socket.on("data", (chunk) => {
      for (const frame of decoder.push(chunk)) frames.push({ connection, frame });
    });
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
  t.after(async () => {
    for (const socket of sockets) socket.destroy();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(root, { recursive: true, force: true });
  });
  return {
    socketPath, sockets, frames,
    get connections() { return connections; },
    send(connection, type, id, payload) { writers.get(connection)(type, id, payload); },
  };
}

async function until(predicate) {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail("condition did not become true");
}
