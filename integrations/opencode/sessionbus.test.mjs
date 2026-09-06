import assert from "node:assert/strict";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { once } from "node:events";
import test from "node:test";

const ACTIONS = ["list", "send", "spawn", "describe", "run", "start", "wait", "status", "interrupt", "close", "forget"];

function fakeTool(definition) { return definition; }
fakeTool.schema = { enum: (values) => ({ values }), string: () => ({}), any: () => ({}), record: () => ({ default() { return this; } }) };

async function load() {
  globalThis.__kit = { ACTIONS, connectPeer() { throw new Error("unexpected peer"); }, validate() { return true; } };
  globalThis.__createClient = () => { throw new Error("unexpected v2 client"); };
  globalThis.__tool = fakeTool;
  let source = await readFile(new URL("./sessionbus.mjs", import.meta.url), "utf8");
  source = source
    .replace('import kit from "@sessionbus/kit";', "const kit = globalThis.__kit;")
    .replace('import { createOpencodeClient } from "@opencode-ai/sdk/v2/client";', "const createOpencodeClient = globalThis.__createClient;")
    .replace('import { tool } from "@opencode-ai/plugin";', "const tool = globalThis.__tool;");
  return import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Math.random()}`);
}

function peerFactory(records) {
  return (identity, deliver, environment) => {
    const peer = {
      identity, deliver, environment, rehellos: [], stopped: false,
      caller: { async action(action, argumentsValue) { records.actions.push({ identity, action, argumentsValue }); return { ok: true }; } },
      async rehello(signal, name, info) { assert.equal(signal, undefined); if (this.stopped) throw new Error("superseded"); this.rehellos.push({ name, info }); },
      shutdown() { this.stopped = true; },
    };
    records.peers.push(peer);
    return peer;
  };
}

function v2(records) {
  return { session: { async prompt(request) {
    records.prompts.push(request);
    return { response: { status: 200 }, data: { data: { admittedSeq: records.prompts.length - 1, id: request.id, sessionID: request.sessionID, prompt: request.prompt, delivery: request.delivery, timeCreated: 1 } } };
  } } };
}

const context = { sessionID: "ses_exact", messageID: "msg_tool", abort: new AbortController().signal };

test("extracted package imports its published kit", { skip: "@sessionbus/kit 0.1.0-pre.2 is not published; 0.1.0-pre.1 lacks context-bearing rehello and admitted-identity semantics" }, () => {});

test("lane plugin is presence-inert and sends one stateless action", async () => {
  const module = await load(), calls = [];
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2({ prompts: [] }) }), connectPeer() { throw new Error("lane created a peer"); }, privateAction: async (...args) => { calls.push(args); return { sessions: [] }; }, onExit() {} });
  const hooks = await plugin({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_LANE_SOCKET: "/tmp/lane.sock", SESSIONBUS_GROUPS: "[]" } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "Lane title", directory: "/work" } } } });
  assert.deepEqual(hooks.tool.sessionbus.args.action.values, ACTIONS);
  assert.deepEqual(JSON.parse(await hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context)), { sessions: [] });
  assert.deepEqual(calls[0].slice(0, 3), ["/tmp/lane.sock", "list", {}]);
});

test("plugin stays discovery-safe without a Sessionbus connection environment", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const hooks = await plugin({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: {} });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "Native", directory: "/work" } } } });
  assert.equal(records.peers.length, 0);
  await assert.rejects(hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context), /no live OpenCode peer/u);
});

test("unknown update publishes a peer, retitles it, and deletion retires it", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const environment = { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_LOCAL_KEY: "", SESSIONBUS_GROUPS: '["team"]' };
  const hooks = await plugin({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment });
  assert.deepEqual(environment, {});
  await hooks.event({ event: { type: "session.updated", properties: { sessionID: "ses_exact", info: { id: "ses_exact", title: "First title", directory: "/work" } } } });
  assert.deepEqual(records.peers[0].identity, { product: "opencode", session_id: "ses_exact", name: "First title", groups: ["team"], info: { cwd: "/work" } });
  assert.deepEqual(records.peers[0].environment, { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_LOCAL_KEY: "" });
  await hooks.event({ event: { type: "session.updated", properties: { sessionID: "ses_exact", info: { id: "ses_exact", title: "Second title", directory: "/next" } } } });
  assert.deepEqual(records.peers[0].rehellos, [{ name: "Second title", info: { cwd: "/next" } }]);
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "ses_exact" } } } });
  assert.equal(records.peers[0].stopped, true);
});

test("created session publishes before its requested title update", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] }, updates = [];
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const hooks = await plugin({ client: { session: { async update(request) { updates.push(request); return { response: { status: 200 }, data: { id: request.path.id, title: request.body.title } }; } } }, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_SESSION_NAME: "Named session", SESSIONBUS_GROUPS: "[]" } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "New session", directory: "/work" } } } });
  assert.equal(updates.length, 1);
  assert.equal(records.peers[0].identity.name, "New session");
  assert.deepEqual(records.peers[0].rehellos, [{ name: "Named session", info: { cwd: "/work" } }]);
});

test("created session must confirm the exact retitled identity", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const hooks = await plugin({ client: { session: { async update(request) { return { response: { status: 200 }, data: { id: `${request.path.id}-other`, title: request.body.title } }; } } }, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_SESSION_NAME: "Named session", SESSIONBUS_GROUPS: "[]" } });
  await assert.rejects(hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "New session", directory: "/work" } } } }), /did not confirm/u);
  assert.equal(records.peers.length, 1);
  assert.equal(records.peers[0].identity.name, "New session");
  assert.deepEqual(records.peers[0].rehellos, []);
});

test("deleted session cannot be republished by a held title update", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const hooks = await plugin({ client: { session: { async update(request) { await held; return { response: { status: 200 }, data: { id: request.path.id, title: request.body.title } }; } } }, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_SESSION_NAME: "Named session", SESSIONBUS_GROUPS: "[]" } });
  const created = hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "New session", directory: "/work" } } } });
  assert.equal(records.peers.length, 1);
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "ses_exact" } } } });
  assert.equal(records.peers[0].stopped, true);
  release();
  await created;
  assert.equal(records.peers.length, 1);
  assert.deepEqual(records.peers[0].rehellos, []);
  await assert.rejects(hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context), /no live OpenCode peer/u);
});

test("peer tool and delivery use the same exact session", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const hooks = await plugin({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_GROUPS: "[]" } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "Exact", directory: "/work" } } } });
  assert.deepEqual(JSON.parse(await hooks.tool.sessionbus.execute({ action: "status", arguments: { turn_id: "t-1" } }, context)), { ok: true });
  const cancelled = new AbortController();
  cancelled.abort();
  assert.deepEqual(await records.peers[0].deliver(cancelled.signal, { message_id: "cancelled" }, records.peers[0].identity), { disposition: "rejected", reason: "closing" });
  assert.equal(records.prompts.length, 0);
  const receipt = await records.peers[0].deliver(context.abort, { message_id: "delivery", from: { session_id: "from", name: "Sender", product: "codex", groups: [] }, body: "hello" }, records.peers[0].identity);
  assert.deepEqual(receipt, { disposition: "injected" });
  assert.equal(records.prompts[0].sessionID, "ses_exact");
  assert.equal(records.prompts[0].delivery, "steer");
  assert.match(records.prompts[0].prompt.text, /sessionbus-metadata/u);
});

test("peer delivery matches the shared native-message fixture", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2(records) }), connectPeer: peerFactory(records), onExit() {} });
  const hooks = await plugin({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_GROUPS: "[]" } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "Exact", directory: "/work" } } } });
  const fixture = JSON.parse(await readFile(new URL("../../wrappers/host/testdata/native-message-envelope.json", import.meta.url), "utf8"));
  await records.peers[0].deliver(context.abort, fixture.message, records.peers[0].identity);
  assert.equal(records.prompts[0].prompt.text, fixture.rendered);
});

test("peer delivery requires exact native admission", async () => {
  const module = await load(), records = { peers: [], actions: [], prompts: [] };
  const plugin = module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: { session: { async prompt(request) {
    return { response: { status: 200 }, data: { data: { admittedSeq: 0, id: `${request.id}-wrong`, sessionID: request.sessionID, prompt: request.prompt, delivery: request.delivery } } };
  } } } }), connectPeer: peerFactory(records), onExit() {} });
  const environment = { SESSIONBUS_SOCKET: "/tmp/bus.sock", SESSIONBUS_LOCAL_KEY: "", SESSIONBUS_GROUPS: "[]" };
  const hooks = await plugin({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment });
  assert.deepEqual(environment, {});
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "Exact", directory: "/work" } } } });
  await assert.rejects(records.peers[0].deliver(context.abort, { message_id: "delivery", from: { session_id: "from", product: "codex" }, body: "hello" }, records.peers[0].identity), /invalid admission/u);
});

test("lane hop accepts one bounded frame and rejects trailing or empty replies", async () => {
  const module = await load(), directory = await mkdtemp(join(tmpdir(), "sessionbus-opencode-"));
  let index = 0;
  const socketPath = join(directory, "lane.sock");
  const responses = ['{"result":{"sessions":[]}}\n', '{"result":{}}\n{"result":{}}\n', '{"result":{},"error":{"code":-32603,"message":"bad"}}\n', null];
  const server = net.createServer((socket) => socket.once("data", () => {
    const body = responses[index++];
    if (body === null) return socket.destroy();
    socket.write(body.slice(0, 8));
    socket.end(body.slice(8));
  }));
  server.listen(socketPath);
  await once(server, "listening");
  try {
    const hooks = await module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2({ prompts: [] }) }), onExit() {} })({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_LANE_SOCKET: socketPath, SESSIONBUS_GROUPS: "[]" } });
    assert.deepEqual(JSON.parse(await hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context)), { sessions: [] });
    await assert.rejects(hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context), /trailing frame/u);
    await assert.rejects(hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context), /exactly one result or error/u);
    await assert.rejects(hooks.tool.sessionbus.execute({ action: "list", arguments: {} }, context), /empty response/u);
  } finally {
    server.close();
    await once(server, "close");
    await rm(directory, { recursive: true, force: true });
  }
});

test("shell ingress requires and exports the exact native session", async () => {
  const module = await load();
  const hooks = await module.createPlugin({ tool: fakeTool, createClient: () => ({ v2: v2({ prompts: [] }) }), onExit() {} })({ client: {}, directory: "/work", serverUrl: new URL("http://127.0.0.1"), environment: { SESSIONBUS_LANE_SOCKET: "/tmp/lane", SESSIONBUS_GROUPS: "[]" } });
  const output = { env: {} };
  await hooks["shell.env"]({ sessionID: "ses_exact" }, output);
  assert.equal(output.env.SESSIONBUS_SESSION_ID, "ses_exact");
  await assert.rejects(hooks["shell.env"]({}, { env: {} }), /exact OpenCode session/u);
});
