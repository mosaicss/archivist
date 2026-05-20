// Annotation discipline for future MCP enablement.
//
// Every Cobra command in this binary sets a minimum set of Annotations so a
// future MCP server (cobratree-style walker; see architecture E36 §11.1) can
// generate MCP tool definitions from the Cobra tree at runtime. The walker
// reads:
//
//	"pp:typed-exit-codes"  — comma-separated exit codes the verb emits.
//	"mcp:read-only"        — "true" for verbs that never mutate server state.
//	"mcp:hidden"           — "true" to opt out of MCP exposure entirely.
//
// Adopt these annotations on every new command. They cost nothing now and
// eliminate the MCP-retrofit pain when the cobratree walker is reimplemented
// later in the project lifecycle.

package cmd

import (
	"github.com/spf13/cobra"
)

func init() {
	// Force registration order (auth, chat, table, companies, usage, update,
	// version) per AC3. Cobra's default alphabetical sort would render
	// "companies" before "table"; the story spec is explicit about the order.
	cobra.EnableCommandSorting = false
}

const longDescription = `Archivist is the Mosaic command line surface for filings research.

It lets any AI agent (Claude Code, Cursor, custom orchestrators) or shell
context (bash, cron, CI) drive Mosaic's chat and table research over Clerk
identity. The Mosaic web UI is the audit surface for every CLI call.

Phase 1 verb behavior lands across Story 36.2 to 36.13. Run 'archivist version'
for build info and 'archivist --help' for the verb list.`

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
	}
	root.CompletionOptions.DisableDefaultCmd = true

	// Global --token flag: overrides ARCHIVIST_TOKEN for a single invocation.
	root.PersistentFlags().String("token", "", "Override ARCHIVIST_TOKEN for this call (e.g., --token ak_...)")

	root.AddCommand(newAuthCmd(version))
	root.AddCommand(NewChatCmd(version))
	root.AddCommand(newDoctorCmd(version, commit, date))
	// table command registered by cmd/archivist/main.go after root is built
	// (Story 36.4: cmd/archivist/table.go lives in package main).
	root.AddCommand(newCompaniesCmd())
	root.AddCommand(NewUsageCmd(version))
	root.AddCommand(NewUpdateCmd(version))
	root.AddCommand(NewVersionCmd(version, commit, date))
	root.AddCommand(newExplainCmd())

	return root
}
