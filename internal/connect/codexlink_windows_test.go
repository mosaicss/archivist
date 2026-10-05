//go:build windows

package connect

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The session's auth.json is a hard link to the owner's file (same file
// identity, writes visible both ways); a link that is no longer the
// owner's file is replaced by a fresh hard link.
func TestWindowsCodexHardLinkAndRelink(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "owner", "auth.json")
	link := filepath.Join(dir, "session", "auth.json")
	for _, d := range []string{filepath.Dir(owner), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(owner, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkOwnerAuth(owner, link); err != nil {
		t.Fatal(err)
	}
	same := func() bool {
		a, err1 := os.Stat(owner)
		b, err2 := os.Stat(link)
		return err1 == nil && err2 == nil && os.SameFile(a, b)
	}
	if !same() {
		t.Fatal("not one file")
	}
	// An in place write through the session name (a Codex refresh) reaches
	// the owner's file.
	f, err := os.OpenFile(link, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":2}`)
	_ = f.Close()
	if b, _ := os.ReadFile(owner); string(b) != `{"v":2}` {
		t.Fatalf("owner file %s", b)
	}
	// Idempotent.
	if err := linkOwnerAuth(owner, link); err != nil || !same() {
		t.Fatalf("second link: %v", err)
	}
	// The owner logs in again: a new file replaces the old one.
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owner, []byte(`{"v":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if same() {
		t.Fatal("the stale link already names the new file")
	}
	if err := linkOwnerAuth(owner, link); err != nil || !same() {
		t.Fatalf("relink: %v", err)
	}
	if b, _ := os.ReadFile(link); string(b) != `{"v":3}` {
		t.Fatalf("session reads %s", b)
	}
	// A separate file written into the session home: the owner's login wins.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(`{"copy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkOwnerAuth(owner, link); err != nil || !same() {
		t.Fatalf("replace a copy: %v", err)
	}
	if _, err := os.Stat(link + ".link"); !os.IsNotExist(err) {
		t.Fatalf("temporary link left: %v", err)
	}
	if err := linkOwnerAuth(filepath.Join(dir, "missing", "auth.json"), link); err == nil {
		t.Fatal("a missing owner file linked")
	}
}

// A session home on another drive than the owner's Codex home cannot hold
// a hard link: the start fails with the drive message and nothing is
// copied. GitHub's Windows runner keeps TEMP on C: and RUNNER_TEMP on D:;
// without a second drive the test cannot run.
func TestWindowsCodexLinkAcrossDrivesFails(t *testing.T) {
	other := os.Getenv("RUNNER_TEMP")
	ownerDir := t.TempDir()
	if other == "" || strings.EqualFold(filepath.VolumeName(other), filepath.VolumeName(ownerDir)) {
		t.Skip("needs a second drive (RUNNER_TEMP on another volume than TEMP)")
	}
	sessionDir, err := os.MkdirTemp(other, "codexlink-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })
	owner := filepath.Join(ownerDir, "auth.json")
	if err := os.WriteFile(owner, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sessionDir, "auth.json")
	if err := linkOwnerAuth(owner, link); !errors.Is(err, errCodexCrossDrive) {
		t.Fatalf("cross drive link: %v, want %v", err, errCodexCrossDrive)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("something was left at the session name: %v", err)
	}
}
