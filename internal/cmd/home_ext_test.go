package cmd_test

import (
	"runtime"
	"testing"
)

// setHome points the home directory at dir for one test: HOME (Unix) and
// USERPROFILE (what os.UserHomeDir reads on Windows, Story 78.34).
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// skipShellStubs skips a test whose harness stand-ins are /bin/sh scripts,
// which Windows cannot run (the Windows flows are covered with .exe fakes in
// internal/connect and stub runners here).
func skipShellStubs(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix only: the harness stand-ins are shell scripts")
	}
}
