"use strict";

const net = require("node:net");
const { ACTIONS, Caller } = require("./caller.js");
const { Connection, ProtocolError } = require("./connection.js");
const { schema, validate } = require("./schema.js");

const ENV = ["AGENTBUS_LAUNCH_TOKEN", "AGENTBUS_LOCAL_KEY", "AGENTBUS_SOCKET"];
const never = new Promise(() => {});

class Run {
  constructor(parent) {
    this.Native = null;
    this.interrupted = false;
    this.controller = new AbortController();
    parent?.addEventListener("abort", () => this.controller.abort(parent.reason), { once: true });
    this.Done = new Promise((resolve) => { this.finish = resolve; });
  }
  Interrupted() { return this.interrupted; }
}

class Worker {
  constructor(callbacks, env = process.env, options = {}) {
    this.callbacks = callbacks;
    this.env = env;
    this.connect = options.connect || ((path) => net.createConnection(path));
    this.controller = new AbortController();
    this.run = null;
    this.opened = false;
    this.closed = new Promise((resolve) => { this.finish = resolve; });
    this.caller = new Caller(this, options.caller);
  }
  async serve() {
    let cause;
    try {
      const { socket, token } = environment(this.env, true);
      const hello = await this.callbacks.hello(this.controller.signal);
      const stream = this.connect(socket);
      this.connection = new Connection(stream, true, (request) => this._handle(request));
      this.connection.signal.addEventListener("abort", () => this.controller.abort(this.connection.signal.reason), { once: true });
      await this.connection.call("session.hello", { protocol: 1, launch_token: token, ...hello }, this.controller.signal); cause = await this.connection.done; throw cause;
    } finally { this.run?.finish?.(); try { await this._closeProduct(); } finally { this.finish(cause); } }
  }
  call(method, params, signal = this.controller.signal) { return this.connection.call(method, params, signal); }
  shutdown() { this.connection?.close(); }
  _handle(request) {
    if (request.method === "session.superseded") { void this.connection.result(request, {}).finally(() => this.shutdown()); return; }
    if (request.method === "session.open") { queueMicrotask(() => void this._open(request)); return; }
    if (request.method === "turn.run") {
      if (!this.opened || this.run) { void this.connection.error(request, -32003); return; }
      const run = new Run(this.controller.signal); this.run = run; queueMicrotask(() => void this._run(request, run)); return;
    }
    if (request.method === "turn.interrupt") {
      if (!this.run) { void this.connection.error(request, -32004); return; }
      if (!this.run.Done) { void this.connection.result(request, {}); return; }
      if (this.run.controller.signal.aborted) { void this.connection.error(request, -32004); return; }
      const run = this.run, call = !run.interrupted; run.interrupted = true; queueMicrotask(() => void this._interrupt(request, run, call)); return;
    }
    if (request.method === "message.deliver") { if (this.run && !this.run.Done) void this.connection.result(request, { disposition: "rejected", reason: "closing" }); else queueMicrotask(() => void this._deliver(request)); return; }
    if (request.method === "session.close") {
      const run = this.run;
      if (run && !run.Done) return this.shutdown();
      this.run = {};
      const call = run && !run.interrupted;
      if (run) run.interrupted = true;
      queueMicrotask(() => void this._close(request, run, call));
    }
  }
  async _open(request) {
    let result; try { result = await this.callbacks.open(this.controller.signal, request.params); } catch (error) { await this._replyError(request, -32009, { stderr_tail: [clean(error)] }); return; }
    this.opened = true; try { await this.connection.result(request, result); } catch { this.shutdown(); }
  }
  async _run(request, run) {
    let result; try { result = await this.callbacks.run(run.controller.signal, run, request.params.input); } catch (error) { result = { outcome: "failed", result: clean(error) }; }
    run.controller.abort(); result = terminal(result);
    try { await this.connection.result(request, result); if (this.run === run) this.run = null; } catch { this.shutdown(); } finally { run.finish(); }
  }
  async _interrupt(request, run, call) {
    try { if (call && !run.controller.signal.aborted) await this.callbacks.interrupt(run.controller.signal, run); } catch (error) { callbackError("interrupt", error); }
    try { await this.connection.result(request, {}); } catch { this.shutdown(); }
  }
  async _deliver(request) {
    let receipt; try { receipt = await this.callbacks.deliver(this.controller.signal, request.params); } catch (error) { receipt = { disposition: "rejected", reason: clean(error) }; }
    try { await this.connection.result(request, receipt); } catch { this.shutdown(); }
  }
  async _close(request, run, interrupt) {
    if (interrupt) void Promise.resolve().then(() => this.callbacks.interrupt(run.controller.signal, run)).catch((error) => callbackError("interrupt", error)); if (run) await run.Done; this.controller.abort();
    await this._closeProduct();
    try { await this.connection.result(request, {}); } catch { this.shutdown(); } finally { this.shutdown(); }
  }
  async _closeProduct() { if (!this.opened || this.closedProduct) return; this.closedProduct = true; try { await this.callbacks.close(this.controller.signal); } catch (error) { callbackError("close", error); } }
  async _replyError(request, code, data) { try { await this.connection.error(request, code, data); } catch { this.shutdown(); } }
}

class Peer {
  constructor(identity, deliver, env, options) {
    this.identity = snapshot(identity);
    this.deliver = deliver;
    this.connect = options.connect || ((path) => net.createConnection(path));
    this.schedule = options.schedule || ((call) => setTimeout(call, 2000));
    this.socket = environment(env, false).socket;
    this.terminal = false;
    this.error = null;
    this.admitted = null;
    this.identityController = null;
    this.closed = new Promise((resolve) => { this.finish = resolve; });
    this.ready = this._open();
    this.caller = new Caller(this, options.caller);
  }
  call(method, params, signal) { if (!this.connection) return Promise.reject(new Error("not connected")); return this.connection.call(method, params, signal); }
  rehello(identity) { return this._change(identity, false); }
  replace(identity) { return this._change(identity, true); }
  async _change(update, replacement) {
    if (this.terminal) throw new Error("superseded");
    const object = update && typeof update === "object" && !Array.isArray(update), desired = replacement ? update : { ...this.identity, name: update?.name, info: update?.info };
    if (!object || !replacement && (Object.keys(update).length !== 2 || !Object.hasOwn(update, "name") || !Object.hasOwn(update, "info")) || !validate("SessionHelloRequest", { protocol: 1, ...desired }) || replacement && update.session_id === this.identity.session_id) throw new Error(`invalid ${replacement ? "replace" : "rehello"} identity`);
    const connection = this.connection;
    this.identity = snapshot(desired);
    const identity = this.identity;
    if (replacement) { this.identityController?.abort(new Error("not connected")); this.identityController = null; this.admitted = null; this.connection = null; this.caller.disconnected(); }
    if (!connection || connection.signal.aborted) throw new Error("not connected");
    try { const result = await this._hello(connection, identity); if (replacement && (this.wire !== connection || connection.signal.aborted)) throw new Error("not connected"); return result; }
    catch (error) { if (this._failHello(error, connection)) throw error; if (connection.signal.aborted) throw new Error("not connected"); if (replacement) connection.close(); throw error; }
  }
  shutdown() { this.terminal = true; this.connection = null; this.identityController?.abort(new Error("not connected")); this.caller.disconnected(); this.wire?.close(); this.finish(); }
  async _open() {
    if (this.terminal) return; let connection; try { connection = new Connection(this.connect(this.socket), true, (request) => this._handle(request, connection)); } catch { this.schedule(() => { this.ready = this._open(); }, 2000); return; } this.wire = connection;
    try { await this._hello(connection); } catch (error) { if (this._failHello(error, connection)) return never; connection.close(); }
    if (!connection.signal.aborted) void connection.done.then(() => this._lost(connection)); else this._lost(connection);
  }
  async _hello(connection, identity = this.identity) { for (;;) { let installed = false; const result = await connection.call("session.hello", { protocol: 1, ...identity }, undefined, () => {
    if (this.wire !== connection || this.terminal) throw new Error("not connected");
    if (identity === this.identity) { if (this.admitted?.session_id !== identity.session_id || !this.identityController || this.identityController.signal.aborted) this.identityController = new AbortController(); this.admitted = identity; this.connection = connection; installed = true; }
  }); if (installed) return result; identity = this.identity; } }
  _lost(connection) { if (this.wire !== connection || this.terminal) return; this.wire = null; this.connection = null; this.admitted = null; this.identityController?.abort(connection.signal.reason); this.identityController = null; this.caller.disconnected(); this.schedule(() => { this.ready = this._open(); }, 2000); }
  _failHello(error, connection) { if (!(error instanceof ProtocolError) || error.code !== -32602) return false; this.terminal = true; this.error = error; this.wire = null; this.connection = null; this.identityController?.abort(error); this.identityController = null; this.admitted = null; this.caller.disconnected(); connection.close(); this.finish(); return true; }
  _handle(request, connection) {
    if (request.method === "session.superseded") { this.terminal = true; this.error = new ProtocolError({ code: -32012, message: "superseded" }); this.connection = null; this.caller.disconnected(); void connection.result(request, {}).finally(() => { this.identityController?.abort(this.error); connection.close(); this.finish(); }); return; }
    if (request.method === "message.deliver") { const current = this.connection === connection && this.admitted && this.identityController && !this.identityController.signal.aborted; const admission = current ? { identity: snapshot(this.admitted), signal: this.identityController.signal } : null; void Promise.resolve().then(() => admission ? this.deliver(admission.signal, request.params, admission.identity) : { disposition: "rejected", reason: "closing" }).then((result) => connection.result(request, result), (error) => connection.result(request, { disposition: "rejected", reason: clean(error) })).catch(() => connection.close()); }
  }
}

function connectPeer(identity, deliver, env = process.env, options = {}) { return new Peer(identity, deliver, env, options); }
function serveWorker(callbacks, env = process.env, options = {}) { const worker = new Worker(callbacks, env, options); worker.serving = worker.serve().catch((error) => error); return worker; }
function snapshot(identity) { return { ...identity, groups: [...identity.groups], info: structuredClone(identity.info) }; }
function environment(env, worker) { const values = Object.fromEntries(ENV.map((name) => [name, env[name]])); for (const name of ENV) delete env[name]; if (!values.AGENTBUS_SOCKET) throw new Error("agentbus socket is required"); if (values.AGENTBUS_LOCAL_KEY) throw new Error("local key transport not implemented in this build"); if (worker && !values.AGENTBUS_LAUNCH_TOKEN) throw new Error("launch token is required"); return { socket: values.AGENTBUS_SOCKET, token: values.AGENTBUS_LAUNCH_TOKEN }; }
function terminal(result = {}) { if (!result || typeof result !== "object") return result; if (!Object.hasOwn(result, "result")) result = { ...result, result: "" }; if (typeof result.result !== "string") return result; const characters = [...result.result]; return { ...result, result: characters.slice(0, 262144).join(""), ...(characters.length > 262144 ? { truncated: true } : {}) }; }
function clean(error) { return String(error?.message || error || "product callback failed"); }
function callbackError(callback, error) { if (error) process.stderr.write(`agentbus: product ${callback}: ${JSON.stringify(clean(error))}\n`); }

module.exports = { ACTIONS, Caller, connectPeer, Connection, ENV, Peer, ProtocolError, Run, serveWorker, Worker, schema, validate };
