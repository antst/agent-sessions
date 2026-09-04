"use strict";

const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { pathToFileURL } = require("node:url");

const root = __dirname;
const repository = "git+https://github.com/antst/agent-sessions.git";
const packageSpecs = [
  {
    packagePath: "dsh/comms", name: "@agent-sessions/dsh-comms",
    bundled: [["shared/live-session.cjs", "shared/live-session.js"], ["shared/lane-worker.schema.json", "shared/lane-worker.schema.json"]],
    generated: ["shared"],
  },
  { packagePath: "dsh/lane", name: "@agent-sessions/dsh-lane", commsPeer: true },
  {
    packagePath: "opencode", name: "@agent-sessions/opencode",
    bundled: [
      ["plugin/agent-sessions.mjs", "opencode/agent-sessions.mjs", true],
      ["shared/live-session.cjs", "shared/live-session.js"],
      ["shared/lane-worker.schema.json", "shared/lane-worker.schema.json"],
    ],
    generated: ["plugin", "shared"],
  },
  {
    packagePath: "kilo", name: "@agent-sessions/kilo",
    bundled: [
      ["plugin/agent-sessions.mjs", "kilo/agent-sessions.mjs", true],
      ["shared/live-session.cjs", "shared/live-session.js"],
      ["shared/lane-worker.schema.json", "shared/lane-worker.schema.json"],
    ],
    generated: ["plugin", "shared"],
  },
  {
    packagePath: "pi", name: "@agent-sessions/pi", importable: true, piExtension: true,
    bundled: [
      ["plugin/agent-sessions.mjs", "pi/agent-sessions.mjs"],
      ["plugin/pifamily.mjs", "pi/pifamily.mjs", true],
      ["shared/live-session.cjs", "shared/live-session.js"],
      ["shared/lane-worker.schema.json", "shared/lane-worker.schema.json"],
    ],
    generated: ["plugin", "shared"],
  },
  {
    packagePath: "omp", name: "@agent-sessions/omp", importable: true, ompExtension: true,
    bundled: [
      ["plugin/agent-sessions.mjs", "omp/agent-sessions.mjs"],
      ["pi/pifamily.mjs", "pi/pifamily.mjs", true],
      ["shared/live-session.cjs", "shared/live-session.js"],
      ["shared/lane-worker.schema.json", "shared/lane-worker.schema.json"],
    ],
    generated: ["plugin", "pi", "shared"],
  },
];
const packageNames = new Set(packageSpecs.map(({ name }) => name));

function pack(packagePath, output) {
  const encoded = execFileSync("npm", ["pack", "--json", "--pack-destination", output], {
    cwd: packagePath, encoding: "utf8",
  });
  const result = JSON.parse(encoded);
  assert.equal(result.length, 1);
  assert.match(result[0].integrity, /^sha512-/u);
  return { archive: path.join(output, result[0].filename), result: result[0] };
}

function publishDryRun(packed) {
  const encoded = execFileSync(
    "npm", ["publish", "--dry-run", "--json", "--access", "public", packed.archive],
    { encoding: "utf8" },
  );
  const parsed = JSON.parse(encoded);
  return parsed[packed.result.name] ?? parsed;
}

function walk(directory) {
  const files = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const target = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...walk(target));
    else if (entry.isFile()) files.push(target);
  }
  return files;
}

function assertSelfContained(packageRoot, manifest) {
  const entry = path.resolve(packageRoot, manifest.main);
  assert.ok(entry.startsWith(`${packageRoot}${path.sep}`));
  assert.ok(fs.statSync(entry).isFile(), `${manifest.name} main is not packed`);

  for (const field of ["dependencies", "devDependencies", "optionalDependencies", "peerDependencies"]) {
    for (const [name, requested] of Object.entries(manifest[field] ?? {})) {
      assert.doesNotMatch(requested, /^(?:file:|workspace:)/u, `${manifest.name} has ${field} ${name}=${requested}`);
      if (packageNames.has(name)) assert.equal(requested, "0.4.0");
    }
  }

  const patterns = [
    /\b(?:import|export)\s+(?:[^"']*?\s+from\s+)?["'](\.{1,2}\/[^"']+)["']/gu,
    /\brequire\(\s*["'](\.{1,2}\/[^"']+)["']\s*\)/gu,
  ];
  for (const file of walk(packageRoot).filter((entryPath) => /\.(?:cjs|js|mjs)$/u.test(entryPath))) {
    const source = fs.readFileSync(file, "utf8");
    for (const pattern of patterns) {
      for (const match of source.matchAll(pattern)) {
        const target = path.resolve(path.dirname(file), match[1]);
        assert.ok(target.startsWith(`${packageRoot}${path.sep}`), `${manifest.name} import escapes its package: ${match[1]}`);
        assert.ok(fs.existsSync(target), `${manifest.name} import is absent from its package: ${match[1]}`);
      }
    }
  }
}

test("all npm packages are exact public, self-contained installation artifacts", async () => {
  const output = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-pack-"));
  const extract = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-extract-"));
  try {
    const inventory = execFileSync(path.join(root, "..", "scripts", "release-inventory"), ["packages"], {
      encoding: "utf8",
    }).trim().split("\n");
    assert.deepEqual(inventory, packageSpecs.map(({ packagePath, name }) => `integrations/${packagePath}|${name}`));

    for (const spec of packageSpecs) {
      const packagePath = path.join(root, spec.packagePath);
      const packed = pack(packagePath, output);
      assert.equal(publishDryRun(packed).id, `${spec.name}@0.4.0`);

      const packageExtract = path.join(extract, spec.name.replaceAll("/", "-"));
      fs.mkdirSync(packageExtract);
      execFileSync("tar", ["-xzf", packed.archive, "-C", packageExtract]);
      const packageRoot = path.join(packageExtract, "package");
      const manifest = JSON.parse(fs.readFileSync(path.join(packageRoot, "package.json"), "utf8"));
      assert.equal(manifest.name, spec.name);
      assert.equal(manifest.version, "0.4.0");
      assert.equal(manifest.private, undefined);
      assert.ok(Array.isArray(manifest.files) && manifest.files.length > 0);
      assert.deepEqual(manifest.publishConfig, { access: "public" });
      assert.deepEqual(manifest.repository, {
        type: "git", url: repository, directory: `integrations/${spec.packagePath}`,
      });
      if (spec.piExtension) {
        assert.ok(manifest.keywords.includes("pi-package"));
        assert.deepEqual(manifest.pi, { extensions: ["./plugin/agent-sessions.mjs"] });
      }
      if (spec.ompExtension) {
        assert.deepEqual(manifest.omp, { extensions: ["./plugin/agent-sessions.mjs"] });
        assert.ok(fs.statSync(path.join(packageRoot, manifest.omp.extensions[0])).isFile());
      }
      if (spec.commsPeer) {
        assert.equal(manifest.dependencies?.["@agent-sessions/dsh-comms"], undefined);
        assert.equal(manifest.peerDependencies?.["@agent-sessions/dsh-comms"], "0.4.0");
      }
      assertSelfContained(packageRoot, manifest);
      if (spec.importable) {
        const loaded = await import(pathToFileURL(path.join(packageRoot, manifest.main)).href);
        assert.equal(typeof loaded.default, "function", `${spec.name} packed entrypoint is not importable`);
      }
      for (const [packedPath, sourcePath, rewriteSharedImport] of spec.bundled ?? []) {
        const source = fs.readFileSync(path.join(root, sourcePath));
        const expected = rewriteSharedImport
          ? Buffer.from(source.toString("utf8").replace("../shared/live-session.js", "../shared/live-session.cjs"))
          : source;
        assert.deepEqual(
          fs.readFileSync(path.join(packageRoot, packedPath)),
          expected,
          `${spec.name} did not bundle ${sourcePath} exactly`,
        );
      }
    }
  } finally {
    fs.rmSync(output, { recursive: true, force: true });
    fs.rmSync(extract, { recursive: true, force: true });
    for (const spec of packageSpecs) {
      for (const generated of spec.generated ?? []) {
        fs.rmSync(path.join(root, spec.packagePath, generated), { recursive: true, force: true });
      }
    }
  }
});

test("pack-packages stages all six at one prerelease without changing source manifests", () => {
  const output = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-prerelease-"));
  const extract = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-prerelease-extract-"));
  const source = new Map(packageSpecs.map(({ packagePath }) => [
    packagePath, fs.readFileSync(path.join(root, packagePath, "package.json")),
  ]));
  try {
    execFileSync(path.join(root, "..", "scripts", "pack-packages"), [], {
      env: { ...process.env, PACKAGE_OUTPUT_DIR: output, PRERELEASE: "pre.1" },
      encoding: "utf8",
    });
    const archives = fs.readdirSync(output).filter((entry) => entry.endsWith(".tgz")).sort();
    assert.deepEqual(archives, [
      "agent-sessions-dsh-comms-0.4.0-pre.1.tgz",
      "agent-sessions-dsh-lane-0.4.0-pre.1.tgz",
      "agent-sessions-kilo-0.4.0-pre.1.tgz",
      "agent-sessions-omp-0.4.0-pre.1.tgz",
      "agent-sessions-opencode-0.4.0-pre.1.tgz",
      "agent-sessions-pi-0.4.0-pre.1.tgz",
    ]);
    for (const archive of archives) {
      fs.rmSync(path.join(extract, "package"), { recursive: true, force: true });
      execFileSync("tar", ["-xzf", path.join(output, archive), "-C", extract]);
      const manifest = JSON.parse(fs.readFileSync(path.join(extract, "package", "package.json"), "utf8"));
      assert.equal(manifest.version, "0.4.0-pre.1");
      for (const field of ["dependencies", "devDependencies", "optionalDependencies", "peerDependencies"]) {
        for (const [name, requested] of Object.entries(manifest[field] ?? {})) {
          assert.doesNotMatch(requested, /^(?:file:|workspace:)/u);
          if (packageNames.has(name)) assert.equal(requested, "0.4.0-pre.1");
        }
      }
    }
    for (const [packagePath, encoded] of source) {
      assert.deepEqual(fs.readFileSync(path.join(root, packagePath, "package.json")), encoded);
    }
  } finally {
    fs.rmSync(output, { recursive: true, force: true });
    fs.rmSync(extract, { recursive: true, force: true });
  }
});

test("pack-packages accepts exact inventory names for a package subset", () => {
  const output = fs.mkdtempSync(path.join(os.tmpdir(), "agent-sessions-subset-"));
  try {
    execFileSync(path.join(root, "..", "scripts", "pack-packages"), [
      "@agent-sessions/dsh-comms", "@agent-sessions/dsh-lane",
    ], { env: { ...process.env, PACKAGE_OUTPUT_DIR: output }, encoding: "utf8" });
    assert.deepEqual(fs.readdirSync(output).filter((entry) => entry.endsWith(".tgz")).sort(), [
      "agent-sessions-dsh-comms-0.4.0.tgz",
      "agent-sessions-dsh-lane-0.4.0.tgz",
    ]);
  } finally {
    fs.rmSync(output, { recursive: true, force: true });
  }
});
