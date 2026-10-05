//go:build !windows

package connect

// codexPlatformConfig adds no -c pairs outside Windows.
var codexPlatformConfig [][2]string

// codexTempEnv points TMPDIR at the session's .tmp inside the cwd.
func codexTempEnv(tmp string) map[string]string {
	return map[string]string{"TMPDIR": tmp}
}
