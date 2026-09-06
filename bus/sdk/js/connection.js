"use strict";

const { isUtf8 } = require("node:buffer");
const METHODS = Object.freeze({
  "session.hello": ["SessionHelloRequest", "SessionHelloResult", "client"], "session.superseded": ["SessionSupersededRequest", "SessionSupersededResult", "daemon"],
  "session.list": ["SessionListRequest", "SessionListResult", "client"], "message.send": ["MessageSendRequest", "MessageSendResult", "client"],
  "message.deliver": ["MessageDeliverRequest", "MessageDeliverResult", "daemon"], "lane.describe": ["LaneDescribeRequest", "LaneDescribeResult", "client"],
  "lane.spawn": ["LaneSpawnRequest", "LaneSpawnResult", "client"], "session.open": ["SessionOpenRequest", "SessionOpenResult", "daemon"],
  "turn.run": ["TurnRunRequest", "TurnRunResult", "both"], "turn.interrupt": ["TurnInterruptRequest", "TurnInterruptResult", "both"], "session.close": ["SessionCloseRequest", "SessionCloseResult", "both"],
});
const MESSAGES = Object.freeze({
  "-32600": "invalid_frame", "-32602": "invalid_hello", "-32603": "internal", "-32001": "unknown_session", "-32002": "not_connected", "-32003": "busy", "-32004": "not_running", "-32005": "already_connected", "-32007": "unknown_product", "-32008": "unsupported_open_field", "-32009": "spawn_failed", "-32010": "timeout", "-32011": "not_committed", "-32012": "superseded", "-32013": "name_taken", "-32014": "unknown_host", "-32015": "forward_lost",
});
const { encode, validate } = require("./schema.js");

class ProtocolError extends Error {
  constructor(value) { super(value.message); this.code = value.code; this.data = value.data; }
}

class Connection {
  constructor(stream, client, handler = () => {}) {
    this.stream = stream; this.client = client; this.handler = handler; this.buffer = Buffer.alloc(0); this.pending = new Map(); this.next = 0; this.last = 0; this.controller = new AbortController();
    this.done = new Promise((resolve) => { this.finish = resolve; });
    stream.on("data", (chunk) => this._data(chunk)); stream.on("error", (error) => this.close(error)); stream.on("close", () => this.close());
  }
  get signal() { return this.controller.signal; }
  async call(method, params, signal, observed) {
    const spec = METHODS[method]; if (!spec) throw new Error("invalid method"); if (signal?.aborted) throw signal.reason || new Error("aborted"); const id = ++this.next; if (!Number.isSafeInteger(id)) throw new Error("request id space exhausted");
    let accept, reject; const result = new Promise((yes, no) => { accept = yes; reject = no; }); this.pending.set(id, { method, accept, reject, observed });
    const response = result.then((value) => [true, value], (error) => [false, error]);
    const abort = () => { if (this.pending.delete(id)) reject(signal.reason || new Error("aborted")); }; signal?.addEventListener("abort", abort, { once: true });
    try { await this._send({ jsonrpc: "2.0", id, method, params: JSON.parse(encode(spec[0], params)) }); const [ok, value] = await response; if (!ok) throw value; return value; }
    catch (error) { this.pending.delete(id); throw error; }
    finally { signal?.removeEventListener("abort", abort); }
  }
  result(request, value) { return this._send({ jsonrpc: "2.0", id: request.id, result: JSON.parse(encode(METHODS[request.method][1], value)) }); }
  error(request, code, data) { const value = { code, message: MESSAGES[code], ...(data === undefined ? {} : { data }) }; encode("RPCError", value); return this._send({ jsonrpc: "2.0", id: request.id, error: value }); }
  close(cause = new Error("sessionbus connection closed")) {
    if (this.controller.signal.aborted) return; this.controller.abort(cause); this.stream.destroy?.();
    for (const pending of this.pending.values()) pending.reject(cause); this.pending.clear(); this.finish(cause);
  }
  _send(frame) {
    if (this.signal.aborted) return Promise.reject(this.signal.reason); const body = `${JSON.stringify(frame)}\n`; if (Buffer.byteLength(body) > 1 << 20) return Promise.reject(new Error("frame too large"));
    return new Promise((resolve, reject) => { const fail = (error) => { this.close(error); reject(error); }; try { this.stream.write(body, (error) => error ? fail(error) : resolve()); } catch (error) { fail(error); } });
  }
  _data(chunk) {
    this.buffer = Buffer.concat([this.buffer, Buffer.from(chunk)]);
    for (let newline; (newline = this.buffer.indexOf(10)) >= 0;) { const line = this.buffer.subarray(0, newline); this.buffer = this.buffer.subarray(newline + 1); if (line.length + 1 > 1 << 20 || !isUtf8(line) || !this._receive(line.toString())) return this.close(); if (this.rejected) return; }
    if (this.buffer.length + 1 > 1 << 20) this.close();
  }
  _receive(line) {
    let frame; try { frame = JSON.parse(line); } catch { return false; } const request = frame && (Object.hasOwn(frame, "method") || Object.hasOwn(frame, "params"));
    if (!frame || frame.jsonrpc !== "2.0" || !Number.isSafeInteger(frame.id) || frame.id < 1 || !exact(frame, request ? ["jsonrpc", "id", "method", "params"] : Object.hasOwn(frame, "result") ? ["jsonrpc", "id", "result"] : ["jsonrpc", "id", "error"])) return this._reject(frame, request);
    if (request) {
      const spec = METHODS[frame.method], allowed = spec && (spec[2] === "both" || spec[2] === (this.client ? "daemon" : "client"));
      if (frame.id <= this.last || !allowed || !validate(spec[0], frame.params)) return this._reject(frame, true); this.last = frame.id; this.handler({ id: frame.id, method: frame.method, params: frame.params }); return true;
    }
    const pending = this.pending.get(frame.id); if (!pending) return false; this.pending.delete(frame.id);
    if (!(frame.error ? validate("RPCError", frame.error) : validate(METHODS[pending.method][1], frame.result))) { pending.reject(new Error("invalid frame")); return false; }
    try { if (!frame.error) pending.observed?.(); } catch (error) { pending.reject(error); return false; }
    (frame.error ? pending.reject : pending.accept)(frame.error ? new ProtocolError(frame.error) : frame.result);
    return true;
  }
  _reject(frame, request) {
    if (!request || !Number.isSafeInteger(frame?.id) || frame.id < 1) return false; const method = typeof frame.method === "string" ? frame.method : "";
    this.rejected = true; void this.error({ id: frame.id, method }, method === "session.hello" ? -32602 : -32600).finally(() => this.close()); return true;
  }
}

function exact(value, keys) { return value && Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key)); }

module.exports = { Connection, METHODS, ProtocolError };
