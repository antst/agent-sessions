"use strict";

const fs = require("node:fs");
const path = require("node:path");

const destination = path.join(__dirname, "shared", "live-session.cjs");
if (fs.existsSync(destination)) process.exit(0);
fs.mkdirSync(path.dirname(destination), { recursive: true });
fs.copyFileSync(path.join(__dirname, "..", "..", "shared", "live-session.js"), destination);
