import assert from "node:assert/strict";
import test from "node:test";

import extension from "./agent-sessions.mjs";

test("OMP entrypoint exports the shared extension factory result", () => {
  assert.equal(typeof extension, "function");
});
