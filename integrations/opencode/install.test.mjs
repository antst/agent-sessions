import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { configure } from "./install.mjs";

function fixture() {
  const root = mkdtempSync(path.join(os.tmpdir(), "sessionbus-opencode-install-"));
  const file = path.join(root, "opencode", "opencode.json");
  const plugin = path.join(root, "plugin");
  mkdirSync(plugin);
  writeFileSync(path.join(plugin, "package.json"), '{"name":"@sessionbus/opencode"}\n');
  return { file, specifier: `file:${plugin}` };
}

test("install replaces its one entry and preserves unrelated config", () => {
  const { file, specifier } = fixture();
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, '{"model":"native/model","plugin":["other-plugin","@sessionbus/opencode@0.0.1"]}\n');
  assert.equal(configure({ file, specifier }), true);
  assert.deepEqual(JSON.parse(readFileSync(file, "utf8")), { model: "native/model", plugin: ["other-plugin", specifier] });
  const body = readFileSync(file, "utf8");
  assert.equal(configure({ file, specifier }), false);
  assert.equal(readFileSync(file, "utf8"), body);
});

test("remove is convergent and keeps every unrelated setting", () => {
  const { file, specifier } = fixture();
  configure({ file, specifier });
  const value = JSON.parse(readFileSync(file, "utf8"));
  value.theme = "native";
  value.plugin.unshift("other-plugin");
  writeFileSync(file, `${JSON.stringify(value)}\n`);
  assert.equal(configure({ file, remove: true }), true);
  assert.deepEqual(JSON.parse(readFileSync(file, "utf8")), { theme: "native", plugin: ["other-plugin"] });
  assert.equal(configure({ file, remove: true }), false);
});

test("install replaces local and remote archives without duplicating its entry", () => {
  const { file, specifier } = fixture();
  mkdirSync(path.dirname(file), { recursive: true });
  const priorArchive = `file:${path.join(path.dirname(file), "sessionbus-opencode-0.1.0-pre.0.tgz")}`;
  writeFileSync(file, `${JSON.stringify({ plugin: [priorArchive, "https://pkg.pr.new/antst/sessionbus/@sessionbus/opencode@old", "other-plugin"] })}\n`);
  assert.equal(configure({ file, specifier }), true);
  assert.deepEqual(JSON.parse(readFileSync(file, "utf8")), { plugin: ["other-plugin", specifier] });
  assert.equal(configure({ file, specifier }), false);
});

test("validation and write failures leave the prior config byte-exact", () => {
  const { file, specifier } = fixture();
  mkdirSync(path.dirname(file), { recursive: true });
  for (const invalid of ['{"plugin":{}}\n', '{broken\n']) {
    writeFileSync(file, invalid);
    assert.throws(() => configure({ file, specifier }), /config/u);
    assert.equal(readFileSync(file, "utf8"), invalid);
  }
  const original = '{"theme":"native"}\n';
  writeFileSync(file, original);
  assert.throws(() => configure({ file, specifier, write() { throw new Error("disk full"); } }), /disk full/u);
  assert.equal(readFileSync(file, "utf8"), original);
});
