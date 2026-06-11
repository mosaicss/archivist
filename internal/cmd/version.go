package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

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

			skillVer, skillPath := readSkillVersion()
			if skillVer != "" {
				binaryVer := strings.TrimPrefix(rv, "v")
				sVer := strings.TrimPrefix(skillVer, "v")
				if binaryVer != sVer {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"  skill: %s at %s [outdated — binary is %s]\n",
						skillVer, skillPath, rv)
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"[skill outdated: binary %s, skill %s; run 'archivist update --skill' to sync]\n",
						rv, skillVer)
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"  skill: %s at %s\n",
						skillVer, skillPath)
				}
			}
			return nil
		},
	}
}

// readSkillVersion reads the version header from ~/.claude/skills/archivist/SKILL.md.
// Returns ("", "") if the file doesn't exist or has no version header.
// Marker format: <!-- version: x.y.z -->. The marker sits below the YAML
// frontmatter Claude Code requires for skill discovery, so scan the head of
// the file rather than assuming line 1 (pre-frontmatter bundles had it first).
func readSkillVersion() (version, path string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	skillPath := filepath.Join(home, ".claude", "skills", "archivist", "SKILL.md")
	f, err := os.Open(skillPath)
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()

	const maxHeaderLines = 20
	scanner := bufio.NewScanner(f)
	for i := 0; i < maxHeaderLines && scanner.Scan(); i++ {
		line := scanner.Text()
		// Expected: <!-- version: x.y.z -->
		if strings.HasPrefix(line, "<!-- version:") && strings.HasSuffix(line, "-->") {
			ver := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "<!-- version:"), "-->"))
			if ver != "" {
				return ver, skillPath
			}
		}
	}
	return "", ""
}

// ResolveBuildInfo returns the version/commit/date triple, falling back to
// runtime/debug.ReadBuildInfo when ldflags weren't injected (the `go install`
// path). The caller's values win for goreleaser-built binaries; ReadBuildInfo
// fills in for `go install` so users see the real tag and VCS info even
// without goreleaser's ldflags.
//
// Story 37.9 promoted this from package-internal to exported so main.go can
// resolve once at startup and pass the canonical triple to every verb
// (doctor, update, usage, etc.). Previously only the version verb resolved,
// and the others reported "dev"/"vdev" on `go install` builds.
func ResolveBuildInfo(version, commit, date string) (string, string, string) {
	return resolveBuildInfo(version, commit, date)
}

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
