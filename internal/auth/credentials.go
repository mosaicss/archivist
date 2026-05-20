package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const tokenPrefix = "mc_pat_"

// ErrNoToken is returned when no credential is found.
var ErrNoToken = errors.New("no CLI token found. Run 'archivist auth login' to get setup instructions")

// ErrInvalidFormat is returned when a token is present but malformed.
var ErrInvalidFormat = errors.New("token format invalid. Expected mc_pat_... — re-issue from https://mosaic-finance.com/account/cli-tokens")

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

// ValidateTokenFormat checks that the token starts with the expected prefix.
func ValidateTokenFormat(token string) error {
	if !strings.HasPrefix(token, tokenPrefix) {
		return fmt.Errorf("%w", ErrInvalidFormat)
	}
	return nil
}
