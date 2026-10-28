// Command oldarchivist is the archivist.exe of a release older than
// `connect --preflight` (Story 94.14) for scripts/test-install.ps1: it
// answers `connect --preflight` the way such a binary does, "Error: unknown
// flag: --preflight" on stderr and exit 2 (logged to STUB_LOG as
// "oldarchivist connect --preflight"), and runs every other command as the
// real archivist.exe at STUB_REAL_ARCHIVIST, with the same arguments,
// standard streams and exit code.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	args := os.Args[1:]
	for _, a := range args {
		if a == "--preflight" {
			logLine("oldarchivist " + strings.Join(args, " "))
			fmt.Fprintln(os.Stderr, "Error: unknown flag: --preflight")
			os.Exit(2)
		}
	}
	c := exec.Command(os.Getenv("STUB_REAL_ARCHIVIST"), args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	case err != nil:
		fmt.Fprintln(os.Stderr, "oldarchivist:", err)
		os.Exit(1)
	}
}

func logLine(s string) {
	f, err := os.OpenFile(os.Getenv("STUB_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, s)
}
