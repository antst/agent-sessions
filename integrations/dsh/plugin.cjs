"use strict";

const { createLiveSessionClient, renderDelivery } = require("./shared/live-session.cjs");

const name = "agent-sessions-native";
const inject = ["appReady", "appExit", "sessionController", "sessions", "tools", "sessionTitle", "permissionPresets"];
const LIVE_OPERATIONS = Object.freeze([
  "peers.list", "message.send", "lane.start", "lane.run", "lane.resume",
  "lane.wait", "lane.status", "lane.steer", "lane.interrupt", "lane.archive",
]);

function cleanError(error) {
  return String(error?.message ?? error ?? "DSH native operation failed").replace(/[\0\r\n]/gu, " ").slice(0, 4096);
}

function textOf(message) {
  if (!message || !Array.isArray(message.content)) return "";
  return message.content.filter((block) => block?.type === "text" && typeof block.text === "string")
    .map((block) => block.text).join("");
}

function titleOf(ctx, agent) {
  const title = ctx.sessionTitle.get(agent.session)?.title;
  return typeof title === "string" ? title : "";
}

function modelOf(agent) {
  const provider = agent?.options?.provider;
  const model = agent?.options?.model;
  return typeof provider === "string" && provider && typeof model === "string" && model ? `${provider}/${model}` : "";
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

function createRuntime(ctx, createUserMessage, agent) {
  const inflight = new Map();
  const byInput = new Map();
  let openTurn = null;

  function finish(record, terminal) {
    if (record.terminal) return;
    record.terminal = terminal;
    record.resolve?.(terminal);
  }

  function removedBy(event) {
    const count = event.data.removedCount ?? 0;
    if (count === 0) return [];
    const current = event.data.target === "next-step" ? agent.inbox.nextStep : agent.inbox.nextTurn;
    return current.slice(event.data.start, event.data.start + count);
  }

  const removeEvents = ctx.on("session/event", (session, event) => {
    if (session !== agent.session) return;
    if (event.type === "agent/inbox/spliced") {
      if (event.data.outcome === "canceled") {
        for (const message of removedBy(event)) {
          const record = inflight.get(message.id);
          if (record) finish(record, { outcome: "failed", result: "", reason: { type: "canceled", outcome: event.data.outcome } });
        }
      }
      for (const message of event.data.inserted) {
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
        finish(record, { outcome: "failed", result: "", reason: { type: "protocol", detail: "DSH consumed input outside a turn" } });
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

  function newMessage(body) {
    return createUserMessage({
      content: [{ type: "text", text: body }],
      source: { kind: "plugin", plugin: name, form: "relay" },
    });
  }

  function submit(inputID, body, mode, tracked = true) {
    if (tracked && byInput.has(inputID)) throw new Error(`duplicate DSH lane input ${inputID}`);
    const message = newMessage(body);
    const record = { inputID, accepted: false, consumed: false, turn: null, result: "", terminal: null, resolve: null };
    inflight.set(message.id, record);
    if (tracked) byInput.set(inputID, message.id);
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
    if (!tracked) inflight.delete(message.id);
    return message.id;
  }

  async function wait(nativeMessageID) {
    const record = inflight.get(nativeMessageID);
    if (!record) throw new Error(`unknown DSH native message ${nativeMessageID}`);
    if (!record.terminal) {
      if (record.resolve) throw new Error(`DSH native message ${nativeMessageID} already has a waiter`);
      record.terminal = await new Promise((resolve) => { record.resolve = resolve; });
    }
    inflight.delete(nativeMessageID);
    byInput.delete(record.inputID);
    if (record.terminal.error) throw record.terminal.error;
    return record.terminal;
  }

  function deliver(payload) {
    submit(payload.messageID, renderDelivery(payload), "steer", false);
    return {};
  }

  function close() {
    removeEvents();
    for (const record of inflight.values()) finish(record, { outcome: "failed", result: "", reason: { type: "disposed" } });
  }

  return { byInput, close, deliver, submit, wait };
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

async function admit(ctx, createUserMessage) {
  const sessionID = process.env.AGENT_SESSIONS_SESSION_ID;
  const cwd = process.cwd();
  const resume = process.env.AGENT_SESSIONS_DSH_RESUME === "1";
  if (typeof sessionID !== "string" || !sessionID) throw new Error("DSH native lane requires its exact session id");
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

  if (!resume && process.env.AGENT_SESSIONS_SESSION_NAME) {
    await ctx.sessionController.rename({ sessionId: sessionID, title: process.env.AGENT_SESSIONS_SESSION_NAME });
    await ctx.sessions.flush(agent.session);
  }
  let selectedModel = modelOf(agent);
  const provider = process.env.AGENT_SESSIONS_DSH_MODEL_PROVIDER;
  const model = process.env.AGENT_SESSIONS_DSH_MODEL_ID;
  if (provider || model) {
    if (!provider || !model) throw new Error("DSH model selection requires provider and model");
    const selection = await ctx.sessionController.selectModel({
      sessionId: sessionID, provider, model,
      ...(process.env.AGENT_SESSIONS_DSH_REASONING_EFFORT ? { reasoningEffort: process.env.AGENT_SESSIONS_DSH_REASONING_EFFORT } : {}),
    });
    selectedModel = `${selection.selected.provider}/${selection.selected.model}`;
  }
  const preset = process.env.AGENT_SESSIONS_DSH_PERMISSION_PRESET;
  if (!ctx.permissionPresets.names.includes(preset)) throw new Error(`DSH permission preset ${String(preset)} is unavailable`);
  ctx.permissionPresets.set(agent.session, preset);
  await ctx.sessions.flush(agent.session);

  const runtime = createRuntime(ctx, createUserMessage, agent);
  const client = createLiveSessionClient({ env: process.env });
  client.handleLaneRequests(async ({ nativeSessionID, method, params }) => {
    if (nativeSessionID !== sessionID) throw new Error("DSH lane request addressed a different session");
    if (method === "lane.turn.start") {
      const nativeMessageID = runtime.submit(params.input_id, params.body, params.mode, params.mode === "followup");
      return { native_message_id: nativeMessageID };
    }
    if (method === "lane.turn.wait") return runtime.wait(params.native_message_id);
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
  });
  client.on("message", (payload) => {
    try {
      runtime.deliver(payload);
      client.acceptMessage(payload.messageID, {});
    } catch (error) {
      client.rejectMessage(payload.messageID, error);
    }
  });
  client.on("diagnostic", (message) => process.stderr.write(`agent-sessions: ${cleanError(message)}\n`));
  let toolSequence = 0;
  ctx.tools.register((await import("@deepseek-ai/dsh-tools")).defineTool({
    name: "agent_sessions",
    description: "Use one exact Agent Sessions operation: peers.list or message.send; lane.start, lane.run, lane.resume, lane.wait, lane.status, lane.steer, lane.interrupt, or lane.archive.",
    parameters: {
      action: { type: "string", enum: LIVE_OPERATIONS, required: true, description: "Exact Agent Sessions operation." },
      arguments: { type: "object", additionalProperties: true, description: "Arguments in the exact shape for the selected operation." },
    },
    output: {
      schema: { type: "object", additionalProperties: true, properties: {} },
      render: (_arguments, result) => [{ type: "text", text: JSON.stringify(result) }],
    },
    async execute(argumentsValue, execution) {
      if (execution?.agent !== agent) throw new Error("agent_sessions requires the exact managed DSH Agent");
      toolSequence += 1;
      return client.callTool(sessionID, `tool-${toolSequence}`, argumentsValue.action, argumentsValue.arguments || {});
    },
  }));
  if (!client.report(sessionID, titleOf(ctx, agent), { ...(selectedModel ? { model: selectedModel } : {}), cwd }, { lane: true })) {
    throw new Error("DSH native lane report is invalid");
  }
  await client.start();
  ctx.on("session/event", (session, event) => {
    if (session === agent.session && event.type === "session/title") client.updateName(sessionID, titleOf(ctx, agent));
  }, { global: true });
  ctx.effect(() => () => {
    runtime.close();
    void client.stop();
  }, "agent-sessions-native.lifecycle");
}

function apply(ctx) {
  let removeReady = () => {};
  const ready = ctx.get("appReady");
  if (!ready) throw new Error("agent-sessions-native requires appReady");
  removeReady = ready.onReady(() => {
    void (async () => {
      const inspectID = process.env.AGENT_SESSIONS_DSH_INSPECT_SESSION_ID;
      if (inspectID) return inspectSession(ctx, inspectID);
      const { createUserMessage } = await import("@deepseek-ai/dsh-llm");
      return admit(ctx, createUserMessage);
    })().catch((error) => {
      process.stderr.write(`agent-sessions: ${cleanError(error)}\n`);
      ctx.appExit(1);
    });
  });
  ctx.effect(() => () => removeReady(), "agent-sessions-native.ready");
}

module.exports = { apply, createRuntime, inject, inspectSession, name, resultReason, textOf };
