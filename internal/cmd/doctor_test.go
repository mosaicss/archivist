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
	"github.com/spf13/cobra"
)

// makeDoctorRoot builds a root command pointing at the given test server URL.
// Set baseURL="" to use the real DefaultBaseURL (network unreachable in CI).
// withGoodSkill creates a temp SKILL.md with matching version so skill check passes.
func makeDoctorRoot(t *testing.T, baseURL string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	root := cmd.NewRootCmd("0.4.2", "abc1234", "2026-05-20")
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if baseURL != "" {
		_ = os.Setenv("ARCHIVIST_BASE_URL", baseURL)
		t.Cleanup(func() { _ = os.Unsetenv("ARCHIVIST_BASE_URL") })
	}
	// Point skill path to a non-existent location for most tests (consistent WARN)
	_ = os.Setenv("ARCHIVIST_SKILL_PATH", "/nonexistent/path/SKILL.md")
	t.Cleanup(func() { _ = os.Unsetenv("ARCHIVIST_SKILL_PATH") })
	return root, &buf
}

// makeDoctorRootWithSkill creates a root command with a valid matching skill file.
func makeDoctorRootWithSkill(t *testing.T, baseURL string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	f, err := os.CreateTemp("", "SKILL-*.md")
	if err != nil {
		t.Fatalf("could not create temp skill file: %v", err)
	}
	_, _ = f.WriteString("---\nversion: 0.4.2\n---\n# Archivist Skill\n")
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	root := cmd.NewRootCmd("0.4.2", "abc1234", "2026-05-20")
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if baseURL != "" {
		_ = os.Setenv("ARCHIVIST_BASE_URL", baseURL)
		t.Cleanup(func() { _ = os.Unsetenv("ARCHIVIST_BASE_URL") })
	}
	_ = os.Setenv("ARCHIVIST_SKILL_PATH", f.Name())
	t.Cleanup(func() { _ = os.Unsetenv("ARCHIVIST_SKILL_PATH") })
	return root, &buf
}

// cliTokensHandler returns a handler that mocks GET /account/cli-tokens.
func cliTokensHandler(statusCode int, body interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok","version":"1.0.0"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusCode)
			if body != nil {
				_ = json.NewEncoder(w).Encode(body)
			}
		default:
			http.NotFound(w, r)
		}
	}
}

func TestDoctorNoCredentials(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "")
	srv := httptest.NewServer(cliTokensHandler(http.StatusOK, map[string]interface{}{
		"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
	}))
	defer srv.Close()

	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "FAIL") {
		t.Errorf("expected FAIL in output; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "ARCHIVIST_TOKEN") {
		t.Errorf("expected ARCHIVIST_TOKEN mention in output; got:\n%s", buf.String())
	}
}

func TestDoctorMalformedToken(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "badtoken_not_mc_pat")
	srv := httptest.NewServer(cliTokensHandler(http.StatusOK, map[string]interface{}{
		"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
	}))
	defer srv.Close()

	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "ak_") {
		t.Errorf("expected ak_ in FAIL message; got:\n%s", buf.String())
	}
}

func TestDoctorServerUnreachable(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")
	// Use a port that no server is listening on
	root, buf := makeDoctorRoot(t, "http://127.0.0.1:1")
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 5 {
		t.Errorf("expected exit code 5, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "FAIL") {
		t.Errorf("expected FAIL for server check; got:\n%s", buf.String())
	}
}

func TestDoctorMinVersionBlock(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("X-Archivist-Min-CLI-Version", "0.9.0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Binary version is 0.4.2 (< 0.9.0)
	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 5 {
		t.Errorf("expected exit code 5, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "0.9.0") {
		t.Errorf("expected min version 0.9.0 in output; got:\n%s", buf.String())
	}
}

func TestDoctorTokenRevoked(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(cliTokensHandler(http.StatusUnauthorized, nil))
	defer srv.Close()

	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "revoked") || !strings.Contains(buf.String(), "invalid") {
		// check for at least one of these words
		output := buf.String()
		if !strings.Contains(output, "revoked") && !strings.Contains(output, "invalid") {
			t.Errorf("expected revoked/invalid in output; got:\n%s", output)
		}
	}
}

func TestDoctorNoCliScope(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(cliTokensHandler(http.StatusForbidden, nil))
	defer srv.Close()

	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "CLI scope") {
		t.Errorf("expected CLI scope in output; got:\n%s", buf.String())
	}
}

func TestDoctorQuotaExhausted(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("X-Queries-Remaining", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 0 {
		t.Errorf("expected exit code 0 (WARN only), got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("expected WARN for quota; got:\n%s", buf.String())
	}
}

func TestDoctorAllPass(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("X-Queries-Remaining", "412")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	root, buf := makeDoctorRootWithSkill(t, srv.URL)
	root.SetArgs([]string{"doctor"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "All checks passed") {
		t.Errorf("expected 'All checks passed'; got:\n%s", buf.String())
	}
}

func TestDoctorQuietNoOutput(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("X-Queries-Remaining", "412")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	root, buf := makeDoctorRootWithSkill(t, srv.URL)
	root.SetArgs([]string{"doctor", "--quiet"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d; output:\n%s", exitCode, buf.String())
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty stdout with --quiet + all pass; got:\n%s", buf.String())
	}
}

func TestDoctorQuietShowsFails(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "")

	srv := httptest.NewServer(cliTokensHandler(http.StatusOK, map[string]interface{}{
		"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
	}))
	defer srv.Close()

	root, buf := makeDoctorRoot(t, srv.URL)
	root.SetArgs([]string{"doctor", "--quiet"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 4 {
		t.Errorf("expected exit code 4, got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "FAIL") {
		t.Errorf("expected FAIL line in quiet+fail output; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Binary:") {
		t.Errorf("expected passing checks suppressed in quiet mode; got:\n%s", buf.String())
	}
}

func TestDoctorNoNetwork(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	root, buf := makeDoctorRoot(t, "http://127.0.0.1:1")
	root.SetArgs([]string{"doctor", "--no-network"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 0 {
		t.Errorf("expected exit code 0 (offline checks pass), got %d; output:\n%s", exitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "--no-network") {
		t.Errorf("expected --no-network note in output; got:\n%s", buf.String())
	}
}

func TestDoctorJSONOutput(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken123")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("X-Queries-Remaining", "412")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	root, buf := makeDoctorRootWithSkill(t, srv.URL)
	root.SetArgs([]string{"doctor", "--format", "json"})

	err := root.Execute()
	exitCode := exitCodeFrom(err)
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d", exitCode)
	}

	var report map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput:\n%s", err, buf.String())
	}

	requiredKeys := []string{"binary", "skill", "credentials", "user", "server", "min_version", "quota", "overall"}
	for _, k := range requiredKeys {
		if _, ok := report[k]; !ok {
			t.Errorf("JSON output missing key %q; keys: %v", k, keysOf(report))
		}
	}
}

func TestDoctorTokenNeverPrinted(t *testing.T) {
	rawToken := "mc_pat_secretsecretXYZ"
	t.Setenv("ARCHIVIST_TOKEN", rawToken)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/account/cli-tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_email": "test@example.com", "tier": "pro", "tokens": []interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Human output
	root, buf := makeDoctorRootWithSkill(t, srv.URL)
	root.SetArgs([]string{"doctor"})
	_ = root.Execute()
	if strings.Contains(buf.String(), rawToken) {
		t.Errorf("raw token appeared in human output:\n%s", buf.String())
	}

	// JSON output
	root2, buf2 := makeDoctorRootWithSkill(t, srv.URL)
	root2.SetArgs([]string{"doctor", "--format", "json"})
	_ = root2.Execute()
	if strings.Contains(buf2.String(), rawToken) {
		t.Errorf("raw token appeared in JSON output:\n%s", buf2.String())
	}
}

// exitCodeFrom extracts the exit code from an ExitError, or 0 for nil.
func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	unwrapped := err
	for unwrapped != nil {
		if e, ok := unwrapped.(*cmd.ExitError); ok {
			return e.Code
		}
		type unwrapErr interface{ Unwrap() error }
		if u, ok := unwrapped.(unwrapErr); ok {
			unwrapped = u.Unwrap()
		} else {
			break
		}
	}
	return 1
}

func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
