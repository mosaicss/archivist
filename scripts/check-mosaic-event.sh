#!/usr/bin/env bash
# Candidate Go parser + exact vendored canonical TS package/corpus. No private Mosaic access.
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
const p=JSON.parse(readFileSync(`${root}/internal/mosaicevent/vendor-source.json`,'utf8'));
assert.equal(p.schemaVersion,'mosaic-event/1');
assert.equal(p.repository,'https://forge.hive.mosaic-finance.com/tunc/mosaic.git');
assert.match(p.sourceCommit,/^[0-9a-f]{40}$/);
assert.equal(createHash('sha256').update(readFileSync(`${root}/internal/mosaicevent/vendor/1/manifest.json`)).digest('hex'),p.bundleDigest);
JS
npm --prefix "$bundle/contract" ci --ignore-scripts --no-audit --no-fund
npm --prefix "$bundle/contract" run typecheck
npm --prefix "$bundle/contract" test
go build -o "$work/mosaic-event-contract" ./cmd/mosaic-event-contract
npm --prefix "$bundle/contract" run check -- "$work/mosaic-event-contract"
