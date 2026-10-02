# Mosaic event contract, version 1

`mosaic-event/1` defines a closed catalogue of AI SDK UI message chunks and Mosaic data parts.
Every start requires `messageMetadata.schemaVersion`. The relay envelope requires the same
version, origin, sequence, timestamp and correlation identity; its type must match payload.type.
Only origin `relay` may carry `approval_resolved` with a reserved `resolved:` correlation id.
The validators also require that id to equal `resolved:` plus the payload correlationId.
That payload retains the 78.4 observed `{correlationId, decision: allow|deny, reason: user|timeout}`.
Timeout resolutions must deny; user resolutions may allow or deny.
Origin is a structural claim. Story 78.14 must derive it from authenticated socket identity.

The supported catalogue excludes `custom` and `reset-step`: Grafana v0.1.0-alpha.1 cannot decode
them. Both validators reject them. This contract makes no claim of complete ai@7 union coverage.
Unknown canonical names/properties and invalid extension payloads reject. Opaque tool input/output,
provider metadata and application message metadata remain JSON. Keep raw bytes with Go typed chunks
because SDK reserialization omits optional fields, including approvalDescriptor/inputSchemaInput.
`approved:false` is a denial and is never omitted. Schemas use draft-07, bundle-only references,
no format assertions, no coercion/default injection, finite JSON numbers throughout opaque payloads,
and common JavaScript safe integer limits.

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
Injected start/finish chunks and schemaVersion metadata are normalization values, not stdout fields.
Existing compatible start metadata is preserved; a present incompatible schemaVersion rejects.
Claude inputTokens adds uncached, cache-read and cache-creation tokens, while cachedInputTokens
means cache reads only. A Claude modelUsage key supplies the model when present; otherwise
`claude-captured` is a synthetic fallback. Codex usage notifications lack a model field, so
`codex-captured` is always a synthetic normalization label. The synthetic failed MCP fixture
models a permitted protocol state with null result/error; its fallback errorText is injected.
Tool-reported MCP failures can instead carry result diagnostics with no separate error. Their
result content is retained in errorText; explicit error diagnostics are retained too. Only the
absence of both diagnostics uses the synthetic fallback. These failure regressions are synthetic.
None of these normalization values claim additional fields captured from the agent's stdout.

The locked `contract/` package validates with Ajv and the public ai UI chunk schema. Its CLI compares
every expected verdict and event count against the Go schema validator and actual Grafana decoder.
Missing/empty reports, crashes, disagreement and even an agreed incorrect answer fail. Both CI
entrypoints run identical locked TS code/corpus plus Go, negative controls and manifest verification.
Invalid corpus kind metadata fails setup before event evaluation; a routing typo cannot count as
an expected negative verdict. Static negatives independently omit every required data payload field,
including each ACP plan entry field. Literal overflow fixtures keep `1e400` as JSON text.
An optional root passed to report.ts supplies schemas as well as manifest, provenance and fixtures.
Each report compiles that root's local schemas in a separate Ajv instance; exported validate uses
the canonical bundle. Supplied schemas cannot reuse a different bundle's compiled schema cache.

From contract/: `npm ci --ignore-scripts`, `npm run typecheck`, `npm test`, then
`npm run check -- /absolute/path/to/mosaic-event-contract`. Dependency retrieval precedes offline
validation. `npx tsx stamp.ts` intentionally refreshes the sorted byte manifest after reviewed edits;
CI never regenerates expected outputs or stamps. The manifest covers schemas, fixtures, provenance,
this documentation, validator, tests, dependency lock and project `.npmrc`. Its own bytes have a
separate SHA-256. The `.npmrc` enforces a three-day release age during new npm resolution, with no
exemptions or absolute cutoff. Frozen `npm ci` installs the reviewed lock; it does not audit the
release age of already locked versions.

Renovate excludes `reference/schemas/mosaic-event/**` before package extraction for every version.
This prevents partial bot changes from ordinary updates, lock maintenance or OSV security force;
an ordinary package hold can be overridden by that force. The contract lock remains in the
blocking nightly, PR and delivery security scans, with the existing malicious/critical thresholds
and reviewed acceptance rules. A detected vulnerability requires explicit reviewed canonical
dependency/lock changes and the digest/vendor/source-pin reconciliation below, or a new version.

Go vendors this exact version byte for byte. Refresh from an actual committed Mosaic bundle, then
record that full canonical source commit and manifest digest in Go vendor-source.json. Commit Go
with hooks, then pin its full commit in Mosaic's CI source file. Root must push that Go commit
before Mosaic CI preparation can retrieve it. Local sibling validation checks pinned source and
bundle identity. Go CI can verify its snapshot offline; it cannot discover later private Mosaic
changes. Root retains exact-head source CI and real-PR control evidence in both repositories before
claiming delivered acceptance. Changing a contract requires an explicit reviewed bundle reconciliation
or a new version; it is never silently accepted through mutable main or a stale vendor snapshot.
