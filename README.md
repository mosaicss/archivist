# Archivist CLI

Mosaic's command line surface for filings research. Lets any AI agent (Claude Code, Cursor, custom orchestrators) or shell context (bash, cron, CI) drive Mosaic's chat and table research over Clerk identity. The Mosaic web UI is the audit surface for every CLI call.

## Install

Choose the channel that matches your environment:

### curl | bash (recommended, no dependencies)

```sh
curl -fsSL https://install.mosaic-finance.com | bash
```

Installs to `~/.local/bin/archivist`. Automatically places the Claude Code skill
in `~/.claude/skills/archivist/` if you have Claude Code installed.

**macOS note:** If you see a gatekeeper warning on first launch, clear it with:

```sh
xattr -d com.apple.quarantine ~/.local/bin/archivist
```

### npm (for Node.js users)

```sh
npx -y @mosaic-finance/archivist install
```

Pinned installs are recommended for reproducibility:

```sh
npx -y @mosaic-finance/archivist@0.2.5 install
```

### Homebrew (macOS)

```sh
brew install mosaic-finance-inc/tap/archivist
```

Installs the universal macOS binary as a Homebrew cask, places the Claude Code
skill automatically, and clears the Gatekeeper quarantine flag so no manual
`xattr` step is needed. On Linux, use the curl | bash installer above instead.

### GitHub Releases (manual download)

Download the archive for your platform from:

```
https://github.com/mosaicss/archivist/releases
```

**Manual skill install** (for direct-download users):

```sh
mkdir -p ~/.claude/skills/archivist && \
  curl -fsSL https://github.com/mosaicss/archivist/releases/latest/download/archivist_skill-bundle.tar.gz \
  | tar -xzf - -C ~/.claude/skills/archivist/
```

### go install (developer escape hatch)

```sh
go install github.com/mosaicss/archivist/cmd/archivist@latest
```

Note: `go install` does not auto-install the Claude Code skill. Run the one-liner
below to add Claude Code integration after installing:

```sh
mkdir -p ~/.claude/skills/archivist && \
  curl -fsSL https://github.com/mosaicss/archivist/releases/latest/download/archivist_skill-bundle.tar.gz \
  | tar -xzf - -C ~/.claude/skills/archivist/
```

## Quickstart

```text
$ archivist --help
Archivist is the Mosaic command line surface for filings research.

It lets any AI agent (Claude Code, Cursor, custom orchestrators) or shell
context (bash, cron, CI) drive Mosaic's chat and table research over Clerk
identity. The Mosaic web UI is the audit surface for every CLI call.

Usage:
  archivist [command]

Available Commands:
  auth        Manage credentials
  chat        Run a research question against Mosaic filings
  table       Build or rerun a research table over filings
  companies   Search or fetch issuer records
  usage       Report quota and rate limit consumption
  update      Replace the binary in place with the latest release
  version     Print binary version, commit SHA, build date, and platform
  doctor      Diagnose connectivity and auth
  explain     Show default-window descriptor and cascade rules
```

```text
$ archivist version
archivist-cli v0.2.0 (commit abc1234 built 2026-05-20) darwin/arm64
  skill: v0.2.0 at ~/.claude/skills/archivist/
```

## Authenticating

Open https://mosaic-finance.com (sign in if needed), click your user avatar (top right)
→ Manage account → API keys → Add new key. Copy the `ak_...` token, then save it once:

```sh
archivist auth login --token ak_<your-token>
```

The CLI verifies the token against the server and writes it to
`~/.archivist/credentials` (mode 0600). Every new terminal works from then on.
`archivist auth status` shows which credential is active and where it came
from; `archivist auth logout` deletes the saved file.

For CI and scripting, the environment variable override is still supported and
takes precedence over the saved file:

```sh
export ARCHIVIST_TOKEN=ak_<your-token>
```

Or pass per-call (highest precedence):

```sh
archivist --token ak_<your-token> chat --company cik:1594805 "What are the key risks?"
```

## Self-update

```sh
archivist update          # upgrades the binary in place
archivist update --skill  # refreshes the Claude Code skill only
archivist update --check  # checks for updates without installing
```

For brew-installed binaries: `brew upgrade mosaic-finance-inc/tap/archivist`.
For npm-installed binaries: `npx -y @mosaic-finance/archivist@latest install`.

## MCP server

`archivist mcp serve` runs the binary as an MCP server over stdio. Every CLI
verb becomes an MCP tool with the same auth flow, exit code semantics, and
web UI audit surface, so any MCP host (Claude Desktop, Cursor, custom agents)
can drive Mosaic filing research without shell access.

Prerequisites: the `archivist` binary on PATH and an authenticated archivist
CLI (one-time `archivist auth login --token ak_...`, or `ARCHIVIST_TOKEN`; see
Authenticating above).

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
credential; keep it to pin a specific token for the MCP host.

Tools exposed (15): `auth_status`, `auth_whoami`, `chat`, `doctor`, `table`,
`table_run`, `table_rerun`, `table_list`, `table_watch`, `companies_search`,
`companies_get`, `usage`, `version`, `explain_cascade`, `explain_defaults`.
Tool names join the CLI verb path with underscores; hosts add their own
namespace prefix from the server name (`archivist`).

Notes:

- `auth login`, `auth logout`, and `update` are deliberately not exposed:
  token setup and binary self-replacement are operator actions, not agent
  tools.
- `table_run` takes the spec as a `spec_yaml` string parameter, the exact
  contents of the file you would pass on the command line.
- MCP calls share the same per account rate limit as the web UI and CLI.
  Heavy MCP usage draws down the same quota `archivist usage` reports.
- Long table runs can exceed host tool timeouts: pass `async: true` and poll
  with `table_watch`.

## Exit codes

Agents should branch on exit codes, not parse output text:

| Code | Meaning                                                                |
| ---- | ---------------------------------------------------------------------- |
| 0    | success                                                                |
| 1    | generic error                                                          |
| 2    | usage error (bad flag combo, bad spec)                                 |
| 3    | not found (companies get matched nothing; missing file; bad session)   |
| 4    | auth error (missing or expired credential; tier mismatch)              |
| 5    | server error (5xx after retries; minimum CLI version block)            |
| 6    | ambiguous match (multiple candidates; rerun with --company)            |
| 7    | rate limit (429 after retries; per user quota exhausted)               |
| 8    | cascade violation (custom entity x filings column; country lock)       |
| 9    | not implemented (stub verb in current binary; do not retry)            |

## Claude Code skill

The Archivist CLI ships a Claude Code skill that teaches Claude how to drive
`archivist` correctly. The skill is installed automatically by the curl|sh, npm,
and brew channels. For go install and direct download, use the manual one-liner above.

Plugin manifest: `.claude-plugin/plugin.json`

```sh
# Install via plugin CLI (reads from plugin.json)
gh skill install mosaicss/archivist

# Or via skills CLI
npx skills add mosaic-finance/archivist/skills -g -a claude-code -y
```

## Local development

```sh
git clone https://github.com/mosaicss/archivist.git
cd archivist
go test ./... -race
go build ./cmd/archivist
./archivist version
```

Run the linter (matches CI):

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
golangci-lint run
```

Run goreleaser locally to verify the 5 platform builds without publishing:

```sh
brew install goreleaser
goreleaser build --snapshot --clean
ls dist/
```

## Release process

Releases are cut by tagging a semver version and pushing the tag:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The `.github/workflows/release.yml` workflow runs goreleaser, builds 5 platform
binaries, generates `archivist_v<version>_SHA256SUMS` and `archivist_v<version>_skill-bundle.tar.gz`,
publishes artifacts to GitHub Releases, attests SLSA provenance, publishes the npm
package (if `NPM_TOKEN` secret is set), pushes the Homebrew cask `Casks/archivist.rb`
to the `mosaic-finance-inc/homebrew-tap` repo, and deploys `install.sh` to
`https://install.mosaic-finance.com`. The tap repo and `HOMEBREW_TAP_GITHUB_TOKEN`
secret must exist before tagging or the release fails at the cask publish step.

## Architecture

This binary is a thin client over the Mosaic chat-api Cloud Run service. See the
architecture document at `_bmad-output/planning-artifacts/architecture-e36-archivist-cli.md`
in the mosaic monorepo for the design ground truth.

## License

[Apache 2.0](./LICENSE). Copyright 2026 Mosaic Finance Inc.
