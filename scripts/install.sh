#!/usr/bin/env bash
# Archivist CLI installer (macOS, Linux).
#
#   curl -fsSL https://github.com/mosaicss/archivist/releases/latest/download/install.sh | bash
#   curl -fsSL https://github.com/mosaicss/archivist/releases/latest/download/install.sh | bash -s -- --pair CODE
#
# Installs or updates archivist, then with --pair redeems the pairing code
# from the Mosaic workspace (archivist connect --pair), installs the
# background service (archivist connect --install) and reports it
# (archivist connect --status). Every downloaded archive is checked against
# the release's archivist_v<ver>_SHA256SUMS before anything is installed.
#
# Options:
#   --pair CODE    pair this machine with the code shown in Mosaic
#   --no-service   pair only; do not install the background service (one
#                  already installed is still restarted on the new key)
#   --help         show this help
#
# Environment:
#   ARCHIVIST_INSTALL_DIR       install directory (default ~/.local/bin)
#   ARCHIVIST_INSTALL_VERSION   install this tag (e.g. v0.2.26) instead of the
#                               version this script was released with
#   ARCHIVIST_RELEASE_BASE_URL  release base URL (default
#                               https://github.com/mosaicss/archivist/releases);
#                               assets are read from <base>/download/<tag>/,
#                               the latest tag from the <base>/latest redirect.
#                               For local testing against a fake release.
#
# Bash, not sh: pipefail is not POSIX and /bin/sh is often dash.

set -euo pipefail

# Stamped with the release tag by the release workflow. An unstamped copy
# (run from a checkout) installs the latest release.
ARCHIVIST_STAMPED_VERSION="__ARCHIVIST_VERSION__"

RELEASE_BASE="${ARCHIVIST_RELEASE_BASE_URL:-https://github.com/mosaicss/archivist/releases}"
RELEASE_BASE="${RELEASE_BASE%/}"
BREW_FORMULA="mosaic-finance-inc/tap/archivist"
# The cask token (brew/Casks/archivist.rb); `brew list --cask` with it
# succeeds only while the cask is actually installed.
BREW_CASK="archivist"
CHANNEL_FILE="${HOME}/.archivist/install-channel"

PAIR=""
WANT_SERVICE=1

die() {
  printf 'archivist install: %s\n' "$*" >&2
  exit 1
}

say() {
  printf '%s\n' "$*"
}

usage() {
  cat <<'USAGE'
Usage: install.sh [--pair CODE] [--no-service]

Installs or updates archivist. With --pair, pairs this machine with the code
shown in Mosaic and installs the background service (unless --no-service).
USAGE
}

parse_args() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --pair)
        [ "$#" -ge 2 ] || die "--pair needs the code shown in Mosaic"
        PAIR="$2"
        shift 2
        ;;
      --pair=*)
        PAIR="${1#--pair=}"
        shift
        ;;
      --no-service)
        WANT_SERVICE=0
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown option: $1 (use --pair CODE or --no-service)"
        ;;
    esac
  done
}

# curl_get URL OUT: HTTPS only unless a release base override is in use.
curl_get() {
  local proto="=https"
  if [ -n "${ARCHIVIST_RELEASE_BASE_URL:-}" ]; then
    proto="=http,https"
  fi
  curl -fsSL --proto "$proto" --retry 2 -o "$2" "$1"
}

detect_platform() {
  local os arch
  os="$(uname -s)"
  arch="$(uname -m)"
  case "$os" in
    Darwin)
      # One universal binary (arm64 and amd64) per release.
      PLATFORM="darwin_all"
      ;;
    Linux)
      case "$arch" in
        x86_64|amd64)  PLATFORM="linux_amd64" ;;
        aarch64|arm64) PLATFORM="linux_arm64" ;;
        *) die "unsupported Linux architecture: $arch" ;;
      esac
      ;;
    *)
      die "unsupported operating system: $os. On Windows use install.ps1; see ${RELEASE_BASE}"
      ;;
  esac
  OS="$os"
}

# resolve_version sets VERSION (a vX.Y.Z tag) without the GitHub API: the
# stamped tag, an override, or the tag the latest release redirect names.
resolve_version() {
  if [ -n "${ARCHIVIST_INSTALL_VERSION:-}" ]; then
    VERSION="$ARCHIVIST_INSTALL_VERSION"
  else
    case "$ARCHIVIST_STAMPED_VERSION" in
      v[0-9]*) VERSION="$ARCHIVIST_STAMPED_VERSION" ;;
      *)
        local effective
        effective="$(curl -fsSIL --proto '=http,https' -o /dev/null -w '%{url_effective}' "${RELEASE_BASE}/latest")" \
          || die "could not resolve the latest release from ${RELEASE_BASE}/latest"
        VERSION="${effective##*/}"
        ;;
    esac
  fi
  case "$VERSION" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) die "could not determine a release version (got '${VERSION}')" ;;
  esac
  case "$VERSION" in
    *[!A-Za-z0-9._-]*) die "invalid release version '${VERSION}'" ;;
  esac
  VER="${VERSION#v}"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "no sha256sum or shasum found; cannot verify the download"
  fi
}

# verify FILE NAME: FILE's SHA-256 must equal NAME's line in SHA256SUMS.
verify() {
  local expected actual
  expected="$(awk -v f="$2" '$2 == f || $2 == "*"f {print $1}' "${TMP}/${SUMS}" | head -n 1)"
  case "$expected" in
    [0-9a-f]*) ;;
    *) die "no checksum for $2 in ${SUMS}" ;;
  esac
  [ "${#expected}" -eq 64 ] || die "malformed checksum for $2 in ${SUMS}"
  actual="$(sha256_of "$1")"
  [ "$actual" = "$expected" ] || die "checksum mismatch for $2 (expected ${expected}, got ${actual}). Nothing was installed."
}

# download_and_verify fetches the binary archive (and the skill bundle when
# ~/.claude/skills exists), verifies both, and test runs the binary. Nothing
# is installed until all of that passed.
download_and_verify() {
  local base="${RELEASE_BASE}/download/${VERSION}"
  ARCHIVE="archivist_v${VER}_${PLATFORM}.tar.gz"
  SUMS="archivist_v${VER}_SHA256SUMS"
  SKILL_ARCHIVE="archivist_v${VER}_skill-bundle.tar.gz"

  say "Downloading archivist ${VERSION} (${PLATFORM})..."
  curl_get "${base}/${SUMS}" "${TMP}/${SUMS}" || die "download failed: ${base}/${SUMS}"
  curl_get "${base}/${ARCHIVE}" "${TMP}/${ARCHIVE}" || die "download failed: ${base}/${ARCHIVE}"
  verify "${TMP}/${ARCHIVE}" "$ARCHIVE"

  mkdir -p "${TMP}/x"
  tar -xzf "${TMP}/${ARCHIVE}" -C "${TMP}/x" archivist || die "the archive has no archivist binary"
  chmod 0755 "${TMP}/x/archivist"
  "${TMP}/x/archivist" version >/dev/null 2>&1 || die "the downloaded binary does not run on this machine"

  HAVE_SKILL=0
  if [ -d "${HOME}/.claude/skills" ]; then
    if curl_get "${base}/${SKILL_ARCHIVE}" "${TMP}/${SKILL_ARCHIVE}" 2>/dev/null; then
      verify "${TMP}/${SKILL_ARCHIVE}" "$SKILL_ARCHIVE"
      HAVE_SKILL=1
    else
      say "Note: the Claude Code skill bundle could not be downloaded; skipped."
    fi
  fi
  say "Checksums verified."
}

install_binary() {
  INSTALL_DIR="${ARCHIVIST_INSTALL_DIR:-${HOME}/.local/bin}"
  mkdir -p "$INSTALL_DIR" || die "cannot create ${INSTALL_DIR}; set ARCHIVIST_INSTALL_DIR"
  [ -w "$INSTALL_DIR" ] || die "${INSTALL_DIR} is not writable; set ARCHIVIST_INSTALL_DIR"
  local staged="${INSTALL_DIR}/.archivist.new.$$"
  cp "${TMP}/x/archivist" "$staged"
  chmod 0755 "$staged"
  # Same directory: the rename is atomic, so a running archivist keeps its
  # old inode and nothing sees a half-written binary.
  mv -f "$staged" "${INSTALL_DIR}/archivist"
  BIN="${INSTALL_DIR}/archivist"
  say "Installed ${BIN}"

  if [ "$HAVE_SKILL" = 1 ]; then
    local skill_dir="${HOME}/.claude/skills/archivist"
    mkdir -p "$skill_dir"
    tar -xzf "${TMP}/${SKILL_ARCHIVE}" -C "$skill_dir"
    say "Installed the Claude Code skill in ${skill_dir}"
  fi

  mkdir -p "${HOME}/.archivist"
  printf 'curl-sh\n' > "$CHANNEL_FILE"
}

# brew_update upgrades a Homebrew install in place and uses its binary.
brew_update() {
  say "archivist was installed with Homebrew; upgrading it there..."
  brew upgrade "$BREW_FORMULA" || die "brew upgrade ${BREW_FORMULA} failed"
  BIN="$(command -v archivist)" || die "archivist is not on PATH after brew upgrade"
}

service_installed() {
  case "$OS" in
    Darwin) [ -f "${HOME}/Library/LaunchAgents/com.mosaic-finance.archivist.connect.plist" ] ;;
    *) [ -f "${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user/archivist-connect.service" ] ;;
  esac
}

main() {
  parse_args "$@"
  command -v curl >/dev/null 2>&1 || die "curl is required"
  detect_platform

  TMP="$(mktemp -d "${TMPDIR:-/tmp}/archivist-install.XXXXXX")"
  trap 'rm -rf "$TMP"' EXIT

  local channel=""
  [ -f "$CHANNEL_FILE" ] && channel="$(tr -d '[:space:]' < "$CHANNEL_FILE")"
  # A stale channel file (after brew uninstall) must not send us to brew.
  if [ "$channel" = "brew" ] && command -v brew >/dev/null 2>&1 \
    && brew list --cask "$BREW_CASK" >/dev/null 2>&1; then
    brew_update
  else
    resolve_version
    download_and_verify
    install_binary
  fi

  local on_path
  on_path="$(command -v archivist 2>/dev/null || true)"
  if [ -n "$on_path" ] && [ "$on_path" != "$BIN" ]; then
    say "Note: ${on_path} comes first on PATH; this install is ${BIN}."
  fi

  if [ -n "$PAIR" ]; then
    "$BIN" connect --pair "$PAIR" || exit $?
    # An installed service is restarted on the new binary and key even with
    # --no-service.
    if [ "$WANT_SERVICE" = 1 ] || service_installed; then
      if ! "$BIN" connect --install; then
        printf '\n%s\n%s\n' \
          "This computer is paired, but the background service could not be installed." \
          "Fix the error above, then run: ${BIN} connect --install (no new code is needed)" >&2
        exit 1
      fi
    fi
  elif service_installed; then
    # An update restarts an installed service on the new binary.
    "$BIN" connect --install || exit $?
  fi

  if [ "$WANT_SERVICE" = 1 ] && service_installed; then
    say ""
    if ! "$BIN" connect --status; then
      say ""
      say "The background service is installed but not running. The last log line above says why."
      exit 1
    fi
  fi

  case ":${PATH}:" in
    *":$(dirname "$BIN"):"*) ;;
    *)
      say ""
      say "Add $(dirname "$BIN") to your PATH to run archivist from any terminal:"
      say "  export PATH=\"$(dirname "$BIN"):\$PATH\""
      ;;
  esac
  if [ -z "$PAIR" ] && ! service_installed; then
    say ""
    say "Next: copy the connect command from Mosaic, or run: archivist connect --pair CODE"
  fi
}

main "$@"
