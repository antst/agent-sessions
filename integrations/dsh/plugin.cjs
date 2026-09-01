"use strict";

const path = require("node:path");
const { createComponentClient, readConfiguration } = require("../shared/component/client.js");
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
    throw new Error("DSH Cordis plugin requires component client, defineTool, and createUserMessage");
  }
  const client = options.client;
  const defineTool = options.defineTool;
  const createUserMessage = options.createUserMessage;
  const sessionSequences = new Map();
  let managedAgent;
  let selected = false;
  let announced = false;
  let observedTitle = "";
  let operationSequence = 0;
  const nextOperation = (prefix) => prefix + "-" + (++operationSequence);
  const nextSessionSequence = (sessionID) => {
    const sequence = (sessionSequences.get(sessionID) || 0) + 1;
    sessionSequences.set(sessionID, sequence);
    return sequence;
  };

  function exactAgent(agent) { return selected && managedAgent === agent; }

  function nativeFacts(ctx, agent) {
    if (!exactAgent(agent) || typeof agent.id !== "string" || !agent.id || !agent.session || agent.session.header?.id !== agent.id) return null;
    const cwd = agent.session.header?.cwd;
    const title = ctx.sessionTitle.get(agent.session)?.title;
    if (typeof cwd !== "string" || !path.isAbsolute(cwd) || path.normalize(cwd) !== cwd || typeof title !== "string" || !title) return null;
    return { cwd, title };
  }

  function announce(ctx, agent) {
    if (!client.bindingID) return false;
    const facts = nativeFacts(ctx, agent);
    if (!facts) return false;
    const sequence = nextSessionSequence(agent.id);
    const sent = client.send("session.announce", nextOperation("announce"), {
      binding_id: client.bindingID,
      native_session_id: agent.id,
      cwd: facts.cwd,
      native_name: facts.title,
      product_event_seq: sequence,
    });
    if (sent) {
      announced = true;
      observedTitle = facts.title;
    }
    return sent;
  }

  function state(agent, status) {
    if (!client.bindingID || !announced || !exactAgent(agent)) return false;
    const normalized = status === "running" || status === "busy" ? "busy" : "idle";
    return client.send("session.state", nextOperation("state"), {
      native_session_id: agent.id,
      state: normalized,
      product_event_seq: nextSessionSequence(agent.id),
    });
  }

  function turnEvent(agent, kind, metadata = {}) {
    if (!client.bindingID || !announced || !exactAgent(agent)) return false;
    return client.send("turn.event", nextOperation("turn"), {
      native_session_id: agent.id,
      event_seq: nextSessionSequence(agent.id),
      kind,
      metadata,
    });
  }

  async function deliver(ctx, payload) {
    const agent = ctx.agents.get(payload.native_session_id);
    if (!exactAgent(agent) || !announced) throw new Error("unknown exact managed DSH session");
    const text = textOf(payload.body);
    if (!text) throw new Error("empty Agent Sessions delivery");
    const input = createUserMessage({
      content: [{ type: "text", text }],
      source: { kind: "plugin", plugin: name },
    });
    if (!input || typeof input.id !== "string" || !input.id) throw new Error("DSH did not allocate a native message identity");
    if (payload.mode === "busy-steer") {
      agent.steer(input);
    } else if (payload.mode === "idle-wake" || payload.mode === "busy-follow-up") {
      agent.followup(input);
    } else {
      throw new Error("unsupported DSH delivery mode " + String(payload.mode));
    }
    client.send("delivery.accept", payload.delivery_id, {
      delivery_id: payload.delivery_id,
      native_session_id: agent.id,
      native_message_id: input.id,
      accepted_at: Date.now(),
    });
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
        let cancelSent = false;
        const onAbort = () => {
          if (cancelSent) return;
          cancelSent = true;
          client.send("tool.cancel", nextOperation("cancel"), { call_id: callID });
        };
        const pending = client.callTool(callID, argumentsValue.action, {
          ...(argumentsValue.arguments || {}),
          claimed_native_session_id: execution.agent.id,
        });
        execution.signal?.addEventListener("abort", onAbort, { once: true });
        if (execution.signal?.aborted) onAbort();
        try {
          return await pending;
        } finally {
          execution.signal?.removeEventListener("abort", onAbort);
        }
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
    client.on("ready", () => {
      if (managedAgent) announce(ctx, managedAgent);
    });
    client.on("delivery.present", (payload) => {
      Promise.resolve(deliver(ctx, payload)).catch((error) => {
        client.send("delivery.reject", payload.delivery_id, {
          delivery_id: payload.delivery_id,
          category: "protocol",
          detail: String(error?.message || error).slice(0, 512),
        });
      });
    });
    client.on("session.bound", (payload) => {
      const agent = ctx.agents.get(payload.native_session_id);
      if (exactAgent(agent)) state(agent, agent.status);
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
    ctx.on("agent/status", ({ agent, status }) => {
      if (exactAgent(agent)) state(agent, status);
    });
    ctx.on("session/event", (session, event) => {
      if (!managedAgent || session !== managedAgent.session) return;
      if (event?.type === "session/title") {
        const title = ctx.sessionTitle.get(managedAgent.session)?.title;
        if (typeof event.seq !== "number" || !Number.isSafeInteger(event.seq) || event.seq < 0 || typeof title !== "string" || !title) return;
        if (!announced) {
          announce(ctx, managedAgent);
        } else if (title !== observedTitle) {
          const sequence = nextSessionSequence(managedAgent.id);
          if (client.observeRename("title-" + String(event.seq), managedAgent.id, title, sequence)) observedTitle = title;
        }
        return;
      }
      if (event?.type !== "turn/end" || !announced) return;
      const reason = typeof event.data?.reason?.kind === "string" ? event.data.reason.kind : "unknown";
      let kind = "failed";
      if (reason === "completed" || reason === "max-tokens") kind = "settled";
      else if (reason === "aborted" || reason === "interrupted") kind = "cancelled";
      turnEvent(managedAgent, kind, { stop_reason: reason, turn: event.data?.turn });
    });
    ctx.on("agent/disposed", ({ agent }) => {
      if (!client.bindingID || !announced || !exactAgent(agent)) return;
      client.send("session.close", nextOperation("close"), {
        native_session_id: agent.id,
        reason: "disposed",
      });
    });
    const started = client.start();
    ctx.effect?.(() => () => client.stop(), "Agent Sessions DSH component");
    return started;
  }

  return { apply, announce, deliver, state, turnEvent };
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
  if (gate.active && lanePolicy) throw new Error("DSH process cannot be both a component peer and an ACP lane");
  if (lanePolicy) {
    const native = loadLanePolicy();
    return native && typeof native.then === "function" ? native.then((loaded) => applyLanePolicy(ctx, lanePolicy, loaded)) : applyLanePolicy(ctx, lanePolicy, native);
  }
  if (!gate.active) return { active: false, reason: gate.reason };
  validateSocket(gate.socketPath, environment);
  const client = createComponentClient({ env: environment });
  // Load DSH's ESM-only native helpers only for a managed live connection; an
  // ambient profile remains entirely inert.
  return Promise.resolve(loadNative()).then(({ defineTool, createUserMessage }) =>
    createCordisPlugin({ client, defineTool, createUserMessage }).apply(ctx));
}

function apply(ctx) { return applyWithEnvironment(ctx, process.env); }

module.exports = { apply, applyLanePolicy, applyWithEnvironment, createCordisPlugin, inject, name, readLanePolicy, textOf, version };
