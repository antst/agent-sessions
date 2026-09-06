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
  async interrupt(cancel, run) { this.calls[3]++; assert.equal(run.Interrupted(), true); this.interrupted?.resolve(); if (this.hangInterrupt) await aborted(cancel); if (this.interruptError) throw this.interruptError; }
  async deliver(cancel) {
    this.calls[4]++; if (this.deliverStart) { this.deliverStart.resolve(); await aborted(cancel); throw cancel.reason; }
    if (this.outbound) await this.worker.caller.list({}); return { disposition: "injected" };
  }
  async close(cancel, request) { this.calls[5]++; this.closeSignal = cancel; this.closeRequest = request; this.closeAborted = cancel.aborted; this.closeStart?.resolve(); if (this.closeEnd) await this.closeEnd.promise; if (this.closeError) throw this.closeError; }
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

test("unmatched response closes the connection", async () => {
  const [clientSocket, daemonSocket] = pair(), connection = new Connection(clientSocket, true);
  daemonSocket.write('{"jsonrpc":"2.0","id":99,"result":{}}\n'); await connection.done;
  await assert.rejects(connection.call("session.list", {}), /closed/);
});

test("worker closed resolves when product close rejects", async (t) => {
  const product = new FakeProduct(); product.closeError = new Error("close failed"); const stderr = captureStderr(t); const { worker, daemon, serving } = await harness(t, product); daemon.close();
  await worker.closed; await serving; assert.equal(product.calls[5], 1); assert.equal(stderr(), 'agentbus: product close: "close failed"\n');
});

test("worker waits for orderly product close across EOF", async (t) => {
  const product = new FakeProduct(); product.started = deferred(); product.release = deferred(); product.interrupted = deferred(); product.closeStart = deferred(); product.closeEnd = deferred(); const { worker, daemon, serving } = await harness(t, product); let closed = false; worker.closed.then(() => { closed = true; });
  const running = daemon.call("turn.run", { ...target, input: "block" }).catch((error) => error); await product.started.promise; const request = { ...target, forget: true }, closing = daemon.call("session.close", request).catch((error) => error); await product.interrupted.promise; daemon.close(); await product.closeStart.promise;
  assert.equal(product.closeSignal.aborted, true); assert.deepEqual(product.closeRequest, request); assert.equal(closed, false); product.release.resolve(); product.closeEnd.resolve(); await worker.closed; await serving; await Promise.all([running, closing]); assert.deepEqual(product.closeRequest, request); assert.equal(product.calls[5], 1);
});

test("one chunk is admitted before its first callback", async (t) => {
  const product = new FakeProduct(); product.started = deferred(); const { worker } = await harness(t, product);
  worker.connection.result = () => Promise.resolve(); worker.connection.error = () => { product.admitted = true; return Promise.resolve(); };
  const frame = (id, input) => JSON.stringify({ jsonrpc: "2.0", id, method: "turn.run", params: { ...target, input } }) + "\n";
  worker.connection._data(Buffer.from(frame(2, "admission") + frame(3, "again"))); await product.started.promise;
});

test("peer replace settles old runs and reconnects with the new identity", async (t) => {
  const first = peerEndpoint(), second = peerEndpoint(), clients = [first.client, second.client], scheduled = [], scheduledReady = deferred();
  const identity = { product: "native-product", session_id: "old", name: "old", groups: ["old"], info: {} };
  const peer = connectPeer(identity, async () => ({ disposition: "injected" }), { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => clients.shift(), schedule: (call) => { scheduled.push(call); scheduledReady.resolve(); } });
  t.after(() => { peer.shutdown(); first.daemon.close(); second.daemon.close(); });
  await first.daemon.result(await first.next(), {}); await peer.ready;
  const before = structuredClone(peer.identity);
  await assert.rejects(peer.replace({ ...before, name: "same" }), /invalid replace identity/); assert.deepEqual(peer.identity, before);
  const started = peer.caller.start({ session_id: "lane@local", input: "block" }), run = await first.next();
  const replacementIdentity = { product: "native-product", session_id: "new", name: "new", groups: ["new"], info: { revision: 1 } };
  const replaced = peer.replace(replacementIdentity), replacement = await first.next(); replacementIdentity.groups.push("mutated"); replacementIdentity.info.revision = 9;
  assert.deepEqual(await peer.caller.wait({ turn_id: started.turn_id }), { turn_id: "t-1", session_id: "lane@local", state: "unavailable", reason: "-32002 not_connected" });
  await assert.rejects(peer.call("session.list", {}), /not connected/);
  const update = { name: "retitled", info: { revision: 2 } }; await assert.rejects(peer.rehello(update), /not connected/); update.info.revision = 9;
  await first.daemon.error(run, -32002); await first.daemon.result(replacement, {});
  const corrective = await first.next(); assert.deepEqual(corrective.params, { protocol: 1, product: "native-product", session_id: "new", name: "retitled", groups: ["new"], info: { revision: 2 } });
  await first.daemon.result(corrective, {}); await replaced;
  first.daemon.close(); await scheduledReady.promise; scheduled.shift()();
  const reconnect = await second.next(); assert.deepEqual(reconnect.params, corrective.params); await second.daemon.result(reconnect, {}); await peer.ready;
});

test("peer delivery keeps its admission identity across replace", async (t) => {
  const endpoint = peerEndpoint(), entered = deferred();
  const identity = { product: "native-product", session_id: "old", name: "old", groups: ["old"], info: { nested: { value: "old" } } };
  const peer = connectPeer(identity, async (signal, _request, admitted) => {
    const seen = structuredClone(admitted); admitted.groups[0] = "mutated"; admitted.info.nested.value = "mutated"; entered.resolve({ seen, signal }); await aborted(signal);
    return { disposition: "rejected", reason: "closing" };
  }, { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => endpoint.client, schedule: () => {} });
  t.after(() => { peer.shutdown(); endpoint.daemon.close(); });
  await endpoint.daemon.result(await endpoint.next(), {}); await peer.ready;
  const delivering = endpoint.daemon.call("message.deliver", delivery), admission = await entered.promise;
  assert.deepEqual(admission.seen, identity); assert.deepEqual(peer.identity, identity);
  const replacing = peer.replace({ product: "native-product", session_id: "new", name: "new", groups: ["new"], info: {} }), replacement = await endpoint.next();
  assert.equal(admission.signal.aborted, true); assert.deepEqual(await delivering, { disposition: "rejected", reason: "closing" });
  await endpoint.daemon.result(replacement, {}); await replacing;
});

test("peer installs delivery identity before reading the next frame", async (t) => {
  const endpoint = peerEndpoint(), called = deferred();
  const identity = { product: "native-product", session_id: "session", name: "peer", groups: [], info: {} };
  const peer = connectPeer(identity, async (signal, _request, admitted) => { called.resolve({ identity: admitted, signal }); return { disposition: "injected" }; }, { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => endpoint.client, schedule: () => {} });
  t.after(() => { peer.shutdown(); endpoint.daemon.close(); });
  const hello = await endpoint.next(), response = { jsonrpc: "2.0", id: hello.id, result: {} }, request = { jsonrpc: "2.0", id: 1, method: "message.deliver", params: delivery };
  endpoint.daemon.stream.write(`${JSON.stringify(response)}\n${JSON.stringify(request)}\n`); await peer.ready;
  const admission = await called.promise; assert.equal(admission.identity.session_id, "session"); assert.equal(admission.signal.aborted, false);
});

test("peer delivery context aborts on EOF", async (t) => {
  const endpoint = peerEndpoint(), entered = deferred(), finished = deferred();
  const peer = connectPeer({ product: "native-product", session_id: "session", name: "peer", groups: [], info: {} }, async (signal) => { entered.resolve(signal); await aborted(signal); finished.resolve(); return { disposition: "rejected", reason: "closing" }; }, { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => endpoint.client, schedule: () => {} });
  t.after(() => peer.shutdown()); await endpoint.daemon.result(await endpoint.next(), {}); await peer.ready;
  void endpoint.daemon.call("message.deliver", delivery).catch(() => {}); const signal = await entered.promise; endpoint.daemon.close(); await finished.promise; assert.equal(signal.aborted, true);
});

test("peer shutdown closes a held replacement hello", async (t) => {
  const endpoint = peerEndpoint(), peer = connectPeer({ product: "native-product", session_id: "old", name: "old", groups: [], info: {} }, async () => ({ disposition: "injected" }), { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => endpoint.client, schedule: () => {} });
  t.after(() => endpoint.daemon.close()); await endpoint.daemon.result(await endpoint.next(), {}); await peer.ready;
  const replacing = peer.replace({ product: "native-product", session_id: "new", name: "new", groups: [], info: {} }); await endpoint.next(); peer.shutdown(); await assert.rejects(replacing, /not connected|closed/); await peer.closed; assert.equal(peer.error, null);
});

test("peer rejected hello is terminal", async (t) => {
  const endpoint = peerEndpoint(), scheduled = [];
  const peer = connectPeer({ product: "native-product", session_id: "session", name: "sentence title", groups: [], info: {} }, async () => ({ disposition: "injected" }), { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => endpoint.client, schedule: (call) => scheduled.push(call) });
  t.after(() => endpoint.daemon.close());
  const hello = await endpoint.next(); assert.equal(hello.params.name, "sentence title"); await endpoint.daemon.error(hello, -32602); await peer.closed;
  assert.equal(peer.error instanceof ProtocolError, true); assert.equal(peer.error.code, -32602); assert.equal(scheduled.length, 0);
  let ready = false; peer.ready.then(() => { ready = true; }); await Promise.resolve(); assert.equal(ready, false);
});

for (const replacement of [false, true]) test(`peer rejected ${replacement ? "replace" : "live rehello"} is terminal`, async (t) => {
  const endpoint = peerEndpoint(), scheduled = [];
  const peer = connectPeer({ product: "native-product", session_id: "session", name: "old title", groups: [], info: {} }, async () => ({ disposition: "injected" }), { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }, { connect: () => endpoint.client, schedule: (call) => scheduled.push(call) });
  t.after(() => endpoint.daemon.close()); const hello = await endpoint.next(); await endpoint.daemon.result(hello, {}); await peer.ready;
  const changed = replacement ? peer.replace({ product: "native-product", session_id: "next", name: "next title", groups: [], info: {} }) : peer.rehello({ name: "next title", info: {} });
  const rejected = await endpoint.next(); await endpoint.daemon.error(rejected, -32602); await assert.rejects(changed, (error) => error instanceof ProtocolError && error.code === -32602); await peer.closed;
  assert.equal(peer.error.code, -32602); assert.equal(scheduled.length, 0); await assert.rejects(peer.call("session.list", {}), /not connected/);
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

function peerEndpoint() {
  const [client, socket] = pair(), queued = [], waiting = [];
  const daemon = new Connection(socket, false, (request) => { const resolve = waiting.shift(); if (resolve) resolve(request); else queued.push(request); });
  return { client, daemon, next: () => queued.length ? Promise.resolve(queued.shift()) : new Promise((resolve) => waiting.push(resolve)) };
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
    case "one-run": case "one-interrupt": case "interrupt-error": case "full-duplex": case "callback-originated-method": case "close-during-run": case "run-done": {
      product.started = deferred(); product.release = deferred(); const { daemon } = await harness(t, product); const running = daemon.call("turn.run", { ...target, input: "block" }); const run = await product.started.promise;
      if (row.name === "one-run") await errorCode(daemon.call("turn.run", { ...target, input: "again" }), -32003);
      if (row.name === "one-interrupt") { product.interrupted = deferred(); await Promise.all([daemon.call("turn.interrupt", target), daemon.call("turn.interrupt", target)]); await product.interrupted.promise; }
      if (row.name === "interrupt-error") { product.interruptError = new Error("first failure\nsecond failure"); const stderr = captureStderr(t); await daemon.call("turn.interrupt", target); assert.equal(stderr(), 'agentbus: product interrupt: "first failure\\nsecond failure"\n'); }
      if (row.name === "full-duplex" || row.name === "callback-originated-method") { product.outbound = true; assert.equal((await daemon.call("message.deliver", delivery)).disposition, "injected"); }
      if (row.name === "run-done") { let done = false; run.Done.then(() => { done = true; }); await Promise.resolve(); assert.equal(done, false); }
      if (row.name === "close-during-run") {
        const stderr = captureStderr(t); product.interrupted = deferred(); product.closeStart = deferred(); product.closeEnd = deferred(); product.hangInterrupt = true; product.interruptError = new Error("first failure\nsecond failure"); const closing = daemon.call("session.close", target); await product.interrupted.promise; await daemon.call("turn.interrupt", target); product.release.resolve(); await running; await product.closeStart.promise; product.closeEnd.resolve(); await closing; assert.equal(stderr(), 'agentbus: product interrupt: "first failure\\nsecond failure"\n'); break;
      }
      product.release.resolve(); result = await running; await run.Done; if (row.name === "one-interrupt") assert.equal(run.Native, null); break;
    }
    case "eof-during-run": {
      product.closeError = new Error("first failure\nsecond failure"); const stderr = captureStderr(t); product.started = deferred(); const { daemon, worker, serving } = await harness(t, product); const running = daemon.call("turn.run", { ...target, input: "eof" }); const run = await product.started.promise; daemon.close(); await serving; await assert.rejects(running); await run.Done; assert.equal(worker.controller.signal.aborted, true); assert.equal(product.closeAborted, true); assert.deepEqual(product.closeRequest, {}); assert.equal(stderr(), 'agentbus: product close: "first failure\\nsecond failure"\n'); break;
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
    case "shutdown": { const { worker, serving } = await harness(t, product, { open: false }); worker.shutdown(); worker.shutdown(); await serving; break; }
    case "run-done-write-failure": {
      product.started = deferred(); product.release = deferred(); const { daemon, workerSocket, serving } = await harness(t, product); const running = daemon.call("turn.run", { ...target, input: "block" }); const run = await product.started.promise; workerSocket.failNext = true; product.release.resolve(); await serving; await assert.rejects(running); await run.Done; break;
    }
    case "close-error": {
      product.closeError = new Error("first failure\nsecond failure"); const stderr = captureStderr(t); const { daemon } = await harness(t, product); assert.deepEqual(await daemon.call("session.close", target), {}); assert.equal(product.closeAborted, false); assert.deepEqual(product.closeRequest, target); assert.equal(stderr(), 'agentbus: product close: "first failure\\nsecond failure"\n'); break;
    }
    case "close-forget": { const { daemon } = await harness(t, product); const request = { ...target, forget: true }; assert.deepEqual(await daemon.call("session.close", request), {}); assert.deepEqual(product.closeRequest, request); break; }
    default: assert.fail(`unknown row ${row.name}`);
  }
  assert.deepEqual(product.calls, row.calls);
});

function captureStderr(t) {
  const write = process.stderr.write; let output = ""; process.stderr.write = (chunk) => { output += chunk; return true; }; t.after(() => { process.stderr.write = write; }); return () => output;
}

async function peerLifetime() {
  const connections = [], scheduled = []; let scheduledReady = deferred(), holdHello; const env = { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" }; let currentIdentity, deliveries = 0;
  const identity = { product: "native-product", session_id: "session-1", name: "peer", groups: ["shared"], info: {} };
  const peer = connectPeer(identity, async () => { deliveries++; return { disposition: "injected" }; }, env, { connect: () => { const [client, server] = pair(); const daemon = new Connection(server, false, (request) => {
    if (request.method === "session.hello") { if (holdHello) holdHello.resolve(); else if (currentIdentity?.session_id === request.params.session_id && JSON.stringify(currentIdentity.groups) !== JSON.stringify(request.params.groups)) void daemon.error(request, -32602); else { currentIdentity = request.params; void daemon.result(request, {}); } }
  }); connections.push(daemon); return client; }, schedule: (call, milliseconds) => { scheduled.push({ call, milliseconds }); scheduledReady.resolve(); } });
  await peer.ready; assert.deepEqual(env, {}); identity.groups.push("mutated"); identity.info.changed = true;
  connections[0].close(); await scheduledReady.promise; await assert.rejects(peer.call("session.list", {}), /not connected/); assert.equal(scheduled[0].milliseconds, 2000);
  const offline = { name: "offline title", info: { phase: "stored" } }; await assert.rejects(peer.rehello(offline), /not connected/); offline.info.phase = "mutated";
  scheduledReady = deferred(); scheduled.shift().call(); await peer.ready; assert.equal(currentIdentity.name, "offline title"); assert.deepEqual(currentIdentity.groups, ["shared"]); assert.deepEqual(currentIdentity.info, { phase: "stored" });
  const beforeInvalid = structuredClone(peer.identity);
  for (const invalid of [{ name: "changed", info: {}, groups: ["changed"] }, { name: "", info: {} }, { name: "changed", info: [] }]) await assert.rejects(peer.rehello(invalid), /invalid rehello identity/);
  assert.deepEqual(peer.identity, beforeInvalid);
  assert.equal((await connections[1].call("message.deliver", delivery)).disposition, "injected"); assert.equal(deliveries, 1);
  const renamed = { name: "renamed", info: {} }; await peer.rehello(renamed); assert.equal(currentIdentity.name, "renamed"); renamed.info.changed = true;
  holdHello = deferred(); const crossed = peer.rehello({ name: "crossed title", info: { phase: "new" } }); await holdHello.promise; connections[1].close(); holdHello = null;
  await assert.rejects(crossed, /not connected/); await scheduledReady.promise; scheduledReady = deferred(); scheduled.shift().call(); await peer.ready;
  assert.equal(currentIdentity.name, "crossed title"); assert.deepEqual(currentIdentity.info, { phase: "new" });
  const beforeTerminal = structuredClone(peer.identity); await connections[2].call("session.superseded", {}); await peer.closed;
  assert.equal(peer.error instanceof ProtocolError, true); assert.equal(peer.error.code, -32012);
  await assert.rejects(peer.rehello({ name: "too late", info: {} }), /superseded/); assert.deepEqual(peer.identity, beforeTerminal); assert.equal(scheduled.length, 0);
  await assert.rejects(peer.replace({ ...beforeTerminal, session_id: "too-late" }), /superseded/); assert.deepEqual(peer.identity, beforeTerminal);
}
