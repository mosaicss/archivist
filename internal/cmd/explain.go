package cmd

import (
	"fmt"

	"github.com/mosaicss/archivist/internal/cascade"
	"github.com/spf13/cobra"
)

// newExplainCmd returns the top-level `archivist explain` command tree.
// Designed for future subcommands: explain cascade, explain exit-codes, explain rate-limits.
func newExplainCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "explain",
		Short: "Explain CLI rules, exit codes, and behavior",
		Long: `explain provides human-readable documentation for archivist rules and behavior.

Available subcommands:
  cascade     Explain the filter cascade rules enforced at spec-parse time.`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0",
			"mcp:read-only":       "true",
		},
	}
	root.AddCommand(newExplainCascadeCmd())
	return root
}

func newExplainCascadeCmd() *cobra.Command {
	var formatFlag string
	c := &cobra.Command{
		Use:   "cascade",
		Short: "Explain cascade rules (filter validation enforced before wire-send)",
		Long: `Prints the cascade rules enforced by archivist CLI at spec-parse time.

These rules mirror the UX rules in apps/web/lib/filter-cascade.ts.
They prevent quota burn on requests the server will silently return empty results for.

Use --format json to emit the raw cascade-rules.json content.`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0",
			"mcp:read-only":       "true",
		},
		RunE: func(c *cobra.Command, args []string) error {
			if formatFlag == "json" {
				_, err := fmt.Fprint(c.OutOrStdout(), cascade.ExplainRulesJSON())
				return err
			}
			_, err := fmt.Fprint(c.OutOrStdout(), cascade.ExplainRules())
			return err
		},
	}
	c.Flags().StringVar(&formatFlag, "format", "", "Output format: json (default: human-readable text)")
	return c
}
