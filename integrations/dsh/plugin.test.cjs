"use strict";

const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
const test = require("node:test");
const { applyWithEnvironment, createCordisPlugin } = require("./plugin.cjs");

class FakeLiveSession extends EventEmitter {
  constructor() { super(); this.sessions = new Map(); this.reported = []; this.updated = []; this.accepted = []; this.calls = []; }
  report(id, name) { this.sessions.set(id, { id, name }); this.reported.push({ id, name }); return true; }
  updateName(id, name) { this.sessions.get(id).name = name; this.updated.push({ id, name }); return true; }
  closeSession(id) { this.sessions.delete(id); }
  acceptMessage(id, result) { this.accepted.push({ id, result }); return true; }
  rejectMessage() { return true; }
  async callTool(id, callID, operation, argumentsValue) { this.calls.push({ id, callID, operation, argumentsValue }); return { ok: true }; }
  start() { return Promise.resolve({ active: true }); }
  stop() { return Promise.resolve(); }
}

function fixture(initialName = "") {
  const client = new FakeLiveSession();
  const handlers = new Map(), agents = new Map(), tools = [];
  const ctx = {
    agents: { get: (id) => agents.get(id), list: () => [...agents.values()] },
    tools: { register: (tool) => tools.push(tool) },
    sessionTitle: {
      get: (session) => typeof session.title === "string" ? { title: session.title } : undefined,
      rename: (session, title) => { session.title = title; return { title, eventSeq: 1 }; },
    },
    on: (event, handler) => handlers.set(event, handler), effect: () => {},
  };
  let message = 0;
  const plugin = createCordisPlugin({
    client, defineTool: (value) => value,
    createUserMessage: (value) => ({ ...value, id: `native-${++message}`, role: "user" }),
    initialName,
  });
  plugin.apply(ctx);
  return { client, handlers, agents, tools, ctx, plugin };
}

function add(value, status = "idle") {
  const calls = [];
  const session = { header: { id: "native", cwd: "/work" }, title: "native title" };
  const agent = { id: "native", status, session, followup: (input) => calls.push(["followup", input]), steer: (input) => calls.push(["steer", input]) };
  value.agents.set(agent.id, agent);
  value.handlers.get("agent/created")({ agent });
  return { agent, calls };
}

test("DSH reports one live session, updates its title, and closes it", () => {
  const value = fixture();
  const { agent } = add(value);
  assert.deepEqual(value.client.reported, [{ id: "native", name: "native title" }]);
  agent.session.title = "renamed";
  value.handlers.get("session/event")(agent.session, { type: "session/title", seq: 1 });
  agent.session.title = "";
  value.handlers.get("session/event")(agent.session, { type: "session/title", seq: 2 });
  assert.deepEqual(value.client.updated, [{ id: "native", name: "renamed" }, { id: "native", name: "" }]);
  value.handlers.get("agent/disposed")({ agent });
  assert.equal(value.client.sessions.has("native"), false);
});

test("DSH reports an untitled session and writes the requested launch name through the product", () => {
  const unnamed = fixture();
  const unnamedAgent = add(unnamed);
  delete unnamedAgent.agent.session.title;
  unnamed.client.reported.length = 0;
  unnamed.plugin.announce(unnamed.ctx, unnamedAgent.agent);
  assert.deepEqual(unnamed.client.reported, [{ id: "native", name: "" }]);

  const named = fixture("launch name");
  const { agent } = add(named);
  assert.equal(agent.session.title, "launch name");
  assert.deepEqual(named.client.reported, [{ id: "native", name: "launch name" }]);
});

test("DSH live delivery follows up when idle and steers when busy", async () => {
  const value = fixture();
  const { agent, calls } = add(value);
  await value.plugin.deliver(value.ctx, { messageID: "one", nativeSessionID: "native", body: "hello" });
  agent.status = "busy";
  await value.plugin.deliver(value.ctx, { messageID: "two", nativeSessionID: "native", body: "priority" });
  assert.deepEqual(calls.map((entry) => entry[0]), ["followup", "steer"]);
  assert.deepEqual(value.client.accepted.map((entry) => entry.id), ["one", "two"]);
});

test("DSH parent tool uses the exact live native session", async () => {
  const value = fixture();
  const { agent } = add(value);
  assert.deepEqual(value.tools[0].parameters.action.enum, [
    "peers.list", "message.send", "lane.start", "lane.run", "lane.resume", "lane.wait",
    "lane.status", "lane.steer", "lane.interrupt", "lane.collect", "lane.archive",
  ]);
  assert.match(value.tools[0].description, /message\.send with \{target or targets, message, optional summary\}/u);
  assert.deepEqual(await value.tools[0].execute({ action: "peers.list", arguments: {} }, { agent }), { ok: true });
  assert.equal(value.client.calls[0].id, "native");
  await assert.rejects(value.tools[0].execute({ action: "peers.list" }, {}), /exec\.agent/u);
});

test("DSH profile rejects sibling sessions and stays inert without live settings", async () => {
  const value = fixture();
  add(value);
  assert.throws(() => value.handlers.get("agent/created")({ agent: { id: "sibling" } }), /sibling/u);
  let loaded = false;
  const result = await applyWithEnvironment({}, {}, async () => { loaded = true; });
  assert.equal(result.active, false);
  assert.equal(loaded, false);
});
