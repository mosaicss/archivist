package cmd

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/service"
	"github.com/spf13/cobra"
)

const (
	archivistReleaseAPI = "https://api.github.com/repos/mosaicss/archivist/releases/latest"
	archivistRepo       = "mosaicss/archivist"
)

// NewUpdateCmd returns the `archivist update` command.
func NewUpdateCmd(currentVersion string) *cobra.Command {
	var skillOnly bool
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Replace the binary in place with the latest release",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,3,5",
			// Hidden from MCP: self-replaces the running binary; agents must
			// never trigger an in-place update (Story 39.7).
			"mcp:hidden": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			if checkOnly {
				return runUpdateCheck(ctx, cmd, currentVersion)
			}
			if skillOnly {
				return runUpdateSkill(ctx, cmd, currentVersion)
			}
			return runUpdate(ctx, cmd, currentVersion)
		},
	}

	cmd.Flags().BoolVar(&skillOnly, "skill", false, "Update only the Claude Code skill files")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Check for updates without installing")
	return cmd
}

// runUpdateCheck prints whether an update is available without installing.
func runUpdateCheck(ctx context.Context, cmd *cobra.Command, current string) error {
	latest, _, err := fetchLatestVersion(ctx)
	if err != nil {
		return &ExitError{Code: ExitServerError}
	}

	normalCurrent := strings.TrimPrefix(current, "v")
	normalLatest := strings.TrimPrefix(latest, "v")

	if normalCurrent == normalLatest {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Up to date: v%s\n", normalCurrent)
		return &ExitError{Code: ExitNotFound}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Current: v%s | Latest: v%s — run 'archivist update' to upgrade\n",
		normalCurrent, normalLatest)
	return nil
}

// runUpdateSkill downloads and installs only the skill-bundle.
func runUpdateSkill(ctx context.Context, cmd *cobra.Command, current string) error {
	latest, _, err := fetchLatestVersion(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "archivist update --skill: failed to fetch release info:", err)
		return &ExitError{Code: ExitServerError}
	}

	ver := strings.TrimPrefix(latest, "v")
	skillArchive := fmt.Sprintf("archivist_v%s_skill-bundle.tar.gz", ver)
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", archivistRepo, latest, skillArchive)

	skillDir, err := skillInstallDir()
	if err != nil || skillDir == "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "~/.claude/skills/ not found; skill install skipped")
		return nil
	}

	tmpFile, err := os.CreateTemp("", "archivist-skill-*.tar.gz")
	if err != nil {
		return &ExitError{Code: ExitServerError}
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if err := downloadTo(ctx, url, tmpFile.Name()); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update --skill: download failed: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return &ExitError{Code: ExitGenericError}
	}
	if err := extractTarGz(tmpFile.Name(), skillDir); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update --skill: extract failed: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Skill updated to v%s\n", ver)
	_ = current
	return nil
}

// runUpdate performs the self-replace update, routing on install channel.
func runUpdate(ctx context.Context, cmd *cobra.Command, current string) error {
	channel := readInstallChannel()

	switch channel {
	case "brew":
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Run: brew upgrade mosaic-finance-inc/tap/archivist")
		return nil
	case "npm":
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Run: npx -y @mosaic-finance/archivist@latest install")
		return nil
	}

	// Self-replace path: curl-sh, github-releases, unknown, or absent channel
	latest, sumsURL, err := fetchLatestVersion(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "archivist update: failed to fetch release info:", err)
		return &ExitError{Code: ExitServerError}
	}

	normalCurrent := strings.TrimPrefix(current, "v")
	normalLatest := strings.TrimPrefix(latest, "v")

	if normalCurrent == normalLatest {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Already up to date: v%s\n", normalCurrent)
		return &ExitError{Code: ExitNotFound}
	}

	platform := releasePlatform(runtime.GOOS, runtime.GOARCH)
	var archiveExt string
	if runtime.GOOS == "windows" {
		archiveExt = "zip"
	} else {
		archiveExt = "tar.gz"
	}

	ver := strings.TrimPrefix(latest, "v")
	archiveName := fmt.Sprintf("archivist_v%s_%s.%s", ver, platform, archiveExt)
	archiveURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", archivistRepo, latest, archiveName)

	// Download archive
	tmpArchive, err := os.CreateTemp("", "archivist-update-*."+archiveExt)
	if err != nil {
		return &ExitError{Code: ExitServerError}
	}
	defer func() { _ = os.Remove(tmpArchive.Name()) }()

	if err := downloadTo(ctx, archiveURL, tmpArchive.Name()); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: download failed: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}

	// Download and verify SHA256
	sumsData, err := fetchBytes(ctx, sumsURL)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: failed to fetch SHA256SUMS: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}
	expectedHash, err := extractHashFromSums(sumsData, archiveName)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}
	if err := verifySHA256(tmpArchive.Name(), expectedHash); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}

	// Extract binary to temp file
	tmpBin, err := os.CreateTemp("", "archivist-new-*")
	if err != nil {
		return &ExitError{Code: ExitServerError}
	}
	_ = tmpBin.Close()
	defer func() { _ = os.Remove(tmpBin.Name()) }()

	binaryName := "archivist"
	if runtime.GOOS == "windows" {
		binaryName = "archivist.exe"
	}
	if archiveExt == "zip" {
		if err := extractBinaryFromZip(tmpArchive.Name(), binaryName, tmpBin.Name()); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: extract failed: %v\n", err)
			return &ExitError{Code: ExitServerError}
		}
	} else {
		if err := extractBinaryFromTarGz(tmpArchive.Name(), binaryName, tmpBin.Name()); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: extract failed: %v\n", err)
			return &ExitError{Code: ExitServerError}
		}
	}

	if err := os.Chmod(tmpBin.Name(), 0o755); err != nil {
		return &ExitError{Code: ExitServerError}
	}

	// Get current binary path
	currentBin, err := os.Executable()
	if err != nil {
		return &ExitError{Code: ExitServerError}
	}

	if runtime.GOOS == "windows" {
		// Windows (Story 78.34): a running exe cannot be overwritten but can
		// be renamed, so archivist.exe and archivistw.exe (the background
		// service's console-less companion, when the archive has it) are
		// renamed to unique .old-<id> names and the new ones copied in.
		stageDir, err := os.MkdirTemp("", "archivist-update-*")
		if err != nil {
			return &ExitError{Code: ExitServerError}
		}
		defer func() { _ = os.RemoveAll(stageDir) }()
		staged, err := stageWindowsBinaries(tmpArchive.Name(), filepath.Base(currentBin), stageDir)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: extract failed: %v\n", err)
			return &ExitError{Code: ExitServerError}
		}
		if err := swapBinaries(filepath.Dir(currentBin), staged); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: failed to replace the binaries: %v\n", err)
			return &ExitError{Code: ExitServerError}
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"Updated from v%s to v%s. Run 'archivist version' to confirm.\n",
			normalCurrent, normalLatest)
		if home, err := os.UserHomeDir(); err == nil {
			if hint := serviceRestartHint(home, ""); hint != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), hint)
			}
		}
		return nil
	}

	// POSIX: atomic rename (src and dst must be on the same filesystem)
	newPath := currentBin + ".new"
	if err := copyFile(tmpBin.Name(), newPath); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: failed to stage new binary: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}
	if err := os.Rename(newPath, currentBin); err != nil {
		_ = os.Remove(newPath)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist update: failed to replace binary: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Updated from v%s to v%s. Run 'archivist version' to confirm.\n",
		normalCurrent, normalLatest)
	if home, err := os.UserHomeDir(); err == nil {
		if hint := serviceRestartHint(home, os.Getenv("XDG_CONFIG_HOME")); hint != "" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), hint)
		}
	}
	return nil
}

// stageWindowsBinaries extracts the Windows zip's executables into dir and
// maps each to the name it replaces: archivist.exe to the running
// executable's own name (a renamed copy such as archivist2.exe is the one
// updated), and archivistw.exe, when the archive has it, to archivistw.exe.
// A missing archivist.exe, or any extraction error other than a missing
// archivistw.exe, fails.
func stageWindowsBinaries(archive, currentName, dir string) (map[string]string, error) {
	if strings.EqualFold(currentName, "archivistw.exe") {
		currentName = "archivist.exe"
	}
	primary := filepath.Join(dir, "archivist.exe")
	if err := extractBinaryFromZip(archive, "archivist.exe", primary); err != nil {
		return nil, err
	}
	staged := map[string]string{currentName: primary}
	companion := filepath.Join(dir, "archivistw.exe")
	switch err := extractBinaryFromZip(archive, "archivistw.exe", companion); {
	case err == nil:
		staged["archivistw.exe"] = companion
	case !errors.Is(err, errBinaryNotInZip):
		return nil, err
	}
	return staged, nil
}

// swapBinaries installs each staged file (name -> source path) into dir:
// an existing file is first renamed to a unique <name>.old-<id> (a running
// Windows exe can be renamed, not overwritten), then the new one copied in.
// A failed copy puts the renamed file back. Stale <name>.old-* files of the
// staged names from earlier swaps, and the legacy <name>.new, are removed best effort (one still running
// stays until next time); nothing else in dir is touched (it may be a shared
// folder such as ~/bin or an npm prefix).
func swapBinaries(dir string, staged map[string]string) error {
	names := make([]string, 0, len(staged))
	for name := range staged {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if olds, err := filepath.Glob(filepath.Join(dir, globEscape(name)+".old-*")); err == nil {
			for _, o := range olds {
				_ = os.Remove(o)
			}
		}
		// The pre-78.34 updater left <name>.new beside the binary.
		_ = os.Remove(filepath.Join(dir, name+".new"))
	}
	id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	for _, name := range names {
		target := filepath.Join(dir, name)
		old := ""
		if _, err := os.Stat(target); err == nil {
			old = target + ".old-" + id
			if err := os.Rename(target, old); err != nil {
				return fmt.Errorf("move %s aside: %w", name, err)
			}
		}
		if err := copyFile(staged[name], target); err != nil {
			_ = os.Remove(target)
			if old != "" {
				_ = os.Rename(old, target)
			}
			return fmt.Errorf("write %s: %w", name, err)
		}
		_ = os.Chmod(target, 0o755)
	}
	return nil
}

// globEscape quotes the filepath.Match metacharacters in a literal name.
func globEscape(name string) string {
	return strings.NewReplacer("*", "[*]", "?", "[?]", "[", "[[]").Replace(name)
}

// serviceRestartHint is the one line telling a user whose background service
// is installed to restart it on the new binary; the running service keeps the
// old one until then. Empty when no unit or plist exists.
func serviceRestartHint(home, configHome string) string {
	if !filepath.IsAbs(configHome) {
		configHome = ""
	}
	if _, ok := service.InstalledPath(home, configHome); !ok {
		return ""
	}
	return "Your background service still runs the old version. Run 'archivist connect --install' to restart it on the new one."
}

// releasePlatform is the release archive's platform part. goreleaser ships
// one universal macOS binary (universal_binaries replace: true), so every
// darwin build downloads archivist_v<ver>_darwin_all.tar.gz; there is no
// darwin_arm64 or darwin_amd64 archive.
func releasePlatform(goos, goarch string) string {
	if goos == "darwin" {
		return "darwin_all"
	}
	return goos + "_" + goarch
}

// readInstallChannel reads ~/.archivist/install-channel. Returns "" if absent.
func readInstallChannel() string {
	p := filepath.Join(os.Getenv("HOME"), ".archivist", "install-channel")
	if runtime.GOOS == "windows" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, ".archivist", "install-channel")
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// githubRelease is a minimal JSON shape from the GitHub releases API.
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchLatestVersion queries GitHub API and returns (tag, sha256sums_url, error).
func fetchLatestVersion(ctx context.Context) (string, string, error) {
	data, err := fetchBytes(ctx, archivistReleaseAPI)
	if err != nil {
		return "", "", err
	}
	var rel githubRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", "", fmt.Errorf("parse release JSON: %w", err)
	}
	if rel.TagName == "" {
		return "", "", fmt.Errorf("empty tag_name in release response")
	}
	var sumsURL string
	ver := strings.TrimPrefix(rel.TagName, "v")
	for _, a := range rel.Assets {
		if a.Name == fmt.Sprintf("archivist_v%s_SHA256SUMS", ver) {
			sumsURL = a.BrowserDownloadURL
			break
		}
	}
	if sumsURL == "" {
		sumsURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/archivist_v%s_SHA256SUMS",
			archivistRepo, rel.TagName, ver)
	}
	return rel.TagName, sumsURL, nil
}

// fetchBytes downloads a URL into memory.
func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "archivist-update/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// downloadTo downloads url to dest file path.
func downloadTo(ctx context.Context, url, dest string) error {
	data, err := fetchBytes(ctx, url)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}

// extractHashFromSums finds the SHA256 hash for filename in a SHA256SUMS-format byte slice.
func extractHashFromSums(data []byte, filename string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == filename {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("hash for %q not found in SHA256SUMS", filename)
}

// verifySHA256 checks the SHA256 of a file against an expected hex digest.
func verifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open for hash: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash file: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA256 mismatch: expected %s got %s", expected, actual)
	}
	return nil
}

// extractBinaryFromTarGz extracts a named binary from a .tar.gz into destPath.
func extractBinaryFromTarGz(archivePath, binaryName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) == binaryName {
			out, err := os.Create(destPath)
			if err != nil {
				return err
			}
			defer func() { _ = out.Close() }()
			_, err = io.Copy(out, tr)
			return err
		}
	}
	return fmt.Errorf("binary %q not found in archive", binaryName)
}

// extractBinaryFromZip extracts a named binary from a .zip into destPath.
func extractBinaryFromZip(archivePath, binaryName, destPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, f := range r.File {
		if filepath.Base(f.Name) == binaryName {
			src, err := f.Open()
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			out, err := os.Create(destPath)
			if err != nil {
				return err
			}
			defer func() { _ = out.Close() }()
			_, err = io.Copy(out, src)
			return err
		}
	}
	return fmt.Errorf("binary %q: %w", binaryName, errBinaryNotInZip)
}

// errBinaryNotInZip means the archive holds no file of that name.
var errBinaryNotInZip = errors.New("not found in zip archive")

// copyFile copies src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// extractTarGz extracts all files from a tar.gz into destDir (flat, no subdirs).
func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		destPath := filepath.Join(destDir, filepath.Base(hdr.Name))
		out, err := os.Create(destPath)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return err
		}
		_ = out.Close()
	}
	return nil
}

// skillInstallDir returns the path to ~/.claude/skills/archivist, or "" if not applicable.
func skillInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	skillBase := filepath.Join(home, ".claude", "skills")
	if _, err := os.Stat(skillBase); os.IsNotExist(err) {
		return "", nil
	}
	return filepath.Join(skillBase, "archivist"), nil
}
