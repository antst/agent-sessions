import { createHash } from "node:crypto";
import net from "node:net";
import kit from "@sessionbus/kit";
import { createOpencodeClient } from "@opencode-ai/sdk/v2/client";
import { tool } from "@opencode-ai/plugin";

const { ACTIONS, connectPeer, validate } = kit;
const LIMIT = 1024 * 1024;
const CONNECTION_ENV = ["SESSIONBUS_SOCKET", "SESSIONBUS_LOCAL_KEY"];
const ENV = [...CONNECTION_ENV, "SESSIONBUS_LANE_SOCKET", "SESSIONBUS_GROUPS", "SESSIONBUS_SESSION_NAME", "OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD"];

function text(value, maximum = LIMIT) {
  return typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= maximum && !value.includes("\0");
}

function sessionID(value) {
  return typeof value === "string" && value.startsWith("ses") && [...value].length <= 128 && /^[\p{L}\p{M}\p{N}\p{P}\p{S}]+$/u.test(value);
}

function title(value, id) {
  if (typeof value !== "string") throw new Error("OpenCode session title is malformed");
  return value || id;
}

function eventInfo(event) {
  const properties = event?.properties || {};
  return properties.info || properties.session || properties;
}

function eventID(event) {
  const properties = event?.properties || {};
  return properties.info?.id || properties.session?.id || properties.sessionID || "";
}

function render(request) {
  if (!text(request?.from?.session_id, 256) || !text(request?.from?.product, 256)) throw new Error("structured message sender is incomplete");
  const clean = (value) => String(value).replace(/["<>\r\n]/gu, "");
  const name = request.from.name || request.from.session_id;
  const escaped = { "<": "\\u003c", ">": "\\u003e", "&": "\\u0026", "\u2028": "\\u2028", "\u2029": "\\u2029" };
  const metadata = JSON.stringify({ fromProduct: request.from.product, messageId: request.message_id, groups: request.from.groups || [] }).replace(/[<>&\u2028\u2029]/gu, (character) => escaped[character]);
  const body = String(request.body || "").replace(/<\/cross-session-message/giu, "<\\/cross-session-message");
  return `<cross-session-message from="${clean(name)}" from-session="${clean(request.from.session_id)}">\n[sessionbus-metadata: ${metadata}]\n${body}\n</cross-session-message>`;
}

function messageID(value) {
  return `msg_${createHash("sha256").update(String(value)).digest("hex").slice(0, 32)}`;
}

function accepted(result, expected, request) {
  if (!result || result.error != null || result.response?.status !== 200) throw new Error("OpenCode v2 prompt lacked exact native acceptance");
  const value = result.data?.data || result.data;
  if (!Number.isSafeInteger(value?.admittedSeq) || value.admittedSeq < 0 || value.id !== request.id || value.sessionID !== request.sessionID || value.prompt?.text !== request.prompt.text || value.delivery !== expected) {
    throw new Error("OpenCode v2 prompt returned an invalid admission");
  }
  return value;
}

function connectionEnvironment(saved) {
  return Object.fromEntries(CONNECTION_ENV.map((name) => [name, saved[name]]));
}

function privateAction(path, action, argumentsValue, signal) {
  if (signal?.aborted) return Promise.reject(signal.reason || new Error("cancelled"));
  return new Promise((resolve, reject) => {
    const socket = net.createConnection(path);
    let body = Buffer.alloc(0), finished = false;
    const done = (error, value) => {
      if (finished) return;
      finished = true;
      signal?.removeEventListener("abort", abort);
      socket.destroy();
      error ? reject(error) : resolve(value);
    };
    const abort = () => done(signal.reason || new Error("cancelled"));
    signal?.addEventListener("abort", abort, { once: true });
    socket.on("connect", () => socket.write(`${JSON.stringify({ action, arguments: argumentsValue || {} })}\n`));
    socket.on("data", (chunk) => {
      body = Buffer.concat([body, chunk]);
      if (body.length > LIMIT) return done(new Error("sessionbus lane response exceeds 1 MiB"));
    });
    socket.on("end", () => {
      const newline = body.indexOf(10);
      if (newline < 0) return done(new Error("sessionbus lane returned an empty response"));
      if (newline !== body.length - 1) return done(new Error("sessionbus lane returned a trailing frame"));
      let response;
      try { response = JSON.parse(body.subarray(0, newline)); } catch { return done(new Error("sessionbus lane returned malformed JSON")); }
      if (!response || typeof response !== "object" || Array.isArray(response)) return done(new Error("sessionbus lane returned malformed JSON"));
      const hasError = response.error != null, hasResult = Object.hasOwn(response, "result");
      if (hasError === hasResult) return done(new Error("sessionbus lane response must contain exactly one result or error"));
      if (hasError) {
        const error = new Error(response.error.message || "sessionbus lane call failed");
        error.code = response.error.code;
        error.data = response.error.data;
        return done(error);
      }
      done(null, response.result);
    });
    socket.on("error", (error) => done(error));
  });
}

export function createPlugin(dependencies = {}) {
  const defineTool = dependencies.tool || tool;
  const makeV2 = dependencies.createClient || createOpencodeClient;
  const makePeer = dependencies.connectPeer || connectPeer;
  const callLane = dependencies.privateAction || privateAction;
  const onExit = dependencies.onExit || ((callback) => process.once("beforeExit", callback));
  return async function sessionbusOpenCode({ client, directory, serverUrl, environment = process.env }) {
    const saved = Object.fromEntries(ENV.map((name) => [name, environment[name]]));
    for (const name of ENV) delete environment[name];
    const laneSocket = saved.SESSIONBUS_LANE_SOCKET || "";
    if (laneSocket && !text(laneSocket, 4096)) throw new Error("SESSIONBUS_LANE_SOCKET is invalid");
    const groups = JSON.parse(saved.SESSIONBUS_GROUPS || "[]");
    if (!Array.isArray(groups) || groups.some((group) => !text(group, 128))) throw new Error("SESSIONBUS_GROUPS must be a JSON array of names");
    const requestedName = saved.SESSIONBUS_SESSION_NAME || "";
    const headers = {};
    if (Boolean(saved.OPENCODE_SERVER_USERNAME) !== Boolean(saved.OPENCODE_SERVER_PASSWORD)) throw new Error("OpenCode server credentials are incomplete");
    if (saved.OPENCODE_SERVER_USERNAME && saved.OPENCODE_SERVER_PASSWORD) {
      headers.Authorization = `Basic ${Buffer.from(`${saved.OPENCODE_SERVER_USERNAME}:${saved.OPENCODE_SERVER_PASSWORD}`).toString("base64")}`;
    }
    const v2 = dependencies.v2 || makeV2({ baseUrl: String(serverUrl), headers, directory }).v2;
    if (!v2?.session?.prompt) throw new Error("OpenCode v2 session client is unavailable");
    const sessions = new Map();
    const deliver = async (signal, request, identity) => {
      if (signal.aborted) return { disposition: "rejected", reason: "closing" };
      const native = { sessionID: identity.session_id, id: messageID(request.message_id), prompt: { text: render(request) }, delivery: "steer", resume: true };
      accepted(await v2.session.prompt(native, { signal }), "steer", native);
      return { disposition: "injected" };
    };
    const publish = async (event) => {
      const info = eventInfo(event), id = eventID(event);
      if (!sessionID(id) || !Object.hasOwn(info, "title")) return;
      const nativeName = title(info.title, id);
      const name = event.type === "session.created" && requestedName ? requestedName : nativeName;
      const identity = { product: "opencode", session_id: id, name, groups: [...groups], info: { cwd: info.directory || directory } };
      if (!validate("SessionHelloRequest", { protocol: 1, ...identity })) throw new Error("OpenCode peer identity is outside the Sessionbus grammar");
      if (event.type === "session.created" && requestedName && nativeName !== requestedName) {
        const updated = await client.session.update({ path: { id }, query: { directory }, body: { title: requestedName } });
        if (updated?.error != null || updated?.response?.status !== 200 || updated.data?.title !== requestedName) throw new Error("OpenCode did not confirm the requested session title");
      }
      const current = sessions.get(id);
      if (!current) {
        if (!laneSocket) sessions.set(id, { identity, peer: makePeer(identity, deliver, connectionEnvironment(saved)) });
      } else if (current.identity.name !== name || current.identity.info.cwd !== identity.info.cwd) {
        await current.peer.rehello(undefined, name, identity.info);
        current.identity = identity;
      }
    };
    const caller = (id) => laneSocket ? { action: (action, args, signal) => callLane(laneSocket, action, args, signal) } : sessions.get(id)?.peer?.caller;
    const hooks = {
      event: async ({ event }) => {
        if (event?.type === "session.created" || event?.type === "session.updated") await publish(event);
        if (event?.type === "session.deleted") {
          const current = sessions.get(eventID(event));
          current?.peer.shutdown();
          sessions.delete(eventID(event));
        }
      },
      "shell.env": async (input, output) => {
        if (!sessionID(input.sessionID)) throw new Error("shell context omitted the exact OpenCode session identity");
        output.env.SESSIONBUS_SESSION_ID = input.sessionID;
      },
      tool: {
        sessionbus: defineTool({
          description: "List and message sessionbus peers or control product lanes.",
          args: {
            action: defineTool.schema.enum(ACTIONS),
            arguments: defineTool.schema.record(defineTool.schema.string(), defineTool.schema.any()).default({}),
          },
          execute: async ({ action, arguments: args = {} }, context) => {
            if (!sessionID(context.sessionID)) throw new Error("tool context omitted the exact OpenCode session identity");
            const current = caller(context.sessionID);
            if (!current) throw new Error("sessionbus has no live OpenCode peer for this session");
            return JSON.stringify(await current.action(action, args, context.abort));
          },
        }),
      },
    };
    onExit(() => { for (const current of sessions.values()) current.peer.shutdown(); });
    return hooks;
  };
}

export default createPlugin();
