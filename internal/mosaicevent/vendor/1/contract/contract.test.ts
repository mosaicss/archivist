import assert from "node:assert/strict";
import { test } from "node:test";
import { chmodSync, cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { compare, runGo } from "./check.js";
import { bundle, checkNormalization, loadCases, readJSON, report, validate, verifyBundle } from "./validate.js";
import { normalizeSSE, parseSSE } from "./normalize.js";

test("every fixture has its expected verdict, with real SDK validation", async () => {
  const result = await report();
  compare(result, structuredClone(result));
  assert.equal(result.cases.length, loadCases().length);
});

test("captured replay is deterministic and does not duplicate partial text", () => {
  checkNormalization();
  const interrupted = readJSON<Record<string, unknown>[]>(join(bundle, "fixtures/04-claude-interrupt.json"));
  assert.equal(interrupted.filter((c) => c.type === "abort").length, 1);
  assert.equal(interrupted.filter((c) => c.type === "start").length, 2);
  const beforeAbort = interrupted.slice(0, interrupted.findIndex((c) => c.type === "abort"));
  assert.equal(beforeAbort.filter((c) => c.type === "text-delta").map((c) => c.delta).join(""),
    Array.from({ length: 29 }, (_, i) => i + 1).join("\n"));
  const denied = readJSON<Record<string, unknown>[]>(join(bundle, "fixtures/03-claude-deny.json"));
  assert.ok(denied.some((c) => c.type === "tool-approval-response" && c.approved === false));
  assert.ok(denied.some((c) => c.type === "tool-output-error"));
  const raw = readJSON<string>(join(bundle, "fixtures/raw/chat-api-78-2.sse.json"));
  assert.equal(parseSSE(raw).length, 21);
  assert.ok(!parseSSE(raw).some((c) => c.type === "start" || c.type === "finish"));
  assert.deepEqual(normalizeSSE(raw, "legacy").slice(1, -1), parseSSE(raw));
});

test("SSE heartbeats, multiline data and CRLF; malformed/terminal frames reject", () => {
  assert.deepEqual(parseSSE(': heartbeat\r\n\r\ndata: {"type":\r\ndata: "finish"}\r\n\r\ndata: [DONE]\r\n\r\n'), [{ type: "finish" }]);
  for (const wire of ["", "data: {}\n\n", "data: x\n\ndata: [DONE]\n\n",
    "data: {}\n\ndata: [DONE]\n\ndata: {}\n\n", "data: [DONE]\n\n",
    "event: answer\ndata: {}\n\ndata: [DONE]\n\n", "data: {}\n\ndata: [DONE]\n"]) {
    assert.throws(() => parseSSE(wire));
  }
});

test("schema validation preserves raw optional metadata and denial false", async () => {
  const chunk = { type: "tool-approval-response", approvalId: "deny", approved: false, reason: "decline" };
  const bytes = JSON.stringify(chunk);
  assert.equal(await validate(chunk), true);
  assert.equal(JSON.stringify(chunk), bytes);
  assert.equal(await validate({ ...chunk, approved: "false" }), false);
  assert.equal(await validate({ type: "data-status", data: "bad" }), false);
  assert.equal(await validate({ type: "custom", kind: "vendor.test" }), false);
});

test("manifest detects byte, missing, extra and version drift", () => {
  const work = mkdtempSync(join(tmpdir(), "mosaic-drift-"));
  try {
    cpSync(bundle, work, { recursive: true, filter: (path) => !path.includes("node_modules") });
    verifyBundle(work);
    const schema = join(work, "chunk.json"), bytes = readFileSync(schema);
    writeFileSync(schema, Buffer.concat([bytes, Buffer.from(" ")]));
    assert.throws(() => verifyBundle(work), /drift/);
    writeFileSync(schema, bytes);
    writeFileSync(join(work, "extra.json"), "{}");
    assert.throws(() => verifyBundle(work), /inventory/);
    rmSync(join(work, "extra.json")); rmSync(schema);
    assert.throws(() => verifyBundle(work), /inventory/);
  } finally { rmSync(work, { recursive: true, force: true }); }
});

test("acceptance disagreement, agreed wrong verdict, truncated reports and crashes fail", async () => {
  const valid = await report(), flipped = structuredClone(valid);
  flipped.cases[0].accepted = !flipped.cases[0].accepted;
  assert.throws(() => compare(valid, flipped), /disagreement/);
  assert.throws(() => compare(flipped, flipped), /unexpected/);
  assert.throws(() => compare(valid, { ...valid, cases: [] }), /disagreement/);
  const work = mkdtempSync(join(tmpdir(), "mosaic-process-"));
  try {
    const binary = join(work, "fake-go");
    for (const body of ["exit 23", "printf garbage", "printf '{}'", "printf ''"]) {
      writeFileSync(binary, `#!/bin/sh\n${body}\n`); chmodSync(binary, 0o700);
      assert.throws(() => compare(valid, runGo(binary)));
    }
  } finally { rmSync(work, { recursive: true, force: true }); }
});
