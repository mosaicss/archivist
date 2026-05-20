package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// NewVersionCmd returns the `archivist version` command.
//
// Build info comes from -ldflags injection by goreleaser. For binaries
// installed via `go install github.com/mosaicss/archivist/cmd/archivist@vX.Y.Z`
// (which bypasses goreleaser and therefore leaves the ldflags vars at their
// "dev"/"unknown" defaults), resolveBuildInfo falls back to the module
// version + VCS info embedded by the Go toolchain so AC10 keeps working.
func NewVersionCmd(version, commit, date string) *cobra.Command {
	rv, rc, rd := resolveBuildInfo(version, commit, date)
	return &cobra.Command{
		Use:   "version",
		Short: "Print binary version, commit SHA, build date, and platform",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"archivist-cli %s (commit %s built %s) %s/%s\n",
				rv, rc, rd, runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

// resolveBuildInfo returns the version/commit/date triple, falling back to
// runtime/debug.ReadBuildInfo when ldflags weren't injected (the `go install`
// path). The caller's values win for goreleaser-built binaries; ReadBuildInfo
// fills in for `go install` so users see the real tag and VCS info even
// without goreleaser's ldflags.
func resolveBuildInfo(version, commit, date string) (string, string, string) {
	if version != "dev" {
		return version, commit, date
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				commit = s.Value[:7]
			} else if s.Value != "" {
				commit = s.Value
			}
		case "vcs.time":
			// vcs.time is RFC3339; the prefix is the YYYY-MM-DD date.
			if len(s.Value) >= 10 {
				date = s.Value[:10]
			} else if s.Value != "" {
				date = s.Value
			}
		}
	}
	return version, commit, date
}
