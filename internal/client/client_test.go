package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/client"
)

func newTestClient(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	c := client.New("mc_pat_testtoken", "0.2.0")
	c.BaseURL = srv.URL
	return c
}

func TestClientHeaders(t *testing.T) {
	var gotReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	body := strings.NewReader(`{}`)
	resp, err := c.Do(context.Background(), http.MethodPost, "/chat", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if auth := gotReq.Header.Get("Authorization"); auth != "Bearer mc_pat_testtoken" {
		t.Errorf("Authorization: got %q, want %q", auth, "Bearer mc_pat_testtoken")
	}
	if ver := gotReq.Header.Get("X-Archivist-CLI-Version"); ver != "0.2.0" {
		t.Errorf("X-Archivist-CLI-Version: got %q, want 0.2.0", ver)
	}
	if origin := gotReq.Header.Get("X-Archivist-Origin"); origin == "" {
		t.Error("X-Archivist-Origin: missing")
	}
	if ua := gotReq.Header.Get("User-Agent"); !strings.HasPrefix(ua, "archivist-cli/") {
		t.Errorf("User-Agent: got %q, want archivist-cli/... prefix", ua)
	}
	if ct := gotReq.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
}

func TestRetryPolicy_POST_503TriggersOneRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), http.MethodPost, "/chat", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Errorf("expected 2 calls (1 fail + 1 retry), got %d", calls)
	}
}

func TestRetryPolicy_POST_400DoesNotRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), http.MethodPost, "/chat", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 1 {
		t.Errorf("expected exactly 1 call (no retry on 400), got %d", calls)
	}
}

func TestRetryPolicy_GET_ThreeRetriesOn5xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), http.MethodGet, "/companies/search?q=test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 4 {
		t.Errorf("expected 4 calls (3 fail + 1 success), got %d", calls)
	}
}

func TestRetryPolicy_GET_ExhaustReturnExitCode5(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Do(context.Background(), http.MethodGet, "/companies/search", nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	var exitErr *client.ExitCodeError
	if !isExitCode(err, 5) {
		t.Errorf("expected exit code 5, got error: %v (type %T)", err, exitErr)
	}
}

func TestRetryAfterHeader_Clamped(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	start := time.Now()
	resp, err := c.Do(context.Background(), http.MethodGet, "/companies/search", nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected ~1s retry delay, got %v", elapsed)
	}
}

func TestMinVersionBlockExit5(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Archivist-Min-CLI-Version", "1.0.0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv) // version is 0.2.0
	var stderr bytes.Buffer
	c.SetStderr(&stderr)
	_, err := c.Do(context.Background(), http.MethodGet, "/test", nil)
	if err == nil {
		t.Fatal("expected error when server requires newer version")
	}
	if !isExitCode(err, 5) {
		t.Errorf("expected exit code 5, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "archivist update") {
		t.Errorf("expected upgrade message in stderr, got: %q", stderr.String())
	}
}

func TestQuotaRemainingPrinted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Queries-Remaining", "42")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	var stderr bytes.Buffer
	c.SetStderr(&stderr)
	resp, err := c.Do(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(stderr.String(), "42 queries remaining") {
		t.Errorf("expected quota line in stderr, got: %q", stderr.String())
	}
}

func TestQuotaRemainingQuietSuppressed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Queries-Remaining", "42")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.SetQuiet(true)
	var stderr bytes.Buffer
	c.SetStderr(&stderr)
	resp, err := c.Do(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if stderr.Len() != 0 {
		t.Errorf("expected silent stderr in quiet mode, got: %q", stderr.String())
	}
}

// isExitCode checks if err is an ExitCodeError with the given code.
func isExitCode(err error, code int) bool {
	e, ok := err.(*client.ExitCodeError)
	return ok && e.Code == code
}

func TestQueriesRemainingWarning_BelowFive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Queries-Remaining", "3")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	var stderr bytes.Buffer
	c.SetStderr(&stderr)
	resp, err := c.Do(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(stderr.String(), "Warning: 3 queries remaining") {
		t.Errorf("expected warning message in stderr, got: %q", stderr.String())
	}
}

// TestQueriesRemainingZero_SuccessPasses: a 200 carrying X-Queries-Remaining: 0
// is the last allowed call and must reach the caller (no early refusal).
func TestQueriesRemainingZero_SuccessPasses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Queries-Remaining", "0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	var stderr bytes.Buffer
	c.SetStderr(&stderr)
	resp, err := c.Do(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"results":[]}` {
		t.Errorf("body: got %q", body)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no quota line for 0 remaining, got: %q", stderr.String())
	}
}

// TestCLIQuota429_ReturnedWithoutRetry: the fair use refusal is returned at
// once with its body intact, despite a large Retry-After, after exactly one
// request.
func TestCLIQuota429_ReturnedWithoutRetry(t *testing.T) {
	const body = `{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA","suggestion":"The limit resets on 2026-11-01.","limit":1000,"used":1000,"reset_date":"2026-11-01"}`
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Queries-Remaining", "0")
		w.Header().Set("Retry-After", "2592000")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.SetStderr(&bytes.Buffer{})
	start := time.Now()
	resp, err := c.Do(context.Background(), http.MethodGet, "/research/search?q=x", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if time.Since(start) > 2*time.Second {
		t.Errorf("CLI_QUOTA must not wait for Retry-After")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 request, got %d", calls)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: got %d, want 429", resp.StatusCode)
	}
	apiErr := client.ParseAPIError(resp)
	if apiErr.Code != "CLI_QUOTA" || apiErr.ResetDate != "2026-11-01" || apiErr.Message != "Monthly fair use limit reached." {
		t.Errorf("re-buffered body not intact: %+v", apiErr)
	}
}

// TestBurst429_RetriesThenExit7: any other 429 keeps the retry policy and
// ends in exit 7 carrying the server code.
func TestBurst429_RetriesThenExit7(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Rate limit exceeded","code":"rate_limit_exceeded","suggestion":"Retry after 1s"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Do(context.Background(), http.MethodPost, "/x", strings.NewReader(`{}`))
	if !isExitCode(err, 7) {
		t.Fatalf("expected exit code 7, got: %v", err)
	}
	if calls != 2 {
		t.Errorf("POST: expected 2 requests (1 + 1 retry), got %d", calls)
	}
	var exitErr *client.ExitCodeError
	if errors.As(err, &exitErr) && exitErr.APICode != "rate_limit_exceeded" {
		t.Errorf("APICode: got %q, want rate_limit_exceeded", exitErr.APICode)
	}
}

// TestServerErrorCarriesServerMessage: a final 5xx keeps exit 5 and carries
// the server's error text and code.
func TestServerErrorCarriesServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"Search is unavailable.","code":"SEARCH_UNAVAILABLE"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Do(context.Background(), http.MethodPost, "/x", strings.NewReader(`{}`))
	var exitErr *client.ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("expected exit 5, got %v", err)
	}
	if exitErr.APICode != "SEARCH_UNAVAILABLE" || !strings.Contains(exitErr.Message, "Search is unavailable.") {
		t.Errorf("unexpected error: %+v", exitErr)
	}
}

func TestParseAPIErrorToleratesNonJSON(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("<html>bad gateway</html>"))}
	e := client.ParseAPIError(resp)
	if e.Status != http.StatusBadGateway || e.Code != "" || e.Message != "server returned HTTP 502" {
		t.Errorf("unexpected: %+v", e)
	}
}

func TestParseAPIErrorReadsAtMost64KiB(t *testing.T) {
	big := `{"error":"` + strings.Repeat("x", 70*1024) + `","code":"BIG"}`
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(big))}
	e := client.ParseAPIError(resp)
	// The truncated body is not valid JSON, so nothing decodes.
	if e.Code != "" {
		t.Errorf("expected the 64 KiB cap to cut the body, got code %q", e.Code)
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Setenv("ARCHIVIST_BASE_URL", "")
	if got := client.ResolveBaseURL(); got != client.DefaultBaseURL {
		t.Errorf("unset: got %q", got)
	}
	t.Setenv("ARCHIVIST_BASE_URL", "http://localhost:9")
	if got := client.ResolveBaseURL(); got != "http://localhost:9" {
		t.Errorf("set: got %q", got)
	}
}

func TestErrUnauthorizedNamesAccountURL(t *testing.T) {
	if !strings.Contains(client.ErrUnauthorized.Error(), client.AccountURL) {
		t.Errorf("ErrUnauthorized must point at AccountURL: %q", client.ErrUnauthorized.Error())
	}
}

func TestIsOlderVersion(t *testing.T) {
	cases := []struct {
		current, minimum string
		want             bool
	}{
		{"0.2.0", "1.0.0", true},
		{"1.0.0", "0.2.0", false},
		{"1.0.0", "1.0.0", false},
		{"v0.2.0", "v1.0.0", true},
		{"1.2.0", "1.3.0", true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_vs_%s", tc.current, tc.minimum), func(t *testing.T) {
			// We test via the server behavior: server says min=minimum, client has current
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("X-Archivist-Min-CLI-Version", tc.minimum)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := client.New("mc_pat_testtoken", tc.current)
			c.BaseURL = srv.URL
			c.SetStderr(&bytes.Buffer{})
			_, err := c.Do(context.Background(), http.MethodGet, "/test", nil)
			blocked := err != nil && isExitCode(err, 5)
			if blocked != tc.want {
				t.Errorf("current=%s minimum=%s: blocked=%v want=%v", tc.current, tc.minimum, blocked, tc.want)
			}
		})
	}
}
