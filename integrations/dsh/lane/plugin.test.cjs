"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const packageRoot = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-lane-test-"));
const commsRoot = path.join(packageRoot, "node_modules", "@agent-sessions", "dsh-comms");
fs.mkdirSync(commsRoot, { recursive: true });
fs.copyFileSync(path.join(__dirname, "plugin.cjs"), path.join(packageRoot, "plugin.cjs"));
fs.writeFileSync(path.join(commsRoot, "index.js"), 'module.exports = { serviceName: "agentSessionsComms" };\n');
const { createLaneRuntime } = require(path.join(packageRoot, "plugin.cjs"));
test.after(() => fs.rmSync(packageRoot, { recursive: true, force: true }));

function harness() {
  const session = {};
  let listener = null;
  let sequence = 0;
  const ctx = {
    on(_name, callback) { listener = callback; return () => { listener = null; }; },
  };
  const agent = {
    session,
    inbox: { nextStep: [], nextTurn: [] },
    followup(message) {
      listener(session, { type: "agent/inbox/spliced", data: { target: "next-turn", start: this.inbox.nextTurn.length, inserted: [message] } });
      this.inbox.nextTurn.push(message);
    },
    steer(message) {
      listener(session, { type: "agent/inbox/spliced", data: { target: "next-step", start: this.inbox.nextStep.length, inserted: [message] } });
      this.inbox.nextStep.push(message);
    },
  };
  const createUserMessage = ({ content, source }) => ({ id: `dsh-${++sequence}`, role: "user", content, source });
  const runtime = createLaneRuntime(ctx, createUserMessage, agent);
  const emit = (type, data) => listener(session, { type, data });
  return { agent, emit, runtime };
}

test("followup receipt binds consumption and the product terminal", async () => {
  const { emit, runtime } = harness();
  const nativeID = runtime.submit("input-1", "do it", "followup");
  assert.equal(nativeID, "dsh-1");
  assert.equal(runtime.byInput.get("input-1"), nativeID);
  emit("turn/start", { turn: 4 });
  emit("user/message", { id: nativeID });
  emit("assistant/message", { turn: 4, message: { content: [{ type: "text", text: "DSH_" }] } });
  emit("assistant/message", { turn: 4, message: { content: [{ type: "text", text: "OK" }] } });
  emit("turn/end", { turn: 4, reason: { kind: "completed" } });
  assert.deepEqual(await runtime.wait(nativeID), { outcome: "completed", result: "DSH_OK", reason: { kind: "completed" } });
  assert.equal(runtime.byInput.size, 0);
});

test("steer receives the same synchronous native splice receipt", () => {
  const { runtime } = harness();
  assert.equal(runtime.submit("steer-1", "replace it", "steer"), "dsh-1");
  assert.equal(runtime.byInput.get("steer-1"), "dsh-1");
});
