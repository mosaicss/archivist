package cmd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

func TestAuthLoginPrintsURL(t *testing.T) {
	root := cmd.NewRootCmd("v0.2.0", "abc1234", "2026-05-20")

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"auth", "login"})

	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "https://mosaic-finance.com/account/cli-tokens") {
		t.Errorf("expected dashboard URL in output, got:\n%s", output)
	}
	if !strings.Contains(output, "export ARCHIVIST_TOKEN") {
		t.Errorf("expected shell export snippet in output, got:\n%s", output)
	}
}

func TestAuthStatusWithValidToken(t *testing.T) {
	// Set up a test HTTP server simulating the chat-api GET /account/cli-tokens
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/cli-tokens" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer mc_pat_") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"user_email": "test@example.com",
			"tier":       "pro",
			"tokens": []map[string]interface{}{
				{
					"key_id":      "key_abc123",
					"name":        "My Token",
					"created_at":  "2026-05-20T00:00:00Z",
					"last_used_at": nil,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)

	root := cmd.NewRootCmd("v0.2.0", "abc1234", "2026-05-20")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"auth", "status"})

	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "test@example.com") {
		t.Errorf("expected email in output, got:\n%s", output)
	}
	if !strings.Contains(output, "pro") {
		t.Errorf("expected tier in output, got:\n%s", output)
	}
}

func TestAuthStatusNoToken(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "")

	root := cmd.NewRootCmd("v0.2.0", "abc1234", "2026-05-20")
	var errOut bytes.Buffer
	root.SetErr(&errOut)
	root.SetArgs([]string{"auth", "status"})

	err := root.Execute()
	// Expect a non-nil error wrapping ExitAuthError
	if err == nil {
		t.Fatal("expected error for missing token, got nil")
	}
}

func TestAuthLogoutRepurposed(t *testing.T) {
	root := cmd.NewRootCmd("v0.2.0", "abc1234", "2026-05-20")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"auth", "logout"})

	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "not available") {
		t.Errorf("expected 'not available' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "https://mosaic-finance.com/account/cli-tokens") {
		t.Errorf("expected dashboard URL in output, got:\n%s", output)
	}
}
