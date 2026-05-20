#!/usr/bin/env bash
# Archivist CLI installer — curl -fsSL https://install.mosaic-finance.com | bash
#
# Supports: macOS arm64/amd64, Linux arm64/amd64
# Requires: curl, uname, tar or unzip, sha256sum or shasum
# No Go, Node.js, or package manager required.
#
# Uses bash explicitly (not /bin/sh) because `set -o pipefail` is not POSIX.
# On Linux /bin/sh is often dash, which would fail at the first pipefail.
# Document the install command as `... | bash` not `... | sh` to match.

set -euo pipefail

REPO="mosaicss/archivist"
INSTALL_DIR="${ARCHIVIST_INSTALL_DIR:-}"
SKILL_DIR="${HOME}/.claude/skills/archivist"
CHANNEL_FILE="${HOME}/.archivist/install-channel"

#
# Platform detection
#
detect_platform() {
  OS="$(uname -s)"
  ARCH="$(uname -m)"

  case "$OS" in
    Darwin)
      case "$ARCH" in
        arm64)   PLATFORM="darwin_arm64" ;;
        x86_64)  PLATFORM="darwin_amd64" ;;
        *)       die "Unsupported macOS architecture: $ARCH" ;;
      esac
      ;;
    Linux)
      case "$ARCH" in
        aarch64|arm64) PLATFORM="linux_arm64" ;;
        x86_64)        PLATFORM="linux_amd64" ;;
        *)             die "Unsupported Linux architecture: $ARCH" ;;
      esac
      ;;
    *)
      die "Unsupported operating system: $OS. See https://github.com/${REPO}/releases for manual download."
      ;;
  esac
}

#
# Resolve install directory
#
resolve_install_dir() {
  if [ -n "$INSTALL_DIR" ]; then
    return
  fi
  if [ -d "${HOME}/.local/bin" ]; then
    INSTALL_DIR="${HOME}/.local/bin"
  elif mkdir -p "${HOME}/.local/bin" 2>/dev/null; then
    INSTALL_DIR="${HOME}/.local/bin"
  elif command -v sudo >/dev/null 2>&1; then
    echo "${HOME}/.local/bin not available; will install to /usr/local/bin (requires sudo)"
    INSTALL_DIR="/usr/local/bin"
    USE_SUDO=1
  else
    die "Cannot determine install directory. Set ARCHIVIST_INSTALL_DIR."
  fi
}

#
# Fetch the latest release tag from GitHub API
#
fetch_latest_version() {
  API_URL="https://api.github.com/repos/${REPO}/releases/latest"
  VERSION="$(curl -fsSL "$API_URL" | grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  if [ -z "$VERSION" ]; then
    die "Could not determine latest release version from GitHub API."
  fi
}

#
# SHA256 verification helper
# Usage: verify_sha256 <archive_file> <expected_hash>
#
verify_sha256() {
  FILE="$1"
  EXPECTED="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$FILE" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "$FILE" | awk '{print $1}')"
  else
    die "No sha256sum or shasum found. Cannot verify download."
  fi
  if [ "$ACTUAL" != "$EXPECTED" ]; then
    die "SHA256 mismatch for $FILE\n  expected: $EXPECTED\n  got:      $ACTUAL"
  fi
}

#
# Download, verify, and install the binary
#
install_binary() {
  ARCHIVE_NAME="archivist_${VERSION}_${PLATFORM}.tar.gz"
  SUMS_NAME="archivist_${VERSION}_SHA256SUMS"
  BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  echo "Downloading archivist ${VERSION} for ${PLATFORM}..."
  curl -fsSL -o "${TMPDIR}/${ARCHIVE_NAME}" "${BASE_URL}/${ARCHIVE_NAME}"
  curl -fsSL -o "${TMPDIR}/${SUMS_NAME}"   "${BASE_URL}/${SUMS_NAME}"

  # Extract expected hash for our archive
  EXPECTED_HASH="$(grep " ${ARCHIVE_NAME}$" "${TMPDIR}/${SUMS_NAME}" | awk '{print $1}')"
  if [ -z "$EXPECTED_HASH" ]; then
    die "Could not find hash for ${ARCHIVE_NAME} in ${SUMS_NAME}"
  fi

  verify_sha256 "${TMPDIR}/${ARCHIVE_NAME}" "$EXPECTED_HASH"
  echo "SHA256 verified."

  tar -xzf "${TMPDIR}/${ARCHIVE_NAME}" -C "$TMPDIR"

  if [ -n "${USE_SUDO:-}" ]; then
    sudo install -m 755 "${TMPDIR}/archivist" "${INSTALL_DIR}/archivist"
  else
    install -m 755 "${TMPDIR}/archivist" "${INSTALL_DIR}/archivist"
  fi
  echo "Binary installed at ${INSTALL_DIR}/archivist"
}

#
# Install the Claude Code skill bundle (silent skip if ~/.claude/skills/ absent)
#
install_skill() {
  SKILL_BASE="${HOME}/.claude/skills"
  if [ ! -d "$SKILL_BASE" ]; then
    return
  fi

  SKILL_ARCHIVE="archivist_${VERSION}_skill-bundle.tar.gz"
  BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
  TMPDIR_SKILL="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR_SKILL"' EXIT

  echo "Installing Claude Code skill..."
  if curl -fsSL -o "${TMPDIR_SKILL}/${SKILL_ARCHIVE}" \
      "${BASE_URL}/${SKILL_ARCHIVE}" 2>/dev/null; then
    mkdir -p "$SKILL_DIR"
    tar -xzf "${TMPDIR_SKILL}/${SKILL_ARCHIVE}" -C "$SKILL_DIR"
    echo "Skill installed at ${SKILL_DIR}"
  fi
}

#
# Record the install channel
#
record_channel() {
  mkdir -p "${HOME}/.archivist"
  printf 'curl-sh' > "$CHANNEL_FILE"
}

die() {
  printf "error: %s\n" "$*" >&2
  exit 1
}

#
# Main
#
main() {
  detect_platform
  resolve_install_dir
  fetch_latest_version
  install_binary
  install_skill
  record_channel

  echo ""
  echo "Archivist ${VERSION} installed successfully."
  echo ""
  echo "Next steps:"
  echo "  archivist --help"
  echo "  archivist auth login"
  echo ""
  echo "Docs: https://mosaic-finance.com/guides/archivist-cli"

  # PATH hint for ~/.local/bin if not already on PATH
  case ":${PATH}:" in
    *":${HOME}/.local/bin:"*) ;;
    *)
      if [ "$INSTALL_DIR" = "${HOME}/.local/bin" ]; then
        echo ""
        echo "NOTE: Add ~/.local/bin to your PATH if the 'archivist' command is not found:"
        echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
      fi
      ;;
  esac
}

main
