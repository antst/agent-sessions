"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { connectPeer, Connection, ProtocolError, Worker } = require("./index.js");
const { pair, deferred } = require("./test-support.js");

const rows = JSON.parse(fs.readFileSync(path.join(__dirname, "../../internal/protocol/session-lifecycle.fixtures.json"), "utf8"));
const openRequest = { name: "parent/leaf@local", groups: ["session:parent"], resume_session_id: "product-session", open: { cwd: "/tmp", permission_mode: "ask", model: "model", reasoning_effort: "high", arguments: ["--flag"] } };
const target = { session_id: "product-session@local" };
const delivery = { message_id: "m", from: { session_id: "peer@local", name: "peer@local", product: "peer", groups: [] }, body: "body" };

function aborted(signal) { if (signal.aborted) return Promise.resolve(); return new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true })); }
function errorCode(promise, code) { return assert.rejects(promise, (error) => error instanceof ProtocolError && error.code === code); }

class FakeProduct {
  constructor() { this.calls = [0, 0, 0, 0, 0, 0]; }
  hello(_cancel) {
    this.calls[0]++; assert.equal(this.env.AGENTBUS_LAUNCH_TOKEN, undefined); assert.equal(this.env.AGENTBUS_LOCAL_KEY, undefined); assert.equal(this.env.AGENTBUS_SOCKET, undefined);
    return { product: "example-peer", supported_open_fields: [], extra_arguments: [] };
  }
  open(_cancel, request) { this.calls[1]++; assert.deepEqual(request, openRequest); return { session_id: "product-session" }; }
  async run(cancel, run, input) {
    this.calls[2]++;
    if (input === "admission") { assert.equal(this.admitted, true); this.started.resolve(); return { outcome: "completed", result: "" }; }
    if (input === "block") { this.started.resolve(run); await this.release.promise; if (run.Interrupted()) return { outcome: "interrupted", result: "" }; run.Native = "native"; return { outcome: "completed", result: "" }; }
    if (input === "eof") { this.started.resolve(run); await aborted(cancel); throw cancel.reason; }
    if (input === "fail") return Promise.reject(new Error("failed exactly"));
    if (input === "long") return { outcome: "completed", result: "x".repeat(262145) };
    return { outcome: input === "interrupted" ? "interrupted" : "completed", result: input === "completed" || input === "interrupted" ? "" : input };
  }
  async interrupt(cancel, run) { this.calls[3]++; assert.equal(run.Interrupted(), true); this.interrupted?.resolve(); if (this.hangInterrupt) await aborted(cancel); }
  async deliver(cancel) {
    this.calls[4]++; if (this.deliverStart) { this.deliverStart.resolve(); await aborted(cancel); throw cancel.reason; }
    if (this.outbound) await this.worker.caller.list({}); return { disposition: "injected" };
  }
  async close(cancel) { this.calls[5]++; assert.equal(cancel.aborted, true); this.closeStart?.resolve(); if (this.closeEnd) await this.closeEnd.promise; if (this.closeError) throw new Error("close failed"); }
}

test("connection write failure rejects once", async (t) => {
  const [clientSocket, daemonSocket] = pair(); clientSocket.failNext = true; const connection = new Connection(clientSocket, true);
  t.after(() => { connection.close(); daemonSocket.destroy(); });
  await assert.rejects(connection.call("session.list", {}), /write failed/); await Promise.resolve(); assert.equal(connection.pending.size, 0);
});

test("pre-aborted call has no pending request or write", async (t) => {
  const [clientSocket, daemonSocket] = pair(); const connection = new Connection(clientSocket, true); let seen = 0; daemonSocket.on("data", (body) => { seen += body.length; });
  t.after(() => { connection.close(); daemonSocket.destroy(); });
  const cancel = new AbortController(); cancel.abort(new Error("already cancelled")); await assert.rejects(connection.call("session.list", {}, cancel.signal), /already cancelled/);
  assert.equal(seen, 0); assert.equal(connection.pending.size, 0);
});

test("worker closed resolves when product close rejects", async (t) => {
  const product = new FakeProduct(); product.closeError = true; const { worker, daemon, serving } = await harness(t, product); daemon.close();
  await worker.closed; assert.match((await serving).message, /close failed/);
});

test("one chunk is admitted before its first callback", async (t) => {
  const product = new FakeProduct(); product.started = deferred(); const { worker } = await harness(t, product);
  worker.connection.result = () => Promise.resolve(); worker.connection.error = () => { product.admitted = true; return Promise.resolve(); };
  const frame = (id, input) => JSON.stringify({ jsonrpc: "2.0", id, method: "turn.run", params: { ...target, input } }) + "\n";
  worker.connection._data(Buffer.from(frame(2, "admission") + frame(3, "again"))); await product.started.promise;
});

async function harness(t, product, options = {}) {
  const [workerSocket, daemonSocket] = pair(); const hello = deferred(); const env = { AGENTBUS_LAUNCH_TOKEN: "token", AGENTBUS_LOCAL_KEY: "", AGENTBUS_SOCKET: "/fixture/socket" }; product.env = env;
  const worker = new Worker(product, env, { connect: (socket) => { assert.equal(socket, "/fixture/socket"); return workerSocket; } }); product.worker = worker;
  const daemon = new Connection(daemonSocket, false, (request) => {
    if (request.method === "session.hello") { if (options.acknowledge === false) daemon.close(); else void daemon.result(request, {}); hello.resolve(); }
    if (request.method === "session.list") void (worker.opened ? daemon.result(request, { sessions: [] }) : daemon.error(request, -32011));
  });
  const serving = worker.serve().catch((error) => error); await hello.promise;
  t.after(async () => { worker.shutdown(); daemon.close(); await serving; });
  if (options.open !== false && options.acknowledge !== false) assert.deepEqual(await daemon.call("session.open", openRequest), { session_id: "product-session" });
  return { worker, daemon, workerSocket, serving };
}

for (const row of rows) test(`lifecycle: ${row.name}`, async (t) => {
  const product = new FakeProduct(); let result;
  switch (row.name) {
    case "ready-open-commit": {
      const { worker, daemon } = await harness(t, product, { open: false }); await errorCode(worker.call("session.list", {}), -32011); await daemon.call("session.open", openRequest); await worker.call("session.list", {}); break;
    }
    case "describe-eof": { const { serving } = await harness(t, product, { acknowledge: false, open: false }); await serving; break; }
    case "terminal-results": {
      const { daemon } = await harness(t, product); const expected = { ok: ["completed", "ok", false], interrupted: ["interrupted", "", false], fail: ["failed", "failed exactly", false], completed: ["completed", "", false], long: ["completed", "x".repeat(262144), true] };
      for (const [input, want] of Object.entries(expected)) { result = await daemon.call("turn.run", { ...target, input }); assert.deepEqual([result.outcome, result.result, !!result.truncated], want); } break;
    }
    case "one-run": case "one-interrupt": case "full-duplex": case "callback-originated-method": case "close-during-run": case "run-done": {
      product.started = deferred(); product.release = deferred(); const { daemon } = await harness(t, product); const running = daemon.call("turn.run", { ...target, input: "block" }); const run = await product.started.promise;
      if (row.name === "one-run") await errorCode(daemon.call("turn.run", { ...target, input: "again" }), -32003);
      if (row.name === "one-interrupt") { product.interrupted = deferred(); await Promise.all([daemon.call("turn.interrupt", target), daemon.call("turn.interrupt", target)]); await product.interrupted.promise; }
      if (row.name === "full-duplex" || row.name === "callback-originated-method") { product.outbound = true; assert.equal((await daemon.call("message.deliver", delivery)).disposition, "injected"); }
      if (row.name === "run-done") { let done = false; run.Done.then(() => { done = true; }); await Promise.resolve(); assert.equal(done, false); }
      if (row.name === "close-during-run") {
        product.interrupted = deferred(); product.closeStart = deferred(); product.closeEnd = deferred(); product.hangInterrupt = true; const closing = daemon.call("session.close", target); await product.interrupted.promise; await daemon.call("turn.interrupt", target); product.release.resolve(); await running; await product.closeStart.promise; product.closeEnd.resolve(); await closing; break;
      }
      product.release.resolve(); result = await running; await run.Done; if (row.name === "one-interrupt") assert.equal(run.Native, null); break;
    }
    case "eof-during-run": {
      product.started = deferred(); const { daemon, worker, serving } = await harness(t, product); const running = daemon.call("turn.run", { ...target, input: "eof" }); const run = await product.started.promise; daemon.close(); await serving; await assert.rejects(running); await run.Done; assert.equal(worker.controller.signal.aborted, true); break;
    }
    case "peer-lifetime": { await peerLifetime(); const { daemon, serving } = await harness(t, product, { open: false }); await daemon.call("session.superseded", {}); await serving; break; }
    case "wrong-direction-request": { const { daemon, serving } = await harness(t, product, { open: false }); await errorCode(daemon.call("session.hello", { protocol: 1, product: "example-peer", launch_token: "again", supported_open_fields: [], extra_arguments: [] }), -32602); await serving; break; }
    case "terminal-before-interrupt": {
      const { daemon, worker } = await harness(t, product); const entered = deferred(), release = deferred(), result = worker.connection.result.bind(worker.connection); worker.connection.result = async (request, value) => { if (request.method === "turn.run") { entered.resolve(); await release.promise; } return result(request, value); };
      const running = daemon.call("turn.run", { ...target, input: "fail" }); await entered.promise; await errorCode(daemon.call("turn.interrupt", target), -32004); release.resolve(); await running; break;
    }
    case "idle-close-deliver": {
      product.deliverStart = deferred(); product.closeStart = deferred(); product.closeEnd = deferred(); const { daemon } = await harness(t, product); const delivering = daemon.call("message.deliver", delivery); await product.deliverStart.promise; const closing = daemon.call("session.close", target); await product.closeStart.promise; assert.equal((await daemon.call("message.deliver", delivery)).reason, "closing"); await delivering; product.closeEnd.resolve(); await closing; break;
    }
    case "environment": {
      const reads = {}, env = new Proxy({ AGENTBUS_LAUNCH_TOKEN: "token", AGENTBUS_LOCAL_KEY: "key", AGENTBUS_SOCKET: "/fixture/socket" }, { get(object, name) { reads[name] = (reads[name] || 0) + 1; return object[name]; } }); product.env = env;
      const worker = new Worker(product, env); await assert.rejects(worker.serve(), /local key/); assert.deepEqual(reads, { AGENTBUS_LAUNCH_TOKEN: 1, AGENTBUS_LOCAL_KEY: 1, AGENTBUS_SOCKET: 1 }); assert.deepEqual(env, {}); break;
    }
    case "shutdown": { const { worker, serving } = await harness(t, product, { open: false }); worker.Shutdown(); worker.Shutdown(); await serving; break; }
    case "run-done-write-failure": {
      product.started = deferred(); product.release = deferred(); const { daemon, workerSocket, serving } = await harness(t, product); const running = daemon.call("turn.run", { ...target, input: "block" }); const run = await product.started.promise; workerSocket.failNext = true; product.release.resolve(); await serving; await assert.rejects(running); await run.Done; break;
    }
    default: assert.fail(`unknown row ${row.name}`);
  }
  assert.deepEqual(product.calls, row.calls);
});

async function peerLifetime() {
  const connections = [], scheduled = []; let scheduledReady = deferred(); const env = { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }; let currentIdentity, deliveries = 0;
  const identity = { product: "native-product", session_id: "session-1", name: "peer", groups: ["shared"], info: {} };
  const peer = connectPeer(identity, async () => { deliveries++; return { disposition: "injected" }; }, env, { connect: () => { const [client, server] = pair(); const daemon = new Connection(server, false, (request) => {
    if (request.method === "session.hello") { if (currentIdentity?.session_id === request.params.session_id && JSON.stringify(currentIdentity.groups) !== JSON.stringify(request.params.groups)) void daemon.error(request, -32602); else { currentIdentity = request.params; void daemon.result(request, {}); } }
  }); connections.push(daemon); return client; }, schedule: (call, milliseconds) => { scheduled.push({ call, milliseconds }); scheduledReady.resolve(); } });
  await peer.ready; assert.deepEqual(env, {}); identity.groups.push("mutated"); identity.info.changed = true; connections[0].close(); await scheduledReady.promise; await assert.rejects(peer.call("session.list", {}), /not connected/); assert.equal(connections.length, 1); assert.equal(scheduled[0].milliseconds, 2000); scheduledReady = deferred(); scheduled.shift().call(); await peer.ready;
  assert.deepEqual(currentIdentity.groups, ["shared"]); assert.deepEqual(currentIdentity.info, {}); identity.groups.pop(); delete identity.info.changed;
  assert.equal((await connections[1].call("message.deliver", delivery)).disposition, "injected"); assert.equal(deliveries, 1);
  const renamed = { ...identity, name: "renamed", groups: ["shared"], info: {} }; await peer.rehello(renamed); assert.equal(currentIdentity.name, "renamed"); renamed.groups.push("mutated"); renamed.info.changed = true;
  connections[1].close(); await scheduledReady.promise; await assert.rejects(peer.call("session.list", {}), /not connected/); assert.equal(connections.length, 2); scheduled.shift().call(); await peer.ready; assert.deepEqual(currentIdentity.groups, ["shared"]); assert.deepEqual(currentIdentity.info, {});
  await errorCode(peer.rehello({ ...identity, groups: ["changed"] }), -32602); assert.deepEqual(peer.identity.groups, ["shared"]);
  await peer.rehello({ ...identity, session_id: "session-2", name: "other" }); assert.equal(currentIdentity.session_id, "session-2");
  await connections[2].call("session.superseded", {}); await peer.closed; assert.equal(scheduled.length, 0);
}
