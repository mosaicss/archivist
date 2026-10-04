# Mosaic event contract, version 3

`mosaic-event/3` is `mosaic-event/2` plus two additive changes for the 78.32 agent session
controls. Versions 1 and 2 are frozen: their bundles, digests and verdicts do not change, and
every v2 rule in its README (and every v1 rule it keeps) holds here unless this file names a
change.

## What changed from v2

| Where | v2 | v3 |
|---|---|---|
| `data-usage` | `{inputTokens, outputTokens, reasoningTokens?, cachedInputTokens?, model}` | adds optional `contextTokens` and `contextWindow` (safe integers >= 0): the context the harness reports in use and the model's context window. Absent means unknown, never a guess |
| `data-session-controls` | (none) | new data part `{mode, maxMode, model?, effort?}`: the session's actual permission mode, the machine's ceiling, and the session's model and effort when one was chosen |
| `schemaVersion` consts | `mosaic-event/2` | `mosaic-event/3` in the envelope and in the `start` chunk's `messageMetadata` |

`data-session-controls` fields:

- `mode` and `maxMode`: one of `read_only`, `ask`, `auto_edits`, `full_auto` (low to high; the
  archivist daemon's closed set). The daemon always sends `mode` at or below `maxMode`; the schema
  checks each value, not their order.
- `model`: `^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$` (the daemon's `modelIDRe`), for example
  `opus` or `claude-opus-5-5[1m]`.
- `effort`: `^[a-z][a-z0-9_-]{0,31}$` (the daemon's `effortRe`), for example `high`.

The part is wired into `chunk.json` and into the envelope for the `daemon` and `chat-api`
origins, exactly like `data-usage`; the relay origin keeps the v2 rules (`approval_resolved` and
`data-session-status` only). Every schema `$id` sits under `/schemas/mosaic-event/3/`.

## Versioning rule

A version is immutable once a consumer pins its digest. A contract change is a new directory
`reference/schemas/mosaic-event/<n>/`, never an edit of a pinned one. Validators dispatch on the
envelope's (or the `start` chunk's) `schemaVersion`: `mosaic-event/1` goes to the v1 bundle,
`mosaic-event/2` to v2, `mosaic-event/3` to this one, and anything else is invalid. A v3 envelope
must carry a v3 payload: a `start` chunk inside it must say `mosaic-event/3`, and a v2 envelope
carrying `data-session-controls` or the context fields stays invalid.

## Who stamps what

- **Archivist daemon** (the 78.32 release, which vendors this bundle as Go
  `internal/mosaicevent/vendor/3`): stamps v3 only on `data-usage` that carries `contextTokens`
  or `contextWindow` (each present only when the harness reported it; a usage report without
  either stays v1) and on `data-session-controls` (while a session is running: when it starts
  running and after every `set_mode` outcome). `data-auth-prompt` stays v2 and every other event
  v1, so the rest of a turn reads the same to older relays and consumers.
- **Relay** (`infrastructure/cloudflare/workers/agent-relay`): validates v1, v2 and v3 from one
  generated validator with three pinned digests. It stamps no v3 event itself.
- **chat-api** stays on v1.

A relay that predates v3 (the deployed relay runs the v2 validator) drops v3 envelopes as
invalid, so every turn's usage report would be lost: the relay deploy with the v3 validator comes
before any archivist release that stamps v3.

## Bundle contents

- The schemas: `chunk.json`, `envelope.json` and one file per data part, as in v2, plus
  `data-session-controls.json`.
- `cases.json`: every v2 case rewritten to v3 (same ids, kinds, verdicts and sources), the
  wrong-version probes now using `mosaic-event/4` (including `invalid-v2-envelope-wrong-version-v3`,
  whose id is kept), the `-v1` copies unchanged, new `-v2` copies using `mosaic-event/2`, and
  synthetic valid and invalid cases for both additions (ids `*-v3-*`). The five
  `invalid-opaque-overflow-*` cases are refused by the consumers' finite-JSON check, not by the
  schemas, exactly as in v1 and v2.
- `fixtures/`: one JSON array per case.
- `provenance.json`: the v2 manifest digest this bundle derives from, the rewrite rules, the
  additions, and each derived file's v2 SHA-256.
- `stamp.mjs`, `manifest.json`, `manifest.sha256`.

As in v2 there is no `contract/` npm package and no lockfile: the relay's corpus test (Ajv, in
workerd) runs this `cases.json` against its own validator, and the archivist Go parser tests run
it from `vendor/3`. Renovate's `reference/schemas/mosaic-event/**` exclusion covers this
directory.

## Checking and stamping

```sh
node reference/schemas/mosaic-event/3/stamp.mjs --check   # verify only; exit 1 on drift
node reference/schemas/mosaic-event/3/stamp.mjs           # after a reviewed edit only
```

The manifest lists every file except itself and `manifest.sha256`, sorted, with its SHA-256;
`manifest.sha256` holds the manifest's own digest, which consumers pin.
