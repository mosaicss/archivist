#!/usr/bin/env sh
# Basic archivist chat invocation.
# Prerequisite: ARCHIVIST_TOKEN set in environment.
#
# Step 1 — resolve the company to get the canonical issuer_key
ISSUER_KEY=$(archivist companies search "Shopify" --format json | grep '"issuer_key"' | head -1 | sed 's/.*"issuer_key": *"\([^"]*\)".*/\1/')

if [ -z "$ISSUER_KEY" ]; then
  echo "Company not found" >&2
  exit 3
fi

# Step 2 — run the chat query
archivist chat --company "$ISSUER_KEY" \
  "What were the main revenue growth drivers in the most recent annual report?"
