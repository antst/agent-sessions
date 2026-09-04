import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";
import test from "node:test";
import liveSessionModule from "../shared/live-session.js";

class FakeLiveSession extends EventEmitter {
  constructor(active = true) {
    super(); this.active = active; this.sessions = new Map(); this.reported = []; this.updated = [];
    this.accepted = []; this.rejected = []; this.calls = [];
  }
  async start() { return this.active ? { active: true } : { active: false, reason: "inert" }; }
  report(id, name, info = {}) { this.sessions.set(id, { id, name, info }); this.reported.push({ id, name, info }); return true; }
  updateName(id, name) { this.sessions.get(id).name = name; this.updated.push({ id, name }); return true; }
  closeSession(id) { this.sessions.delete(id); }
  acceptMessage(id, result) { this.accepted.push({ id, result }); return true; }
  rejectMessage(id, error) { this.rejected.push({ id, error: String(error) }); return true; }
  async callTool(id, callID, operation, argumentsValue) {
    this.calls.push({ id, callID, operation, argumentsValue }); return { ok: true };
  }
  async stop() {}
}

function fakeTool(definition) { return definition; }
fakeTool.schema = { enum: (values) => ({ values }), string: () => ({}), any: () => ({}), record: () => ({ default() { return this; } }) };

async function loadPlugin(live, deadline = 10_000) {
  globalThis.__testTool = fakeTool;
  globalThis.__testLiveSessionModule = { ...liveSessionModule, createLiveSessionClient: () => live, renderDelivery: (payload) => payload.body };
  let source = await readFile(new URL("./agent-sessions.mjs", import.meta.url), "utf8");
  source = source
    .replace('import { tool } from "@opencode-ai/plugin";', "const tool = globalThis.__testTool;")
    .replace('import liveSessionModule from "../shared/live-session.js";', "const liveSessionModule = globalThis.__testLiveSessionModule;")
    .replace("const DELIVERY_DEADLINE_MS = 10_000;", `const DELIVERY_DEADLINE_MS = ${deadline};`);
  return (await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Math.random()}`)).default;
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test("OpenCode reports live sessions, title changes, and closes on deletion", async () => {
  const live = new FakeLiveSession();
  const hooks = await (await loadPlugin(live))({ client: {}, directory: "/work" });
  assert.deepEqual(hooks.tool.agent_sessions.args.operation.values, liveSessionModule.CLIENT_OPERATIONS);
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "", directory: "/work" } } } });
  assert.deepEqual(live.reported, [{ id: "ses_one", name: "", info: { cwd: "/work" } }]);
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_one", title: "native", directory: "/work" } } } });
  assert.deepEqual(live.updated, [{ id: "ses_one", name: "native" }]);
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "ses_one" } } } });
  assert.equal(live.sessions.has("ses_one"), false);
});

test("OpenCode writes a requested fresh name through the native session API before reporting", async () => {
  const live = new FakeLiveSession();
  const updates = [];
  const client = { session: { async update(request) {
    updates.push(request);
    return { data: { id: request.path.id, title: request.body.title }, response: { status: 200 } };
  } } };
  const hooks = await (await loadPlugin(live))({
    client,
    directory: "/work",
    environment: { AGENT_SESSIONS_SESSION_NAME: "named-opencode" },
  });
  await hooks.event({ event: { type: "session.created", properties: {
    info: { id: "ses_one", title: "New session", directory: "/work" },
  } } });
  assert.deepEqual(updates, [{
    path: { id: "ses_one" }, query: { directory: "/work" }, body: { title: "named-opencode" },
  }]);
  assert.deepEqual(live.reported, [{ id: "ses_one", name: "named-opencode", info: { cwd: "/work" } }]);
});

test("OpenCode reports an exact resumed session directly from the product", async () => {
  const live = new FakeLiveSession();
  const gets = [];
  const client = { session: { async get(request) {
    gets.push(request);
    return { data: { id: request.path.id, title: "existing-title", directory: "/work" }, response: { status: 200 } };
  } } };
  await (await loadPlugin(live))({
    client,
    directory: "/work",
    environment: { AGENT_SESSIONS_SESSION_ID: "ses_exact" },
  });
  await tick();
  assert.deepEqual(gets, [{ path: { id: "ses_exact" }, query: { directory: "/work" } }]);
  assert.deepEqual(live.reported, [{ id: "ses_exact", name: "existing-title", info: { cwd: "/work" } }]);
});

test("OpenCode delivers and calls tools on the exact reported session", async () => {
  const live = new FakeLiveSession();
  const prompts = [];
  const client = { session: {
    async messages() {
      return { data: [{ info: { role: "user", agent: "brainstormer", model: { providerID: "google", modelID: "gemini" } } }], response: { status: 200 } };
    },
    async promptAsync(request) { prompts.push(request); return { data: {}, response: { status: 204 } }; },
  } };
  const hooks = await (await loadPlugin(live))({ client, directory: "/work" });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.equal(prompts[0].path.id, "ses_one");
  assert.equal(prompts[0].body.agent, "brainstormer");
  assert.deepEqual(prompts[0].body.model, { providerID: "google", modelID: "gemini" });
  assert.deepEqual(live.accepted.map((value) => value.id), ["delivery"]);
  for (const operation of ["lane.doctor", "lane.list"]) {
    const result = await hooks.tool.agent_sessions.execute({ operation, arguments: { product: "codex", arguments: [] } }, {
      sessionID: "ses_one", messageID: operation, abort: new AbortController().signal,
    });
    assert.equal(JSON.parse(result).ok, true);
  }
  assert.deepEqual(live.calls.map(({ id, operation }) => ({ id, operation })), [
    { id: "ses_one", operation: "lane.doctor" }, { id: "ses_one", operation: "lane.list" },
  ]);
  await assert.rejects(hooks.tool.agent_sessions.execute({ operation: "peers.list", arguments: {} }, {
    sessionID: "ses_other", messageID: "message", abort: new AbortController().signal,
  }), /reported live/u);
});

test("OpenCode truthfully rejects native prompt refusal", async () => {
  const live = new FakeLiveSession();
  const hooks = await (await loadPlugin(live))({
    client: { session: {
      messages: async () => ({ data: [{ info: { role: "user", agent: "build", model: { providerID: "openai", modelID: "gpt" } } }], response: { status: 200 } }),
      promptAsync: async () => ({ error: {}, response: { status: 409 } }),
    } }, directory: "/work",
  });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.equal(live.accepted.length, 0);
  assert.equal(live.rejected.length, 1);
});

test("OpenCode refuses delivery without product-confirmed prompt configuration", async () => {
  const live = new FakeLiveSession();
  let prompted = false;
  const hooks = await (await loadPlugin(live))({
    client: { session: {
      messages: async () => ({ data: [], response: { status: 200 } }),
      promptAsync: async () => { prompted = true; return { data: {}, response: { status: 204 } }; },
    } }, directory: "/work",
  });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.equal(prompted, false);
  assert.equal(live.accepted.length, 0);
  assert.equal(live.rejected.length, 1);
  assert.match(live.rejected[0].error, /product-confirmed prompt configuration/u);
});
