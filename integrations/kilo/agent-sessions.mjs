import { tool } from "@kilocode/plugin";
import liveSessionModule from "../shared/live-session.js";

const { createLiveSessionClient, renderDelivery } = liveSessionModule;
const OPERATIONS = [
  "peers.list", "message.send",
  "lane.start", "lane.run", "lane.resume", "lane.wait", "lane.status",
  "lane.steer", "lane.interrupt", "lane.archive",
];
const MAX_MESSAGE_SNAPSHOT_BYTES = 1024 * 1024;
const MAX_MESSAGE_ENTRIES = 4096;
const DELIVERY_DEADLINE_MS = 10_000;
const DELIVERY_POLL_MS = 25;

function boundedText(value, maximum = 1024 * 1024) {
  const text = String(value ?? "");
  if (!text || Buffer.byteLength(text, "utf8") > maximum || text.includes("\0")) throw new Error("native product text is outside its bound");
  return text;
}

function validNativeID(value, prefix) {
  return typeof value === "string" && value.length > prefix.length && Buffer.byteLength(value, "utf8") <= 256 &&
    value.startsWith(prefix) && new RegExp(`^${prefix}[A-Za-z0-9_-]+$`, "u").test(value);
}

function toolCallID(counter, messageID) {
  const suffix = Buffer.from(boundedText(messageID, 512)).toString("hex").slice(0, 48);
  return `kilo-tool-${counter}-${suffix}`;
}

function requireSDKSuccess(result, expectedStatus, requireTrue = false) {
  if (!result || result.error != null || result.response?.status !== expectedStatus ||
      (requireTrue && (result.data ?? result) !== true)) {
    const error = new Error("Kilo SDK request lacked exact native acceptance");
    if (result?.error != null || Number.isInteger(result?.response?.status)) error.category = "native-rejected";
    throw error;
  }
  return result.data;
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

async function boundedNativeCall(invoke, deadline, controller) {
  const remaining = deadline - Date.now();
  if (remaining <= 0) {
    controller.abort();
    const error = new Error("Kilo native delivery deadline expired");
    error.category = "timed-out";
    throw error;
  }
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(invoke),
      new Promise((_, reject) => {
        timer = setTimeout(() => {
          controller.abort();
          const error = new Error("Kilo native delivery deadline expired");
          error.category = "timed-out";
          reject(error);
        }, remaining);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

async function listMessages(client, directory, sessionID, deadline, controller) {
  const response = await boundedNativeCall(() => client.session.messages(
    { path: { id: sessionID }, query: { directory } }, { signal: controller.signal },
  ), deadline, controller);
  const messages = response?.data;
  if (response?.error != null || response?.response?.status !== 200 || !Array.isArray(messages) || messages.length > MAX_MESSAGE_ENTRIES) {
    throw new Error("Kilo message query lacked an exact bounded response");
  }
  let encoded;
  try {
    encoded = JSON.stringify(messages);
  } catch {
    throw new Error("Kilo message query was not bounded JSON");
  }
  if (Buffer.byteLength(encoded, "utf8") > MAX_MESSAGE_SNAPSHOT_BYTES) throw new Error("Kilo message query exceeded its fixed bound");
  return messages;
}

export default async function agentSessionsKiloPlugin({ client, directory, environment = process.env }) {
  let toolCounter = 0;
  const known = new Map();
  const requestedName = String(environment.AGENT_SESSIONS_SESSION_NAME ?? "").trim();
  const live = createLiveSessionClient();
  const activation = await live.start();
  if (!activation.active) return {};

  const resumedSessionID = String(environment.AGENT_SESSIONS_SESSION_ID ?? "").trim();
  if (resumedSessionID) {
    if (!validNativeID(resumedSessionID, "ses_")) throw new Error("Kilo resume omitted an exact native session identity");
    void Promise.resolve().then(async () => {
      const resumed = requireSDKSuccess(await client.session.get({
        path: { id: resumedSessionID },
        query: { directory },
      }), 200);
      const resumedTitle = eventNativeTitle(resumed?.title);
      if (resumedTitle === undefined) throw new Error("Kilo resume omitted the native session title");
      known.set(resumedSessionID, { title: resumedTitle, cwd: resumed?.directory ?? directory });
      live.report(resumedSessionID, resumedTitle, { cwd: resumed?.directory ?? directory });
    }).catch((error) => live.emit("diagnostic", String(error?.message ?? error)));
  }

  live.on("message", async (payload) => {
    let nativeSubmitAttempted = false;
    const controller = new AbortController();
    try {
      if (!known.has(payload.nativeSessionID)) throw new Error("no exact live native session is reported");
      const deadline = Date.now() + DELIVERY_DEADLINE_MS;
      const before = new Set((await listMessages(client, directory, payload.nativeSessionID, deadline, controller)).map((entry) => entry?.info?.id));
      const text = renderDelivery(payload);
      requireSDKSuccess(await boundedNativeCall(() => client.tui.clearPrompt(
        { query: { directory } }, { signal: controller.signal },
      ), deadline, controller), 200, true);
      requireSDKSuccess(await boundedNativeCall(() => client.tui.appendPrompt(
        { query: { directory }, body: { text } }, { signal: controller.signal },
      ), deadline, controller), 200, true);
      nativeSubmitAttempted = true;
      requireSDKSuccess(await boundedNativeCall(() => client.tui.submitPrompt(
        { query: { directory } }, { signal: controller.signal },
      ), deadline, controller), 200, true);

      let nativeMessageID = "";
      while (!nativeMessageID && Date.now() < deadline) {
        const messages = await listMessages(client, directory, payload.nativeSessionID, deadline, controller);
        for (const message of messages) {
          if (before.has(message?.info?.id) || message?.info?.sessionID !== payload.nativeSessionID || message?.info?.role !== "user" ||
              !validNativeID(message?.info?.id, "msg_")) continue;
          if (message?.parts?.some((part) => part?.type === "text" && part?.text === text)) nativeMessageID = message.info.id;
        }
        if (!nativeMessageID) await new Promise((resolve) => setTimeout(resolve, Math.min(DELIVERY_POLL_MS, Math.max(0, deadline - Date.now()))));
      }
      if (!nativeMessageID) throw new Error("Kilo /tui submit lacked exact native acceptance evidence");
      live.acceptMessage(payload.messageID, { native_message_id: nativeMessageID });
    } catch (error) {
      live.rejectMessage(payload.messageID, nativeSubmitAttempted ? "native delivery outcome is unknown" : error?.message ?? error);
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
          let nativeTitle = eventNativeTitle(info?.title);
          if (nativeTitle === undefined) break;
          if (event.type === "session.created" && requestedName && nativeTitle !== requestedName) {
            const updated = requireSDKSuccess(await client.session.update({
              path: { id: sessionID },
              query: { directory },
              body: { title: requestedName },
            }), 200);
            nativeTitle = eventNativeTitle(updated?.title);
            if (nativeTitle !== requestedName) throw new Error("Kilo did not confirm the requested native session title");
          }
          const prior = known.get(sessionID);
          known.set(sessionID, { title: nativeTitle, cwd: info?.directory ?? directory });
          if (!prior) {
            live.report(sessionID, nativeTitle, { cwd: info?.directory ?? directory });
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
        live.report(sessionID, input.title, { cwd: input.cwd ?? directory });
        known.set(sessionID, { title: input.title, cwd: input.cwd ?? directory });
      }
    },

    tool: {
      agent_sessions: tool({
        description: "Use an exact managed Agent Sessions operation.",
        args: {
          operation: tool.schema.enum(OPERATIONS),
          arguments: tool.schema.record(tool.schema.string(), tool.schema.any()).default({}),
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
