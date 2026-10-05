package cmd

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/service"
	"github.com/spf13/cobra"
)

// TestUpdateChannelRoutingBrew verifies that the brew channel prints the upgrade instruction.
func TestUpdateChannelRoutingBrew(t *testing.T) {
	tmpHome := t.TempDir()
	writeChannelFile(t, tmpHome, "brew")
	setHome(t, tmpHome)

	cmd := NewUpdateCmd("0.2.0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	// Execute returns nil for exit 0 instructions path
	_ = cmd.Execute()

	if !strings.Contains(out.String(), "brew upgrade mosaic-finance-inc/tap/archivist") {
		t.Errorf("brew channel: expected upgrade instruction in stdout, got: %q", out.String())
	}
}

// TestUpdateChannelRoutingNPM verifies that the npm channel prints the upgrade instruction.
func TestUpdateChannelRoutingNPM(t *testing.T) {
	tmpHome := t.TempDir()
	writeChannelFile(t, tmpHome, "npm")
	setHome(t, tmpHome)

	cmd := NewUpdateCmd("0.2.0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	_ = cmd.Execute()

	if !strings.Contains(out.String(), "npx -y @mosaic-finance/archivist@latest install") {
		t.Errorf("npm channel: expected upgrade instruction in stdout, got: %q", out.String())
	}
}

// TestUpdateFlagsRegistered verifies --skill and --check flags are wired.
func TestUpdateFlagsRegistered(t *testing.T) {
	cmd := NewUpdateCmd("0.2.0")
	if f := cmd.Flags().Lookup("skill"); f == nil {
		t.Error("--skill flag not registered on update command")
	}
	if f := cmd.Flags().Lookup("check"); f == nil {
		t.Error("--check flag not registered on update command")
	}
}

// TestUpdateAnnotations verifies exit-code annotations and no mcp:read-only.
func TestUpdateAnnotations(t *testing.T) {
	cmd := NewUpdateCmd("0.2.0")
	codes, ok := cmd.Annotations["pp:typed-exit-codes"]
	if !ok {
		t.Fatal("pp:typed-exit-codes annotation missing on update command")
	}
	for _, code := range []string{"0", "3", "5"} {
		if !strings.Contains(codes, code) {
			t.Errorf("pp:typed-exit-codes %q missing code %s", codes, code)
		}
	}
	if _, ok := cmd.Annotations["mcp:read-only"]; ok {
		t.Error("mcp:read-only must NOT be set on update (it mutates the binary)")
	}
}

// TestUpdateRegisteredInRoot verifies the real update command (not a stub) is in the root.
func TestUpdateRegisteredInRoot(t *testing.T) {
	root := &cobra.Command{Use: "archivist"}
	root.AddCommand(NewUpdateCmd("0.2.0"))
	c, _, err := root.Find([]string{"update"})
	if err != nil || c == nil {
		t.Fatal("update command not found in root cobra tree")
	}
	if strings.Contains(c.Short, "not yet implemented") {
		t.Error("update command is still a stub — should be the real implementation")
	}
}

// TestReadInstallChannel verifies that the channel value is read from the file.
func TestReadInstallChannel(t *testing.T) {
	tmpHome := t.TempDir()
	writeChannelFile(t, tmpHome, "curl-sh")
	setHome(t, tmpHome)

	ch := readInstallChannel()
	if ch != "curl-sh" {
		t.Errorf("readInstallChannel() = %q; want %q", ch, "curl-sh")
	}
}

// TestReadInstallChannelMissing verifies that a missing channel file returns "".
func TestReadInstallChannelMissing(t *testing.T) {
	setHome(t, t.TempDir())
	ch := readInstallChannel()
	if ch != "" {
		t.Errorf("readInstallChannel() with no file = %q; want empty string", ch)
	}
}

// TestExtractHashFromSums verifies the SHA256SUMS parser.
func TestExtractHashFromSums(t *testing.T) {
	sums := []byte(
		"abc123  archivist_v0.2.0_darwin_arm64.tar.gz\n" +
			"def456  archivist_v0.2.0_linux_amd64.tar.gz\n",
	)
	hash, err := extractHashFromSums(sums, "archivist_v0.2.0_darwin_arm64.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != "abc123" {
		t.Errorf("got %q; want abc123", hash)
	}
}

// TestExtractHashFromSumsMissing verifies that a missing entry returns an error.
func TestExtractHashFromSumsMissing(t *testing.T) {
	sums := []byte("abc123  somefile.tar.gz\n")
	_, err := extractHashFromSums(sums, "missing.tar.gz")
	if err == nil {
		t.Error("expected error for missing file in sums, got nil")
	}
}

// writeChannelFile is a test helper that creates the install-channel file.
func writeChannelFile(t *testing.T, home, channel string) {
	t.Helper()
	dir := filepath.Join(home, ".archivist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install-channel"), []byte(channel), 0o644); err != nil {
		t.Fatal(err)
	}
}

// macOS downloads the universal archive; there is no per-arch darwin asset.
func TestReleasePlatform(t *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "arm64"}:  "darwin_all",
		{"darwin", "amd64"}:  "darwin_all",
		{"linux", "amd64"}:   "linux_amd64",
		{"linux", "arm64"}:   "linux_arm64",
		{"windows", "amd64"}: "windows_amd64",
	}
	for in, want := range cases {
		if got := releasePlatform(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}

// After a self update, an installed background service gets a one line
// hint to restart it on the new binary; no unit or plist, no hint.
func TestServiceRestartHint(t *testing.T) {
	home := t.TempDir()
	if hint := serviceRestartHint(home, ""); hint != "" {
		t.Fatalf("hint without a service: %q", hint)
	}
	path, ok := service.InstalledPath(home, "")
	if ok || path == "" {
		t.Fatalf("InstalledPath before install: %q %v", path, ok)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	hint := serviceRestartHint(home, "relative/ignored")
	if !strings.Contains(hint, "archivist connect --install") || strings.Contains(hint, "\n") {
		t.Fatalf("hint %q", hint)
	}
	// An absolute XDG_CONFIG_HOME moves the Linux unit; the hint follows it.
	if runtime.GOOS == "linux" {
		xdg := t.TempDir()
		if hint := serviceRestartHint(home, xdg); hint != "" {
			t.Fatalf("hint for a unit outside XDG_CONFIG_HOME: %q", hint)
		}
	}
}

// Story 78.34: the Windows update renames each running exe to a unique
// .old-<id> name and copies the new one in, removing stale .old-* files.
func TestSwapBinaries(t *testing.T) {
	dir, src := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "archivist.exe"), "old")
	write(filepath.Join(dir, "archivist.exe.old-1-2"), "stale")
	write(filepath.Join(dir, "archivistw.exe.old-3-4"), "stale")
	// Other programs' files in a shared directory are never touched.
	write(filepath.Join(dir, "tool.exe.old-1-2"), "keep")
	write(filepath.Join(dir, "notes.old-draft"), "keep")
	write(filepath.Join(src, "a"), "new")
	write(filepath.Join(src, "w"), "neww")
	if err := swapBinaries(dir, map[string]string{"archivist.exe": filepath.Join(src, "a"), "archivistw.exe": filepath.Join(src, "w")}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"archivist.exe": "new", "archivistw.exe": "neww"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	for _, keep := range []string{"tool.exe.old-1-2", "notes.old-draft"} {
		if b, err := os.ReadFile(filepath.Join(dir, keep)); err != nil || string(b) != "keep" {
			t.Fatalf("%s touched: %q %v", keep, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "archivistw.exe.old-3-4")); !os.IsNotExist(err) {
		t.Fatalf("stale archivistw.exe.old-3-4 kept: %v", err)
	}
	olds, _ := filepath.Glob(filepath.Join(dir, "archivist*.exe.old-*"))
	if len(olds) != 1 || strings.HasSuffix(olds[0], ".old-1-2") {
		t.Fatalf("old files %v", olds)
	}
	if b, _ := os.ReadFile(olds[0]); string(b) != "old" {
		t.Fatalf("renamed file holds %q", b)
	}
	// A failed copy puts the renamed exe back.
	if err := swapBinaries(dir, map[string]string{"archivist.exe": filepath.Join(src, "missing")}); err == nil {
		t.Fatal("a missing source swapped")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "archivist.exe")); string(b) != "new" {
		t.Fatalf("restore failed: %q", b)
	}
}

// writeZip writes a zip holding files (name -> content).
func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// Story 78.34: the Windows zip's executables are staged for the running
// executable's own name and archivistw.exe, then swapped in; an archive
// without archivistw.exe updates archivist only, and one without
// archivist.exe fails.
func TestStageWindowsBinaries(t *testing.T) {
	src := t.TempDir()
	both := filepath.Join(src, "both.zip")
	writeZip(t, both, map[string]string{"LICENSE": "l", "archivist.exe": "new-main", "archivistw.exe": "new-w"})
	dir := t.TempDir()
	for name, body := range map[string]string{"archivist2.exe": "old-main", "archivistw.exe": "old-w", "archivist2.exe.new": "legacy"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := stageWindowsBinaries(both, "archivist2.exe", t.TempDir())
	if err != nil || len(staged) != 2 {
		t.Fatalf("staged %v %v", staged, err)
	}
	if err := swapBinaries(dir, staged); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"archivist2.exe": "new-main", "archivistw.exe": "new-w"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "archivist.exe")); !os.IsNotExist(err) {
		t.Fatalf("an archivist.exe was written beside the renamed binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "archivist2.exe.new")); !os.IsNotExist(err) {
		t.Fatalf("legacy .new kept: %v", err)
	}

	only := filepath.Join(src, "only.zip")
	writeZip(t, only, map[string]string{"archivist.exe": "only-main"})
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "archivistw.exe"), []byte("old-w"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, err = stageWindowsBinaries(only, "archivist.exe", t.TempDir())
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged %v %v", staged, err)
	}
	if err := swapBinaries(dir2, staged); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, "archivist.exe")); string(b) != "only-main" {
		t.Fatalf("archivist.exe %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, "archivistw.exe")); string(b) != "old-w" {
		t.Fatalf("archivistw.exe changed without one in the archive: %q", b)
	}

	// Run from archivistw.exe: archivist.exe takes the primary, archivistw.exe
	// the companion (never the primary's content).
	dir3 := t.TempDir()
	for name, body := range map[string]string{"archivist.exe": "old-main", "archivistw.exe": "old-w"} {
		if err := os.WriteFile(filepath.Join(dir3, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	staged, err = stageWindowsBinaries(both, "ArchivistW.exe", t.TempDir())
	if err != nil || len(staged) != 2 {
		t.Fatalf("staged from archivistw.exe %v %v", staged, err)
	}
	if err := swapBinaries(dir3, staged); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"archivist.exe": "new-main", "archivistw.exe": "new-w"} {
		if b, err := os.ReadFile(filepath.Join(dir3, name)); err != nil || string(b) != want {
			t.Fatalf("from archivistw.exe, %s: %q %v", name, b, err)
		}
	}

	none := filepath.Join(src, "none.zip")
	writeZip(t, none, map[string]string{"archivistw.exe": "w"})
	if _, err := stageWindowsBinaries(none, "archivist.exe", t.TempDir()); err == nil {
		t.Fatal("an archive without archivist.exe staged")
	}
}
