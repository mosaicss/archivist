// Package flags provides shared flag helpers consumed by every output-producing verb
// (chat, table, companies). Defined here in Story 36.3; consumed by 36.4+ verbs.
package flags

import "github.com/spf13/cobra"

// AgentFlags holds the parsed values of the 5 agent-native flags.
type AgentFlags struct {
	Compact bool
	DryRun  bool
	Quiet   bool
	NoColor bool
	Stdin   bool
}

// RegisterAgentFlags wires the 5 agent-native flags onto cmd.
// Call this from every verb that produces output (chat, table, companies).
func RegisterAgentFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("compact", false, "Drop low-value output fields (citations block, watch footer, etc.)")
	cmd.Flags().Bool("dry-run", false, "Print request payload to stdout; do not hit the network")
	cmd.Flags().Bool("quiet", false, "Suppress all stderr progress messages (errors still print)")
	cmd.Flags().Bool("no-color", false, "Disable ANSI color in stdout and stderr")
	cmd.Flags().Bool("stdin", false, "Read structured params from stdin as JSON (filter fields win over flags)")
}

// ReadAgentFlags reads the parsed flag values from cmd after cobra.Execute().
func ReadAgentFlags(cmd *cobra.Command) AgentFlags {
	compact, _ := cmd.Flags().GetBool("compact")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	quiet, _ := cmd.Flags().GetBool("quiet")
	noColor, _ := cmd.Flags().GetBool("no-color")
	stdin, _ := cmd.Flags().GetBool("stdin")
	return AgentFlags{
		Compact: compact,
		DryRun:  dryRun,
		Quiet:   quiet,
		NoColor: noColor,
		Stdin:   stdin,
	}
}
