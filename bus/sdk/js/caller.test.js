"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { ACTIONS, Caller, connectPeer, Connection, ProtocolError } = require("./index.js");
const { pair, deferred } = require("./test-support.js");

const fixtures = JSON.parse(fs.readFileSync(path.join(__dirname, "../../internal/protocol/caller-sugar.fixtures.json"), "utf8"));

const replies = {
  "session.list": { sessions: [] },
  "message.send": { message_id: "message", deliveries: [] },
  "lane.describe": { product: "example-peer", supported_open_fields: [], extra_arguments: [] },
  "lane.spawn": { session_id: "lane@local" },
  "turn.run": fixtures.shapes.done.terminal,
  "turn.interrupt": {},
  "session.close": {},
};

test("caller maps operations to closed wire requests", async (t) => {
  const [clientSocket, daemonSocket] = pair();
  const seen = [];
  const daemon = new Connection(daemonSocket, false, (request) => { seen.push([request.method, request.params]); void daemon.result(request, replies[request.method]); });
  const connection = new Connection(clientSocket, true);
  const caller = new Caller(connection);
  t.after(() => { connection.close(); daemon.close(); });

  const table = [
    [() => caller.list({}), "session.list", {}],
    [() => caller.send({ target: "lane", message: "hello" }), "message.send", { target: "lane", message: "hello" }],
    [() => caller.describe({ product: "example-peer" }), "lane.describe", { product: "example-peer" }],
    [() => caller.spawn({ name: "child", product: "example-peer", open: {} }), "lane.spawn", { name: "child", product: "example-peer", open: {} }],
    [() => caller.resume("lane@local"), "lane.spawn", { resume_session_id: "lane@local" }],
    [() => caller.run({ session_id: "lane@local", input: "work" }), "turn.run", { session_id: "lane@local", input: "work" }],
    [() => caller.interrupt({ session_id: "lane@local" }), "turn.interrupt", { session_id: "lane@local" }],
    [() => caller.close({ session_id: "lane@local", forget: true }), "session.close", { session_id: "lane@local", forget: true }],
  ];
  for (const [call, method, params] of table) { await call(); assert.deepEqual(seen.shift(), [method, params]); }

  const actions = [
    ["list", {}, "session.list", {}],
    ["send", { target: "lane", message: "hello" }, "message.send", { target: "lane", message: "hello" }],
    ["spawn", { name: "child", product: "example-peer", open: {} }, "lane.spawn", { name: "child", product: "example-peer", open: {} }],
    ["describe", { product: "example-peer" }, "lane.describe", { product: "example-peer" }],
    ["run", { session_id: "lane@local", input: "work" }, "turn.run", { session_id: "lane@local", input: "work" }],
    ["interrupt", { session_id: "lane@local" }, "turn.interrupt", { session_id: "lane@local" }],
    ["close", { session_id: "lane@local" }, "session.close", { session_id: "lane@local" }],
    ["forget", { session_id: "lane@local" }, "session.close", { session_id: "lane@local", forget: true }],
  ];
  for (const [action, args, method, params] of actions) { await caller.action(action, args); assert.deepEqual(seen.shift(), [method, params]); }
  assert.deepEqual(ACTIONS, ["list", "send", "spawn", "describe", "run", "start", "wait", "status", "interrupt", "close", "forget"]);
  await assert.rejects(caller.action("unknown", {}), /unknown action/);
  await assert.rejects(caller.action("list", { extra: true }), /invalid SessionListRequest/);
  await assert.rejects(caller.action("status", { turn_id: "missing", extra: true }), /invalid status request/);
});

test("caller sugar matches the shared shapes and sequences", async (t) => {
  const [clientSocket, daemonSocket] = pair();
  const sequence = fixtures.sequences.target_release_before_collection;
  const pending = new Map(), timers = [], arrived = new Map([[sequence.first_input, deferred()], [sequence.second_input, deferred()]]);
  const daemon = new Connection(daemonSocket, false, (request) => { pending.set(request.params.input, request); arrived.get(request.params.input)?.resolve(); });
  const connection = new Connection(clientSocket, true);
  const caller = new Caller(connection, { schedule: (call, milliseconds) => { const timer = { call, milliseconds, stopped: false }; timers.push(timer); return () => { timer.stopped = true; }; } });
  t.after(() => { connection.close(); daemon.close(); });

  const first = await caller.action("start", fixtures.shapes.start.request);
  assert.deepEqual(first, fixtures.shapes.start.result);
  await arrived.get(sequence.first_input).promise;
  await assert.rejects(caller.action("start", fixtures.shapes.start.request), (error) => error instanceof ProtocolError && error.code === -32003);
  assert.deepEqual(await caller.action("status", { turn_id: first.turn_id }), fixtures.shapes.running.result);
  assert.throws(() => caller.status({ turn_id: first.turn_id, extra: true }), /invalid status request/);
  await assert.rejects(caller.wait({ turn_id: first.turn_id, timeout_ms: 1.5 }), /invalid wait request/);
  const timed = caller.action("wait", { turn_id: first.turn_id, timeout_ms: 25 });
  await Promise.resolve();
  assert.equal(timers[0].milliseconds, 25);
  timers[0].call();
  assert.deepEqual(await timed, fixtures.shapes.running.result);
  await daemon.result(pending.get(sequence.first_input), fixtures.shapes.done.terminal);
  await caller.runs.get(first.turn_id).settled;
  const second = await caller.action("start", { session_id: sequence.session_id, input: sequence.second_input });
  assert.deepEqual([first.turn_id, second.turn_id], [sequence.first_turn_id, sequence.second_turn_id]);
  assert.deepEqual(await caller.action("status", { turn_id: first.turn_id }), fixtures.shapes.done.result);
  assert.throws(() => caller.status({ turn_id: first.turn_id }), /unknown_turn/);
  for (const row of fixtures.sequences.invalid_local_requests) {
    const call = () => caller[row.operation](row.request);
    if (row.operation === "wait") await assert.rejects(call(), new RegExp(row.error)); else assert.throws(call, new RegExp(row.error));
  }
  await arrived.get(sequence.second_input).promise;
  await daemon.result(pending.get(sequence.second_input), fixtures.shapes.done.terminal);
  assert.deepEqual(await caller.action("wait", { turn_id: second.turn_id }), { ...fixtures.shapes.done.result, turn_id: second.turn_id });
});

test("connection loss removes local runs and reports a resumable lane", async () => {
  const [clientSocket, daemonSocket] = pair();
  const connection = new Connection(clientSocket, true);
  const caller = new Caller(connection);
  const id = caller.start(fixtures.shapes.start.request).turn_id;
  const waiting = caller.wait({ turn_id: id });
  daemonSocket.destroy();
  assert.deepEqual(await waiting, fixtures.shapes.eof.result);
  assert.throws(() => caller.status({ turn_id: id }), /unknown_turn/);
});

test("wire run error becomes an unavailable result", async (t) => {
  const [clientSocket, daemonSocket] = pair();
  const daemon = new Connection(daemonSocket, false, (request) => { void daemon.error(request, fixtures.shapes.wire_error.code); });
  const connection = new Connection(clientSocket, true); const caller = new Caller(connection);
  t.after(() => { connection.close(); daemon.close(); });
  const id = caller.start(fixtures.shapes.start.request).turn_id;
  assert.deepEqual(await caller.wait({ turn_id: id }), fixtures.shapes.wire_error.result);
  assert.throws(() => caller.status({ turn_id: id }), /unknown_turn/);
});

test("crossed rehello preserves the fixture's newest identity", async (t) => {
  const [clientSocket, daemonSocket] = pair();
  const queued = [], waiting = [];
  const next = () => queued.length ? Promise.resolve(queued.shift()) : new Promise((resolve) => waiting.push(resolve));
  const daemon = new Connection(daemonSocket, false, (request) => { const resolve = waiting.shift(); if (resolve) resolve(request); else queued.push(request); });
  const row = structuredClone(fixtures.sequences.crossed_rehello);
  const env = { AGENTBUS_SOCKET: "/fixture/socket", AGENTBUS_LOCAL_KEY: "" };
  const peer = connectPeer(row.identity, async () => ({ disposition: "injected" }), env, { connect: () => clientSocket, schedule: () => {} });
  t.after(() => { peer.shutdown(); daemon.close(); });

  row.identity.info.nested.value = "mutated";
  const initial = await next();
  assert.deepEqual([initial.params.name, initial.params.info.nested.value], ["initial", "initial"]);
  await daemon.result(initial, {}); await peer.ready;

  const first = peer.rehello(row.first); const firstRequest = await next(); row.first.info.nested.value = "mutated";
  const second = peer.rehello(row.second); const secondRequest = await next(); row.second.info.nested.value = "mutated";
  await daemon.result(secondRequest, {}); await second;
  await daemon.result(firstRequest, {});
  const corrective = await next();
  assert.deepEqual([corrective.params.name, corrective.params.info.nested.value], ["second", "second"]);
  await daemon.result(corrective, {}); await first;
  assert.deepEqual([peer.identity.name, peer.identity.info.nested.value], ["second", "second"]);
});
