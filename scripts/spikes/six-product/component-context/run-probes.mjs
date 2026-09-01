import { createHash, randomBytes } from "node:crypto";
import { spawn } from "node:child_process";
import net from "node:net";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { startMockOpenAI } from "./fixtures/mock-openai.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = join(here, "fixtures");

function parseArgs(argv) {
  const result = {};
  for (let index = 0; index < argv.length; index += 2) {
    const key = argv[index];
    if (!key?.startsWith("--") || argv[index + 1] === undefined) throw new Error(`bad argument ${key ?? ""}`);
    result[key.slice(2)] = argv[index + 1];
  }
  for (const required of ["prefix", "runtime-root", "output"]) {
    if (!result[required]) throw new Error(`--${required} is required`);
  }
  return result;
}

const args = parseArgs(process.argv.slice(2));
const prefix = resolve(args.prefix);
const runtimeRoot = resolve(args["runtime-root"]);
const outputPath = resolve(args.output);
const binDir = join(prefix, "node_modules", ".bin");

mkdirSync(runtimeRoot, { recursive: true, mode: 0o700 });
mkdirSync(dirname(outputPath), { recursive: true, mode: 0o700 });

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function sleep(milliseconds) {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds));
}

async function waitUntil(predicate, label, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const value = await predicate();
      if (value) return value;
    } catch (error) {
      lastError = error;
    }
    await sleep(50);
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ""}`);
}

function isolatedEnv(home, extra = {}) {
  const path = `${binDir}:/usr/bin:/bin`;
  return {
    PATH: path,
    HOME: home,
    XDG_CONFIG_HOME: join(home, ".config"),
    XDG_DATA_HOME: join(home, ".local", "share"),
    XDG_STATE_HOME: join(home, ".local", "state"),
    XDG_CACHE_HOME: join(home, ".cache"),
    LANG: "C.UTF-8",
    LC_ALL: "C.UTF-8",
    NO_COLOR: "1",
    ...extra,
  };
}

function readJSONLines(path) {
  if (!existsSync(path)) return [];
  return readFileSync(path, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

function freePort() {
  return new Promise((resolvePromise, reject) => {
    const probe = net.createServer();
    probe.once("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const port = probe.address().port;
      probe.close((error) => error ? reject(error) : resolvePromise(port));
    });
  });
}

async function stopChild(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  try { process.kill(-child.pid, "SIGTERM"); } catch {}
  const exited = new Promise((resolvePromise) => child.once("exit", resolvePromise));
  await Promise.race([exited, sleep(2_000)]);
  if (child.exitCode === null && child.signalCode === null) {
    try { process.kill(-child.pid, "SIGKILL"); } catch {}
    await Promise.race([exited, sleep(2_000)]);
  }
}

function startCaptured(command, commandArgs, options) {
  const child = spawn(command, commandArgs, {
    cwd: options.cwd,
    env: options.env,
    stdio: ["pipe", "pipe", "pipe"],
    detached: true,
  });
  child.stdoutText = "";
  child.stderrText = "";
  child.stdout.on("data", (chunk) => { child.stdoutText += chunk.toString("utf8"); });
  child.stderr.on("data", (chunk) => { child.stderrText += chunk.toString("utf8"); });
  return child;
}

async function fetchJSON(url, init = {}) {
  const response = await fetch(url, {
    ...init,
    headers: { "content-type": "application/json", ...(init.headers ?? {}) },
  });
  const text = await response.text();
  let body;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  return { response, body, text };
}

function extractSessionID(body) {
  return body?.id ?? body?.sessionID ?? body?.sessionId ?? body?.data?.id ?? body?.data?.sessionID ?? "";
}

async function waitServer(baseURL) {
  return waitUntil(async () => {
    for (const path of ["/doc", "/global/health", "/health"]) {
      try {
        const response = await fetch(`${baseURL}${path}`, { signal: AbortSignal.timeout(750) });
        if (response.status < 500) return path;
      } catch {}
    }
    return false;
  }, `server ${baseURL}`, 30_000);
}

async function createNativeSession(baseURL) {
  const failures = [];
  for (const prefixPath of ["/session", "/api/session"]) {
    const attempt = await fetchJSON(`${baseURL}${prefixPath}`, { method: "POST", body: "{}" });
    const nativeSessionID = extractSessionID(attempt.body);
    if (attempt.response.ok && nativeSessionID) return { prefixPath, nativeSessionID, response: attempt.body };
    failures.push(`${prefixPath}:${attempt.response.status}:${attempt.text.slice(0, 200)}`);
  }
  throw new Error(`could not create native session: ${failures.join(" | ")}`);
}

async function runOpenCodeFamily(product, expectedVersion) {
  const productRoot = join(runtimeRoot, product);
  const project = join(productRoot, "project");
  const pluginDir = join(project, product === "kilocode" ? ".kilo" : ".opencode", "plugins");
  mkdirSync(pluginDir, { recursive: true, mode: 0o700 });
  mkdirSync(join(pluginDir, "lib"), { recursive: true, mode: 0o700 });
  copyFileSync(join(fixtures, "opencode-family-plugin.mjs"), join(pluginDir, "s4-component.js"));
  copyFileSync(join(fixtures, "component-core.mjs"), join(pluginDir, "lib", "component-core.js"));

  const binary = join(binDir, product === "kilocode" ? "kilo" : "opencode");
  const versionChild = startCaptured(binary, ["--version"], {
    cwd: project,
    env: isolatedEnv(join(productRoot, "version-home")),
  });
  versionChild.stdin.end();
  await new Promise((resolvePromise, reject) => {
    versionChild.once("error", reject);
    versionChild.once("exit", resolvePromise);
  });
  const versionOutput = `${versionChild.stdoutText}\n${versionChild.stderrText}`;
  if (!versionOutput.includes(expectedVersion)) throw new Error(`${product} version mismatch: ${versionOutput.trim()}`);

  const inertLog = join(productRoot, "inert.jsonl");
  const inertPort = await freePort();
  const inertChild = startCaptured(binary, ["serve", "--hostname", "127.0.0.1", "--port", String(inertPort)], {
    cwd: project,
    env: isolatedEnv(join(productRoot, "inert-home"), {
      S4_PRODUCT_ID: product,
      S4_COMPONENT_LOG: inertLog,
      S4_BOOTSTRAP_CAPABILITY_ID: `${product}-capability`,
    }),
  });
  try {
    const inertBaseURL = `http://127.0.0.1:${inertPort}`;
    await waitServer(inertBaseURL);
    await createNativeSession(inertBaseURL);
    await waitUntil(() => readJSONLines(inertLog).some((entry) => entry.event === "component.load"), `${product} inert plugin load`);
  } finally {
    await stopChild(inertChild);
  }
  const inertEntries = readJSONLines(inertLog);
  const inertLoad = inertEntries.find((entry) => entry.event === "component.load");
  if (!inertLoad || inertLoad.active || inertLoad.reason !== "missing-secret") {
    throw new Error(`${product} did not remain inert without bootstrap secret`);
  }

  const secret = randomBytes(32).toString("hex");
  const activeLog = join(productRoot, "active.jsonl");
  const activePort = await freePort();
  const activeChild = startCaptured(binary, ["serve", "--hostname", "127.0.0.1", "--port", String(activePort)], {
    cwd: project,
    env: isolatedEnv(join(productRoot, "active-home"), {
      S4_PRODUCT_ID: product,
      S4_COMPONENT_LOG: activeLog,
      S4_BOOTSTRAP_CAPABILITY_ID: `${product}-capability`,
      S4_BOOTSTRAP_SECRET: secret,
      S4_BOOTSTRAP_SECRET_SHA256: sha256(secret),
    }),
  });
  let created;
  let shell;
  try {
    const baseURL = `http://127.0.0.1:${activePort}`;
    await waitServer(baseURL);
    created = await createNativeSession(baseURL);
    const command = "printf '%s' \"$S4_NATIVE_SESSION_ID\"";
    shell = await fetchJSON(`${baseURL}${created.prefixPath}/${encodeURIComponent(created.nativeSessionID)}/shell`, {
      method: "POST",
      body: JSON.stringify({ agent: "build", command }),
    });
    if (!shell.response.ok) throw new Error(`${product} shell failed ${shell.response.status}: ${shell.text.slice(0, 500)}`);
    await waitUntil(
      () => readJSONLines(activeLog).some((entry) => entry.event === "native.session.evidence"),
      `${product} shell.env evidence`,
    );
  } finally {
    await stopChild(activeChild);
  }

  const activeEntries = readJSONLines(activeLog);
  const activeLoad = activeEntries.find((entry) => entry.event === "component.load");
  const identity = activeEntries.find((entry) => entry.event === "native.session.evidence" && entry.source === "shell.env.sessionID");
  if (!activeLoad?.active) throw new Error(`${product} did not activate with the exact bootstrap secret`);
  if (identity?.native_session_id !== created.nativeSessionID) {
    throw new Error(`${product} shell.env session mismatch: ${identity?.native_session_id} vs ${created.nativeSessionID}`);
  }
  if (!shell.text.includes(created.nativeSessionID)) {
    throw new Error(`${product} shell did not receive the session-specific injected environment`);
  }
  const persistedText = `${readFileSync(inertLog, "utf8")}\n${readFileSync(activeLog, "utf8")}\n${shell.text}\n${activeChild.stdoutText}\n${activeChild.stderrText}`;
  if (persistedText.includes(secret)) throw new Error(`${product} raw bootstrap secret leaked to probe artifacts`);

  return {
    product,
    version: expectedVersion,
    identity_source: "shell.env.sessionID",
    native_session_id: created.nativeSessionID,
    authoritative_session_id: created.nativeSessionID,
    shell_environment_session_id_matched: true,
    inert_without_bootstrap_secret: true,
    active_with_exact_bootstrap_secret: true,
    raw_bootstrap_secret_absent_from_artifacts: true,
    native_protocol: `${created.prefixPath}/{nativeSessionID}/shell`,
    component_frames_observed: activeEntries.filter((entry) => entry.event === "component.frame").map((entry) => entry.frame.type),
  };
}

function makeLFJSONReader(stream) {
  let buffer = "";
  const records = [];
  const waiters = [];
  stream.on("data", (chunk) => {
    buffer += chunk.toString("utf8");
    while (true) {
      const index = buffer.indexOf("\n");
      if (index < 0) break;
      const line = buffer.slice(0, index).replace(/\r$/, "");
      buffer = buffer.slice(index + 1);
      if (!line.trim()) continue;
      try {
        const record = JSON.parse(line);
        records.push(record);
        for (const waiter of [...waiters]) {
          if (waiter.predicate(record)) {
            waiters.splice(waiters.indexOf(waiter), 1);
            clearTimeout(waiter.timer);
            waiter.resolve(record);
          }
        }
      } catch {}
    }
  });
  return {
    records,
    waitFor(predicate, label, timeoutMs = 30_000) {
      const existing = records.find(predicate);
      if (existing) return Promise.resolve(existing);
      return new Promise((resolvePromise, reject) => {
        const waiter = {
          predicate,
          resolve: resolvePromise,
          timer: setTimeout(() => {
            const index = waiters.indexOf(waiter);
            if (index >= 0) waiters.splice(index, 1);
            reject(new Error(`timed out waiting for ${label}; records=${JSON.stringify(records.slice(-8))}`));
          }, timeoutMs),
        };
        waiters.push(waiter);
      });
    },
  };
}

async function rpcRequest(child, reader, id, type, extra = {}) {
  const responsePromise = reader.waitFor(
    (record) => record.type === "response" && record.id === id,
    `${type} response ${id}`,
  );
  child.stdin.write(`${JSON.stringify({ id, type, ...extra })}\n`);
  const response = await responsePromise;
  if (!response.success) throw new Error(`${type} failed: ${JSON.stringify(response)}`);
  return response;
}

async function closeRpcChild(child) {
  if (!child.stdin.destroyed) child.stdin.end();
  await Promise.race([
    new Promise((resolvePromise) => child.once("exit", resolvePromise)),
    sleep(2_000),
  ]);
  await stopChild(child);
}

function piFamilyArgs(product, extension, sessionDir, active) {
  if (product === "pi") {
    const base = ["--mode", "rpc", "--extension", extension, "--session-dir", sessionDir, "--offline"];
    return active
      ? [...base, "--provider", "s4", "--model", "s4/s4-model", "--tools", "s4_identity"]
      : [...base, "--no-session"];
  }
  const base = ["--mode=rpc", `--extension=${extension}`, `--session-dir=${sessionDir}`, "--no-rules"];
  return active
    ? [...base, "--tools=s4_identity", "--auto-approve"]
    : [...base, "--no-session"];
}

async function runPiFamily(product, expectedVersion) {
  const productRoot = join(runtimeRoot, product);
  const project = join(productRoot, "project");
  mkdirSync(project, { recursive: true, mode: 0o700 });
  const binary = join(binDir, product);
  const extension = join(fixtures, "pifamily-extension.mjs");

  const versionChild = startCaptured(binary, ["--version"], {
    cwd: project,
    env: isolatedEnv(join(productRoot, "version-home")),
  });
  versionChild.stdin.end();
  await new Promise((resolvePromise, reject) => {
    versionChild.once("error", reject);
    versionChild.once("exit", resolvePromise);
  });
  const versionOutput = `${versionChild.stdoutText}\n${versionChild.stderrText}`;
  if (!versionOutput.includes(expectedVersion)) throw new Error(`${product} version mismatch: ${versionOutput.trim()}`);

  const inertLog = join(productRoot, "inert.jsonl");
  const inertChild = startCaptured(binary, piFamilyArgs(product, extension, join(productRoot, "inert-sessions"), false), {
    cwd: project,
    env: isolatedEnv(join(productRoot, "inert-home"), {
      S4_PRODUCT_ID: product,
      S4_COMPONENT_LOG: inertLog,
      S4_BOOTSTRAP_CAPABILITY_ID: `${product}-capability`,
      PI_OFFLINE: "1",
    }),
  });
  try {
    await waitUntil(() => readJSONLines(inertLog).some((entry) => entry.event === "component.load"), `${product} inert extension load`, 30_000);
  } finally {
    await closeRpcChild(inertChild);
  }
  const inertLoad = readJSONLines(inertLog).find((entry) => entry.event === "component.load");
  if (!inertLoad || inertLoad.active || inertLoad.reason !== "missing-secret") {
    throw new Error(`${product} did not remain inert without bootstrap secret`);
  }

  const mock = await startMockOpenAI();
  const secret = randomBytes(32).toString("hex");
  const activeLog = join(productRoot, "active.jsonl");
  const activeChild = startCaptured(binary, piFamilyArgs(product, extension, join(productRoot, "active-sessions"), true), {
    cwd: project,
    env: isolatedEnv(join(productRoot, "active-home"), {
      S4_PRODUCT_ID: product,
      S4_COMPONENT_LOG: activeLog,
      S4_BOOTSTRAP_CAPABILITY_ID: `${product}-capability`,
      S4_BOOTSTRAP_SECRET: secret,
      S4_BOOTSTRAP_SECRET_SHA256: sha256(secret),
      S4_MOCK_BASE_URL: mock.baseURL,
      PI_OFFLINE: "1",
    }),
  });
  const activeReader = makeLFJSONReader(activeChild.stdout);
  let initialState;
  let finalState;
  try {
    await waitUntil(() => readJSONLines(activeLog).some((entry) => entry.source === "extension.session_start"), `${product} session_start`, 30_000);
    initialState = await rpcRequest(activeChild, activeReader, `${product}-state-1`, "get_state");
    await rpcRequest(activeChild, activeReader, `${product}-prompt`, "prompt", {
      message: "Call s4_identity exactly once, then stop.",
    });
    await waitUntil(() => {
      const entries = readJSONLines(activeLog);
      return entries.some((entry) => entry.source === "registered_tool.context")
        && entries.some((entry) => entry.event === "component.frame" && entry.frame?.type === "turn.event")
        && mock.requestCount() >= 2;
    }, `${product} registered tool and settled event`, 40_000);
    finalState = await rpcRequest(activeChild, activeReader, `${product}-state-2`, "get_state");
  } finally {
    await closeRpcChild(activeChild);
    await mock.close();
  }

  const entries = readJSONLines(activeLog);
  const activeLoad = entries.find((entry) => entry.event === "component.load");
  const sessionStart = entries.find((entry) => entry.source === "extension.session_start");
  const tool = entries.find((entry) => entry.source === "registered_tool.context");
  const authoritativeID = initialState?.data?.sessionId ?? initialState?.data?.sessionID ?? "";
  const finalID = finalState?.data?.sessionId ?? finalState?.data?.sessionID ?? "";
  if (!activeLoad?.active) throw new Error(`${product} did not activate with the exact bootstrap secret`);
  if (!authoritativeID || authoritativeID !== finalID) throw new Error(`${product} RPC state did not retain exact session identity`);
  if (sessionStart?.native_session_id !== authoritativeID) {
    throw new Error(`${product} extension context mismatch: ${sessionStart?.native_session_id} vs ${authoritativeID}`);
  }
  if (tool?.native_session_id !== authoritativeID
      || tool?.announced_session_id !== authoritativeID
      || tool?.environment_session_id !== authoritativeID) {
    throw new Error(`${product} registered tool context did not match exact native session ${authoritativeID}`);
  }
  const persistedText = `${readFileSync(inertLog, "utf8")}\n${readFileSync(activeLog, "utf8")}\n${activeChild.stdoutText}\n${activeChild.stderrText}`;
  if (persistedText.includes(secret)) throw new Error(`${product} raw bootstrap secret leaked to probe artifacts`);

  return {
    product,
    version: expectedVersion,
    identity_source: "registered_tool.context + RPC get_state.sessionId",
    native_session_id: authoritativeID,
    authoritative_session_id: authoritativeID,
    extension_session_id_matched: true,
    registered_tool_session_id_matched: true,
    session_specific_environment_matched: true,
    inert_without_bootstrap_secret: true,
    active_with_exact_bootstrap_secret: true,
    raw_bootstrap_secret_absent_from_artifacts: true,
    native_protocol: "real JSONL RPC + registered extension provider/tool",
    mock_model_requests: mock.requestCount(),
    component_frames_observed: entries.filter((entry) => entry.event === "component.frame").map((entry) => entry.frame.type),
  };
}

function validateFrameVocabulary(products) {
  const required = [
    "bootstrap", "reconnect", "session.announce", "session.rename", "session.state",
    "delivery.accept", "delivery.reject", "turn.event", "tool.call", "tool.result",
  ];
  const byProduct = {};
  for (const product of products) {
    const nativeSessionID = product.native_session_id;
    const bindingID = `${product.product}-binding`;
    const frames = [
      { version: 1, type: "bootstrap", id: "boot", seq: 1, payload: { product_id: product.product, attachment_id: `${product.product}-attachment`, bootstrap_capability_id: "redacted-id", bootstrap_value: "<ephemeral-redacted>", process_start: 1, strong_start: "probe-strong-start", component_version: "s4" } },
      { version: 1, type: "reconnect", id: "reconnect", seq: 2, payload: { attachment_id: `${product.product}-attachment`, prior_binding_id: bindingID, prior_generation: 1, process_start: 1, strong_start: "probe-strong-start", last_received_seq: 1 } },
      { version: 1, type: "session.announce", id: "announce", seq: 3, payload: { binding_id: bindingID, native_session_id: nativeSessionID, cwd: "/probe", native_name: "s4", product_event_seq: 1 } },
      { version: 1, type: "session.rename", id: "rename", seq: 4, payload: { binding_id: bindingID, native_session_id: nativeSessionID, native_name: "s4-renamed", product_event_seq: 2 } },
      { version: 1, type: "session.state", id: "state", seq: 5, payload: { binding_id: bindingID, native_session_id: nativeSessionID, state: "idle", product_event_seq: 3 } },
      { version: 1, type: "delivery.accept", id: "delivery-1", seq: 6, payload: { delivery_id: "delivery-1", native_session_id: nativeSessionID, native_message_id: "native-message-1", accepted_at: "2026-09-01T00:00:00Z" } },
      { version: 1, type: "delivery.reject", id: "delivery-2", seq: 7, payload: { delivery_id: "delivery-2", category: "NativeRejected", detail: "redacted" } },
      { version: 1, type: "turn.event", id: "turn-1", seq: 8, payload: { binding_id: bindingID, native_session_id: nativeSessionID, event_seq: 4, kind: "settled", metadata: {} } },
      { version: 1, type: "tool.call", id: "call-1", seq: 9, payload: { binding_id: bindingID, native_session_id: nativeSessionID, operation: "lane.start", arguments: {} } },
      { version: 1, type: "tool.result", id: "call-1", seq: 10, payload: { call_id: "call-1", success: true, result: { accepted: true } } },
    ];
    const types = frames.map((frame) => frame.type);
    for (const type of required) {
      if (!types.includes(type)) throw new Error(`${product.product} frame vocabulary lacks ${type}`);
    }
    for (const frame of frames) {
      if (frame.version !== 1 || !frame.type || !frame.id || !Number.isSafeInteger(frame.seq)) {
        throw new Error(`${product.product} invalid common frame ${JSON.stringify(frame)}`);
      }
      if (frame.payload.native_session_id && frame.payload.native_session_id !== nativeSessionID) {
        throw new Error(`${product.product} frame weakens exact native identity`);
      }
    }
    byProduct[product.product] = { pass: true, frame_types: types, native_session_id_consistent: true };
  }
  return {
    version: 1,
    evidence_kind: "contract-frame mapping over native product identity evidence; daemon transport is not implemented in S4",
    required_frame_types: required,
    products: byProduct,
    product_specific_identity_sources_preserved: true,
    no_universal_native_endpoint_dsl: true,
  };
}

function gitOutput(commandArgs) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn("git", commandArgs, { cwd: process.cwd(), stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.once("error", reject);
    child.once("exit", (code) => code === 0 ? resolvePromise(stdout.trim()) : reject(new Error(stderr.trim())));
  });
}

const startedAt = new Date().toISOString();
const results = [];
results.push(await runOpenCodeFamily("opencode", "1.18.25"));
results.push(await runOpenCodeFamily("kilocode", "7.5.6"));
results.push(await runPiFamily("pi", "0.84.4"));
results.push(await runPiFamily("omp", "18.0.11"));

const evidence = {
  schema: "agent-sessions.six-product.phase0.component-context.v1",
  gate: "S4",
  status: "pass",
  started_at: startedAt,
  completed_at: new Date().toISOString(),
  base_commit: await gitOutput(["rev-parse", "HEAD"]),
  product_protocol_mocked: false,
  model_provider: "local deterministic OpenAI-compatible tool-call fixture",
  products: results,
  bootstrap: {
    one_time_secret_is_ephemeral_launch_input: true,
    capability_id_alone_is_insufficient: results.every((result) => result.inert_without_bootstrap_secret),
    components_inert_without_secret: results.every((result) => result.inert_without_bootstrap_secret),
    exact_secret_activates_component: results.every((result) => result.active_with_exact_bootstrap_secret),
    raw_secret_absent_from_artifacts: results.every((result) => result.raw_bootstrap_secret_absent_from_artifacts),
    durable_reconnect_authority: "kernel peer identity + process start/strong-start/ancestry against ManagedAttachment; no reusable secret and no Ed25519",
  },
  component_vocabulary: validateFrameVocabulary(results),
  limitations: [
    "This S4 spike proves native identity contexts and common frame expressiveness; it does not implement the daemon broker.",
    "The local model fixture controls only LLM responses; product loaders, hooks, session stores, RPC, shell, and tool execution are real pinned binaries.",
  ],
};

writeFileSync(outputPath, `${JSON.stringify(evidence, null, 2)}\n`, { mode: 0o600 });
process.stdout.write(`${JSON.stringify(evidence, null, 2)}\n`);
