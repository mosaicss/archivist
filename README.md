# Archivist CLI

Mosaic's command line surface for filings research. Any AI agent (Claude Code, Cursor, custom orchestrators) or shell context (bash, cron, CI) can search and read SEC and SEDAR filings with it. Every passage it returns carries a permalink that opens the passage in Mosaic's filing viewer, so an answer can cite its source.

The CLI returns passages, not answers: Mosaic runs no model for it. Chat and tables run only in the Mosaic web app.

## Install

### Homebrew (macOS)

```sh
brew install mosaic-finance-inc/tap/archivist
```

Installs the universal macOS binary as a Homebrew cask and places the Claude
Code skill in `~/.claude/skills/archivist/`.

### GitHub Releases (macOS, Linux, Windows)

Download the archive for your platform and the checksum file from
https://github.com/mosaicss/archivist/releases, verify, and put the binary on
your PATH. For example, Linux amd64:

```sh
VER=0.2.22
curl -fsSLO https://github.com/mosaicss/archivist/releases/download/v${VER}/archivist_v${VER}_linux_amd64.tar.gz
curl -fsSLO https://github.com/mosaicss/archivist/releases/download/v${VER}/archivist_v${VER}_SHA256SUMS
grep "archivist_v${VER}_linux_amd64.tar.gz" archivist_v${VER}_SHA256SUMS | sha256sum -c -
tar -xzf archivist_v${VER}_linux_amd64.tar.gz archivist
install -m 0755 archivist ~/.local/bin/archivist
```

Then add the Claude Code skill:

```sh
archivist update --skill
```

### go install

```sh
go install github.com/mosaicss/archivist/cmd/archivist@latest
archivist update --skill   # optional: the Claude Code skill
```

## Authenticating

Agent access is part of a Mosaic Pro account (plans:
https://mosaic-finance.com/en/pricing/). Signed in on Mosaic, open your avatar
menu, then Manage account, then API keys, and add a key. Copy the `ak_...`
token and save it once:

```sh
archivist auth login --token ak_<your-token>
```

The CLI verifies the token against the server and writes it to
`~/.archivist/credentials` (mode 0600). `archivist auth status` shows which
credential is active and where it came from; `archivist auth logout` deletes
the saved file.

For CI and scripting, `ARCHIVIST_TOKEN=ak_...` overrides the saved file, and
`--token ak_...` overrides both for one call.

## Quickstart

```sh
# 1. Find a company's symbol
archivist companies search "Shopify"

# 2. Search its filings
archivist search "revenue growth drivers" --symbol SHOP:US --formtype 10-K

# 3. Read around a hit (ids come from the search results)
archivist read passage <chunk_id> --window 2
archivist toc <filing_id>
archivist read section <filing_id> "Item 7. Management's Discussion and Analysis"
```

On a terminal, `search` prints a table:

```text
CHUNK_ID  FILING_ID  SYMBOL   FORM  DATE        SECTION                SNIPPET                               URL
5f0c…     a9d2…      SHOP:US  10-K  2026-02-12  Item 7. Management's…  Revenue grew 26% driven by growth in…  https://mosaic-finance.com/filings/a9d2…/?c=5f0c…&t=…
```

Piped or redirected, every verb prints JSON instead: the server response,
indented, keys and values unchanged. Pass `--format json` or `--format table`
to choose. Diagnostics go to stderr, so stdout stays clean for `jq`.

When a response is too large it is truncated and stderr says
`More results: rerun with --cursor <token>`.

## Verbs

| Verb | What it does |
|------|--------------|
| `search <query>` | Search passages. `--symbol`, `--formtype`, `--date-from`, `--date-to`, `--mode semantic\|broad`, `--limit 1-25`, `--cursor` |
| `read passage <chunk_id>` | A passage with up to `--window` (0 to 2, default 1) neighbours on each side |
| `read section <filing_id> <section_header>` | Every passage of one section, in order |
| `toc <filing_id>` | A filing's section headers |
| `companies search <query>` | Find a company and its symbol |
| `companies get <issuer_key>` | One company's details |
| `auth login\|status\|whoami\|logout` | Manage the credential |
| `usage` | This month's fair use count and the rate limit |
| `doctor` | Check credential, connectivity, version and skill |
| `update` | Replace the binary with the latest release (`--skill`, `--check`) |
| `version` | Print version, commit, build date and platform |

```text
$ archivist version
archivist-cli 0.2.22 (commit abc1234 built 2026-10-01) linux/amd64
```

## Fair use and errors

Every research call counts toward a monthly fair use limit. When it is
reached, calls exit 7 and stderr names the reset date; `archivist usage` shows
the count. A free account exits 4 with the plans page.

With `--format json`, a failure also prints
`{"error": <code>, "message", "exit_code"}` on stdout, plus `suggestion`,
`account_url` or `reset_date` when the server sent them.

## Exit codes

Agents should branch on exit codes, not parse output text:

| Code | Meaning |
| ---- | ------- |
| 0    | success |
| 1    | generic error |
| 2    | usage error (bad flag, id or argument; unknown command) |
| 3    | not found (no passages; unknown id or company) |
| 4    | auth error (missing or invalid credential; no Pro account) |
| 5    | server error (5xx after retries; network failure; minimum CLI version block) |
| 6    | ambiguous match (a search symbol matched several issuers) |
| 7    | rate limit or monthly fair use limit reached |
| 8    | reserved, not emitted |
| 9    | not implemented (do not retry) |

An old binary exits 5 with "Run 'archivist update' to upgrade."

## MCP server

`archivist mcp serve` runs the binary as an MCP server over stdio. Each
research verb becomes a read only MCP tool with the same credential, exit
codes and JSON, so any MCP host (Claude Desktop, Cursor, custom agents) can
search filings without shell access.

Claude Desktop config (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "archivist": {
      "command": "archivist",
      "args": ["mcp", "serve"],
      "env": { "ARCHIVIST_TOKEN": "ak_..." }
    }
  }
}
```

The `env` block is optional once `archivist auth login` has saved a
credential.

Tools (11): `search`, `read_passage`, `read_section`, `toc`,
`companies_search`, `companies_get`, `auth_status`, `auth_whoami`, `usage`,
`doctor`, `version`. Tool names join the verb path with underscores. Every
tool has a title and the annotations `readOnlyHint: true`,
`destructiveHint: false`, `openWorldHint: false`.

`auth login`, `auth logout` and `update` are not exposed: token setup and
binary replacement are operator actions. A failed call returns an error result
naming the exit code, with the verb's stderr and stdout.

## Claude Code skill

The skill in `skill/` teaches Claude Code the research loop: search, read,
cite the permalink. Homebrew installs it; elsewhere run
`archivist update --skill`. Plugin manifest: `.claude-plugin/plugin.json`.

## Local development

```sh
git clone https://github.com/mosaicss/archivist.git
cd archivist
go test ./... -race
go build ./cmd/archivist
./archivist version
```

`go.mod` pins the toolchain (`toolchain go1.27.1`); CI and releases build with
exactly that version. Lint and release config checks match CI:

```sh
golangci-lint run                 # v2.14.0
goreleaser check
goreleaser build --snapshot --clean
```

## Release process

Releases are cut by pushing a semver tag on a merged, CI green commit:

```sh
git tag v0.2.22
git push origin v0.2.22
```

`.github/workflows/release.yml` runs goreleaser: 5 platform binaries plus a
universal macOS binary, `archivist_v<version>_SHA256SUMS`,
`archivist_v<version>_skill-bundle.tar.gz`, a GitHub Release, SLSA build
provenance, and the Homebrew cask `Casks/archivist.rb` pushed to
`mosaic-finance-inc/homebrew-tap` (needs the `HOMEBREW_TAP_GITHUB_TOKEN`
secret). macOS signing and notarization run only when the `APPLE_*` secrets are
set.

## Architecture

This binary is a thin client over Mosaic's chat-api: the `/research` routes for
search and reading, `/account` routes for credentials and usage. The monorepo
reference is `reference/archivist/cli.md`.

## License

[Apache 2.0](./LICENSE). Copyright 2026 Mosaic Finance Inc.
