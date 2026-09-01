import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";
import test from "node:test";
import componentProtocolModule from "../shared/component/protocol.js";

const { validNativeTitleObservation } = componentProtocolModule;

class FakeComponent extends EventEmitter {
  constructor() {
    super();
    this.bindingID = "binding-kilo";
    this.sent = [];
    this.calls = [];
    this.observed = [];
  }
  async start() { return { active: true, bindingID: this.bindingID }; }
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
  enum: (values) => ({ values }), any: () => ({}), record: () => ({ default() { return this; } }),
};

async function loadPlugin(component) {
  globalThis.__agentSessionsTestTool = fakeTool;
  globalThis.__agentSessionsTestValidNativeTitleObservation = validNativeTitleObservation;
  globalThis.__agentSessionsTestComponentFactory = (options = {}) => {
    component.renameSession = options.renameSession;
    return component;
  };
  let source = await readFile(new URL("./agent-sessions.mjs", import.meta.url), "utf8");
  source = source
    .replace('import { tool } from "@kilocode/plugin";', "const tool = globalThis.__agentSessionsTestTool;")
    .replace('import componentModule from "../shared/component/client.js";', "")
    .replace('const { createComponentClient } = componentModule;', "const createComponentClient = globalThis.__agentSessionsTestComponentFactory;");
  source = source
    .replace('import componentProtocolModule from "../shared/component/protocol.js";', "")
    .replace('const { validNativeTitleObservation } = componentProtocolModule;', "const validNativeTitleObservation = globalThis.__agentSessionsTestValidNativeTitleObservation;");
  return (await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}#${Date.now()}-${Math.random()}`)).default;
}

const tick = () => new Promise((resolve) => setImmediate(resolve));
const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

test("Kilo projects genuine empty titles, clear events, and no shell fabrication", async () => {
  const component = new FakeComponent();
  const plugin = await loadPlugin(component);
  const hooks = await plugin({ client: {}, directory: "/work/kilo" });

  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_title", title: "", directory: "/work/kilo" } } } });
  const announce = component.sent.find((frame) => frame.type === "session.announce" && frame.payload.native_session_id === "ses_title");
  assert.equal(announce?.payload.native_name, "");

  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_title", title: "native", directory: "/work/kilo" } } } });
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_title", title: "", directory: "/work/kilo" } } } });
  assert.deepEqual(component.observed.slice(-2).map((entry) => entry.nativeName), ["native", ""]);

  const beforeUnsafe = component.sent.length + component.observed.length;
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_missing", directory: "/work/kilo" } } } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_null", title: null, directory: "/work/kilo" } } } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_unsafe", title: "bad\ntitle", directory: "/work/kilo" } } } });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_oversized", title: "x".repeat(1025), directory: "/work/kilo" } } } });
  assert.equal(component.sent.length + component.observed.length, beforeUnsafe, "missing, null, unsafe, and oversized title evidence must stay unavailable");
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_unsafe", title: "", directory: "/work/kilo" } } } });
  assert.equal(component.sent.at(-1).payload.native_name, "", "unsafe title must not mutate follower state");

  const noEvidenceOutput = { env: {} };
  const beforeShell = component.sent.length;
  await hooks["shell.env"]({ sessionID: "ses_shell", cwd: "/work/kilo" }, noEvidenceOutput);
  assert.equal(component.sent.length, beforeShell, "shell context without title evidence must not fabricate an announcement");
  assert.equal(noEvidenceOutput.env.AGENT_SESSIONS_NATIVE_SESSION_ID, "ses_shell");

  await hooks["shell.env"]({ sessionID: "ses_shell_empty", cwd: "/work/kilo", title: "" }, { env: {} });
  const shellAnnounce = component.sent.find((frame) => frame.type === "session.announce" && frame.payload.native_session_id === "ses_shell_empty");
  assert.equal(shellAnnounce?.payload.native_name, "");
});

test("Kilo plugin uses only the bound full-TUI routes and exact native acceptance", async () => {
  const component = new FakeComponent();
  const tuiCalls = [];
  const messages = [];
  const client = {
    session: {
      async messages() { return { data: [...messages], response: { status: 200 } }; },
      async update(request) { return { data: { id: request.path.id, title: request.body.title }, response: { status: 200 } }; },
    },
    tui: {
      async clearPrompt(request) { tuiCalls.push(["clear", request]); return { data: true, response: { status: 200 } }; },
      async appendPrompt(request) { tuiCalls.push(["append", request]); return { data: true, response: { status: 200 } }; },
      async submitPrompt(request) {
        tuiCalls.push(["submit", request]);
        const text = tuiCalls.find((entry) => entry[0] === "append")[1].body.text;
        messages.push({ info: { id: "msg_kilo_exact", sessionID: "ses_kilo", role: "user" }, parts: [{ type: "text", text }] });
        return { data: true, response: { status: 200 } };
      },
    },
  };
  const plugin = await loadPlugin(component);
  const hooks = await plugin({ client, directory: "/work/kilo" });
  component.emit("session.bound", { binding_id: "foreign", native_session_id: "ses_foreign" });
  await assert.rejects(hooks.tool.agent_sessions.execute({ operation: "peers.list", arguments: {} }, { sessionID: "ses_foreign", messageID: "msg_forged" }), /exact bound/u);
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_kilo" });
  component.emit("delivery.present", {
    delivery_id: "delivery-kilo", mode: "busy-steer",
    body: { version: 1, type: "delivery", message_id: "frame-kilo", content: "route only here", source: { name: "peer" } },
  });
  await tick();
  assert.deepEqual(tuiCalls.map((entry) => entry[0]), ["clear", "append", "submit"]);
  const acceptance = component.sent.find((frame) => frame.type === "delivery.accept");
  assert.equal(acceptance.payload.native_session_id, "ses_kilo");
  assert.equal(acceptance.payload.native_message_id, "msg_kilo_exact");

  const renamed = await component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "Kilo title" });
  assert.equal(renamed.nativeName, "Kilo title");
  const result = await hooks.tool.agent_sessions.execute({ operation: "lane.status", arguments: { __agent_sessions_native_session_id: "forged" } }, {
    sessionID: "ses_kilo", messageID: "msg_tool", abort: new AbortController().signal,
  });
  assert.equal(JSON.parse(result).ok, true);
  assert.equal(component.calls[0].argumentsValue.__agent_sessions_native_session_id, "ses_kilo");

  await hooks.event({ event: { type: "message.updated", properties: { info: { id: "msg_event", sessionID: "ses_kilo", role: "assistant" } } } });
  await hooks.event({ event: { type: "permission.asked", properties: { id: "per_event", sessionID: "ses_kilo" } } });
  const turnEvents = component.sent.filter((frame) => frame.type === "turn.event").slice(-2);
  assert.deepEqual(turnEvents.map((frame) => frame.payload.native_session_id), ["ses_kilo", "ses_kilo"]);
  assert.equal(turnEvents.some((frame) => frame.payload.native_session_id === "msg_event" || frame.payload.native_session_id === "per_event"), false);
});

test("Kilo plugin rejects unknown delivery mode and failed /tui acceptance", async () => {
  const component = new FakeComponent();
  const client = {
    session: { messages: async () => ({ data: [], response: { status: 200 } }), update: async () => ({}) },
    tui: {
      clearPrompt: async () => ({ error: {}, response: { status: 409 } }),
      appendPrompt: async () => ({ data: true, response: { status: 200 } }),
      submitPrompt: async () => ({ data: true, response: { status: 200 } }),
    },
  };
  const plugin = await loadPlugin(component);
  await plugin({ client, directory: "/work/kilo" });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_kilo" });
  component.emit("delivery.present", { delivery_id: "unknown", mode: "invented", body: { content: "x" } });
  component.emit("delivery.present", { delivery_id: "failed", mode: "idle-wake", body: { content: "x" } });
  await tick();
  assert.equal(component.sent.some((frame) => frame.type === "delivery.accept"), false);
  assert.equal(component.sent.filter((frame) => frame.type === "delivery.reject").length, 2);
});

test("Kilo plugin bounds post-submit confirmation and reports ambiguity", async () => {
  const component = new FakeComponent();
  let snapshots = 0;
  const client = {
    session: {
      async messages() {
        snapshots += 1;
        if (snapshots === 1) return { data: [], response: { status: 200 } };
        return {
          data: [{ info: { id: "msg_oversized", sessionID: "ses_kilo", role: "user" }, parts: [{ type: "text", text: "x".repeat(1024 * 1024 + 1) }] }],
          response: { status: 200 },
        };
      },
      update: async () => ({ data: {}, response: { status: 200 } }),
    },
    tui: {
      clearPrompt: async () => ({ data: true, response: { status: 200 } }),
      appendPrompt: async () => ({ data: true, response: { status: 200 } }),
      submitPrompt: async () => ({ data: true, response: { status: 200 } }),
    },
  };
  const plugin = await loadPlugin(component);
  await plugin({ client, directory: "/work/kilo" });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_kilo" });
  component.emit("delivery.present", { delivery_id: "uncertain", mode: "idle-wake", body: { content: "may have submitted" } });
  await tick();
  const rejection = component.sent.find((frame) => frame.type === "delivery.reject" && frame.id === "uncertain");
  assert.equal(rejection?.payload.category, "ambiguous-session");
  assert.equal(component.sent.some((frame) => frame.type === "delivery.accept"), false);
});

test("Kilo rename serializes one writer and correlates early and late native events", async () => {
  const component = new FakeComponent();
  const updates = [];
  const resolvers = [];
  const plugin = await loadPlugin(component);
  const hooks = await plugin({
    directory: "/work/kilo",
    client: {
      session: {
        messages: async () => ({ data: [], response: { status: 200 } }),
        update: async (request, options) => {
          updates.push({ request, signal: options.signal });
          return new Promise((resolve) => resolvers.push(resolve));
        },
      },
      tui: {
        clearPrompt: async () => ({ data: true, response: { status: 200 } }),
        appendPrompt: async () => ({ data: true, response: { status: 200 } }),
        submitPrompt: async () => ({ data: true, response: { status: 200 } }),
      },
    },
  });
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_kilo", title: "before", directory: "/work/kilo" } } } });
  component.emit("session.bound", { binding_id: component.bindingID, native_session_id: "ses_kilo" });

  const cancelledBeforeWrite = new AbortController();
  cancelledBeforeWrite.abort();
  await assert.rejects(
    component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "must-not-write", signal: cancelledBeforeWrite.signal }),
    (error) => error?.category === "timed-out",
  );
  assert.equal(updates.length, 0);

  const first = component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "managed", signal: new AbortController().signal });
  await tick();
  assert.equal(updates.length, 1);
  await assert.rejects(
    component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "second", signal: new AbortController().signal }),
    /already in progress/u,
  );
  assert.equal(updates.length, 1);
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_kilo", title: "managed", directory: "/work/kilo" } } } });
  assert.equal(component.observed.length, 0);
  resolvers.shift()({ data: { id: "ses_kilo", title: "managed" }, response: { status: 200 } });
  assert.equal((await first).nativeName, "managed");

  const second = component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "managed-again", signal: new AbortController().signal });
  await tick();
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_kilo", title: "", directory: "/work/kilo" } } } });
  assert.equal(component.observed.at(-1).nativeName, "", "empty native title must conflict with a pending nonempty rename");
  resolvers.shift()({ data: { id: "ses_kilo", title: "managed-again" }, response: { status: 200 } });
  await assert.rejects(second, (error) => error?.category === "ambiguous-session");

  const rejected = component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "native-but-rejected", signal: new AbortController().signal });
  await tick();
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_kilo", title: "native-but-rejected", directory: "/work/kilo" } } } });
  assert.equal(component.observed.some((entry) => entry.nativeName === "native-but-rejected"), false);
  resolvers.shift()({ error: { message: "rejected" }, response: { status: 409 } });
  await assert.rejects(rejected, (error) => error?.category === "native-rejected");
  await delay(5);
  assert.equal(component.observed.some((entry) => entry.nativeName === "native-but-rejected"), true);

  const aborted = new AbortController();
  const uncertain = component.renameSession({ nativeSessionID: "ses_kilo", requestedName: "late-native", signal: aborted.signal });
  await tick();
  aborted.abort();
  await assert.rejects(uncertain, (error) => error?.category === "ambiguous-session");
  assert.equal(updates.at(-1).signal.aborted, true);
  resolvers.shift()({ data: { id: "ses_kilo", title: "late-native" }, response: { status: 200 } });
  await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_kilo", title: "late-native", directory: "/work/kilo" } } } });
  assert.equal(component.observed.some((entry) => entry.nativeName === "late-native"), true);
  assert.equal(updates.length, 4);
});
