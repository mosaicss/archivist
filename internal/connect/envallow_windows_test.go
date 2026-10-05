//go:build windows

package connect

import (
	"strings"
	"testing"
)

// Windows keys compare case-insensitively: a lowercase provider key is
// denied, Path and the core keys pass under any case and keep the parent's
// spelling, and an override replaces the parent's entry under its name.
func TestWindowsEnvCaseInsensitive(t *testing.T) {
	for _, k := range []string{"anthropic_api_key", "Anthropic_Api_Key", "openai_api_key", "claude_code_oauth_token", "Archivist_Token", "codex_home", "herdr_env"} {
		if EnvAllowed(k) {
			t.Errorf("%s allowed", k)
		}
	}
	for _, k := range []string{"Path", "PATH", "path", "SystemRoot", "SYSTEMROOT", "ComSpec", "windir", "ProgramFiles(x86)", "LocalAppData", "PSModulePath", "TEMP", "Tmp", "USERPROFILE", "lc_all"} {
		if !EnvAllowed(k) {
			t.Errorf("%s denied", k)
		}
	}
	env, err := BuildChildEnv([]string{
		"Path=C:\\Windows", "SystemRoot=C:\\Windows", "anthropic_api_key=sk-x", "Temp=C:\\t", "=C:=C:\\",
		"claude_config_dir=relative", "OneDrive=C:\\o",
	}, map[string]string{"TEMP": "C:\\s\\.tmp", "TMPDIR": "C:\\s\\.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(env, "|")
	want := `Path=C:\Windows|SystemRoot=C:\Windows|TMPDIR=C:\s\.tmp|Temp=C:\s\.tmp`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := BuildChildEnv(nil, map[string]string{"anthropic_base_url": "x"}); err == nil {
		t.Fatal("a lowercase denied override was accepted")
	}
}

// Codex commands see the Windows core keys and run in the unelevated sandbox.
func TestWindowsCodexArgs(t *testing.T) {
	args := strings.Join(codexArgs(CodexConfig{Bin: `C:\codex.exe`, Executable: `C:\archivist.exe`}, `C:\t`, nil), "\n")
	if !strings.Contains(args, `windows.sandbox="unelevated"`) || strings.Contains(args, `windows.sandbox="elevated"`) {
		t.Fatalf("no unelevated sandbox:\n%s", args)
	}
	for _, k := range []string{`"SystemRoot"`, `"ComSpec"`, `"PATHEXT"`, `"USERPROFILE"`, `"TEMP"`} {
		if !strings.Contains(args, k) {
			t.Errorf("include_only lacks %s", k)
		}
	}
	if env := codexTempEnv(`C:\c\.tmp`); env["TEMP"] != `C:\c\.tmp` || env["TMP"] != `C:\c\.tmp` || env["TMPDIR"] != `C:\c\.tmp` {
		t.Fatalf("temp env %v", env)
	}
}
