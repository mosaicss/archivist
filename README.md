# Archivist CLI

Mosaic's command line surface for filings research. Lets any AI agent (Claude Code, Cursor, custom orchestrators) or shell context (bash, cron, CI) drive Mosaic's chat and table research over Clerk identity. The Mosaic web UI is the audit surface for every CLI call.

> **Status:** v0.1.0 is a scaffold. All verbs return exit code 9 (not implemented) with a pointer to the story that ships them. Real behavior lands across Stories 36.2 through 36.13.

## Install

Four install paths are planned. Only `go install` works today; the rest land in Story 36.11 alongside the curl|sh installer, the npm orchestrator, and the Homebrew tap.

```sh
# Working today (Go 1.22+)
go install github.com/mosaicss/archivist/cmd/archivist@latest

# Coming in Phase 2 (Story 36.11)
curl -fsSL https://install.mosaic-finance.com | sh
npx -y @mosaic-finance/archivist install
brew install mosaicss/tap/archivist
```

**macOS note for v0.1.0:** binaries are unsigned (notarization lands in Story 36.11). On first launch you may see a gatekeeper warning. Clear it once with:

```sh
xattr -d com.apple.quarantine ~/go/bin/archivist
```

## Quickstart

```text
$ archivist --help
Archivist is the Mosaic command line surface for filings research.

It lets any AI agent (Claude Code, Cursor, custom orchestrators) or shell
context (bash, cron, CI) drive Mosaic's chat and table research over Clerk
identity. The Mosaic web UI is the audit surface for every CLI call.

Phase 1 verb behavior lands across Story 36.2 to 36.13. Run 'archivist version'
for build info and 'archivist --help' for the verb list.

Usage:
  archivist [command]

Available Commands:
  auth        Manage credentials (env var ARCHIVIST_TOKEN; dashboard issues tokens) (not yet implemented)
  chat        Run a research question against Mosaic filings (not yet implemented)
  table       Build or rerun a research table over filings (not yet implemented)
  companies   Search or fetch issuer records (not yet implemented)
  usage       Report quota and rate limit consumption (not yet implemented)
  update      Replace the binary in place with the latest release (not yet implemented)
  version     Print binary version, commit SHA, build date, and platform
```

```text
$ archivist version
archivist-cli v0.1.0 (commit abc1234 built 2026-05-19) darwin/arm64
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

## Exit codes

Agents that orchestrate this binary should read exit codes rather than parse output:

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

## Release process

Releases are cut by tagging a semver version and pushing the tag:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The `.github/workflows/release.yml` workflow runs goreleaser on the tag, builds 5 platform binaries, generates `archivist_v<version>_SHA256SUMS`, and publishes the artifacts to GitHub Releases. SLSA provenance attestation and macOS notarization land in Story 36.11.

## Architecture

This binary is a thin client over the Mosaic chat-api Cloud Run service. See the architecture document at `_bmad-output/planning-artifacts/architecture-e36-archivist-cli.md` in the mosaic monorepo for the design ground truth.

## License

[Apache 2.0](./LICENSE). Copyright 2026 Mosaic Finance Inc.
