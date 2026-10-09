package cmd

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/client"
)

// TestHTTPFailureMatrix drives `search` through every mapped HTTP failure
// and asserts the exit code and stderr content (story 73.7 I/O matrix).
func TestHTTPFailureMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantExit   int
		wantCalls  int32 // 0 = do not check
		wantStderr []string
		notStderr  []string
	}{
		{
			name:   "fair use ceiling",
			status: 429,
			headers: map[string]string{
				"X-Queries-Remaining": "0",
				"Retry-After":         "2592000",
			},
			body:       `{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA","suggestion":"The limit resets on 2026-11-01. Run 'archivist usage' for details.","limit":1000,"used":1000,"reset_date":"2026-11-01"}`,
			wantExit:   ExitRateLimit,
			wantCalls:  1,
			wantStderr: []string{"Monthly fair use limit reached. [CLI_QUOTA]", "2026-11-01"},
		},
		{
			name:       "free account",
			status:     403,
			body:       `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`,
			wantExit:   ExitAuthError,
			wantStderr: []string{"This requires a Mosaic Pro account. [PRO_REQUIRED]", "https://mosaic-finance.com/en/pricing/"},
		},
		{
			name:       "free account without account_url falls back",
			status:     403,
			body:       `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED"}`,
			wantExit:   ExitAuthError,
			wantStderr: []string{client.AccountURL},
		},
		{
			name:       "inference refused",
			status:     403,
			body:       `{"error":"Chat and tables run only in the Mosaic web app.","code":"NO_INFERENCE_ON_CLI","suggestion":"Ask this in the Mosaic web app."}`,
			wantExit:   ExitGenericError,
			wantStderr: []string{"Chat and tables run only in the Mosaic web app. [NO_INFERENCE_ON_CLI]", "Ask this in the Mosaic web app."},
		},
		{
			name:       "other 403",
			status:     403,
			body:       `{"error":"Nope.","code":"SOMETHING_ELSE","suggestion":"Try later."}`,
			wantExit:   ExitAuthError,
			wantStderr: []string{"Nope. [SOMETHING_ELSE]", "Try later."},
		},
		{
			name:       "legacy paywall",
			status:     402,
			body:       `{"error":"Upgrade to continue.","code":"PAYWALL","suggestion":"See plans.","account_url":"https://mosaic-finance.com/en/pricing/"}`,
			wantExit:   ExitAuthError,
			wantStderr: []string{"Upgrade to continue. [PAYWALL]", "See plans.", "https://mosaic-finance.com/en/pricing/"},
			notStderr:  []string{"quota exceeded"},
		},
		{
			name:       "legacy paywall without body",
			status:     402,
			body:       ``,
			wantExit:   ExitAuthError,
			notStderr:  []string{"quota exceeded"},
			wantStderr: []string{"HTTP 402"},
		},
		{
			name:       "bad credential",
			status:     401,
			body:       `{"error":"Unauthorized"}`,
			wantExit:   ExitAuthError,
			wantStderr: []string{"authentication failed. Run 'archivist auth status'"},
		},
		{
			name:       "bad request",
			status:     400,
			body:       `{"error":"'q' is required, 1 to 2000 characters.","code":"BAD_REQUEST"}`,
			wantExit:   ExitUsageError,
			wantStderr: []string{"'q' is required, 1 to 2000 characters. [BAD_REQUEST]"},
		},
		{
			name:       "bad cursor",
			status:     400,
			body:       `{"error":"Cursor does not match.","code":"BAD_CURSOR"}`,
			wantExit:   ExitUsageError,
			wantStderr: []string{"Cursor does not match. [BAD_CURSOR]"},
		},
		{
			name:       "unprocessable",
			status:     422,
			body:       `{"error":"Bad thing.","code":"VALIDATION"}`,
			wantExit:   ExitUsageError,
			wantStderr: []string{"Bad thing. [VALIDATION]"},
		},
		{
			name:       "unknown id",
			status:     404,
			body:       `{"error":"Passage not found.","code":"NOT_FOUND"}`,
			wantExit:   ExitNotFound,
			wantStderr: []string{"Passage not found. [NOT_FOUND]"},
		},
		{
			name:       "conflict",
			status:     409,
			body:       `{"error":"Conflict.","code":"CONFLICT"}`,
			wantExit:   ExitGenericError,
			wantStderr: []string{"Conflict. [CONFLICT]"},
		},
		{
			name:       "too large",
			status:     413,
			body:       `not json`,
			wantExit:   ExitGenericError,
			wantStderr: []string{"server returned HTTP 413 [HTTP_ERROR]"},
		},
		{
			name:       "server down after retries",
			status:     503,
			body:       `{"error":"Filing search is temporarily unavailable.","code":"SEARCH_UNAVAILABLE"}`,
			wantExit:   ExitServerError,
			wantCalls:  4,
			wantStderr: []string{"Filing search is temporarily unavailable.", "SEARCH_UNAVAILABLE"},
		},
		{
			// hf-date-fix: a search 504 is not retried and suggests one period.
			name:       "search timeout without retry",
			status:     504,
			body:       `{"error":"Archivist timed out.","code":"ARCHIVIST_TIMEOUT"}`,
			wantExit:   ExitServerError,
			wantCalls:  1,
			wantStderr: []string{"Archivist timed out.", "ARCHIVIST_TIMEOUT", "--date-from"},
		},
		{
			name:       "old binary",
			status:     200,
			headers:    map[string]string{"X-Archivist-Min-CLI-Version": "9.0.0"},
			body:       `{"results":[]}`,
			wantExit:   ExitServerError,
			wantStderr: []string{"Server requires archivist-cli >= 9.0.0", "Run 'archivist update' to upgrade."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStub(t, tc.status, tc.headers, tc.body)
			_, stderr, code := runVerb(t, srv.URL, "search", "risk", "--format", "table")
			if code != tc.wantExit {
				t.Errorf("exit: got %d, want %d\nstderr:\n%s", code, tc.wantExit, stderr)
			}
			if tc.wantCalls != 0 && srv.calls.Load() != tc.wantCalls {
				t.Errorf("requests: got %d, want %d", srv.calls.Load(), tc.wantCalls)
			}
			for _, s := range tc.wantStderr {
				if !strings.Contains(stderr, s) {
					t.Errorf("stderr missing %q:\n%s", s, stderr)
				}
			}
			for _, s := range tc.notStderr {
				if strings.Contains(strings.ToLower(stderr), s) {
					t.Errorf("stderr must not contain %q:\n%s", s, stderr)
				}
			}
		})
	}
}

// TestBurstLimitRetriesThenExit7: a non-quota 429 keeps the GET retry policy
// (Retry-After honored) and then fails with exit 7.
func TestBurstLimitRetriesThenExit7(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on Retry-After")
	}
	srv := newStub(t, 429, map[string]string{"Retry-After": "1"},
		`{"error":"Rate limit exceeded","code":"rate_limit_exceeded","suggestion":"Retry after 1s or run 'archivist usage' to check your quota."}`)
	stdout, stderr, code := runVerb(t, srv.URL, "search", "risk", "--format", "json")
	if code != ExitRateLimit {
		t.Errorf("exit: got %d, want 7\n%s", code, stderr)
	}
	if n := srv.calls.Load(); n != 4 {
		t.Errorf("requests: got %d, want 4 (1 + 3 GET retries)", n)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON envelope: %v\n%s", err, stdout)
	}
	if env["error"] != "rate_limit_exceeded" || env["exit_code"] != float64(ExitRateLimit) {
		t.Errorf("envelope: %v", env)
	}
}

// TestNetworkErrorExit5: an unreachable server is exit 5.
func TestNetworkErrorExit5(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, stderr, code := runVerb(t, url, "toc", testFilingID)
	if code != ExitServerError {
		t.Errorf("exit: got %d, want 5\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "network error") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// TestJSONModeFailureEnvelope: with --format json every mapped failure also
// writes {error, message, exit_code, suggestion?, account_url?, reset_date?}
// to stdout.
func TestJSONModeFailureEnvelope(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   map[string]any
	}{
		{429, `{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA","suggestion":"Resets soon.","reset_date":"2026-11-01"}`,
			map[string]any{"error": "CLI_QUOTA", "message": "Monthly fair use limit reached.", "exit_code": float64(7), "suggestion": "Resets soon.", "reset_date": "2026-11-01"}},
		{403, `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`,
			map[string]any{"error": "PRO_REQUIRED", "message": "This requires a Mosaic Pro account.", "exit_code": float64(4), "account_url": "https://mosaic-finance.com/en/pricing/"}},
		{404, `{"error":"Passage not found.","code":"NOT_FOUND"}`,
			map[string]any{"error": "NOT_FOUND", "message": "Passage not found.", "exit_code": float64(3)}},
		{401, ``,
			map[string]any{"error": "UNAUTHORIZED", "message": "authentication failed. Run 'archivist auth status'", "exit_code": float64(4)}},
	}
	for _, tc := range cases {
		srv := newStub(t, tc.status, nil, tc.body)
		stdout, _, code := runVerb(t, srv.URL, "read", "passage", testChunkID, "--format", "json")
		if code != int(tc.want["exit_code"].(float64)) {
			t.Errorf("status %d: exit %d", tc.status, code)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("status %d: stdout is not JSON: %v\n%s", tc.status, err, stdout)
		}
		if mustJSON(t, got) != mustJSON(t, tc.want) {
			t.Errorf("status %d: envelope\ngot  %s\nwant %s", tc.status, mustJSON(t, got), mustJSON(t, tc.want))
		}
	}
}

// TestNoCredentialExit4: with no flag, env or file credential, research and
// usage verbs exit 4 with login guidance (and the JSON envelope in JSON
// mode) before any request.
func TestNoCredentialExit4(t *testing.T) {
	srv := newStub(t, 200, nil, "{}")
	cases := [][]string{
		{"search", "risk", "--format", "table"},
		{"search", "risk", "--format", "json"},
		{"usage"},
	}
	for _, argv := range cases {
		setHome(t, t.TempDir())
		t.Setenv("ARCHIVIST_TOKEN", "")
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		root := NewRootCmd("0.2.22", "abc1234", "2026-10-01")
		var out, errBuf strings.Builder
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetArgs(argv)
		err := root.Execute()
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != ExitAuthError {
			t.Errorf("%v: want exit 4, got %v", argv, err)
		}
		if !strings.Contains(errBuf.String(), "auth login --token") || !strings.Contains(errBuf.String(), "[NO_CREDENTIAL]") {
			t.Errorf("%v: stderr missing login guidance:\n%s", argv, errBuf.String())
		}
		// usage is not a TTY here, so it defaults to JSON like --format json.
		if argv[len(argv)-1] == "table" {
			if out.Len() != 0 {
				t.Errorf("%v: table mode must keep stdout empty, got %q", argv, out.String())
			}
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(out.String()), &env); err != nil {
			t.Fatalf("%v: stdout is not the JSON envelope: %v\n%s", argv, err, out.String())
		}
		if env["error"] != "NO_CREDENTIAL" || env["exit_code"] != float64(ExitAuthError) {
			t.Errorf("%v: envelope %v", argv, env)
		}
	}
	if n := srv.calls.Load(); n != 0 {
		t.Errorf("no-credential runs sent %d requests", n)
	}
}

// TestNoDeadMosaicURLsInSource: outside comments, no non-test Go source
// contains mosaic-finance.com except the AccountURL constant definition.
// Permalinks come from the server; every other page the binary names is
// client.AccountURL. The agent relay endpoint (Story 78.16) is a websocket
// service, not a page, and is the only other allowed definition.
func TestNoDeadMosaicURLsInSource(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	const allowed = `const AccountURL = "https://mosaic-finance.com/en/pricing/"`
	const relayEndpoint = `const DefaultRelayURL = "wss://relay.mosaic-finance.com"`
	found := 0
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !strings.Contains(line, "mosaic-finance.com") {
				continue
			}
			if trimmed == allowed {
				found++
				continue
			}
			if trimmed == relayEndpoint && strings.HasSuffix(filepath.ToSlash(path), "internal/connect/relay.go") {
				continue
			}
			t.Errorf("%s:%d names mosaic-finance.com outside AccountURL: %s", path, i+1, trimmed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Errorf("expected exactly one AccountURL definition, found %d", found)
	}
}
