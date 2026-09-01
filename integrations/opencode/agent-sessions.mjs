import { tool } from "@opencode-ai/plugin";
import componentModule from "../shared/component/client.js";
import componentProtocolModule from "../shared/component/protocol.js";

const { createComponentClient } = componentModule;
const { validNativeTitleObservation } = componentProtocolModule;

const OPERATIONS = [
  "peers.list", "message.send",
  "lane.start", "lane.run", "lane.resume", "lane.wait", "lane.status",
  "lane.steer", "lane.interrupt", "lane.collect", "lane.archive",
];
const DELIVERY_MODES = new Set(["idle-wake", "busy-steer", "busy-follow-up"]);
const DELIVERY_DEADLINE_MS = 10_000;

function boundedText(value, maximum = 1024 * 1024) {
  const text = String(value ?? "");
  if (!text || Buffer.byteLength(text, "utf8") > maximum || text.includes("\0")) {
    throw new Error("native component text is missing or outside its bound");
  }
  return text;
}

function validNativeID(value, prefix) {
  return typeof value === "string" && value.length > prefix.length && Buffer.byteLength(value, "utf8") <= 256 &&
    value.startsWith(prefix) && new RegExp(`^${prefix}[A-Za-z0-9_-]+$`, "u").test(value);
}

function nativeMessageID(operationID) {
  const suffix = Buffer.from(String(operationID)).toString("hex").slice(0, 48);
  return `msg_${suffix || Date.now().toString(16)}`;
}

function toolCallID(counter, messageID) {
  const suffix = Buffer.from(boundedText(messageID, 512)).toString("hex").slice(0, 48);
  return `opencode-tool-${counter}-${suffix}`;
}

function requireSDKSuccess(result, expectedStatus) {
  if (!result || result.error != null || result.response?.status !== expectedStatus) {
    const error = new Error("OpenCode SDK request lacked exact native acceptance");
    if (result?.error != null || Number.isInteger(result?.response?.status)) error.category = "native-rejected";
    throw error;
  }
  return result.data;
}

function abortError(message, category) {
  const error = new Error(message);
  error.category = category;
  return error;
}

async function boundedNativeCall(invoke, deadline, controller) {
  const remaining = deadline - Date.now();
  if (remaining <= 0) {
    controller.abort();
    throw abortError("OpenCode native delivery deadline expired", "timed-out");
  }
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(invoke),
      new Promise((_, reject) => {
        timer = setTimeout(() => {
          controller.abort();
          reject(abortError("OpenCode native delivery deadline expired", "timed-out"));
        }, remaining);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

async function abortableRenameCall(invoke, signal) {
  if (signal?.aborted) throw abortError("native rename was cancelled before dispatch", "timed-out");
  const request = Promise.resolve().then(invoke);
  if (!signal?.addEventListener) return request;
  let onAbort;
  try {
    return await Promise.race([
      request,
      new Promise((_, reject) => {
        onAbort = () => reject(abortError("native rename outcome is uncertain after cancellation", "ambiguous-session"));
        signal.addEventListener("abort", onAbort, { once: true });
      }),
    ]);
  } finally {
    signal.removeEventListener?.("abort", onAbort);
  }
}

function renderAgentFrame(frame) {
  const content = boundedText(frame?.content ?? JSON.stringify(frame));
  const from = frame?.source?.name ?? frame?.source?.id ?? frame?.source_session_id ?? "peer";
  return `<cross-session-message from="${String(from).replace(/[<>"\r\n]/gu, "")}">\n${content.replace(/<\/cross-session-message/giu, "<\\/cross-session-message")}\n</cross-session-message>`;
}

function sessionInfo(event) {
  const properties = event?.properties ?? {};
  return properties.info ?? properties.session ?? properties;
}

function eventSessionID(event) {
  const properties = event?.properties ?? {};
  if (event?.type === "session.created" || event?.type === "session.updated" || event?.type === "session.deleted") {
    return properties.info?.id ?? properties.session?.id ?? properties.sessionID ?? "";
  }
  return properties.sessionID ?? properties.info?.sessionID ?? properties.permission?.sessionID ?? event?.data?.sessionID ?? "";
}

function eventNativeTitle(value) {
  return typeof value === "string" && validNativeTitleObservation(value) ? value : undefined;
}

export default async function agentSessionsOpenCodePlugin({ client, directory }) {
  let productEventSeq = 0;
  let toolCounter = 0;
  let boundSessionID = "";
  const known = new Map();
  const pendingRenames = new Map();

  const component = createComponentClient({
    renameSession: async ({ nativeSessionID, requestedName, signal }) => {
      if (nativeSessionID !== boundSessionID) {
        const error = new Error("rename request targeted a foreign native session");
        error.category = "unauthorized";
        throw error;
      }
      const nativeName = boundedText(requestedName, 1024);
      if (signal?.aborted) throw abortError("native rename was cancelled before dispatch", "timed-out");
      if (pendingRenames.has(nativeSessionID)) {
        throw abortError("an exact native-session rename is already in progress", "native-rejected");
      }
      const pending = { nativeName, writeStarted: false, held: undefined, conflicted: false };
      pendingRenames.set(nativeSessionID, pending);
      try {
        pending.writeStarted = true;
        const response = await abortableRenameCall(() => client.session.update({
          path: { id: nativeSessionID },
          query: { directory },
          body: { title: nativeName },
        }, { signal }), signal);
        const updated = requireSDKSuccess(response, 200);
        if (signal?.aborted) throw abortError("native rename outcome is uncertain after cancellation", "ambiguous-session");
        if (boundSessionID !== nativeSessionID || updated?.id !== nativeSessionID || updated?.title !== nativeName) {
          throw abortError("native rename response was not exact", "native-rejected");
        }
        if (pending.conflicted) throw abortError("native rename raced a conflicting native title", "ambiguous-session");
        const prior = known.get(nativeSessionID) ?? {};
        known.set(nativeSessionID, { ...prior, title: nativeName });
        const correlatedSeq = pending.held?.productEventSeq ?? ++productEventSeq;
        return { nativeName, productEventSeq: correlatedSeq };
      } catch (error) {
        if (pending.held) {
          const held = pending.held;
          setTimeout(() => component.observeRename(held.nativeEventID, nativeSessionID, held.nativeName, held.productEventSeq), 0);
        }
        throw error;
      } finally {
        if (pendingRenames.get(nativeSessionID) === pending) pendingRenames.delete(nativeSessionID);
      }
    },
  });
  const activation = await component.start();
  if (!activation.active) return {};

  const sendSession = (type, sessionID, extra = {}) => {
    if (!validNativeID(sessionID, "ses_")) return;
    if (type === "session.rename" && !validNativeTitleObservation(extra.native_name)) return;
    productEventSeq += 1;
    if (type === "session.rename") {
      component.observeRename(`${sessionID}.${productEventSeq}`, sessionID, extra.native_name, productEventSeq);
      return;
    }
    component.send(type, `${type}-${sessionID}-${productEventSeq}`, {
      native_session_id: sessionID,
      product_event_seq: productEventSeq,
      ...extra,
    });
  };

  component.on("session.bound", (payload) => {
    if (payload?.binding_id === component.bindingID && validNativeID(payload.native_session_id, "ses_")) boundSessionID = payload.native_session_id;
  });

  component.on("delivery.present", async (payload) => {
    let nativeSubmitAttempted = false;
    const controller = new AbortController();
    try {
      if (!boundSessionID) throw new Error("no exact native session is bound");
      if (!DELIVERY_MODES.has(payload?.mode)) throw new Error("OpenCode delivery mode is unsupported");
      const messageID = nativeMessageID(payload.delivery_id);
      const deadline = Date.now() + DELIVERY_DEADLINE_MS;
      nativeSubmitAttempted = true;
      const response = await boundedNativeCall(() => client.session.promptAsync({
        path: { id: boundSessionID },
        query: { directory },
        body: {
          messageID,
          noReply: payload.body?.no_reply === true || payload.body?.type === "notice",
          parts: [{ type: "text", text: renderAgentFrame(payload.body) }],
        },
      }, { signal: controller.signal }), deadline, controller);
      requireSDKSuccess(response, 204);
      component.send("delivery.accept", payload.delivery_id, {
        delivery_id: payload.delivery_id,
        native_session_id: boundSessionID,
        native_message_id: messageID,
        accepted_at: Date.now(),
      });
    } catch (error) {
      component.send("delivery.reject", payload.delivery_id, {
        delivery_id: payload.delivery_id,
        category: nativeSubmitAttempted && error?.category !== "native-rejected" ? "ambiguous-session" : (error?.category ?? "protocol"),
        detail: String(error?.message ?? error).slice(0, 512),
      });
    } finally {
      controller.abort();
    }
  });

  const hooks = {
    event: async ({ event }) => {
      const info = sessionInfo(event);
      const sessionID = eventSessionID(event);
      switch (event?.type) {
        case "session.created":
        case "session.updated": {
          if (!sessionID) break;
          if (!Object.hasOwn(info, "title")) break;
          const nativeTitle = eventNativeTitle(info?.title);
          if (nativeTitle === undefined) break;
          const prior = known.get(sessionID);
          known.set(sessionID, { title: nativeTitle, cwd: info?.directory ?? directory });
          const pending = pendingRenames.get(sessionID);
          if (pending?.nativeName === nativeTitle) {
            if (!pending.held) {
              productEventSeq += 1;
              pending.held = {
                nativeEventID: `${sessionID}.${productEventSeq}`,
                nativeName: nativeTitle,
                productEventSeq,
              };
            }
            break;
          }
          if (pending && nativeTitle !== pending.nativeName) pending.conflicted = true;
          if (!prior) {
            productEventSeq += 1;
            component.send("session.announce", `announce-${sessionID}-${productEventSeq}`, {
              binding_id: component.bindingID,
              native_session_id: sessionID,
              cwd: info?.directory ?? directory,
              native_name: nativeTitle,
              product_event_seq: productEventSeq,
            });
          } else if (nativeTitle !== prior.title) {
            sendSession("session.rename", sessionID, { native_name: nativeTitle });
          }
          break;
        }
        case "session.idle":
          sendSession("session.state", sessionID, { state: "idle" });
          break;
        case "session.status": {
          const state = event?.properties?.status?.type;
          if (state === "busy" || state === "idle") sendSession("session.state", sessionID, { state });
          break;
        }
        case "session.deleted":
          if (sessionID) component.send("session.close", `close-${sessionID}-${++productEventSeq}`, { native_session_id: sessionID, reason: "native-session-deleted" });
          break;
        case "message.updated":
        case "permission.asked":
          if (sessionID) component.send("turn.event", `turn-${sessionID}-${++productEventSeq}`, {
            native_session_id: sessionID,
            event_seq: productEventSeq,
            kind: event.type,
            metadata: {},
          });
          break;
      }
    },

    "shell.env": async (input, output) => {
      const sessionID = boundedText(input.sessionID, 256);
      if (!validNativeID(sessionID, "ses_")) throw new Error("shell context omitted the exact native session identity");
      output.env.AGENT_SESSIONS_SESSION_ID = sessionID;
      output.env.AGENT_SESSIONS_NATIVE_SESSION_ID = sessionID;
      output.env.AGENT_SESSIONS_COMPONENT_BINDING_ID = component.bindingID;
      const hasNativeTitle = Object.hasOwn(input, "title") && typeof input.title === "string" &&
        validNativeTitleObservation(input.title);
      if (!known.has(sessionID) && hasNativeTitle) {
        productEventSeq += 1;
        component.send("session.announce", `shell-announce-${sessionID}-${productEventSeq}`, {
          binding_id: component.bindingID,
          native_session_id: sessionID,
          cwd: input.cwd ?? directory,
          native_name: input.title,
          product_event_seq: productEventSeq,
        });
        known.set(sessionID, { title: input.title, cwd: input.cwd ?? directory });
      }
    },

    tool: {
      agent_sessions: tool({
        description: "Use an exact managed Agent Sessions operation.",
        args: {
          operation: tool.schema.enum(OPERATIONS),
          arguments: tool.schema.record(tool.schema.any()).default({}),
        },
        execute: async ({ operation, arguments: argumentsValue }, context) => {
          if (!boundSessionID || context.sessionID !== boundSessionID) throw new Error("tool context is not the exact bound native session");
          const callID = toolCallID(++toolCounter, context.messageID);
          const cancel = () => { component.send("tool.cancel", callID, { call_id: callID }); };
          if (context.abort?.aborted) {
            cancel();
            throw new Error("Agent Sessions operation was cancelled before dispatch");
          }
          context.abort?.addEventListener?.("abort", cancel, { once: true });
          try {
            const result = await component.callTool(callID, operation, {
              ...argumentsValue,
              __agent_sessions_native_session_id: context.sessionID,
            });
            return JSON.stringify(result);
          } finally {
            context.abort?.removeEventListener?.("abort", cancel);
          }
        },
      }),
    },
  };

  process.once("beforeExit", () => { void component.stop(); });
  return hooks;
}
