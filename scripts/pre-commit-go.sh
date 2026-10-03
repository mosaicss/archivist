#!/usr/bin/env bash
set -euo pipefail
unformatted="$(gofmt -l cmd internal)"
if [[ -n "$unformatted" ]]; then
  printf 'Go formatting required:\n%s\n' "$unformatted" >&2
  exit 1
fi
go vet ./...
go test ./... -race
golangci-lint run
go build ./cmd/archivist ./cmd/mosaic-event-contract
