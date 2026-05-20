package cmd

import "fmt"

// ExitError signals main.go to exit with a specific code without re-printing
// the error via Cobra's default machinery. Verbs print user-facing diagnostics
// to cmd.ErrOrStderr() themselves before returning this error.
//
// Construct with &ExitError{Code: ExitAuthError} (etc.) from verb RunE
// implementations.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit code %d", e.Code)
}
