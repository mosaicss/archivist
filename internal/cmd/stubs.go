package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// stubMeta describes the Cobra command shape for a verb whose behavior
// lands in a later story. Each stub returns ExitNotImplemented (9) with a
// stderr message naming the future story so agent loops can self-correct
// without retrying the verb.
type stubMeta struct {
	name       string // verb as it appears in user-facing copy, e.g. "chat"
	use        string // Cobra Use string, e.g. "chat <question>"
	short      string // Cobra Short description (the "(not yet implemented)" suffix is appended here)
	story      string // future story that ships the verb, e.g. "36.3"
	typedCodes string // comma-separated exit codes the verb will emit when shipped
	readOnly   bool   // true if the future verb never mutates server state
}

func newStubCmd(meta stubMeta) *cobra.Command {
	annotations := map[string]string{
		"pp:typed-exit-codes": meta.typedCodes,
	}
	if meta.readOnly {
		annotations["mcp:read-only"] = "true"
	}
	return &cobra.Command{
		Use:         meta.use,
		Short:       meta.short + " (not yet implemented)",
		Args:        cobra.ArbitraryArgs,
		Annotations: annotations,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"archivist %s: not yet implemented (lands in Story %s)\n",
				meta.name, meta.story)
			return &ExitError{Code: ExitNotImplemented}
		},
	}
}
