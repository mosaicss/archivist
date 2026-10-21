#!/usr/bin/env bash
# Installs the pinned build tools of CI and the release workflow from their
# release tarballs, each checked against a pinned SHA-256 (Linux amd64).
#
#   scripts/install-release-tools.sh DEST TOOL...
#
# TOOL is goreleaser or golangci-lint; each binary lands in DEST. A changed
# version needs its new tarball digest here (from the tool's release
# checksums file).

set -euo pipefail

GORELEASER_VERSION="2.18.2"
GORELEASER_SHA256="0a96edc9d9bc594e4a41cc4d59467c182062910ab24d9d1f6dd7b667d32606d3"
GOLANGCI_LINT_VERSION="2.14.0"
GOLANGCI_LINT_SHA256="ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab"

die() {
  printf 'install-release-tools: %s\n' "$*" >&2
  exit 1
}

[ "$#" -ge 2 ] || die "usage: install-release-tools.sh DEST TOOL..."
DEST="$1"
shift
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64 | Linux/amd64) ;;
  *) die "Linux amd64 only" ;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$DEST"

# fetch URL SHA256 OUT: download URL to OUT and check its digest.
fetch() {
  curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 -o "$3" "$1" || die "download failed: $1"
  printf '%s  %s\n' "$2" "$3" | sha256sum -c --quiet - || die "checksum mismatch: $1"
}

for tool in "$@"; do
  case "$tool" in
    goreleaser)
      fetch "https://github.com/goreleaser/goreleaser/releases/download/v${GORELEASER_VERSION}/goreleaser_Linux_x86_64.tar.gz" \
        "$GORELEASER_SHA256" "$WORK/goreleaser.tar.gz"
      tar -xzf "$WORK/goreleaser.tar.gz" -C "$WORK" goreleaser
      install -m 0755 "$WORK/goreleaser" "$DEST/goreleaser"
      ;;
    golangci-lint)
      dir="golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64"
      fetch "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/${dir}.tar.gz" \
        "$GOLANGCI_LINT_SHA256" "$WORK/golangci-lint.tar.gz"
      tar -xzf "$WORK/golangci-lint.tar.gz" -C "$WORK" "${dir}/golangci-lint"
      install -m 0755 "$WORK/${dir}/golangci-lint" "$DEST/golangci-lint"
      ;;
    *)
      die "unknown tool: $tool"
      ;;
  esac
  printf 'installed %s\n' "$DEST/$tool"
done
