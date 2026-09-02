import liveSessionModule from "../shared/live-session.js";

const { createLiveSessionClient } = liveSessionModule;

const OPERATIONS = Object.freeze([
  "peers.list", "message.send",
  "lane.start", "lane.run", "lane.resume", "lane.wait", "lane.status",
  "lane.steer", "lane.interrupt", "lane.collect", "lane.archive",
]);
const LANE_OPERATIONS = new Set(OPERATIONS.filter((operation) => operation.startsWith("lane.")));
const MAX_TEXT_BYTES = 1024 * 1024;
const GLOBAL_RUNTIME = Symbol.for("agent-sessions.pifamily.live-session");

function boundedText(value, maximum = MAX_TEXT_BYTES) {
  const text = String(value ?? "");
  if (!text || Buffer.byteLength(text, "utf8") > maximum || text.includes("\0")) {
    throw new Error("native product text is missing or outside its bound");
  }
  return text;
}

function exactAgentFrame(value) {
  if (typeof value === "string") return { messageID: `live-${Date.now()}`, content: boundedText(value) };
  if (!value || value.version !== 1 || value.type !== "delivery" || typeof value.message_id !== "string") {
    throw new Error("delivery body is not an AgentFrame v1 delivery");
  }
  return { messageID: boundedText(value.message_id, 512), content: boundedText(value.content) };
}

function schema() {
  return {
    type: "object",
    properties: {
      operation: { type: "string", enum: [...OPERATIONS] },
      arguments: { type: "object", additionalProperties: true },
    },
    required: ["operation"],
    additionalProperties: false,
  };
}

function operationID(state, prefix, nativeSessionID = "session") {
  state.counter += 1;
  return `${prefix}-${nativeSessionID}-${state.counter}`;
}

function createRuntime(options) {
  if (options?.liveSessionClient) {
    return { client: options.liveSessionClient, counter: 0 };
  }
  if (!globalThis[GLOBAL_RUNTIME]) {
    globalThis[GLOBAL_RUNTIME] = { client: createLiveSessionClient(), counter: 0 };
  }
  return globalThis[GLOBAL_RUNTIME];
}

function validNativeTitleObservation(value) {
  return typeof value === "string" && Buffer.from(value, "utf8").toString("utf8") === value &&
    Buffer.byteLength(value, "utf8") <= 1024 && !/\p{Cc}/u.test(value);
}

function sessionID(ctx) {
  const value = ctx?.sessionManager?.getSessionId?.();
  return boundedText(value, 128);
}

function safeResult(value) {
  const encoded = JSON.stringify(value ?? null);
  if (Buffer.byteLength(encoded, "utf8") > MAX_TEXT_BYTES) throw new Error("parent tool result exceeds its bound");
  return encoded;
}

// createPiFamilyExtension is the only shared product adapter. Product entry
// points select the closed terminal-event quirk; native APIs do the framing.
export function createPiFamilyExtension(productID, options = {}) {
  if (productID !== "pi" && productID !== "omp") throw new Error("unknown Pi-family product");

  return function agentSessionsPiFamilyExtension(pi) {
    const runtime = createRuntime(options);
    const live = runtime.client;
    const environment = options.environment ?? process.env;
    // Connection activity is fixed synchronously by the shared client from the
    // complete managed environment. Ambient/global loads must not alter the
    // model prompt, command UI, or native hook set while waiting for a later
    // session event.
    if (live?.active !== true) return;
    let current = null;
    let deliveryHandler = null;

    const observedNativeTitle = (value) => {
      const title = value === undefined || value === null ? "" : value;
      if (!validNativeTitleObservation(title)) {
        throw new Error("native session title is unsafe or outside its bound");
      }
      return title;
    };

    const assertExactContext = (ctx) => {
      const nativeSessionID = sessionID(ctx);
      if (!current || nativeSessionID !== current.nativeSessionID || !live.sessions.has(nativeSessionID)) {
        throw new Error("registered operation is not attached to the live native session");
      }
      return nativeSessionID;
    };

    const callParent = async (operation, argumentsValue, ctx, signal) => {
      if (!OPERATIONS.includes(operation)) throw new Error("unsupported Agent Sessions operation");
      const nativeSessionID = assertExactContext(ctx);
      const callID = operationID(runtime, `${productID}-tool`, nativeSessionID);
      if (signal?.aborted) throw new Error("Agent Sessions operation was cancelled before dispatch");
      return live.callTool(nativeSessionID, callID, operation, argumentsValue ?? {});
    };

    pi.registerTool({
      name: "agent_sessions",
      label: "Agent Sessions",
      description: "List peers, message sessions, and operate child lanes from this live session.",
      promptSnippet: "Use agent_sessions for managed cross-session messages and child lanes.",
      promptGuidelines: ["The registered tool uses the current live product session."],
      parameters: schema(),
      async execute(_toolCallID, parameters, signal, _onUpdate, ctx) {
        try {
          const result = await callParent(parameters.operation, parameters.arguments, ctx, signal);
          return { content: [{ type: "text", text: safeResult(result) }], details: { operation: parameters.operation } };
        } catch (error) {
          return { content: [{ type: "text", text: boundedText(error?.message ?? error, 4096) }], details: {}, isError: true };
        }
      },
    });

    pi.registerCommand("lane", {
      description: "Run an Agent Sessions lane operation: /lane <operation> [JSON object]",
      async handler(argumentsText, ctx) {
        try {
          const match = String(argumentsText ?? "").trim().match(/^(\S+)(?:\s+([\s\S]+))?$/u);
          if (!match || !LANE_OPERATIONS.has(match[1])) throw new Error("usage: /lane <lane.operation> [JSON object]");
          const argumentsValue = match[2] ? JSON.parse(match[2]) : {};
          if (!argumentsValue || Array.isArray(argumentsValue) || typeof argumentsValue !== "object") throw new Error("lane arguments must be a JSON object");
          const result = await callParent(match[1], argumentsValue, ctx, ctx.signal);
          ctx.ui.notify(safeResult(result), "info");
        } catch (error) {
          ctx.ui.notify(String(error?.message ?? error).slice(0, 4096), "error");
        }
      },
    });

    pi.on("session_start", async (_event, ctx) => {
      let initialNativeTitle;
      try {
        const requestedName = String(environment.AGENT_SESSIONS_SESSION_NAME ?? "").trim();
        if (requestedName && !pi.getSessionName?.()) {
          await Promise.resolve(pi.setSessionName?.(requestedName));
        }
        initialNativeTitle = observedNativeTitle(pi.getSessionName?.());
      } catch {
        return;
      }
      const nativeSessionID = sessionID(ctx);
      if (current?.nativeSessionID && current.nativeSessionID !== nativeSessionID) {
        live.closeSession(current.nativeSessionID);
      }
      current = { ctx, nativeSessionID };
      const activation = await live.start();
      if (!activation.active) {
        current = null;
        return;
      }
      process.env.AGENT_SESSIONS_SESSION_ID = current.nativeSessionID;
      process.env.AGENT_SESSIONS_NATIVE_SESSION_ID = current.nativeSessionID;

      if (!deliveryHandler) {
        deliveryHandler = async (payload) => {
          try {
            if (!current || payload.nativeSessionID !== current.nativeSessionID) {
              throw new Error("delivery targets a different live native session");
            }
            const frame = exactAgentFrame(payload.body);
            if (current.ctx.isIdle()) {
              await Promise.resolve(pi.sendUserMessage(frame.content));
            } else {
              await Promise.resolve(pi.sendUserMessage(frame.content, { deliverAs: "steer" }));
            }
            live.acceptMessage(payload.messageID, { native_message_id: frame.messageID });
          } catch (error) {
            live.rejectMessage(payload.messageID, error?.message ?? error);
          }
        };
        live.on("message", deliveryHandler);
      }
      live.report(current.nativeSessionID, initialNativeTitle);
    });

    pi.on("session_info_changed", (event, ctx) => {
      const nativeSessionID = sessionID(ctx);
      // setSessionName may synchronously emit this event while session_start
      // is still establishing the live report. The report below carries the
      // final product title, so there is nothing to update until it exists.
      if (!current || current.nativeSessionID !== nativeSessionID || !live.sessions.has(nativeSessionID)) return;
      let nativeName;
      try {
        // Pi emits name:undefined when its native title is cleared. Empty is
        // the genuine product observation; it must never be replaced with a
        // fabricated Agent Sessions title.
        nativeName = observedNativeTitle(event.name);
      } catch {
        // Unsafe or oversized product data cannot mutate the follower or be
        // sent as a false observation. Wait for a later valid product event.
        return;
      }
      live.updateName(nativeSessionID, nativeName);
    });

    pi.on("session_shutdown", async (event, ctx) => {
      const nativeSessionID = sessionID(ctx);
      live.closeSession(nativeSessionID);
      if (event.reason === "quit") {
        await live.stop();
        delete globalThis[GLOBAL_RUNTIME];
      }
      if (process.env.AGENT_SESSIONS_SESSION_ID === nativeSessionID) delete process.env.AGENT_SESSIONS_SESSION_ID;
      if (process.env.AGENT_SESSIONS_NATIVE_SESSION_ID === nativeSessionID) delete process.env.AGENT_SESSIONS_NATIVE_SESSION_ID;
      current = null;
    });
  };
}

export { OPERATIONS };
