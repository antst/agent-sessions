"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const { InactiveError, LiveSessionClient, readConfiguration } = require("./live-session.js");

test("default reconnect cadence is two seconds", () => {
  const client = new LiveSessionClient({ env: {} });
  assert.equal(client.reconnectMs, 2000);
});

test("one socket reports, calls, updates, and receives messages", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 }); t.after(() => client.stop());
  assert.deepEqual(await client.start(), { active: true });
  assert.equal(client.report("native", "before", { cwd: "/work" }), true);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  assert.deepEqual(fixture.reports[0], { protocol: 1, uuid: "native", name: "before", groups: ["team", "team"], product: "pi", info: { cwd: "/work" } });

  const call = client.callTool("native", "tool-one", "peers.list", {});
  await until(() => fixture.requests.some((frame) => frame.method === "peers.list"));
  fixture.write({ jsonrpc: "2.0", id: fixture.requests[0].id, result: { peers: 2 } });
  assert.deepEqual(await call, { peers: 2 });

  const groupCall = client.callTool("native", "group-one", "message.send", { group: "team", message: "hello all" });
  await until(() => fixture.requests.some((frame) => frame.method === "message.send"));
  const groupRequest = fixture.requests.find((frame) => frame.method === "message.send");
  assert.deepEqual(groupRequest.params, { group: "team", message: "hello all" });
  fixture.write({ jsonrpc: "2.0", id: groupRequest.id, result: { message_id: "group-message", deliveries: [] } });
  assert.deepEqual(await groupCall, { message_id: "group-message", deliveries: [] });

  const laneCall = client.callTool("native", "lane-one", "lane.status", { product: "qwen", arguments: ["worker"] });
  await until(() => fixture.requests.some((frame) => frame.method === "lane.status"));
  const laneRequest = fixture.requests.find((frame) => frame.method === "lane.status");
  assert.equal(laneRequest.params.cwd, "/work");
  fixture.write({ jsonrpc: "2.0", id: laneRequest.id, result: { type: "lane.status" } });
  assert.deepEqual(await laneCall, { type: "lane.status" });

  client.updateName("native", "after");
  await until(() => fixture.requests.some((frame) => frame.method === "session.update"));
  const update = fixture.requests.find((frame) => frame.method === "session.update");
  assert.equal(update.params.name, "after");
  fixture.write({ jsonrpc: "2.0", id: update.id, result: {} });

  const delivered = new Promise((resolve) => client.once("message", resolve));
  fixture.write({ jsonrpc: "2.0", id: "daemon.message", method: "message.deliver", params: {
    message_id: "message", from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "hello",
  } });
  assert.deepEqual(await delivered, {
    messageID: "message", nativeSessionID: "native",
    from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "hello",
  });
  client.acceptMessage("message");
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.message"));

  const rejected = new Promise((resolve) => client.once("message", resolve));
  fixture.write({ jsonrpc: "2.0", id: "daemon.failed", method: "message.deliver", params: {
    message_id: "failed", from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "fail",
  } });
  await rejected;
  client.rejectMessage("failed", "product rejected exact input");
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.failed"));
  assert.deepEqual(fixture.responses.find((frame) => frame.id === "daemon.failed").error, {
    code: -32006, message: "product rejected exact input", data: {
      detail: "product rejected exact input",
      agent_sessions_bug_report: "If Agent Sessions behaves contrary to this description or its documentation and the gh CLI is authorized in your environment, you are encouraged to open an issue on github.com/antst/agent-sessions with gh issue create, including the exact command, observed behavior, and expected behavior.",
    },
  });

  const nativeFailure = new Promise((resolve) => client.once("message", resolve));
  fixture.write({ jsonrpc: "2.0", id: "daemon.native-failed", method: "message.deliver", params: {
    message_id: "native-failed", from: { uuid: "parent", name: "parent", product: "codex", groups: ["team"] }, body: "fail natively",
  } });
  await nativeFailure;
  client.rejectMessage("native-failed", { code: -32006, message: "native exact failure", data: { detail: "native exact failure" } });
  await until(() => fixture.responses.some((frame) => frame.id === "daemon.native-failed"));
  assert.deepEqual(fixture.responses.find((frame) => frame.id === "daemon.native-failed").error, {
    code: -32006, message: "native exact failure", data: { detail: "native exact failure" },
  });
});

test("one session may report product-owned groups instead of process launch groups", async (t) => {
  const fixture = await server(t);
  const client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 });
  t.after(() => client.stop());
  assert.equal(client.report("native", "", { cwd: "/native" }, {}, ["profile-group"]), true);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  assert.deepEqual(fixture.reports[0].groups, ["profile-group"]);
  assert.equal(client.report("other", "", { cwd: "/native" }, {}, [1]), false);
});

test("identity is local and group replacement reconnects only while quiescent", async (t) => {
  const fixture = await server(t), client = new LiveSessionClient({ env: env(fixture.path), reconnectMs: 5 }); t.after(() => client.stop());
  client.report("native", "named", { cwd: "/work" }, {}, ["first"]);
  await until(() => fixture.reports.length === 1 && client.sessions.get("native")?.ready);
  const identity = await client.callTool("native", "identity", "identity", {});
  assert.deepEqual(identity, { uuid: "native", name: "named", groups: ["first"] });
  identity.groups.push("mutated");
  assert.deepEqual((await client.callTool("native", "again", "identity", {})).groups, ["first"]);
  assert.equal(fixture.requests.length, 0);
  const pending = client.callTool("native", "pending", "peers.list", {});
  await until(() => fixture.requests.length === 1);
  assert.throws(() => client.replaceGroups("native", ["second"]), /in-flight work/u);
  fixture.write({ jsonrpc: "2.0", id: fixture.requests[0].id, result: {} });
  await pending;
  const delivered = new Promise((resolve) => client.once("message", resolve));
  fixture.write({ jsonrpc: "2.0", id: "delivery", method: "message.deliver", params: {
    message_id: "inbound", from: { uuid: "parent", name: "", product: "codex", groups: [] }, body: "work",
  } });
  await delivered;
  assert.throws(() => client.replaceGroups("native", ["second"]), /in-flight work/u);
  client.acceptMessage("inbound");
  client.replaceGroups("native", ["second"]);
  assert.throws(() => client.replaceGroups("native", ["third"]), InactiveError);
  await until(() => fixture.reports.length === 2 && client.sessions.get("native")?.ready);
  assert.deepEqual(fixture.reports[1], { ...fixture.reports[0], groups: ["second"] });
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
  return { path: socketPath, reports, requests, responses, get socket() { return socket; }, write(frame) { socket.write(`${JSON.stringify(frame)}\n`); } };
}

async function until(predicate) {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail("condition did not become true");
}
