import assert from "node:assert/strict";
import { test } from "node:test";
import { chmodSync, cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { compare, runGo } from "./check.js";
import { bundle, checkNormalization, loadCases, readJSON, report, sha256, validate, verifyBundle } from "./validate.js";
import { normalizeClaude, normalizeCodex, normalizeSSE, parseSSE } from "./normalize.js";

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

test("Claude total input includes cache reads and writes while cachedInputTokens means reads", () => {
  const raw = readJSON<unknown>(join(bundle, "fixtures/raw/02-claude-allow.json"));
  const usage = normalizeClaude(raw).find((event) => event.type === "data-usage")!.data as {
    inputTokens: number; cachedInputTokens: number;
  };
  assert.equal(usage.inputTokens, 39084);
  assert.equal(usage.cachedInputTokens, 31325);
});

test("SSE stamps legacy metadata, preserves compatible fields and rejects present incompatible versions", () => {
  const wire = (chunk: unknown): string => `data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`;
  const legacy = { type: "start", messageId: "retained", messageMetadata: { other: { finite: 1 } } };
  const compatible = { ...legacy, messageMetadata: { ...legacy.messageMetadata, schemaVersion: "mosaic-event/1" } };
  assert.deepEqual(normalizeSSE(wire(legacy), "injected")[0], compatible);
  assert.deepEqual(normalizeSSE(wire(compatible), "injected")[0], compatible);
  for (const schemaVersion of ["mosaic-event/2", "", null, 1, false, {}, []]) {
    assert.throws(() => normalizeSSE(wire({ ...legacy, messageMetadata: { schemaVersion } }), "test"), /schemaVersion/);
  }
});

test("synthetic Codex failed MCP item with null result/error remains a failure", async () => {
  const raw = readJSON<unknown>(join(bundle, "fixtures/raw/synthetic-codex-mcp-failed.json"));
  const normalized = normalizeCodex(raw);
  assert.deepEqual(normalized, readJSON(join(bundle, "fixtures/synthetic-codex-mcp-failed.json")));
  assert.equal(normalized[0].type, "tool-output-error");
  assert.equal(await validate(normalized[0]), true);
});

test("nonfinite numbers reject recursively in opaque JSON without changing the input", async () => {
  for (const value of [Infinity, -Infinity, NaN]) {
    for (const chunk of [
      { type: "tool-input-available", toolCallId: "t", toolName: "synthetic", input: { nested: [{ value }] } },
      { type: "tool-output-available", toolCallId: "t", output: { nested: [{ value }] } },
      { type: "start", messageMetadata: { schemaVersion: "mosaic-event/1", nested: [{ value }] } },
      { type: "text-start", id: "t", providerMetadata: { synthetic: { nested: [{ value }] } } },
      { type: "data-plan", data: { entries: [], _meta: { nested: [{ value }] } } },
    ]) {
      const before = structuredClone(chunk);
      assert.equal(await validate(chunk), false);
      assert.deepEqual(chunk, before);
    }
  }
  const literal = join(bundle, "fixtures/invalid-opaque-overflow-output.json");
  assert.match(readFileSync(literal, "utf8"), /1e400/);
  assert.equal(await validate(readJSON<unknown[]>(literal)[0]), false);
  const finite = readJSON<unknown[]>(join(bundle, "fixtures/valid-opaque-finite-output.json"))[0];
  const before = structuredClone(finite);
  assert.equal(await validate(finite), true);
  assert.deepEqual(finite, before);
});

test("corrupted negative-case kind is setup failure even with a coherent manifest", async () => {
  const work = mkdtempSync(join(tmpdir(), "mosaic-corpus-"));
  try {
    cpSync(bundle, work, { recursive: true, filter: (path) => !path.includes("node_modules") });
    const corpus = readJSON<{ cases: { valid: boolean; kind: string }[] }>(join(work, "cases.json"));
    corpus.cases.find((entry) => !entry.valid)!.kind = "chnuk";
    writeFileSync(join(work, "cases.json"), JSON.stringify(corpus, null, 2) + "\n");
    const manifest = readJSON<{ files: { path: string; sha256: string }[] }>(join(work, "manifest.json"));
    manifest.files.find((file) => file.path === "cases.json")!.sha256 = sha256(readFileSync(join(work, "cases.json")));
    const bytes = JSON.stringify(manifest, null, 2) + "\n";
    writeFileSync(join(work, "manifest.json"), bytes);
    writeFileSync(join(work, "manifest.sha256"), sha256(bytes) + "\n");
    verifyBundle(work);
    assert.throws(() => loadCases(work), /invalid corpus kind/);
    await assert.rejects(report(work), /invalid corpus kind/);
    assert.throws(() => execFileSync(process.execPath,
      ["--import", "tsx", join(bundle, "contract/report.ts"), work], { encoding: "utf8", stdio: "pipe" }),
      (error: unknown) => {
        const failed = error as { status: number; stderr: string };
        return failed.status !== 0 && /invalid corpus kind/.test(failed.stderr);
      });
  } finally { rmSync(work, { recursive: true, force: true }); }
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
