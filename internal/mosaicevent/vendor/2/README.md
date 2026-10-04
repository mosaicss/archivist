# Mosaic event contract, version 2

`mosaic-event/2` is `mosaic-event/1` plus three additive changes for the 78.22 cloud sandboxes.
Version 1 is frozen: its bundle, digest and verdicts do not change, and every v1 rule in its
README holds here unless this file names a change.

## What changed from v1

| Where | v1 | v2 |
|---|---|---|
| `data-auth-prompt` | `{promptId, provider, message, url?}` | adds optional `code` (string, 1..64 characters from `!` to `~`: printable ASCII, no whitespace) and `expiresAt` (safe integer, epoch milliseconds, like the envelope `ts`) |
| `data-session-status.status` | `starting`, `ready`, `running`, `completed`, `failed`, `interrupted`, `disconnected` | adds `lost`: the sandbox running the session disappeared and is never restarted |
| Envelope origin `relay` | only `approval_resolved` | also `data-session-status` (correlation id nonempty and not `resolved:`) |
| `schemaVersion` consts | `mosaic-event/1` | `mosaic-event/2` in the envelope and in the `start` chunk's `messageMetadata` |

Daemon and chat-api origins keep the v1 rules. `message` stays required on `data-auth-prompt`
and keeps the full human text (including the code and expiry), so a v1 renderer that ignores the
new fields still works. Every schema `$id` sits under `/schemas/mosaic-event/2/`.

## Versioning rule

A version is immutable once a consumer pins its digest. A contract change is a new directory
`reference/schemas/mosaic-event/<n>/`, never an edit of a pinned one. Validators dispatch on the
envelope's (or the `start` chunk's) `schemaVersion`: `mosaic-event/1` goes to the v1 bundle,
`mosaic-event/2` to this one, and anything else is invalid. A v2 envelope must carry a v2
payload: a `start` chunk inside it must say `mosaic-event/2`.

## Who stamps what

- **Archivist daemon** (the Go `internal/mosaicevent` vendor of this bundle): stamps
  `mosaic-event/2` only on `data-auth-prompt` envelopes, which carry `code` (the Codex device
  code) and `expiresAt` (the daemon's sign-in deadline) when it has them. Every other event stays
  `mosaic-event/1`, so laptop daemons and older relays see no change in normal turns.
- **Relay** (`infrastructure/cloudflare/workers/agent-relay`): validates both versions from one
  generated validator with two pinned digests. It stamps one relay-origin v2 event itself:
  `data-session-status` with status `lost`, correlation id `lost:<sessionId>`, appended at most
  once per session through the workspace consumer route `POST /sessions/:uuid/lost`.
- **chat-api** stays on v1 (its wire test keeps refusing v2 through the v1 contract).

A relay that predates v2 drops v2 envelopes as invalid, so the relay deploy comes before an
archivist release that stamps v2.

## Bundle contents

- The schemas: `chunk.json`, `envelope.json` and one file per data part, as in v1.
- `cases.json`: every v1 case rewritten to v2 (same ids, kinds, verdicts and sources), the two
  wrong-version probes now using `mosaic-event/3` plus `-v1` copies using `mosaic-event/1`, and
  synthetic valid and invalid cases for every addition (ids `*-v2-*`). The five
  `invalid-opaque-overflow-*` cases are refused by the consumers' finite-JSON check, not by the
  schemas, exactly as in v1.
- `fixtures/`: one JSON array per case.
- `provenance.json`: the v1 manifest digest this bundle derives from, the rewrite rules, the
  additions, and each derived file's v1 SHA-256. Capture provenance stays in v1.
- `stamp.mjs`, `manifest.json`, `manifest.sha256`.

There is no `contract/` npm package and no lockfile: the relay's corpus test (Ajv, in workerd)
and the archivist Go parser tests run this `cases.json` against their own validators. Renovate's
`reference/schemas/mosaic-event/**` exclusion covers this directory.

## Checking and stamping

```sh
node reference/schemas/mosaic-event/2/stamp.mjs --check   # verify only; exit 1 on drift
node reference/schemas/mosaic-event/2/stamp.mjs           # after a reviewed edit only
```

The manifest lists every file except itself and `manifest.sha256`, sorted, with its SHA-256;
`manifest.sha256` holds the manifest's own digest, which consumers pin. Go vendors this bundle
byte for byte and records the Mosaic source commit and this digest beside it.
