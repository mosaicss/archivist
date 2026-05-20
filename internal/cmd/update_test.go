package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestUpdateChannelRoutingBrew verifies that the brew channel prints the upgrade instruction.
func TestUpdateChannelRoutingBrew(t *testing.T) {
	tmpHome := t.TempDir()
	writeChannelFile(t, tmpHome, "brew")
	t.Setenv("HOME", tmpHome)

	cmd := NewUpdateCmd("0.2.0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	// Execute returns nil for exit 0 instructions path
	_ = cmd.Execute()

	if !strings.Contains(out.String(), "brew upgrade mosaic-finance/tap/archivist") {
		t.Errorf("brew channel: expected upgrade instruction in stdout, got: %q", out.String())
	}
}

// TestUpdateChannelRoutingNPM verifies that the npm channel prints the upgrade instruction.
func TestUpdateChannelRoutingNPM(t *testing.T) {
	tmpHome := t.TempDir()
	writeChannelFile(t, tmpHome, "npm")
	t.Setenv("HOME", tmpHome)

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
	t.Setenv("HOME", tmpHome)

	ch := readInstallChannel()
	if ch != "curl-sh" {
		t.Errorf("readInstallChannel() = %q; want %q", ch, "curl-sh")
	}
}

// TestReadInstallChannelMissing verifies that a missing channel file returns "".
func TestReadInstallChannelMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
