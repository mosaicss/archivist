package cmd

// output_helpers.go hosts the output helpers every verb shares: TTY
// detection for the --format default and the JSON error envelope.

import (
	"encoding/json"
	"io"
	"os"
)

// isTerminal returns true if w is an *os.File wrapping a real TTY.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// resolveFormat returns the explicit --format value, or json off a TTY and
// fallback on one.
func resolveFormat(explicit string, stdout io.Writer, fallback string) string {
	if explicit != "" {
		return explicit
	}
	if !isTerminal(stdout) {
		return "json"
	}
	return fallback
}

// jsonError is the stdout envelope a failing verb writes in JSON mode.
type jsonError struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	ExitCode   int    `json:"exit_code"`
	Suggestion string `json:"suggestion,omitempty"`
	AccountURL string `json:"account_url,omitempty"`
	ResetDate  string `json:"reset_date,omitempty"`
}

// writeJSONError writes a JSON error envelope to w.
func writeJSONError(w io.Writer, e jsonError) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(e)
}
