"use strict";

const { EventEmitter } = require("node:events");
const net = require("node:net");
const { DEFAULT_LIMITS, FrameDecoder, encodeFrame, makeFrame, redact } = require("./protocol.js");

const ENV = Object.freeze({
  socket: "AGENT_SESSIONS_COMPONENT_SOCKET",
  product: "AGENT_SESSIONS_PRODUCT_ID",
  attachment: "AGENT_SESSIONS_ATTACHMENT_ID",
  capabilityID: "AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID",
  bootstrapValue: "AGENT_SESSIONS_BOOTSTRAP_VALUE",
  processStart: "AGENT_SESSIONS_PROCESS_START",
  strongStart: "AGENT_SESSIONS_STRONG_START",
  componentVersion: "AGENT_SESSIONS_COMPONENT_VERSION",
});

const NON_JOURNALED_TYPES = new Set(["bootstrap", "reconnect", "heartbeat", "heartbeat.ack", "reject"]);

class InactiveError extends Error {
  constructor(reason) {
    super(`Agent Sessions component is inactive: ${reason}`);
    this.name = "InactiveError";
    this.category = "inactive";
  }
}

class ComponentClient extends EventEmitter {
  constructor(options = {}) {
    super();
    this.env = options.env ?? process.env;
    this.connect = options.connect ?? ((socketPath) => net.createConnection(socketPath));
    this.limits = { ...DEFAULT_LIMITS, ...(options.limits ?? {}) };
    if (this.limits.maxFrameBytes > DEFAULT_LIMITS.maxFrameBytes ||
        this.limits.maxNesting > DEFAULT_LIMITS.maxNesting ||
        this.limits.maxStringBytes > DEFAULT_LIMITS.maxStringBytes) {
      throw new Error("component client limits cannot widen protocol defaults");
    }
    this.maxQueue = boundedOption(options.maxQueue, 128, 1, 4096, "maxQueue");
    this.maxJournal = boundedOption(options.maxJournal, this.maxQueue, 1, this.maxQueue, "maxJournal");
    this.maxOutstanding = boundedOption(options.maxOutstanding, 128, 1, 4096, "maxOutstanding");
    this.heartbeatGrace = boundedOption(options.heartbeatGrace, 3, 1, 10, "heartbeatGrace");
    this.reconnectMinMs = boundedOption(options.reconnectMinMs, 50, 1, 60000, "reconnectMinMs");
    this.reconnectMaxMs = boundedOption(options.reconnectMaxMs, 2000, this.reconnectMinMs, 60000, "reconnectMaxMs");

    const gate = readBootstrap(this.env);
    this.active = gate.active;
    this.inactiveReason = gate.reason;
    this.socketPath = gate.socketPath;
    this.productID = gate.productID;
    this.attachmentID = gate.attachmentID;
    this.capabilityID = gate.capabilityID;
    this.bootstrapValue = gate.bootstrapValue;
    this.processStart = gate.processStart;
    this.strongStart = gate.strongStart;
    this.componentVersion = gate.componentVersion;

    this.socket = null;
    this.decoder = null;
    this.ready = false;
    this.everReady = false;
    this.stopping = false;
    this.connecting = false;
    this.bindingID = "";
    this.daemonGeneration = 0;
    this.priorBindingID = "";
    this.priorGeneration = 0;
    this.outboundSeq = 0;
    this.inboundSeq = 0;
    this.inboundReplay = new Map();
    this.lastReceivedSeq = 0;
    this.queue = [];
    this.outboundJournal = new Map();
    this.journalOrder = 0;
    this.connectionEpoch = 0;
    this.lastOutboundAckSeq = 0;
    this.pendingTools = new Map();
    this.heartbeatTimer = null;
    this.lastHeartbeatAckAt = 0;
    this.reconnectTimer = null;
    this.reconnectDelay = this.reconnectMinMs;
    this.wireBlocked = false;
    this.operationCounter = 0;
    this.startPromise = null;
    this.resolveStart = null;
  }

  start() {
    if (!this.active) return Promise.resolve({ active: false, reason: this.inactiveReason });
    if (this.stopping) return Promise.reject(new Error("component client has stopped"));
    if (this.ready) return Promise.resolve({ active: true, bindingID: this.bindingID, daemonGeneration: this.daemonGeneration });
    if (!this.startPromise) {
      this.startPromise = new Promise((resolve) => { this.resolveStart = resolve; });
      this._open();
    }
    return this.startPromise;
  }

  send(type, id, payload) {
    if (!this.active || this.stopping) return false;
    const operation = snapshotOperation(type, id, payload);
    encodeFrame(makeFrame(operation.type, operation.id, 1, operation.payload), this.limits);
    if (NON_JOURNALED_TYPES.has(type)) {
      if (!this.ready || this.wireBlocked) return false;
      this._writeOperation(operation);
      return true;
    }
    const fingerprint = JSON.stringify({ type: operation.type, payload: operation.payload });
    const existing = this.outboundJournal.get(id);
    if (existing) {
      if (existing.fingerprint !== fingerprint) throw new Error("component operation id conflicts with its outbound journal");
      return true;
    }
    if (this.outboundJournal.size >= this.maxJournal) throw new Error("component outbound journal is full");
    const entry = {
      ...operation,
      fingerprint,
      order: ++this.journalOrder,
      generation: this.ready ? this.daemonGeneration : 0,
      epoch: 0,
      sentSeq: 0,
      queued: false,
    };
    this.outboundJournal.set(id, entry);
    if (this.ready && !this.wireBlocked) {
      this._tryWriteJournalEntry(entry);
      return true;
    }
    this._enqueueJournal(entry);
    return true;
  }

  callTool(callID, operation, argumentsValue) {
    if (!this.active || this.stopping) return Promise.reject(new InactiveError(this.inactiveReason));
    if (this.pendingTools.has(callID)) return Promise.reject(new Error("component tool call id is already outstanding"));
    if (this.pendingTools.size >= this.maxOutstanding) return Promise.reject(new Error("too many component tool calls are outstanding"));
    let resolveCall;
    let rejectCall;
    const result = new Promise((resolve, reject) => {
      resolveCall = resolve;
      rejectCall = reject;
    });
    const operationFrame = { type: "tool.call", id: callID, payload: { call_id: callID, operation, arguments: argumentsValue ?? {} } };
    this.pendingTools.set(callID, { resolve: resolveCall, reject: rejectCall });
    try {
      this.send(operationFrame.type, operationFrame.id, operationFrame.payload);
    } catch (error) {
      this.pendingTools.delete(callID);
      rejectCall(error);
    }
    return result;
  }

  async stop() {
    if (this.stopping) return;
    this.stopping = true;
    this.ready = false;
    clearTimeout(this.reconnectTimer);
    clearInterval(this.heartbeatTimer);
    this.reconnectTimer = null;
    this.heartbeatTimer = null;
    const socket = this.socket;
    this.socket = null;
    if (socket) socket.destroy();
    const error = new Error("component client stopped");
    for (const pending of this.pendingTools.values()) pending.reject(error);
    this.pendingTools.clear();
    this.queue.length = 0;
    this.outboundJournal.clear();
  }

  _open() {
    if (this.stopping || this.connecting || !this.active) return;
    this.connecting = true;
    let socket;
    try {
      socket = this.connect(this.socketPath);
    } catch (error) {
      this.connecting = false;
      this.emit("diagnostic", redact(error.message, this.bootstrapValue));
      this._scheduleReconnect();
      return;
    }
    this.socket = socket;
    this.decoder = new FrameDecoder(this.limits);
    this.connectionEpoch += 1;
    this.outboundSeq = 0;
    this.inboundSeq = 0;
    this.inboundReplay.clear();
    this.lastOutboundAckSeq = 0;
    this.wireBlocked = false;
    socket.once("connect", () => this._onConnect(socket));
    socket.on("data", (chunk) => this._onData(socket, chunk));
    socket.on("drain", () => {
      if (socket !== this.socket) return;
      this.wireBlocked = false;
      this._flush();
    });
    socket.on("error", (error) => this.emit("diagnostic", redact(error.message, this.bootstrapValue)));
    socket.once("close", () => this._onClose(socket));
  }

  _onConnect(socket) {
    if (socket !== this.socket || this.stopping) return;
    this.connecting = false;
    if (this.everReady) {
      this._writeOperation({ type: "reconnect", id: this._nextOperationID("reconnect"), payload: {
        attachment_id: this.attachmentID,
        prior_binding_id: this.priorBindingID,
        prior_generation: this.priorGeneration,
        process_start: this.processStart,
        strong_start: this.strongStart,
        last_received_seq: this.lastReceivedSeq,
      } });
      return;
    }
    this._writeOperation({ type: "bootstrap", id: this._nextOperationID("bootstrap"), payload: {
      product_id: this.productID,
      attachment_id: this.attachmentID,
      bootstrap_capability_id: this.capabilityID,
      bootstrap_value: this.bootstrapValue,
      process_start: this.processStart,
      strong_start: this.strongStart,
      component_version: this.componentVersion,
    } });
    // Remove the raw value from the launch environment immediately. It remains
    // only in this client until the daemon confirms ready, then is discarded.
    delete this.env[ENV.bootstrapValue];
  }

  _onData(socket, chunk) {
    if (socket !== this.socket || this.stopping) return;
    let frames;
    try {
      frames = this.decoder.push(chunk);
    } catch (error) {
      this.emit("diagnostic", redact(error.message, this.bootstrapValue));
      socket.destroy();
      return;
    }
    for (const frame of frames) {
      const admission = this._acceptInbound(frame);
      if (!admission.accepted) {
        socket.destroy();
        return;
      }
      if (admission.duplicate) continue;
      this._handleFrame(frame);
    }
  }

  _acceptInbound(frame) {
    const digest = JSON.stringify(frame);
    if (frame.seq <= this.inboundSeq) {
      return { accepted: this.inboundReplay.get(frame.seq) === digest, duplicate: true };
    }
    if (frame.seq !== this.inboundSeq + 1) {
      this.emit("diagnostic", "component inbound sequence gap");
      return { accepted: false, duplicate: false };
    }
    this.inboundSeq = frame.seq;
    this.lastReceivedSeq = frame.seq;
    this.inboundReplay.set(frame.seq, digest);
    if (frame.seq > 64) this.inboundReplay.delete(frame.seq - 64);
    return { accepted: true, duplicate: false };
  }

  _handleFrame(frame) {
    if (frame.type === "ready") {
      if (!frame.payload || frame.payload.protocol_version !== 1 || frame.payload.attachment_id !== this.attachmentID ||
          typeof frame.payload.binding_id !== "string" || !frame.payload.binding_id ||
          !Number.isSafeInteger(frame.payload.daemon_generation) || frame.payload.daemon_generation <= 0 ||
          !Number.isSafeInteger(frame.payload.max_frame_bytes) || frame.payload.max_frame_bytes <= 0 || frame.payload.max_frame_bytes > this.limits.maxFrameBytes ||
          !Number.isSafeInteger(frame.payload.heartbeat_interval_ms) || frame.payload.heartbeat_interval_ms <= 0) {
        this.emit("diagnostic", "component ready payload is invalid");
        this.socket.destroy();
        return;
      }
      const priorGeneration = this.priorGeneration;
      const reconnecting = this.everReady;
      const sameGeneration = reconnecting && frame.payload.daemon_generation === priorGeneration;
      if (reconnecting && !sameGeneration) this._dropGenerationJournal();
      this.bindingID = frame.payload.binding_id;
      this.daemonGeneration = frame.payload.daemon_generation;
      this.priorBindingID = this.bindingID;
      this.priorGeneration = this.daemonGeneration;
      this.ready = true;
      this.everReady = true;
      this.reconnectDelay = this.reconnectMinMs;
      this.bootstrapValue = "";
      this.capabilityID = "";
      this.limits.maxFrameBytes = frame.payload.max_frame_bytes;
      this.limits.maxStringBytes = Math.min(this.limits.maxStringBytes, this.limits.maxFrameBytes);
      this.decoder.limits.maxFrameBytes = this.limits.maxFrameBytes;
      this.decoder.limits.maxStringBytes = this.limits.maxStringBytes;
      this._prepareJournalForReady();
      this._startHeartbeat(frame.payload.heartbeat_interval_ms);
      this._flush();
      if (this.resolveStart) {
        this.resolveStart({ active: true, bindingID: this.bindingID, daemonGeneration: this.daemonGeneration });
        this.resolveStart = null;
      }
      this.emit("ready", { bindingID: this.bindingID, daemonGeneration: this.daemonGeneration });
      return;
    }
    if (frame.type === "tool.result") {
      this._dropJournal(frame.payload?.call_id);
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
    } else if (frame.type === "heartbeat.ack") {
      if (frame.payload?.binding_id === this.bindingID) {
        const acknowledged = frame.payload.last_received_seq;
        if (Number.isSafeInteger(acknowledged) && acknowledged >= this.lastOutboundAckSeq && acknowledged <= this.outboundSeq) {
          this.lastOutboundAckSeq = acknowledged;
          this.lastHeartbeatAckAt = Date.now();
          this._acknowledgeJournal(acknowledged);
        }
      }
    } else if (frame.type === "generation.retire") {
      if (frame.payload?.binding_id === this.bindingID && frame.payload?.generation === this.daemonGeneration) this.socket.destroy();
    } else if (frame.type === "reject") {
      const operationID = frame.payload?.operation_id;
      const detail = redact(frame.payload?.detail ?? "");
      this._dropJournal(operationID);
      const pending = this.pendingTools.get(operationID);
      if (pending) {
        this.pendingTools.delete(operationID);
        const error = new Error(detail || frame.payload?.category || "component operation rejected");
        error.category = frame.payload?.category ?? "protocol";
        pending.reject(error);
      }
      this.emit("reject", { ...frame.payload, detail });
    }
    this.emit("frame", frame);
    this.emit(frame.type, frame.payload);
  }

  _startHeartbeat(intervalMs) {
    clearInterval(this.heartbeatTimer);
    this.lastHeartbeatAckAt = Date.now();
    this.heartbeatTimer = setInterval(() => {
      if (!this.ready || this.wireBlocked) return;
      if (Date.now() - this.lastHeartbeatAckAt > intervalMs * this.heartbeatGrace) {
        this.emit("diagnostic", "component heartbeat acknowledgment timed out");
        this.socket?.destroy();
        return;
      }
      try {
        this._writeOperation({ type: "heartbeat", id: this._nextOperationID("heartbeat"), payload: {
          binding_id: this.bindingID,
          last_received_seq: this.lastReceivedSeq,
        } });
      } catch (error) {
        this.emit("diagnostic", redact(error.message, this.bootstrapValue));
        this.socket?.destroy();
      }
    }, intervalMs);
    this.heartbeatTimer.unref?.();
  }

  _writeOperation(operation) {
    if (!this.socket || this.socket.destroyed) throw new Error("component socket is unavailable");
    this.outboundSeq += 1;
    const frame = makeFrame(operation.type, operation.id, this.outboundSeq, operation.payload);
    const wire = encodeFrame(frame, this.limits);
    if (!this.socket.write(wire)) this.wireBlocked = true;
    return this.outboundSeq;
  }

  _flush() {
    while (this.ready && !this.wireBlocked && this.queue.length > 0) {
      const entry = this.queue.shift();
      entry.queued = false;
      if (this.outboundJournal.get(entry.id) !== entry) continue;
      if (!this._tryWriteJournalEntry(entry)) break;
    }
  }

  _onClose(socket) {
    if (socket !== this.socket) return;
    this.socket = null;
    this.connecting = false;
    this.ready = false;
    clearInterval(this.heartbeatTimer);
    this.heartbeatTimer = null;
    if (!this.stopping) this._prepareJournalForReconnect();
    if (!this.stopping) this._scheduleReconnect();
  }

  _writeJournalEntry(entry) {
    const operation = this._operationForCurrentBinding(entry);
    entry.sentSeq = this._writeOperation(operation);
    entry.epoch = this.connectionEpoch;
    entry.generation = this.daemonGeneration;
  }

  _tryWriteJournalEntry(entry) {
    try {
      this._writeJournalEntry(entry);
      return true;
    } catch (error) {
      entry.sentSeq = 0;
      entry.epoch = 0;
      this.emit("diagnostic", redact(error.message, this.bootstrapValue));
      this.socket?.destroy();
      return false;
    }
  }

  _operationForCurrentBinding(entry) {
    if (entry.type !== "session.announce" && entry.type !== "session.rebind") return entry;
    return { ...entry, payload: { ...entry.payload, binding_id: this.bindingID } };
  }

  _enqueueJournal(entry) {
    if (entry.queued) return;
    if (this.queue.length >= this.maxQueue) throw new Error("component outbound queue is full");
    entry.queued = true;
    this.queue.push(entry);
  }

  _prepareJournalForReconnect() {
    this.queue.length = 0;
    const entries = [...this.outboundJournal.values()].sort((left, right) => left.order - right.order);
    for (const entry of entries) {
      entry.sentSeq = 0;
      entry.epoch = 0;
      entry.queued = false;
      this._enqueueJournal(entry);
    }
  }

  _prepareJournalForReady() {
    for (const entry of this.outboundJournal.values()) {
      entry.generation = this.daemonGeneration;
    }
    this._prepareJournalForReconnect();
  }

  _acknowledgeJournal(sequence) {
    for (const entry of [...this.outboundJournal.values()]) {
      if (entry.epoch !== this.connectionEpoch || entry.sentSeq === 0 || entry.sentSeq > sequence) continue;
      if (entry.type === "tool.call") continue;
      this._dropJournal(entry.id);
    }
  }

  _dropJournal(operationID) {
    if (typeof operationID !== "string" || operationID === "") return;
    const entry = this.outboundJournal.get(operationID);
    if (!entry) return;
    this.outboundJournal.delete(operationID);
    if (entry.queued) this.queue = this.queue.filter((candidate) => candidate !== entry);
    entry.queued = false;
  }

  _dropGenerationJournal() {
    const error = new Error("component daemon generation changed before operation completion");
    error.category = "generation-changed";
    for (const pending of this.pendingTools.values()) pending.reject(error);
    this.pendingTools.clear();
    this.outboundJournal.clear();
    this.queue.length = 0;
  }

  _scheduleReconnect() {
    if (this.stopping || !this.active || this.reconnectTimer) return;
    const delay = this.reconnectDelay;
    this.reconnectDelay = Math.min(this.reconnectMaxMs, this.reconnectDelay * 2);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this._open();
    }, delay);
    this.reconnectTimer.unref?.();
  }

  _nextOperationID(prefix) {
    this.operationCounter += 1;
    return `${prefix}-${this.operationCounter}`;
  }
}

function readBootstrap(env) {
  const values = {
    socketPath: env[ENV.socket] ?? "",
    productID: env[ENV.product] ?? "",
    attachmentID: env[ENV.attachment] ?? "",
    capabilityID: env[ENV.capabilityID] ?? "",
    bootstrapValue: env[ENV.bootstrapValue] ?? "",
    processStart: env[ENV.processStart] ?? "",
    strongStart: env[ENV.strongStart] ?? "",
    componentVersion: env[ENV.componentVersion] ?? "",
  };
  const checks = [
    ["socketPath", "missing-component-socket"], ["productID", "missing-product-id"],
    ["attachmentID", "missing-attachment-id"], ["capabilityID", "missing-bootstrap-capability-id"],
    ["bootstrapValue", "missing-bootstrap-value"], ["processStart", "missing-process-start"],
    ["strongStart", "missing-strong-start"], ["componentVersion", "missing-component-version"],
  ];
  for (const [field, reason] of checks) {
    if (!values[field]) return { active: false, reason, ...values };
  }
  return { active: true, reason: "managed-bootstrap", ...values };
}

function boundedOption(value, fallback, minimum, maximum, name) {
  const result = value ?? fallback;
  if (!Number.isSafeInteger(result) || result < minimum || result > maximum) throw new Error(`${name} is outside its fixed bound`);
  return result;
}

function snapshotOperation(type, id, payload) {
  let encoded;
  try {
    encoded = JSON.stringify(payload);
  } catch (error) {
    throw new Error(`component operation payload is not JSON: ${redact(error.message)}`);
  }
  if (encoded === undefined) throw new Error("component operation payload is not JSON");
  return { type, id, payload: JSON.parse(encoded) };
}

function createComponentClient(options) {
  return new ComponentClient(options);
}

module.exports = { ComponentClient, ENV, InactiveError, createComponentClient, readBootstrap };
