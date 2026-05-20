#!/usr/bin/env sh
# Resolve a company name and pipe the issuer_key into a chat query.
# Prerequisite: ARCHIVIST_TOKEN set in environment.

QUERY="${1:-Shopify}"
QUESTION="${2:-What were the key highlights in the most recent annual report?}"

# Search returns JSON lines; extract the first issuer_key
ISSUER_KEY=$(archivist companies search "$QUERY" --format json \
  | grep '"issuer_key"' | head -1 \
  | sed 's/.*"issuer_key": *"\([^"]*\)".*/\1/')

if [ -z "$ISSUER_KEY" ]; then
  echo "No company found for: $QUERY" >&2
  exit 3
fi

echo "Resolved: $ISSUER_KEY"
archivist chat --company "$ISSUER_KEY" "$QUESTION"
