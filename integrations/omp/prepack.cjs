"use strict";

const fs = require("node:fs");
const path = require("node:path");

for (const [source, destination, rewriteSharedImport] of [
  ["agent-sessions.mjs", "plugin/agent-sessions.mjs", false],
  ["../pi/pifamily.mjs", "pi/pifamily.mjs", true],
  ["../shared/live-session.js", "shared/live-session.cjs", false],
]) {
  const target = path.join(__dirname, destination);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  if (!rewriteSharedImport) {
    fs.copyFileSync(path.join(__dirname, source), target);
    continue;
  }
  const encoded = fs.readFileSync(path.join(__dirname, source), "utf8");
  const packed = encoded.replace("../shared/live-session.js", "../shared/live-session.cjs");
  if (packed === encoded) throw new Error(`${source} omitted its shared client import`);
  fs.writeFileSync(target, packed);
}
