"use strict";

const path = require("node:path");
const { createLiveSessionClient, readConfiguration } = require("../shared/live-session.js");
const { validateSocket } = require("./mcp-env.cjs");

const name = "agent-sessions";
const version = "0.1.2-alpha.3";
const inject = ["agents", "tools", "sessionTitle", "sandboxPolicy"];
const LANE_POLICY_ENV = "AGENT_SESSIONS_DSH_LANE_POLICY";

function readLanePolicy(environment) {
  const value = environment[LANE_POLICY_ENV];
  if (!value) return null;
  if (value === "workspace-write:ask") return { sandbox: "workspace-write", approval: "ask" };
  if (value === "danger-full-access:never") return { sandbox: "danger-full-access", approval: "never" };
  throw new Error("unsupported exact Agent Sessions DSH lane policy");
}

function applyLanePolicy(ctx, policy, native) {
  if (!ctx?.agents || typeof ctx.sandboxPolicy?.overrideOf !== "function" ||
      typeof native?.setSandboxMode !== "function" || typeof native?.setApprovalPolicy !== "function" ||
      typeof native?.effectiveApprovalPolicy !== "function") {
    throw new Error("DSH lane policy enforcement requires supported sandbox and approval services");
  }
  ctx.on("agent/created", ({ agent }) => {
    if (!agent?.session || agent.id !== agent.session.header?.id) throw new Error("DSH lane policy received an incomplete exact session");
    native.setSandboxMode(agent.session, policy.sandbox);
    native.setApprovalPolicy(agent.session, policy.approval);
    if (ctx.sandboxPolicy.overrideOf(agent.session) !== policy.sandbox ||
        native.effectiveApprovalPolicy(agent.session.events) !== policy.approval) {
      throw new Error("DSH lane policy failed live exact-session verification");
    }
  });
  return { active: true, lanePolicy: policy };
}

function textOf(body) {
  if (typeof body === "string") return body;
  if (!body || typeof body !== "object") return "";
  if (typeof body.text === "string") return body.text;
  if (typeof body.message === "string") return body.message;
  if (Array.isArray(body.content)) {
    return body.content.filter((block) => block && block.type === "text" && typeof block.text === "string")
      .map((block) => block.text).join("");
  }
  return "";
}

function createCordisPlugin(options) {
  if (!options || !options.client || typeof options.defineTool !== "function" || typeof options.createUserMessage !== "function") {
    throw new Error("DSH Cordis plugin requires live session client, defineTool, and createUserMessage");
  }
  const client = options.client;
  const defineTool = options.defineTool;
  const createUserMessage = options.createUserMessage;
  let managedAgent;
  let selected = false;
  let observedTitle = "";
  let operationSequence = 0;
  const nextOperation = (prefix) => prefix + "-" + (++operationSequence);

  function exactAgent(agent) { return selected && managedAgent === agent; }

  function nativeFacts(ctx, agent) {
    if (!exactAgent(agent) || typeof agent.id !== "string" || !agent.id || !agent.session || agent.session.header?.id !== agent.id) return null;
    const cwd = agent.session.header?.cwd;
    const title = ctx.sessionTitle.get(agent.session)?.title;
    if (typeof cwd !== "string" || !path.isAbsolute(cwd) || path.normalize(cwd) !== cwd || typeof title !== "string" || !title) return null;
    return { cwd, title };
  }

  function announce(ctx, agent) {
    const facts = nativeFacts(ctx, agent);
    if (!facts) return false;
    const sent = client.report(agent.id, facts.title);
    if (sent) observedTitle = facts.title;
    return sent;
  }

  async function deliver(ctx, payload) {
    const agent = ctx.agents.get(payload.nativeSessionID);
    if (!exactAgent(agent)) throw new Error("unknown live DSH session");
    const text = textOf(payload.body);
    if (!text) throw new Error("empty Agent Sessions delivery");
    const input = createUserMessage({
      content: [{ type: "text", text }],
      source: { kind: "plugin", plugin: name },
    });
    if (!input || typeof input.id !== "string" || !input.id) throw new Error("DSH did not allocate a native message identity");
    if (agent.status === "running" || agent.status === "busy") {
      agent.steer(input);
    } else {
      agent.followup(input);
    }
    client.acceptMessage(payload.messageID, { native_message_id: input.id });
  }

  function registerParentTool(ctx) {
    ctx.tools.register(defineTool({
      name: "agent_sessions",
      description: "List or message Agent Sessions peers and control exact managed child lanes.",
      parameters: {
        action: { type: "string", required: true, description: "Registered Agent Sessions operation." },
        arguments: { type: "object", additionalProperties: true, description: "Bounded operation arguments." },
      },
      output: {
        schema: { type: "object", additionalProperties: true, properties: {} },
        render: (_arguments, result) => [{ type: "text", text: JSON.stringify(result) }],
      },
      async execute(argumentsValue, execution) {
        if (!exactAgent(execution?.agent)) throw new Error("agent_sessions requires the exact managed native exec.agent identity");
        const callID = nextOperation("tool");
        if (execution.signal?.aborted) throw new Error("Agent Sessions operation was cancelled before dispatch");
        return client.callTool(execution.agent.id, callID, argumentsValue.action, argumentsValue.arguments || {});
      },
    }));
  }

  function apply(ctx) {
    if (!ctx?.agents || !ctx?.tools || typeof ctx.sessionTitle?.get !== "function") throw new Error("DSH Cordis plugin requires ctx.agents, ctx.tools, and ctx.sessionTitle");
    const existing = ctx.agents.list();
    if (!Array.isArray(existing) || existing.length !== 0) {
      throw new Error("DSH managed profile must start before any native session exists");
    }
    registerParentTool(ctx);
    client.on("message", (payload) => {
      Promise.resolve(deliver(ctx, payload)).catch((error) => {
        client.rejectMessage(payload.messageID, String(error?.message || error).slice(0, 512));
      });
    });
    ctx.on("agent/created", ({ agent }) => {
      if (selected) throw new Error("DSH managed profile rejects sibling native sessions");
      const cwd = agent?.session?.header?.cwd;
      if (!agent || typeof agent.id !== "string" || !agent.id || !agent.session || agent.session.header?.id !== agent.id ||
          typeof cwd !== "string" || !path.isAbsolute(cwd) || path.normalize(cwd) !== cwd) {
        throw new Error("DSH managed profile received incomplete native session identity");
      }
      managedAgent = agent;
      selected = true;
      announce(ctx, agent);
    });
    ctx.on("session/event", (session, event) => {
      if (!managedAgent || session !== managedAgent.session) return;
      if (event?.type === "session/title") {
        const title = ctx.sessionTitle.get(managedAgent.session)?.title;
        if (typeof event.seq !== "number" || !Number.isSafeInteger(event.seq) || event.seq < 0 || typeof title !== "string" || !title) return;
        if (!client.sessions.has(managedAgent.id)) {
          announce(ctx, managedAgent);
        } else if (title !== observedTitle) {
          if (client.updateName(managedAgent.id, title)) observedTitle = title;
        }
        return;
      }
    });
    ctx.on("agent/disposed", ({ agent }) => {
      if (exactAgent(agent)) client.closeSession(agent.id);
    });
    const started = client.start();
    ctx.effect?.(() => () => client.stop(), "Agent Sessions live session");
    return started;
  }

  return { apply, announce, deliver };
}

function applyWithEnvironment(ctx, environment, loadNative = async () => {
  const [{ defineTool }, { createUserMessage }] = await Promise.all([
    import("@deepseek-ai/dsh-tools"),
    import("@deepseek-ai/dsh-llm"),
  ]);
  return { defineTool, createUserMessage };
}, loadLanePolicy = () => {
  // The pinned DSH runtime supports synchronous require() of these ESM
  // packages. Keeping the load inside the lane-only branch preserves ambient
  // profile inertness while registering the policy hook before any Agent can
  // be published.
  const { setSandboxMode } = require("@deepseek-ai/dsh-sandbox-policy");
  const { effectiveApprovalPolicy, setApprovalPolicy } = require("@deepseek-ai/dsh-user-approval");
  return { effectiveApprovalPolicy, setApprovalPolicy, setSandboxMode };
}) {
  const gate = readConfiguration(environment);
  const lanePolicy = readLanePolicy(environment);
  if (gate.active && lanePolicy) throw new Error("DSH process cannot be both a live peer and an ACP lane");
  if (lanePolicy) {
    const native = loadLanePolicy();
    return native && typeof native.then === "function" ? native.then((loaded) => applyLanePolicy(ctx, lanePolicy, loaded)) : applyLanePolicy(ctx, lanePolicy, native);
  }
  if (!gate.active) return { active: false, reason: gate.reason };
  validateSocket(gate.socketPath, environment);
  const client = createLiveSessionClient({ env: environment });
  // Load DSH's ESM-only native helpers only for a managed live connection; an
  // ambient profile remains entirely inert.
  return Promise.resolve(loadNative()).then(({ defineTool, createUserMessage }) =>
    createCordisPlugin({ client, defineTool, createUserMessage }).apply(ctx));
}

function apply(ctx) { return applyWithEnvironment(ctx, process.env); }

module.exports = { apply, applyLanePolicy, applyWithEnvironment, createCordisPlugin, inject, name, readLanePolicy, textOf, version };
