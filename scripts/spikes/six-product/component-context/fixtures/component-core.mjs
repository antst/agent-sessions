import { createHash, timingSafeEqual } from "node:crypto";
import { appendFileSync } from "node:fs";

function safeEqualHex(left, right) {
  if (!left || !right || left.length !== right.length) return false;
  try {
    return timingSafeEqual(Buffer.from(left, "hex"), Buffer.from(right, "hex"));
  } catch {
    return false;
  }
}

export function bootstrapGate(product) {
  const capabilityID = process.env.S4_BOOTSTRAP_CAPABILITY_ID ?? "";
  const secret = process.env.S4_BOOTSTRAP_SECRET ?? "";
  const expectedHash = process.env.S4_BOOTSTRAP_SECRET_SHA256 ?? "";
  const actualHash = secret ? createHash("sha256").update(secret).digest("hex") : "";
  const active = Boolean(capabilityID && secret && expectedHash && safeEqualHex(actualHash, expectedHash));
  const reason = active
    ? "authenticated"
    : !capabilityID
      ? "missing-capability-id"
      : !secret
        ? "missing-secret"
        : !expectedHash
          ? "missing-expected-secret-hash"
          : "secret-mismatch";

  record({
    event: "component.load",
    product,
    active,
    reason,
    capability_id_present: Boolean(capabilityID),
    secret_present: Boolean(secret),
    secret_persisted: false,
  });
  return { active, capabilityID, reason };
}

export function record(value) {
  const path = process.env.S4_COMPONENT_LOG;
  if (!path) throw new Error("S4_COMPONENT_LOG is required");
  appendFileSync(path, `${JSON.stringify({ at: new Date().toISOString(), ...value })}\n`, { mode: 0o600 });
}

export function componentFrame(type, id, seq, payload) {
  return { version: 1, type, id, seq, payload };
}

export function recordSessionEvidence(product, source, nativeSessionID, extra = {}) {
  if (!nativeSessionID) throw new Error(`${product} ${source} did not supply a native session ID`);
  record({
    event: "native.session.evidence",
    product,
    source,
    native_session_id: nativeSessionID,
    ...extra,
  });
}
