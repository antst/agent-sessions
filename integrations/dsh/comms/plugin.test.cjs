"use strict";

const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const packageRoot = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-comms-test-"));
fs.mkdirSync(path.join(packageRoot, "shared"));
fs.copyFileSync(path.join(__dirname, "plugin.cjs"), path.join(packageRoot, "plugin.cjs"));
fs.copyFileSync(path.join(__dirname, "..", "..", "shared", "live-session.js"), path.join(packageRoot, "shared", "live-session.cjs"));
const { createCommsRuntime, launchIdentity } = require(path.join(packageRoot, "plugin.cjs"));
test.after(() => fs.rmSync(packageRoot, { recursive: true, force: true }));

function launchEnvironment(values = {}) {
  return { get(key) { return Object.hasOwn(values, key) ? { value: values[key] } : undefined; } };
}

class FakeClient extends EventEmitter {
  constructor() {
    super();
    this.reports = [];
    this.accepted = [];
    this.rejected = [];
    this.calls = [];
  }
  report(...argumentsValue) { this.reports.push(argumentsValue); return true; }
  closeSession() {}
  handleLaneRequests(handler) { this.laneHandler = handler; }
  acceptMessage(...argumentsValue) { this.accepted.push(argumentsValue); }
  rejectMessage(...argumentsValue) { this.rejected.push(argumentsValue); }
  callTool(...argumentsValue) { this.calls.push(argumentsValue); return Promise.resolve({}); }
  updateName() {}
  stop() { return Promise.resolve(); }
}

test("group send uses the one message.send operation unchanged", async () => {
  const { agent, client, runtime } = harness();
  const root = agent();
  runtime.present(root);
  await runtime.callTool(root, "message.send", { group: "team", message: "hello all" });
  assert.deepEqual(client.calls, [["session-native", "dsh-tool-1", "message.send", { group: "team", message: "hello all" }]]);
});

function harness(environmentValues = {}, config = {}) {
  const listeners = new Map();
  const roots = [];
  let sequence = 0;
  const environment = launchEnvironment(environmentValues);
  const ctx = {
    get(name) { return name === "launchEnvironment" ? environment : undefined; },
    agents: {
      roots: () => [...roots],
      get: (id) => roots.find((agent) => agent.session.id === id),
    },
    on(name, callback) { listeners.set(name, callback); return () => listeners.delete(name); },
  };
  const client = new FakeClient();
  const createUserMessage = ({ content, source }) => ({ id: `dsh-${++sequence}`, role: "user", content, source });
  const runtime = createCommsRuntime(ctx, createUserMessage, config, { client });
  function agent(id = "session-native", status = "idle") {
    const value = {
      id,
      status,
      options: { provider: "deepseek", model: "chat" },
      session: { id, header: { cwd: "/product/cwd" } },
      steer(message) {
        listeners.get("session/event")(this.session, {
          type: "agent/inbox/spliced", data: { target: "next-step", start: 0, inserted: [message] },
        });
      },
    };
    roots.push(value);
    return value;
  }
  return { agent, client, runtime };
}

test("zero-config roots report native identity, empty name and product cwd", () => {
  const { agent, client, runtime } = harness();
  const root = agent();
  assert.equal(runtime.present(root), true);
  assert.deepEqual(client.reports, [[
    "session-native", "", { cwd: "/product/cwd", model: "deepseek/chat" }, {}, [],
  ]]);
});

test("matching launch identity wins and a provisional launch identity never does", () => {
  const exact = launchEnvironment({
    AGENT_SESSIONS_SESSION_ID: "native",
    AGENT_SESSIONS_SESSION_NAME: "named",
    AGENT_SESSIONS_GROUPS: '["launch"]',
  });
  assert.deepEqual(launchIdentity(exact, { session: { id: "native" } }, ["profile"]), {
    sessionID: "native", name: "named", groups: ["launch"],
  });
  assert.deepEqual(launchIdentity(exact, { session: { id: "other" } }, ["profile"]), {
    sessionID: "other", name: "", groups: ["profile"],
  });
});

for (const status of ["idle", "running"]) {
  test(`message delivery uses the native steer receipt while ${status}`, () => {
    const { agent, client, runtime } = harness();
    const root = agent("native", status);
    runtime.present(root);
    client.emit("message", {
      messageID: `message-${status}`,
      nativeSessionID: "native",
      from: { uuid: "sender", name: "sender", product: "claude", groups: ["group"] },
      body: "hello",
    });
    assert.deepEqual(client.rejected, []);
    assert.deepEqual(client.accepted, [[`message-${status}`, { native_message_id: "dsh-1" }]]);
  });
}
