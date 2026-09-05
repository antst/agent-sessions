"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { Caller, Connection, ProtocolError } = require("./index.js");
const { pair, deferred } = require("./test-support.js");

const replies = {
  "session.list": { sessions: [] },
  "message.send": { message_id: "message", deliveries: [] },
  "lane.describe": { product: "example-peer", supported_open_fields: [], extra_arguments: [] },
  "lane.spawn": { session_id: "lane@local" },
  "turn.run": { outcome: "completed", result: "done" },
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
});

test("start tracks concurrent targets and wait timeout leaves the run active", async (t) => {
  const [clientSocket, daemonSocket] = pair();
  const pending = new Map(), timers = [], arrived = new Map([["one@local", deferred()], ["two@local", deferred()]]);
  const daemon = new Connection(daemonSocket, false, (request) => { pending.set(request.params.session_id, request); arrived.get(request.params.session_id)?.resolve(); });
  const connection = new Connection(clientSocket, true);
  const caller = new Caller(connection, { schedule: (call, milliseconds) => { const timer = { call, milliseconds, stopped: false }; timers.push(timer); return () => { timer.stopped = true; }; } });
  t.after(() => { connection.close(); daemon.close(); });

  const first = caller.start({ session_id: "one@local", input: "first" }).turn_id;
  const second = caller.start({ session_id: "two@local", input: "second" }).turn_id;
  await Promise.all([...arrived.values()].map((entry) => entry.promise));
  assert.deepEqual([first, second], ["t-1", "t-2"]);
  assert.throws(() => caller.start({ session_id: "one@local", input: "again" }), (error) => error instanceof ProtocolError && error.code === -32003);
  assert.throws(() => caller.status({ turn_id: first, extra: true }), /invalid status request/);
  await assert.rejects(caller.wait({ turn_id: first, timeout_ms: 1.5 }), /invalid wait request/);
  assert.deepEqual(caller.status({ turn_id: first }), { turn_id: first, session_id: "one@local", state: "running" });
  const timed = caller.wait({ turn_id: first, timeout_ms: 25 });
  assert.equal(timers[0].milliseconds, 25);
  timers[0].call();
  assert.equal((await timed).state, "running");
  assert.equal(caller.status({ turn_id: first }).state, "running");

  const completed = caller.wait({ turn_id: first });
  await daemon.result(pending.get("one@local"), replies["turn.run"]);
  assert.deepEqual(await completed, { turn_id: first, session_id: "one@local", state: "done", result: replies["turn.run"] });
  assert.throws(() => caller.status({ turn_id: first }), /unknown_turn/);
  await daemon.result(pending.get("two@local"), replies["turn.run"]);
  await Promise.resolve();
  assert.equal(caller.start({ session_id: "two@local", input: "again" }).turn_id, "t-3");
  assert.deepEqual(caller.status({ turn_id: second }), { turn_id: second, session_id: "two@local", state: "done", result: replies["turn.run"] });
});

test("connection loss removes local runs and reports a resumable lane", async () => {
  const [clientSocket, daemonSocket] = pair();
  const connection = new Connection(clientSocket, true);
  const caller = new Caller(connection);
  const id = caller.start({ session_id: "lane@local", input: "work" }).turn_id;
  const waiting = caller.wait({ turn_id: id });
  daemonSocket.destroy();
  assert.deepEqual(await waiting, { turn_id: id, session_id: "lane@local", state: "unavailable", reason: "result unavailable, lane resumable" });
  assert.throws(() => caller.status({ turn_id: id }), /unknown_turn/);
});
