import { test } from "node:test";
import assert from "node:assert/strict";
import { transitions } from "../src/state.ts";

test("REQ-001: pay moves pending to paid", (t) => {
  t.skip("graybox");
});

test("REQ-002: shipped cannot be cancelled", (t) => {
  t.skip("graybox");
});

test("REQ-003: unknown order throws NotFoundError", (t) => {
  t.skip("graybox");
});

// The transition table itself is checked now, not skipped.
test("transitions: terminal states have no exits", () => {
  assert.equal(transitions.shipped, undefined);
  assert.equal(transitions.cancelled, undefined);
});
