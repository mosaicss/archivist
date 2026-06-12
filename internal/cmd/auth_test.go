package cmd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

// sandboxAuthHome points os.UserHomeDir at a temp dir so login/logout tests
// never touch the developer's real ~/.archivist/credentials.
func sandboxAuthHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func credPath(home string) string {
	return home + "/.archivist/credentials"
}

// serveCLITokens returns an httptest server mocking GET /account/cli-tokens
// with the given status code.
func serveCLITokens(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/cli-tokens" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if statusCode == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com",
				"tier":       "pro",
				"tokens": []map[string]interface{}{
					{"key_id": "key_abc123", "name": "My Token", "created_at": "2026-05-20T00:00:00Z", "last_used_at": nil},
				},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runAuthCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := cmd.NewRootCmd("v0.2.0", "abc1234", "2026-05-20")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestAuthLoginPrintsURL(t *testing.T) {
	sandboxAuthHome(t)
	output, err := runAuthCmd(t, "auth", "login")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(output, "https://mosaic-finance.com/chat") {
		t.Errorf("expected dashboard URL in output, got:\n%s", output)
	}
	if !strings.Contains(output, "ARCHIVIST_TOKEN") {
		t.Errorf("expected env var override mention in output, got:\n%s", output)
	}
}

func TestAuthLoginBareNonTTYPrintsInstructionsOnly(t *testing.T) {
	home := sandboxAuthHome(t)
	// Bare login with stdin not a TTY (the go test default) must print
	// instructions mentioning --token, exit 0, and never write a file or hang.
	output, err := runAuthCmd(t, "auth", "login")
	if err != nil {
		t.Fatalf("expected exit 0, got error: %v", err)
	}
	if !strings.Contains(output, "--token") {
		t.Errorf("expected --token mention for non-TTY callers, got:\n%s", output)
	}
	if _, statErr := os.Stat(credPath(home)); !os.IsNotExist(statErr) {
		t.Errorf("bare non-TTY login must not write a credentials file, stat: %v", statErr)
	}
}

func TestAuthLoginWithTokenSavesCredential(t *testing.T) {
	home := sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusOK)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)

	fullToken := "ak_1234567890abcdefxyz"
	output, err := runAuthCmd(t, "auth", "login", "--token", fullToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, readErr := os.ReadFile(credPath(home))
	if readErr != nil {
		t.Fatalf("credentials file not written: %v", readErr)
	}
	if string(data) != fullToken+"\n" {
		t.Errorf("file content: got %q, want token plus newline", string(data))
	}
	fi, _ := os.Stat(credPath(home))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode: got %o, want 0600", fi.Mode().Perm())
	}

	if strings.Contains(output, fullToken) {
		t.Errorf("full token must never be printed, got:\n%s", output)
	}
	if !strings.Contains(output, "ak_1234567...xyz") {
		t.Errorf("expected masked key id in confirmation, got:\n%s", output)
	}
	if !strings.Contains(output, credPath(home)) {
		t.Errorf("expected credentials file path in confirmation, got:\n%s", output)
	}
}

func TestAuthLoginWithTokenInvalidFormatNoWrite(t *testing.T) {
	home := sandboxAuthHome(t)
	output, err := runAuthCmd(t, "auth", "login", "--token", "bogus_token")
	if exitCodeFrom(err) != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCodeFrom(err), output)
	}
	if _, statErr := os.Stat(credPath(home)); !os.IsNotExist(statErr) {
		t.Error("invalid format must not write the credentials file")
	}
}

func TestAuthLoginWithTokenUnauthorizedNoWrite(t *testing.T) {
	home := sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusUnauthorized)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)

	output, err := runAuthCmd(t, "auth", "login", "--token", "ak_revokedtoken12345")
	if exitCodeFrom(err) != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCodeFrom(err), output)
	}
	if !strings.Contains(output, "API keys") {
		t.Errorf("expected key reissue guidance, got:\n%s", output)
	}
	if _, statErr := os.Stat(credPath(home)); !os.IsNotExist(statErr) {
		t.Error("401 must not write the credentials file")
	}
}

func TestAuthLoginWithTokenServerErrorNoWrite(t *testing.T) {
	home := sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusInternalServerError)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)

	output, err := runAuthCmd(t, "auth", "login", "--token", "ak_validformat12345")
	if exitCodeFrom(err) != 5 {
		t.Errorf("expected exit code 5, got %d; output:\n%s", exitCodeFrom(err), output)
	}
	if _, statErr := os.Stat(credPath(home)); !os.IsNotExist(statErr) {
		t.Error("server error must not write the credentials file")
	}
}

func TestAuthLoginWithTokenUnreachableNoWrite(t *testing.T) {
	home := sandboxAuthHome(t)
	t.Setenv("ARCHIVIST_BASE_URL", "http://127.0.0.1:1")

	output, err := runAuthCmd(t, "auth", "login", "--token", "ak_validformat12345")
	if exitCodeFrom(err) != 5 {
		t.Errorf("expected exit code 5, got %d; output:\n%s", exitCodeFrom(err), output)
	}
	if !strings.Contains(output, "ARCHIVIST_TOKEN") {
		t.Errorf("expected env var workaround mention when server is down, got:\n%s", output)
	}
	if _, statErr := os.Stat(credPath(home)); !os.IsNotExist(statErr) {
		t.Error("unreachable server must not write the credentials file")
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
	sandboxAuthHome(t)
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

func TestAuthStatusShowsSourceEnv(t *testing.T) {
	sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusOK)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_TOKEN", "ak_envtokenvalue123")

	output, err := runAuthCmd(t, "auth", "status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output, "source: env") {
		t.Errorf("expected 'source: env' in output, got:\n%s", output)
	}
	// No credentials file exists: shadow note must NOT appear.
	if strings.Contains(output, "overridden") {
		t.Errorf("shadow note must not appear without a credentials file, got:\n%s", output)
	}
}

func TestAuthStatusShowsSourceFile(t *testing.T) {
	home := sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusOK)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_TOKEN", "")
	if err := os.MkdirAll(home+"/.archivist", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath(home), []byte("ak_filetokenvalue12\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runAuthCmd(t, "auth", "status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output, "source: file (~/.archivist/credentials)") {
		t.Errorf("expected 'source: file (~/.archivist/credentials)' in output, got:\n%s", output)
	}
}

func TestAuthStatusShowsSourceFlag(t *testing.T) {
	sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusOK)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_TOKEN", "")

	output, err := runAuthCmd(t, "--token", "ak_flagtokenvalue12", "auth", "status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output, "source: flag") {
		t.Errorf("expected 'source: flag' in output, got:\n%s", output)
	}
}

func TestAuthStatusJSONHasSource(t *testing.T) {
	sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusOK)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_TOKEN", "ak_envtokenvalue123")

	output, err := runAuthCmd(t, "auth", "status", "--format", "json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]string
	if jsonErr := json.Unmarshal([]byte(output), &parsed); jsonErr != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", jsonErr, output)
	}
	if parsed["source"] != "env" {
		t.Errorf("expected JSON source key 'env', got %q; full: %v", parsed["source"], parsed)
	}
}

func TestAuthStatusShadowNoteWhenEnvOverridesFile(t *testing.T) {
	home := sandboxAuthHome(t)
	srv := serveCLITokens(t, http.StatusOK)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	t.Setenv("ARCHIVIST_TOKEN", "ak_envtokenvalue123")
	if err := os.MkdirAll(home+"/.archivist", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath(home), []byte("ak_filetokenvalue12\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runAuthCmd(t, "auth", "status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output, "credentials file present but overridden by env") {
		t.Errorf("expected shadow note, got:\n%s", output)
	}
}

func TestAuthLogoutDeletesCredentialsFile(t *testing.T) {
	home := sandboxAuthHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	if err := os.MkdirAll(home+"/.archivist", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath(home), []byte("ak_tokenvalue\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runAuthCmd(t, "auth", "logout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(credPath(home)); !os.IsNotExist(statErr) {
		t.Errorf("credentials file should be deleted, stat: %v", statErr)
	}
	if !strings.Contains(output, credPath(home)) {
		t.Errorf("expected deleted path in output, got:\n%s", output)
	}
	// Dashboard revocation note survives the stub replacement.
	if !strings.Contains(output, "https://mosaic-finance.com/chat") {
		t.Errorf("expected dashboard revocation note, got:\n%s", output)
	}
	// No env var set: the env note must NOT appear.
	if strings.Contains(output, "ARCHIVIST_TOKEN is still set") {
		t.Errorf("env note must not appear when ARCHIVIST_TOKEN is unset, got:\n%s", output)
	}
}

func TestAuthLogoutIdempotentWhenNoFile(t *testing.T) {
	sandboxAuthHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")

	output, err := runAuthCmd(t, "auth", "logout")
	if err != nil {
		t.Fatalf("expected exit 0 for missing file, got: %v", err)
	}
	if !strings.Contains(output, "no credentials file found") {
		t.Errorf("expected 'no credentials file found', got:\n%s", output)
	}
}

func TestAuthLogoutNotesEnvStillSet(t *testing.T) {
	home := sandboxAuthHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "ak_stillinenv12345")
	if err := os.MkdirAll(home+"/.archivist", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath(home), []byte("ak_tokenvalue\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runAuthCmd(t, "auth", "logout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output, "ARCHIVIST_TOKEN is still set") {
		t.Errorf("expected env still set note, got:\n%s", output)
	}
}

func TestAuthLogoutDeleteFailureExitsOne(t *testing.T) {
	home := sandboxAuthHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	// Make the credentials path a NON-EMPTY directory: os.Remove fails with
	// a non-IsNotExist error.
	if err := os.MkdirAll(credPath(home)+"/x", 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runAuthCmd(t, "auth", "logout")
	if exitCodeFrom(err) != 1 {
		t.Errorf("expected exit code 1 on delete failure, got %d; output:\n%s", exitCodeFrom(err), output)
	}
}
