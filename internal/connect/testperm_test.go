package connect

import (
	"os"
	"runtime"
	"testing"
)

// modeIs reports whether mode's permission bits are want. Windows has no
// Unix modes (Go reports 0666 or 0444), so it always holds there.
func modeIs(mode os.FileMode, want os.FileMode) bool {
	return runtime.GOOS == "windows" || mode.Perm() == want
}

// skipOnWindows skips a test whose subject is a Unix mechanism (a PTY,
// Unix modes, Unix path literals) with the reason (Story 78.34).
func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix only: " + why)
	}
}

// noPTY is the reason Claude's sign-in tests skip on Windows.
const noPTY = "Claude Code's sign-in runs on a pseudo terminal (the Linux sandboxes)"
