"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const { CLIENT_OPERATIONS, METHOD_DEFINITIONS, InactiveError, LiveSessionClient, readConfiguration, renderDelivery } = require("./live-session.js");

test("default reconnect cadence is two seconds", () => {
  const client = new LiveSessionClient({ env: {} });
  assert.equal(client.reconnectMs, 2000);
});

test("one shared method authority owns the exact common surface and closed lane results", () => {
  const expected = {
    "session.hello": ["client", false, false, true, false], "session.update": ["client", false, false, false, false],
    "peers.list": ["client", false, false, false, true], "message.send": ["client", false, false, false, true],
    "lane.doctor": ["client", true, false, false, true], "lane.list": ["client", true, false, false, true],
    "lane.start": ["client", true, true, false, true], "lane.run": ["client", true, true, false, true], "lane.resume": ["client", true, true, false, true], "lane.steer": ["client", true, true, false, true],
    "lane.wait": ["client", true, false, false, true], "lane.status": ["client", true, false, false, true], "lane.interrupt": ["client", true, false, false, true], "lane.archive": ["client", true, false, false, true],
    "message.deliver": ["daemon", false, false, false, false], "lane.turn.start": ["daemon", true, false, false, false], "lane.turn.wait": ["daemon", true, false, false, false],
    "lane.turn.interrupt": ["daemon", true, false, false, false], "lane.session.archive": ["daemon", true, false, false, false],
  };
  assert.equal(Object.keys(METHOD_DEFINITIONS).length, 19);
  for (const [name, fields] of Object.entries(expected)) {
    const spec = METHOD_DEFINITIONS[name];
    assert.deepEqual([spec.direction, spec.lane, spec.needsInput, spec.first, spec.tool], fields, name);
    assert.equal(typeof spec.params, "function", `${name} params`); assert.equal(typeof spec.result, "function", `${name} result`);
  }
  assert.equal(METHOD_DEFINITIONS["lane.future"], undefined);
  assert.deepEqual(CLIENT_OPERATIONS, Object.entries(expected).filter(([, fields]) => fields[4]).map(([name]) => name));

  const status = { type: "lane.status", product: "codex", session_id: "s", name: "n", cwd: "/w", groups: [], permission_mode: "default", state: "idle", turn_id: "", outcome: "", exit: null, owner_session_id: "p", persistent: false, auto_archive: true, auto_archive_after_seconds: 1.5, auto_archive_at: 0 };
  const completed = { type: "turn.completed", product: "codex", session_id: "s", turn_id: "t", status: "completed", outcome: "completed", exit: 0, result: "done", diagnostic: "" };
  const results = {
    "lane.doctor": { type: "lane.doctor", contract_version: 2, authority: "daemon", product: "codex", ready: true, native_path: "/bin/codex", runtime_path: "/bin/codex", daemon_reachable: true, supervisor_reachable: true, codex_available: true, codex_path: "/bin/codex", codex_version: "1" },
    "lane.list": { type: "lane.list", product: "codex", lanes: [status] }, "lane.start": { ...status, type: "lane.ready", contract_version: 2 },
    "lane.run": completed, "lane.resume": completed, "lane.wait": completed, "lane.steer": { type: "turn.steered", session_id: "s", turn_id: "t", native_message_id: "m" },
    "lane.status": status, "lane.interrupt": { type: "turn.interrupting", session_id: "s", turn_id: "t" }, "lane.archive": { type: "lane.archived", product: "codex", session_id: "s", name: "n", already_archived: false },
  };
  const params = { product: "codex", arguments: [] };
  for (const [name, value] of Object.entries(results)) {
    const validate = METHOD_DEFINITIONS[name].result;
    assert.equal(validate(params, value), true, `${name} valid`); assert.equal(validate(params, null), false, `${name} null`);
    const missing = { ...value }; delete missing.type; assert.equal(validate(params, missing), false, `${name} missing`);
    assert.equal(validate(params, { ...value, "type product": true }), false, `${name} extra`);
    assert.equal(validate(params, { ...value, type: 1 }), false, `${name} wrong type`);
    if ("product" in value) assert.equal(validate({ product: "qwen", arguments: [] }, value), false, `${name} wrong product`);
  }
  for (const name of ["lane.run", "lane.resume", "lane.wait"]) {
    const validate = METHOD_DEFINITIONS[name].result;
    assert.equal(validate(params, { ...completed, native_stop_reason: "aborted" }), true);
    assert.equal(validate(params, { ...completed, native_stop_reason: "" }), false);
    assert.equal(validate(params, { ...completed, native_stop_reason: 1 }), false);
  }
  for (const name of ["lane.start", "lane.steer", "lane.status", "lane.interrupt", "lane.archive"])
    assert.equal(METHOD_DEFINITIONS[name].result(params, { ...results[name], native_stop_reason: "aborted" }), false, name);
});

test("shared params authority follows Appendix A grammar", async () => {
  const hello = (overrides = {}) => ({ protocol: 1, uuid: "native", name: "", groups: [], product: "codex", info: {}, ...overrides });
  const validateHello = METHOD_DEFINITIONS["session.hello"].params;
  for (const value of [hello(), hello({ protocol: 1.0 }), hello({ uuid: `native\ufeffid` }), hello({ uuid: "x".repeat(128) }), hello({ capabilities: { lane: true } })]) assert.equal(validateHello(value), true);
  for (const value of [hello({ protocol: 1.5 }), hello({ protocol: 9007199254740992 }), hello({ uuid: "native id" }), hello({ uuid: "native/id" }), hello({ uuid: "native\n" }), hello({ uuid: "x".repeat(129) }), hello({ capabilities: null }), hello({ capabilities: {} }), hello({ capabilities: { lane: false } }), hello({ capabilities: { lane: true, extra: true } })]) assert.equal(validateHello(value), false, JSON.stringify(value));
  for (const [name, valid] of [["", true], ["n".repeat(256), true], ["n".repeat(257), false], ["bad\u007fname", false]]) {
    assert.equal(validateHello(hello({ name })), valid, `hello name ${name.length}`);
    assert.equal(METHOD_DEFINITIONS["session.update"].params({ name, info: {} }), valid, `update name ${name.length}`);
  }
  for (const [info, valid] of [[{}, true], [{ cwd: "/work" }, true], [{ cwd: "" }, false], [{ cwd: "work" }, false]]) {
    assert.equal(validateHello(hello({ info })), valid, JSON.stringify(info));
    assert.equal(METHOD_DEFINITIONS["session.update"].params({ name: "", info }), valid, JSON.stringify(info));
  }
  const laneStart = METHOD_DEFINITIONS["lane.start"].params;
  assert.equal(laneStart({ product: "codex", arguments: ["line\nbreak", "\0"], input: " " }), true);
  assert.equal(laneStart({ product: "future-product", arguments: [], input: "work" }), true);
  assert.equal(laneStart({ product: "codex", arguments: [], input: "work", cwd: "/work", host: " " }), true);
  assert.equal(laneStart({ product: "codex", arguments: [], input: "work", cwd: "work" }), false);
  assert.equal(laneStart({ product: "codex", arguments: [], input: "work", cwd: null }), false);
  assert.equal(laneStart({ product: "codex", arguments: [], input: "work", host: null }), false);
  assert.equal(laneStart({ product: "codex", arguments: [], input: "" }), false);
  assert.equal(METHOD_DEFINITIONS["lane.status"].params({ product: "codex", arguments: [], input: null }), false);
  assert.equal(METHOD_DEFINITIONS["lane.turn.start"].params({ input_id: " ", body: "\0", mode: "followup" }), true);
  assert.equal(METHOD_DEFINITIONS["lane.turn.start"].params({ input_id: "", body: "x", mode: "followup" }), false);
  assert.equal(METHOD_DEFINITIONS["lane.turn.wait"].params({ native_message_id: "\n" }), true);
  assert.equal(METHOD_DEFINITIONS["lane.turn.wait"].params({ native_message_id: "" }), false);
  const delivery = { message_id: " ", from: { uuid: "parent", name: "", product: "codex", groups: [] }, body: "" };
  assert.equal(METHOD_DEFINITIONS["message.deliver"].params(delivery), true);
  const noBody = { ...delivery }; delete noBody.body; assert.equal(METHOD_DEFINITIONS["message.deliver"].params(noBody), false);
  const noName = { ...delivery, from: { ...delivery.from } }; delete noName.from.name; assert.equal(METHOD_DEFINITIONS["message.deliver"].params(noName), false);

  const client = new LiveSessionClient({ env: {} });
  const session = { pending: new Map(), socket: { destroyed: false, write: () => true }, ready: true };
  await assert.rejects(client._call(session, "wrong-first", "peers.list", {}, true), /connection phase/u);
  await assert.rejects(client._call(session, "late-hello", "session.hello", hello(), false), /connection phase/u);
});

test("delivery rendering matches the shared golden fixture", () => {
  const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, "../../internal/sessiontools/testdata/native-message-envelope.json"), "utf8"));
  const { message_id: messageID, from, body } = fixture.message;
  assert.equal(renderDelivery({ messageID, from, body }), fixture.rendered);
});

test("framing processes complete frames, rejects truncated tails and write throws, and accepts backpressure", async () => {
  const client = new LiveSessionClient({ env: {} });
  let destroyed = 0, responses = 0, diagnostic;
  client.on("diagnostic", (value) => { diagnostic = value; });
  const socket = { destroyed: false, write: () => false, destroy: () => { destroyed += 1; } };
  const session = { ready: true, socket, buffer: "", pending: new Map() };
  assert.equal(client._write(session, { jsonrpc: "2.0", id: "one", result: {} }), true);
  client._response(session, { jsonrpc: "2.0", id: "unknown", result: {} });
  assert.equal(destroyed, 1);
  client._response = () => { responses += 1; };
  const prefix = '{"jsonrpc":"2.0","id":"known","result":{"padding":"', suffix = '"}}';
  const frame = (newline) => prefix + "x".repeat(1024 * 1024 - prefix.length - suffix.length - (newline ? 1 : 0)) + suffix + (newline ? "\n" : "");
  client._data(session, frame(true));
  assert.equal(responses, 1);
  client._data(session, frame(false));
  assert.equal(destroyed, 1);
  let tailError;
  session.pending.set("tail", { reject: (error) => { tailError = error; } });
  client._closed(session, socket);
  assert.equal(tailError?.message, "live frame is not newline terminated");
  assert.equal(diagnostic, "live frame is not newline terminated");
  assert.equal(session.buffer, "");
  session.socket = socket;
  client._data(session, `${frame(false)}x`);
  assert.equal(destroyed, 2);

  for (const invalid of [
    { jsonrpc: "2.0", id: "request-null", method: "peers.list", params: {}, error: null },
    { jsonrpc: "2.0", id: "response-null", result: {}, error: null },
    { jsonrpc: "2.0", id: "method-request-null", method: null, params: {} },
    { jsonrpc: "2.0", id: "method-response-null", method: null, result: {} },
  ]) {
    let rejected = 0;
    const invalidSession = { ready: true, capabilities: {}, buffer: "", pending: new Map(), socket: { destroyed: false, destroy: () => { rejected += 1; } } };
    client._data(invalidSession, `${JSON.stringify(invalid)}\n`);
    assert.equal(rejected, 1);
  }

  let writeDestroyed = 0;
  const throwing = { ready: true, pending: new Map(), socket: { destroyed: false, write: () => { throw new Error("boom"); }, destroy: () => { writeDestroyed += 1; } } };
  await assert.rejects(client._call(throwing, "throw", "peers.list", {}), /boom/u);
  assert.equal(throwing.pending.size, 0);
  assert.equal(writeDestroyed, 1);
});

test("one socket reports, calls, updates, and receives messages", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  assert.deepEqual(await client.start(), { active: true });
  assert.equal(client.report("native", "before", { cwd: "/work" }), true);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  assert.deepEqual(fixture.reports[0], { protocol: 1, uuid: "native", name: "before", groups: ["team", "team"], product: "pi", info: { cwd: "/work" } });

  const call = client.callTool("native", "tool-one", "peers.list", {});
  await until(() => fixture.requests.some((frame) => frame.method === "peers.list"));
  fixture.write({ jsonrpc: "2.0", id: fixture.requests[0].id, result: { peers: [] } });
  assert.deepEqual(await call, { peers: [] });

  const invalidCall = client.callTool("native", "invalid-result", "peers.list", {});
  await until(() => fixture.requests.some((frame) => frame.id === "session.invalid-result"));
  fixture.write({ jsonrpc: "2.0", id: "session.invalid-result", result: null });
  await assert.rejects(invalidCall, /invalid peers\.list result/u);

  const groupCall = client.callTool("native", "group-one", "message.send", { group: "team", message: "hello all" });
  await until(() => fixture.requests.some((frame) => frame.method === "message.send"));
  const groupRequest = fixture.requests.find((frame) => frame.method === "message.send");
  assert.deepEqual(groupRequest.params, { group: "team", message: "hello all" });
  fixture.write({ jsonrpc: "2.0", id: groupRequest.id, result: { message_id: "group-message", deliveries: [] } });
  assert.deepEqual(await groupCall, { message_id: "group-message", deliveries: [] });

  const laneCall = client.callTool("native", "lane-one", "lane.status", { product: "qwen", arguments: ["worker"] });
  await until(() => fixture.requests.some((frame) => frame.method === "lane.status"));
  const laneRequest = fixture.requests.find((frame) => frame.method === "lane.status");
  assert.deepEqual(laneRequest.params, { product: "qwen", arguments: ["worker"] });
  const laneStatus = { type: "lane.status", product: "qwen", session_id: "lane", name: "worker", cwd: "/work", groups: [], permission_mode: "default", state: "idle", turn_id: "", outcome: "", exit: null, owner_session_id: "native", persistent: false, auto_archive: true, auto_archive_after_seconds: 60, auto_archive_at: 0 };
  fixture.write({ jsonrpc: "2.0", id: laneRequest.id, result: laneStatus });
  assert.deepEqual(await laneCall, laneStatus);

  client.updateName("native", "after");
  client.updateName("native", "after-again");
  await until(() => fixture.requests.filter((frame) => frame.method === "session.update").length === 2);
  const updates = fixture.requests.filter((frame) => frame.method === "session.update");
  assert.deepEqual(updates.map((frame) => frame.params.name), ["after", "after-again"]);
  assert.notEqual(updates[0].id, updates[1].id);
  for (const update of updates) fixture.write({ jsonrpc: "2.0", id: update.id, result: {} });

  const delivered = new Promise((resolve) => client.once("message", resolve));
  fixture.write({ jsonrpc: "2.0", id: "daemon.message", method: "message.deliver", params: {
    message_id: "message", from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "hello",
  } });
  assert.deepEqual(await delivered, {
    messageID: "message", nativeSessionID: "native",
    from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "hello",
  });
  assert.equal(client.acceptMessage("message", null), false);
  client.acceptMessage("message");
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.message"));

  async function reject(messageID, error) {
    const delivered = new Promise((resolve) => client.once("message", resolve));
    fixture.write({ jsonrpc: "2.0", id: `daemon.${messageID}`, method: "message.deliver", params: {
      message_id: messageID, from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "fail",
    } });
    await delivered; client.rejectMessage(messageID, error);
    await until(() => fixture.responses.some((frame) => frame.id === `daemon.${messageID}`));
    return fixture.responses.find((frame) => frame.id === `daemon.${messageID}`).error;
  }
  assert.deepEqual(await reject("failed", "product rejected exact input"), {
    code: -32006, message: "product rejected exact input", data: {
      detail: "product rejected exact input",
      agent_sessions_bug_report: "If Agent Sessions behaves contrary to this description or its documentation and the gh CLI is authorized in your environment, you are encouraged to open an issue on github.com/antst/agent-sessions with gh issue create, including the exact command, observed behavior, and expected behavior.",
    },
  });

  const exactData = { detail: "native exact", nested: [null, true, 3.5, { value: "kept" }] };
  assert.deepEqual(await reject("native-failed", { code: -32006, message: "native exact failure", data: exactData }), {
    code: -32006, message: "native exact failure", data: exactData,
  });

  assert.deepEqual(await reject("unknown-failure", { code: -32001, message: "Unknown session or target", data: { target: "missing" } }),
    { code: -32001, message: "Unknown session or target", data: { target: "missing" } });

  const cyclic = {}; cyclic.self = cyclic;
  const sparse = []; sparse.length = 1;
  const extraArray = []; extraArray.extra = true;
  const nativeError = (data) => ({ code: -32006, message: "native failure", data });
  for (const [id, malformed] of [
    ["malformed-shape", { code: -32002, message: "Session busy", data: { uuid: "bad/id" } }],
    ["invalid-method-space", { code: -32602, message: "Invalid params", data: { method: " lane.steer " } }],
    ["invalid-method-control", { code: -32003, message: "Operation not permitted", data: { method: "lane.\nsteer" } }],
    ["invalid-received", { code: -32004, message: "Unsupported protocol version", data: { supported: 1, received: 9007199254740992 } }],
    ["malformed-json", nativeError(1n)],
    ["inherited-message", Object.assign(new Error("native failure"), { code: -32006, data: { detail: "lost message" } })],
    ...[["undefined", { detail: undefined }], ["function", { detail() {} }], ["symbol", { detail: Symbol("x") }], ["nan", { detail: Number.NaN }],
      ["infinite", { detail: Number.POSITIVE_INFINITY }], ["exotic", new Date(0)], ["accessor", Object.defineProperty({}, "detail", { get() { return "x"; }, enumerable: true })], ["sparse", sparse], ["extra-array", extraArray], ["cyclic", cyclic]]
      .map(([id, data]) => [`${id}-json`, nativeError(data)]),
  ]) {
    const malformedError = await reject(id, malformed);
    assert.equal(malformedError.code, -32006); assert.match(malformedError.data.agent_sessions_bug_report, /github\.com\/antst\/agent-sessions/u);
  }
});

test("one session may report product-owned groups instead of process launch groups", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  assert.equal(client.report("native", "", { cwd: "/native" }, {}, ["profile-group"]), true);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  assert.deepEqual(fixture.reports[0].groups, ["profile-group"]);
  assert.equal(client.report("other", "", { cwd: "/native" }, {}, [1]), false);
  assert.equal(client.report("relative", "", { cwd: "work" }), false);
  assert.equal(client.update("native", "", { cwd: "" }), false);
});

test("disconnect rejects calls and reconnect reports from scratch", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  client.report("native", "worker");
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  const pending = client.callTool("native", "tool", "peers.list", {});
  await until(() => fixture.requests.length === 1);
  fixture.socket.destroy();
  await assert.rejects(pending, /disconnected/u);
  await until(() => fixture.reports.length === 2 && client.sessions.get("native")?.ready);
});

test("lane capability serves native lane requests on the held socket", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  const handled = [];
  client.handleLaneRequests(({ nativeSessionID, method, params }) => {
    handled.push({ nativeSessionID, method, params });
    return { native_message_id: "product-message" };
  });
  assert.equal(client.report("native-lane", "lane", {}, { lane: true }), true);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native-lane")?.ready);
  assert.deepEqual(fixture.reports[0].capabilities, { lane: true });

  fixture.write({ jsonrpc: "2.0", id: "daemon.start", method: "lane.turn.start", params: {
    input_id: "input", body: "work", mode: "followup",
  } });
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.start"));
  assert.deepEqual(handled, [{
    nativeSessionID: "native-lane", method: "lane.turn.start",
    params: { input_id: "input", body: "work", mode: "followup" },
  }]);
  assert.deepEqual(fixture.responses.find((frame) => frame.id === "daemon.start").result, { native_message_id: "product-message" });

  fixture.write({ jsonrpc: "2.0", id: "daemon.invalid", method: "lane.turn.start", params: { input_id: "input" } });
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.invalid"));
  assert.equal(fixture.responses.find((frame) => frame.id === "daemon.invalid").error.code, -32602);
});

test("lane handlers must return the exact native result shape", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop()); let result;
  client.handleLaneRequests(() => result);
  client.report("native-lane", "lane", {}, { lane: true });
  await until(() => fixture.reports.length === 1 && client.sessions.get("native-lane")?.ready);
  const cyclic = {}; cyclic.self = cyclic;
  const sparse = []; sparse.length = 1;
  const nonEnumerable = { result: "done", reason: {} };
  Object.defineProperty(nonEnumerable, "outcome", { value: "completed", enumerable: false });
  const invalidWait = [
    { outcome: "unknown", result: "done", reason: {} }, nonEnumerable,
    ...[undefined, () => {}, Symbol("x"), Number.NaN, Number.POSITIVE_INFINITY, new Date(0), sparse, cyclic]
      .map((reason) => ({ outcome: "completed", result: "done", reason })),
  ];
  const cases = [
    ["lane.turn.start", { input_id: "input", body: "work", mode: "followup" }, { native_message_id: "native" }, [{ native_message_id: "native", extra: true }]],
    ["lane.turn.wait", { native_message_id: "native" }, { outcome: "completed", result: "done", reason: { kind: "completed" } }, invalidWait],
    ["lane.turn.interrupt", {}, {}, [{ extra: true }]], ["lane.session.archive", {}, {}, [null]],
  ];
  let sequence = 0;
  async function invoke(method, params, value) {
    result = value; const id = `${method}.${sequence++}`;
    fixture.write({ jsonrpc: "2.0", id, method, params });
    await until(() => fixture.responses.some((frame) => frame.id === id));
    return fixture.responses.find((frame) => frame.id === id);
  }
  for (const [method, params, valid, invalids] of cases) {
    assert.deepEqual((await invoke(method, params, valid)).result, valid);
    for (const invalid of invalids) {
      const error = (await invoke(method, params, invalid)).error;
      assert.equal(error.code, -32006); assert.match(error.data.agent_sessions_bug_report, /github\.com\/antst\/agent-sessions/u);
    }
  }
});

test("numeric JSON-RPC ids are limited to safe mathematical integers", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 }); t.after(() => client.stop());
  client.on("message", (message) => client.acceptMessage(message.messageID)); client.report("native", "worker");
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  for (const [rawID, messageID, expectedID] of [
    ["9007199254740991", "safe-max", 9007199254740991], ["-9007199254740991", "safe-min", -9007199254740991], ["9007199254740991.1", "rounded-safe", 9007199254740991], ["1e3", "safe-exponent", 1000],
  ]) {
    fixture.writeRaw(`{"jsonrpc":"2.0","id":${rawID},"method":"message.deliver","params":{"message_id":"${messageID}","from":{"uuid":"parent","name":"parent","product":"codex","groups":["team"]},"body":"body"}}`);
    await until(() => fixture.responses.some((frame) => frame.id === expectedID));
  }
  for (const rawID of ["9007199254740992", "-9007199254740992", "1.5", "1e-3"]) {
    const reportCount = fixture.reports.length; fixture.writeRaw(`{"jsonrpc":"2.0","id":${rawID},"method":"message.deliver","params":{"message_id":"invalid","from":{"uuid":"parent","name":"parent","product":"codex","groups":["team"]},"body":"body"}}`);
    await until(() => fixture.reports.length === reportCount + 1 && client.sessions.get("native")?.ready);
  }
  const version = client.callTool("native", "version", "peers.list", {}); await until(() => fixture.requests.some((frame) => frame.id === "session.version"));
  fixture.writeRaw('{"jsonrpc":"2.0","id":"session.version","error":{"code":-32004,"message":"Unsupported protocol version","data":{"supported":1,"received":9007199254740991.1}}}');
  await assert.rejects(version, (error) => error.code === -32004 && error.data.received === Number.MAX_SAFE_INTEGER);
});

test("hello acknowledgement publishes readiness before a coalesced lane request", async (t) => {
  const fixture = await server(t, { jsonrpc: "2.0", id: "daemon.first", method: "lane.turn.start", params: {
    input_id: "first", body: "work", mode: "followup",
  } });
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  client.handleLaneRequests(() => ({ native_message_id: "product-first" }));
  assert.equal(client.report("native-lane", "lane", {}, { lane: true }), true);
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.first"));
  assert.deepEqual(fixture.responses.find((frame) => frame.id === "daemon.first").result, { native_message_id: "product-first" });
});

test("inactive client never connects", async () => {
  let connects = 0;
  const client = new LiveSessionClient({ env: {}, connect: () => { connects += 1; } });
  assert.equal(readConfiguration({}).reason, "missing-presence-socket");
  assert.deepEqual(await client.start(), { active: false, reason: "missing-presence-socket" });
  await assert.rejects(client.callTool("native", "call", "peers.list", {}), InactiveError);
  assert.equal(connects, 0);
});

test("socket discovery honors explicit, state-root, XDG, then home", () => {
  assert.equal(readConfiguration({
    AGENT_SESSIONS_PRESENCE_SOCKET: "/explicit.sock", AGENT_SESSIONS_STATE_ROOT: "/state",
    XDG_STATE_HOME: "/xdg", HOME: "/home", AGENT_SESSIONS_PRODUCT: "pi", AGENT_SESSIONS_GROUPS: "[]",
  }).socketPath, "/explicit.sock");
  assert.equal(readConfiguration({
    AGENT_SESSIONS_STATE_ROOT: "/state", XDG_STATE_HOME: "/xdg", HOME: "/home",
    AGENT_SESSIONS_PRODUCT: "pi", AGENT_SESSIONS_GROUPS: "[]",
  }).socketPath, "/state/run/presence.sock");
  assert.equal(readConfiguration({
    XDG_STATE_HOME: "/xdg", HOME: "/home", AGENT_SESSIONS_PRODUCT: "pi", AGENT_SESSIONS_GROUPS: "[]",
  }).socketPath, "/xdg/agent-sessions/run/presence.sock");
  assert.equal(readConfiguration({
    HOME: "/home", AGENT_SESSIONS_PRODUCT: "pi", AGENT_SESSIONS_GROUPS: "[]",
  }).socketPath, "/home/.local/state/agent-sessions/run/presence.sock");
});

function env(socketPath) {
  return { AGENT_SESSIONS_PRESENCE_SOCKET: socketPath, AGENT_SESSIONS_PRODUCT: "pi", AGENT_SESSIONS_GROUPS: '["team","team"]' };
}

async function server(t, firstRequest = null) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-live-"));
  const socketPath = path.join(root, "presence.sock");
  const reports = [], requests = [], responses = [], sockets = [];
  let socket;
  const listener = net.createServer((connection) => {
    socket = connection;
    sockets.push(connection);
    connection.setEncoding("utf8");
    connection.on("error", () => {});
    let buffer = "", reported = false;
    connection.on("data", (chunk) => {
      buffer += chunk;
      for (;;) {
        const newline = buffer.indexOf("\n");
        if (newline < 0) return;
        const frame = JSON.parse(buffer.slice(0, newline));
        buffer = buffer.slice(newline + 1);
        if (!reported) {
          if (frame.jsonrpc !== "2.0" || frame.method !== "session.hello") return connection.destroy();
          reported = true;
          reports.push(frame.params);
          const acknowledgement = `${JSON.stringify({ jsonrpc: "2.0", id: frame.id, result: {} })}\n`;
          const request = firstRequest === null ? "" : `${JSON.stringify(firstRequest)}\n`;
          connection.write(acknowledgement + request);
        }
        else if (frame.method) requests.push(frame); else responses.push(frame);
      }
    });
  });
  await new Promise((resolve, reject) => { listener.once("error", reject); listener.listen(socketPath, resolve); });
  t.after(async () => {
    for (const value of sockets) value.destroy();
    await new Promise((resolve) => listener.close(resolve));
    fs.rmSync(root, { recursive: true, force: true });
  });
  return {
    path: socketPath, reports, requests, responses, get socket() { return socket; },
    write(frame) { socket.write(`${JSON.stringify(frame)}\n`); },
    writeRaw(line) { socket.write(`${line}\n`); },
  };
}

async function until(predicate) {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail("condition did not become true");
}
