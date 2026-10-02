# Mosaic event contract, version 1

`mosaic-event/1` defines a closed catalogue of AI SDK UI message chunks and Mosaic data parts.
Every start requires `messageMetadata.schemaVersion`. The relay envelope requires the same
version, origin, sequence, timestamp and correlation identity; its type must match payload.type.
Only origin `relay` may carry `approval_resolved` with a reserved `resolved:` correlation id.
The validators also require that id to equal `resolved:` plus the payload correlationId.
That payload retains the 78.4 observed `{correlationId, decision: allow|deny, reason: user|timeout}`.
Origin is a structural claim. Story 78.14 must derive it from authenticated socket identity.

The supported catalogue excludes `custom` and `reset-step`: Grafana v0.1.0-alpha.1 cannot decode
them. Both validators reject them. This contract makes no claim of complete ai@7 union coverage.
Unknown canonical names/properties and invalid extension payloads reject. Opaque tool input/output,
provider metadata and application message metadata remain JSON. Keep raw bytes with Go typed chunks
because SDK reserialization omits optional fields, including approvalDescriptor/inputSchemaInput.
`approved:false` is a denial and is never omitted. Schemas use draft-07, bundle-only references,
no format assertions, no coercion/default injection, and common JavaScript safe integer limits.

The nine reserved extensions describe Mosaic payloads, not promised harness capabilities:

| Part | Payload |
|---|---|
| data-plan | ACP entries with content, priority high/medium/low, status pending/in_progress/completed and optional _meta |
| data-permission-scope | approvalId, scope allow_once/allow_always/reject_once/reject_always, optional toolCallId |
| data-exec-output | toolCallId, stdout/stderr stream, text, optional exitCode and truncated |
| data-patch | toolCallId, path, diff, operation add/update/delete |
| data-task | taskId, status pending/running/completed/failed/cancelled, optional title and parentTaskId |
| data-usage | inputTokens, outputTokens, model, optional reasoningTokens and cachedInputTokens |
| data-auth-prompt | promptId, provider claude/codex, message, optional url |
| data-session-status | sessionId, status starting/ready/running/completed/failed/interrupted/disconnected, optional message |
| data-artifact | artifactId, name, mediaType, url and lowercase SHA-256 digest |

Existing chat parts are data-status, data-conversation-id, data-teaser-tail, data-teaser-gate and
data-anon-turn. Nullable remainingChars is intentional. Each payload has its own schema file.

`provenance.json` lists retained source versions, original hashes, extracts, ignored notices and
normalization rules. Raw captured source and checked normalized goldens remain separate. SSE bodies
are stored as JSON strings so required frame terminators survive mandatory EOF hooks. Legacy
chat-api lacked start/finish; normalization injects them explicitly and stamps versions. Current
route/producer tests exercise workspace usage and anonymous parts without paid calls. No runtime
stream change is implied. Synthetic coverage is labelled in cases.json and includes every variant,
required field failures, wrong versions, relay spoofing, invalid enums and unsupported SDK parts.

The locked `contract/` package validates with Ajv and the public ai UI chunk schema. Its CLI compares
every expected verdict and event count against the Go schema validator and actual Grafana decoder.
Missing/empty reports, crashes, disagreement and even an agreed incorrect answer fail. Both CI
entrypoints run identical locked TS code/corpus plus Go, negative controls and manifest verification.

From contract/: `npm ci --ignore-scripts`, `npm run typecheck`, `npm test`, then
`npm run check -- /absolute/path/to/mosaic-event-contract`. Dependency retrieval precedes offline
validation. `npx tsx stamp.ts` intentionally refreshes the sorted byte manifest after reviewed edits;
CI never regenerates expected outputs or stamps. The manifest covers schemas, fixtures, provenance,
this documentation, validator, tests and dependency lock. Its own bytes have a separate SHA-256.

Go vendors this exact version byte for byte. Refresh from an actual committed Mosaic bundle, then
record that full canonical source commit and manifest digest in Go vendor-source.json. Commit Go
with hooks, then pin its full commit in Mosaic's CI source file. Root must push that Go commit
before Mosaic CI preparation can retrieve it. Local sibling validation checks pinned source and
bundle identity. Go CI can verify its snapshot offline; it cannot discover later private Mosaic
changes. Root retains exact-head source CI and real-PR control evidence in both repositories before
claiming delivered acceptance. Changing a contract requires an explicit reviewed bundle reconciliation
or a new version; it is never silently accepted through mutable main or a stale vendor snapshot.
