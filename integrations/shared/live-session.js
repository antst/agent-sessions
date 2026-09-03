"use strict";

const { EventEmitter } = require("node:events");
const net = require("node:net");
const path = require("node:path");

const ENV = Object.freeze({
  socket: "AGENT_SESSIONS_PRESENCE_SOCKET",
  stateRoot: "AGENT_SESSIONS_STATE_ROOT",
  product: "AGENT_SESSIONS_PRODUCT",
  groups: "AGENT_SESSIONS_GROUPS",
});

class InactiveError extends Error {
  constructor(reason) {
    super(`Agent Sessions session is inactive: ${reason}`);
    this.name = "InactiveError";
    this.category = "inactive";
  }
}

// LiveSessionClient keeps one presence socket per native session. The first
// frame is the complete live report; every later frame is a JSON-RPC request
// or response. EOF removes the session and rejects its outstanding work.
class LiveSessionClient extends EventEmitter {
  constructor(options = {}) {
    super();
    this.env = options.env ?? process.env;
    this.connect = options.connect ?? ((socketPath) => net.createConnection(socketPath));
    this.reconnectMs = integer(options.reconnectMs ?? 100, 1, 60000, "reconnectMs");
    const config = readConfiguration(this.env);
    this.active = config.active;
    this.inactiveReason = config.reason;
    this.socketPath = config.socketPath;
    this.productID = config.productID;
    this.groups = config.groups;
    this.stopping = false;
    this.sessions = new Map();
    this.inbound = new Map();
  }

  start() {
    return Promise.resolve(this.active ? { active: true } : { active: false, reason: this.inactiveReason });
  }

  report(nativeSessionID, name, info = {}) {
    if (!this.active || this.stopping || !text(nativeSessionID) || typeof name !== "string") return false;
    const existing = this.sessions.get(nativeSessionID);
    if (existing) return this.update(nativeSessionID, name, info);
    if (!stringMap(info)) return false;
    const session = { id: nativeSessionID, name, info: { ...info }, socket: null, ready: false, buffer: "", pending: new Map(), timer: null };
    this.sessions.set(nativeSessionID, session);
    this._open(session);
    return true;
  }

  updateName(nativeSessionID, name) {
    const session = this.sessions.get(nativeSessionID);
    return this.update(nativeSessionID, name, session?.info ?? {});
  }

  update(nativeSessionID, name, info) {
    const session = this.sessions.get(nativeSessionID);
    if (!session || typeof name !== "string" || !stringMap(info)) return false;
    session.name = name;
    session.info = { ...info };
    if (session.ready) {
      this._call(session, `update-${Date.now()}`, "session.update", { name: session.name, info: { ...session.info } })
        .catch((error) => this.emit("diagnostic", cleanError(error)));
    }
    return true;
  }

  closeSession(nativeSessionID) {
    const session = this.sessions.get(nativeSessionID);
    if (!session) return;
    this.sessions.delete(nativeSessionID);
    clearTimeout(session.timer);
    session.socket?.destroy();
    this._fail(session, "live session closed");
  }

  callTool(nativeSessionID, callID, operation, argumentsValue) {
    const session = this.sessions.get(nativeSessionID);
    if (!session?.ready) return Promise.reject(new InactiveError("disconnected"));
    let params = argumentsValue ?? {};
    if (operation.startsWith("lane.") && !Object.hasOwn(params, "cwd")) {
      params = { ...params, cwd: process.cwd() };
    }
    return this._call(session, callID, operation, params);
  }

  acceptMessage(messageID, result = {}) { return this._answer(messageID, result); }
  rejectMessage(messageID, error) { return this._answer(messageID, null, error); }

  async stop() {
    if (this.stopping) return;
    this.stopping = true;
    for (const id of [...this.sessions.keys()]) this.closeSession(id);
  }

  _open(session) {
    if (this.stopping || this.sessions.get(session.id) !== session || session.socket) return;
    let socket;
    try { socket = this.connect(this.socketPath); } catch (error) {
      this.emit("diagnostic", cleanError(error));
      this._reconnect(session);
      return;
    }
    session.socket = socket;
    socket.setEncoding("utf8");
    socket.once("connect", () => {
      this._call(session, "hello", "session.hello", this._hello(session), true).then(() => {
        if (session.socket !== socket || socket.destroyed) return;
        session.ready = true;
        this.emit("ready", { nativeSessionID: session.id });
      }).catch((error) => {
        this.emit("diagnostic", cleanError(error));
        socket.destroy();
      });
    });
    socket.on("data", (chunk) => this._data(session, chunk));
    socket.on("error", (error) => this.emit("diagnostic", cleanError(error)));
    socket.once("close", () => this._closed(session, socket));
  }

  _hello(session) {
    return { protocol: 1, uuid: session.id, name: session.name, groups: [...this.groups], product: this.productID, info: { ...session.info } };
  }

  _data(session, chunk) {
    session.buffer += chunk;
    if (Buffer.byteLength(session.buffer, "utf8") > 1024 * 1024) return session.socket.destroy();
    for (;;) {
      const newline = session.buffer.indexOf("\n");
      if (newline < 0) return;
      const line = session.buffer.slice(0, newline);
      session.buffer = session.buffer.slice(newline + 1);
      let frame;
      try { frame = JSON.parse(line); } catch { session.socket.destroy(); return; }
      if (!validFrame(frame)) { session.socket.destroy(); return; }
      if (frame.method) {
        if (!session.ready) { session.socket.destroy(); return; }
        this._request(session, frame);
      } else {
        this._response(session, frame);
      }
    }
  }

  _request(session, frame) {
    if (frame.method !== "message.deliver") {
      this._write(session, { jsonrpc: "2.0", id: frame.id, error: { code: -32003, message: "Operation not permitted", data: { method: frame.method } } });
      return;
    }
    if (!validDelivery(frame.params)) {
      this._write(session, { jsonrpc: "2.0", id: frame.id, error: { code: -32602, message: "Invalid params", data: { method: frame.method } } });
      return;
    }
    const messageID = frame.params.message_id;
    this.inbound.set(messageID, { session, wireID: frame.id });
    this.emit("message", { messageID, nativeSessionID: session.id, from: { ...frame.params.from, groups: [...frame.params.from.groups] }, body: frame.params.body });
  }

  _response(session, frame) {
    const pending = session.pending.get(frame.id);
    if (!pending) return;
    session.pending.delete(frame.id);
    if (frame.error) pending.reject(Object.assign(new Error(frame.error.message), { category: "unavailable", code: frame.error.code, data: frame.error.data }));
    else pending.resolve(frame.result ?? {});
  }

  _call(session, id, method, params, handshake = false) {
    const wireID = `session.${id}`;
    if (session.pending.has(wireID)) return Promise.reject(new Error("live call id is already outstanding"));
    return new Promise((resolve, reject) => {
      session.pending.set(wireID, { resolve, reject });
      if (!this._write(session, { jsonrpc: "2.0", id: wireID, method, params }, handshake)) {
        session.pending.delete(wireID);
        reject(new InactiveError("disconnected"));
      }
    });
  }

  _answer(messageID, result, error) {
    const inbound = this.inbound.get(messageID);
    if (!inbound) return false;
    this.inbound.delete(messageID);
    return this._write(inbound.session, error
      ? { jsonrpc: "2.0", id: inbound.wireID, error: wireError(error) }
      : { jsonrpc: "2.0", id: inbound.wireID, result: result ?? {} });
  }

  _write(session, frame, handshake = false) {
    return !!((handshake || session.ready) && session.socket && !session.socket.destroyed &&
      session.socket.write(`${JSON.stringify(frame)}\n`));
  }

  _closed(session, socket) {
    if (session.socket !== socket) return;
    session.socket = null;
    session.ready = false;
    session.buffer = "";
    this._fail(session, "live session disconnected");
    for (const [id, inbound] of this.inbound) if (inbound.session === session) this.inbound.delete(id);
    this._reconnect(session);
  }

  _fail(session, detail) {
    const error = Object.assign(new Error(detail), { category: "unavailable" });
    for (const pending of session.pending.values()) pending.reject(error);
    session.pending.clear();
  }

  _reconnect(session) {
    if (this.stopping || this.sessions.get(session.id) !== session || session.timer) return;
    session.timer = setTimeout(() => { session.timer = null; this._open(session); }, this.reconnectMs);
    session.timer.unref?.();
  }
}

function readConfiguration(env) {
  const socketPath = presenceSocket(env);
  const productID = String(env[ENV.product] ?? "").trim();
  let groups = [];
  try {
    const decoded = JSON.parse(env[ENV.groups] ?? "[]");
    if (!Array.isArray(decoded) || decoded.some((value) => typeof value !== "string")) throw new Error("groups");
    groups = [...decoded];
  } catch { return { active: false, reason: "invalid-groups", socketPath, productID, groups: [] }; }
  if (!socketPath) return { active: false, reason: "missing-presence-socket", socketPath, productID, groups };
  if (!productID) return { active: false, reason: "missing-product-id", socketPath, productID, groups };
  return { active: true, reason: "live-presence", socketPath, productID, groups };
}

function presenceSocket(env) {
  if (env[ENV.socket]) return env[ENV.socket];
  let root = env[ENV.stateRoot];
  if (!root && env.XDG_STATE_HOME) root = path.join(env.XDG_STATE_HOME, "agent-sessions");
  if (!root && env.HOME) root = path.join(env.HOME, ".local", "state", "agent-sessions");
  return root ? path.join(root, "run", "presence.sock") : "";
}

function exactKeys(value, allowed) {
  return !!value && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).every((key) => allowed.includes(key));
}
function stringMap(value) {
  return exactKeys(value, Object.keys(value ?? {})) && Object.values(value).every((item) => typeof item === "string");
}
function validID(value) { return typeof value === "string" || Number.isFinite(value); }
function validError(value) {
  return exactKeys(value, ["code", "message", "data"]) &&
    [-32602, -32001, -32002, -32003, -32004, -32005, -32006].includes(value.code) && typeof value.message === "string";
}
function wireError(value) {
  if (validError(value)) return value;
  const message = String(value?.message ?? value ?? "Product operation failed");
  return { code: -32006, message, data: { detail: message } };
}
function validFrame(frame) {
  if (!exactKeys(frame, ["jsonrpc", "id", "method", "params", "result", "error"]) || frame.jsonrpc !== "2.0" || !validID(frame.id)) return false;
  if (typeof frame.method === "string") {
    return frame.method.length > 0 && ("params" in frame) && !("result" in frame) && !("error" in frame);
  }
  return !("method" in frame) && !("params" in frame) && (("result" in frame) !== ("error" in frame)) && (!("error" in frame) || validError(frame.error));
}
function validDelivery(params) {
  return exactKeys(params, ["message_id", "from", "body"]) && text(params.message_id) && typeof params.body === "string" &&
    exactKeys(params.from, ["uuid", "name", "product", "groups"]) && text(params.from.uuid) && typeof params.from.name === "string" &&
    text(params.from.product) && Array.isArray(params.from.groups) && params.from.groups.every((group) => typeof group === "string");
}
function renderDelivery(payload) {
  if (!payload || !validDelivery({ message_id: payload.messageID, from: payload.from, body: payload.body })) throw new Error("live message delivery is invalid");
  const from = payload.from.name || payload.from.uuid;
  const safeFrom = String(from).replace(/[<>"\r\n]/gu, "");
  const metadata = JSON.stringify({ fromProduct: payload.from.product, messageId: payload.messageID, groups: payload.from.groups });
  return `<cross-session-message from="${safeFrom}" from-session="${payload.from.uuid.replace(/[<>"\r\n]/gu, "")}">\n[codex-peer-metadata: ${metadata}]\n${payload.body.replace(/<\/cross-session-message/giu, "<\\/cross-session-message")}\n</cross-session-message>`;
}
function text(value) { return typeof value === "string" && value.trim() === value && value.length > 0 && !/[\0\r\n]/u.test(value); }
function integer(value, minimum, maximum, name) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) throw new Error(`${name} is outside its bound`);
  return value;
}
function cleanError(error) { return String(error?.message ?? error ?? "live session error").replace(/[\0\r\n]/gu, " ").slice(0, 512); }
function createLiveSessionClient(options) { return new LiveSessionClient(options); }

module.exports = { ENV, InactiveError, LiveSessionClient, createLiveSessionClient, readConfiguration, renderDelivery };
