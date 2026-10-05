//go:build windows

package connect

import (
	"os"
	"path/filepath"
	"strings"
)

// HarnessLookPath wraps look so a harness resolves only to a real .exe
// (Story 78.34). An npm .cmd or .bat shim found on PATH is resolved to the
// package's native executable; any other non .exe result fails with a
// *NativeHarnessError naming the native installer. A batch file is never
// returned, so it is never executed.
func HarnessLookPath(look LookPath) LookPath {
	return func(file string) (string, error) {
		p, err := look(file)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".exe":
			return p, nil
		case ".cmd", ".bat":
			for _, c := range npmNativeCandidates(file, filepath.Dir(p)) {
				if isRegularFile(c) {
					return c, nil
				}
			}
		}
		return "", &NativeHarnessError{Name: file, Shim: p}
	}
}

// npmNativeCandidates are the native executables an npm global install
// keeps beside its shims in dir (the npm prefix):
//   - Claude Code: node_modules\@anthropic-ai\claude-code\bin\claude.exe
//     (the package's bin entry since the native builds);
//   - Codex: the platform package's vendor\<triple>\bin\codex.exe, hoisted
//     to node_modules\@openai\codex-win32-<arch> or nested under
//     node_modules\@openai\codex\node_modules, or the package's own vendor
//     directory (bin/codex.js resolves the same places).
func npmNativeCandidates(name, dir string) []string {
	nm := filepath.Join(dir, "node_modules")
	switch name {
	case "claude":
		return []string{filepath.Join(nm, "@anthropic-ai", "claude-code", "bin", "claude.exe")}
	case "codex":
		var out []string
		for _, arch := range [][2]string{{"x64", "x86_64-pc-windows-msvc"}, {"arm64", "aarch64-pc-windows-msvc"}} {
			rel := filepath.Join("vendor", arch[1], "bin", "codex.exe")
			out = append(out,
				filepath.Join(nm, "@openai", "codex-win32-"+arch[0], rel),
				filepath.Join(nm, "@openai", "codex", "node_modules", "@openai", "codex-win32-"+arch[0], rel),
				filepath.Join(nm, "@openai", "codex", rel),
			)
		}
		return out
	}
	return nil
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
