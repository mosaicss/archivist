#!/usr/bin/env sh
# Search filing passages, then read the top hit with its neighbours.
# Prerequisite: a saved credential ('archivist auth login --token ak_...')
# or ARCHIVIST_TOKEN in the environment, on a Mosaic Pro account.
#
# Usage: search-and-read.sh "<query>" [SYMBOL]
#   search-and-read.sh "supply chain risk" AAPL:US

set -eu

QUERY="${1:-supply chain risk}"
SYMBOL="${2:-}"

# Exit 3 means no passages, 6 means the symbol matched several issuers.
# Capture the status so `set -e` does not abort before we can explain it.
rc=0
if [ -n "$SYMBOL" ]; then
  RESULTS=$(archivist search "$QUERY" --symbol "$SYMBOL" --limit 5 --format json) || rc=$?
else
  RESULTS=$(archivist search "$QUERY" --limit 5 --format json) || rc=$?
fi

case "$rc" in
  0) ;;
  3)
    echo "No passages found for: $QUERY" >&2
    exit 3
    ;;
  6)
    echo "Ambiguous symbol: $SYMBOL matches several issuers; pass the full TICKER:EXCHANGE symbol." >&2
    exit 6
    ;;
  *) exit "$rc" ;;
esac

# Every passage carries a permalink "url" to cite. Print them.
echo "Permalinks:"
printf '%s\n' "$RESULTS" | grep '"url"' | sed 's/.*"url": *//; s/,$//'

# Read the top hit with one neighbouring passage on each side.
CHUNK_ID=$(printf '%s\n' "$RESULTS" | grep '"id"' | head -1 \
  | sed 's/.*"id": *"\([^"]*\)".*/\1/')

if [ -z "$CHUNK_ID" ]; then
  echo "No passages found for: $QUERY" >&2
  exit 3
fi

archivist read passage "$CHUNK_ID" --window 1 --format json
