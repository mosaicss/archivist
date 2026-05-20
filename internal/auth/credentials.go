package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Accepted token prefixes:
//   - "ak_"     — Clerk's native UserProfile API key format (what real users get)
//   - "mc_pat_" — historical placeholder from early 36.x design; kept for
//                 test fixtures and backwards-compat with any docs in the wild.
var tokenPrefixes = []string{"ak_", "mc_pat_"}

// ErrNoToken is returned when no credential is found.
var ErrNoToken = errors.New("no CLI token found. Run 'archivist auth login' to get setup instructions")

// ErrInvalidFormat is returned when a token is present but malformed.
var ErrInvalidFormat = errors.New("token format invalid. Expected ak_... — create one in the 'API keys' section of Clerk's UserProfile popup at https://mosaic-finance.com")

// ResolveToken returns the active CLI token or an error.
// Resolution order: --token flag > ARCHIVIST_TOKEN env var > error.
func ResolveToken(flagValue string) (string, error) {
	token := flagValue
	if token == "" {
		token = os.Getenv("ARCHIVIST_TOKEN")
	}
	if token == "" {
		return "", ErrNoToken
	}
	if err := ValidateTokenFormat(token); err != nil {
		return "", err
	}
	return token, nil
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
