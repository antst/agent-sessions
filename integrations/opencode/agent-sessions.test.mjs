import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";
import test from "node:test";
import componentProtocolModule from "../shared/component/protocol.js";

const { validNativeTitleObservation } = componentProtocolModule;

class FakeComponent extends EventEmitter {
  constructor(active = true) {
    super();
    this.active = active;
    this.bindingID = "binding-opencode";
    this.sent = [];
    this.calls = [];
    this.observed = [];
  }
  async start() { return this.active ? { active: true, bindingID: this.bindingID } : { active: false, reason: "inert" }; }
  send(type, id, payload) { this.sent.push({ type, id, payload }); return true; }
  observeRename(nativeEventID, nativeSessionID, nativeName, productEventSeq) {
    this.observed.push({ nativeEventID, nativeSessionID, nativeName, productEventSeq });
    return true;
  }
  async callTool(callID, operation, argumentsValue) {
    this.calls.push({ callID, operation, argumentsValue });
    return { ok: true };
  }
  async stop() {}
}

function fakeTool(definition) { return definition; }
fakeTool.schema = {
  enum: (values) => ({ values }),
  any: () => ({}),
  record: () => ({ default() { return this; } }),
};

async function loadPlugin(component, deliveryDeadlineMS = 10_000) {
  globalThis.__agentSessionsTestTool = fakeTool;
  globalThis.__agentSessionsTestValidNativeTitleObservation = validNativeTitleObservation;
  globalThis.__agentSessionsTestComponentFactory = (options = {}) => {
    component.renameSession = options.renameSession;
    return component;
  };
  let source = await readFile(new URL("./agent-sessions.mjs", import.meta.url), "utf8");
  source = source
    .replace('import { tool } from "@opencode-ai/plugin";', "const tool = globalThis.__agentSessionsTestTool;")
    .replace('import componentModule from "../shared/component/client.js";', "")
    .replace('const { createComponentClient } = componentModule;', "const createComponentClient = globalThis.__agentSessionsTestComponentFactory;")
    .replace('import componentProtocolModule from "../shared/component/protocol.js";', "")
    .replace('const { validNativeTitleObservation } = componentProtocolModule;', "const validNativeTitleObservation = globalThis.__agentSessionsTestValidNativeTitleObservation;")
    .replace("const DELIVERY_DEADLINE_MS = 10_000;", `const DELIVERY_DEADLINE_MS = ${deliveryDeadlineMS};`);
  const encoded = Buffer.from(source).toString("base64");
  return (await import(`data:text/javascript;base64,${encoded}#${Date.now()}-${Math.random()}`)).default;
}

const tick = () => new Promise((resolve) => setImmediate(resolve));
const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

test("OpenCode plugin is inert without managed component activation", async () => {
  const component = new FakeComponent(false);
  const plugin = await loadPlugin(component);
  assert.deepEqual(await plugin({ client: {}, directory: "/work/project" }), {});
  assert.equal(component.sent.length, 0);
});

test("OpenCode projects genuine empty titles, clear events, and no shell fabrication", async () => {
  const component = new FakeComponent();
  const plugin = await loadPlugin(component);
  const hooks = await plugin({ client: {}, directory: "/work/project" });

  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_title", title: "", directory: "/work/project" } } } });
  const announce = component.sent.find((frame) => frame.type === "session.announce" && frame.payload.native_session_id === "ses_title");
  assert.equal(announce?.payload.native_name, "");

  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_title", title: "native", directory: "/work/project" } } } });
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_title", title: "", directory: "/work/project" } } } });
  assert.deepEqual(component.observed.slice(-2).map((entry) => entry.nativeName), ["native", ""]);

  const beforeUnsafe = component.sent.length + component.observed.length;
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_missing", directory: "/work/project" } } } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_null", title: null, directory: "/work/project" } } } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_unsafe", title: "bad\ntitle", directory: "/work/project" } } } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_oversized", title: "x".repeat(1025), directory: "/work/project" } } } });
  assert.equal(component.sent.length + component.observed.length, beforeUnsafe, "missing, null, unsafe, and oversized title evidence must stay unavailable");
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_unsafe", title: "", directory: "/work/project" } } } });
  assert.equal(component.sent.at(-1).payload.native_name, "", "unsafe title must not mutate follower state");

  const noEvidenceOutput = { env: {} };
  const beforeShell = component.sent.length;
  await hooks["shell.env"]({ sessionID: "ses_shell", cwd: "/work/project" }, noEvidenceOutput);
  assert.equal(component.sent.length, beforeShell, "shell context without title evidence must not fabricate an announcement");
  assert.equal(noEvidenceOutput.env.AGENT_SESSIONS_NATIVE_SESSION_ID, "ses_shell");

  await hooks["shell.env"]({ sessionID: "ses_shell_empty", cwd: "/work/project", title: "" }, { env: {} });
  const shellAnnounce = component.sent.find((frame) => frame.type === "session.announce" && frame.payload.native_session_id === "ses_shell_empty");
  assert.equal(shellAnnounce?.payload.native_name, "");
});

test("OpenCode plugin binds delivery, rename, and parent tool to one exact native session", async () => {
  const component = new FakeComponent();
  const prompts = [];
  const updates = [];
  const client = { session: {
    async promptAsync(request) {
      prompts.push(request);
      return { data: {}, response: { status: 204 } };
    },
    async update(request) {
      updates.push(request);
      return { data: { id: request.path.id, title: request.body.title }, response: { status: 200 } };
    },
  } };
  const plugin = await loadPlugin(component);
  const hooks = await plugin({ client, directory: "/work/project" });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "before", directory: "/work/project" } } } });
  component.emit("session.bound", { binding_id: "foreign", native_session_id: "ses_foreign" });
  const nativeTool = hooks.tool.agent_sessions;
  await assert.rejects(nativeTool.execute({ operation: "peers.list", arguments: {} }, { sessionID: "ses_foreign", messageID: "msg_forged" }), /exact bound/u);
  assert.equal(component.calls.length, 0);

  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_exact" });
  component.emit("delivery.present", {
    delivery_id: "delivery-one", mode: "idle-wake",
    body: { version: 1, type: "delivery", message_id: "frame-one", content: "hello exact", source: { name: "peer" } },
  });
  await tick();
  assert.equal(prompts.length, 1);
  assert.equal(prompts[0].path.id, "ses_exact");
  assert.match(prompts[0].body.parts[0].text, /hello exact/u);
  assert.equal(component.sent.some((frame) => frame.type === "delivery.accept" && frame.payload.native_session_id === "ses_exact"), true);

  const renamed = await component.renameSession({ nativeSessionID: "ses_exact", requestedName: "managed title" });
  assert.deepEqual(renamed, { nativeName: "managed title", productEventSeq: renamed.productEventSeq });
  assert.equal(updates.length, 1);
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_exact", title: "external title", directory: "/work/project" } } } });
  assert.equal(component.observed.at(-1).nativeName, "external title");

  const result = await nativeTool.execute({ operation: "peers.list", arguments: { __agent_sessions_native_session_id: "forged" } }, {
    sessionID: "ses_exact", messageID: "msg_tool", abort: new AbortController().signal,
  });
  assert.equal(JSON.parse(result).ok, true);
  assert.equal(component.calls[0].argumentsValue.__agent_sessions_native_session_id, "ses_exact");

  await hooks.event({ event: { type: "message.updated", properties: { info: { id: "msg_event", sessionID: "ses_exact", role: "assistant" } } } });
  await hooks.event({ event: { type: "permission.asked", properties: { id: "per_event", sessionID: "ses_exact" } } });
  const turnEvents = component.sent.filter((frame) => frame.type === "turn.event").slice(-2);
  assert.deepEqual(turnEvents.map((frame) => frame.payload.native_session_id), ["ses_exact", "ses_exact"]);
  assert.equal(turnEvents.some((frame) => frame.payload.native_session_id === "msg_event" || frame.payload.native_session_id === "per_event"), false);
});

test("OpenCode plugin does not acknowledge an SDK-native prompt rejection", async () => {
  const component = new FakeComponent();
  const plugin = await loadPlugin(component);
  const hooks = await plugin({
    directory: "/work/project",
    client: { session: { promptAsync: async () => ({ error: { message: "rejected" }, response: { status: 409 } }), update: async () => ({}) } },
  });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_exact" });
  component.emit("delivery.present", { delivery_id: "delivery-reject", mode: "idle-wake", body: { content: "body" } });
  await tick();
  assert.equal(component.sent.some((frame) => frame.type === "delivery.accept"), false);
  assert.equal(component.sent.some((frame) => frame.type === "delivery.reject"), true);
});

test("OpenCode plugin rejects missing 204 evidence and unknown delivery modes", async () => {
  const component = new FakeComponent();
  let prompts = 0;
  const plugin = await loadPlugin(component);
  await plugin({
    directory: "/work/project",
    client: { session: {
      promptAsync: async () => { prompts += 1; return {}; },
      update: async () => ({ data: {}, response: { status: 200 } }),
    } },
  });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_exact" });
  component.emit("delivery.present", { delivery_id: "missing-status", mode: "idle-wake", body: { content: "body" } });
  component.emit("delivery.present", { delivery_id: "unknown-mode", mode: "invented", body: { content: "body" } });
  await tick();
  assert.equal(prompts, 1);
  assert.equal(component.sent.some((frame) => frame.type === "delivery.accept"), false);
  assert.equal(component.sent.filter((frame) => frame.type === "delivery.reject").length, 2);
});

test("OpenCode delivery aborts a hung native submit and never accepts its late 204", async () => {
  const component = new FakeComponent();
  let lateResolve;
  let requestSignal;
  const plugin = await loadPlugin(component, 20);
  await plugin({
    directory: "/work/project",
    client: { session: {
      promptAsync: async (_request, options) => {
        requestSignal = options.signal;
        return new Promise((resolve) => { lateResolve = resolve; });
      },
      update: async () => ({}),
    } },
  });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_exact" });
  component.emit("delivery.present", { delivery_id: "hung-submit", mode: "idle-wake", body: { content: "possibly accepted" } });
  await delay(60);
  const rejection = component.sent.find((frame) => frame.type === "delivery.reject" && frame.id === "hung-submit");
  assert.equal(rejection?.payload.category, "ambiguous-session");
  assert.equal(requestSignal?.aborted, true);
  lateResolve({ data: {}, response: { status: 204 } });
  await tick();
  assert.equal(component.sent.some((frame) => frame.type === "delivery.accept" && frame.id === "hung-submit"), false);
});

test("OpenCode rename serializes one writer and correlates early and late native events", async () => {
  const component = new FakeComponent();
  const updates = [];
  const resolvers = [];
  const plugin = await loadPlugin(component);
  const hooks = await plugin({
    directory: "/work/project",
    client: { session: {
      promptAsync: async () => ({ data: {}, response: { status: 204 } }),
      update: async (request, options) => {
        updates.push({ request, signal: options.signal });
        return new Promise((resolve) => resolvers.push(resolve));
      },
    } },
  });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_exact", title: "before", directory: "/work/project" } } } });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_exact" });

  const cancelledBeforeWrite = new AbortController();
  cancelledBeforeWrite.abort();
  await assert.rejects(
    component.renameSession({ nativeSessionID: "ses_exact", requestedName: "must-not-write", signal: cancelledBeforeWrite.signal }),
    (error) => error?.category === "timed-out",
  );
  assert.equal(updates.length, 0);

  const firstSignal = new AbortController();
  const first = component.renameSession({ nativeSessionID: "ses_exact", requestedName: "managed", signal: firstSignal.signal });
  await tick();
  assert.equal(updates.length, 1);
  await assert.rejects(
    component.renameSession({ nativeSessionID: "ses_exact", requestedName: "second", signal: new AbortController().signal }),
    /already in progress/u,
  );
  assert.equal(updates.length, 1);
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_exact", title: "managed", directory: "/work/project" } } } });
  assert.equal(component.observed.length, 0);
  resolvers.shift()({ data: { id: "ses_exact", title: "managed" }, response: { status: 200 } });
  assert.equal((await first).nativeName, "managed");
  assert.equal(component.observed.length, 0);

  const second = component.renameSession({ nativeSessionID: "ses_exact", requestedName: "managed-again", signal: new AbortController().signal });
  await tick();
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_exact", title: "", directory: "/work/project" } } } });
  assert.equal(component.observed.at(-1).nativeName, "", "empty native title must conflict with a pending nonempty rename");
  resolvers.shift()({ data: { id: "ses_exact", title: "managed-again" }, response: { status: 200 } });
  await assert.rejects(second, (error) => error?.category === "ambiguous-session");

  const rejected = component.renameSession({ nativeSessionID: "ses_exact", requestedName: "native-but-rejected", signal: new AbortController().signal });
  await tick();
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_exact", title: "native-but-rejected", directory: "/work/project" } } } });
  assert.equal(component.observed.some((entry) => entry.nativeName === "native-but-rejected"), false);
  resolvers.shift()({ error: { message: "rejected" }, response: { status: 409 } });
  await assert.rejects(rejected, (error) => error?.category === "native-rejected");
  await delay(5);
  assert.equal(component.observed.some((entry) => entry.nativeName === "native-but-rejected"), true);

  const aborted = new AbortController();
  const uncertain = component.renameSession({ nativeSessionID: "ses_exact", requestedName: "late-native", signal: aborted.signal });
  await tick();
  aborted.abort();
  await assert.rejects(uncertain, (error) => error?.category === "ambiguous-session");
  assert.equal(updates.at(-1).signal.aborted, true);
  resolvers.shift()({ data: { id: "ses_exact", title: "late-native" }, response: { status: 200 } });
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_exact", title: "late-native", directory: "/work/project" } } } });
  assert.equal(component.observed.some((entry) => entry.nativeName === "late-native"), true);
  assert.equal(updates.length, 4);
});
