"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { schema, validate } = require("./index.js");

const fixtures = JSON.parse(fs.readFileSync(path.join(__dirname, "../../internal/protocol/session.fixtures.json"), "utf8"));

for (const fixture of fixtures.cases) test(`schema: ${fixture.name}`, () => {
  const value = fixture.raw ? JSON.parse(fixture.raw) : structuredClone(fixture.value);
  if (fixture.repeat) {
    let parent = value;
    for (const key of fixture.repeat.path.slice(0, -1)) parent = parent[key];
    parent[fixture.repeat.path.at(-1)] = fixture.repeat.text.repeat(fixture.repeat.count);
  }
  assert.equal(validate(fixture.definition, value), fixture.valid);
});

test("schema export is the authoritative document", () => {
  assert.equal(schema.$id, "urn:sessionbus:session:v1");
  assert.deepEqual(Object.keys(schema).sort(), ["$defs", "$id", "$schema"]);
});
