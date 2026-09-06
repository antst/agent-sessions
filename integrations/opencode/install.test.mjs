import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { parse } from "jsonc-parser";
import { configure } from "./install.mjs";

function fixture() {
  const root = mkdtempSync(path.join(os.tmpdir(), "sessionbus-opencode-install-"));
  const directory = path.join(root, "opencode");
  const plugin = path.join(root, "plugin");
  mkdirSync(directory);
  mkdirSync(plugin);
  writeFileSync(path.join(plugin, "package.json"), '{"name":"@sessionbus/opencode"}\n');
  return { directory, specifier: `file:${plugin}` };
}

function value(file) {
  return parse(readFileSync(file, "utf8"));
}

test("commented selected config converges once and preserves an unrelated tuple verbatim", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "opencode.jsonc");
  const tuple = '["other-plugin", { "native": true }]';
  writeFileSync(selected, `{// keep root\n  "theme": "native",\n  "plugin": [\n    ${tuple}, // keep tuple\n    "@sessionbus/opencode@0.0.1",\n  ],\n}\n`);
  assert.equal(configure({ directory, specifier }), true);
  const body = readFileSync(selected, "utf8");
  assert.match(body, /\/\/ keep root/u);
  assert.match(body, /\/\/ keep tuple/u);
  assert.ok(body.includes(tuple));
  assert.deepEqual(value(selected), { theme: "native", plugin: [["other-plugin", { native: true }], specifier] });
  assert.equal(configure({ directory, specifier }), false);
  assert.equal(readFileSync(selected, "utf8"), body);
});

test("install and remove own entries across every merged config file", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "opencode.json");
  const alternate = path.join(directory, "config.json");
  writeFileSync(selected, '{"plugin":["other-plugin"]}\n');
  writeFileSync(alternate, '{"model":"native/model","plugin":[["@sessionbus/opencode@old",{"x":1}],"last-plugin"]}\n');
  assert.equal(configure({ directory, specifier }), true);
  assert.deepEqual(value(selected), { plugin: ["other-plugin", specifier] });
  assert.deepEqual(value(alternate), { model: "native/model", plugin: ["last-plugin"] });
  assert.equal(configure({ directory, remove: true }), true);
  assert.deepEqual(value(selected), { plugin: ["other-plugin"] });
  assert.deepEqual(value(alternate), { model: "native/model", plugin: ["last-plugin"] });
  assert.equal(configure({ directory, remove: true }), false);
});

test("local and remote archives are replaced without duplicate entries", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "config.json");
  const archive = `file:${path.join(directory, "sessionbus-opencode-0.1.0-pre.0.tgz")}`;
  writeFileSync(selected, `${JSON.stringify({ plugin: [archive, "https://pkg.pr.new/antst/sessionbus/@sessionbus/opencode@old", "other-plugin"] })}\n`);
  assert.equal(configure({ directory, specifier }), true);
  assert.deepEqual(value(selected), { plugin: ["other-plugin", specifier] });
});

test("an invalid merged file or failed transaction leaves every file byte-exact", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "opencode.jsonc");
  const alternate = path.join(directory, "opencode.json");
  writeFileSync(selected, '{// selected\n"theme":"native"}\n');
  writeFileSync(alternate, '{broken\n');
  const beforeSelected = readFileSync(selected, "utf8");
  const beforeAlternate = readFileSync(alternate, "utf8");
  assert.throws(() => configure({ directory, specifier }), /not valid JSONC/u);
  assert.equal(readFileSync(selected, "utf8"), beforeSelected);
  assert.equal(readFileSync(alternate, "utf8"), beforeAlternate);
  writeFileSync(alternate, '{"plugin":["other-plugin"]}\n');
  const validAlternate = readFileSync(alternate, "utf8");
  assert.throws(() => configure({ directory, specifier, commit() { throw new Error("disk full"); } }), /disk full/u);
  assert.equal(readFileSync(selected, "utf8"), beforeSelected);
  assert.equal(readFileSync(alternate, "utf8"), validAlternate);
});
