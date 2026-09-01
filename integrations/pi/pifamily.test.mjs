import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import componentModule from "../shared/component/client.js";
import protocolModule from "../shared/component/protocol.js";
import { createPiFamilyExtension } from "./pifamily.mjs";

const { ComponentClient } = componentModule;
const { CONTRACT_REVISION, FrameDecoder, encodeFrame } = protocolModule;

class FakeComponent extends EventEmitter {
  constructor(active = true) {
    super();
    this.active = active;
    this.bindingID = "binding-1";
    this.sent = [];
    this.calls = [];
    this.stopped = false;
    this.starts = 0;
  }
  async start() {
    this.starts += 1;
    return this.active ? { active: true, bindingID: this.bindingID, daemonGeneration: 7 } : { active: false, reason: "inert" };
  }
  send(type, id, payload) { this.sent.push({ type, id, payload }); return true; }
  observeRename(nativeEventID, nativeSessionID, nativeName, productEventSeq) {
    return this.send("session.rename", `component.rename.${nativeEventID}`, {
      native_session_id: nativeSessionID, native_name: nativeName, product_event_seq: productEventSeq,
    });
  }
  async callTool(callID, operation, argumentsValue) {
    this.calls.push({ callID, operation, argumentsValue });
    return { ok: true, operation };
  }
  async stop() { this.stopped = true; }
}

class FakePi {
  constructor(sessionName = "native name") {
    this.handlers = new Map();
    this.tools = new Map();
    this.commands = new Map();
    this.messages = [];
    this.names = [];
    this.sessionName = sessionName;
  }
  on(name, handler) { this.handlers.set(name, handler); }
  registerTool(tool) { this.tools.set(tool.name, tool); }
  registerCommand(name, command) { this.commands.set(name, command); }
  sendUserMessage(content, options) { this.messages.push({ content, options }); }
  getSessionName() { return this.sessionName; }
  setSessionName(name) { this.names.push(name); }
  async fire(name, event, ctx) {
    const handler = this.handlers.get(name);
    if (handler) return handler(event, ctx);
  }
}

function context(nativeSessionID, idle = true) {
  return {
    cwd: "/work/project",
    sessionManager: { getSessionId: () => nativeSessionID },
    isIdle: () => idle,
    signal: undefined,
    ui: { notifications: [], notify(text, level) { this.notifications.push({ text, level }); } },
  };
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

async function until(predicate, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error("timed out waiting for component fixture state");
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

async function listen(server, socketPath) {
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, () => {
      server.off("error", reject);
      resolve();
    });
  });
}

async function closeServer(server) {
  if (!server.listening) return;
  await new Promise((resolve) => server.close(resolve));
}

test("ambient extension remains inert without the managed bootstrap", async () => {
  const component = new FakeComponent(false);
  const pi = new FakePi();
  delete process.env.AGENT_SESSIONS_SESSION_ID;
  delete process.env.AGENT_SESSIONS_NATIVE_SESSION_ID;
  delete process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID;
  createPiFamilyExtension("pi", { componentClient: component })(pi);
  await pi.fire("session_start", { reason: "startup" }, context("native-inert"));
  assert.equal(component.starts, 0);
  assert.equal(component.sent.length, 0);
  assert.equal(component.calls.length, 0);
  assert.equal(pi.tools.size, 0);
  assert.equal(pi.commands.size, 0);
  assert.equal(pi.handlers.size, 0);
  assert.equal(process.env.AGENT_SESSIONS_SESSION_ID, undefined);
  assert.equal(process.env.AGENT_SESSIONS_NATIVE_SESSION_ID, undefined);
  assert.equal(process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID, undefined);
});

test("Pi uses exact identity, native delivery, and agent_settled terminal semantics", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  createPiFamilyExtension("pi", { componentClient: component })(pi);
  const idle = context("pi-session", true);
  await pi.fire("session_start", { reason: "startup" }, idle);
  const announce = component.sent.find((frame) => frame.type === "session.announce");
  assert.equal(announce.payload.native_session_id, "pi-session");
  assert.equal(announce.payload.binding_id, "binding-1");
  assert.equal(process.env.AGENT_SESSIONS_SESSION_ID, "pi-session");

  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "pi-session" });
  component.emit("delivery.present", {
    delivery_id: "delivery-1", mode: "idle-wake",
    body: { version: 1, type: "delivery", message_id: "native-message-1", content: "wake exactly" },
  });
  await tick();
  assert.deepEqual(pi.messages[0], { content: "wake exactly", options: undefined });
  assert.deepEqual(component.sent.find((frame) => frame.type === "delivery.accept").payload, {
    delivery_id: "delivery-1", native_session_id: "pi-session",
    native_message_id: "native-message-1", accepted_at: component.sent.find((frame) => frame.type === "delivery.accept").payload.accepted_at,
  });

  const before = component.sent.length;
  await pi.fire("agent_end", {}, idle);
  assert.equal(component.sent.length, before, "Pi agent_end must not masquerade as fully settled");
  await pi.fire("agent_settled", {}, idle);
  assert.equal(component.sent.some((frame) => frame.type === "turn.event" && frame.payload.kind === "agent_settled"), true);

  await pi.fire("session_info_changed", { name: "renamed" }, idle);
  const rename = component.sent.find((frame) => frame.type === "session.rename");
  assert.match(rename.id, /^component\.rename\./u);
  assert.equal(rename.payload.native_name, "renamed");
});

test("daemon rename has one native writer and resolves only on native observation", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  pi.setSessionName = (name) => {
    pi.names.push(name);
    return Promise.resolve();
  };
  createPiFamilyExtension("pi", { componentClient: component })(pi);
  const ctx = context("pi-rename", true);
  await pi.fire("session_start", { reason: "startup" }, ctx);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "pi-rename" });

  let completed = false;
  const resultPromise = component.renameSession({
    operationID: "daemon.rename.1", nativeSessionID: "pi-rename", requestedName: "new title",
  }).then((value) => { completed = true; return value; });
  assert.deepEqual(pi.names, ["new title"]);
  await tick();
  assert.equal(completed, false, "setSessionName alone is not native confirmation");
  await pi.fire("session_info_changed", { name: "new title" }, ctx);
  const result = await resultPromise;
  assert.equal(result.nativeName, "new title");
  assert.equal(Number.isSafeInteger(result.productEventSeq), true);
  assert.equal(component.sent.some((frame) => frame.type === "session.rename"), false, "shared client owns the correlated response");
});

test("daemon rename observes asynchronous native rejection without an unhandled promise", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  pi.setSessionName = (name) => {
    pi.names.push(name);
    return Promise.reject(new Error("native rename denied"));
  };
  createPiFamilyExtension("omp", { componentClient: component })(pi);
  const ctx = context("omp-rename-reject", true);
  await pi.fire("session_start", { reason: "startup" }, ctx);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "omp-rename-reject" });

  await assert.rejects(component.renameSession({
    operationID: "daemon.rename.reject", nativeSessionID: "omp-rename-reject", requestedName: "denied title",
  }), (error) => error?.category === "native-rejected" && error?.message === "native rename denied");
  assert.deepEqual(pi.names, ["denied title"], "native title writer must be invoked exactly once");
  await tick();
});

test("cleared native title observes genuine empty without confirming a pending rename", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  pi.setSessionName = (name) => {
    pi.names.push(name);
    return Promise.resolve();
  };
  createPiFamilyExtension("pi", { componentClient: component })(pi);
  const ctx = context("pi-rename-clear", true);
  await pi.fire("session_start", { reason: "startup" }, ctx);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "pi-rename-clear" });

  let completed = false;
  const resultPromise = component.renameSession({
    operationID: "daemon.rename.after-clear", nativeSessionID: "pi-rename-clear", requestedName: "daemon title",
  }).then((value) => { completed = true; return value; });
  await tick();
  assert.equal(completed, false);

  await pi.fire("session_info_changed", { name: undefined }, ctx);
  await tick();
  assert.equal(completed, false, "cleared title must not confirm a pending nonempty daemon rename");
  const observation = component.sent.find((frame) => frame.type === "session.rename");
  assert.equal(observation.payload.native_name, "");
  assert.equal(Number.isSafeInteger(observation.payload.product_event_seq), true);

  await pi.fire("session_info_changed", { name: "daemon title" }, ctx);
  assert.equal((await resultPromise).nativeName, "daemon title");
});

test("Pi-family title observations preserve product data and fail closed on unsafe text", async (t) => {
  for (const title of ["", "  "]) {
    await t.test(`valid ${JSON.stringify(title)}`, async () => {
      const component = new FakeComponent();
      const pi = new FakePi(title);
      createPiFamilyExtension("pi", { componentClient: component })(pi);
      const ctx = context(`pi-title-${title.length}`, true);
      await pi.fire("session_start", { reason: "startup" }, ctx);
      const announce = component.sent.find((frame) => frame.type === "session.announce");
      assert.equal(announce.payload.native_name, title);
      component.emit("session.bound", { binding_id: "binding-1", native_session_id: ctx.sessionManager.getSessionId() });
      await pi.fire("session_info_changed", { name: title }, ctx);
      const observations = component.sent.filter((frame) => frame.type === "session.rename");
      assert.equal(observations.at(-1).payload.native_name, title);
    });
  }

  const component = new FakeComponent();
  const pi = new FakePi("initial");
  createPiFamilyExtension("omp", { componentClient: component })(pi);
  const ctx = context("omp-title-hostile", true);
  await pi.fire("session_start", { reason: "startup" }, ctx);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "omp-title-hostile" });
  const before = component.sent.length;
  for (const title of ["bad\0title", "bad\ttitle", "bad\u0085title", "x".repeat(1025)]) {
    await pi.fire("session_info_changed", { name: title }, ctx);
  }
  assert.equal(component.sent.length, before, "unsafe title events must not mutate or emit an observation");

  const invalidStartComponent = new FakeComponent();
  const invalidStartPi = new FakePi("bad\u0085title");
  createPiFamilyExtension("pi", { componentClient: invalidStartComponent })(invalidStartPi);
  await invalidStartPi.fire("session_start", { reason: "startup" }, context("pi-invalid-title", true));
  assert.equal(invalidStartComponent.starts, 0);
  assert.equal(invalidStartComponent.sent.length, 0, "unsafe initial title must not be truncated or fabricated");
});

test("session switch restores exact-session native rename routing", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  pi.setSessionName = (name) => {
    pi.names.push(name);
    return Promise.resolve();
  };
  createPiFamilyExtension("omp", { componentClient: component })(pi);
  const oldContext = context("omp-before-switch", true);
  await pi.fire("session_start", { reason: "startup" }, oldContext);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "omp-before-switch" });
  await pi.fire("session_shutdown", { reason: "switch" }, oldContext);

  const newContext = context("omp-after-switch", true);
  await pi.fire("session_start", { reason: "switch" }, newContext);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "omp-after-switch" });
  assert.equal(component.sent.some((frame) => frame.type === "session.rebind" &&
    frame.payload.old_native_session_id === "omp-before-switch" &&
    frame.payload.new_native_session_id === "omp-after-switch"), true);

  await assert.rejects(async () => component.renameSession({
    operationID: "daemon.rename.old", nativeSessionID: "omp-before-switch", requestedName: "old title",
  }), (error) => error?.category === "native-rejected");
  const renamePromise = component.renameSession({
    operationID: "daemon.rename.new", nativeSessionID: "omp-after-switch", requestedName: "new title",
  });
  await tick();
  await pi.fire("session_info_changed", { name: "new title" }, newContext);
  assert.equal((await renamePromise).nativeName, "new title");
  assert.deepEqual(pi.names, ["new title"], "foreign old session must never reach the native writer");
});

test("real shared component transport preserves exact Pi and OMP peers across kill/resume and daemon-generation reconnect", async (t) => {
  for (const productID of ["pi", "omp"]) {
    await t.test(productID, async () => {
      const root = fs.mkdtempSync(path.join(os.tmpdir(), `agent-sessions-${productID}-peer-`));
      const socketPath = path.join(root, "component.sock");
      const received = [];
      const sockets = [];
      let connections = 0;
      const server = net.createServer((socket) => {
        socket.on("error", () => {});
        sockets.push(socket);
        connections += 1;
        const ordinal = connections;
        let outboundSequence = 0;
        const decoder = new FrameDecoder();
        const send = (type, id, payload) => {
          outboundSequence += 1;
          socket.write(encodeFrame({ version: 1, type, id, seq: outboundSequence, payload }));
        };
        socket.on("data", (chunk) => {
          for (const frame of decoder.push(chunk)) {
            received.push({ ordinal, frame });
            if (frame.type === "bootstrap" || frame.type === "reconnect") {
              send("ready", frame.id, {
                binding_id: `binding-${productID}-${ordinal}`,
                attachment_id: `attachment-${productID}`,
                daemon_generation: 30 + ordinal - 1,
                protocol_version: 1,
                max_frame_bytes: 1024 * 1024,
                heartbeat_interval_ms: 1000,
              });
            } else if (frame.type === "session.announce") {
              send("session.bound", frame.id, {
                binding_id: `binding-${productID}-${ordinal}`,
                native_session_id: frame.payload.native_session_id,
              });
            } else if (frame.type === "heartbeat") {
              send("heartbeat.ack", frame.id, {
                binding_id: `binding-${productID}-${ordinal}`,
                last_received_seq: frame.seq,
              });
            }
          }
        });
      });
      await listen(server, socketPath);

      const clientEnv = {
        AGENT_SESSIONS_COMPONENT_SOCKET: socketPath,
        AGENT_SESSIONS_PRODUCT_ID: productID,
        AGENT_SESSIONS_ATTACHMENT_ID: `attachment-${productID}`,
        AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: `capability-${productID}`,
        AGENT_SESSIONS_BOOTSTRAP_VALUE: `secret-${productID}`,
        AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
      };
      const component = new ComponentClient({
        env: clientEnv, reconnectMinMs: 5, reconnectMaxMs: 20,
      });
      const pi = new FakePi();
      createPiFamilyExtension(productID, { componentClient: component })(pi);
      const nativeSessionID = `${productID}-native-persistent`;
      const ctx = context(nativeSessionID, true);
      try {
        await pi.fire("session_start", { reason: "startup" }, ctx);
        await until(() => received.some(({ ordinal, frame }) => ordinal === 1 && frame.type === "session.announce"));
        await until(() => component.bindingID === `binding-${productID}-1`);
        const firstAnnounce = received.find(({ ordinal, frame }) => ordinal === 1 && frame.type === "session.announce").frame;
        assert.equal(firstAnnounce.payload.native_session_id, nativeSessionID);
        assert.equal(firstAnnounce.payload.binding_id, `binding-${productID}-1`);

        await pi.fire("session_shutdown", { reason: "killed" }, ctx);
        assert.equal(component.stopping, false, "native peer death must not stop the daemon component connection");
        await pi.fire("session_start", { reason: "resume" }, ctx);
        await until(() => received.some(({ ordinal, frame }) => ordinal === 1 && frame.type === "session.state" &&
          frame.payload.native_session_id === nativeSessionID));
        assert.equal(received.some(({ frame }) => frame.type === "session.rebind" &&
          frame.payload.new_native_session_id !== nativeSessionID), false, "native resume must not substitute a session id");

        sockets[0].destroy();
        await until(() => component.daemonGeneration === 31 && component.bindingID === `binding-${productID}-2`);
        await until(() => received.some(({ ordinal, frame }) => ordinal === 2 && frame.type === "session.announce"));
        const reconnect = received.find(({ ordinal, frame }) => ordinal === 2 && frame.type === "reconnect").frame;
        assert.equal(reconnect.payload.prior_binding_id, `binding-${productID}-1`);
        assert.equal(reconnect.payload.prior_generation, 30);
        const secondAnnounce = received.find(({ ordinal, frame }) => ordinal === 2 && frame.type === "session.announce").frame;
        assert.equal(secondAnnounce.payload.binding_id, `binding-${productID}-2`);
        assert.equal(secondAnnounce.payload.native_session_id, nativeSessionID);
        assert.equal(process.env.AGENT_SESSIONS_SESSION_ID, nativeSessionID);
        assert.equal(process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID, `binding-${productID}-2`);
      } finally {
        await pi.fire("session_shutdown", { reason: "quit" }, ctx);
        await component.stop();
        for (const socket of sockets) socket.destroy();
        await closeServer(server);
        fs.rmSync(root, { recursive: true, force: true });
        delete process.env.AGENT_SESSIONS_SESSION_ID;
        delete process.env.AGENT_SESSIONS_NATIVE_SESSION_ID;
        delete process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID;
      }
    });
  }
});

test("OMP leaves busy steer text unwrapped for native interjection framing", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  createPiFamilyExtension("omp", { componentClient: component })(pi);
  const busy = context("omp-session", false);
  await pi.fire("session_start", { reason: "resume" }, busy);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "omp-session" });
  component.emit("delivery.present", {
    delivery_id: "delivery-omp", mode: "busy-steer",
    body: { version: 1, type: "delivery", message_id: "native-omp", content: "raw priority" },
  });
  await tick();
  assert.deepEqual(pi.messages, [{ content: "raw priority", options: { deliverAs: "steer" } }]);
  await pi.fire("agent_settled", {}, busy);
  assert.equal(component.sent.some((frame) => frame.type === "turn.event" && frame.payload.kind === "agent_settled"), false);
  await pi.fire("agent_end", {}, busy);
  assert.equal(component.sent.some((frame) => frame.type === "turn.event" && frame.payload.kind === "agent_end"), true);
});

test("Pi and OMP registered parent surfaces require exact binding and session and export exact native identity", async (t) => {
  for (const productID of ["pi", "omp"]) {
    await t.test(productID, async () => {
      const component = new FakeComponent();
      const pi = new FakePi();
      createPiFamilyExtension(productID, { componentClient: component })(pi);
      const nativeSessionID = `${productID}-parent`;
      const exact = context(nativeSessionID, true);
      await pi.fire("session_start", { reason: "startup" }, exact);
      assert.equal(process.env.AGENT_SESSIONS_SESSION_ID, nativeSessionID);
      assert.equal(process.env.AGENT_SESSIONS_NATIVE_SESSION_ID, nativeSessionID);

      component.emit("session.bound", { binding_id: "foreign-binding", native_session_id: nativeSessionID });
      const tool = pi.tools.get("agent_sessions");
      const wrongBinding = await tool.execute("model-call-wrong-binding", { operation: "peers.list", arguments: {} }, undefined, undefined, exact);
      assert.equal(wrongBinding.isError, true);
      assert.equal(component.calls.length, 0);

      component.emit("session.bound", { binding_id: "binding-1", native_session_id: nativeSessionID });
      const forged = await tool.execute("model-call-forged-session", { operation: "peers.list", arguments: {} }, undefined, undefined, context("forged", true));
      assert.equal(forged.isError, true);
      assert.equal(component.calls.length, 0);

      const accepted = await tool.execute("model-call", { operation: "peers.list", arguments: {} }, undefined, undefined, exact);
      assert.equal(accepted.isError, undefined);
      assert.equal(component.calls[0].operation, "peers.list");

      const commandContext = context(nativeSessionID, true);
      await pi.commands.get("lane").handler("lane.status {\"lane_id\":\"worker\"}", commandContext);
      assert.equal(component.calls[1].operation, "lane.status");
      assert.equal(commandContext.ui.notifications[0].level, "info");

      await pi.fire("session_shutdown", { reason: "quit" }, exact);
      delete process.env.AGENT_SESSIONS_SESSION_ID;
      delete process.env.AGENT_SESSIONS_NATIVE_SESSION_ID;
      delete process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID;
    });
  }
});
