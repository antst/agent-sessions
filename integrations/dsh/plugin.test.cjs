"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const packageRoot = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-test-"));
fs.mkdirSync(path.join(packageRoot, "shared"));
fs.copyFileSync(path.join(__dirname, "plugin.cjs"), path.join(packageRoot, "plugin.cjs"));
fs.copyFileSync(path.join(__dirname, "..", "shared", "live-session.js"), path.join(packageRoot, "shared", "live-session.cjs"));
const { createRuntime } = require(path.join(packageRoot, "plugin.cjs"));
test.after(() => fs.rmSync(packageRoot, { recursive: true, force: true }));

test("bundle declares noninteractive presets for both permission modes", () => {
  const patch = fs.readFileSync(path.join(__dirname, "cordis.patch.yml"), "utf8");
  assert.match(patch, /workspace-write-noninteractive:\s*\n\s+sandbox: workspace-write\s*\n\s+approval: never/u);
  assert.match(patch, /danger-full-access:\s*\n\s+sandbox: danger-full-access\s*\n\s+approval: never/u);
});

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
  const runtime = createRuntime(ctx, createUserMessage, agent);
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
  emit("turn/end", { turn: 4, reason: "completed" });
  assert.deepEqual(await runtime.wait(nativeID), { outcome: "completed", result: "DSH_OK", reason: "completed" });
  assert.equal(runtime.byInput.size, 0);
});

test("steer acknowledges its native receipt without retaining a waiter", () => {
  const { runtime } = harness();
  assert.equal(runtime.submit("steer-1", "replace it", "steer", false), "dsh-1");
  assert.equal(runtime.byInput.size, 0);
});

test("canceled inbox removal resolves the exact queued input", async () => {
  const { agent, emit, runtime } = harness();
  const nativeID = runtime.submit("input-cancel", "later", "followup");
  emit("agent/inbox/spliced", {
    target: "next-turn", start: 0, removedCount: 1, inserted: [], outcome: "canceled",
  });
  agent.inbox.nextTurn.splice(0, 1);
  assert.deepEqual(await runtime.wait(nativeID), {
    outcome: "failed", result: "", reason: { type: "canceled", outcome: "canceled" },
  });
});
