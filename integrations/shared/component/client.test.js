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

test("managed client reconnects in the same generation and preserves delivery/tool correlation", async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-component-"));
  const socketPath = path.join(root, "component.sock");
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const received = [];
  let connections = 0;
  let secondSend;
  const server = net.createServer((socket) => {
    socket.on("error", () => {});
    connections += 1;
    const ordinal = connections;
    let outboundSeq = 0;
    const send = (type, id, payload) => {
      outboundSeq += 1;
      socket.write(encodeFrame({ version: 1, type, id, seq: outboundSeq, payload }));
    };
    if (ordinal === 2) secondSend = send;
    const decoder = new FrameDecoder({ maxFrameBytes: 4096, maxNesting: 16, maxStringBytes: 1024 });
    socket.on("data", (chunk) => {
      for (const frame of decoder.push(chunk)) {
        received.push({ ordinal, frame });
        if (frame.type === "bootstrap") {
          assert.equal(frame.payload.bootstrap_value, "ephemeral-secret");
          send("ready", frame.id, {
            binding_id: "binding-one", attachment_id: "attachment-life", daemon_generation: 1,
            protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 100,
          });
        } else if (frame.type === "reconnect") {
          assert.equal(Object.hasOwn(frame.payload, "bootstrap_value"), false);
          assert.equal(frame.payload.prior_generation, 1, "transient reconnect retains the current daemon generation");
          send("ready", frame.id, {
            binding_id: "binding-two", attachment_id: "attachment-life", daemon_generation: 1,
            protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 100,
          });
        } else if (frame.type === "heartbeat") {
          send("heartbeat.ack", frame.id, {
            binding_id: ordinal === 1 ? "binding-one" : "binding-two", last_received_seq: frame.seq,
          });
        } else if (frame.type === "tool.call") {
          if (frame.payload.operation === "sessions.reject") {
            send("reject", frame.id, { operation_id: frame.payload.call_id, category: "native-rejected", detail: "refused" });
          } else {
            send("tool.result", frame.id, {
              call_id: frame.payload.call_id, success: true, result: { peers: 2 },
            });
          }
        }
      }
    });
  });
  await listen(server, socketPath);

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
  const client = new ComponentClient({ env, reconnectMinMs: 5, reconnectMaxMs: 20, maxQueue: 3, maxJournal: 3, maxOutstanding: 1 });
  t.after(async () => {
    await client.stop();
    await closeServer(server);
  });
  const state = await client.start();
  assert.equal(state.active, true);
  assert.equal(client.bindingID, "binding-one");
  assert.equal(env.AGENT_SESSIONS_BOOTSTRAP_VALUE, undefined, "raw bootstrap is removed from the ephemeral env after use");

  const boundaryOperations = [
    { type: "session.announce", id: "announce-boundary", payload: { binding_id: "binding-one", native_session_id: "native-session-distinct", cwd: "/work", native_name: "native", product_event_seq: 1 } },
    { type: "delivery.accept", id: "delivery-boundary", payload: { delivery_id: "delivery-boundary", native_session_id: "native-session-distinct", native_message_id: "native-message-boundary", accepted_at: Date.now() } },
    { type: "turn.event", id: "turn-boundary", payload: { native_session_id: "native-session-distinct", event_seq: 1, kind: "settled", metadata: {} } },
  ];
  for (const operation of boundaryOperations) client.send(operation.type, operation.id, operation.payload);
  assert.throws(() => client.send("session.state", "journal-overflow", { native_session_id: "native-session-distinct", state: "idle", product_event_seq: 2 }), /journal/i);
  await until(() => boundaryOperations.every((operation) => received.some(({ ordinal, frame }) => ordinal === 1 && frame.id === operation.id)));

  client.socket.destroy();
  await until(() => client.bindingID === "binding-two");
  assert.equal(received.some(({ frame }) => frame.type === "reconnect"), true);
  await until(() => boundaryOperations.every((operation) => received.some(({ ordinal, frame }) => ordinal === 2 && frame.id === operation.id)));
  const replayedAnnounce = received.find(({ ordinal, frame }) => ordinal === 2 && frame.id === "announce-boundary").frame;
  assert.equal(replayedAnnounce.payload.binding_id, "binding-two");
  await until(() => received.some(({ ordinal, frame }) => ordinal === 2 && frame.type === "heartbeat"));
  await until(() => client.outboundJournal.size === 0);

  let deliveryPresents = 0;
  client.on("delivery.present", (payload) => {
    deliveryPresents += 1;
    client.send("delivery.accept", payload.delivery_id, {
      delivery_id: payload.delivery_id,
      native_session_id: "native-session-distinct",
      native_message_id: "native-message",
      accepted_at: Date.now(),
    });
  });
  secondSend("delivery.present", "delivery-1", {
    delivery_id: "delivery-1", mode: "wake", body: { text: "hello" },
  });
  await until(() => received.some(({ ordinal, frame }) => ordinal === 2 && frame.type === "delivery.accept" && frame.id === "delivery-1"));
  assert.equal(deliveryPresents, 1);

  const tool = await client.callTool("call-1", "sessions.list", {});
  assert.deepEqual(tool, { peers: 2 });
  await assert.rejects(client.callTool("call-rejected", "sessions.reject", {}), (error) => error.category === "native-rejected");
  assert.equal(client.pendingTools.has("call-rejected"), false);
  const retriedTool = await client.callTool("call-after-reject", "sessions.list", {});
  assert.deepEqual(retriedTool, { peers: 2 });
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
  const received = [];
  const server = net.createServer((socket) => {
    socket.on("error", () => {});
    connections += 1;
    const ordinal = connections;
    const decoder = new FrameDecoder();
    socket.on("data", (chunk) => {
      for (const frame of decoder.push(chunk)) {
        received.push({ ordinal, frame });
        if (frame.type !== "bootstrap" && frame.type !== "reconnect") continue;
        socket.write(encodeFrame({ version: 1, type: "ready", id: frame.id, seq: 1, payload: {
          binding_id: `binding-${ordinal}`, attachment_id: "attachment", daemon_generation: ordinal,
          protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 10,
        } }));
      }
    });
  });
  await listen(server, socketPath);
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
  t.after(async () => {
    await client.stop();
    await closeServer(server);
  });
  await client.start();
  client.send("session.state", "generation-scoped-state", { native_session_id: "native", state: "idle", product_event_seq: 1 });
  await until(() => received.some(({ ordinal, frame }) => ordinal === 1 && frame.id === "generation-scoped-state"));
  await until(() => client.daemonGeneration >= 2);
  assert.equal(client.outboundJournal.size, 0, "cross-generation reconnect drops the ephemeral journal");
  assert.equal(received.some(({ ordinal, frame }) => ordinal >= 2 && frame.id === "generation-scoped-state"), false);
});

function listen(server, socketPath) {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
}

function closeServer(server) {
  return new Promise((resolve, reject) => {
    if (!server.listening) {
      resolve();
      return;
    }
    server.close((error) => error ? reject(error) : resolve());
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
