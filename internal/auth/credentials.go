package auth

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mosaicss/archivist/internal/client"
)

// Accepted token prefixes:
//   - "ak_"     — Clerk's native UserProfile API key format (what real users get)
//   - "mc_pat_" — historical placeholder from early 36.x design; kept for
//     test fixtures and backwards-compat with any docs in the wild.
var tokenPrefixes = []string{"ak_", "mc_pat_"}

// TaskTokenPrefix marks an opaque Mosaic agent session task token (Story
// 78.15). Task tokens are minted per session by `archivist connect` and are
// accepted only where a call reads them (mcp serve, verbs); they are never a
// login credential.
const TaskTokenPrefix = "mst_"

// taskTokenRe is the strict 78.15 wire: mst_<lowercase UUID>.<32 bytes b64url>.
var taskTokenRe = regexp.MustCompile(`^mst_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.[A-Za-z0-9_-]{43}$`)

// ErrInvalidTaskToken is returned for an mst_ token that is not the strict wire.
var ErrInvalidTaskToken = errors.New("task token format invalid. Expected mst_<uuid>.<secret> as minted for one Mosaic agent session")

// ErrTaskTokenLogin is returned when a task token is offered as a login credential.
var ErrTaskTokenLogin = errors.New("task tokens (mst_...) belong to one agent session and cannot be saved as a login. Use an ak_... API key")

// ErrNoToken is returned when no credential is found on any rung.
var ErrNoToken = errors.New("no CLI token found. Run 'archivist auth login --token ak_...' to save a credential, or set ARCHIVIST_TOKEN")

// ErrInvalidFormat is returned when a token is present but malformed.
var ErrInvalidFormat = errors.New("token format invalid. Expected ak_... — create one in the 'API keys' section of your account (avatar menu → Manage account → API keys) at " + client.AccountURL)

// Source identifies which rung of the resolution ladder produced the token.
type Source int

const (
	SourceNone Source = iota
	SourceFlag
	SourceEnv
	SourceFile
)

func (s Source) String() string {
	switch s {
	case SourceFlag:
		return "flag"
	case SourceEnv:
		return "env"
	case SourceFile:
		return "file"
	default:
		return "none"
	}
}

// CredentialsPath returns the canonical credentials file path,
// ~/.archivist/credentials, via os.UserHomeDir (reads $HOME on unix).
func CredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".archivist", "credentials"), nil
}

// Resolve returns the active CLI token and the ladder rung it came from.
// Resolution order: --token flag > ARCHIVIST_TOKEN env var > credentials
// file > ErrNoToken. A found-but-malformed token returns the token and its
// source alongside a format error so diagnostic callers (doctor) can still
// report on the credential that was found.
func Resolve(flagValue string) (string, Source, error) {
	if flagValue != "" {
		return flagValue, SourceFlag, validate(flagValue, SourceFlag, "")
	}
	if env := os.Getenv("ARCHIVIST_TOKEN"); env != "" {
		return env, SourceEnv, validate(env, SourceEnv, "")
	}

	path, err := CredentialsPath()
	if err != nil {
		return "", SourceNone, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", SourceNone, ErrNoToken
		}
		return "", SourceNone, fmt.Errorf("read credentials file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		// Empty or whitespace-only file is "no credential", not a format error.
		return "", SourceNone, ErrNoToken
	}
	return token, SourceFile, validate(token, SourceFile, path)
}

// validate wraps ValidateTokenFormat, naming the credentials file when the
// malformed token came from disk.
func validate(token string, source Source, path string) error {
	if err := ValidateTokenFormat(token); err != nil {
		if source == SourceFile {
			return fmt.Errorf("credentials file %s: %w", path, err)
		}
		return err
	}
	return nil
}

// SavedToken returns the credentials file's token, ignoring the flag and
// ARCHIVIST_TOKEN rungs: it is what a background service (which never
// carries ARCHIVIST_TOKEN) will use. A missing or empty file is ErrNoToken.
func SavedToken() (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoToken
		}
		return "", fmt.Errorf("read credentials file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", ErrNoToken
	}
	return token, validate(token, SourceFile, path)
}

// ResolveToken returns the active CLI token or an error.
// Thin wrapper over Resolve for the existing per-verb call sites.
func ResolveToken(flagValue string) (string, error) {
	token, _, err := Resolve(flagValue)
	if err != nil {
		return "", err
	}
	return token, nil
}

// SaveToken writes the token to the credentials file (mode 0600, parent dir
// 0700) atomically (temporary file plus rename) and returns the path
// written. When the credentials file is a symlink (a dotfiles setup), the
// link is kept: the temporary file is written beside its target and renamed
// onto it. Callers verify the token first; this function only persists.
func SaveToken(token string) (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	target, err := credentialsTarget(path)
	if err != nil {
		return "", fmt.Errorf("resolve credentials link %s: %w", path, err)
	}
	if target != path {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
	}
	if err := writeFileAtomic(target, []byte(token+"\n")); err != nil {
		return "", fmt.Errorf("write credentials file %s: %w", path, err)
	}
	return path, nil
}

// credentialsTarget is the file SaveToken replaces: path itself, or, when
// path is a symlink (dotfiles), the file it points to, so the link survives.
// A link whose target does not exist yet (a fresh dotfiles checkout) is
// followed with os.Readlink, relative targets against the link's directory.
func credentialsTarget(path string) (string, error) {
	target := path
	for hops := 0; ; hops++ {
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			return target, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return target, nil
		}
		if hops >= 40 {
			return "", errors.New("too many levels of symbolic links")
		}
		next, err := os.Readlink(target)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(filepath.Dir(target), next)
		}
		target = next
	}
}

// writeFileAtomic writes data to a 0600 temporary file in path's directory,
// syncs it and renames it over path, so a reader sees the old credential or
// the new one, never a partial file. The temporary file is removed on any
// failure.
func writeFileAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	// CreateTemp already opens 0600; Chmod states it for every platform.
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DeleteCredentials removes the credentials file. Idempotent: a missing file
// returns existed=false with no error.
func DeleteCredentials() (bool, string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return false, "", err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, path, nil
		}
		return true, path, fmt.Errorf("remove credentials file %s: %w", path, err)
	}
	return true, path, nil
}

// MaskToken returns a display-safe form of the token: first 10 characters,
// an ellipsis, and the last 3. Never prints the full token. Same shape as
// doctor's key_id display.
func MaskToken(token string) string {
	if len(token) < 15 {
		return "ak_???"
	}
	return token[:10] + "..." + token[len(token)-3:]
}

// IsTaskToken reports whether token carries the task token prefix.
func IsTaskToken(token string) bool {
	return strings.HasPrefix(token, TaskTokenPrefix)
}

// ValidateTokenFormat checks that the token starts with one of the
// accepted prefixes (see tokenPrefixes), or is a strictly formed task token.
func ValidateTokenFormat(token string) error {
	if IsTaskToken(token) {
		if !taskTokenRe.MatchString(token) {
			return ErrInvalidTaskToken
		}
		return nil
	}
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(token, p) {
			return nil
		}
	}
	return fmt.Errorf("%w", ErrInvalidFormat)
}

// ValidateLoginTokenFormat is ValidateTokenFormat for credentials that will be
// saved or used as the owner's login: task tokens are refused.
func ValidateLoginTokenFormat(token string) error {
	if IsTaskToken(token) {
		return ErrTaskTokenLogin
	}
	return ValidateTokenFormat(token)
}

// KeyID derives the non-secret key_id component used for fingerprints:
// "<prefix><first 8 chars after the prefix>". Falls back to the first 10
// characters when no recognized prefix matches.
func KeyID(token string) string {
	for _, prefix := range []string{"ak_", "mc_pat_", TaskTokenPrefix} {
		if !strings.HasPrefix(token, prefix) {
			continue
		}
		rest := token[len(prefix):]
		if len(rest) >= 8 {
			return prefix + rest[:8]
		}
		return token
	}
	if len(token) >= 10 {
		return token[:10]
	}
	return token
}

// Fingerprint is the log-safe identity of a credential: SHA256(key_id)[:4] as
// hex (doctor's OQ4 rule). It never reveals secret material.
func Fingerprint(token string) string {
	h := sha256.Sum256([]byte(KeyID(token)))
	return fmt.Sprintf("%x", h[:4])
}
