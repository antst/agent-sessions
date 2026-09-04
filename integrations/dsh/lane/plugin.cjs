"use strict";

const commsPackage = require("@agent-sessions/dsh-comms");

const name = "agent-sessions-lane";
const inject = [
  commsPackage.serviceName, "appReady", "appExit", "sessionController",
  "sessions", "sessionTitle", "permissionPresets",
];

function cleanError(error) {
  return String(error?.message ?? error ?? "DSH native lane operation failed").replace(/[\0\r\n]/gu, " ").slice(0, 4096);
}

function text(value) {
  return typeof value === "string" && value.trim() === value && value.length > 0 && !/[\0\r\n]/u.test(value);
}

function launchValue(environment, key) {
  const value = environment?.get(key)?.value;
  return typeof value === "string" ? value : "";
}

function textOf(message) {
  if (!message || !Array.isArray(message.content)) return "";
  return message.content.filter((block) => block?.type === "text" && typeof block.text === "string")
    .map((block) => block.text).join("");
}

function resultReason(reason) {
  if (reason?.kind === "completed") return { outcome: "completed", reason };
  if (reason?.kind === "aborted" || reason?.kind === "interrupted") return { outcome: "interrupted", reason };
  if (reason?.kind === "blocked" || reason?.kind === "error" || reason?.kind === "max-tokens") {
    return { outcome: "failed", reason };
  }
  const native = JSON.stringify(reason);
  throw new Error(`DSH returned unknown turn end reason ${native === undefined ? String(reason) : native}`);
}

function removedBy(agent, event) {
  const count = event.data.removedCount ?? 0;
  if (count === 0) return [];
  const current = event.data.target === "next-step" ? agent.inbox.nextStep : agent.inbox.nextTurn;
  return current.slice(event.data.start, event.data.start + count);
}

function createLaneRuntime(ctx, createUserMessage, agent) {
  const inflight = new Map();
  const byInput = new Map();
  let openTurn = null;

  function finish(record, terminal) {
    if (record.terminal) return;
    record.terminal = terminal;
    record.resolve?.(terminal);
  }

  const removeEvents = ctx.on("session/event", (session, event) => {
    if (session !== agent.session) return;
    if (event.type === "agent/inbox/spliced") {
      if (event.data.outcome === "canceled") {
        for (const message of removedBy(agent, event)) {
          const record = inflight.get(message.id);
          if (record) finish(record, { error: new Error("DSH canceled native input before consumption") });
        }
      }
      for (const message of event.data.inserted ?? []) {
        const record = inflight.get(message.id);
        if (record) record.accepted = true;
      }
      return;
    }
    if (event.type === "turn/start") {
      openTurn = event.data.turn;
      return;
    }
    if (event.type === "user/message") {
      const record = inflight.get(event.data.id);
      if (!record) return;
      if (!Number.isSafeInteger(openTurn)) {
        finish(record, { error: new Error("DSH consumed native input outside a turn") });
        return;
      }
      record.turn = openTurn;
      record.consumed = true;
      return;
    }
    if (event.type === "assistant/message") {
      for (const record of inflight.values()) {
        if (record.consumed && record.turn === event.data.turn && !record.terminal) record.result += textOf(event.data.message);
      }
      return;
    }
    if (event.type === "turn/end") {
      for (const record of inflight.values()) {
        if (record.consumed && record.turn === event.data.turn && !record.terminal) {
          try {
            finish(record, { ...resultReason(event.data.reason), result: record.result });
          } catch (error) {
            finish(record, { error });
          }
        }
      }
      if (openTurn === event.data.turn) openTurn = null;
    }
  }, { global: true });

  function submit(inputID, body, mode) {
    if (byInput.has(inputID)) throw new Error(`duplicate DSH lane input ${inputID}`);
    const message = createUserMessage({
      content: [{ type: "text", text: body }],
      source: { kind: "plugin", plugin: name, form: "relay" },
    });
    const record = { inputID, accepted: false, consumed: false, turn: null, result: "", terminal: null, resolve: null };
    inflight.set(message.id, record);
    byInput.set(inputID, message.id);
    try {
      agent[mode](message);
    } catch (error) {
      inflight.delete(message.id);
      byInput.delete(inputID);
      throw error;
    }
    if (!record.accepted) {
      inflight.delete(message.id);
      byInput.delete(inputID);
      throw new Error("DSH did not publish the native input receipt");
    }
    return message.id;
  }

  async function wait(nativeMessageID) {
    const record = inflight.get(nativeMessageID);
    if (!record) throw {
      code: -32001, message: "Unknown session or target", data: { target: nativeMessageID },
    };
    if (!record.terminal) {
      if (record.resolve) throw new Error(`DSH native message ${nativeMessageID} already has a waiter`);
      record.terminal = await new Promise((resolve) => { record.resolve = resolve; });
    }
    inflight.delete(nativeMessageID);
    byInput.delete(record.inputID);
    if (record.terminal.error) throw record.terminal.error;
    return record.terminal;
  }

  async function handle(method, params) {
    if (method === "lane.turn.start") {
      return { native_message_id: submit(params.input_id, params.body, params.mode) };
    }
    if (method === "lane.turn.wait") return wait(params.native_message_id);
    if (method === "lane.turn.interrupt") {
      agent.cancel({ kind: "user" }, { keepInbox: true });
      return {};
    }
    if (method === "lane.session.archive") {
      agent.cancel({ kind: "disposed" });
      void (async () => {
        await agent.whenIdle();
        await ctx.sessions.flush(agent.session);
        setImmediate(() => ctx.appExit(0));
      })();
      return {};
    }
    throw new Error(`unsupported DSH lane method ${method}`);
  }

  function close() {
    removeEvents();
    for (const record of inflight.values()) {
      finish(record, { error: new Error("DSH lane disposed before native turn completion") });
    }
  }

  return { byInput, close, handle, inflight, submit, wait };
}

async function inspectSession(ctx, sessionID) {
  const abort = new AbortController();
  const value = await ctx.sessionController.list({}, abort.signal);
  const row = value.items.find((item) => item.sessionId === sessionID);
  const result = row ? {
    found: true,
    session_id: row.sessionId,
    name: typeof row.projections?.values?.title === "string" ? row.projections.values.title : "",
    cwd: typeof row.cwd === "string" ? row.cwd : "",
    updated_at: row.updatedAt,
  } : { found: false };
  process.stdout.write(`${JSON.stringify(result)}\n`, () => ctx.appExit(0));
}

async function admit(ctx, createUserMessage, environment, registration) {
  const sessionID = launchValue(environment, "AGENT_SESSIONS_SESSION_ID");
  const cwd = launchValue(environment, "AGENT_SESSIONS_DSH_CWD");
  const resume = launchValue(environment, "AGENT_SESSIONS_DSH_RESUME") === "1";
  if (!text(sessionID)) throw new Error("DSH native lane requires its exact session id");
  if (!text(cwd)) throw new Error("DSH native lane requires its invocation cwd");
  let resolved;
  if (resume) {
    resolved = await ctx.sessionController.resolveAgent(sessionID);
  } else {
    const created = await ctx.sessionController.create({ cwd, sessionId: sessionID });
    if (created.sessionId !== sessionID) throw new Error("DSH changed the caller-supplied session id");
    resolved = await ctx.sessionController.resolveAgent(sessionID);
  }
  if ("error" in resolved) throw resolved.error;
  const agent = resolved.agent;
  if (agent.id !== sessionID || agent.session.id !== sessionID) throw new Error("DSH resolved a different native session");

  const sessionName = launchValue(environment, "AGENT_SESSIONS_SESSION_NAME");
  if (!resume && sessionName) {
    await ctx.sessionController.rename({ sessionId: sessionID, title: sessionName });
    await ctx.sessions.flush(agent.session);
  }
  const provider = launchValue(environment, "AGENT_SESSIONS_DSH_MODEL_PROVIDER");
  const model = launchValue(environment, "AGENT_SESSIONS_DSH_MODEL_ID");
  if (provider || model) {
    if (!provider || !model) throw new Error("DSH model selection requires provider and model");
    await ctx.sessionController.selectModel({
      sessionId: sessionID, provider, model,
      ...(launchValue(environment, "AGENT_SESSIONS_DSH_REASONING_EFFORT")
        ? { reasoningEffort: launchValue(environment, "AGENT_SESSIONS_DSH_REASONING_EFFORT") } : {}),
    });
  }
  const preset = launchValue(environment, "AGENT_SESSIONS_DSH_PERMISSION_PRESET");
  if (!ctx.permissionPresets.names.includes(preset)) throw new Error(`DSH permission preset ${String(preset)} is unavailable`);
  ctx.permissionPresets.set(agent.session, preset);
  await ctx.sessions.flush(agent.session);

  const runtime = createLaneRuntime(ctx, createUserMessage, agent);
  registration.present(agent);
  return runtime;
}

function apply(ctx) {
  const environment = ctx.get("launchEnvironment");
  const comms = ctx.get(commsPackage.serviceName);
  let registration = null;
  let runtime = null;
  let removeReady = () => {};
  const ready = ctx.get("appReady");
  if (!ready) throw new Error("agent-sessions-lane requires appReady");

  const inspectID = launchValue(environment, "AGENT_SESSIONS_DSH_INSPECT_SESSION_ID");
  if (!inspectID) {
    const sessionID = launchValue(environment, "AGENT_SESSIONS_SESSION_ID");
    registration = comms.registerExtension(sessionID, {
      capabilities: { lane: true }, defer: true,
      handle(method, params) {
        if (!runtime) throw new Error("DSH native lane is not ready");
        return runtime.handle(method, params);
      },
    });
  }

  removeReady = ready.onReady(() => {
    void (async () => {
      if (inspectID) return inspectSession(ctx, inspectID);
      const { createUserMessage } = await import("@deepseek-ai/dsh-llm");
      runtime = await admit(ctx, createUserMessage, environment, registration);
    })().catch((error) => {
      process.stderr.write(`agent-sessions: ${cleanError(error)}\n`);
      ctx.appExit(1);
    });
  });
  ctx.effect(() => () => {
    removeReady();
    runtime?.close();
    registration?.dispose();
  }, "agent-sessions-lane.lifecycle");
}

module.exports = {
  admit, apply, createLaneRuntime, inject, inspectSession, launchValue, name, resultReason, textOf,
};
