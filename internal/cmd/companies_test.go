package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// strPtrCmd returns a *string for test helpers.
func strPtrCmd(s string) *string { return &s }

// mockCompanyResult is a helper to build JSON responses for tests.
type mockCompanyResult struct {
	CompanyName    string  `json:"company_name"`
	Symbol         string  `json:"symbol"`
	Exchange       string  `json:"exchange"`
	FilingCount    int     `json:"filing_count"`
	EarliestFiling *string `json:"earliest_filing"`
	LatestFiling   *string `json:"latest_filing"`
	IssuerKey      *string `json:"issuer_key"`
}

// researchCompaniesBody wraps results in the GET /research/companies envelope.
func researchCompaniesBody(results []mockCompanyResult) map[string]interface{} {
	if results == nil {
		results = []mockCompanyResult{}
	}
	return map[string]interface{}{"results": results, "truncated": false, "next_cursor": nil}
}

// serveCompanies answers GET /research/companies with the envelope and
// GET /companies (the catalog) with the bare list.
func serveCompanies(results []mockCompanyResult) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/research/companies" {
			_ = json.NewEncoder(w).Encode(researchCompaniesBody(results))
			return
		}
		_ = json.NewEncoder(w).Encode(results)
	}))
}

func runCompaniesCmd(args []string, srv *httptest.Server) (stdout, stderr string, err error) {
	t := os.TempDir()
	_ = t
	// Set the base URL to the test server.
	t2 := os.Getenv("ARCHIVIST_BASE_URL")
	_ = os.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	defer func() { _ = os.Setenv("ARCHIVIST_BASE_URL", t2) }()

	// Set a dummy token so auth.ResolveToken passes.
	t3 := os.Getenv("ARCHIVIST_TOKEN")
	_ = os.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	defer func() { _ = os.Setenv("ARCHIVIST_TOKEN", t3) }()

	root := NewRootCmd("0.1.0-test", "abc1234", "2026-05-19")
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetArgs(args)

	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestCompaniesSearchTableOutput(t *testing.T) {
	results := []mockCompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127,
			EarliestFiling: strPtrCmd("1994-09-29"), LatestFiling: strPtrCmd("2024-11-01"), IssuerKey: strPtrCmd("aapl_us")},
		{CompanyName: "Apple Hospitality", Symbol: "APPL:CA", Exchange: "TSX", FilingCount: 312,
			IssuerKey: strPtrCmd("appl_to")},
	}
	srv := serveCompanies(results)
	defer srv.Close()

	stdout, _, err := runCompaniesCmd([]string{"companies", "search", "--format", "table", "Apple"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Header and first row should be present.
	if !strings.Contains(stdout, "ISSUER_KEY") {
		t.Errorf("expected ISSUER_KEY header in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "aapl_us") {
		t.Errorf("expected aapl_us in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Apple Inc.") {
		t.Errorf("expected Apple Inc. in output:\n%s", stdout)
	}
	// Country derived from exchange NGS -> US.
	if !strings.Contains(stdout, "US") {
		t.Errorf("expected country US in output:\n%s", stdout)
	}
	// CA result.
	if !strings.Contains(stdout, "CA") {
		t.Errorf("expected country CA in output:\n%s", stdout)
	}
}

func TestCompaniesSearchJSONOutput(t *testing.T) {
	results := []mockCompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127,
			EarliestFiling: strPtrCmd("1994-09-29"), LatestFiling: strPtrCmd("2024-11-01"), IssuerKey: strPtrCmd("aapl_us")},
	}
	srv := serveCompanies(results)
	defer srv.Close()

	stdout, _, err := runCompaniesCmd([]string{"companies", "search", "--format", "json", "Apple"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out []map[string]interface{}
	if err2 := json.Unmarshal([]byte(stdout), &out); err2 != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err2, stdout)
	}
	if len(out) != 1 {
		t.Errorf("expected 1 result, got %d", len(out))
	}
	if out[0]["country"] != "US" {
		t.Errorf("expected country=US, got %v", out[0]["country"])
	}
	if out[0]["issuer_key"] != "aapl_us" {
		t.Errorf("expected issuer_key=aapl_us, got %v", out[0]["issuer_key"])
	}
	if out[0]["earliest_filing"] != "1994-09-29" {
		t.Errorf("expected earliest_filing in JSON output, got %v", out[0]["earliest_filing"])
	}
}

func TestCompaniesSearchCountryFilter(t *testing.T) {
	results := []mockCompanyResult{
		{CompanyName: "Apple Inc.", Exchange: "NGS", FilingCount: 4127, IssuerKey: strPtrCmd("aapl_us")},
		{CompanyName: "RBC", Exchange: "TSX", FilingCount: 3000, IssuerKey: strPtrCmd("ry_ca")},
		{CompanyName: "Shopify", Exchange: "TSX", FilingCount: 800, IssuerKey: strPtrCmd("shop_ca")},
		{CompanyName: "Microsoft", Exchange: "NSD", FilingCount: 3500, IssuerKey: strPtrCmd("msft_us")},
	}
	srv := serveCompanies(results)
	defer srv.Close()

	// --country CA should retain only TSX/TSV/CSE/NEO rows.
	stdout, _, err := runCompaniesCmd([]string{"companies", "search", "--format", "json", "--country", "CA", "Apple"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out []map[string]interface{}
	if err2 := json.Unmarshal([]byte(stdout), &out); err2 != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err2, stdout)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 CA results after filter, got %d:\n%s", len(out), stdout)
	}
	for _, row := range out {
		if row["country"] != "CA" {
			t.Errorf("expected country=CA, got %v", row["country"])
		}
	}
}

func TestCompaniesSearchInvalidCountry(t *testing.T) {
	srv := serveCompanies(nil)
	defer srv.Close()

	_, _, err := runCompaniesCmd([]string{"companies", "search", "--country", "XX", "Apple"}, srv)
	if err == nil {
		t.Fatal("expected error for invalid country, got nil")
	}
	var exitErr *ExitError
	if !isExitCode(err, ExitUsageError, &exitErr) {
		t.Errorf("expected ExitUsageError, got %T %v", err, err)
	}
}

func TestCompaniesSearchLimitClamp(t *testing.T) {
	var capturedLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(researchCompaniesBody([]mockCompanyResult{
			{CompanyName: "Apple Inc.", Exchange: "NGS", FilingCount: 100, IssuerKey: strPtrCmd("aapl_us")},
		}))
	}))
	defer srv.Close()

	_, _, _ = runCompaniesCmd([]string{"companies", "search", "--limit", "25", "--format", "table", "Apple"}, srv)
	if capturedLimit != "20" {
		t.Errorf("expected limit=20 after clamp, got %q", capturedLimit)
	}
}

func TestCompaniesSearchLimitZero(t *testing.T) {
	srv := serveCompanies(nil)
	defer srv.Close()

	_, _, err := runCompaniesCmd([]string{"companies", "search", "--limit", "0", "Apple"}, srv)
	if err == nil {
		t.Fatal("expected error for --limit 0, got nil")
	}
	var exitErr *ExitError
	if !isExitCode(err, ExitUsageError, &exitErr) {
		t.Errorf("expected ExitUsageError, got %T %v", err, err)
	}
}

func TestCompaniesSearchNoResults(t *testing.T) {
	srv := serveCompanies([]mockCompanyResult{})
	defer srv.Close()

	_, _, err := runCompaniesCmd([]string{"companies", "search", "--format", "table", "XyzCorpDoesNotExist"}, srv)
	if err == nil {
		t.Fatal("expected error for no results, got nil")
	}
	var exitErr *ExitError
	if !isExitCode(err, ExitNotFound, &exitErr) {
		t.Errorf("expected ExitNotFound, got %T %v", err, err)
	}
}

func TestCompaniesSearchDryRun(t *testing.T) {
	srv := serveCompanies(nil)
	defer srv.Close()

	stdout, _, err := runCompaniesCmd([]string{"companies", "search", "--dry-run", "Apple"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(stdout, "[dry-run] GET ") {
		t.Errorf("expected [dry-run] GET prefix, got: %q", stdout)
	}
	if !strings.Contains(stdout, "q=Apple") {
		t.Errorf("expected query in dry-run URL, got: %q", stdout)
	}
	if !strings.Contains(stdout, "/research/companies?") {
		t.Errorf("expected the research route in dry-run URL, got: %q", stdout)
	}
}

// TestCompaniesSearchUsesResearchRoute asserts the request path.
func TestCompaniesSearchUsesResearchRoute(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(researchCompaniesBody([]mockCompanyResult{
			{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 1, IssuerKey: strPtrCmd("cik:320193")},
		}))
	}))
	defer srv.Close()

	if _, _, err := runCompaniesCmd([]string{"companies", "search", "--format", "json", "Apple"}, srv); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/research/companies" {
		t.Errorf("path: got %q, want /research/companies", gotPath)
	}
}

// TestCompaniesSearchCLIQuotaExit7: the fair use refusal is exit 7 after one
// request, with the reset date on stderr and the JSON envelope on stdout.
func TestCompaniesSearchCLIQuotaExit7(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Queries-Remaining", "0")
		w.Header().Set("Retry-After", "2592000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA","suggestion":"The limit resets on 2026-11-01. Run 'archivist usage' for details.","limit":1000,"used":1000,"reset_date":"2026-11-01"}`))
	}))
	defer srv.Close()

	stdout, stderr, err := runCompaniesCmd([]string{"companies", "search", "--format", "json", "Apple"}, srv)
	var exitErr *ExitError
	if !isExitCode(err, ExitRateLimit, &exitErr) {
		t.Fatalf("expected exit 7, got %T %v", err, err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 request, got %d", calls)
	}
	if !strings.Contains(stderr, "Monthly fair use limit reached.") || !strings.Contains(stderr, "2026-11-01") {
		t.Errorf("stderr must name the error and reset date:\n%s", stderr)
	}
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON envelope: %v\n%s", err, stdout)
	}
	if env["error"] != "CLI_QUOTA" || env["exit_code"] != float64(ExitRateLimit) || env["reset_date"] != "2026-11-01" {
		t.Errorf("unexpected envelope: %v", env)
	}
}

// TestCompaniesSearchProRequiredExit4 maps the free account refusal to 4.
func TestCompaniesSearchProRequiredExit4(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`))
	}))
	defer srv.Close()

	_, stderr, err := runCompaniesCmd([]string{"companies", "search", "--format", "table", "Apple"}, srv)
	var exitErr *ExitError
	if !isExitCode(err, ExitAuthError, &exitErr) {
		t.Fatalf("expected exit 4, got %T %v", err, err)
	}
	if !strings.Contains(stderr, "https://mosaic-finance.com/en/pricing/") {
		t.Errorf("stderr missing account URL:\n%s", stderr)
	}
}

func TestCompaniesGetFound(t *testing.T) {
	results := []mockCompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127,
			EarliestFiling: strPtrCmd("1994-09-29"), LatestFiling: strPtrCmd("2024-11-01"), IssuerKey: strPtrCmd("aapl_us")},
	}
	srv := serveCompanies(results)
	defer srv.Close()

	// Use --format table explicitly: the test output buffer is not a TTY so the default would be json.
	stdout, _, err := runCompaniesCmd([]string{"companies", "get", "--format", "table", "aapl_us"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "Apple Inc.") {
		t.Errorf("expected Apple Inc. in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "aapl_us") {
		t.Errorf("expected aapl_us in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NGS") {
		t.Errorf("expected NGS exchange in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NASDAQ GS") {
		t.Errorf("expected NASDAQ GS display name in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "US") {
		t.Errorf("expected country US in output:\n%s", stdout)
	}
}

func TestCompaniesGetNotFound(t *testing.T) {
	// Pass 1 returns empty; pass 2 via /companies also returns empty.
	srv := serveCompanies([]mockCompanyResult{})
	defer srv.Close()

	_, stderr, err := runCompaniesCmd([]string{"companies", "get", "nonexistent_us"}, srv)
	if err == nil {
		t.Fatal("expected error for not-found issuer_key, got nil")
	}
	var exitErr *ExitError
	if !isExitCode(err, ExitNotFound, &exitErr) {
		t.Errorf("expected ExitNotFound, got %T %v", err, err)
	}
	if !strings.Contains(stderr, "nonexistent_us") {
		t.Errorf("expected issuer_key in stderr message, got: %q", stderr)
	}
}

func TestCompaniesGetJSONOutput(t *testing.T) {
	results := []mockCompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127,
			EarliestFiling: strPtrCmd("1994-09-29"), LatestFiling: strPtrCmd("2024-11-01"), IssuerKey: strPtrCmd("aapl_us")},
	}
	srv := serveCompanies(results)
	defer srv.Close()

	stdout, _, err := runCompaniesCmd([]string{"companies", "get", "--format", "json", "aapl_us"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	if err2 := json.Unmarshal([]byte(stdout), &out); err2 != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err2, stdout)
	}
	if out["issuer_key"] != "aapl_us" {
		t.Errorf("expected issuer_key=aapl_us, got %v", out["issuer_key"])
	}
	if out["country"] != "US" {
		t.Errorf("expected country=US, got %v", out["country"])
	}
}

func TestCompaniesGetDryRun(t *testing.T) {
	srv := serveCompanies(nil)
	defer srv.Close()

	stdout, _, err := runCompaniesCmd([]string{"companies", "get", "--dry-run", "aapl_us"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(stdout, "[dry-run] GET ") {
		t.Errorf("expected [dry-run] prefix, got: %q", stdout)
	}
}

func TestCompaniesGetPassTwoFallback(t *testing.T) {
	// Pass 1 (/research/companies) returns a result with a different key;
	// pass 2 (/companies) contains the target key.
	target := mockCompanyResult{
		CompanyName: "TD Bank", Exchange: "TSX", FilingCount: 3000, IssuerKey: strPtrCmd("td_ca"),
	}
	decoy := mockCompanyResult{
		CompanyName: "TD Ameritrade", Exchange: "NSD", FilingCount: 1500, IssuerKey: strPtrCmd("tdamd_us"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/companies" {
			_ = json.NewEncoder(w).Encode([]mockCompanyResult{decoy, target})
			return
		}
		// /research/companies returns decoy (no exact key match for "td_ca")
		_ = json.NewEncoder(w).Encode(researchCompaniesBody([]mockCompanyResult{decoy}))
	}))
	defer srv.Close()

	stdout, _, err := runCompaniesCmd([]string{"companies", "get", "--format", "json", "td_ca"}, srv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	if err2 := json.Unmarshal([]byte(stdout), &out); err2 != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err2, stdout)
	}
	if out["issuer_key"] != "td_ca" {
		t.Errorf("expected issuer_key=td_ca from pass 2, got %v", out["issuer_key"])
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{4127, "4,127"},
		{10000, "10,000"},
		{100000, "100,000"},
		{1000000, "1,000,000"},
	}
	for _, tc := range cases {
		if got := formatCount(tc.n); got != tc.want {
			t.Errorf("formatCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestCompaniesSearchStdinFlag(t *testing.T) {
	results := []mockCompanyResult{
		{CompanyName: "Apple Inc.", Exchange: "NGS", FilingCount: 4127, IssuerKey: strPtrCmd("aapl_us")},
	}
	srv := serveCompanies(results)
	defer srv.Close()

	t2 := os.Getenv("ARCHIVIST_BASE_URL")
	_ = os.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	defer func() { _ = os.Setenv("ARCHIVIST_BASE_URL", t2) }()
	t3 := os.Getenv("ARCHIVIST_TOKEN")
	_ = os.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	defer func() { _ = os.Setenv("ARCHIVIST_TOKEN", t3) }()

	root := NewRootCmd("0.1.0-test", "abc1234", "2026-05-19")
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetIn(strings.NewReader("Apple\n"))
	root.SetArgs([]string{"companies", "search", "--stdin", "--format", "table"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(outBuf.String(), "aapl_us") {
		t.Errorf("expected aapl_us in table output:\n%s", outBuf.String())
	}
}

// TestCompaniesAnnotations confirms Cobra annotations are set for the whole companies tree.
func TestCompaniesAnnotations(t *testing.T) {
	root := NewRootCmd("dev", "unknown", "unknown")
	root.SetArgs([]string{"--help"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	_ = root.Execute()

	walkAndAssertAnnotations(t, root)
}

// isExitCode is a test helper to check if err is an *ExitError with a specific code.
func isExitCode(err error, code int, target **ExitError) bool {
	var e *ExitError
	if fmt.Sprintf("%T", err) == "*cmd.ExitError" {
		e2, ok := err.(*ExitError)
		if ok && e2.Code == code {
			*target = e2
			return true
		}
	}
	_ = e
	// Also try unwrapping.
	if e2, ok := err.(*ExitError); ok {
		if target != nil {
			*target = e2
		}
		return e2.Code == code
	}
	return false
}
