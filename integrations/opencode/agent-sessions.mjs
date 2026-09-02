import { tool } from "@opencode-ai/plugin";
import liveSessionModule from "../shared/live-session.js";

const { createLiveSessionClient } = liveSessionModule;

const OPERATIONS = [
  "peers.list", "message.send",
  "lane.start", "lane.run", "lane.resume", "lane.wait", "lane.status",
  "lane.steer", "lane.interrupt", "lane.collect", "lane.archive",
];
const DELIVERY_DEADLINE_MS = 10_000;

function boundedText(value, maximum = 1024 * 1024) {
  const text = String(value ?? "");
  if (!text || Buffer.byteLength(text, "utf8") > maximum || text.includes("\0")) {
    throw new Error("native product text is missing or outside its bound");
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

function renderAgentFrame(frame) {
	if (typeof frame === "string") return boundedText(frame);
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

function validNativeTitleObservation(value) {
  return typeof value === "string" && Buffer.from(value, "utf8").toString("utf8") === value &&
    Buffer.byteLength(value, "utf8") <= 1024 && !/\p{Cc}/u.test(value);
}

export default async function agentSessionsOpenCodePlugin({ client, directory }) {
  let toolCounter = 0;
  const known = new Map();
  const live = createLiveSessionClient();
  const activation = await live.start();
  if (!activation.active) return {};

  live.on("message", async (payload) => {
    let nativeSubmitAttempted = false;
    const controller = new AbortController();
    try {
      if (!known.has(payload.nativeSessionID)) throw new Error("no exact live native session is reported");
      const messageID = nativeMessageID(payload.messageID);
      const deadline = Date.now() + DELIVERY_DEADLINE_MS;
      nativeSubmitAttempted = true;
      const response = await boundedNativeCall(() => client.session.promptAsync({
        path: { id: payload.nativeSessionID },
        query: { directory },
        body: {
          messageID,
          noReply: payload.body?.no_reply === true || payload.body?.type === "notice",
          parts: [{ type: "text", text: renderAgentFrame(payload.body) }],
        },
      }, { signal: controller.signal }), deadline, controller);
      requireSDKSuccess(response, 204);
      live.acceptMessage(payload.messageID, { native_message_id: messageID });
    } catch (error) {
      live.rejectMessage(payload.messageID,
        nativeSubmitAttempted && error?.category !== "native-rejected" ? "native delivery outcome is unknown" : error?.message ?? error);
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
          if (!prior) {
            live.report(sessionID, nativeTitle);
          } else if (nativeTitle !== prior.title) {
            live.updateName(sessionID, nativeTitle);
          }
          break;
        }
        case "session.deleted":
          if (sessionID) live.closeSession(sessionID);
          break;
      }
    },

    "shell.env": async (input, output) => {
      const sessionID = boundedText(input.sessionID, 256);
      if (!validNativeID(sessionID, "ses_")) throw new Error("shell context omitted the exact native session identity");
      output.env.AGENT_SESSIONS_SESSION_ID = sessionID;
      output.env.AGENT_SESSIONS_NATIVE_SESSION_ID = sessionID;
      const hasNativeTitle = Object.hasOwn(input, "title") && typeof input.title === "string" &&
        validNativeTitleObservation(input.title);
      if (!known.has(sessionID) && hasNativeTitle) {
        live.report(sessionID, input.title);
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
          if (!known.has(context.sessionID)) throw new Error("tool context is not a reported live native session");
          const callID = toolCallID(++toolCounter, context.messageID);
          if (context.abort?.aborted) {
            throw new Error("Agent Sessions operation was cancelled before dispatch");
          }
          return JSON.stringify(await live.callTool(context.sessionID, callID, operation, argumentsValue));
        },
      }),
    },
  };

  process.once("beforeExit", () => { void live.stop(); });
  return hooks;
}
