import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";

import { createPiFamilyExtension } from "./pifamily.mjs";
import liveSessionModule from "../shared/live-session.js";

const { renderDelivery } = liveSessionModule;

class FakeLiveSession extends EventEmitter {
  constructor(active = true) {
    super(); this.active = active; this.sessions = new Map(); this.reported = []; this.updated = [];
    this.accepted = []; this.rejected = []; this.calls = []; this.stopped = false;
  }
  async start() { return this.active ? { active: true } : { active: false, reason: "inert" }; }
  report(id, name, info = {}) { this.sessions.set(id, { id, name, info }); this.reported.push({ id, name, info }); return true; }
  updateName(id, name) { this.sessions.get(id).name = name; this.updated.push({ id, name }); return true; }
  closeSession(id) { this.sessions.delete(id); }
  acceptMessage(id, result) { this.accepted.push({ id, result }); return true; }
  rejectMessage(id, error) { this.rejected.push({ id, error: String(error) }); return true; }
  async callTool(id, callID, operation, argumentsValue) { this.calls.push({ id, callID, operation, argumentsValue }); return { ok: true }; }
  async stop() { this.stopped = true; }
}

class FakePi {
  constructor(name = "native") { this.handlers = new Map(); this.tools = new Map(); this.commands = new Map(); this.messages = []; this.name = name; }
  on(name, handler) { this.handlers.set(name, handler); }
  registerTool(tool) { this.tools.set(tool.name, tool); }
  registerCommand(name, command) { this.commands.set(name, command); }
  sendUserMessage(content, options) { this.messages.push({ content, options }); }
  getSessionName() { return this.name; }
  async setSessionName(name) { this.name = name; }
  async fire(name, event, ctx) { return this.handlers.get(name)?.(event, ctx); }
}

function context(id, idle = true) {
  return {
    cwd: "/work", sessionManager: { getSessionId: () => id }, isIdle: () => idle,
    ui: { notifications: [], notify(text, level) { this.notifications.push({ text, level }); } },
  };
}
const tick = () => new Promise((resolve) => setImmediate(resolve));

test("Pi and OMP report the current product session and its initial native title", async (t) => {
  for (const product of ["pi", "omp"]) await t.test(product, async () => {
    const live = new FakeLiveSession();
    const pi = new FakePi("first");
    createPiFamilyExtension(product, { liveSessionClient: live })(pi);
    const ctx = context(`${product}-session`);
    await pi.fire("session_start", {}, ctx);
    assert.deepEqual(live.reported, [{ id: `${product}-session`, name: "first", info: { cwd: "/work" } }]);
  });
});

test("Pi reports its native session title event", async () => {
  const live = new FakeLiveSession();
  const pi = new FakePi("first");
  createPiFamilyExtension("pi", { liveSessionClient: live })(pi);
  const ctx = context("pi-session");
  await pi.fire("session_start", {}, ctx);
  await pi.fire("session_info_changed", { name: "renamed" }, ctx);
  assert.deepEqual(live.updated, [{ id: "pi-session", name: "renamed" }]);
});

test("Pi and OMP seed a requested fresh name through the product title writer", async (t) => {
  for (const product of ["pi", "omp"]) await t.test(product, async () => {
    const live = new FakeLiveSession();
    const pi = new FakePi("");
    createPiFamilyExtension(product, {
      liveSessionClient: live,
      environment: { AGENT_SESSIONS_SESSION_NAME: `named-${product}` },
    })(pi);
    await pi.fire("session_start", {}, context(`${product}-session`));
    assert.equal(pi.getSessionName(), `named-${product}`);
    assert.deepEqual(live.reported, [{ id: `${product}-session`, name: `named-${product}`, info: { cwd: "/work" } }]);
  });
});

test("a synchronous native title event during startup is carried by the initial report", async () => {
  const live = new FakeLiveSession();
  const pi = new FakePi("");
  const ctx = context("pi-session");
  createPiFamilyExtension("pi", {
    liveSessionClient: live,
    environment: { AGENT_SESSIONS_SESSION_NAME: "named-pi" },
  })(pi);
  pi.setSessionName = async function setSessionName(name) {
    this.name = name;
    await this.fire("session_info_changed", { name }, ctx);
  };
  await pi.fire("session_start", {}, ctx);
  assert.deepEqual(live.reported, [{ id: "pi-session", name: "named-pi", info: { cwd: "/work" } }]);
  assert.deepEqual(live.updated, []);
});

test("Pi-family reconnect preserves an existing product title", async () => {
  const live = new FakeLiveSession();
  const pi = new FakePi("product-owned");
  createPiFamilyExtension("pi", {
    liveSessionClient: live,
    environment: { AGENT_SESSIONS_SESSION_NAME: "stale-launch-name" },
  })(pi);
  await pi.fire("session_start", {}, context("pi-session"));
  assert.equal(pi.getSessionName(), "product-owned");
  assert.deepEqual(live.reported, [{ id: "pi-session", name: "product-owned", info: { cwd: "/work" } }]);
});

test("Pi family live delivery uses ordinary send when idle and native steer when busy", async () => {
  const live = new FakeLiveSession();
  const pi = new FakePi();
  createPiFamilyExtension("omp", { liveSessionClient: live })(pi);
  const ctx = context("omp-session", false);
  await pi.fire("session_start", {}, ctx);
	const delivery = {
		messageID: "delivery", nativeSessionID: "omp-session", body: "priority",
		from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] },
	};
	live.emit("message", delivery);
  await tick();
	assert.deepEqual(pi.messages, [{ content: renderDelivery(delivery), options: { deliverAs: "steer" } }]);
  assert.deepEqual(live.accepted.map((value) => value.id), ["delivery"]);
});

test("Pi family tools stay tied to the reported live native session", async () => {
  const live = new FakeLiveSession();
  const pi = new FakePi();
  createPiFamilyExtension("pi", { liveSessionClient: live })(pi);
  const exact = context("pi-session");
  await pi.fire("session_start", {}, exact);
  const tool = pi.tools.get("agent_sessions");
  const accepted = await tool.execute("call", { operation: "peers.list", arguments: {} }, undefined, undefined, exact);
  assert.equal(accepted.isError, undefined);
  assert.equal(live.calls[0].id, "pi-session");
  await tool.execute("identity", { operation: "identity", arguments: {} }, undefined, undefined, exact);
  assert.equal(live.calls[1].operation, "identity");
  const rejected = await tool.execute("call", { operation: "peers.list", arguments: {} }, undefined, undefined, context("other"));
  assert.equal(rejected.isError, true);
  await pi.fire("session_shutdown", { reason: "quit" }, exact);
  assert.equal(live.sessions.has("pi-session"), false);
});
