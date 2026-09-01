import {
  bootstrapGate,
  componentFrame,
  record,
  recordSessionEvidence,
} from "./component-core.mjs";

export default function s4PiFamilyExtension(pi) {
  const product = process.env.S4_PRODUCT_ID ?? "unknown";
  const gate = bootstrapGate(product);
  if (!gate.active) return;

  let announcedSessionID = "";
  let seq = 0;
  const mockBaseURL = process.env.S4_MOCK_BASE_URL;
  if (!mockBaseURL) throw new Error("S4_MOCK_BASE_URL is required for the active probe");

  pi.registerProvider("s4", {
    name: "S4 local deterministic provider",
    baseUrl: mockBaseURL,
    apiKey: "s4-local-noncredential",
    api: "openai-completions",
    models: [
      {
        id: "s4-model",
        name: "S4 deterministic tool model",
        reasoning: false,
        input: ["text"],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 32768,
        maxTokens: 1024,
      },
    ],
  });

  pi.on("session_start", async (_event, ctx) => {
    announcedSessionID = ctx.sessionManager.getSessionId();
    process.env.S4_NATIVE_SESSION_ID = announcedSessionID;
    recordSessionEvidence(product, "extension.session_start", announcedSessionID, {
      session_file: ctx.sessionManager.getSessionFile?.() ?? "",
    });
    record({
      event: "component.frame",
      product,
      frame: componentFrame("session.announce", `${product}-announce`, ++seq, {
        binding_id: `${product}-probe-binding`,
        native_session_id: announcedSessionID,
        cwd: process.cwd(),
        native_name: `${product}-s4`,
        product_event_seq: seq,
      }),
    });
  });

  pi.registerTool({
    name: "s4_identity",
    label: "S4 identity",
    description: "Return the exact native session identity for the S4 probe.",
    parameters: { type: "object", properties: {}, additionalProperties: false },
    async execute(toolCallID, _params, _signal, _onUpdate, ctx) {
      const contextSessionID = ctx.sessionManager.getSessionId();
      recordSessionEvidence(product, "registered_tool.context", contextSessionID, {
        tool_call_id: toolCallID,
        announced_session_id: announcedSessionID,
        environment_session_id: process.env.S4_NATIVE_SESSION_ID ?? "",
      });
      record({
        event: "component.frame",
        product,
        frame: componentFrame("tool.call", toolCallID, ++seq, {
          binding_id: `${product}-probe-binding`,
          native_session_id: contextSessionID,
          operation: "lane.start",
          arguments: { probe: true },
        }),
      });
      return {
        content: [{ type: "text", text: `native-session:${contextSessionID}` }],
        details: { nativeSessionID: contextSessionID },
      };
    },
  });

  pi.on("agent_end", async (_event, ctx) => {
    const nativeSessionID = ctx.sessionManager.getSessionId();
    record({
      event: "component.frame",
      product,
      frame: componentFrame("turn.event", `${product}-settled`, ++seq, {
        binding_id: `${product}-probe-binding`,
        native_session_id: nativeSessionID,
        event_seq: seq,
        kind: "settled",
        metadata: {},
      }),
    });
  });
}
