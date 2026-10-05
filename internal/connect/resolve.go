package connect

import "fmt"

// NativeHarnessError means the harness found on PATH is a script shim (an
// npm .cmd or .bat on Windows) with no native executable archivist could
// run instead; archivist never runs a batch file (Story 78.34).
type NativeHarnessError struct {
	// Name is the harness command ("claude" or "codex").
	Name string
	// Shim is the script found on PATH.
	Shim string
}

func (e *NativeHarnessError) Error() string {
	switch e.Name {
	case "claude":
		return fmt.Sprintf("Claude Code on PATH is a script (%s) and its native claude.exe was not found; install the native Claude Code (in PowerShell: irm https://claude.ai/install.ps1 | iex)", e.Shim)
	case "codex":
		return fmt.Sprintf("Codex on PATH is a script (%s) and its native codex.exe was not found; install the native Codex (or reinstall it with npm install -g @openai/codex)", e.Shim)
	}
	return fmt.Sprintf("%s on PATH is a script (%s), not a native executable", e.Name, e.Shim)
}
