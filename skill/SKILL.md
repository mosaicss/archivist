---
name: archivist
description: Research SEC and SEDAR public company filings with the Archivist CLI. Use when the user invokes /archivist or asks to research filings, financials, risk factors, or build multi-company comparison tables from the terminal. Requires the archivist binary on PATH and an authenticated Mosaic account.
---
<!-- version: 0.0.0 -->
# Archivist CLI — Claude Code Skill

Use `archivist` to drive Mosaic filings research from within Claude Code.
This skill documents the verbs, exit codes, and agent patterns for the Archivist CLI.

## Resolve first, then research

Always resolve a company name to its canonical `issuer_key` before running
`archivist chat` or `archivist table`. Ambiguous names return exit 6; a resolved
key returns exit 0 every time.

```sh
# Step 1: resolve
archivist companies search "Shopify"
# Output includes issuer_key, e.g. cik:1594805

# Step 2: use the key
archivist chat --company cik:1594805 "What was the revenue growth driver in Q3 2024?"
```

**An `issuer_key` is always `prefix:value`. It is never a slug.** There are four
prefixes, one per identifier authority:

| Prefix | Authority | Example |
|--------|-----------|---------|
| `cik:` | SEC Central Index Key — US filers and cross-listed Canadians | `cik:1594805` |
| `sedar:` | SEDAR — Canadian-only filers | `sedar:000051994` |
| `uuid:` | issuers with neither a CIK nor a SEDAR id (CDRs, funds) | `uuid:36947d0a-6e51-4cdd-8dc5-48846b07cae6` |
| `mkk:` | MKK — Borsa Istanbul filers | `mkk:4028e4a1416e696301416f37201c5f2e` |

Anything that is not one of these forms is treated as a **search term**, not a
key: it goes to the fuzzy company-search path and can silently resolve to the
wrong issuer. Never invent a key from a company name.

> **`mkk:` keys do not yet bypass resolution.** The CLI's literal-key check
> recognizes only `cik:`, `sedar:` and `uuid:`, so `--company mkk:...` falls
> through to fuzzy search and returns not-found. Turkish issuers currently have
> no canonical-key escape hatch from ambiguity — resolve them by name via
> `archivist companies search` and expect exit 6 on ambiguous ones. Tracked as a
> known gap; this note comes out when the `mkk:` rung ships.

## Chat vs table

Use `archivist chat` for free-form research questions about a single company.
Use `archivist table` for structured multi-company or multi-metric comparisons.

```sh
# Chat: one company, open-ended question
archivist chat --company cik:1594805 "Describe the main risk factors."

# Table: multiple companies, structured spec
archivist table --spec myspec.yaml
```

## Cascade rules summary

These rules are enforced at the CLI level (exit 8 on violation):

- Custom rows (entities you define) only work with web-sourced columns. A custom
  row combined with a filings column is a cascade violation.
- Selecting a company locks the country and exchange for that row. You cannot
  override country after the company is resolved.
- Country is a projection of the company exchange (e.g., TSX => CA).
- Sector and industry columns are available only for filings rows, not custom entities.

## When to use --stream

Use `--stream` for interactive terminal output where you want to see results as
they arrive. Omit `--stream` (the default blocking mode) when you are piping
output to another tool, writing to a file, or running inside an agent loop.

```sh
# Interactive: streaming output
archivist chat --company cik:1594805 --stream "What are the key risks?"

# Piped: blocking, output is complete before the pipe runs
archivist chat --company cik:1594805 "Revenue?" | jq .
```

## Citation interpretation

Citations in `archivist chat` output appear as `[1]`, `[2]`, etc. A citations
block at the end of the response maps each number to a numeric filing ID (e.g.
`2054838`) plus its form type. Filing IDs are integers, not slugs. Use
`archivist companies get <issuer_key>` to retrieve filing metadata if you need
the full document URL.

## Exit codes

Agents should branch on exit codes, not parse output text:

| Code | Meaning | Recommended action |
|------|---------|-------------------|
| 0 | Success | Continue |
| 1 | Generic error | Log and surface to user |
| 2 | Usage error (bad flags or spec) | Fix the invocation |
| 3 | Not found (company, file, session) | Try a different search term |
| 4 | Auth error (missing or expired token) | Run `archivist auth login --token ak_...` (or set `ARCHIVIST_TOKEN`) |
| 5 | Server error or CLI version too old | Run `archivist update` then retry |
| 6 | Ambiguous company match | Re-run with `--company <issuer_key>` |
| 7 | Rate limit or quota exhausted | Wait or upgrade tier |
| 8 | Cascade violation | Fix the table spec |
| 9 | Not implemented (stub) | Do not retry; use a newer binary |

## Available verbs

```
archivist auth      Manage credentials (auth login saves ~/.archivist/credentials)
archivist chat      Run a research question
archivist table     Build or rerun a research table
archivist companies Search or fetch issuer records
archivist usage     Report quota and rate limit consumption
archivist update    Upgrade the binary in place
archivist version   Print binary version and platform
archivist doctor    Diagnose connectivity and auth
archivist explain   Show default-window and cascade rules
```

## Self-update

Keep the binary current to avoid server-enforced version blocks (exit 5):

```sh
archivist update          # self-replace (curl-sh / github-releases channel)
archivist update --skill  # refresh only the Claude Code skill files
archivist update --check  # check whether an update is available without installing
```

For brew-installed binaries: `brew upgrade mosaic-finance-inc/tap/archivist`.
For npm-installed binaries: `npx -y @mosaic-finance/archivist@latest install`.
