// Command archivist is the Mosaic Archivist CLI binary.
//
// Build with:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always) \
//	                   -X main.commit=$(git rev-parse --short HEAD) \
//	                   -X main.date=$(date -u +%Y-%m-%d)" \
//	         ./cmd/archivist
//
// Release builds inject the same flags via goreleaser.yaml.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/mosaicss/archivist/internal/cmd"
)

// Injected at build time via -ldflags.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	root := cmd.NewRootCmd(version, commit, date)
	// Register verbs that live in package main (table — Story 36.4).
	root.AddCommand(newTableCmd(version))
	err := root.Execute()
	if err == nil {
		return
	}
	var exitErr *cmd.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.Code)
	}
	// Cobra usage errors (unknown command, unknown flag, missing arg, etc.)
	// reach here because SilenceErrors is true on root. Print and exit 2.
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(cmd.ExitUsageError)
}
