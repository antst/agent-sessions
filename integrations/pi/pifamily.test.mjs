import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";

import { createPiFamilyExtension } from "./pifamily.mjs";

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
  constructor() {
    this.handlers = new Map();
    this.tools = new Map();
    this.commands = new Map();
    this.messages = [];
    this.names = [];
  }
  on(name, handler) { this.handlers.set(name, handler); }
  registerTool(tool) { this.tools.set(tool.name, tool); }
  registerCommand(name, command) { this.commands.set(name, command); }
  sendUserMessage(content, options) { this.messages.push({ content, options }); }
  getSessionName() { return "native name"; }
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

test("cleared native title observes a nonempty fallback without confirming a pending rename", async () => {
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
  assert.equal(observation.payload.native_name, "pi session");
  assert.equal(Number.isSafeInteger(observation.payload.product_event_seq), true);

  await pi.fire("session_info_changed", { name: "daemon title" }, ctx);
  assert.equal((await resultPromise).nativeName, "daemon title");
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

test("component reconnect republishes exact session identity for post-admission routing", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  createPiFamilyExtension("pi", { componentClient: component })(pi);
  const ctx = context("pi-reconnect", true);
  await pi.fire("session_start", { reason: "startup" }, ctx);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "pi-reconnect" });
  component.bindingID = "binding-2";
  component.emit("ready", { bindingID: "binding-2", daemonGeneration: 8 });
  const announces = component.sent.filter((frame) => frame.type === "session.announce");
  assert.equal(announces.length, 2);
  assert.equal(announces[1].payload.binding_id, "binding-2");
  assert.equal(announces[1].payload.native_session_id, "pi-reconnect");
  assert.equal(process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID, "binding-2");
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

test("registered tool rejects a foreign handler session and command uses the same attested route", async () => {
  const component = new FakeComponent();
  const pi = new FakePi();
  createPiFamilyExtension("pi", { componentClient: component })(pi);
  const exact = context("pi-parent", true);
  await pi.fire("session_start", { reason: "startup" }, exact);
  component.emit("session.bound", { binding_id: "binding-1", native_session_id: "pi-parent" });

  const tool = pi.tools.get("agent_sessions");
  const forged = await tool.execute("model-call", { operation: "peers.list", arguments: {} }, undefined, undefined, context("forged", true));
  assert.equal(forged.isError, true);
  assert.equal(component.calls.length, 0);

  const accepted = await tool.execute("model-call", { operation: "peers.list", arguments: {} }, undefined, undefined, exact);
  assert.equal(accepted.isError, undefined);
  assert.equal(component.calls[0].operation, "peers.list");

  const commandContext = context("pi-parent", true);
  await pi.commands.get("lane").handler("lane.status {\"lane_id\":\"worker\"}", commandContext);
  assert.equal(component.calls[1].operation, "lane.status");
  assert.equal(commandContext.ui.notifications[0].level, "info");
});
