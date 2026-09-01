import {
  bootstrapGate,
  componentFrame,
  record,
  recordSessionEvidence,
} from "./lib/component-core.js";

export default async function s4OpenCodeFamilyPlugin() {
  const product = process.env.S4_PRODUCT_ID ?? "unknown";
  const gate = bootstrapGate(product);
  if (!gate.active) return {};

  let seq = 0;
  return {
    "shell.env": async (input, output) => {
      const nativeSessionID = input.sessionID ?? "";
      recordSessionEvidence(product, "shell.env.sessionID", nativeSessionID, {
        call_id: input.callID ?? "",
        cwd: input.cwd,
      });
      output.env.S4_NATIVE_SESSION_ID = nativeSessionID;
      record({
        event: "component.frame",
        product,
        frame: componentFrame("session.announce", `${product}-announce`, ++seq, {
          binding_id: `${product}-probe-binding`,
          native_session_id: nativeSessionID,
          cwd: input.cwd,
          native_name: `${product}-s4`,
          product_event_seq: seq,
        }),
      });
    },
    "tool.execute.before": async (input) => {
      recordSessionEvidence(product, "tool.execute.before.sessionID", input.sessionID, {
        call_id: input.callID,
        tool: input.tool,
      });
    },
  };
}
