import http from "node:http";

function writeSSE(response, chunks) {
  response.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-cache",
    connection: "keep-alive",
  });
  for (const chunk of chunks) response.write(`data: ${JSON.stringify(chunk)}\n\n`);
  response.end("data: [DONE]\n\n");
}

export async function startMockOpenAI() {
  let requests = 0;
  const server = http.createServer((request, response) => {
    if (request.method === "GET" && request.url === "/v1/models") {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify({ object: "list", data: [{ id: "s4-model", object: "model" }] }));
      return;
    }
    if (request.method !== "POST" || request.url !== "/v1/chat/completions") {
      response.writeHead(404).end();
      return;
    }
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk) => { body += chunk; });
    request.on("end", () => {
      requests += 1;
      const parsed = JSON.parse(body);
      const hasToolResult = parsed.messages?.some((message) => message.role === "tool");
      const base = {
        id: `chatcmpl-s4-${requests}`,
        object: "chat.completion.chunk",
        created: Math.floor(Date.now() / 1000),
        model: "s4-model",
      };
      if (!hasToolResult) {
        writeSSE(response, [
          {
            ...base,
            choices: [{
              index: 0,
              delta: {
                role: "assistant",
                tool_calls: [{
                  index: 0,
                  id: "call_s4_identity",
                  type: "function",
                  function: { name: "s4_identity", arguments: "{}" },
                }],
              },
              finish_reason: null,
            }],
          },
          { ...base, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }] },
        ]);
        return;
      }
      writeSSE(response, [
        { ...base, choices: [{ index: 0, delta: { role: "assistant", content: "S4_OK" }, finish_reason: null }] },
        { ...base, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] },
      ]);
    });
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  return {
    baseURL: `http://127.0.0.1:${address.port}/v1`,
    requestCount: () => requests,
    close: () => new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve())),
  };
}
