package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mosaicss/archivist/internal/release"
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

// Any channel value other than brew self-updates, an old "npm" value
// included, and prints no other upgrade command.
func TestUpdateChannelNPMSelfUpdates(t *testing.T) {
	tmpHome := t.TempDir()
	writeChannelFile(t, tmpHome, "npm")
	setHome(t, tmpHome)
	rel := newFakeRelease(t, "v9.9.9", releaseGood)
	_, stdout, _, code := rel.run(t, "9.9.9")
	if code != ExitNotFound || !strings.Contains(stdout, "Already up to date: v9.9.9") || strings.Contains(stdout, "npx") {
		t.Fatalf("npm channel: code %d, stdout %q", code, stdout)
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

// --- end to end: a local release server signed by a test key ---------------

var (
	updateTestKeysOnce sync.Once
	updateTestKey      *rsa.PrivateKey
	updateOtherKey     *rsa.PrivateKey
)

// updateTestKeys returns the key the fake releases are trusted under and
// one that is not trusted (3072 bit throwaway keys, made once per run).
func updateTestKeys() (*rsa.PrivateKey, *rsa.PrivateKey) {
	updateTestKeysOnce.Do(func() {
		var err error
		if updateTestKey, err = rsa.GenerateKey(rand.Reader, 3072); err != nil {
			panic(err)
		}
		if updateOtherKey, err = rsa.GenerateKey(rand.Reader, 3072); err != nil {
			panic(err)
		}
	})
	return updateTestKey, updateOtherKey
}

func signSums(t *testing.T, key *rsa.PrivateKey, sums []byte) []byte {
	t.Helper()
	d := sha256.Sum256(sums)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

type releaseMode int

const (
	releaseGood              releaseMode = iota
	releaseArchiveMismatch               // the archive does not match SHA256SUMS
	releaseUnsignedSums                  // SHA256SUMS (matching the archive) differs from what was signed
	releaseNoSig                         // no SHA256SUMS.sig asset
	releaseOtherKey                      // signed by a key that is not trusted
	releaseSkillMismatch                 // the skill bundle does not match SHA256SUMS
	releaseSkillUnsignedSums             // SHA256SUMS (matching the bundle) differs from what was signed
)

// newBinary is the released "binary": a script on POSIX that prints a
// marker, so a test can run the replaced file.
const newBinary = "#!/bin/sh\necho archivist-new-binary-ran\n"

type fakeRelease struct {
	tag      string
	files    map[string][]byte
	mu       sync.Mutex
	requests []string
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(files[n]))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, body := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sumsFor(files map[string][]byte, names ...string) []byte {
	var b strings.Builder
	for _, n := range names {
		h := sha256.Sum256(files[n])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	return []byte(b.String())
}

// releaseArchive is this platform's archive of the released binary, holding
// body plus the extra files.
func releaseArchive(t *testing.T, ver, body string, extra map[string]string) (string, []byte) {
	t.Helper()
	files := map[string]string{"LICENSE": "license"}
	for k, v := range extra {
		files[k] = v
	}
	platform := releasePlatform(runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		files["archivist.exe"] = body
		return fmt.Sprintf("archivist_v%s_%s.zip", ver, platform), zipBytes(t, files)
	}
	files["archivist"] = body
	return fmt.Sprintf("archivist_v%s_%s.tar.gz", ver, platform), tarGz(t, files)
}

// newFakeRelease serves release tag (mode decides what is wrong with it) on
// 127.0.0.1 and points update's release URLs and verifier at it.
func newFakeRelease(t *testing.T, tag string, mode releaseMode) *fakeRelease {
	t.Helper()
	trusted, other := updateTestKeys()
	ver := strings.TrimPrefix(tag, "v")
	archive, archiveBody := releaseArchive(t, ver, newBinary, nil)
	skill := fmt.Sprintf("archivist_v%s_skill-bundle.tar.gz", ver)
	sumsName := fmt.Sprintf("archivist_v%s_SHA256SUMS", ver)
	r := &fakeRelease{tag: tag, files: map[string][]byte{
		archive: archiveBody,
		skill:   tarGz(t, map[string]string{"SKILL.md": "---\nname: archivist\n---\n"}),
	}}
	r.files[sumsName] = sumsFor(r.files, archive, skill)
	key := trusted
	if mode == releaseOtherKey {
		key = other
	}
	if mode != releaseNoSig {
		r.files[sumsName+".sig"] = signSums(t, key, r.files[sumsName])
	}
	switch mode {
	case releaseArchiveMismatch:
		r.files[archive] = append(append([]byte(nil), r.files[archive]...), "changed"...)
	case releaseSkillMismatch:
		r.files[skill] = append(append([]byte(nil), r.files[skill]...), "changed"...)
	case releaseUnsignedSums:
		_, r.files[archive] = releaseArchive(t, ver, newBinary, map[string]string{"EXTRA": "extra"})
		r.files[sumsName] = sumsFor(r.files, archive, skill)
	case releaseSkillUnsignedSums:
		r.files[skill] = tarGz(t, map[string]string{"SKILL.md": "---\nname: archivist\n---\nother\n"})
		r.files[sumsName] = sumsFor(r.files, archive, skill)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, req.URL.Path)
		r.mu.Unlock()
		if req.URL.Path == "/api/latest" {
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": r.tag})
			return
		}
		name, ok := strings.CutPrefix(req.URL.Path, "/download/"+r.tag+"/")
		body, found := r.files[name]
		if !ok || !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	verifier, err := release.NewVerifier(pemPublicKey(t, &trusted.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	oldAPI, oldBase, oldVerify, oldExe := releaseAPIURL, releaseDownloadBase, releaseVerifier, executablePath
	t.Cleanup(func() {
		releaseAPIURL, releaseDownloadBase, releaseVerifier, executablePath = oldAPI, oldBase, oldVerify, oldExe
	})
	releaseAPIURL = srv.URL + "/api/latest"
	releaseDownloadBase = srv.URL + "/download"
	releaseVerifier = verifier.Verify
	return r
}

func pemPublicKey(t *testing.T, key *rsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// downloads lists the asset paths the server was asked for.
func (r *fakeRelease) downloads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.requests {
		if strings.HasPrefix(p, "/download/") {
			out = append(out, p)
		}
	}
	return out
}

// run runs `archivist update args...` as build version current against a
// stand-in installed binary (content "old"); it returns that binary's path,
// stdout, stderr and the exit code.
func (r *fakeRelease) run(t *testing.T, current string, args ...string) (string, string, string, int) {
	t.Helper()
	name := "archivist"
	if runtime.GOOS == "windows" {
		name = "archivist.exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	executablePath = func() (string, error) { return bin, nil }
	cmd := NewUpdateCmd(current)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{}, args...))
	err := cmd.Execute()
	code := 0
	if err != nil {
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("update: %v", err)
		}
		code = exitErr.Code
	}
	return bin, stdout.String(), stderr.String(), code
}

func assertUnchanged(t *testing.T, bin string) {
	t.Helper()
	if b, err := os.ReadFile(bin); err != nil || string(b) != "old" {
		t.Fatalf("the installed binary changed: %q %v", b, err)
	}
	if _, err := os.Stat(bin + ".new"); !os.IsNotExist(err) {
		t.Fatalf("a staged binary was left: %v", err)
	}
}

// A genuine newer release replaces the binary with an executable file that
// runs (POSIX; Windows renames and copies, covered by TestSwapBinaries).
func TestUpdateReplacesBinaryAndItRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX self update")
	}
	setHome(t, t.TempDir())
	for _, current := range []string{"0.2.0", "dev", "0.2.35-snapshot"} {
		rel := newFakeRelease(t, "v9.9.9", releaseGood)
		bin, stdout, stderr, code := rel.run(t, current)
		if code != 0 {
			t.Fatalf("%s: exit %d\nstdout %s\nstderr %s", current, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "Updated from v"+strings.TrimPrefix(current, "v")+" to v9.9.9") {
			t.Fatalf("%s: stdout %q", current, stdout)
		}
		info, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: replaced binary mode %v, want 0755", current, info.Mode().Perm())
		}
		out, err := exec.Command(bin).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "archivist-new-binary-ran" {
			t.Fatalf("%s: the replaced binary does not run: %q %v", current, out, err)
		}
		if _, err := os.Stat(bin + ".new"); !os.IsNotExist(err) {
			t.Fatalf("%s: staged .new left: %v", current, err)
		}
	}
}

// Every release whose checksums are not signed by a trusted key, or whose
// archive does not match them, is refused before anything is replaced.
func TestUpdateRefusesUnverifiedReleases(t *testing.T) {
	setHome(t, t.TempDir())
	for name, c := range map[string]struct {
		mode releaseMode
		want string
	}{
		"checksums without a valid signature": {releaseUnsignedSums, "could not be verified"},
		"missing signature":                   {releaseNoSig, "SHA256SUMS signature"},
		"signed by another key":               {releaseOtherKey, "could not be verified"},
		"an archive that does not match":      {releaseArchiveMismatch, "SHA256 mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			rel := newFakeRelease(t, "v9.9.9", c.mode)
			bin, stdout, stderr, code := rel.run(t, "0.2.0")
			if code == 0 || !strings.Contains(stderr, c.want) {
				t.Fatalf("exit %d, want refusal with %q\nstdout %s\nstderr %s", code, c.want, stdout, stderr)
			}
			assertUnchanged(t, bin)
			if c.mode != releaseArchiveMismatch {
				for _, p := range rel.downloads() {
					if strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".zip") {
						t.Fatalf("the archive was downloaded before the checksums verified: %s", p)
					}
				}
			}
		})
	}
}

// The latest release older than this build: nothing is downloaded or
// replaced, and --check does not suggest updating.
func TestUpdateRefusesDowngrade(t *testing.T) {
	setHome(t, t.TempDir())
	rel := newFakeRelease(t, "v9.9.8", releaseGood)
	bin, stdout, stderr, code := rel.run(t, "9.9.9")
	if code == 0 || !strings.Contains(stderr, "newer than the latest release (v9.9.8)") {
		t.Fatalf("downgrade: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	assertUnchanged(t, bin)
	if d := rel.downloads(); len(d) != 0 {
		t.Fatalf("downgrade downloaded %v", d)
	}
	// A prerelease of the next version is newer than the release before it.
	_, _, stderr, code = rel.run(t, "9.9.9-rc.1")
	if code == 0 || !strings.Contains(stderr, "newer") {
		t.Fatalf("prerelease build: exit %d, stderr %s", code, stderr)
	}

	_, stdout, _, code = rel.run(t, "9.9.9", "--check")
	if code != ExitNotFound || strings.Contains(stdout, "run 'archivist update'") || !strings.Contains(stdout, "newer than the latest release") {
		t.Fatalf("--check on a newer build: exit %d, stdout %q", code, stdout)
	}
	_, stdout, _, code = rel.run(t, "9.9.7", "--check")
	if code != 0 || !strings.Contains(stdout, "run 'archivist update'") {
		t.Fatalf("--check on an older build: exit %d, stdout %q", code, stdout)
	}

	same := newFakeRelease(t, "v9.9.9", releaseGood)
	bin, stdout, _, code = same.run(t, "v9.9.9")
	if code != ExitNotFound || !strings.Contains(stdout, "Already up to date: v9.9.9") {
		t.Fatalf("same version: exit %d, stdout %q", code, stdout)
	}
	assertUnchanged(t, bin)
}

// A latest tag that is not a release tag is refused.
func TestUpdateRefusesMalformedTag(t *testing.T) {
	setHome(t, t.TempDir())
	for _, tag := range []string{"latest", "v1.2", "v1.2.3/../../x", "1.2.3"} {
		rel := newFakeRelease(t, "v9.9.9", releaseGood)
		rel.tag = tag
		bin, _, stderr, code := rel.run(t, "0.2.0")
		if code == 0 || !strings.Contains(stderr, "unexpected release tag") {
			t.Fatalf("tag %q: exit %d, stderr %s", tag, code, stderr)
		}
		assertUnchanged(t, bin)
	}
}

// update --skill unpacks the bundle only when the signed SHA256SUMS
// verifies and the bundle matches it.
func TestUpdateSkillVerifiesTheBundle(t *testing.T) {
	for name, c := range map[string]struct {
		mode releaseMode
		want string
	}{
		"good":                                {releaseGood, ""},
		"a bundle that does not match":        {releaseSkillMismatch, "SHA256 mismatch"},
		"checksums without a valid signature": {releaseSkillUnsignedSums, "could not be verified"},
		"missing signature":                   {releaseNoSig, "SHA256SUMS signature"},
		"signed by another key":               {releaseOtherKey, "could not be verified"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			setHome(t, home)
			if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
				t.Fatal(err)
			}
			rel := newFakeRelease(t, "v9.9.9", c.mode)
			_, stdout, stderr, code := rel.run(t, "0.2.0", "--skill")
			skill := filepath.Join(home, ".claude", "skills", "archivist", "SKILL.md")
			if c.want == "" {
				if code != 0 || !strings.Contains(stdout, "Skill updated to v9.9.9") {
					t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
				}
				if _, err := os.Stat(skill); err != nil {
					t.Fatalf("SKILL.md not installed: %v", err)
				}
				return
			}
			if code == 0 || !strings.Contains(stderr, c.want) {
				t.Fatalf("exit %d, want refusal with %q\nstdout %s\nstderr %s", code, c.want, stdout, stderr)
			}
			if _, err := os.Stat(filepath.Dir(skill)); !os.IsNotExist(err) {
				t.Fatalf("the skill directory was written: %v", err)
			}
		})
	}
}

// Extracted and copied binaries are 0755 whatever the archive or umask says.
func TestExtractedBinaryIsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, tarGz(t, map[string]string{"archivist": newBinary}), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := extractBinaryFromTarGz(archive, "archivist", out); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(dir, "a.zip")
	writeZip(t, zipPath, map[string]string{"archivist.exe": "exe"})
	outZip := filepath.Join(dir, "out.exe")
	if err := extractBinaryFromZip(zipPath, "archivist.exe", outZip); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "copied")
	if err := copyFile(out, copied); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{out, outZip, copied} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: mode %v %v", p, info.Mode(), err)
		}
	}
}

// Release downloads follow redirects only to https:// or loopback http://.
func TestFetchBytesRedirectPolicy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("redirected body"))
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain":
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		case "/loopback":
			http.Redirect(w, r, target.URL+"/x", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)

	if _, err := fetchBytes(t.Context(), srv.URL+"/plain"); err == nil || !strings.Contains(err.Error(), "redirect to http:// refused") {
		t.Fatalf("redirect to a non loopback http:// URL: %v", err)
	}
	body, err := fetchBytes(t.Context(), srv.URL+"/loopback")
	if err != nil || string(body) != "redirected body" {
		t.Fatalf("redirect to a loopback http:// URL: %q %v", body, err)
	}
}

// tarGzEntries builds a .tar.gz from headers and bodies, in order.
func tarGzEntries(t *testing.T, entries []tar.Header, bodies []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range entries {
		hdr := entries[i]
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(bodies[i]))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(bodies[i]))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Only a regular file named archivist is extracted; a link of that name is
// skipped.
func TestExtractBinarySkipsNonRegularEntries(t *testing.T) {
	dir := t.TempDir()
	link := tar.Header{Name: "archivist", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh", Mode: 0o777}
	reg := tar.Header{Name: "archivist", Typeflag: tar.TypeReg, Mode: 0o755}

	onlyLink := filepath.Join(dir, "link.tar.gz")
	if err := os.WriteFile(onlyLink, tarGzEntries(t, []tar.Header{link}, []string{""}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractBinaryFromTarGz(onlyLink, "archivist", filepath.Join(dir, "out1")); err == nil || !strings.Contains(err.Error(), "not found in archive") {
		t.Fatalf("an archive whose archivist is a link: %v", err)
	}

	both := filepath.Join(dir, "both.tar.gz")
	if err := os.WriteFile(both, tarGzEntries(t, []tar.Header{link, reg}, []string{"", "regular-binary"}), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out2")
	if err := extractBinaryFromTarGz(both, "archivist", out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "regular-binary" {
		t.Fatalf("extracted %q %v", b, err)
	}
}
