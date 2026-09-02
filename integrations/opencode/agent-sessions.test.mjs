import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";
import test from "node:test";

class FakeLiveSession extends EventEmitter {
  constructor(active = true) {
    super(); this.active = active; this.sessions = new Map(); this.reported = []; this.updated = [];
    this.accepted = []; this.rejected = []; this.calls = [];
  }
  async start() { return this.active ? { active: true } : { active: false, reason: "inert" }; }
  report(id, name) { this.sessions.set(id, { id, name }); this.reported.push({ id, name }); return true; }
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
fakeTool.schema = { enum: () => ({}), any: () => ({}), record: () => ({ default() { return this; } }) };

async function loadPlugin(live, deadline = 10_000) {
  globalThis.__testTool = fakeTool;
  globalThis.__testLiveFactory = () => live;
  let source = await readFile(new URL("./agent-sessions.mjs", import.meta.url), "utf8");
  source = source
    .replace('import { tool } from "@opencode-ai/plugin";', "const tool = globalThis.__testTool;")
    .replace('import liveSessionModule from "../shared/live-session.js";', "")
    .replace('const { createLiveSessionClient } = liveSessionModule;', "const createLiveSessionClient = globalThis.__testLiveFactory;")
    .replace("const DELIVERY_DEADLINE_MS = 10_000;", `const DELIVERY_DEADLINE_MS = ${deadline};`);
  return (await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Math.random()}`)).default;
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test("OpenCode reports live sessions, title changes, and closes on deletion", async () => {
  const live = new FakeLiveSession();
  const hooks = await (await loadPlugin(live))({ client: {}, directory: "/work" });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "", directory: "/work" } } } });
  assert.deepEqual(live.reported, [{ id: "ses_one", name: "" }]);
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_one", title: "native", directory: "/work" } } } });
  assert.deepEqual(live.updated, [{ id: "ses_one", name: "native" }]);
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "ses_one" } } } });
  assert.equal(live.sessions.has("ses_one"), false);
});

test("OpenCode delivers and calls tools on the exact reported session", async () => {
  const live = new FakeLiveSession();
  const prompts = [];
  const client = { session: { async promptAsync(request) { prompts.push(request); return { data: {}, response: { status: 204 } }; } } };
  const hooks = await (await loadPlugin(live))({ client, directory: "/work" });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.equal(prompts[0].path.id, "ses_one");
  assert.deepEqual(live.accepted.map((value) => value.id), ["delivery"]);
  const result = await hooks.tool.agent_sessions.execute({ operation: "peers.list", arguments: {} }, {
    sessionID: "ses_one", messageID: "message", abort: new AbortController().signal,
  });
  assert.equal(JSON.parse(result).ok, true);
  assert.equal(live.calls[0].id, "ses_one");
  await assert.rejects(hooks.tool.agent_sessions.execute({ operation: "peers.list", arguments: {} }, {
    sessionID: "ses_other", messageID: "message", abort: new AbortController().signal,
  }), /reported live/u);
});

test("OpenCode truthfully rejects native prompt refusal", async () => {
  const live = new FakeLiveSession();
  const hooks = await (await loadPlugin(live))({
    client: { session: { promptAsync: async () => ({ error: {}, response: { status: 409 } }) } }, directory: "/work",
  });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_one", title: "one", directory: "/work" } } } });
  live.emit("message", { messageID: "delivery", nativeSessionID: "ses_one", body: "hello" });
  await tick();
  assert.equal(live.accepted.length, 0);
  assert.equal(live.rejected.length, 1);
});
