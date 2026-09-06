import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { parse } from "jsonc-parser";
import { configure } from "./install.mjs";

function fixture() {
  const root = mkdtempSync(path.join(os.tmpdir(), "sessionbus-opencode-install-"));
  const directory = path.join(root, "opencode");
  const plugin = path.join(root, "@sessionbus", "opencode");
  mkdirSync(directory);
  mkdirSync(plugin, { recursive: true });
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
  const remote = "https://packages.example/sessionbus-opencode-0.1.0-pre.0.tgz#sha256=old";
  writeFileSync(selected, `${JSON.stringify({ plugin: [archive, remote, "other-plugin"] })}\n`);
  assert.equal(configure({ directory, specifier }), true);
  assert.deepEqual(value(selected), { plugin: ["other-plugin", specifier] });
});

test("HTTP npm archives are replaced and removed as one owned package", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "opencode.jsonc");
  const archive = "http://127.0.0.1:34567/sessionbus-opencode-0.1.0-pre.0.tgz?download=1";
  writeFileSync(selected, `${JSON.stringify({ plugin: [archive, "other-plugin"] })}\n`);
  assert.equal(configure({ directory, specifier }), true);
  assert.deepEqual(value(selected), { plugin: ["other-plugin", specifier] });
  assert.equal(configure({ directory, remove: true }), true);
  assert.deepEqual(value(selected), { plugin: ["other-plugin"] });
});

test("unrelated package tokens and URL queries are preserved on install and remove", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "opencode.jsonc");
  const unrelatedName = "not@sessionbus/opencode";
  const unrelatedURL = "https://example.invalid/unrelated.tgz?note=@sessionbus/opencode";
  const unrelatedDirectory = path.join(directory, "unrelated");
  mkdirSync(unrelatedDirectory);
  writeFileSync(path.join(unrelatedDirectory, "package.json"), '{"name":"unrelated"}\n');
  const unrelatedFile = `file:${unrelatedDirectory}`;
  writeFileSync(selected, `${JSON.stringify({ plugin: [unrelatedName, [unrelatedURL, { native: true }], unrelatedFile] })}\n`);
  assert.equal(configure({ directory, specifier }), true);
  assert.deepEqual(value(selected), { plugin: [unrelatedName, [unrelatedURL, { native: true }], unrelatedFile, specifier] });
  assert.equal(configure({ directory, remove: true }), true);
  assert.deepEqual(value(selected), { plugin: [unrelatedName, [unrelatedURL, { native: true }], unrelatedFile] });
});

test("requested package, directory, and archive forms each converge and remove", () => {
  const root = mkdtempSync(path.join(os.tmpdir(), "sessionbus-opencode-forms-"));
  const plugin = path.join(root, "plugin");
  mkdirSync(plugin);
  writeFileSync(path.join(plugin, "package.json"), '{"name":"@sessionbus/opencode"}\n');
  const archive = path.join(root, "sessionbus-opencode-0.1.0-pre.1.tgz");
  writeFileSync(archive, "package");
  const forms = ["@sessionbus/opencode@0.1.0-pre.1", `file:${plugin}`, `file:${archive}`, "https://packages.example/sessionbus-opencode-0.1.0-pre.1.tgz?download=1"];
  for (const [index, specifier] of forms.entries()) {
    const directory = path.join(root, String(index));
    mkdirSync(directory);
    assert.equal(configure({ directory, specifier }), true);
    assert.equal(configure({ directory, specifier }), false);
    assert.deepEqual(value(path.join(directory, "opencode.jsonc")), { plugin: [specifier] });
    assert.equal(configure({ directory, remove: true }), true);
    assert.equal(configure({ directory, remove: true }), false);
  }
});

test("unrelated or unsupported requested packages are rejected without edits", () => {
  const { directory } = fixture();
  const selected = path.join(directory, "opencode.jsonc");
  const unrelated = path.join(directory, "unrelated");
  mkdirSync(unrelated);
  writeFileSync(path.join(unrelated, "package.json"), '{"name":"unrelated"}\n');
  writeFileSync(selected, '{// untouched\n"plugin":["native"]}\n');
  const before = readFileSync(selected, "utf8");
  for (const specifier of ["https://example.invalid/unrelated.tgz?note=@sessionbus/opencode", `file:${unrelated}`, "sessionbus-opencode-0.1.0-pre.1.tgz", "ftp://example/sessionbus-opencode-0.1.0-pre.1.tgz"]) {
    assert.throws(() => configure({ directory, specifier }), /not an installable @sessionbus\/opencode/u);
    assert.equal(readFileSync(selected, "utf8"), before);
  }
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

test("partial multi-file commit failure restores every byte and removes transaction files", () => {
  const { directory, specifier } = fixture();
  const selected = path.join(directory, "opencode.jsonc");
  const alternate = path.join(directory, "opencode.json");
  writeFileSync(selected, '{// selected\n"plugin":["@sessionbus/opencode@old","one"]}\n');
  writeFileSync(alternate, '{"plugin":["@sessionbus/opencode@older","two"]}\n');
  const before = [readFileSync(selected, "utf8"), readFileSync(alternate, "utf8")];
  let installed = 0;
  assert.throws(() => configure({ directory, specifier, rename(from, to) {
    renameSync(from, to);
    if (from.endsWith(".new") && ++installed === 1) throw new Error("disk disappeared after first install");
  } }), /disk disappeared/u);
  assert.deepEqual([readFileSync(selected, "utf8"), readFileSync(alternate, "utf8")], before);
  assert.deepEqual(readdirSync(directory).filter((name) => /\.(?:new|old)$/u.test(name)), []);
});

test("the packed plugin's real node_modules bin performs installation", () => {
  const root = mkdtempSync(path.join(os.tmpdir(), "sessionbus-opencode-pack-"));
  const packageRoot = path.dirname(fileURLToPath(import.meta.url));
  const importHome = path.join(root, "import-only");
  const imported = spawnSync(process.execPath, ["--input-type=module", "--eval", `import(${JSON.stringify(new URL("./install.mjs", import.meta.url).href)})`], { encoding: "utf8", env: { ...process.env, XDG_CONFIG_HOME: importHome } });
  assert.equal(imported.status, 0, imported.stderr);
  assert.deepEqual(readdirSync(root), []);
  const packed = spawnSync("npm", ["pack", "--json", "--ignore-scripts", "--pack-destination", root], { cwd: packageRoot, encoding: "utf8" });
  assert.equal(packed.status, 0, packed.stderr);
  const archive = path.join(root, JSON.parse(packed.stdout)[0].filename);
  writeFileSync(path.join(root, "package.json"), '{"private":true}\n');
  const installed = spawnSync("npm", ["install", "--ignore-scripts", "--omit=peer", "--no-package-lock", "--no-save", archive], { cwd: root, encoding: "utf8" });
  assert.equal(installed.status, 0, installed.stderr);
  const configHome = path.join(root, "config");
  const plugin = path.join(root, "@sessionbus", "opencode");
  mkdirSync(plugin, { recursive: true });
  writeFileSync(path.join(plugin, "package.json"), '{"name":"@sessionbus/opencode"}\n');
  const command = path.join(root, "node_modules", ".bin", "sessionbus-opencode-install");
  const invoked = spawnSync(command, ["--specifier", `file:${plugin}`], { encoding: "utf8", env: { ...process.env, XDG_CONFIG_HOME: configHome } });
  assert.equal(invoked.status, 0, invoked.stderr);
  assert.deepEqual(value(path.join(configHome, "opencode", "opencode.jsonc")), { plugin: [`file:${plugin}`] });
});
