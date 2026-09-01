import componentModule from "../shared/component/client.js";
import protocolModule from "../shared/component/protocol.js";

const { createComponentClient } = componentModule;
const { validNativeTitleObservation } = protocolModule;

const OPERATIONS = Object.freeze([
  "peers.list", "message.send",
  "lane.start", "lane.run", "lane.resume", "lane.wait", "lane.status",
  "lane.steer", "lane.interrupt", "lane.collect", "lane.archive",
]);
const LANE_OPERATIONS = new Set(OPERATIONS.filter((operation) => operation.startsWith("lane.")));
const MAX_TEXT_BYTES = 1024 * 1024;
const GLOBAL_RUNTIME = Symbol.for("agent-sessions.pifamily.component.v1");

function boundedText(value, maximum = MAX_TEXT_BYTES) {
  const text = String(value ?? "");
  if (!text || Buffer.byteLength(text, "utf8") > maximum || text.includes("\0")) {
    throw new Error("native component text is missing or outside its bound");
  }
  return text;
}

function exactAgentFrame(value) {
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
  if (options?.componentClient) {
    const runtime = { client: options.componentClient, counter: 0, renameSession: null };
    runtime.client.renameSession = (request) => runtime.renameSession(request);
    return runtime;
  }
  if (!globalThis[GLOBAL_RUNTIME]) {
    const runtime = { client: null, counter: 0, renameSession: null };
    runtime.client = createComponentClient({ renameSession: (request) => runtime.renameSession(request) });
    globalThis[GLOBAL_RUNTIME] = runtime;
  }
  return globalThis[GLOBAL_RUNTIME];
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
  const terminalEvent = productID === "pi" ? "agent_settled" : "agent_end";

  return function agentSessionsPiFamilyExtension(pi) {
    const runtime = createRuntime(options);
    const component = runtime.client;
    // Bootstrap activity is fixed synchronously by the shared client from the
    // complete managed environment. Ambient/global loads must not alter the
    // model prompt, command UI, or native hook set while waiting for a later
    // session event to discover that no bootstrap exists.
    if (component?.active !== true) return;
    let current = null;
    let boundNativeSessionID = runtime.boundNativeSessionID ?? "";
    let active = false;
    let deliveryHandler = null;
    let boundHandler = null;
    let readyHandler = null;
    let pendingNativeRename = null;

    const sendSession = (type, nativeSessionID, extra = {}) => {
      const id = operationID(runtime, type, nativeSessionID);
      return component.send(type, id, {
        native_session_id: nativeSessionID,
        product_event_seq: runtime.counter,
        ...extra,
      });
    };

    const observedNativeTitle = (value) => {
      const title = value === undefined || value === null ? "" : value;
      if (!validNativeTitleObservation(title)) {
        throw new Error("native session title is unsafe or outside its bound");
      }
      return title;
    };

    const assertExactContext = (ctx) => {
      const nativeSessionID = sessionID(ctx);
      if (!active || !current || nativeSessionID !== current.nativeSessionID ||
          !boundNativeSessionID || boundNativeSessionID !== nativeSessionID) {
        throw new Error("registered operation is not bound to the exact managed native session");
      }
      return nativeSessionID;
    };

    const nativeRename = ({ nativeSessionID, requestedName }) => {
      if (!active || !current || !boundNativeSessionID || nativeSessionID !== current.nativeSessionID ||
          boundNativeSessionID !== nativeSessionID) {
        const error = new Error("rename request is not bound to the exact managed native session");
        error.category = "native-rejected";
        throw error;
      }
      if (typeof pi.setSessionName !== "function") {
        const error = new Error("native session naming API is unavailable");
        error.category = "unsupported";
        throw error;
      }
      if (pendingNativeRename) {
        const error = new Error("a native rename is already outstanding");
        error.category = "unavailable";
        throw error;
      }
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          if (pendingNativeRename?.resolve !== resolve) return;
          pendingNativeRename = null;
          const error = new Error("native title change was not observed");
          error.category = "timed-out";
          reject(error);
        }, 5000);
        const pending = { nativeSessionID, requestedName, resolve, reject, timer };
        pendingNativeRename = pending;
        const rejectNativeRename = (cause) => {
          if (pendingNativeRename !== pending) return;
          pendingNativeRename = null;
          clearTimeout(timer);
          const detail = cause instanceof Error ? cause.message : String(cause ?? "native rename rejected");
          const error = new Error(detail);
          error.category = "native-rejected";
          reject(error);
        };
        try {
          // This is the only daemon-requested title writer. Confirmation
          // remains the product's session_info_changed observation.
          const nativeCompletion = pi.setSessionName(requestedName);
          Promise.resolve(nativeCompletion).catch(rejectNativeRename);
        } catch (error) {
          rejectNativeRename(error);
        }
      });
    };
    runtime.renameSession = nativeRename;

    const callParent = async (operation, argumentsValue, ctx, signal) => {
      if (!OPERATIONS.includes(operation)) throw new Error("unsupported Agent Sessions operation");
      const nativeSessionID = assertExactContext(ctx);
      const callID = operationID(runtime, `${productID}-tool`, nativeSessionID);
      let cancelled = false;
      const cancel = () => {
        if (cancelled) return;
        cancelled = true;
        component.send("tool.cancel", callID, { call_id: callID });
      };
      signal?.addEventListener?.("abort", cancel, { once: true });
      try {
        return await component.callTool(callID, operation, argumentsValue ?? {});
      } finally {
        signal?.removeEventListener?.("abort", cancel);
      }
    };

    pi.registerTool({
      name: "agent_sessions",
      label: "Agent Sessions",
      description: "List peers, message sessions, and operate managed child lanes through the attested parent session.",
      promptSnippet: "Use agent_sessions for managed cross-session messages and child lanes.",
      promptGuidelines: ["Never claim a native session id; the registered tool uses the current attested session."],
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
        initialNativeTitle = observedNativeTitle(pi.getSessionName?.());
      } catch {
        // A product-owned title outside the shared component contract cannot
        // be truncated or replaced. Stay inactive until a later clean start.
        return;
      }
      current = { ctx, nativeSessionID: sessionID(ctx) };
      const activation = await component.start();
      if (!activation.active) {
        active = false;
        current = null;
        return;
      }
      active = true;
      // A non-final session shutdown clears the exact-session callback while
      // the component connection stays alive. Restore it for every newly
      // activated in-process session before announcing/rebinding that session.
      runtime.renameSession = nativeRename;
      process.env.AGENT_SESSIONS_SESSION_ID = current.nativeSessionID;
      process.env.AGENT_SESSIONS_NATIVE_SESSION_ID = current.nativeSessionID;
      process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID = activation.bindingID;

      if (!deliveryHandler) {
        readyHandler = ({ bindingID, daemonGeneration }) => {
          if (!active || !current || !bindingID) return;
          let nativeTitle;
          try {
            nativeTitle = observedNativeTitle(pi.getSessionName?.());
          } catch {
            return;
          }
          boundNativeSessionID = "";
          runtime.boundNativeSessionID = "";
          runtime.daemonGeneration = daemonGeneration;
          process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID = bindingID;
          sendSession("session.announce", current.nativeSessionID, {
            binding_id: bindingID,
            cwd: boundedText(current.ctx.cwd),
            native_name: nativeTitle,
          });
        };
        boundHandler = (payload) => {
          if (payload?.binding_id === component.bindingID && current && payload?.native_session_id === current.nativeSessionID) {
            boundNativeSessionID = payload.native_session_id;
            runtime.boundNativeSessionID = payload.native_session_id;
          }
        };
        deliveryHandler = async (payload) => {
          try {
            if (!current || !boundNativeSessionID || boundNativeSessionID !== current.nativeSessionID) {
              throw new Error("delivery has no exact session.bound witness");
            }
            const frame = exactAgentFrame(payload?.body);
            const mode = payload?.mode;
            if (mode === "idle-wake") {
              if (!current.ctx.isIdle()) throw new Error("idle delivery reached a busy native session");
              await Promise.resolve(pi.sendUserMessage(frame.content));
            } else if (mode === "busy-steer") {
              if (current.ctx.isIdle()) throw new Error("steer delivery reached an idle native session");
              // OMP must receive the original text; it owns its native
              // <system-notice> interjection framing.
              await Promise.resolve(pi.sendUserMessage(frame.content, { deliverAs: "steer" }));
            } else if (mode === "busy-follow-up") {
              if (current.ctx.isIdle()) throw new Error("follow-up delivery reached an idle native session");
              await Promise.resolve(pi.sendUserMessage(frame.content, { deliverAs: "followUp" }));
            } else {
              throw new Error("delivery mode is unsupported");
            }
            component.send("delivery.accept", payload.delivery_id, {
              delivery_id: payload.delivery_id,
              native_session_id: current.nativeSessionID,
              native_message_id: frame.messageID,
              accepted_at: Date.now(),
            });
          } catch (error) {
            component.send("delivery.reject", payload?.delivery_id ?? operationID(runtime, "delivery-reject"), {
              delivery_id: payload?.delivery_id ?? "unknown-delivery",
              category: "protocol",
              detail: String(error?.message ?? error).slice(0, 512),
            });
          }
        };
        component.on("ready", readyHandler);
        component.on("session.bound", boundHandler);
        component.on("delivery.present", deliveryHandler);
      }

      const priorNativeSessionID = runtime.nativeSessionID ?? "";
      if (priorNativeSessionID && priorNativeSessionID !== current.nativeSessionID) {
        boundNativeSessionID = "";
        runtime.boundNativeSessionID = "";
        const id = operationID(runtime, "session.rebind", current.nativeSessionID);
        component.send("session.rebind", id, {
          binding_id: component.bindingID,
          old_native_session_id: priorNativeSessionID,
          new_native_session_id: current.nativeSessionID,
          evidence: { reason: _event.reason },
          product_event_seq: runtime.counter,
        });
      } else if (!priorNativeSessionID) {
        sendSession("session.announce", current.nativeSessionID, {
          binding_id: component.bindingID,
          cwd: boundedText(ctx.cwd),
          native_name: initialNativeTitle,
        });
      } else {
        sendSession("session.state", current.nativeSessionID, { state: ctx.isIdle() ? "idle" : "busy" });
      }
      runtime.nativeSessionID = current.nativeSessionID;
    });

    pi.on("session_info_changed", (event, ctx) => {
      const nativeSessionID = assertExactContext(ctx);
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
      runtime.counter += 1;
      const productEventSeq = runtime.counter;
      if (pendingNativeRename && pendingNativeRename.nativeSessionID === nativeSessionID && pendingNativeRename.requestedName === nativeName) {
        const pending = pendingNativeRename;
        pendingNativeRename = null;
        clearTimeout(pending.timer);
        pending.resolve({ nativeName, productEventSeq });
        return;
      }
      component.observeRename(`${productID}-${nativeSessionID}-${productEventSeq}`, nativeSessionID, nativeName, productEventSeq);
    });

    pi.on("agent_start", (_event, ctx) => {
      const nativeSessionID = assertExactContext(ctx);
      sendSession("session.state", nativeSessionID, { state: "busy" });
      sendSession("turn.event", nativeSessionID, { event_seq: runtime.counter, kind: "agent_start", metadata: {} });
    });

    pi.on(terminalEvent, (_event, ctx) => {
      const nativeSessionID = assertExactContext(ctx);
      if (productID === "omp" && _event?.willContinue === true) {
        sendSession("turn.event", nativeSessionID, { event_seq: runtime.counter, kind: "agent_end_continuing", metadata: {} });
        return;
      }
      sendSession("turn.event", nativeSessionID, { event_seq: runtime.counter, kind: terminalEvent, metadata: {} });
      sendSession("session.state", nativeSessionID, { state: "idle" });
    });

    pi.on("session_shutdown", async (event, ctx) => {
      const nativeSessionID = sessionID(ctx);
      if (pendingNativeRename) {
        const pending = pendingNativeRename;
        pendingNativeRename = null;
        clearTimeout(pending.timer);
        const error = new Error("native session shut down during rename");
        error.category = "unavailable";
        pending.reject(error);
      }
      if (active && event.reason === "quit" && current?.nativeSessionID === nativeSessionID) {
        component.send("session.close", operationID(runtime, "session.close", nativeSessionID), {
          native_session_id: nativeSessionID,
          reason: "native-session-quit",
        });
        await component.stop();
        delete globalThis[GLOBAL_RUNTIME];
      }
      if (process.env.AGENT_SESSIONS_SESSION_ID === nativeSessionID) delete process.env.AGENT_SESSIONS_SESSION_ID;
      if (process.env.AGENT_SESSIONS_NATIVE_SESSION_ID === nativeSessionID) delete process.env.AGENT_SESSIONS_NATIVE_SESSION_ID;
      if (process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID === component.bindingID) delete process.env.AGENT_SESSIONS_COMPONENT_BINDING_ID;
      if (boundHandler) component.off?.("session.bound", boundHandler);
      if (deliveryHandler) component.off?.("delivery.present", deliveryHandler);
      if (readyHandler) component.off?.("ready", readyHandler);
      boundHandler = null;
      deliveryHandler = null;
      readyHandler = null;
      boundNativeSessionID = "";
      active = false;
      current = null;
      if (runtime.renameSession === nativeRename) runtime.renameSession = null;
    });
  };
}

export { OPERATIONS };
