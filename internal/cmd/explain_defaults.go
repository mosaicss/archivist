package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/mosaicss/archivist/internal/defaults"
	"github.com/spf13/cobra"
)

func newExplainDefaultsCmd() *cobra.Command {
	var formatFlag string

	c := &cobra.Command{
		Use:   "defaults",
		Short: "Explain the default date window applied to issuer-locked rows",
		Long: `Prints the default date window that archivist fills in automatically when
a row pins an issuer (issuer_key) without specifying date_from / date_to.

Use --format json to emit a structured equivalent with section keys.`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0",
			"mcp:read-only":       "true",
		},
		RunE: func(c *cobra.Command, args []string) error {
			if formatFlag == "json" {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(defaults.ExplainDefaultsJSONSections)
			}
			_, err := fmt.Fprint(c.OutOrStdout(), defaults.ExplainDefaultsText)
			return err
		},
	}
	c.Flags().StringVar(&formatFlag, "format", "", "Output format: json (default: human-readable text)")
	return c
}
