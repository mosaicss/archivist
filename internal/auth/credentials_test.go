package auth_test

import (
	"errors"
	"testing"

	"github.com/mosaicss/archivist/internal/auth"
)

func TestResolveTokenFlagWins(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_env_value")
	got, err := auth.ResolveToken("mc_pat_flag_value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mc_pat_flag_value" {
		t.Errorf("got %q, want mc_pat_flag_value", got)
	}
}

func TestResolveTokenEnvFallback(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_env_value")
	got, err := auth.ResolveToken("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mc_pat_env_value" {
		t.Errorf("got %q, want mc_pat_env_value", got)
	}
}

func TestResolveTokenNeitherExits(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "")
	_, err := auth.ResolveToken("")
	if !errors.Is(err, auth.ErrNoToken) {
		t.Errorf("expected ErrNoToken, got %v", err)
	}
}

func TestTokenFormatValidation(t *testing.T) {
	validToken := "mc_pat_validtoken"
	if err := auth.ValidateTokenFormat(validToken); err != nil {
		t.Errorf("expected valid token to pass, got %v", err)
	}

	invalidToken := "sk_test_notaclittoken"
	if err := auth.ValidateTokenFormat(invalidToken); err == nil {
		t.Error("expected invalid token to fail, got nil")
	}

	emptyToken := ""
	if err := auth.ValidateTokenFormat(emptyToken); err == nil {
		t.Error("expected empty token to fail, got nil")
	}
}
