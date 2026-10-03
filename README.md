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
| `connect` | Drive your own Claude Code or Codex from the Mosaic workspace (preview, see below) |
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

`auth login`, `auth logout`, `update` and `connect` are not exposed: token
setup, binary replacement and harness supervision are operator actions. A
failed call returns an error result naming the exit code, with the verb's
stderr and stdout.

`--token-file <path>` reads the token from a file on every tool call, so a
supervisor can rotate it without restarting the server (it cannot be combined
with `--token`).

**Task mode.** When the token is a session task token (`mst_...`, minted by
`archivist connect` for one workspace session), the server exposes only the
tools whose chat-api routes a task token may call: `search`,
`companies_search`, `read_passage`, `read_section` and `toc`. `companies_get`
falls back to the full company catalog, which task tokens cannot read, so it
is left out. `auth login` refuses task tokens.

## Connect your Claude Code or Codex (preview)

`archivist connect` lets the Mosaic workspace drive the Claude Code or Codex
installed on your machine. It runs in the foreground and keeps one outbound WebSocket to
the Mosaic agent relay; nothing listens on your machine.

```bash
archivist connect --check   # what was detected; connects nothing
archivist connect           # serve workspace sessions until Ctrl-C
archivist connect --claude-model claude-sonnet-5 --claude-effort low
archivist connect --codex-model gpt-6-luna --codex-effort low
```

Requirements: an `ak_` API key (`archivist auth login`) on a Pro account, and
at least one of: Claude Code 2.1.280 or newer logged in with a claude.ai
subscription (`claude auth status` shows `authMethod: claude.ai`), or Codex
0.160.0 or newer logged in with ChatGPT (`codex login status`). API key logins
are refused. Newer versions are reported, not blocked; a harness that is not
usable when `archivist connect` starts is reported as unavailable and its
sessions are refused (restart after logging in).

For each "My Claude Code" session the daemon:

- runs `claude -p` in stream-json mode in a fresh temporary directory, with
  only `HOME`, `PATH`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `TERM`, `TMPDIR`,
  `LC_*` and `XDG_*` in its environment (provider keys and `CLAUDE_*`,
  `ARCHIVIST_*` variables never pass) and without your user, project or local
  Claude settings;
- refuses the session unless Claude reports a subscription login
  (`apiKeySource: none`) and the default permission mode;
- gives Claude the built-in tools Bash, Read, Edit, Write, Glob and Grep and
  the Mosaic search and read tools (`archivist mcp serve` in task mode, under
  a 15 minute task token it rotates and revokes); every tool call that needs
  permission becomes an approval card in the workspace (allow once, allow for
  session, deny once, deny for session; no answer within 60 seconds denies).
  "Allow for session" and "deny for session" apply to every later call of that
  tool in the session, whatever its input, without another card; they live in
  the daemon's memory, so restarting `archivist connect` clears them;
- streams the session as `mosaic-event/1` events, each validated before it is
  sent.

For each "My Codex" session the daemon:

- runs one `codex app-server --stdio --strict-config` in a fresh temporary
  directory, with the same environment allowlist plus `CODEX_HOME` set to a
  private per-session home, `~/.archivist/connect/codex-home/<session>/`
  (0700). That home holds only `auth.json` as a link to your login
  (`$CODEX_HOME/auth.json` if `CODEX_HOME` is an absolute path, else
  `~/.codex/auth.json`), never a copy; Codex refreshes the login in place
  through the link. Your Codex config, `AGENTS.md`, rules, hooks, plugins,
  skills, memories, MCP servers, notifications and project trust are not
  loaded and never written; parent `OPENAI_*` and `CODEX_*` variables never
  pass;
- fixes the rest with `-c` overrides: ChatGPT login only, the `openai`
  provider, a `workspace-write` sandbox that can write only the session
  directory (not `/tmp` or `$TMPDIR`, where other sessions' directories
  live; `TMPDIR` points at `<session dir>/.tmp`) with no network, a core
  shell environment for commands, web search off, history off,
  no project root markers, and the archivist MCP server (task mode tools,
  task token as above). The approval policy (`untrusted`, reviewed by you)
  and model are set per thread; `--codex-model` defaults to Codex's default
  model and `--codex-effort` to Codex's default effort;
- refuses the session (error plus a failed status, Codex stopped, nothing
  else sent) unless Codex reports the session home, a ChatGPT account, the
  `openai` provider, the untrusted approval policy reviewed by the user, the
  workspace-write sandbox without network, extra roots, `/tmp` or `$TMPDIR`
  (TMPDIR points inside the session directory), no instruction files, and the archivist MCP server ready with no other MCP server. A later
  switch away from the ChatGPT login ends the session the same way;
- turns command, file change and archivist tool approvals into workspace
  cards. Your answer maps one to one onto Codex's choices: allow once is
  accept, allow for session is Codex's accept for session (the identical
  command, or the same file, runs again without asking for the rest of the
  session), deny once (or no answer within 60 seconds) is decline, and deny
  for session is cancel (deny and end the turn); for an archivist tool, allow
  for session uses Codex's session-only persistence. archivist connect never
  sends Codex's rule amendments or "always" approvals, so no approval is
  written to your config;
- interrupts with `turn/interrupt` and then cleans the thread's background
  terminals; after a restart the next message resumes the same thread
  (`thread/resume`) from the kept session home;
- stops every process a session started, including commands Codex detaches
  with `setsid`: the daemon records each session's descendants and, on
  Linux, adopts orphaned ones as child subreaper, then kills and reaps them
  when the session stops, is interrupted past its timeout, crashes or
  Ctrl-C ends the daemon.

The session home is kept while the session can resume and removed when the
session stops, fails or is found inactive at the next start.

The relay sends only data (session ids, prompts, approval decisions,
interrupt and stop). Binaries, arguments and flags are fixed on your machine.
Session ids and working directories are kept in `~/.archivist/connect/`
(0700); after a restart, the next message resumes the same Claude session.
Each running session's task token and MCP config sit in
`~/.archivist/connect/run/<session>/` (`task-token`, `mcp.json`, 0600), are
removed when the session stops or the daemon exits, and the token is revoked
then. Ctrl-C stops every Claude Code and Codex process and revokes its tokens.

`ARCHIVIST_RELAY_URL` overrides the relay address (default
`wss://relay.mosaic-finance.com`). macOS and Linux only for now.

Exit codes: 0 stopped by Ctrl-C (or `--check` found a usable Claude Code or
Codex); 1 refused or stopped (feature not enabled, another `archivist connect`
took over, Claude Code too old); 2 bad flag or relay URL; 3 no usable harness
(Claude Code not found, or neither Claude Code nor an installed Codex is
usable); 4 credential or Claude login problem. When Codex is not installed,
the Claude Code codes apply as before.

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
