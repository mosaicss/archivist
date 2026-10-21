package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Story 94.3: the browser the pairing confirmation opens.

func TestEnvBrowserTakesTheFirstEntry(t *testing.T) {
	sep := string(filepath.ListSeparator)
	first := filepath.Join(t.TempDir(), "first browser")
	for value, want := range map[string]string{
		"":                                  "",
		sep + sep:                           "",
		first:                               first,
		sep + " " + first + " " + sep + "x": first,
		first + sep + "second":              first,
	} {
		t.Setenv("BROWSER", value)
		if got := envBrowser(); got != want {
			t.Errorf("BROWSER %q: got %q want %q", value, got, want)
		}
	}
	if runtime.GOOS == "windows" {
		// A drive colon is not a list separator on Windows.
		t.Setenv("BROWSER", `C:\Tools\open.exe;D:\other.exe`)
		if got := envBrowser(); got != `C:\Tools\open.exe` {
			t.Errorf("windows list: %q", got)
		}
	} else {
		t.Setenv("BROWSER", "/usr/bin/first:/usr/bin/second")
		if got := envBrowser(); got != "/usr/bin/first" {
			t.Errorf("colon list: %q", got)
		}
	}
}

func TestBrowserCommand(t *testing.T) {
	const url = "https://workspace.mosaic-finance.com/connect/confirm#abc"
	program := filepath.Join(t.TempDir(), "browser")
	t.Setenv("BROWSER", program)
	cmd, err := browserCommand(url)
	if err != nil || strings.Join(cmd.Args, "|") != program+"|"+url {
		t.Fatalf("BROWSER: %v %v", cmd, err)
	}
	t.Setenv("BROWSER", "")
	cmd, err = browserCommand(url)
	want := map[string]string{
		"darwin":  "open|" + url,
		"linux":   "xdg-open|" + url,
		"windows": "cmd|/c|start|" + url,
	}[runtime.GOOS]
	if want == "" {
		if err == nil {
			t.Fatalf("unsupported OS opened %v", cmd.Args)
		}
		return
	}
	if err != nil || strings.Join(cmd.Args, "|") != want {
		t.Fatalf("system opener: %v %v", cmd, err)
	}
}

func TestBrowserAvailable(t *testing.T) {
	t.Setenv("BROWSER", filepath.Join(t.TempDir(), "browser"))
	if !browserAvailable() {
		t.Fatal("BROWSER set: not available")
	}
	t.Setenv("BROWSER", "")
	if runtime.GOOS != "linux" {
		if (runtime.GOOS == "darwin" || runtime.GOOS == "windows") && !browserAvailable() {
			t.Fatal("macOS or Windows: not available")
		}
		return
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if browserAvailable() {
		t.Fatal("no display: available")
	}
	t.Setenv("DISPLAY", ":0")
	if browserAvailable() {
		t.Fatal("no xdg-open on PATH: available")
	}
	if err := os.WriteFile(filepath.Join(bin, "xdg-open"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !browserAvailable() {
		t.Fatal("DISPLAY and xdg-open: not available")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if !browserAvailable() {
		t.Fatal("WAYLAND_DISPLAY and xdg-open: not available")
	}
}
