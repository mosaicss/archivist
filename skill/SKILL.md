---
name: archivist
description: Research SEC and SEDAR public company filings with the Archivist CLI. Use when the user invokes /archivist or asks to research filings, financials, risk factors, or management commentary from the terminal and wants answers cited to the source passage. Requires the archivist binary on PATH and an authenticated Mosaic Pro account.
---
<!-- version: 0.0.0 -->
# Archivist CLI: Claude Code skill

`archivist` searches and reads SEC and SEDAR filings. It returns passages, not
answers: you read the passages, write the answer, and cite each claim with the
passage's permalink `url`. Mosaic runs no model for the CLI.

## The research loop

1. **Find the company symbol** (skip when the user already gave one):

   ```sh
   archivist companies search "Shopify" --format json
   ```

   Take `symbol` from the best result, for example `SHOP:US`.

2. **Search for passages:**

   ```sh
   archivist search "revenue growth drivers" --symbol SHOP:US --formtype 10-K --format json
   ```

   Each result is a passage record: `id` (the chunk id), `filing_id`,
   `company_name`, `symbol`, `formtype`, `datefiled`, `section_header`,
   `chunk_index`, `snippet` (the full passage text) and `url`.

3. **Read around a hit** when a snippet is not enough:

   ```sh
   archivist read passage <chunk_id> --window 2 --format json     # the passage and two neighbours on each side
   archivist toc <filing_id> --format json                        # the filing's section headers
   archivist read section <filing_id> "Item 7. Management's Discussion" --format json
   ```

4. **Answer with citations.** Cite the `url` of every passage you rely on. The
   link opens that passage in Mosaic's filing viewer. When `url` is null, cite
   the company, form, filing date and section instead.

## Search options

| Flag | Meaning |
|------|---------|
| `--symbol` | One company, e.g. `AAPL:US` (at most 20 characters) |
| `--formtype` | One form type, e.g. `10-K`, `40-F`, `Annual information form` |
| `--date-from`, `--date-to` | Filing date range, `YYYY-MM-DD` |
| `--mode` | `semantic` (default, takes filters) or `broad` (no filters) |
| `--limit` | 1 to 25 passages, default 10 |
| `--cursor` | Continue a truncated response |

Bad input fails with exit 2 before any request is sent.

## Output

Pass `--format json` from an agent. Off a terminal JSON is already the
default, and it is the server response unchanged. On a terminal the default is
a table with a `URL` column. Diagnostics (warnings, the truncation hint, error
text) go to stderr; stdout carries only content.

When a response is truncated, stderr says `More results: rerun with --cursor
<token>`. Rerun the same command with that `--cursor` to get the rest.

A search with no results exits 3 and prints the server's suggestion. When a
bare symbol matches several issuers it exits 6 with a "Did you mean"
suggestion: rerun with the full `TICKER:EXCHANGE` symbol from `companies search`.

## Account and fair use

Agent access needs a Mosaic Pro account. A free account gets exit 4 with the
plans page, https://mosaic-finance.com/en/pricing/. Every research call counts
toward a monthly fair use limit; when it is reached, calls exit 7 and stderr
names the reset date. `archivist usage` shows this month's count.

Chat and tables are not CLI features: there is no `chat` or `table` verb. They
run only in the Mosaic web app.

## Exit codes

Branch on exit codes, not on output text:

| Code | Meaning | Recommended action |
|------|---------|-------------------|
| 0 | Success | Continue |
| 1 | Generic error | Surface to the user |
| 2 | Usage error: bad flag, id or argument | Fix the invocation |
| 3 | Not found: no passages, unknown id or company | Broaden the query or filters |
| 4 | Auth error: missing or bad credential, or no Pro account | Run `archivist auth status`; see the plans page |
| 5 | Server error, or this CLI version is too old | Run `archivist update`, then retry |
| 6 | A search symbol matched several issuers | Rerun with the full `TICKER:EXCHANGE` symbol |
| 7 | Rate limit or monthly fair use limit reached | Wait; check `archivist usage` |
| 8 | Reserved | Not emitted |
| 9 | Not implemented | Do not retry |

With `--format json` a failure also prints `{"error": <code>, "message",
"exit_code", ...}` on stdout, plus `suggestion`, `account_url` or `reset_date`
when the server sent them.

## Verbs

```
archivist auth          Manage credentials (auth login saves ~/.archivist/credentials)
archivist search        Search filing passages
archivist read passage  Read a passage with its neighbours
archivist read section  Read a whole filing section
archivist toc           List a filing's section headers
archivist companies     Find a company and its symbol
archivist doctor        Diagnose credentials, connectivity and version
archivist usage         Report monthly fair use and rate limit consumption
archivist update        Upgrade the binary and this skill
archivist version       Print binary version and platform
```

`archivist mcp serve` exposes the same research verbs as MCP tools over stdio.

## Self-update

Keep the binary current; an old binary exits 5 with "Run 'archivist update'":

```sh
archivist update          # replace the binary (GitHub release install)
archivist update --skill  # refresh only this skill
archivist update --check  # check without installing
```

For Homebrew installs: `brew upgrade mosaic-finance-inc/tap/archivist`.
