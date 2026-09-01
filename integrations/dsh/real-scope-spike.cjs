#!/usr/bin/env node
"use strict";

// Keyless real-product topology spike. This is intentionally opt-in because it
// installs and boots the exact external DSH tuple. It creates only disposable
// Agent Sessions-owned profiles below HOME/XDG state and removes them on exit.

const assert = require("node:assert/strict");
const childProcess = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const readline = require("node:readline");
const { FrameDecoder, CONTRACT_REVISION, encodeFrame, makeFrame } = require("../shared/component/protocol.js");

const PINNED = "0.1.2-alpha.3";
const PINNED_PNPM = "10.28.1";
const integrationRoot = __dirname;
const dshCLI = process.env.DSH_REAL_CLI;
const pnpmCLI = process.env.DSH_REAL_PNPM || "pnpm";
const pnpmStore = process.env.DSH_REAL_PNPM_STORE;

if (!dshCLI || !path.isAbsolute(dshCLI)) {
  throw new Error("set DSH_REAL_CLI to the absolute 0.1.2-alpha.3 dsh executable");
}

function output(command, args, options = {}) {
  return childProcess.execFileSync(command, args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], ...options }).trim();
}

assert.equal(output(dshCLI, ["--version"]), PINNED, "real DSH CLI tuple mismatch");
assert.equal(output(pnpmCLI, ["--version"]), PINNED_PNPM, "real pnpm tuple mismatch");

const stateBase = process.env.XDG_STATE_HOME || path.join(os.homedir(), ".local", "state");
fs.mkdirSync(path.join(stateBase, "agent-sessions"), { recursive: true, mode: 0o700 });
const spikeRoot = fs.mkdtempSync(path.join(stateBase, "agent-sessions", "dsh-scope-spike-"));
const dshHome = path.join(spikeRoot, "dsh-home");
const profilesRoot = path.join(dshHome, "profiles");
const helperRoot = path.join(spikeRoot, "scope-helper");
const assembledRoot = path.join(spikeRoot, "package");
const assembledPluginRoot = path.join(assembledRoot, "integrations", "dsh");
const socketPath = path.join(spikeRoot, "component.sock");
const captured = new Map();

function writeJSON(file, value) {
  fs.writeFileSync(file, JSON.stringify(value, null, 2) + "\n", { mode: 0o600 });
}

function prepareHelper() {
  fs.mkdirSync(helperRoot, { recursive: true, mode: 0o700 });
  writeJSON(path.join(helperRoot, "package.json"), {
    name: "@agent-sessions/dsh-scope-spike-helper", version: PINNED, private: true,
    type: "commonjs", main: "index.cjs",
  });
  fs.writeFileSync(path.join(helperRoot, "index.cjs"), `"use strict";
const fs = require("node:fs");
exports.inject = ["agents", "sessionTitle"];
exports.name = "agent-sessions-scope-spike-helper";
exports.apply = (ctx) => {
  let first = true;
  ctx.on("agent/created", ({ agent }) => {
    if (!first) return;
    first = false;
    ctx.sessionTitle.rename(agent.session, "Agent Sessions scope spike");
    const witness = process.env.AGENT_SESSIONS_DSH_POLICY_WITNESS;
    if (witness) {
      // Observe only after the complete synchronous agent/created dispatch.
      // This is still before ACP session/new or session/resume returns and
      // therefore before any prompt can be admitted.
      queueMicrotask(() => {
        const events = agent.session.events;
        const sandbox = events.findLast((event) => event.type === "sandbox/mode")?.data?.mode;
        const approval = events.findLast((event) => event.type === "approval/policy")?.data?.policy;
        fs.appendFileSync(witness, JSON.stringify({ session_id: agent.id, sandbox, approval }) + "\\n");
      });
    }
  });
};
`, { mode: 0o600 });
}

function preparePluginAssembly() {
  const sharedTarget = path.join(assembledRoot, "integrations", "shared", "component");
  fs.mkdirSync(assembledPluginRoot, { recursive: true, mode: 0o700 });
  fs.mkdirSync(sharedTarget, { recursive: true, mode: 0o700 });
  for (const file of ["package.json", "plugin.cjs", "mcp-env.cjs"]) {
    fs.copyFileSync(path.join(integrationRoot, file), path.join(assembledPluginRoot, file));
  }
  for (const file of ["client.js", "protocol.js"]) {
    fs.copyFileSync(path.join(integrationRoot, "..", "shared", "component", file), path.join(sharedTarget, file));
  }
}

function prepareProfile(profile) {
  const root = path.join(profilesRoot, profile);
  fs.mkdirSync(root, { recursive: true, mode: 0o700 });
  writeJSON(path.join(root, "package.json"), {
    name: `@agent-sessions/dsh-profile-${profile}`, version: PINNED, private: true,
    packageManager: `pnpm@${PINNED_PNPM}`,
    dependencies: {
      "@deepseek-ai/dsh-acp-app": PINNED,
      "@deepseek-ai/dsh-llm": PINNED,
      "@deepseek-ai/dsh-sandbox-policy": PINNED,
      "@deepseek-ai/dsh-tools": PINNED,
      "@deepseek-ai/dsh-user-approval": PINNED,
      "@agent-sessions/dsh-plugin": `link:${assembledPluginRoot}`,
      "@agent-sessions/dsh-scope-spike-helper": `link:${helperRoot}`,
    },
    dsh: { profile: { bundles: ["@deepseek-ai/dsh-base", "@deepseek-ai/dsh-acp-app"], patchReload: "startup" } },
  });
  fs.writeFileSync(path.join(root, "cordis.yml"), "[]\n", { mode: 0o600 });
  fs.writeFileSync(path.join(root, "cordis.patch.yml"), `- insert:
    - id: agent-sessions
      name: '@agent-sessions/dsh-plugin'
    - id: agent-sessions-scope-spike-helper
      name: '@agent-sessions/dsh-scope-spike-helper'
`, { mode: 0o600 });
  fs.writeFileSync(path.join(root, "pnpm-workspace.yaml"), "packages:\n  - .\n\nnodeLinker: hoisted\nautoInstallPeers: false\n", { mode: 0o600 });
}

function prepareProfiles() {
  fs.mkdirSync(profilesRoot, { recursive: true, mode: 0o700 });
  preparePluginAssembly();
  prepareHelper();
  prepareProfile("scope-a");
  prepareProfile("scope-b");
  for (const profile of ["scope-a", "scope-b"]) {
    try {
      const installArguments = ["install", "--offline", "--no-frozen-lockfile"];
      if (pnpmStore) installArguments.push("--store-dir", pnpmStore);
      childProcess.execFileSync(pnpmCLI, installArguments, {
        cwd: path.join(profilesRoot, profile), env: { ...process.env }, stdio: ["ignore", "pipe", "pipe"],
      });
    } catch (error) {
      throw new Error("prepare exact DSH scope profile " + profile + ": " + [error?.stderr, error?.stdout, error?.message].filter(Boolean).map(String).join("\n"));
    }
  }
  fs.symlinkSync(path.join(profilesRoot, "scope-a", "node_modules"), path.join(assembledRoot, "node_modules"));
}

function startBroker() {
  return new Promise((resolve, reject) => {
    const server = net.createServer((socket) => {
      const decoder = new FrameDecoder();
      let outboundSequence = 0;
      socket.on("data", (chunk) => {
        try {
          for (const frame of decoder.push(chunk)) {
            const attachment = frame.payload?.attachment_id || captured.get(socket)?.attachment;
            if (!captured.has(socket)) captured.set(socket, { attachment, frames: [] });
            const record = captured.get(socket);
            if (attachment) record.attachment = attachment;
            record.frames.push(frame);
            if (frame.type === "bootstrap") {
              assert.equal(frame.payload.product_id, "dsh");
              assert.equal(frame.payload.component_version, CONTRACT_REVISION);
              socket.write(encodeFrame(makeFrame("ready", "ready-" + record.attachment, ++outboundSequence, {
                binding_id: "binding-" + record.attachment,
                attachment_id: record.attachment,
                daemon_generation: 1,
                protocol_version: 1,
                max_frame_bytes: 1024 * 1024,
                heartbeat_interval_ms: 60000,
              })));
            } else if (frame.type === "heartbeat") {
              socket.write(encodeFrame(makeFrame("heartbeat.ack", frame.id, ++outboundSequence, frame.payload)));
            }
          }
        } catch (error) {
          reject(error);
          socket.destroy();
        }
      });
    });
    server.once("error", reject);
    server.listen(socketPath, () => resolve(server));
  });
}

function withTimeout(promise, label, milliseconds = 20000) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(label + " timed out")), milliseconds);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

function runACP(profile, attachment, options = {}) {
  const work = path.join(spikeRoot, "work-" + profile);
  fs.mkdirSync(work, { recursive: true, mode: 0o700 });
  const managedEnvironment = attachment ? {
    AGENT_SESSIONS_COMPONENT_SOCKET: socketPath,
    AGENT_SESSIONS_PRODUCT_ID: "dsh",
    AGENT_SESSIONS_ATTACHMENT_ID: attachment,
    AGENT_SESSIONS_BOOTSTRAP_CAPABILITY_ID: "capability-" + attachment,
    AGENT_SESSIONS_BOOTSTRAP_VALUE: "one-shot-" + attachment,
    AGENT_SESSIONS_COMPONENT_VERSION: CONTRACT_REVISION,
  } : {};
  const laneEnvironment = options.lanePolicy ? {
    AGENT_SESSIONS_DSH_LANE_POLICY: options.lanePolicy,
    AGENT_SESSIONS_DSH_POLICY_WITNESS: options.policyWitness,
  } : {};
  const child = childProcess.spawn(dshCLI, ["--profile", profile], {
    cwd: work,
    env: {
      ...process.env,
      DSH_HOME: options.dshHome || dshHome,
      DSH_PERMISSION_MODE: options.lanePolicy?.split(":", 1)[0] || "workspace-write",
      ...managedEnvironment,
      ...laneEnvironment,
      NO_COLOR: "1",
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  const pending = new Map();
  let requestID = 0;
  let stderr = "";
  child.stderr.on("data", (chunk) => { stderr = (stderr + chunk).slice(-8192); });
  readline.createInterface({ input: child.stdout }).on("line", (line) => {
    let frame;
    try { frame = JSON.parse(line); } catch { return; }
    if (frame.id !== undefined && (frame.result !== undefined || frame.error !== undefined)) {
      const resolver = pending.get(frame.id);
      if (resolver) {
        pending.delete(frame.id);
        resolver(frame);
      }
    }
  });
  const request = (method, params) => {
    const id = ++requestID;
    const result = new Promise((resolve) => pending.set(id, resolve));
    child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
    return withTimeout(result, profile + " " + method).catch((error) => {
      throw new Error(error.message + (stderr ? "\n" + stderr : ""));
    });
  };
  const stop = async () => {
    child.stdin.end();
    child.kill("SIGTERM");
    if (child.exitCode === null && child.signalCode === null) {
      await new Promise((resolve) => {
        const timer = setTimeout(() => { child.kill("SIGKILL"); resolve(); }, 3000);
        child.once("exit", () => { clearTimeout(timer); resolve(); });
      });
    }
  };
  return { child, request, stderr: () => stderr, stop, work };
}

function treeFingerprint(root) {
  const entries = [];
  const visit = (directory) => {
    for (const name of fs.readdirSync(directory).sort()) {
      const absolute = path.join(directory, name);
      const relative = path.relative(root, absolute);
      const stat = fs.lstatSync(absolute);
      if (stat.isSymbolicLink()) entries.push([relative, "link", fs.readlinkSync(absolute)]);
      else if (stat.isDirectory()) {
        entries.push([relative, "directory"]);
        visit(absolute);
      } else if (stat.isFile()) {
        entries.push([relative, "file", stat.mode & 0o777, crypto.createHash("sha256").update(fs.readFileSync(absolute)).digest("hex")]);
      } else entries.push([relative, "other"]);
    }
  };
  visit(root);
  return JSON.stringify(entries);
}

async function waitForAnnouncement(attachment) {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    for (const record of captured.values()) {
      if (record.attachment === attachment) {
        const announce = record.frames.find((frame) => frame.type === "session.announce");
        if (announce) return announce;
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error("component announcement timed out for " + attachment);
}

async function exercise(profile, attachment) {
  const acp = runACP(profile, attachment);
  try {
    const initialized = await acp.request("initialize", {
      protocolVersion: 1,
      clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } },
      clientInfo: { name: "agent-sessions-scope-spike", version: PINNED },
    });
    assert.equal(initialized.result?.protocolVersion, 1, acp.stderr());
    const first = await acp.request("session/new", { cwd: acp.work, mcpServers: [] });
    assert.equal(typeof first.result?.sessionId, "string", acp.stderr());
    const announce = await waitForAnnouncement(attachment);
    assert.equal(announce.payload.native_session_id, first.result.sessionId);
    assert.equal(announce.payload.cwd, acp.work);
    assert.equal(announce.payload.native_name, "Agent Sessions scope spike");

    const sibling = await acp.request("session/new", { cwd: acp.work, mcpServers: [] });
    assert.ok(sibling.error, "second native session unexpectedly succeeded in the managed profile");
    for (const record of captured.values()) {
      if (record.attachment !== attachment) continue;
      assert.equal(record.frames.filter((frame) => frame.type === "session.announce").length, 1);
    }
    const closed = await acp.request("session/close", { sessionId: first.result.sessionId });
    assert.equal(closed.error, undefined, acp.stderr());
    return first.result.sessionId;
  } finally {
    await acp.stop();
  }
}

async function exerciseDisposableDoctorProfile() {
  const configuredBefore = treeFingerprint(dshHome);
  const doctorHome = path.join(spikeRoot, "doctor-home");
  fs.mkdirSync(path.join(doctorHome, "profiles"), { recursive: true, mode: 0o700 });
  fs.symlinkSync(path.join(profilesRoot, "scope-a"), path.join(doctorHome, "profiles", "acp"));
  const acp = runACP("acp", "", { dshHome: doctorHome });
  try {
    const initialized = await acp.request("initialize", {
      protocolVersion: 1,
      clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } },
      clientInfo: { name: "agent-sessions-doctor-spike", version: PINNED },
    });
    assert.equal(initialized.result?.protocolVersion, 1, acp.stderr());
    const created = await acp.request("session/new", { cwd: acp.work, mcpServers: [] });
    assert.equal(typeof created.result?.sessionId, "string", acp.stderr());
    const closed = await acp.request("session/close", { sessionId: created.result.sessionId });
    assert.equal(closed.error, undefined, acp.stderr());
  } finally {
    await acp.stop();
  }
  assert.equal(treeFingerprint(dshHome), configuredBefore, "disposable doctor mutated the configured DSH_HOME");
  assert.equal(fs.existsSync(path.join(doctorHome, "storages")), true, "real session/new did not exercise isolated durable state");
  fs.rmSync(doctorHome, { recursive: true, force: true });
  assert.equal(fs.existsSync(doctorHome), false, "disposable doctor DSH_HOME remains");
}

async function exercisePersistedPolicyOverride() {
  const witness = path.join(spikeRoot, "policy-witness.jsonl");
  const first = runACP("scope-a", "", { lanePolicy: "danger-full-access:never", policyWitness: witness });
  let sessionID;
  try {
    await first.request("initialize", {
      protocolVersion: 1, clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } },
      clientInfo: { name: "agent-sessions-policy-spike", version: PINNED },
    });
    const created = await first.request("session/new", { cwd: first.work, mcpServers: [] });
    sessionID = created.result?.sessionId;
    assert.equal(typeof sessionID, "string", first.stderr());
    await first.request("session/close", { sessionId: sessionID });
  } finally {
    await first.stop();
  }
  const resumed = runACP("scope-a", "", { lanePolicy: "workspace-write:ask", policyWitness: witness });
  try {
    await resumed.request("initialize", {
      protocolVersion: 1, clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } },
      clientInfo: { name: "agent-sessions-policy-spike", version: PINNED },
    });
    const result = await resumed.request("session/resume", { sessionId: sessionID, cwd: resumed.work, mcpServers: [] });
    assert.equal(result.error, undefined, resumed.stderr());
    await resumed.request("session/close", { sessionId: sessionID });
  } finally {
    await resumed.stop();
  }
  const observations = fs.readFileSync(witness, "utf8").trim().split("\n").map(JSON.parse);
  assert.deepEqual(observations.map(({ sandbox, approval }) => ({ sandbox, approval })), [
    { sandbox: "danger-full-access", approval: "never" },
    { sandbox: "workspace-write", approval: "ask" },
  ]);
}

(async () => {
  let server;
  try {
    prepareProfiles();
    server = await startBroker();
    const first = await exercise("scope-a", "attachment-a");
    const second = await exercise("scope-b", "attachment-b");
    assert.notEqual(first, second);
    await exercisePersistedPolicyOverride();
    await exerciseDisposableDoctorProfile();
    process.stdout.write(JSON.stringify({
      status: "PASS",
      tuple: PINNED,
      pnpm: PINNED_PNPM,
      profiles: 2,
      exact_sessions: 2,
      sibling_sessions_visible: 0,
      disposable_doctor_store_growth: 0,
      persisted_policy_override: "danger-full-access:never -> workspace-write:ask",
      socket_root: "HOME/XDG state",
    }) + "\n");
  } finally {
    await new Promise((resolve) => server ? server.close(resolve) : resolve());
    fs.rmSync(spikeRoot, { recursive: true, force: true });
  }
})().catch((error) => {
  process.stderr.write(String(error?.stack || error) + "\n");
  process.exitCode = 1;
});
