"use strict";

const assert = require("node:assert/strict");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const fs = require("node:fs");
const test = require("node:test");

const { ComponentClient, InactiveError } = require("./client.js");
const {
  CONTRACT_REVISION,
  FrameDecoder,
  componentRenameObservationID,
  daemonRenameOperationID,
  encodeFrame,
  makeFrame,
  redact,
  validateContractRevision,
  validNativeTitleObservation,
} = require("./protocol.js");

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

test("optional process corroboration environment is all-or-none", async () => {
  for (const envField of ["AGENT_SESSIONS_PROCESS_START", "AGENT_SESSIONS_STRONG_START"]) {
    let connects = 0;
    const client = new ComponentClient({
      env: {
        AGENT_SESSIONS_COMPONENT_SOCKET: "/tmp/partial-corroboration.sock",
        AGENT_SESSIONS_PRODUCT_ID: "pi",
        AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
        AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability",
        AGENT_SESSIONS_BOOTSTRAP_VALUE: "value",
        AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
        [envField]: "partial",
      },
      connect: () => { connects += 1; throw new Error("partial corroboration connected"); },
    });
    assert.deepEqual(await client.start(), { active: false, reason: "partial-process-corroboration" });
    assert.equal(connects, 0);
    await client.stop();
  }
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
          assert.equal(Object.hasOwn(frame.payload, "process_start"), false);
          assert.equal(Object.hasOwn(frame.payload, "strong_start"), false);
          send("ready", frame.id, {
            binding_id: "binding-one", attachment_id: "attachment-life", daemon_generation: 1,
            protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 100,
          });
        } else if (frame.type === "reconnect") {
          assert.equal(Object.hasOwn(frame.payload, "bootstrap_value"), false);
          assert.equal(Object.hasOwn(frame.payload, "process_start"), false);
          assert.equal(Object.hasOwn(frame.payload, "strong_start"), false);
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
    AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
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
      AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
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

test("rename contract revision and operation namespaces are exact", async () => {
  assert.equal(CONTRACT_REVISION, "agent-sessions.component.v1-r1");
  assert.equal(require("./protocol.js").FRAME_TYPES.size, 21);
  assert.equal(validateContractRevision(CONTRACT_REVISION), true);
  assert.throws(() => validateContractRevision("agent-sessions.component.v1-r0"), /unsupported/i);
  assert.equal(daemonRenameOperationID("stable-op"), "daemon.rename.stable-op");
  assert.equal(componentRenameObservationID("native-event"), "component.rename.native-event");
  assert.throws(() => daemonRenameOperationID("component.rename.collision"), /namespaced/i);
  assert.throws(() => componentRenameObservationID("daemon.rename.collision"), /namespaced/i);
  assert.throws(() => makeFrame("session.rename.request", "component.rename.wrong", 1, {
    native_session_id: "native", requested_name: "new name",
  }), /namespace/i);
  assert.throws(() => makeFrame("session.rename", "ambiguous", 1, {
    native_session_id: "native", native_name: "new name", product_event_seq: 1,
  }), /namespace/i);
  let staleConnects = 0;
  const stale = new ComponentClient({
    env: {
      AGENT_SESSIONS_COMPONENT_SOCKET: "/tmp/stale-component.sock",
      AGENT_SESSIONS_PRODUCT_ID: "pi",
      AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
      AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability",
      AGENT_SESSIONS_BOOTSTRAP_VALUE: "value",
      AGENT_SESSIONS_PROCESS_START: "start",
      AGENT_SESSIONS_STRONG_START: "strong",
      AGENT_SESSIONS_COMPONENT_VERSION: "agent-sessions.component.v1-r0",
    },
    connect: () => { staleConnects += 1; throw new Error("stale component connected"); },
  });
  assert.deepEqual(await stale.start(), { active: false, reason: "component-contract-mismatch" });
  assert.equal(staleConnects, 0, "mismatched component contract must remain inert before authentication");
});

test("title observations preserve empty and whitespace while requests stay nonempty and safe", () => {
  for (const title of ["", " ", "  product whitespace  ", "é".repeat(512)]) {
    assert.equal(validNativeTitleObservation(title), true);
    assert.doesNotThrow(() => makeFrame("session.announce", "announce-title", 1, {
      binding_id: "binding", native_session_id: "native", cwd: "/work",
      native_name: title, product_event_seq: 1,
    }));
    assert.doesNotThrow(() => makeFrame("session.rename", "component.rename.title", 1, {
      native_session_id: "native", native_name: title, product_event_seq: 2,
    }));
  }
  for (const title of ["x".repeat(1025), "bad\0title", "bad\ttitle", "bad\u0085title", "bad\u007ftitle", "bad\ud800title"]) {
    assert.equal(validNativeTitleObservation(title), false);
    assert.throws(() => makeFrame("session.announce", "announce-title", 1, {
      binding_id: "binding", native_session_id: "native", cwd: "/work",
      native_name: title, product_event_seq: 1,
    }), /title|invalid/i);
    assert.throws(() => makeFrame("session.rename", "component.rename.title", 1, {
      native_session_id: "native", native_name: title, product_event_seq: 2,
    }), /rename|invalid/i);
  }
  for (const title of ["", " ", " leading", "trailing ", "bad\u0085title"]) {
    assert.throws(() => makeFrame("session.rename.request", "daemon.rename.title", 1, {
      native_session_id: "native", requested_name: title,
    }), /rename|invalid/i);
    assert.throws(() => makeFrame("session.rename", "daemon.rename.title", 1, {
      native_session_id: "native", native_name: title, product_event_seq: 2,
    }), /rename|invalid/i);
  }

  for (const payload of [
    { binding_id: "binding", native_session_id: "native", cwd: "/work", product_event_seq: 1 },
    { binding_id: "binding", native_session_id: "native", cwd: "/work", native_name: null, product_event_seq: 1 },
  ]) {
    assert.throws(() => makeFrame("session.announce", "announce-title", 1, payload), /title|invalid/i);
  }
  for (const payload of [
    { native_session_id: "native", product_event_seq: 2 },
    { native_session_id: "native", native_name: null, product_event_seq: 2 },
  ]) {
    assert.throws(() => makeFrame("session.rename", "component.rename.title", 1, payload), /rename|invalid/i);
  }
});

test("native rename callback is correlated, bounded, replay-safe, and distinct from observations", async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-rename-"));
  const socketPath = path.join(root, "component.sock");
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const received = [];
  let sendToClient;
  const server = net.createServer((socket) => {
    socket.on("error", () => {});
    let outboundSeq = 0;
    sendToClient = (type, id, payload) => {
      outboundSeq += 1;
      socket.write(encodeFrame({ version: 1, type, id, seq: outboundSeq, payload }));
    };
    const decoder = new FrameDecoder();
    socket.on("data", (chunk) => {
      for (const frame of decoder.push(chunk)) {
        received.push(frame);
        if (frame.type === "bootstrap") {
          assert.equal(frame.payload.component_version, CONTRACT_REVISION);
          sendToClient("ready", frame.id, {
            binding_id: "binding", attachment_id: "attachment", daemon_generation: 1,
            protocol_version: 1, max_frame_bytes: 4096, heartbeat_interval_ms: 100,
          });
        } else if (frame.type === "heartbeat") {
          sendToClient("heartbeat.ack", frame.id, { binding_id: "binding", last_received_seq: frame.seq });
        }
      }
    });
  });
  await listen(server, socketPath);
  let callbackCount = 0;
  let eventSequence = 10;
  const client = new ComponentClient({
    env: {
      AGENT_SESSIONS_COMPONENT_SOCKET: socketPath,
      AGENT_SESSIONS_PRODUCT_ID: "pi",
      AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
      AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability",
      AGENT_SESSIONS_BOOTSTRAP_VALUE: "value",
      AGENT_SESSIONS_PROCESS_START: "start",
      AGENT_SESSIONS_STRONG_START: "strong",
      AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
    },
    maxRenameReplay: 2,
    renameSession: async ({ operationID, nativeSessionID, requestedName }) => {
      callbackCount += 1;
      assert.equal(operationID.startsWith("daemon.rename."), true);
      assert.equal(nativeSessionID, "native");
      if (requestedName === "unavailable") {
        const error = new Error("password=private backend unavailable");
        error.category = "unavailable";
        throw error;
      }
      return { nativeName: requestedName, productEventSeq: ++eventSequence };
    },
  });
  t.after(async () => {
    await client.stop();
    await closeServer(server);
  });
  await client.start();

  const operationID = daemonRenameOperationID("stable-one");
  const request = { native_session_id: "native", requested_name: "renamed" };
  assert.throws(() => client.send("session.rename", operationID, {
    native_session_id: "native", native_name: "forged", product_event_seq: 1,
  }), /callback/i, "public send must not bypass correlated rename callback");
  assert.throws(() => client.send("reject", operationID, {
    operation_id: operationID, category: "unsupported", detail: "forged",
  }), /callback/i, "public send must not forge a daemon-correlated rename rejection");
  assert.throws(() => client.send("session.rename.request", operationID, request), /daemon-to-component/i);
  sendToClient("session.rename.request", operationID, request);
  await until(() => received.some((frame) => frame.type === "session.rename" && frame.id === operationID));
  const firstResponse = received.find((frame) => frame.type === "session.rename" && frame.id === operationID);
  assert.deepEqual(firstResponse.payload, { native_session_id: "native", native_name: "renamed", product_event_seq: 11 });
  assert.equal(callbackCount, 1);

  sendToClient("session.rename.request", operationID, request);
  await until(() => received.filter((frame) => frame.type === "session.rename" && frame.id === operationID).length === 2);
  const replay = received.filter((frame) => frame.type === "session.rename" && frame.id === operationID)[1];
  assert.deepEqual(replay.payload, firstResponse.payload, "identical request must replay exact native acceptance");
  assert.equal(callbackCount, 1, "identical request must not invoke product rename twice");

  sendToClient("session.rename.request", operationID, { native_session_id: "native", requested_name: "conflict" });
  await until(() => received.some((frame) => frame.type === "reject" && frame.id === operationID));
  assert.equal(received.find((frame) => frame.type === "reject" && frame.id === operationID).payload.category, "replay");
  assert.equal(callbackCount, 1);

  const failureID = daemonRenameOperationID("failure");
  sendToClient("session.rename.request", failureID, { native_session_id: "native", requested_name: "unavailable" });
  await until(() => received.some((frame) => frame.type === "reject" && frame.id === failureID));
  const failure = received.find((frame) => frame.type === "reject" && frame.id === failureID);
  assert.equal(failure.payload.category, "unavailable");
  assert.equal(failure.payload.detail.includes("private"), false, "rename failure diagnostic must be redacted");

  client.renameSession = null;
  const unsupportedID = daemonRenameOperationID("unsupported");
  sendToClient("session.rename.request", unsupportedID, { native_session_id: "native", requested_name: "unsupported" });
  await until(() => received.some((frame) => frame.type === "reject" && frame.id === unsupportedID));
  assert.equal(received.find((frame) => frame.type === "reject" && frame.id === unsupportedID).payload.category, "unsupported");

  assert.equal(client.observeRename("native-event", "native", "external", 50), true);
  await until(() => received.some((frame) => frame.id === "component.rename.native-event"));
  const observation = received.find((frame) => frame.id === "component.rename.native-event");
  assert.equal(observation.type, "session.rename");
  assert.equal(observation.payload.native_name, "external");

  assert.equal(client.observeRename("native-empty", "native", "", 51), true);
  assert.equal(client.observeRename("native-whitespace", "native", "  ", 52), true);
  await until(() => received.some((frame) => frame.id === "component.rename.native-empty") &&
    received.some((frame) => frame.id === "component.rename.native-whitespace"));
  assert.equal(received.find((frame) => frame.id === "component.rename.native-empty").payload.native_name, "");
  assert.equal(received.find((frame) => frame.id === "component.rename.native-whitespace").payload.native_name, "  ");

  assert.ok(client.renameOperations.size <= 2, "completed rename replay cache must remain bounded");
});

test("native rename callbacks have bounded deadline, disconnect, stop, and late-result cleanup", async () => {
  const pending = [];
  const responses = [];
  const client = new ComponentClient({
    env: {
      AGENT_SESSIONS_COMPONENT_SOCKET: "/tmp/not-opened-component.sock",
      AGENT_SESSIONS_PRODUCT_ID: "pi",
      AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
      AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability",
      AGENT_SESSIONS_BOOTSTRAP_VALUE: "value",
      AGENT_SESSIONS_PROCESS_START: "start",
      AGENT_SESSIONS_STRONG_START: "strong",
      AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
    },
    renameTimeoutMs: 10,
    renameSession: ({ signal }) => new Promise((resolve) => pending.push({ resolve, signal })),
  });
  client.ready = true;
  client._writeRenameResponse = (operation) => responses.push(operation);

  const timedOutID = daemonRenameOperationID("never-settles");
  client._handleRenameRequest(makeFrame("session.rename.request", timedOutID, 1, {
    native_session_id: "native", requested_name: "timeout",
  }));
  await until(() => responses.some(({ id }) => id === timedOutID));
  assert.equal(responses.find(({ id }) => id === timedOutID).payload.category, "timed-out");
  assert.equal(pending[0].signal.aborted, true);
  assert.equal(client.pendingRenames, 0);
  pending[0].resolve({ nativeName: "timeout", productEventSeq: 1 });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(responses.filter(({ id }) => id === timedOutID).length, 1, "late callback result must be ignored");

  client.renameTimeoutMs = 1000;
  const disconnectedID = daemonRenameOperationID("disconnect-cleanup");
  client._handleRenameRequest(makeFrame("session.rename.request", disconnectedID, 2, {
    native_session_id: "native", requested_name: "disconnect",
  }));
  await until(() => pending.length === 2);
  const socket = {};
  client.socket = socket;
  client.stopping = true;
  client._onClose(socket);
  assert.equal(pending[1].signal.aborted, true);
  assert.equal(responses.find(({ id }) => id === disconnectedID).payload.category, "unavailable");
  assert.equal(client.pendingRenames, 0);
  pending[1].resolve({ nativeName: "disconnect", productEventSeq: 2 });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(responses.filter(({ id }) => id === disconnectedID).length, 1);

  client.stopping = false;
  const stoppedID = daemonRenameOperationID("stop-cleanup");
  client._handleRenameRequest(makeFrame("session.rename.request", stoppedID, 3, {
    native_session_id: "native", requested_name: "stop",
  }));
  await until(() => pending.length === 3);
  await client.stop();
  assert.equal(pending[2].signal.aborted, true);
  assert.equal(responses.find(({ id }) => id === stoppedID).payload.category, "unavailable");
  assert.equal(client.pendingRenames, 0);
  pending[2].resolve({ nativeName: "stop", productEventSeq: 3 });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(responses.filter(({ id }) => id === stoppedID).length, 1);
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
