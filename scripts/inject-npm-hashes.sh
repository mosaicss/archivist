#!/usr/bin/env bash
# Fills the npm package's hash placeholders and version from a release's
# SHA256SUMS. Used by the release workflow's npm job, and by CI against a
# snapshot build (on scratch copies).
#
#   scripts/inject-npm-hashes.sh SUMS_FILE VERSION JS PACKAGE_JSON
#
# VERSION has no leading v. Each SUMS line archivist_v<VERSION>_<part>.<ext>
# fills the 'SHA256_<PART>' placeholder in JS: darwin_all gives
# SHA256_DARWIN_ALL, skill-bundle SHA256_SKILL_BUNDLE. Fails when a
# placeholder is left or package.json does not take the version.

set -euo pipefail

[ "$#" -eq 4 ] || { echo "usage: inject-npm-hashes.sh SUMS_FILE VERSION JS PACKAGE_JSON" >&2; exit 2; }
SUMS_FILE="$1"
VER="$2"
JS="$3"
PKG="$4"
[ -f "$SUMS_FILE" ] || { echo "SHA256SUMS file not found: $SUMS_FILE" >&2; exit 1; }

while read -r hash filename; do
  case "$hash" in
    *[!0-9a-f]* | "") echo "malformed SHA256SUMS line: $hash $filename" >&2; exit 1 ;;
  esac
  [ "${#hash}" -eq 64 ] || { echo "malformed SHA256SUMS line: $hash $filename" >&2; exit 1; }
  part="${filename#"archivist_v${VER}_"}"
  part="${part%.tar.gz}"
  part="${part%.zip}"
  placeholder="SHA256_$(printf '%s' "$part" | tr '[:lower:]-' '[:upper:]_')"
  sed -i "s|'${placeholder}'|'${hash}'|g" "$JS"
done < "$SUMS_FILE"

if grep -nE "'SHA256_[A-Z0-9_]+'" "$JS"; then
  echo "FAIL: a hash placeholder was not filled" >&2
  exit 1
fi
sed -i "s/\"version\": \"0.0.0\"/\"version\": \"${VER}\"/" "$PKG"
grep -q "\"version\": \"${VER}\"" "$PKG" || { echo "FAIL: ${PKG} does not carry version ${VER}" >&2; exit 1; }
echo "npm package hashes and version ${VER} injected"
