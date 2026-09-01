"use strict";

const { EventEmitter } = require("node:events");
const net = require("node:net");
const {
  DAEMON_RENAME_PREFIX,
  CONTRACT_REVISION,
  DEFAULT_LIMITS,
  FrameDecoder,
  componentRenameObservationID,
  encodeFrame,
  makeFrame,
  redact,
} = require("./protocol.js");

const ENV = Object.freeze({
  socket: "AGENT_SESSIONS_COMPONENT_SOCKET",
  product: "AGENT_SESSIONS_PRODUCT_ID",
  attachment: "AGENT_SESSIONS_ATTACHMENT_ID",
  componentVersion: "AGENT_SESSIONS_COMPONENT_VERSION",
});

class InactiveError extends Error {
  constructor(reason) {
    super(`Agent Sessions component is inactive: ${reason}`);
    this.name = "InactiveError";
    this.category = "inactive";
  }
}

// ComponentClient is one live socket connection. A disconnect rejects current
// calls; reconnect starts a fresh connection and product integrations report
// their current sessions again.
class ComponentClient extends EventEmitter {
  constructor(options = {}) {
    super();
    this.env = options.env ?? process.env;
    this.connect = options.connect ?? ((socketPath) => net.createConnection(socketPath));
    this.limits = { ...DEFAULT_LIMITS, ...(options.limits ?? {}) };
    this.reconnectMs = boundedOption(options.reconnectMs ?? options.reconnectMinMs, 100, 1, 60000, "reconnectMs");
    this.renameSession = typeof options.renameSession === "function" ? options.renameSession : null;

    const gate = readConfiguration(this.env);
    this.active = gate.active;
    this.inactiveReason = gate.reason;
    this.socketPath = gate.socketPath;
    this.productID = gate.productID;
    this.attachmentID = gate.attachmentID;
    this.componentVersion = gate.componentVersion;

    this.bindingID = this.attachmentID;
    this.daemonGeneration = 0;
    this.socket = null;
    this.decoder = null;
    this.ready = false;
    this.connecting = false;
    this.stopping = false;
    this.pendingTools = new Map();
    this.pendingRenames = new Map();
    this.reconnectTimer = null;
    this.startPromise = null;
    this.resolveStart = null;
  }

  start() {
    if (!this.active) return Promise.resolve({ active: false, reason: this.inactiveReason });
    if (this.stopping) return Promise.reject(new Error("component client has stopped"));
    if (this.ready) return Promise.resolve(this._activation());
    if (!this.startPromise) {
      this.startPromise = new Promise((resolve) => { this.resolveStart = resolve; });
      this._open();
    }
    return this.startPromise;
  }

  send(type, id, payload) {
    if (!this.active || this.stopping || !this.ready || !this.socket || this.socket.destroyed) return false;
    if (type === "session.rename.request") throw new Error("session.rename.request is daemon-to-component only");
    if ((type === "session.rename" || type === "reject") && id.startsWith(DAEMON_RENAME_PREFIX)) {
      throw new Error("daemon-correlated rename results require the native rename callback");
    }
    return this._write({ type, id, payload });
  }

  callTool(callID, operation, argumentsValue) {
    if (!this.active || this.stopping || !this.ready) {
      return Promise.reject(new InactiveError(this.inactiveReason || "disconnected"));
    }
    if (this.pendingTools.has(callID)) return Promise.reject(new Error("component tool call id is already outstanding"));
    let resolveCall;
    let rejectCall;
    const result = new Promise((resolve, reject) => {
      resolveCall = resolve;
      rejectCall = reject;
    });
    this.pendingTools.set(callID, { resolve: resolveCall, reject: rejectCall });
    try {
      if (!this.send("tool.call", callID, { call_id: callID, operation, arguments: argumentsValue ?? {} })) {
        throw new InactiveError("disconnected");
      }
    } catch (error) {
      this.pendingTools.delete(callID);
      rejectCall(error);
    }
    return result;
  }

  observeRename(nativeEventID, nativeSessionID, nativeName, productEventSeq) {
    return this.send("session.rename", componentRenameObservationID(nativeEventID), {
      native_session_id: nativeSessionID,
      native_name: nativeName,
      product_event_seq: productEventSeq,
    });
  }

  async stop() {
    if (this.stopping) return;
    this.stopping = true;
    this.ready = false;
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    const socket = this.socket;
    this.socket = null;
    if (socket) socket.destroy();
    this._failPending("component client stopped");
    if (this.resolveStart) this.resolveStart({ active: false, reason: "stopped" });
    this.resolveStart = null;
    this.startPromise = null;
  }

  _activation() {
    return { active: true, bindingID: this.bindingID, daemonGeneration: 0 };
  }

  _open() {
    if (this.stopping || this.connecting || this.ready || !this.active) return;
    this.connecting = true;
    let socket;
    try {
      socket = this.connect(this.socketPath);
    } catch (error) {
      this.connecting = false;
      this.emit("diagnostic", redact(error.message));
      this._scheduleReconnect();
      return;
    }
    this.socket = socket;
    this.decoder = new FrameDecoder(this.limits);
    socket.once("connect", () => this._onConnect(socket));
    socket.on("data", (chunk) => this._onData(socket, chunk));
    socket.on("error", (error) => this.emit("diagnostic", redact(error.message)));
    socket.once("close", () => this._onClose(socket));
  }

  _onConnect(socket) {
    if (socket !== this.socket || this.stopping) return;
    this.connecting = false;
    this.ready = true;
    const activation = this._activation();
    if (this.resolveStart) this.resolveStart(activation);
    this.resolveStart = null;
    this.startPromise = null;
    this.emit("ready", activation);
  }

  _onData(socket, chunk) {
    if (socket !== this.socket || this.stopping) return;
    let frames;
    try {
      frames = this.decoder.push(chunk);
    } catch (error) {
      this.emit("diagnostic", redact(error.message));
      socket.destroy();
      return;
    }
    for (const frame of frames) this._handleFrame(frame);
  }

  _handleFrame(frame) {
    if (frame.type === "tool.result") {
      const pending = this.pendingTools.get(frame.payload?.call_id);
      if (pending) {
        this.pendingTools.delete(frame.payload.call_id);
        if (frame.payload.success) pending.resolve(frame.payload.result);
        else {
          const error = new Error(redact(frame.payload.detail ?? frame.payload.category ?? "component tool call failed"));
          error.category = frame.payload.category ?? "protocol";
          pending.reject(error);
        }
      }
    } else if (frame.type === "reject") {
      const pending = this.pendingTools.get(frame.payload?.operation_id);
      if (pending) {
        this.pendingTools.delete(frame.payload.operation_id);
        const error = new Error(redact(frame.payload.detail ?? frame.payload.category ?? "component operation rejected"));
        error.category = frame.payload.category ?? "protocol";
        pending.reject(error);
      }
    } else if (frame.type === "session.rename.request") {
      this._handleRenameRequest(frame);
      this.emit("frame", frame);
      return;
    }
    this.emit("frame", frame);
    this.emit(frame.type, frame.payload);
  }

  _handleRenameRequest(frame) {
    if (!frame.id.startsWith(DAEMON_RENAME_PREFIX) || this.pendingRenames.has(frame.id)) {
      this._writeResult("reject", frame.id, {
        operation_id: frame.id,
        category: "protocol",
        detail: "native rename request is invalid or already active",
      });
      return;
    }
    if (!this.renameSession) {
      this._writeResult("reject", frame.id, {
        operation_id: frame.id,
        category: "unsupported",
        detail: "this component has no native rename callback",
      });
      return;
    }
    const controller = new AbortController();
    this.pendingRenames.set(frame.id, controller);
    Promise.resolve().then(() => this.renameSession({
      operationID: frame.id,
      nativeSessionID: frame.payload.native_session_id,
      requestedName: frame.payload.requested_name,
      signal: controller.signal,
    })).then((result) => {
      if (this.pendingRenames.get(frame.id) !== controller) return;
      this.pendingRenames.delete(frame.id);
      if (result?.nativeName !== frame.payload.requested_name || !Number.isSafeInteger(result?.productEventSeq) || result.productEventSeq <= 0) {
        const error = new Error("native rename did not confirm the requested title");
        error.category = "native-rejected";
        throw error;
      }
      this._writeResult("session.rename", frame.id, {
        native_session_id: frame.payload.native_session_id,
        native_name: result.nativeName,
        product_event_seq: result.productEventSeq,
      });
    }).catch((error) => {
      if (this.pendingRenames.get(frame.id) !== controller) return;
      this.pendingRenames.delete(frame.id);
      const allowed = new Set(["unsupported", "unavailable", "native-rejected", "timed-out"]);
      const category = allowed.has(error?.category) ? error.category : "native-rejected";
      this._writeResult("reject", frame.id, {
        operation_id: frame.id,
        category,
        detail: redact(error?.message ?? category),
      });
    });
  }

  _writeResult(type, id, payload) {
    if (!this.ready || !this.socket || this.socket.destroyed) return false;
    try {
      return this._write({ type, id, payload });
    } catch (error) {
      this.emit("diagnostic", redact(error.message));
      this.socket.destroy();
      return false;
    }
  }

  _write(operation) {
    const wire = encodeFrame(makeFrame(operation.type, operation.id, operation.payload), this.limits);
    this.socket.write(wire);
    return true;
  }

  _onClose(socket) {
    if (socket !== this.socket) return;
    this.socket = null;
    this.connecting = false;
    this.ready = false;
    this._failPending("component connection closed");
    if (!this.stopping) this._scheduleReconnect();
  }

  _failPending(detail) {
    const error = new Error(detail);
    error.category = "unavailable";
    for (const pending of this.pendingTools.values()) pending.reject(error);
    this.pendingTools.clear();
    for (const controller of this.pendingRenames.values()) controller.abort();
    this.pendingRenames.clear();
  }

  _scheduleReconnect() {
    if (this.stopping || !this.active || this.reconnectTimer) return;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this._open();
    }, this.reconnectMs);
    this.reconnectTimer.unref?.();
  }
}

function readConfiguration(env) {
  const values = {
    socketPath: env[ENV.socket] ?? "",
    productID: env[ENV.product] ?? "",
    attachmentID: env[ENV.attachment] ?? "",
    componentVersion: env[ENV.componentVersion] ?? "",
  };
  for (const [field, reason] of [
    ["socketPath", "missing-component-socket"],
    ["productID", "missing-product-id"],
    ["attachmentID", "missing-attachment-id"],
    ["componentVersion", "missing-component-version"],
  ]) {
    if (!values[field]) return { active: false, reason, ...values };
  }
  if (values.componentVersion !== CONTRACT_REVISION) {
    return { active: false, reason: "component-contract-mismatch", ...values };
  }
  return { active: true, reason: "managed-live-socket", ...values };
}

function boundedOption(value, fallback, minimum, maximum, name) {
  const result = value ?? fallback;
  if (!Number.isSafeInteger(result) || result < minimum || result > maximum) throw new Error(`${name} is outside its fixed bound`);
  return result;
}

function createComponentClient(options) {
  return new ComponentClient(options);
}

module.exports = { ComponentClient, ENV, InactiveError, createComponentClient, readConfiguration };
