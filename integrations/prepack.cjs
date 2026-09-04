"use strict";

const fs = require("node:fs");
const path = require("node:path");

const product = process.env.npm_package_name?.replace(/^@agent-sessions\//, "");
if (!["opencode", "kilo", "pi", "omp"].includes(product)) throw new Error(`unsupported package ${product}`);
const root = path.join(__dirname, product);
const family = {
  pi: ["pifamily.mjs", "plugin/pifamily.mjs", true],
  omp: ["../pi/pifamily.mjs", "pi/pifamily.mjs", true],
}[product];
const files = [
  ["agent-sessions.mjs", "plugin/agent-sessions.mjs", product === "opencode" || product === "kilo"],
  ...(family ? [family] : []),
  ["../shared/live-session.js", "shared/live-session.cjs", false],
];
for (const [source, destination, rewriteSharedImport] of files) {
  const target = path.join(root, destination);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  if (!rewriteSharedImport) {
    fs.copyFileSync(path.join(root, source), target);
    continue;
  }
  const encoded = fs.readFileSync(path.join(root, source), "utf8");
  const packed = encoded.replace("../shared/live-session.js", "../shared/live-session.cjs");
  if (packed === encoded) throw new Error(`${source} omitted its shared client import`);
  fs.writeFileSync(target, packed);
}
