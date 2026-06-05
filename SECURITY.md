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
