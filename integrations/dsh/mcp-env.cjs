"use strict";

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const SECRET_NAME = /(?:KEY|PASSWORD|SECRET|TOKEN)/iu;

function isWithin(candidate, root) {
  const relative = path.relative(path.resolve(root), path.resolve(candidate));
  return relative === "" || (relative !== ".." && !relative.startsWith(".." + path.sep) && !path.isAbsolute(relative));
}

function canonicalExisting(candidate) {
  let prefix = path.resolve(candidate);
  const suffix = [];
  for (;;) {
    try {
      let resolved = fs.realpathSync.native(prefix);
      for (let index = suffix.length - 1; index >= 0; index -= 1) resolved = path.join(resolved, suffix[index]);
      return path.resolve(resolved);
    } catch (error) {
      if (error?.code !== "ENOENT") throw error;
    }
    const parent = path.dirname(prefix);
    if (parent === prefix) throw new Error("DSH path has no existing canonical prefix");
    suffix.push(path.basename(prefix));
    prefix = parent;
  }
}

function isTemporary(candidate) {
  const canonical = canonicalExisting(candidate);
  for (const root of ["/tmp", "/private/tmp", os.tmpdir()]) {
    if (isWithin(candidate, root) || isWithin(canonical, canonicalExisting(root))) return true;
  }
  return false;
}

function isCanonicallyWithin(candidate, root) {
  return isWithin(candidate, root) && !isTemporary(root) && isWithin(canonicalExisting(candidate), canonicalExisting(root));
}

function validateSocket(socketPath, environment = process.env) {
  if (!path.isAbsolute(socketPath) || path.basename(socketPath) !== "presence.sock") {
    throw new Error("DSH presence socket must be an absolute presence.sock path");
  }
  if (path.resolve(socketPath) !== socketPath || isTemporary(socketPath)) throw new Error("DSH sandbox masks temporary sockets");
  const roots = [
    environment.HOME,
    environment.XDG_STATE_HOME,
    environment.XDG_RUNTIME_DIR,
    environment.XDG_CONFIG_HOME,
  ].filter(Boolean);
  if (!roots.some((root) => isCanonicallyWithin(socketPath, root))) {
    throw new Error("DSH presence socket must be below HOME or an XDG root");
  }
  return socketPath;
}

function validateStateRoot(stateRoot, environment = process.env) {
  if (!path.isAbsolute(stateRoot) || path.resolve(stateRoot) !== stateRoot || isTemporary(stateRoot)) {
    throw new Error("DSH MCP state root must be an absolute clean HOME/XDG path outside /tmp");
  }
  const roots = [environment.HOME, environment.XDG_STATE_HOME, environment.XDG_RUNTIME_DIR, environment.XDG_CONFIG_HOME].filter(Boolean);
  if (!roots.some((root) => isCanonicallyWithin(stateRoot, root))) {
    throw new Error("DSH MCP state root must be below HOME or an XDG root");
  }
  return stateRoot;
}

// DSH scrubs DSH_* and credential-looking names from inherited MCP env. These
// exact non-secret values are therefore supplied in the MCP server's explicit
// env block. DSH_SESSION_ID remains native-owned and is never forged here.
function explicitMCPEnvironment(values, environment = process.env) {
  if (!values || typeof values.sessionID !== "string" || values.sessionID === "") {
    throw new Error("explicit DSH MCP env requires a session witness");
  }
  const presenceSocket = validateSocket(values.presenceSocket, environment);
  const result = {
    AGENT_SESSIONS_SESSION_ID: values.sessionID,
    AGENT_SESSIONS_PRESENCE_SOCKET: presenceSocket,
  };
  if (values.stateRoot) result.AGENT_SESSIONS_STATE_ROOT = validateStateRoot(values.stateRoot, environment);
  for (const name of Object.keys(result)) {
    if (name.startsWith("DSH_") || SECRET_NAME.test(name)) {
      throw new Error("explicit DSH MCP env contains a scrubbed or secret-like name");
    }
  }
  return result;
}

module.exports = { canonicalExisting, explicitMCPEnvironment, isTemporary, isWithin, validateSocket, validateStateRoot };
