"use strict";

const assert = require("node:assert/strict");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const fs = require("node:fs");
const test = require("node:test");

const { ComponentClient, InactiveError } = require("./client.js");
const { FrameDecoder, encodeFrame, redact } = require("./protocol.js");

test("ambient component is inert without the complete managed bootstrap", async () => {
  let connects = 0;
  const client = new ComponentClient({
    env: {
      AGENT_SESSIONS_COMPONENT_SOCKET: "/tmp/must-not-connect.sock",
      AGENT_SESSIONS_PRODUCT_ID: "pi",
      AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
      AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "public-id-without-value",
    },
    connect: () => {
      connects += 1;
      throw new Error("ambient component attempted a connection");
    },
  });

  const state = await client.start();
  assert.deepEqual(state, { active: false, reason: "missing-bootstrap-value" });
  assert.equal(client.active, false);
  assert.equal(client.send("session.state", "state", {}), false);
  await assert.rejects(client.callTool("call", "sessions.list", {}), InactiveError);
  await new Promise((resolve) => setTimeout(resolve, 30));
  assert.equal(connects, 0, "inert client must not connect or schedule retry");
  await client.stop();
});

test("managed client bootstraps, heartbeats, reconnects without a reusable secret, and correlates tools", async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-component-"));
  const socketPath = path.join(root, "component.sock");
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const received = [];
  let connections = 0;
  let firstSocket;
  const server = net.createServer((socket) => {
    connections += 1;
    const ordinal = connections;
    let outboundSeq = 0;
    const send = (type, id, payload) => {
      outboundSeq += 1;
      socket.write(encodeFrame({ version: 1, type, id, seq: outboundSeq, payload }));
    };
    if (ordinal === 1) firstSocket = socket;
    const decoder = new FrameDecoder({ maxFrameBytes: 4096, maxNesting: 16, maxStringBytes: 1024 });
    socket.on("data", (chunk) => {
      for (const frame of decoder.push(chunk)) {
        received.push({ ordinal, frame });
        if (frame.type === "bootstrap") {
          assert.equal(frame.payload.bootstrap_value, "ephemeral-secret");
          send("ready", frame.id, {
            binding_id: "binding-one", attachment_id: "attachment-life", daemon_generation: 1,
            protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 20,
          });
        } else if (frame.type === "reconnect") {
          assert.equal(Object.hasOwn(frame.payload, "bootstrap_value"), false);
          send("ready", frame.id, {
            binding_id: "binding-two", attachment_id: "attachment-life", daemon_generation: 2,
            protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 20,
          });
        } else if (frame.type === "heartbeat") {
          send("heartbeat.ack", frame.id, {
            binding_id: ordinal === 1 ? "binding-one" : "binding-two", last_received_seq: frame.seq,
          });
        } else if (frame.type === "tool.call") {
          send("tool.result", frame.id, {
            call_id: frame.payload.call_id, success: true, result: { peers: 2 },
          });
        }
      }
    });
  });
  await listen(server, socketPath);
  t.after(() => server.close());

  const env = {
    AGENT_SESSIONS_COMPONENT_SOCKET: socketPath,
    AGENT_SESSIONS_PRODUCT_ID: "pi",
    AGENT_SESSIONS_ATTACHMENT_ID: "attachment-life",
    AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability-id",
    AGENT_SESSIONS_BOOTSTRAP_VALUE: "ephemeral-secret",
    AGENT_SESSIONS_PROCESS_START: "process-start",
    AGENT_SESSIONS_STRONG_START: "strong-start",
    AGENT_SESSIONS_COMPONENT_VERSION: "1.0.0",
  };
  const client = new ComponentClient({ env, reconnectMinMs: 5, reconnectMaxMs: 20, maxQueue: 8, maxOutstanding: 4 });
  t.after(() => client.stop());
  const state = await client.start();
  assert.equal(state.active, true);
  assert.equal(client.bindingID, "binding-one");
  assert.equal(env.AGENT_SESSIONS_BOOTSTRAP_VALUE, undefined, "raw bootstrap is removed from the ephemeral env after use");

  firstSocket.destroy();
  await until(() => client.bindingID === "binding-two");
  assert.equal(received.some(({ frame }) => frame.type === "reconnect"), true);

  const tool = await client.callTool("call-1", "sessions.list", {});
  assert.deepEqual(tool, { peers: 2 });
  await until(() => received.some(({ ordinal, frame }) => ordinal === 2 && frame.type === "heartbeat"));
});

test("protocol framing is bounded and diagnostics redact bootstrap material", () => {
  const decoder = new FrameDecoder({ maxFrameBytes: 32, maxNesting: 4, maxStringBytes: 16 });
  const header = Buffer.alloc(4);
  header.writeUInt32BE(33);
  assert.throws(() => decoder.push(header), /frame size/i);
  assert.throws(() => encodeFrame({ version: 1, type: "x", id: "x", seq: 1, payload: { value: "x".repeat(40) } }, { maxFrameBytes: 32 }), /frame/i);
  const secret = "ephemeral-secret";
  const detail = redact(`bootstrap_value=${secret} password=hunter2 safe=yes`, secret);
  assert.equal(detail.includes(secret), false);
  assert.equal(detail.includes("hunter2"), false);
  assert.equal(detail.includes("safe=yes"), true);

  const invalidUTF8 = Buffer.from([0x7b, 0x22, 0x78, 0x22, 0x3a, 0x22, 0xff, 0x22, 0x7d]);
  const invalidWire = Buffer.alloc(4 + invalidUTF8.length);
  invalidWire.writeUInt32BE(invalidUTF8.length);
  invalidUTF8.copy(invalidWire, 4);
  assert.throws(() => new FrameDecoder().push(invalidWire), /invalid JSON/i);
});

test("missing heartbeat acknowledgments force bounded reconnect", async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-heartbeat-"));
  const socketPath = path.join(root, "component.sock");
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  let connections = 0;
  const server = net.createServer((socket) => {
    connections += 1;
    const decoder = new FrameDecoder();
    socket.on("data", (chunk) => {
      for (const frame of decoder.push(chunk)) {
        if (frame.type !== "bootstrap" && frame.type !== "reconnect") continue;
        socket.write(encodeFrame({ version: 1, type: "ready", id: frame.id, seq: 1, payload: {
          binding_id: `binding-${connections}`, attachment_id: "attachment", daemon_generation: connections,
          protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 10,
        } }));
      }
    });
  });
  await listen(server, socketPath);
  t.after(() => server.close());
  const client = new ComponentClient({
    env: {
      AGENT_SESSIONS_COMPONENT_SOCKET: socketPath,
      AGENT_SESSIONS_PRODUCT_ID: "pi",
      AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
      AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability",
      AGENT_SESSIONS_BOOTSTRAP_VALUE: "value",
      AGENT_SESSIONS_PROCESS_START: "start",
      AGENT_SESSIONS_STRONG_START: "strong",
      AGENT_SESSIONS_COMPONENT_VERSION: "1",
    },
    heartbeatGrace: 1,
    reconnectMinMs: 5,
    reconnectMaxMs: 10,
  });
  t.after(() => client.stop());
  await client.start();
  await until(() => connections >= 2);
});

function listen(server, socketPath) {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
}

async function until(predicate) {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail("condition did not become true");
}
