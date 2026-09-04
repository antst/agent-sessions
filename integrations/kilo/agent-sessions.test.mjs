import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";
import test from "node:test";
import liveSessionModule from "../shared/live-session.js";

function acceptsDeliveryResult(id, result) {
  const client = new liveSessionModule.LiveSessionClient({ env: {} });
  client.inbound.set(id, { session: {}, wireID: id, params: {} });
  client._write = () => true;
  return client.acceptMessage(id, result);
}

class FakeLiveSession extends EventEmitter {
  constructor() { super(); this.active = true; this.sessions = new Map(); this.reported = []; this.updated = []; this.accepted = []; this.rejected = []; this.calls = []; }
  async start() { return { active: true }; }
  report(id, name, info = {}) { this.sessions.set(id, { id, name, info }); this.reported.push({ id, name, info }); return true; }
  updateName(id, name) { this.sessions.get(id).name = name; this.updated.push({ id, name }); return true; }
  closeSession(id) { this.sessions.delete(id); }
  acceptMessage(id, result) { if (!acceptsDeliveryResult(id, result)) return false; this.accepted.push({ id, result }); return true; }
  rejectMessage(id, error) { this.rejected.push({ id, error: String(error) }); return true; }
  async callTool(id, callID, operation, argumentsValue) { this.calls.push({ id, callID, operation, argumentsValue }); return { ok: true }; }
  async stop() {}
}

function fakeTool(definition) { return definition; }
fakeTool.schema = { enum: (values) => ({ values }), string: () => ({}), any: () => ({}), record: () => ({ default() { return this; } }) };

async function loadPlugin(live) {
  globalThis.__testTool = fakeTool;
  globalThis.__testLiveSessionModule = { ...liveSessionModule, createLiveSessionClient: () => live, renderDelivery: (payload) => payload.body };
  let source = await readFile(new URL("./agent-sessions.mjs", import.meta.url), "utf8");
  source = source
    .replace('import { tool } from "@kilocode/plugin";', "const tool = globalThis.__testTool;")
    .replace('import liveSessionModule from "../shared/live-session.js";', "const liveSessionModule = globalThis.__testLiveSessionModule;");
  return (await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Math.random()}`)).default;
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test("Kilo reports live sessions and title changes", async () => {
  const live = new FakeLiveSession();
  const hooks = await (await loadPlugin(live))({ client: {}, directory: "/work" });
  assert.deepEqual(hooks.tool.agent_sessions.args.operation.values, liveSessionModule.CLIENT_OPERATIONS);
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "", directory: "/work" } } } });
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_one", title: "native", directory: "/work" } } } });
  assert.deepEqual(live.reported, [{ id: "ses_one", name: "", info: { cwd: "/work" } }]);
  assert.deepEqual(live.updated, [{ id: "ses_one", name: "native" }]);
});

test("Kilo writes a requested fresh name through the native session API before reporting", async () => {
  const live = new FakeLiveSession();
  const updates = [];
  const client = { session: { async update(request) {
    updates.push(request);
    return { data: { id: request.path.id, title: request.body.title }, response: { status: 200 } };
  } } };
  const hooks = await (await loadPlugin(live))({
    client,
    directory: "/work",
    environment: { AGENT_SESSIONS_SESSION_NAME: "named-kilo" },
  });
  await hooks.event({ event: { type: "session.created", properties: {
    info: { id: "ses_one", title: "New session", directory: "/work" },
  } } });
  assert.deepEqual(updates, [{
    path: { id: "ses_one" }, query: { directory: "/work" }, body: { title: "named-kilo" },
  }]);
  assert.deepEqual(live.reported, [{ id: "ses_one", name: "named-kilo", info: { cwd: "/work" } }]);
});

test("Kilo reports an exact resumed session directly from the product", async () => {
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

test("Kilo submits to the exact live session and acknowledges native evidence", async () => {
  const live = new FakeLiveSession();
  const messages = [];
  const client = {
    session: { messages: async () => ({ data: [...messages], response: { status: 200 } }) },
    tui: {
      clearPrompt: async () => ({ data: true, response: { status: 200 } }),
      appendPrompt: async () => ({ data: true, response: { status: 200 } }),
      submitPrompt: async () => {
        messages.push({ info: { id: "msg_one", sessionID: "ses_one", role: "user" }, parts: [{ type: "text", text: "hello" }] });
        return { data: true, response: { status: 200 } };
      },
    },
  };
  const hooks = await (await loadPlugin(live))({ client, directory: "/work" });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.deepEqual(live.accepted, [{ id: "delivery", result: {} }]);
  for (const operation of ["lane.doctor", "lane.list"]) {
    const result = await hooks.tool.agent_sessions.execute({ operation, arguments: { product: "codex", arguments: [] } }, {
      sessionID: "ses_one", messageID: operation, abort: new AbortController().signal,
    });
    assert.equal(JSON.parse(result).ok, true);
  }
  assert.deepEqual(live.calls.map(({ id, operation }) => ({ id, operation })), [
    { id: "ses_one", operation: "lane.doctor" }, { id: "ses_one", operation: "lane.list" },
  ]);
});

test("Kilo rejects failed native submission", async () => {
  const live = new FakeLiveSession();
  const client = {
    session: { messages: async () => ({ data: [], response: { status: 200 } }) },
    tui: { clearPrompt: async () => ({ error: {}, response: { status: 409 } }), appendPrompt: async () => ({}), submitPrompt: async () => ({}) },
  };
  const hooks = await (await loadPlugin(live))({ client, directory: "/work" });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.equal(live.accepted.length, 0);
  assert.equal(live.rejected.length, 1);
});
