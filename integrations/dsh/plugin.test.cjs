"use strict";

const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { applyWithEnvironment, createCordisPlugin } = require("./plugin.cjs");
const { explicitMCPEnvironment, validateSocket } = require("./mcp-env.cjs");

class FakeClient extends EventEmitter {
  constructor() {
    super();
    this.bindingID = "binding";
    this.sent = [];
    this.tools = [];
  }
  send(type, id, payload) { this.sent.push({ type, id, payload }); return true; }
  callTool(id, operation, argumentsValue) {
    this.tools.push({ id, operation, arguments: argumentsValue });
    return Promise.resolve({ ok: true });
  }
  observeRename(id, nativeSessionID, nativeName, productEventSeq) {
    this.sent.push({ type: "session.rename", id, payload: {
      native_session_id: nativeSessionID, native_name: nativeName, product_event_seq: productEventSeq,
    } });
    return true;
  }
  start() { return Promise.resolve({ active: true }); }
  stop() { return Promise.resolve(); }
}

function fixture() {
  const client = new FakeClient();
  const handlers = new Map();
  const agents = new Map();
  const registered = [];
  const ctx = {
    agents: {
      get: (id) => agents.get(id),
      list: () => [...agents.values()],
    },
    tools: { register: (tool) => registered.push(tool) },
    sessionTitle: { get: (session) => typeof session.nativeTitle === "string" ? { title: session.nativeTitle } : undefined },
    on: (event, handler) => handlers.set(event, handler),
    effect: () => {},
  };
  const defineTool = (value) => value;
  let messageSequence = 0;
  const createUserMessage = (value) => ({ ...value, id: "native-message-" + (++messageSequence), role: "user" });
  const plugin = createCordisPlugin({ client, defineTool, createUserMessage });
  plugin.apply(ctx);
  return { agents, client, ctx, handlers, plugin, registered };
}

function addManaged(value, agent) {
  value.agents.set(agent.id, agent);
  value.handlers.get("agent/created")({ agent });
  return agent;
}

test("Cordis protocol driver uses native followup for wake/queue and steer while busy", async () => {
  const value = fixture();
  const calls = [];
  addManaged(value, {
    id: "native", status: "idle", session: { header: { id: "native", cwd: "/work" }, nativeTitle: "native-title" },
    followup: (input) => { calls.push(["followup", input]); },
    steer: (input) => { calls.push(["steer", input]); },
  });
  value.client.emit("ready");
  await value.plugin.deliver(value.ctx, { delivery_id: "idle", native_session_id: "native", mode: "idle-wake", body: { text: "hello" } });
  await value.plugin.deliver(value.ctx, { delivery_id: "queued", native_session_id: "native", mode: "busy-follow-up", body: { text: "later" } });
  await value.plugin.deliver(value.ctx, { delivery_id: "busy", native_session_id: "native", mode: "busy-steer", body: { text: "more" } });
  await value.plugin.deliver(value.ctx, { delivery_id: "notice", native_session_id: "native", mode: "idle-wake", body: { content: [{ type: "text", text: "lane completed" }] } });
  assert.deepEqual(calls.map((call) => call[0]), ["followup", "followup", "steer", "followup"]);
  assert.deepEqual(value.client.sent.filter((frame) => frame.type === "delivery.accept").map((frame) => frame.payload.native_message_id), ["native-message-1", "native-message-2", "native-message-3", "native-message-4"]);
  assert.equal(calls.every((call) => call[1].role === "user" && call[1].id.startsWith("native-message-")), true);
  assert.equal(calls.at(-1)[1].content[0].text, "lane completed");
  assert.equal(value.client.sent.some((frame) => frame.type === "session.announce" && frame.payload.native_session_id === "native"), true);
});

test("Cordis ready/reconnect re-announces identity and durable turn/end emits completion", () => {
  const value = fixture();
  const agent = addManaged(value, { id: "native", status: "busy", session: { header: { id: "native", cwd: "/work" }, nativeTitle: "native-title" } });
  value.client.emit("ready");
  value.client.emit("ready");
  const announcements = value.client.sent.filter((frame) => frame.type === "session.announce");
  assert.equal(announcements.length, 3);
  assert.equal(announcements.every((frame) => frame.payload.native_session_id === "native" && frame.payload.native_name === "native-title"), true);
  value.handlers.get("session/event")(agent.session, { type: "turn/end", data: { turn: 3, reason: { kind: "aborted" } } });
  const terminal = value.client.sent.findLast((frame) => frame.type === "turn.event");
  assert.equal(terminal.payload.kind, "cancelled");
  assert.equal(terminal.payload.metadata.stop_reason, "aborted");
  assert.equal(terminal.payload.metadata.turn, 3);
});

test("native registered tool carries exact exec.agent witness", async () => {
  const value = fixture();
  const agent = addManaged(value, { id: "native", status: "idle", session: { header: { id: "native", cwd: "/work" }, nativeTitle: "native-title" } });
  assert.equal(value.registered.length, 1);
  await value.registered[0].execute({ action: "lanes.start", arguments: { product: "dsh" } }, { agent });
  assert.equal(value.client.tools[0].arguments.claimed_native_session_id, "native");
  await assert.rejects(value.registered[0].execute({ action: "peers.list" }, {}), /exec\.agent/);
  const controller = new AbortController();
  controller.abort();
  await value.registered[0].execute({ action: "peers.list" }, { agent, signal: controller.signal });
  const cancelledCall = value.client.tools.at(-1).id;
  assert.equal(value.client.sent.some((frame) => frame.type === "tool.cancel" && frame.payload.call_id === cancelledCall && frame.id !== cancelledCall), true);
});

test("managed profile selects one exact session and rejects invisible siblings", () => {
  const value = fixture();
  const managed = addManaged(value, { id: "managed", status: "idle", session: { header: { id: "managed", cwd: "/work" }, nativeTitle: "managed-title" } });
  assert.throws(() => value.handlers.get("agent/created")({
    agent: { id: "sibling", status: "idle", session: { header: { id: "sibling", cwd: "/other" }, nativeTitle: "sibling-title" } },
  }), /rejects sibling/);
  value.client.emit("ready");
  assert.equal(value.client.sent.filter((frame) => frame.type === "session.announce").every((frame) => frame.payload.native_session_id === managed.id), true);
  assert.equal(value.client.sent.some((frame) => JSON.stringify(frame).includes("sibling")), false);
});

test("managed plugin refuses a profile that already contains any session", () => {
  const client = new FakeClient();
  const existing = { id: "sibling" };
  const ctx = {
    agents: { get: () => existing, list: () => [existing] },
    tools: { register: () => assert.fail("tool registered before topology check") },
    sessionTitle: { get: () => undefined },
    on: () => {},
  };
  const plugin = createCordisPlugin({ client, defineTool: (value) => value, createUserMessage: (value) => value });
  assert.throws(() => plugin.apply(ctx), /before any native session exists/);
});

test("missing native title stays unannounced and product title events use observeRename", () => {
  const value = fixture();
  const agent = addManaged(value, { id: "native", status: "idle", session: { header: { id: "native", cwd: "/work" } } });
  value.client.emit("ready");
  assert.equal(value.client.sent.some((frame) => frame.type === "session.announce"), false);
  agent.session.nativeTitle = "first native title";
  value.handlers.get("session/event")(agent.session, { type: "session/title", seq: 4, data: { title: "first native title" } });
  assert.equal(value.client.sent.filter((frame) => frame.type === "session.announce").length, 1);
  agent.session.nativeTitle = "second native title";
  value.handlers.get("session/event")(agent.session, { type: "session/title", seq: 7, data: { title: "second native title" } });
  const rename = value.client.sent.findLast((frame) => frame.type === "session.rename");
  assert.equal(rename.id, "title-7");
  assert.equal(rename.payload.native_name, "second native title");
});

test("missing or non-canonical native cwd is rejected instead of synthesized", () => {
  for (const cwd of [undefined, "relative/work", "/work/../other"]) {
    const value = fixture();
    const session = { header: { id: "native", ...(cwd === undefined ? {} : { cwd }) }, nativeTitle: "native-title" };
    assert.throws(() => addManaged(value, { id: "native", status: "idle", session }), /incomplete native session identity/);
    assert.equal(value.client.sent.some((frame) => frame.type === "session.announce"), false);
  }
});

test("ambient profile stays inert before native helpers or Cordis services load", async () => {
  let loaded = false;
  const result = await applyWithEnvironment({}, {}, async () => {
    loaded = true;
    throw new Error("must remain inert");
  });
  assert.equal(result.active, false);
  assert.equal(loaded, false);
});

test("ACP lane policy overwrites and live-verifies persisted wider session policy", async () => {
  const handlers = new Map();
  const session = {
    header: { id: "native", cwd: "/work" },
    events: [
      { type: "sandbox/mode", data: { mode: "danger-full-access" } },
      { type: "approval/policy", data: { policy: "never" } },
    ],
  };
  const last = (type, field) => session.events.findLast((event) => event.type === type)?.data?.[field];
  const ctx = {
    agents: {},
    sandboxPolicy: { overrideOf: () => last("sandbox/mode", "mode") },
    on: (event, handler) => handlers.set(event, handler),
  };
  let peerHelpersLoaded = false;
  const result = await applyWithEnvironment(ctx, { AGENT_SESSIONS_DSH_LANE_POLICY: "workspace-write:ask" }, async () => {
    peerHelpersLoaded = true;
    throw new Error("peer helpers must remain unloaded in lane mode");
  }, async () => ({
    setSandboxMode: (target, mode) => target.events.push({ type: "sandbox/mode", data: { mode } }),
    setApprovalPolicy: (target, policy) => target.events.push({ type: "approval/policy", data: { policy } }),
    effectiveApprovalPolicy: (events) => events.findLast((event) => event.type === "approval/policy")?.data?.policy,
  }));
  assert.equal(result.active, true);
  assert.equal(peerHelpersLoaded, false);
  handlers.get("agent/created")({ agent: { id: "native", session } });
  assert.equal(last("sandbox/mode", "mode"), "workspace-write");
  assert.equal(last("approval/policy", "policy"), "ask");
});

test("ACP lane policy rejects unknown presets and peer/lane role conflict", async () => {
  assert.throws(() => applyWithEnvironment({}, { AGENT_SESSIONS_DSH_LANE_POLICY: "workspace-write:never" }), /unsupported exact/);
  assert.throws(() => applyWithEnvironment({}, {
    AGENT_SESSIONS_DSH_LANE_POLICY: "workspace-write:ask",
    AGENT_SESSIONS_COMPONENT_SOCKET: "/home/alice/component.sock",
    AGENT_SESSIONS_PRODUCT_ID: "dsh",
    AGENT_SESSIONS_ATTACHMENT_ID: "attachment",
    AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability",
    AGENT_SESSIONS_BOOTSTRAP_VALUE: "secret",
    AGENT_SESSIONS_COMPONENT_VERSION: "agent-sessions.component.v1-r1",
  }), /cannot be both/);
});

test("MCP env is explicit, non-secret, and sandbox-visible", (t) => {
  const environment = { HOME: "/home/alice", XDG_STATE_HOME: "/home/alice/.local/state" };
  const result = explicitMCPEnvironment({
    sessionID: "native",
    componentSocket: "/home/alice/.local/state/agent-sessions/component.sock",
    stateRoot: "/home/alice/.local/state/agent-sessions",
  }, environment);
  assert.equal(result.AGENT_SESSIONS_SESSION_ID, "native");
  assert.equal(Object.keys(result).some((name) => name.startsWith("DSH_") || /KEY|PASSWORD|SECRET|TOKEN/u.test(name)), false);
  assert.throws(() => validateSocket("/tmp/agent-sessions/component.sock", environment), /masks/);
  assert.throws(() => validateSocket("/private/tmp/agent-sessions/component.sock", {
    HOME: "/private/tmp/agent-sessions",
  }), /masks/);
  const symlinkRoot = fs.mkdtempSync(path.join(os.homedir(), ".agent-sessions-dsh-socket-"));
  t.after(() => fs.rmSync(symlinkRoot, { recursive: true, force: true }));
  fs.symlinkSync(os.tmpdir(), path.join(symlinkRoot, "escape"));
  assert.throws(() => validateSocket(path.join(symlinkRoot, "escape", "component.sock"), { HOME: symlinkRoot }), /masks/);
  assert.throws(() => explicitMCPEnvironment({
    sessionID: "native",
    componentSocket: "/home/alice/.local/state/agent-sessions/component.sock",
    stateRoot: "/tmp/agent-sessions",
  }, environment), /outside \/tmp/);
});

test("shipped CLI, ACP app, plugin, profile, and pnpm tuple is exact", () => {
  const plugin = JSON.parse(fs.readFileSync(path.join(__dirname, "package.json"), "utf8"));
  const profile = JSON.parse(fs.readFileSync(path.join(__dirname, "profile", "package.json"), "utf8"));
  assert.equal(plugin.name, "@agent-sessions/dsh-plugin");
  assert.equal(plugin.version, "0.1.2-alpha.3");
  assert.equal(plugin.peerDependencies["@deepseek-ai/dsh"], "0.1.2-alpha.3");
  assert.equal(plugin.peerDependencies["@deepseek-ai/dsh-acp-app"], "0.1.2-alpha.3");
  assert.equal(plugin.dependencies["@deepseek-ai/dsh-llm"], "0.1.2-alpha.3");
  assert.equal(plugin.dependencies["@deepseek-ai/dsh-sandbox-policy"], "0.1.2-alpha.3");
  assert.equal(plugin.dependencies["@deepseek-ai/dsh-user-approval"], "0.1.2-alpha.3");
  assert.equal(profile.name, "@agent-sessions/dsh-profile");
  assert.equal(profile.version, "0.1.2-alpha.3");
  assert.equal(profile.dependencies["@deepseek-ai/dsh-acp-app"], "0.1.2-alpha.3");
  assert.equal(profile.dependencies["@agent-sessions/dsh-plugin"], "0.1.2-alpha.3");
  assert.equal(plugin.packageManager, "pnpm@10.28.1");
  assert.equal(profile.packageManager, "pnpm@10.28.1");
});
