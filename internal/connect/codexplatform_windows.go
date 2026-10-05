//go:build windows

package connect

// codexPlatformConfig are the Windows -c pairs (Story 78.34): the
// unelevated Windows sandbox (a restricted token, no admin rights and no
// sandbox service users that could leave the session's job). With
// windows.sandbox unset Codex silently turns workspace write into read
// only, and the elevated mode is never selected.
var codexPlatformConfig = [][2]string{
	{"windows.sandbox", `"unelevated"`},
}

// codexTempEnv points the session's temp directory variables at its .tmp
// inside the cwd (the sandbox's writable root): TMPDIR as on Unix, plus
// TEMP and TMP, which Windows programs read.
func codexTempEnv(tmp string) map[string]string {
	return map[string]string{"TMPDIR": tmp, "TEMP": tmp, "TMP": tmp}
}
