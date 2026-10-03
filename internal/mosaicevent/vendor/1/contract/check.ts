import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { isDeepStrictEqual } from "node:util";
import { loadCases, report, version, type Report } from "./validate.js";

export function compare(ts: Report, go: Report): void {
  if (ts.schemaVersion !== version || go.schemaVersion !== version || !isDeepStrictEqual(ts, go)) {
    throw new Error("TypeScript/Go acceptance disagreement");
  }
  const expected = loadCases();
  if (ts.cases.length !== expected.length) throw new Error("fixture count mismatch");
  for (const [index, verdict] of ts.cases.entries()) {
    const entry = expected[index];
    if (verdict.id !== entry.id || verdict.accepted !== entry.valid || !Number.isSafeInteger(verdict.eventCount) || verdict.eventCount < 1) {
      throw new Error(`unexpected contract verdict ${verdict.id}`);
    }
  }
}

export function runGo(binary: string): Report {
  // Crashes, timeout, nonzero exit and malformed output propagate; no green fallback.
  return JSON.parse(execFileSync(binary, [], { encoding: "utf8", timeout: 60_000, maxBuffer: 1024 * 1024 })) as Report;
}

export async function check(binary: string): Promise<void> {
  const ts = await report(), go = runGo(binary);
  compare(ts, go);
  // Every CI invocation proves that an altered verdict/count cannot pass the comparator.
  const changed = structuredClone(go); changed.cases[0].accepted = !changed.cases[0].accepted;
  for (const invalid of [changed, { ...go, cases: go.cases.slice(1) }]) {
    let rejected = false;
    try { compare(ts, invalid); } catch { rejected = true; }
    if (!rejected) throw new Error("disagreement negative control unexpectedly passed");
  }
  console.log(`mosaic-event/1: ${ts.cases.length} cases, ${ts.cases.reduce((sum, entry) => sum + entry.eventCount, 0)} events; TS/Go parity and negative controls passed`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  if (!process.argv[2]) throw new Error("usage: check.ts <Go contract binary>");
  await check(process.argv[2]);
}
