package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillVersionMismatchWarning verifies that version command emits a mismatch warning
// to stderr when the skill version differs from the binary version.
func TestSkillVersionMismatchWarning(t *testing.T) {
	// Set up a fake ~/.claude/skills/archivist/SKILL.md with an older version header
	tmpHome := t.TempDir()
	skillDir := filepath.Join(tmpHome, ".claude", "skills", "archivist")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("<!-- version: 0.1.0 -->\n# Archivist\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmpHome)

	cmd := NewVersionCmd("0.2.0", "abc1234", "2026-05-20")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	// stdout should include binary version and skill info
	out := stdout.String()
	if !strings.Contains(out, "archivist-cli 0.2.0") {
		t.Errorf("stdout missing binary version, got: %q", out)
	}
	if !strings.Contains(out, "outdated") {
		t.Errorf("stdout should contain 'outdated' when skill version mismatches, got: %q", out)
	}

	// stderr should contain the mismatch warning
	errOut := stderr.String()
	if !strings.Contains(errOut, "[skill outdated") {
		t.Errorf("stderr missing skill outdated warning, got: %q", errOut)
	}
	if !strings.Contains(errOut, "archivist update --skill") {
		t.Errorf("stderr should suggest 'archivist update --skill', got: %q", errOut)
	}
}

// TestSkillVersionNoWarningWhenMatching verifies no warning when versions match.
func TestSkillVersionNoWarningWhenMatching(t *testing.T) {
	tmpHome := t.TempDir()
	skillDir := filepath.Join(tmpHome, ".claude", "skills", "archivist")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("<!-- version: 0.2.0 -->\n# Archivist\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmpHome)

	cmd := NewVersionCmd("0.2.0", "abc1234", "2026-05-20")
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	if strings.Contains(stderr.String(), "skill outdated") {
		t.Errorf("expected no skill outdated warning when versions match, got: %q", stderr.String())
	}
}

// TestSkillVersionNoWarningWhenAbsent verifies no warning when skill file is missing.
func TestSkillVersionNoWarningWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewVersionCmd("0.2.0", "abc1234", "2026-05-20")
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr when skill file absent, got: %q", stderr.String())
	}
}

// TestReadSkillVersion verifies parsing of the version header.
func TestReadSkillVersion(t *testing.T) {
	tmpHome := t.TempDir()
	skillDir := filepath.Join(tmpHome, ".claude", "skills", "archivist")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("<!-- version: v1.2.3 -->\nsome content"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmpHome)

	ver, path := readSkillVersion()
	if ver != "v1.2.3" {
		t.Errorf("readSkillVersion() version = %q; want v1.2.3", ver)
	}
	if path == "" {
		t.Error("readSkillVersion() path is empty")
	}
}
