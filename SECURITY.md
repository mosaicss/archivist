# Security Policy

## Reporting a Vulnerability

Email **security@mosaic-finance.com** with:

- A description of the issue and where it lives (command, endpoint, package, or file).
- Reproduction steps or a proof of concept if you have one.
- Any impact assessment you can offer (what an attacker gains).

Please do not open public GitHub issues for security reports.

## Response SLA

| Stage | Commitment |
|---|---|
| Acknowledge receipt | within 48 hours |
| Triage verdict (real, duplicate, or not applicable) | within 7 days |
| Fix shipped or risk formally accepted | within 30 days |

## Supported Versions

| Version | Supported |
|---|---|
| Latest 0.2.x release | Yes |
| Older releases | No. Upgrade via `archivist update` or your install channel. |

## Scope

This policy covers the Archivist CLI binary, its install scripts, and the distribution channels in this repository (curl installer, npm package, Homebrew tap, GitHub Releases, `go install`). Reports about the Mosaic web platform and its APIs are welcome at the same address.

A note on tokens: Archivist API keys (`ak_` prefix) are stored at `~/.archivist/credentials` with permissions 0600. If you believe a token has leaked, revoke it immediately from your account settings at mosaic-finance.com (Manage account, API keys) and mint a fresh one.

`archivist connect` (preview) also keeps short-lived session task tokens (`mst_` prefix, at most 15 minutes, search and read scopes only) on disk while a session runs: `~/.archivist/connect/run/<session>/task-token` and the `mcp.json` that names it, both 0600 in a 0700 directory. They are removed when the session stops or the daemon exits, and the daemon revokes each task token at that point (and when it rotates one). Session records in `~/.archivist/connect/sessions/` hold no tokens. In a connected session, "Allow for session" approves (and "Deny for session" refuses) every later call of that tool, any input, for the rest of that session without another prompt; these rules are kept in the daemon's memory only, so restarting `archivist connect` clears them.
