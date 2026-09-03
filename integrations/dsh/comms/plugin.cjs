"use strict";

const { createLiveSessionClient, renderDelivery } = require("./shared/live-session.cjs");

const name = "agent-sessions-comms";
const inject = ["agents", "tools"];
const serviceName = "agentSessionsComms";
const OPERATIONS = Object.freeze(["peers.list", "message.send"]);
const ENVIRONMENT_NAMES = Object.freeze([
  "AGENT_SESSIONS_PRESENCE_SOCKET", "AGENT_SESSIONS_STATE_ROOT",
  "XDG_STATE_HOME", "HOME",
]);

function cleanError(error) {
  return String(error?.message ?? error ?? "DSH communication operation failed").replace(/[\0\r\n]/gu, " ").slice(0, 4096);
}

function text(value) {
  return typeof value === "string" && value.trim() === value && value.length > 0 && !/[\0\r\n]/u.test(value);
}

function configuredGroups(value) {
  const groups = value?.groups ?? [];
  if (!Array.isArray(groups) || groups.some((group) => !text(group))) {
    throw new Error("agent-sessions DSH comms groups are invalid");
  }
  return [...groups];
}

function launchValue(environment, key) {
  const value = environment?.get(key)?.value;
  return typeof value === "string" ? value : "";
}

function launchClientEnvironment(environment) {
  const result = { AGENT_SESSIONS_PRODUCT: "dsh", AGENT_SESSIONS_GROUPS: "[]" };
  for (const key of ENVIRONMENT_NAMES) {
    const value = launchValue(environment, key);
    if (value) result[key] = value;
  }
  return result;
}

function launchIdentity(environment, agent, fallbackGroups) {
  const sessionID = String(agent?.session?.id ?? agent?.id ?? "");
  if (!text(sessionID)) throw new Error("DSH root omitted its native session id");
  if (launchValue(environment, "AGENT_SESSIONS_SESSION_ID") !== sessionID) {
    return { sessionID, name: "", groups: [...fallbackGroups] };
  }
  let groups;
  try {
    groups = JSON.parse(launchValue(environment, "AGENT_SESSIONS_GROUPS") || "[]");
  } catch {
    throw new Error("agent-sessions DSH launch groups are invalid");
  }
  if (!Array.isArray(groups) || groups.some((group) => !text(group))) {
    throw new Error("agent-sessions DSH launch groups are invalid");
  }
  return {
    sessionID,
    name: launchValue(environment, "AGENT_SESSIONS_SESSION_NAME"),
    groups: [...groups],
  };
}

function modelOf(agent) {
  const provider = agent?.options?.provider;
  const model = agent?.options?.model;
  return text(provider) && text(model) ? `${provider}/${model}` : "";
}

function sessionInfo(agent) {
  const cwd = agent?.session?.header?.cwd;
  if (!text(cwd)) throw new Error("DSH root omitted its native session cwd");
  const model = modelOf(agent);
  return { cwd, ...(model ? { model } : {}) };
}

function createCommsRuntime(ctx, createUserMessage, config = {}, options = {}) {
  const environment = ctx.get("launchEnvironment");
  const fallbackGroups = configuredGroups(config);
  const client = options.client ?? createLiveSessionClient({ env: launchClientEnvironment(environment) });
  const sessions = new Map();
  const receipts = new Map();
  const extensions = new Map();
  let toolSequence = 0;

  function root(agent) {
    return ctx.agents.roots().includes(agent);
  }

  function extensionFor(sessionID) {
    return extensions.get(sessionID) ?? null;
  }

  function present(agent) {
    if (!root(agent) || sessions.has(agent)) return false;
    const identity = launchIdentity(environment, agent, fallbackGroups);
    const extension = extensionFor(identity.sessionID);
    if (extension?.defer === true && extension.ready !== true) return false;
    const record = { agent, ...identity };
    sessions.set(agent, record);
    if (!client.report(identity.sessionID, identity.name, sessionInfo(agent), extension?.capabilities ?? {}, identity.groups)) {
      sessions.delete(agent);
      throw new Error("DSH communication presence report is invalid");
    }
    return true;
  }

  function forget(agent) {
    const record = sessions.get(agent);
    if (!record) return;
    sessions.delete(agent);
    client.closeSession(record.sessionID);
  }

  function registerExtension(sessionID, extension) {
    if (!text(sessionID) || extensions.has(sessionID) || !extension || typeof extension.handle !== "function") {
      throw new Error("DSH communication extension registration is invalid");
    }
    const record = {
      capabilities: { ...(extension.capabilities ?? {}) },
      defer: extension.defer === true,
      ready: extension.defer !== true,
      handle: extension.handle,
    };
    extensions.set(sessionID, record);
    return {
      present(agent) {
        record.ready = true;
        if (String(agent?.session?.id ?? "") !== sessionID) throw new Error("DSH lane extension resolved a different session");
        return present(agent);
      },
      dispose() {
        if (extensions.get(sessionID) === record) extensions.delete(sessionID);
      },
    };
  }

  const removeCreated = ctx.on("agent/created", ({ agent }) => { present(agent); });
  const removeDisposed = ctx.on("agent/disposed", ({ agent }) => { forget(agent); });
  const removeEvents = ctx.on("session/event", (session, event) => {
    const record = sessions.get(ctx.agents.get(session.id));
    if (event.type === "agent/inbox/spliced") {
      for (const message of event.data.inserted ?? []) {
        if (receipts.has(message.id)) receipts.set(message.id, true);
      }
    }
    if (!record || record.agent.session !== session || event.type !== "session/title") return;
    const title = typeof event.data?.title === "string" ? event.data.title : "";
    record.name = title;
    client.updateName(record.sessionID, title);
  }, { global: true });

  client.handleLaneRequests(({ nativeSessionID, method, params }) => {
    const extension = extensionFor(nativeSessionID);
    if (!extension?.ready) throw new Error("DSH session has no native lane capability");
    return extension.handle(method, params);
  });

  client.on("message", (payload) => {
    const record = [...sessions.values()].find((candidate) => candidate.sessionID === payload.nativeSessionID);
    if (!record) return client.rejectMessage(payload.messageID, "DSH delivery targets no live root");
    let message;
    try {
      message = createUserMessage({
        content: [{ type: "text", text: renderDelivery(payload) }],
        source: { kind: "plugin", plugin: name, form: "relay" },
      });
      receipts.set(message.id, false);
      // DSH rc.1 defines steer as a wake while idle and a next-step splice
      // while running, so one native operation covers both delivery states.
      record.agent.steer(message);
      if (receipts.get(message.id) !== true) throw new Error("DSH did not publish the native input receipt");
      client.acceptMessage(payload.messageID, { native_message_id: message.id });
    } catch (error) {
      client.rejectMessage(payload.messageID, error);
    } finally {
      if (message) receipts.delete(message.id);
    }
  });
  client.on("diagnostic", (message) => process.stderr.write(`agent-sessions: ${cleanError(message)}\n`));

  async function callTool(agent, operation, argumentsValue) {
    const record = sessions.get(agent);
    if (!record) throw new Error("agent_sessions requires an exact presented DSH root");
    if (!OPERATIONS.includes(operation)) throw new Error("unsupported Agent Sessions communication operation");
    toolSequence += 1;
    return client.callTool(record.sessionID, `dsh-tool-${toolSequence}`, operation, argumentsValue ?? {});
  }

  function close() {
    removeCreated();
    removeDisposed();
    removeEvents();
    for (const agent of [...sessions.keys()]) forget(agent);
    for (const extension of extensions.values()) extension.ready = false;
    extensions.clear();
    void client.stop();
  }

  return { callTool, client, close, present, registerExtension, sessions };
}

async function apply(ctx, config = {}) {
  const { createUserMessage } = await import("@deepseek-ai/dsh-llm");
  const { defineTool } = await import("@deepseek-ai/dsh-tools");
  const runtime = createCommsRuntime(ctx, createUserMessage, config);
  ctx.provide(serviceName, runtime);
  ctx.tools.register(defineTool({
    name: "agent_sessions",
    description: "Use one exact Agent Sessions communication operation: peers.list or message.send.",
    parameters: {
      action: { type: "string", enum: OPERATIONS, required: true, description: "Exact Agent Sessions operation." },
      arguments: { type: "object", additionalProperties: true, description: "Arguments in the exact shape for the selected operation." },
    },
    output: {
      schema: { type: "object", additionalProperties: true, properties: {} },
      render: (_arguments, result) => [{ type: "text", text: JSON.stringify(result) }],
    },
    execute(argumentsValue, execution) {
      return runtime.callTool(execution?.agent, argumentsValue.action, argumentsValue.arguments || {});
    },
  }));
  for (const agent of ctx.agents.roots()) runtime.present(agent);
  ctx.effect(() => () => runtime.close(), "agent-sessions-comms.lifecycle");
}

module.exports = {
  OPERATIONS, apply, configuredGroups, createCommsRuntime, inject, launchClientEnvironment,
  launchIdentity, name, serviceName, sessionInfo,
};
