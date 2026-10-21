//go:build !windows

package connect

// codexPlatformConfig adds no -c pairs outside Windows.
var codexPlatformConfig [][2]string

// codexLegacySandbox is false outside Windows (Story 94.5): sessions run
// in the archivist permission profiles (codexProfileConfig), selected with
// permissions on thread/start, thread/resume and turn/start.
const codexLegacySandbox = false

// codexTempEnv points TMPDIR at the session's .tmp inside the cwd.
func codexTempEnv(tmp string) map[string]string {
	return map[string]string{"TMPDIR": tmp}
}
