// Explicit maintenance command; CI only verifies and never rewrites canonical hashes/goldens.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";
const root = dirname(dirname(fileURLToPath(import.meta.url)));
const hash = (bytes: Buffer): string => createHash("sha256").update(bytes).digest("hex");
const walk = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
  const path = join(dir, entry.name);
  if (entry.name === "node_modules") return [];
  if (entry.isSymbolicLink()) throw new Error(`bundle symlink ${path}`);
  return entry.isDirectory() ? walk(path) : [relative(root, path)];
});
const files = walk(root).filter((path) => !["manifest.json", "manifest.sha256"].includes(path)).sort()
  .map((path) => ({ path, sha256: hash(readFileSync(join(root, path))) }));
const manifest = Buffer.from(JSON.stringify({ schemaVersion: "mosaic-event/1", files }, null, 2) + "\n");
writeFileSync(join(root, "manifest.json"), manifest);
writeFileSync(join(root, "manifest.sha256"), hash(manifest) + "\n");
console.log(hash(manifest));
