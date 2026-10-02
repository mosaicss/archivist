import { readFileSync, readdirSync } from "node:fs";
import { createHash } from "node:crypto";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { Ajv } from "ajv";
import { uiMessageChunkSchema } from "ai";
import { safeValidateTypes } from "@ai-sdk/provider-utils";
import { normalizeClaude, normalizeCodex, normalizeSSE } from "./normalize.js";

export const bundle = dirname(dirname(fileURLToPath(import.meta.url)));
export const version = "mosaic-event/1";
export const readJSON = <T>(path: string): T => JSON.parse(readFileSync(path, "utf8")) as T;
export const sha256 = (bytes: string | Buffer): string => createHash("sha256").update(bytes).digest("hex");
export type Case = { id: string; kind: "chunk" | "envelope"; valid: boolean; input: string; source: "capture" | "synthetic" };
export type Verdict = { id: string; accepted: boolean; eventCount: number };
export type Report = { schemaVersion: string; bundleDigest: string; cases: Verdict[] };

function files(root: string, dir = root): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    if (entry.name === "node_modules") return [];
    const path = join(dir, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`bundle symlink ${path}`);
    return entry.isDirectory() ? files(root, path) : [relative(root, path).replaceAll("\\", "/")];
  }).sort();
}

export function verifyBundle(root = bundle): string {
  const bytes = readFileSync(join(root, "manifest.json"));
  const digest = sha256(bytes);
  if (readFileSync(join(root, "manifest.sha256"), "utf8").trim() !== digest) throw new Error("manifest digest drift");
  const manifest = JSON.parse(bytes.toString()) as { schemaVersion: string; files: { path: string; sha256: string }[] };
  if (manifest.schemaVersion !== version || !manifest.files.length) throw new Error("invalid manifest version/count");
  const expected = manifest.files.map((file) => file.path);
  const actual = files(root).filter((path) => !["manifest.json", "manifest.sha256"].includes(path));
  if (JSON.stringify(expected) !== JSON.stringify(actual)) throw new Error("bundle file inventory drift");
  for (const file of manifest.files) {
    if (sha256(readFileSync(join(root, file.path))) !== file.sha256) throw new Error(`bundle drift: ${file.path}`);
  }
  return digest;
}

const ajv = new Ajv({ strict: true, allErrors: true, coerceTypes: false, useDefaults: false, removeAdditional: false });
for (const name of readdirSync(bundle).filter((name) => name.endsWith(".json") && !["manifest.json", "provenance.json", "cases.json"].includes(name))) {
  ajv.addSchema(readJSON<Record<string, unknown>>(join(bundle, name)));
}
const chunkSchema = ajv.getSchema("https://mosaic-finance.com/schemas/mosaic-event/1/chunk.json")!;
const envelopeSchema = ajv.getSchema("https://mosaic-finance.com/schemas/mosaic-event/1/envelope.json")!;

export async function validate(value: unknown, kind: "chunk" | "envelope" = "chunk"): Promise<boolean> {
  if (!(kind === "chunk" ? chunkSchema : envelopeSchema)(value)) return false;
  const envelope = value as Record<string, unknown>;
  if (kind === "envelope" && envelope.type === "approval_resolved") {
    const resolution = envelope.payload as Record<string, unknown>;
    return envelope.correlationId === `resolved:${String(resolution.correlationId)}`;
  }
  const chunk = kind === "envelope" ? envelope.payload : value;
  // SDK validation is complementary: open data payloads are narrowed by the canonical schemas.
  return (await safeValidateTypes({ value: chunk, schema: uiMessageChunkSchema })).success;
}

export function loadCases(): Case[] {
  const corpus = readJSON<{ schemaVersion: string; cases: Case[] }>(join(bundle, "cases.json"));
  if (corpus.schemaVersion !== version || !corpus.cases.length ||
    new Set(corpus.cases.map((entry) => entry.id)).size !== corpus.cases.length ||
    !corpus.cases.some((entry) => entry.valid) || !corpus.cases.some((entry) => !entry.valid)) {
    throw new Error("invalid/empty contract corpus");
  }
  return corpus.cases;
}

export function checkNormalization(): void {
  const provenance = readJSON<{ schemaVersion: string; captures: { id: string; protocol: string; input: string; inputEncoding?: string; normalized: string; originalSha256: string }[] }>(join(bundle, "provenance.json"));
  if (provenance.schemaVersion !== version || !provenance.captures.length) throw new Error("missing capture provenance");
  for (const capture of provenance.captures) {
    if (!/^[a-f0-9]{64}$/.test(capture.originalSha256)) throw new Error("missing original digest");
    const raw = readFileSync(join(bundle, capture.input), "utf8");
    const normalized = capture.protocol === "sse" ? normalizeSSE(capture.inputEncoding === "json-string" ? JSON.parse(raw) : raw, capture.id)
      : capture.protocol === "claude" ? normalizeClaude(JSON.parse(raw))
      : capture.protocol === "codex" ? normalizeCodex(JSON.parse(raw)) : undefined;
    if (!normalized || JSON.stringify(normalized) !== JSON.stringify(readJSON(join(bundle, capture.normalized)))) {
      throw new Error(`normalization drift: ${capture.id}`);
    }
    if (!loadCases().some((entry) => entry.source === "capture" && entry.input === capture.normalized)) {
      throw new Error(`capture omitted from corpus: ${capture.id}`);
    }
  }
}

export async function report(): Promise<Report> {
  const bundleDigest = verifyBundle(); checkNormalization();
  const cases: Verdict[] = [];
  for (const entry of loadCases()) {
    const events = readJSON<unknown[]>(join(bundle, entry.input));
    if (!Array.isArray(events) || !events.length) throw new Error(`empty fixture ${entry.id}`);
    let accepted = true;
    for (const event of events) if (!(await validate(event, entry.kind))) accepted = false;
    cases.push({ id: entry.id, accepted, eventCount: events.length });
  }
  return { schemaVersion: version, bundleDigest, cases };
}
