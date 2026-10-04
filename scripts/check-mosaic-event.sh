#!/usr/bin/env bash
# Candidate Go parser + exact vendored canonical bundles. No private Mosaic access.
# v1: vendored TS contract package/corpus against the Go report. v2 and v3 (no contract package):
# source record, manifest digest, stamp.mjs --check and the Go corpus/drift tests.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
bundle="$root/internal/mosaicevent/vendor/1"
work="$(mktemp -d)"
trap 'rm -r -- "$work"' EXIT
node --input-type=module - "$root" <<'JS'
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import assert from 'node:assert/strict';
const root=process.argv[2];
for (const [record, version] of [['vendor-source.json', 'mosaic-event/1'], ['vendor-source-2.json', 'mosaic-event/2'], ['vendor-source-3.json', 'mosaic-event/3']]) {
  const p=JSON.parse(readFileSync(`${root}/internal/mosaicevent/${record}`,'utf8'));
  const n=version.split('/')[1];
  assert.equal(p.schemaVersion,version);
  assert.equal(p.repository,'https://forge.hive.mosaic-finance.com/tunc/mosaic.git');
  assert.match(p.sourceCommit,/^[0-9a-f]{40}$/);
  assert.equal(p.bundlePath,`reference/schemas/mosaic-event/${n}`);
  assert.equal(p.vendoredPath,`internal/mosaicevent/vendor/${n}`);
  const manifest=readFileSync(`${root}/${p.vendoredPath}/manifest.json`);
  assert.equal(createHash('sha256').update(manifest).digest('hex'),p.bundleDigest);
  assert.equal(readFileSync(`${root}/${p.vendoredPath}/manifest.sha256`,'utf8').trim(),p.bundleDigest);
  assert.equal(JSON.parse(manifest).schemaVersion,version);
}
JS
npm --prefix "$bundle/contract" ci --ignore-scripts --no-audit --no-fund
npm --prefix "$bundle/contract" run typecheck
npm --prefix "$bundle/contract" test
go build -o "$work/mosaic-event-contract" ./cmd/mosaic-event-contract
npm --prefix "$bundle/contract" run check -- "$work/mosaic-event-contract"
node "$root/internal/mosaicevent/vendor/2/stamp.mjs" --check
node "$root/internal/mosaicevent/vendor/3/stamp.mjs" --check
go test ./internal/mosaicevent/ -count=1 -run 'TestCorpusPerVersion|TestV2DriftAndCrossVersionBundles|TestV3DriftAndCrossVersionBundles|TestSetDispatchesOnSchemaVersion'
