"use strict";

const utf8Decoder = new TextDecoder("utf-8", { fatal: true });

const VERSION = 1;
const CONTRACT_REVISION = "agent-sessions.component.v1-r1";
const DAEMON_RENAME_PREFIX = "daemon.rename.";
const COMPONENT_RENAME_PREFIX = "component.rename.";
const DEFAULT_LIMITS = Object.freeze({
  maxFrameBytes: 1024 * 1024,
  maxNesting: 32,
  maxStringBytes: 256 * 1024,
});

const FRAME_TYPES = new Set([
  "session.announce", "session.rebind", "session.rename", "session.rename.request", "session.state", "session.close", "session.bound",
  "delivery.present", "delivery.accept", "delivery.reject", "turn.event",
  "tool.call", "tool.cancel", "tool.result", "reject",
]);

function makeFrame(type, id, payload) {
  const frame = { version: VERSION, type, id, payload };
  validateFrame(frame);
  return frame;
}

function validateContractRevision(revision) {
  if (revision !== CONTRACT_REVISION) throw new Error(`unsupported component contract revision ${String(revision)}`);
  return true;
}

function validateFrame(frame, limits = DEFAULT_LIMITS) {
  limits = normalizeLimits(limits);
  if (!frame || Array.isArray(frame) || typeof frame !== "object") throw new Error("component frame must be an object");
  if (frame.version !== VERSION) throw new Error(`unsupported component version ${String(frame.version)}`);
  if (!FRAME_TYPES.has(frame.type)) throw new Error(`unknown component frame type ${String(frame.type)}`);
  if (!validID(frame.id)) throw new Error("component frame id is invalid");
  if (!frame.payload || Array.isArray(frame.payload) || typeof frame.payload !== "object") throw new Error("component frame payload must be an object");
  validateJSONBounds(frame, limits);
  validateSessionTitleFrame(frame);
  return frame;
}

function validateSessionTitleFrame(frame) {
  if (frame.type === "session.announce") {
    if (!validID(frame.payload.binding_id) || !validID(frame.payload.native_session_id) ||
        typeof frame.payload.cwd !== "string" || frame.payload.cwd.length === 0 ||
        !validNativeTitleObservation(frame.payload.native_name) ||
        !Number.isSafeInteger(frame.payload.product_event_seq) || frame.payload.product_event_seq <= 0) {
      throw new Error("component session announce title or identity is invalid");
    }
    return;
  }
  if (frame.type === "session.rename.request") {
    if (!frame.id.startsWith(DAEMON_RENAME_PREFIX) || frame.id.length === DAEMON_RENAME_PREFIX.length) {
      throw new Error("component rename request id uses the wrong namespace");
    }
    if (!validID(frame.payload.native_session_id) || !validNativeRenameName(frame.payload.requested_name)) {
      throw new Error("component rename request payload is invalid");
    }
    return;
  }
  if (frame.type !== "session.rename") return;
  const daemonResponse = frame.id.startsWith(DAEMON_RENAME_PREFIX) && frame.id.length > DAEMON_RENAME_PREFIX.length;
  const componentObservation = frame.id.startsWith(COMPONENT_RENAME_PREFIX) && frame.id.length > COMPONENT_RENAME_PREFIX.length;
  const validTitle = daemonResponse ? validNativeRenameName(frame.payload.native_name) :
    componentObservation ? validNativeTitleObservation(frame.payload.native_name) : false;
  if (!validTitle || !validID(frame.payload.native_session_id) ||
      !Number.isSafeInteger(frame.payload.product_event_seq) || frame.payload.product_event_seq <= 0) {
    throw new Error("component rename payload or id namespace is invalid");
  }
}

function encodeFrame(frame, limits = DEFAULT_LIMITS) {
  limits = normalizeLimits(limits);
  validateFrame(frame, limits);
  const body = Buffer.from(JSON.stringify(frame), "utf8");
  if (body.length === 0 || body.length > limits.maxFrameBytes) throw new Error("component frame exceeds configured frame size");
  const header = Buffer.allocUnsafe(4);
  header.writeUInt32BE(body.length, 0);
  return Buffer.concat([header, body]);
}

class FrameDecoder {
  constructor(limits = DEFAULT_LIMITS) {
    this.limits = normalizeLimits(limits);
    this.buffer = Buffer.alloc(0);
    this.expected = null;
  }

  push(chunk) {
    if (!Buffer.isBuffer(chunk)) chunk = Buffer.from(chunk);
    const frames = [];
    let offset = 0;
    const decodeBody = (body) => {
      let frame;
      try {
        frame = JSON.parse(utf8Decoder.decode(body));
      } catch (error) {
        throw new Error(`component frame is invalid JSON: ${redact(error.message)}`);
      }
      frames.push(validateFrame(frame, this.limits));
    };
    while (offset < chunk.length) {
      if (this.expected === null) {
        if (this.buffer.length > 0 || chunk.length - offset < 4) {
          const count = Math.min(4 - this.buffer.length, chunk.length - offset);
          this.buffer = Buffer.concat([this.buffer, chunk.subarray(offset, offset + count)]);
          offset += count;
          if (this.buffer.length < 4) break;
          this.expected = this.buffer.readUInt32BE(0);
          this.buffer = Buffer.alloc(0);
        } else {
          this.expected = chunk.readUInt32BE(offset);
          offset += 4;
        }
        if (this.expected === 0 || this.expected > this.limits.maxFrameBytes) {
          this.buffer = Buffer.alloc(0);
          this.expected = null;
          throw new Error("component frame size is invalid");
        }
      }
      const available = chunk.length - offset;
      if (this.buffer.length === 0 && available >= this.expected) {
        const body = chunk.subarray(offset, offset + this.expected);
        offset += this.expected;
        this.expected = null;
        decodeBody(body);
        continue;
      }
      const needed = this.expected - this.buffer.length;
      const count = Math.min(needed, available);
      if (count > 0) {
        this.buffer = Buffer.concat([this.buffer, chunk.subarray(offset, offset + count)]);
        offset += count;
      }
      if (this.buffer.length < this.expected) break;
      const body = this.buffer;
      this.buffer = Buffer.alloc(0);
      this.expected = null;
      decodeBody(body);
    }
    return frames;
  }
}

function validateJSONBounds(root, limits) {
  const stack = [{ value: root, depth: 1 }];
  while (stack.length > 0) {
    const { value, depth } = stack.pop();
    if (depth > limits.maxNesting) throw new Error("component frame JSON nesting exceeds configured bound");
    if (typeof value === "string") {
      if (Buffer.byteLength(value, "utf8") > limits.maxStringBytes) throw new Error("component frame JSON string exceeds configured bound");
      continue;
    }
    if (!value || typeof value !== "object") continue;
    for (const [key, child] of Object.entries(value)) {
      if (Buffer.byteLength(key, "utf8") > limits.maxStringBytes) throw new Error("component frame JSON key exceeds configured bound");
      if (typeof child === "string") {
        if (Buffer.byteLength(child, "utf8") > limits.maxStringBytes) throw new Error("component frame JSON string exceeds configured bound");
      } else if (child && typeof child === "object") {
        stack.push({ value: child, depth: depth + 1 });
      }
    }
  }
}

function normalizeLimits(limits) {
  const normalized = {
    maxFrameBytes: limits.maxFrameBytes ?? DEFAULT_LIMITS.maxFrameBytes,
    maxNesting: limits.maxNesting ?? DEFAULT_LIMITS.maxNesting,
    maxStringBytes: limits.maxStringBytes ?? Math.min(DEFAULT_LIMITS.maxStringBytes, limits.maxFrameBytes ?? DEFAULT_LIMITS.maxFrameBytes),
  };
  if (!Number.isSafeInteger(normalized.maxFrameBytes) || normalized.maxFrameBytes <= 0 ||
      !Number.isSafeInteger(normalized.maxNesting) || normalized.maxNesting <= 0 ||
      !Number.isSafeInteger(normalized.maxStringBytes) || normalized.maxStringBytes <= 0 ||
      normalized.maxStringBytes > normalized.maxFrameBytes) {
    throw new Error("component protocol limits are invalid");
  }
  return normalized;
}

function validID(value) {
  return typeof value === "string" && value.trim() === value && value.length > 0 && Buffer.byteLength(value, "utf8") <= 256 && !/[\0\r\n]/u.test(value);
}

function validNativeTitleObservation(value) {
  return typeof value === "string" && Buffer.from(value, "utf8").toString("utf8") === value &&
    Buffer.byteLength(value, "utf8") <= 1024 && !/\p{Cc}/u.test(value);
}

function validNativeRenameName(value) {
  return validNativeTitleObservation(value) && value.length > 0 && value.trim() === value;
}

function daemonRenameOperationID(stableID) {
  if (!validID(stableID) || stableID.startsWith(DAEMON_RENAME_PREFIX) || stableID.startsWith(COMPONENT_RENAME_PREFIX)) {
    throw new Error("stable rename operation id is invalid or already namespaced");
  }
  const value = `${DAEMON_RENAME_PREFIX}${stableID}`;
  if (!validID(value)) throw new Error("daemon rename operation id exceeds its bound");
  return value;
}

function componentRenameObservationID(nativeEventID) {
  if (!validID(nativeEventID) || nativeEventID.startsWith(DAEMON_RENAME_PREFIX) || nativeEventID.startsWith(COMPONENT_RENAME_PREFIX)) {
    throw new Error("native rename event id is invalid or already namespaced");
  }
  const value = `${COMPONENT_RENAME_PREFIX}${nativeEventID}`;
  if (!validID(value)) throw new Error("component rename observation id exceeds its bound");
  return value;
}

function redact(detail, ...secrets) {
  let value = String(detail ?? "").replaceAll("\0", "");
  for (const secret of secrets) {
    if (secret) value = value.split(String(secret)).join("<redacted>");
  }
  value = value.replace(/(password|secret|token)(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)/giu, "$1$2<redacted>");
  const bytes = Buffer.from(value, "utf8");
  if (bytes.length <= 512) return value;
  let end = 512;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end -= 1;
  return bytes.subarray(0, end).toString("utf8");
}

module.exports = {
  VERSION,
  CONTRACT_REVISION,
  DAEMON_RENAME_PREFIX,
  COMPONENT_RENAME_PREFIX,
  DEFAULT_LIMITS,
  FRAME_TYPES,
  FrameDecoder,
  encodeFrame,
  makeFrame,
  daemonRenameOperationID,
  componentRenameObservationID,
  validNativeTitleObservation,
  redact,
  validateFrame,
  validateContractRevision,
};
