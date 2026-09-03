"use strict";

const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const root = __dirname;

function pack(packagePath, output) {
  const encoded = execFileSync("npm", ["pack", "--json", "--pack-destination", output], {
    cwd: packagePath, encoding: "utf8",
  });
  const result = JSON.parse(encoded);
  assert.equal(result.length, 1);
  assert.match(result[0].integrity, /^sha512-/u);
  return path.join(output, result[0].filename);
}

test("DSH packages pack into the exact public artifacts used by installation", () => {
  const output = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-pack-"));
  const extract = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-extract-"));
  try {
    const commsArchive = pack(path.join(root, "comms"), output);
    const laneArchive = pack(path.join(root, "lane"), output);
    execFileSync("tar", ["-xzf", commsArchive, "-C", extract]);
    assert.deepEqual(
      fs.readFileSync(path.join(extract, "package", "shared", "live-session.cjs")),
      fs.readFileSync(path.join(root, "..", "shared", "live-session.js")),
    );
    fs.rmSync(path.join(extract, "package"), { recursive: true, force: true });
    execFileSync("tar", ["-xzf", laneArchive, "-C", extract]);
    const lanePackage = JSON.parse(fs.readFileSync(path.join(extract, "package", "package.json"), "utf8"));
    assert.equal(lanePackage.dependencies["@agent-sessions/dsh-comms"], "0.4.0");
    for (const packageName of ["comms", "lane"]) {
      const manifest = JSON.parse(fs.readFileSync(path.join(root, packageName, "package.json"), "utf8"));
      assert.equal(manifest.private, undefined);
      assert.deepEqual(manifest.publishConfig, { access: "public" });
    }
  } finally {
    fs.rmSync(output, { recursive: true, force: true });
    fs.rmSync(extract, { recursive: true, force: true });
    fs.rmSync(path.join(root, "comms", "shared"), { recursive: true, force: true });
  }
});

test("pack-packages stages an exact prerelease without changing source manifests", () => {
  const output = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-prerelease-"));
  const extract = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-dsh-prerelease-extract-"));
  const source = new Map(["comms", "lane"].map((packageName) => [
    packageName, fs.readFileSync(path.join(root, packageName, "package.json")),
  ]));
  try {
    execFileSync(path.join(root, "..", "..", "scripts", "pack-packages"), [], {
      env: { ...process.env, PACKAGE_OUTPUT_DIR: output, PRERELEASE: "pre.1" },
      encoding: "utf8",
    });
    const archives = fs.readdirSync(output).filter((entry) => entry.endsWith(".tgz")).sort();
    assert.deepEqual(archives, [
      "agent-sessions-dsh-comms-0.4.0-pre.1.tgz",
      "agent-sessions-dsh-lane-0.4.0-pre.1.tgz",
    ]);
    for (const archive of archives) {
      fs.rmSync(path.join(extract, "package"), { recursive: true, force: true });
      execFileSync("tar", ["-xzf", path.join(output, archive), "-C", extract]);
      const manifest = JSON.parse(fs.readFileSync(path.join(extract, "package", "package.json"), "utf8"));
      assert.equal(manifest.version, "0.4.0-pre.1");
      if (manifest.name === "@agent-sessions/dsh-lane") {
        assert.equal(manifest.dependencies["@agent-sessions/dsh-comms"], "0.4.0-pre.1");
      }
    }
    for (const [packageName, encoded] of source) {
      assert.deepEqual(fs.readFileSync(path.join(root, packageName, "package.json")), encoded);
    }
  } finally {
    fs.rmSync(output, { recursive: true, force: true });
    fs.rmSync(extract, { recursive: true, force: true });
    fs.rmSync(path.join(root, "comms", "shared"), { recursive: true, force: true });
  }
});
