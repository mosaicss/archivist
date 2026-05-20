#!/usr/bin/env sh
# CI drift check: verify that the marketing page install commands match the
# canonical strings defined here.
#
# Usage: scripts/ci-drift-check.sh <path-to-marketing-page-source>
# The marketing page source is typically apps/web/components/marketing/archivist-page.tsx
# or the equivalent HTML/TSX file from the mosaic repo.
#
# Exit codes:
#   0 — all canonical commands found
#   1 — one or more canonical commands missing (advisory warning only in CI)
#
# This check is ADVISORY in Phase 1 — it warns but does not block releases.

set -eu

MARKETING_FILE="${1:-}"

if [ -z "$MARKETING_FILE" ]; then
  echo "Usage: $0 <marketing-page-source-file>" >&2
  exit 1
fi

if [ ! -f "$MARKETING_FILE" ]; then
  echo "WARNING: marketing page file not found: $MARKETING_FILE" >&2
  echo "Skipping drift check (file not present)."
  exit 0
fi

FAILED=0

check_command() {
  CMD="$1"
  LABEL="$2"
  if grep -qF "$CMD" "$MARKETING_FILE"; then
    echo "  OK: $LABEL"
  else
    echo "  MISSING: $LABEL — expected: $CMD" >&2
    FAILED=1
  fi
}

echo "=== Archivist CLI install-command drift check ==="
echo "Marketing file: $MARKETING_FILE"
echo ""

check_command \
  'curl -fsSL https://install.mosaic-finance.com | sh' \
  "curl|sh (primary channel)"

check_command \
  'npx -y @mosaic-finance/archivist install' \
  "npm orchestrator"

check_command \
  'brew install mosaic-finance/tap/archivist' \
  "Homebrew tap"

check_command \
  'https://github.com/mosaicss/archivist/releases' \
  "GitHub Releases link"

check_command \
  'go install github.com/mosaicss/archivist/cmd/archivist@latest' \
  "go install (dev fallback)"

echo ""
if [ "$FAILED" -eq 1 ]; then
  echo "DRIFT DETECTED: one or more canonical install commands are missing from the marketing page."
  echo "(Advisory only in Phase 1 — this does not block the release.)"
  # Exit 0 in Phase 1 so CI passes — this is advisory only
  exit 0
else
  echo "No drift detected."
  exit 0
fi
