#!/usr/bin/env sh
# Resolve a company name to its symbol, then search that company's filings.
# Prerequisite: a saved credential ('archivist auth login --token ak_...')
# or ARCHIVIST_TOKEN in the environment, on a Mosaic Pro account.
#
# Usage: companies-resolve.sh "<company>" "<query>"
#   companies-resolve.sh "Shopify" "revenue growth drivers"

set -eu

COMPANY="${1:-Shopify}"
QUERY="${2:-revenue growth drivers}"

# companies search prints a JSON array off a terminal; take the first symbol.
SYMBOL=$(archivist companies search "$COMPANY" --format json \
  | grep '"symbol"' | head -1 \
  | sed 's/.*"symbol": *"\([^"]*\)".*/\1/')

if [ -z "$SYMBOL" ]; then
  echo "No company found for: $COMPANY" >&2
  exit 3
fi

echo "Resolved: $COMPANY -> $SYMBOL" >&2
archivist search "$QUERY" --symbol "$SYMBOL" --format json
