"use strict";

const { ProtocolError } = require("./connection.js");
const { validate } = require("./schema.js");

class Caller {
  constructor(connection, options = {}) {
    this.connection = connection;
    this.runs = new Map();
    this.targets = new Map();
    this.next = 0;
    this.schedule = options.schedule || ((call, milliseconds) => {
      const timer = setTimeout(call, milliseconds);
      return () => clearTimeout(timer);
    });
  }

  list(request = {}, cancel) { return this.connection.call("session.list", request, cancel); }
  send(request, cancel) { return this.connection.call("message.send", request, cancel); }
  describe(request, cancel) { return this.connection.call("lane.describe", request, cancel); }
  spawn(request, cancel) { return this.connection.call("lane.spawn", request, cancel); }
  resume(sessionID, cancel) { return this.spawn({ resume_session_id: sessionID }, cancel); }
  run(request, cancel) { return this.connection.call("turn.run", request, cancel); }
  interrupt(request, cancel) { return this.connection.call("turn.interrupt", request, cancel); }
  close(request, cancel) { return this.connection.call("session.close", request, cancel); }

  start(request) {
    if (!validate("TurnRunRequest", request)) throw new Error("invalid TurnRunRequest");
    if (this.targets.has(request.session_id)) throw new ProtocolError({ code: -32003, message: "busy" });
    const id = `t-${++this.next}`;
    let finish;
    const record = { id, session_id: request.session_id, state: "running", settled: new Promise((resolve) => { finish = resolve; }) };
    record.finish = finish;
    this.runs.set(id, record);
    this.targets.set(request.session_id, record);
    Promise.resolve().then(() => this.run(request)).then((result) => this._settle(record, "done", result), (error) => this._settle(record, "unavailable", error instanceof ProtocolError ? `${error.code} ${error.message}` : "result unavailable, lane resumable"));
    return { turn_id: id };
  }

  status(request) {
    if (!localRequest(request, false)) throw new Error("invalid status request");
    const run = this._find(request);
    if (run.state === "running") return this._view(run);
    this.runs.delete(run.id);
    return this._view(run);
  }

  async wait(request) {
    if (!localRequest(request, true)) throw new Error("invalid wait request");
    const run = this._find(request);
    if (run.state !== "running") return this.status({ turn_id: run.id });
    if (request.timeout_ms === undefined) { await run.settled; return this.status({ turn_id: run.id }); }
    return new Promise((resolve, reject) => {
      let settled = false;
      const stop = this.schedule(() => { if (!settled) { settled = true; try { resolve(run.state === "running" ? this._view(run) : this.status({ turn_id: run.id })); } catch (error) { reject(error); } } }, request.timeout_ms);
      run.settled.then(() => { if (!settled) { settled = true; stop?.(); try { resolve(this.status({ turn_id: run.id })); } catch (error) { reject(error); } } });
    });
  }

  _find(request) {
    const run = request && this.runs.get(request.turn_id);
    if (!run) throw new Error("unknown_turn");
    return run;
  }

  _settle(run, state, result) {
    run.state = state;
    run.result = result;
    if (this.targets.get(run.session_id) === run) this.targets.delete(run.session_id);
    run.finish();
  }

  _view(run) {
    const value = { turn_id: run.id, session_id: run.session_id, state: run.state };
    if (run.state === "done") value.result = run.result;
    if (run.state === "unavailable") value.reason = run.result;
    return value;
  }
}

function localRequest(value, timeout) {
  if (!value || typeof value !== "object" || Array.isArray(value) || typeof value.turn_id !== "string" || value.turn_id.length === 0) return false;
  const keys = Object.keys(value);
  return keys.every((key) => key === "turn_id" || timeout && key === "timeout_ms") && keys.length <= (timeout ? 2 : 1) && (value.timeout_ms === undefined || Number.isSafeInteger(value.timeout_ms) && value.timeout_ms >= 0);
}

module.exports = { Caller };
