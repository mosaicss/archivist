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
	// Resolve "dev"/"unknown" ldflag defaults once at startup. For binaries
	// installed via `go install` (no goreleaser ldflags), this pulls the real
	// module tag + VCS info from debug.ReadBuildInfo so every verb — version,
	// doctor, update, usage, table — reports the same number (Story 37.9).
	resolvedVersion, resolvedCommit, resolvedDate := cmd.ResolveBuildInfo(version, commit, date)

	root := cmd.NewRootCmd(resolvedVersion, resolvedCommit, resolvedDate)
	// Register verbs that live in package main (table — Story 36.4).
	root.AddCommand(newTableCmd(resolvedVersion))
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
