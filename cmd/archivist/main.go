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
	"github.com/spf13/cobra"
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

	// newRoot builds the full command tree: internal/cmd verbs + package-main
	// verbs (table — Story 36.4). The factory is passed to newMCPCmd so the
	// MCP walker and every tools/call dispatch get a pristine tree; the
	// closure predates mcp registration below, so dispatch roots exclude mcp
	// by construction (Story 39.7).
	newRoot := func() *cobra.Command {
		r := cmd.NewRootCmd(resolvedVersion, resolvedCommit, resolvedDate)
		r.AddCommand(newTableCmd(resolvedVersion))
		return r
	}

	root := newRoot()
	root.AddCommand(newMCPCmd(newRoot, resolvedVersion))
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
