package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Accepted token prefixes:
//   - "ak_"     — Clerk's native UserProfile API key format (what real users get)
//   - "mc_pat_" — historical placeholder from early 36.x design; kept for
//                 test fixtures and backwards-compat with any docs in the wild.
var tokenPrefixes = []string{"ak_", "mc_pat_"}

// ErrNoToken is returned when no credential is found on any rung.
var ErrNoToken = errors.New("no CLI token found. Run 'archivist auth login --token ak_...' to save a credential, or set ARCHIVIST_TOKEN")

// ErrInvalidFormat is returned when a token is present but malformed.
var ErrInvalidFormat = errors.New("token format invalid. Expected ak_... — create one in the 'API keys' section of Clerk's UserProfile popup at https://mosaic-finance.com")

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
// 0700) and returns the path written. Callers verify the token first; this
// function only persists.
func SaveToken(token string) (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write credentials file %s: %w", path, err)
	}
	return path, nil
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

// ValidateTokenFormat checks that the token starts with one of the
// accepted prefixes (see tokenPrefixes).
func ValidateTokenFormat(token string) error {
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(token, p) {
			return nil
		}
	}
	return fmt.Errorf("%w", ErrInvalidFormat)
}
