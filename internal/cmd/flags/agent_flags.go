// Package flags provides the shared agent-native flag set registered on every
// archivist verb that produces output. This stub is fulfilled by Story 36.3;
// callers in 36.4 wire to this package so the call site is unchanged when 36.3
// lands its real implementation.
package flags

import "github.com/spf13/cobra"

// AgentFlags holds the parsed values of the shared agent-native flags.
type AgentFlags struct {
	Compact bool
	DryRun  bool
	Quiet   bool
	NoColor bool
	Stdin   bool
}

// RegisterAgentFlags adds --compact, --dry-run, --quiet, --no-color, --stdin
// persistent flags to cmd. Call in each verb's init() or cobra.Command setup.
func RegisterAgentFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("compact", false, "Omit citations block and confidence scores (~60-80% token reduction)")
	cmd.Flags().Bool("dry-run", false, "Validate spec and print wire payload; do NOT send HTTP request")
	cmd.Flags().Bool("quiet", false, "Suppress all stderr progress messages")
	cmd.Flags().Bool("no-color", false, "Disable ANSI color in stdout and stderr")
	cmd.Flags().Bool("stdin", false, "Read spec content from stdin (mutually exclusive with file path argument)")
}

// ReadAgentFlags reads the shared flag values from cmd after Cobra has parsed
// the command line. Safe to call from RunE.
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
