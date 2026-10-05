//go:build windows

package connect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An npm .cmd shim resolves to the package's native exe (Claude; Codex
// hoisted, nested, or in its own vendor directory); a real .exe passes
// through; a .cmd or .bat shim without a native exe, or any other non .exe
// file (.ps1, .com), is refused.
func TestHarnessLookPathResolvesNpmShims(t *testing.T) {
	prefix := t.TempDir()
	nm := filepath.Join(prefix, "node_modules")
	shim := func(name string) LookPath {
		return func(string) (string, error) { return filepath.Join(prefix, name), nil }
	}

	claudeExe := filepath.Join(nm, "@anthropic-ai", "claude-code", "bin", "claude.exe")
	if _, err := HarnessLookPath(shim("claude.cmd"))("claude"); !errorsAsShim(err, "claude") {
		t.Fatalf("missing native claude: %v", err)
	}
	touch(t, claudeExe)
	if got, err := HarnessLookPath(shim("claude.cmd"))("claude"); err != nil || got != claudeExe {
		t.Fatalf("claude shim: %q %v", got, err)
	}

	rel := filepath.Join("vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")
	for _, native := range []string{
		filepath.Join(nm, "@openai", "codex-win32-x64", rel),
		filepath.Join(nm, "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", rel),
		filepath.Join(nm, "@openai", "codex", rel),
	} {
		if err := os.RemoveAll(filepath.Join(nm, "@openai")); err != nil {
			t.Fatal(err)
		}
		if _, err := HarnessLookPath(shim("codex.CMD"))("codex"); !errorsAsShim(err, "codex") {
			t.Fatalf("missing native codex: %v", err)
		}
		touch(t, native)
		if got, err := HarnessLookPath(shim("codex.CMD"))("codex"); err != nil || got != native {
			t.Fatalf("codex shim (%s): %q %v", native, got, err)
		}
	}

	exe := filepath.Join(prefix, "bin", "claude.exe")
	if got, err := HarnessLookPath(func(string) (string, error) { return exe, nil })("claude"); err != nil || got != exe {
		t.Fatalf("exe: %q %v", got, err)
	}
	// A .bat npm shim resolves like a .cmd one (the native exe exists here).
	if got, err := HarnessLookPath(shim("claude.bat"))("claude"); err != nil || got != claudeExe {
		t.Fatalf("claude.bat: %q %v", got, err)
	}
	// Any other non .exe result is refused.
	for _, name := range []string{"claude.ps1", "claude.com"} {
		if _, err := HarnessLookPath(shim(name))("claude"); !errorsAsShim(err, "claude") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	notFound := errors.New("not found")
	if _, err := HarnessLookPath(func(string) (string, error) { return "", notFound })("claude"); !errors.Is(err, notFound) {
		t.Fatalf("lookup error: %v", err)
	}
}

func errorsAsShim(err error, name string) bool {
	var shim *NativeHarnessError
	return errors.As(err, &shim) && shim.Name == name && strings.Contains(err.Error(), "install the native")
}

// Detection reports a shim without a native exe as a problem naming the
// native installer, never running it.
func TestDetectReportsBatchShim(t *testing.T) {
	prefix := t.TempDir()
	look := HarnessLookPath(func(f string) (string, error) { return filepath.Join(prefix, f+".cmd"), nil })
	ran := false
	run := func(context.Context, []string, string, string, ...string) ([]byte, error) {
		ran = true
		return nil, errors.New("must not run")
	}
	d := DetectWith(context.Background(), DetectOptions{LookPath: look, Run: run, CodexHome: prefix})
	if ran {
		t.Fatal("a shim was executed")
	}
	if d.Claude.Usable() || d.Claude.ProblemCode != "missing" || !strings.Contains(d.Claude.Problem, "install the native Claude Code") {
		t.Fatalf("claude %+v", d.Claude)
	}
	if d.Codex.Usable() || d.Codex.Path == "" || !strings.Contains(d.Codex.Problem, "install the native Codex") {
		t.Fatalf("codex %+v", d.Codex)
	}
	if caps := d.Capabilities(); len(caps) != 1 || caps[0].Agent != "codex" || caps[0].Available {
		t.Fatalf("caps %+v", caps)
	}
}
