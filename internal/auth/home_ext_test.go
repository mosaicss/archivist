package auth_test

import "testing"

// setHome points the home directory at dir for one test: HOME (Unix) and
// USERPROFILE (what os.UserHomeDir reads on Windows, Story 78.34).
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
