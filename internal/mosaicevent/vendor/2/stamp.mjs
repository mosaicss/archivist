// mosaic-event/2 manifest: `node stamp.mjs` rewrites it after a reviewed edit; `node stamp.mjs
// --check` only verifies it and exits non-zero on any drift. Node built-ins only; the manifest
// format and path sorting are those of v1 contract/stamp.ts. CI and consumers never restamp.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";

const VERSION = "mosaic-event/2";
const root = dirname(fileURLToPath(import.meta.url));
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
const walk = (dir) =>
  readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.name === "node_modules") throw new Error(`unexpected node_modules ${path}`);
    if (entry.isSymbolicLink()) throw new Error(`bundle symlink ${path}`);
    return entry.isDirectory() ? walk(path) : [relative(root, path).split(sep).join("/")];
  });
const files = walk(root)
  .filter((path) => !["manifest.json", "manifest.sha256"].includes(path))
  .sort()
  .map((path) => ({ path, sha256: hash(readFileSync(join(root, path))) }));
const manifest = Buffer.from(JSON.stringify({ schemaVersion: VERSION, files }, null, 2) + "\n");
const digest = hash(manifest);

if (process.argv.includes("--check")) {
  const read = (name) => {
    try {
      return readFileSync(join(root, name));
    } catch {
      return Buffer.alloc(0);
    }
  };
  const problems = [];
  if (!read("manifest.json").equals(manifest)) problems.push("manifest.json is not current");
  if (read("manifest.sha256").toString("utf8") !== `${digest}\n`)
    problems.push("manifest.sha256 is not current");
  if (problems.length) {
    console.error(`${VERSION}: ${problems.join("; ")} (run node stamp.mjs after review)`);
    process.exit(1);
  }
  console.log(`${VERSION} manifest current: ${digest} (${files.length} files)`);
} else {
  writeFileSync(join(root, "manifest.json"), manifest);
  writeFileSync(join(root, "manifest.sha256"), `${digest}\n`);
  console.log(digest);
}
