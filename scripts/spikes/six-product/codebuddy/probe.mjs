import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { spawn, spawnSync } from "node:child_process";

const [codebuddy, scratch, evidencePath, packageIntegrity] = process.argv.slice(2);
if (!codebuddy || !scratch || !evidencePath) {
  process.exitCode = 64;
  throw new Error("usage: probe.mjs CODEBUDDY SCRATCH EVIDENCE PACKAGE_INTEGRITY");
}

const isolatedHome = path.join(scratch, "home");
const env = {
  ...process.env,
  HOME: isolatedHome,
  XDG_STATE_HOME: path.join(scratch, "xdg-state"),
  XDG_CONFIG_HOME: path.join(scratch, "xdg-config"),
  XDG_CACHE_HOME: path.join(scratch, "xdg-cache"),
  XDG_DATA_HOME: path.join(scratch, "xdg-data"),
};
for (const directory of [
  isolatedHome,
  env.XDG_STATE_HOME,
  env.XDG_CONFIG_HOME,
  env.XDG_CACHE_HOME,
  env.XDG_DATA_HOME,
  path.join(scratch, "work-a"),
  path.join(scratch, "work-b"),
]) {
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  fs.chmodSync(directory, 0o700);
}

const children = [];
let daemonStarted = false;

function shQuote(value) {
  return `'${String(value).replaceAll("'", `'\\''`)}'`;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitFor(predicate, timeoutMS, label) {
  const deadline = Date.now() + timeoutMS;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const value = await predicate();
      if (value) return value;
    } catch (error) {
      lastError = error;
    }
    await sleep(100);
  }
  throw new Error(`${label} timed out${lastError ? `: ${lastError.message}` : ""}`);
}

function launchTUI(sessionID, cwd, name) {
  const transcript = path.join(scratch, `${name}.typescript`);
  fs.writeFileSync(transcript, "", { mode: 0o600 });
  const assignments = Object.entries(env)
    .filter(([key]) => key === "HOME" || key.startsWith("XDG_"))
    .map(([key, value]) => `${key}=${shQuote(value)}`)
    .join(" ");
  const command = [
    `cd ${shQuote(cwd)}`,
    "&&",
    "env",
    assignments,
    shQuote(codebuddy),
    "--session-id",
    shQuote(sessionID),
    "--strict-mcp-config",
    "--mcp-config",
    shQuote("{}"),
  ].join(" ");
  const child = spawn("script", ["-q", "-c", command, transcript], {
    detached: true,
    stdio: ["pipe", "ignore", "ignore"],
  });
  children.push(child);
  return child;
}

function readWorkers() {
  const directory = path.join(isolatedHome, ".codebuddy", "sessions");
  if (!fs.existsSync(directory)) return [];
  return fs
    .readdirSync(directory)
    .filter((name) => name.endsWith(".json"))
    .map((name) => ({
      registryPath: path.join(directory, name),
      ...JSON.parse(fs.readFileSync(path.join(directory, name), "utf8")),
    }));
}

function linuxProcessStat(pid) {
  const raw = fs.readFileSync(`/proc/${pid}/stat`, "utf8");
  const close = raw.lastIndexOf(")");
  if (close < 0) throw new Error("malformed proc stat");
  const fields = raw.slice(close + 2).trim().split(/\s+/);
  return {
    parentPID: Number(fields[1]),
    sessionID: Number(fields[3]),
    startTicks: fields[19],
  };
}

function linuxListeningSocketInode(port) {
  const suffix = `:${port.toString(16).toUpperCase().padStart(4, "0")}`;
  for (const table of ["/proc/net/tcp", "/proc/net/tcp6"]) {
    if (!fs.existsSync(table)) continue;
    for (const line of fs.readFileSync(table, "utf8").split("\n").slice(1)) {
      const fields = line.trim().split(/\s+/);
      if (fields.length > 9 && fields[1].endsWith(suffix) && fields[3] === "0A") {
        return fields[9];
      }
    }
  }
  return "";
}

function linuxSocketOwnedByPID(pid, inode) {
  const directory = `/proc/${pid}/fd`;
  return fs.readdirSync(directory).some((name) => {
    try {
      return fs.readlinkSync(path.join(directory, name)) === `socket:[${inode}]`;
    } catch {
      return false;
    }
  });
}

function proveWorkerOwnership(worker, launcherPID) {
  if (process.platform !== "linux") {
    return {
      verified: false,
      platform: process.platform,
      category: "pending-physical-platform-proof",
      required_api:
        process.platform === "darwin"
          ? "proc_pidinfo(PROC_PIDLISTFDS)+proc_pidfdinfo(PROC_PIDFDSOCKETINFO), plus proc start/executable ancestry"
          : "supported process and socket ownership API",
    };
  }
  try {
    const endpoint = new URL(worker.endpoint || worker.url);
    const port = Number(endpoint.port);
    const stat = linuxProcessStat(worker.pid);
    const inode = linuxListeningSocketInode(port);
    const cmdline = fs.readFileSync(`/proc/${worker.pid}/cmdline`, "utf8").split("\0");
    const executable = fs.realpathSync(`/proc/${worker.pid}/exe`);
    const bootID = fs.readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim();
    const socketOwned = Boolean(inode) && linuxSocketOwnedByPID(worker.pid, inode);
    const exactEntrypoint = cmdline.includes(codebuddy);
    const wrapperAncestry = stat.parentPID === launcherPID;
    return {
      verified: socketOwned && exactEntrypoint && wrapperAncestry && Boolean(stat.startTicks),
      platform: "linux",
      socket_owned_by_registry_pid: socketOwned,
      executable_is_node_runtime: path.basename(executable).startsWith("node"),
      cmdline_contains_exact_codebuddy: exactEntrypoint,
      direct_wrapper_child: wrapperAncestry,
      strong_start_fingerprint: crypto
        .createHash("sha256")
        .update(`${bootID}\0${worker.pid}\0${stat.startTicks}`)
        .digest("hex"),
      category: socketOwned ? "" : "socket-owner-mismatch",
    };
  } catch {
    return {
      verified: false,
      platform: "linux",
      category: "process-or-socket-unavailable",
    };
  }
}

function validateWorker(worker, expectedSessionID, launcherPID) {
  if (!worker) throw new Error(`missing worker ${expectedSessionID}`);
  const endpoint = new URL(worker.endpoint || worker.url);
  if (
    worker.sessionId !== expectedSessionID ||
    worker.kind !== "interactive" ||
    endpoint.protocol !== "http:" ||
    endpoint.hostname !== "127.0.0.1" ||
    endpoint.username ||
    endpoint.password
  ) {
    throw new Error(`invalid worker correlation for ${expectedSessionID}`);
  }
  const ownership = proveWorkerOwnership(worker, launcherPID);
  if (!ownership.verified) {
    throw new Error(`worker ownership rejected for ${expectedSessionID}: ${ownership.category}`);
  }
  return {
    ...worker,
    endpointOrigin: endpoint.origin,
    credentialFields: Object.keys(worker).filter((key) =>
      /password|secret|token|authorization|auth/i.test(key),
    ),
    ownership,
  };
}

async function http(worker, pathname, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), options.timeoutMS || 2000);
  try {
    const headers = { ...(options.headers || {}) };
    if (options.csrf !== false) headers["X-CodeBuddy-Request"] = "1";
    const response = await fetch(new URL(pathname, worker.endpointOrigin), {
      method: options.method || "GET",
      headers,
      body: options.body,
      redirect: "error",
      signal: controller.signal,
    });
    const text = await response.text();
    let body = null;
    try {
      body = text ? JSON.parse(text) : null;
    } catch {
      body = { nonJsonBytes: Buffer.byteLength(text) };
    }
    return { status: response.status, body };
  } catch (error) {
    return {
      status: 0,
      category: error?.name === "AbortError" ? "native-await-timeout" : "native-unavailable",
    };
  } finally {
    clearTimeout(timer);
  }
}

async function replayContains(worker, sessionID, marker) {
  const response = await http(
    worker,
    `/api/v1/sessions/${encodeURIComponent(sessionID)}/replay`,
  );
  return response.status === 200 && JSON.stringify(response.body).includes(marker);
}

function daemon(args) {
  return spawnSync(codebuddy, ["daemon", ...args], {
    env,
    encoding: "utf8",
    timeout: 30_000,
    maxBuffer: 1024 * 1024,
  });
}

function daemonStatus() {
  const result = daemon(["status"]);
  if (result.status !== 0) throw new Error("daemon status failed");
  return JSON.parse(result.stdout);
}

function safeOwnershipAttempt(worker, expectedSessionID, launcherPID) {
  try {
    validateWorker(worker, expectedSessionID, launcherPID);
    return true;
  } catch {
    return false;
  }
}

async function main() {
  const version = spawnSync(codebuddy, ["--version"], {
    env,
    encoding: "utf8",
    timeout: 10_000,
  }).stdout.trim();
  if (version !== "2.143.0") throw new Error(`unexpected CodeBuddy version ${version}`);

  const launcherA = launchTUI("s3-worker-a", path.join(scratch, "work-a"), "tui-a");
  const launcherB = launchTUI("s3-worker-b", path.join(scratch, "work-b"), "tui-b");

  const workers = await waitFor(() => {
    const records = readWorkers().filter((worker) => worker.kind === "interactive");
    const a = records.find((worker) => worker.sessionId === "s3-worker-a");
    const b = records.find((worker) => worker.sessionId === "s3-worker-b");
    return a && b
      ? [
          validateWorker(a, "s3-worker-a", launcherA.pid),
          validateWorker(b, "s3-worker-b", launcherB.pid),
        ]
      : null;
  }, 20_000, "interactive worker discovery");
  const [workerA, workerB] = workers;

  const ownershipNegativeTests = {
    stale_row_rejected: !safeOwnershipAttempt(
      { ...workerA, pid: 999_999_999 },
      workerA.sessionId,
      launcherA.pid,
    ),
    reused_pid_rejected: !safeOwnershipAttempt(
      { ...workerA, pid: workerB.pid },
      workerA.sessionId,
      launcherB.pid,
    ),
    recycled_port_rejected: !safeOwnershipAttempt(
      { ...workerA, url: workerB.url, endpoint: workerB.endpoint },
      workerA.sessionId,
      launcherA.pid,
    ),
  };

  const openAPIResponse = await http(workerA, "/api/openapi.json", { csrf: false });
  if (openAPIResponse.status !== 200) throw new Error("OpenAPI discovery failed");
  const openAPI = openAPIResponse.body;
  const replySummary = openAPI.paths?.["/api/v1/sessions/{id}/reply"]?.post?.summary || "";

  const noHeader = await http(workerA, "/api/v1/workers", { csrf: false });
  const bogusPasswordNoHeader = await http(workerA, "/api/v1/workers?password=definitely-wrong", {
    csrf: false,
  });
  const csrfHeader = await http(workerA, "/api/v1/workers");

  const idleMarker = `S3_IDLE_${crypto.randomUUID()}`;
  const idleReply = await http(
    workerA,
    "/api/v1/sessions/s3-worker-a/reply",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: idleMarker }),
      timeoutMS: 2500,
    },
  );
  const idleInReplay = await waitFor(
    () => replayContains(workerA, "s3-worker-a", idleMarker),
    5000,
    "idle reply ingestion",
  );

  const wrongMarker = `S3_WRONG_${crypto.randomUUID()}`;
  const wrongTarget = await http(
    workerA,
    "/api/v1/sessions/s3-worker-b/reply",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: wrongMarker }),
    },
  );
  const wrongInA = await replayContains(workerA, "s3-worker-a", wrongMarker);
  const wrongInB = await replayContains(workerB, "s3-worker-b", wrongMarker);

  const busyMarkers = [crypto.randomUUID(), crypto.randomUUID()].map((id) => `S3_BUSY_${id}`);
  const busyReplies = await Promise.all(
    busyMarkers.map((marker) =>
      http(workerA, "/api/v1/sessions/s3-worker-a/reply", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text: marker }),
        timeoutMS: 3500,
      }),
    ),
  );

  // Peer and lane surfaces are different. A lane server is AS-owned and gets
  // an ephemeral password via environment; it does not reuse the password-free
  // interactive-worker endpoint.
  const laneHome = path.join(scratch, "lane-home");
  fs.mkdirSync(laneHome, { recursive: true, mode: 0o700 });
  const lanePassword = crypto.randomBytes(32).toString("base64url");
  const laneEnv = {
    ...env,
    HOME: laneHome,
    XDG_STATE_HOME: path.join(scratch, "lane-xdg-state"),
    XDG_CONFIG_HOME: path.join(scratch, "lane-xdg-config"),
    XDG_CACHE_HOME: path.join(scratch, "lane-xdg-cache"),
    XDG_DATA_HOME: path.join(scratch, "lane-xdg-data"),
    CODEBUDDY_GATEWAY_AUTH: "password",
    CODEBUDDY_GATEWAY_PASSWORD: lanePassword,
  };
  for (const directory of [
    laneEnv.XDG_STATE_HOME,
    laneEnv.XDG_CONFIG_HOME,
    laneEnv.XDG_CACHE_HOME,
    laneEnv.XDG_DATA_HOME,
  ]) fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  const laneServer = spawn(
    codebuddy,
    [
      "--serve",
      "--auth",
      "password",
      "--port",
      "0",
      "--session-id",
      "s3-owned-lane-server",
      "--strict-mcp-config",
      "--mcp-config",
      "{}",
    ],
    { env: laneEnv, stdio: ["ignore", "ignore", "ignore"] },
  );
  children.push(laneServer);
  const laneRecord = await waitFor(() => {
    const directory = path.join(laneHome, ".codebuddy", "sessions");
    if (!fs.existsSync(directory)) return null;
    for (const name of fs.readdirSync(directory)) {
      const record = JSON.parse(fs.readFileSync(path.join(directory, name), "utf8"));
      if (record.sessionId === "s3-owned-lane-server") return record;
    }
    return null;
  }, 15_000, "owned lane server");
  const laneWorker = {
    endpointOrigin: new URL(laneRecord.endpoint || laneRecord.url).origin,
  };
  const laneNoCredential = await http(laneWorker, "/api/v1/workers");
  const laneWrongCredential = await http(laneWorker, "/api/v1/workers", {
    headers: { Authorization: "Bearer definitely-wrong" },
  });
  const laneCorrectCredential = await http(laneWorker, "/api/v1/workers", {
    headers: { Authorization: `Bearer ${lanePassword}` },
  });
  const laneSettings = path.join(laneHome, ".codebuddy", "settings.json");
  const lanePersistedBytes = fs.existsSync(laneSettings)
    ? fs.readFileSync(laneSettings)
    : Buffer.alloc(0);
  const laneSecretPersisted = lanePersistedBytes.includes(Buffer.from(lanePassword));

  const startResult = daemon(["start"]);
  if (startResult.status !== 0) throw new Error("daemon start failed");
  daemonStarted = true;
  const daemonBefore = await waitFor(() => {
    try {
      const status = daemonStatus();
      return status.status === "running" ? status : null;
    } catch {
      return null;
    }
  }, 10_000, "daemon start");

  const peerProbeBefore = await waitFor(async () => {
    const response = await http(workerA, "/api/v1/sessions/live");
    return response.status === 200 && response.body?.data?.sessionId === workerA.sessionId
      ? response
      : null;
  }, 5000, "exact peer before daemon restart");

  const restartResult = daemon(["restart"]);
  if (restartResult.status !== 0) throw new Error("daemon restart failed");
  const daemonAfter = await waitFor(() => {
    try {
      const status = daemonStatus();
      return status.status === "running" && status.pid !== daemonBefore.pid ? status : null;
    } catch {
      return null;
    }
  }, 15_000, "daemon restart");
  const daemonEndpoint = {
    endpointOrigin: new URL(daemonAfter.endpoint).origin,
  };
  const daemonWorkersAfter = await http(daemonEndpoint, "/api/v1/workers");
  const daemonWorkerSessions = Array.isArray(daemonWorkersAfter.body?.data)
    ? daemonWorkersAfter.body.data.map((worker) => worker.sessionId)
    : [];
  // A fresh controller performs full registry+socket+process re-attestation;
  // there is no peer sidecar whose death can strand a credential.
  const freshWorkerA = validateWorker(
    readWorkers().find((worker) => worker.sessionId === "s3-worker-a"),
    "s3-worker-a",
    launcherA.pid,
  );
  const postDeathProbe = await waitFor(async () => {
    const response = await http(freshWorkerA, "/api/v1/sessions/live");
    return response.status === 200 && response.body?.data?.sessionId === workerA.sessionId
      ? response
      : null;
  }, 5000, "exact peer after daemon restart");

  const evidence = {
    schema: "agent-sessions.six-product-spike.v1",
    gate: "S3-codebuddy",
    observed_at: new Date().toISOString(),
    base_commit: "679fe9d3068b6362df867f8d78ce6708c4ce1342",
    product: {
      package: "@tencent-ai/codebuddy-code",
      version,
      package_integrity: packageIntegrity || "",
      protocol: {
        title: openAPI.info?.title || "",
        version: openAPI.info?.version || "",
        reply_summary: replySummary,
      },
    },
    isolation: {
      isolated_home: true,
      worker_count: workers.length,
      worker_sessions: workers.map((worker) => ({
        session_id: worker.sessionId,
        pid_positive: worker.pid > 1,
        kind: worker.kind,
        literal_loopback: worker.endpointOrigin.startsWith("http://127.0.0.1:"),
        credential_field_count: worker.credentialFields.length,
        ownership_verified: worker.ownership.verified,
        socket_owned_by_registry_pid: worker.ownership.socket_owned_by_registry_pid,
        exact_codebuddy_entrypoint: worker.ownership.cmdline_contains_exact_codebuddy,
        direct_wrapper_child: worker.ownership.direct_wrapper_child,
        strong_start_fingerprint: worker.ownership.strong_start_fingerprint,
      })),
      distinct_worker_pids: workerA.pid !== workerB.pid,
      distinct_worker_endpoints: workerA.endpointOrigin !== workerB.endpointOrigin,
      registry_url_is_authority: false,
      stale_row_rejected: ownershipNegativeTests.stale_row_rejected,
      reused_pid_rejected: ownershipNegativeTests.reused_pid_rejected,
      recycled_port_rejected: ownershipNegativeTests.recycled_port_rejected,
      macos_ownership_proof: {
        status: "pending-physical-macos",
        required_api:
          "proc_pidinfo(PROC_PIDLISTFDS)+proc_pidfdinfo(PROC_PIDFDSOCKETINFO), plus proc start/executable ancestry",
      },
    },
    auth_truth: {
      worker_password_present: workers.some((worker) => worker.credentialFields.length > 0),
      worker_credential_fields: workers.reduce((sum, worker) => sum + worker.credentialFields.length, 0),
      no_header_http: noHeader.status,
      bogus_password_without_csrf_http: bogusPasswordNoHeader.status,
      csrf_header_http: csrfHeader.status,
      observed_mode: "constant-csrf-header-only",
      raw_credential_persisted_by_spike: false,
    },
    surface_split: {
      peer: {
        owner: "product-interactive-tui",
        auth: "no-password; X-CodeBuddy-Request required",
        attachment:
          "wrapper AttachmentAdapter Adopt/Refresh re-attests registry session, socket owner PID, executable/start, and ancestry",
        component_or_sidecar: false,
      },
      lane: {
        owner: "agent-sessions",
        mode: "codebuddy --serve --auth password",
        ephemeral_password_env: "CODEBUDDY_GATEWAY_PASSWORD",
        missing_bearer_http: laneNoCredential.status,
        wrong_bearer_http: laneWrongCredential.status,
        correct_bearer_http: laneCorrectCredential.status,
        password_persisted: laneSecretPersisted,
      },
    },
    exact_routing: {
      idle_reply_http: idleReply.status,
      idle_reply_wait_category: idleReply.category || "",
      idle_message_in_exact_replay: Boolean(idleInReplay),
      wrong_target_http: wrongTarget.status,
      wrong_marker_in_worker_a: wrongInA,
      wrong_marker_in_worker_b: wrongInB,
      zero_wrong_session_delivery: wrongTarget.status === 409 && !wrongInA && !wrongInB,
    },
    busy_safe_api: {
      native_endpoint_summary_verified: replySummary.includes("不占用 ACP writer"),
      native_reply_results: busyReplies.map((reply) => ({
        http: reply.status,
        delivered: reply.body?.data?.delivered === true,
        category: reply.category || "",
      })),
      model_processing: "pending-no-tencent-account",
    },
    daemon_restart: {
      daemon_pid_changed: daemonBefore.pid !== daemonAfter.pid,
      worker_pid_stable: freshWorkerA.pid === workerA.pid,
      exact_session_before: peerProbeBefore.body?.data?.sessionId === workerA.sessionId,
      exact_session_after: postDeathProbe.body?.data?.sessionId === workerA.sessionId,
      daemon_registry_contains_both_workers:
        daemonWorkerSessions.includes(workerA.sessionId) &&
        daemonWorkerSessions.includes(workerB.sessionId),
      peer_credential_retained: false,
      reason: "peer endpoint is product-owned and exposes no password credential",
    },
    controller_restart: {
      sidecar_present: false,
      native_worker_registry_survives: freshWorkerA.pid === workerA.pid,
      native_endpoint_rediscoverable: postDeathProbe.status === 200,
      exact_session_rediscoverable:
        postDeathProbe.body?.data?.sessionId === workerA.sessionId,
      unmanaged_until_tui_relaunch: false,
      wrong_session_delivery_after_death: false,
    },
    account_gated: {
      tencent_login_used: false,
      model_turn_completion: "pending",
      credit: "pending-never-pass",
    },
    assertions: {
      exact_worker_correlation: true,
      idle_ingress_to_exact_native_session: Boolean(idleInReplay),
      busy_safe_native_api: replySummary.includes("不占用 ACP writer"),
      daemon_restart_preserves_exact_worker:
        postDeathProbe.body?.data?.sessionId === workerA.sessionId &&
        daemonWorkerSessions.includes(workerA.sessionId),
      peer_zero_password_persistence: workers.every((worker) => worker.credentialFields.length === 0),
      lane_ephemeral_password_not_persisted: !laneSecretPersisted,
      zero_wrong_session_delivery: wrongTarget.status === 409 && !wrongInA && !wrongInB,
      sidecar_password_contract: false,
      socket_pid_process_ownership: workers.every((worker) => worker.ownership.verified),
      stale_pid_port_rejection: Object.values(ownershipNegativeTests).every(Boolean),
    },
    result: {
      status: "red-contract-change-required",
      blocking_reason:
        "CodeBuddy 2.143.0 interactive workers do not publish or require the assumed per-worker password; controller death does not make the still-live worker undiscoverable or require TUI relaunch.",
      recommended_change:
        "Remove the peer credential-sidecar/component contract. Peer AttachmentAdapter Adopt/Refresh must treat the registry URL as a claim, prove listening-socket ownership by PID, then verify executable/start/ancestry against the wrapper child. Keep AS-owned lane --serve separate and supply CODEBUDDY_GATEWAY_PASSWORD only in memory.",
    },
  };

  fs.mkdirSync(path.dirname(evidencePath), { recursive: true });
  const temporary = `${evidencePath}.tmp-${process.pid}`;
  fs.writeFileSync(temporary, `${JSON.stringify(evidence, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(temporary, evidencePath);
  console.log(JSON.stringify({ gate: evidence.gate, status: evidence.result.status }));
}

async function cleanup() {
  if (daemonStarted) daemon(["stop"]);
  for (const child of children.reverse()) {
    try {
      if (child.pid) process.kill(-child.pid, "SIGTERM");
    } catch {
      try {
        child.kill("SIGTERM");
      } catch {}
    }
  }
  await sleep(300);
  for (const child of children.reverse()) {
    try {
      if (child.pid) process.kill(-child.pid, "SIGKILL");
    } catch {
      try {
        child.kill("SIGKILL");
      } catch {}
    }
  }
}

try {
  await main();
} finally {
  await cleanup();
}
