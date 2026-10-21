// Annotation discipline for MCP exposure.
//
// Every Cobra command in this binary sets a minimum set of Annotations so the
// `mcp serve` walker (cmd/archivist/mcp.go) can generate MCP tool definitions
// from the Cobra tree at runtime. The walker reads:
//
//	"pp:typed-exit-codes"  — comma-separated exit codes the verb emits.
//	"mcp:read-only"        — "true" for verbs that never mutate server state.
//	"mcp:title"            — the human-readable tool title.
//	"mcp:hidden"           — "true" to opt out of MCP exposure entirely.
//
// Every MCP-exposed command carries pp:typed-exit-codes, mcp:read-only "true"
// and a non-empty mcp:title.

package cmd

import (
	"github.com/mosaicss/archivist/internal/client"
	"github.com/spf13/cobra"
)

func init() {
	// Keep registration order (auth, search, read, toc, filings, screen, find,
	// companies, doctor, usage, update, connect, version) in --help: research verbs first, in the order
	// an agent uses them. Cobra would otherwise sort alphabetically.
	cobra.EnableCommandSorting = false
}

const longDescription = `Archivist is the Mosaic command line surface for filings research.

It lets any AI agent (Claude Code, Cursor, custom orchestrators) or shell
context (bash, cron, CI) search and read global filings (SEC, SEDAR+, KAP and
expanding). 'search' finds passages; 'read passage', 'read section' and 'toc'
read around them. 'filings' lists a company's filings newest first; 'screen'
finds companies by ratios, size and returns on the nightly snapshot; 'find'
says whether one filing mentions a term. Every passage carries a permalink
url that opens it in Mosaic's filing viewer, so an answer can cite its source.

Output is a table on a terminal and JSON when piped; --format compact prints
minified JSON fitted to about 6k tokens. Run 'archivist version'
for build info and 'archivist <verb> --help' for each verb.`

// NewRootCmd returns the root archivist command with all verbs registered.
// version/commit/date come from -ldflags injection in cmd/archivist/main.go.
func NewRootCmd(version, commit, date string) *cobra.Command {
	root := &cobra.Command{
		Use:   "archivist",
		Short: "Mosaic Archivist CLI",
		Long:  longDescription,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2",
		},
		SilenceErrors: true,
		SilenceUsage:  true,
		// One early check of the chat-api URL override, before any command
		// makes a request; the error is a usage error (exit 2).
		// 'archivist version' makes no request and stays usable.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "version" && cmd.Parent() == cmd.Root() {
				return nil
			}
			return client.CheckBaseURLEnv()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true

	// Global --token flag: overrides ARCHIVIST_TOKEN for a single invocation.
	root.PersistentFlags().String("token", "", "Override ARCHIVIST_TOKEN for this call (e.g., --token ak_...)")

	root.AddCommand(newAuthCmd(version))
	root.AddCommand(newSearchCmd(version))
	root.AddCommand(newReadCmd(version))
	root.AddCommand(newTocCmd(version))
	root.AddCommand(newFilingsCmd(version))
	root.AddCommand(newScreenCmd(version))
	root.AddCommand(newFindCmd(version))
	root.AddCommand(newCompaniesCmd(version))
	root.AddCommand(newDoctorCmd(version, commit, date))
	root.AddCommand(NewUsageCmd(version))
	root.AddCommand(NewUpdateCmd(version))
	root.AddCommand(newConnectCmd(version))
	root.AddCommand(NewVersionCmd(version, commit, date))

	return root
}
