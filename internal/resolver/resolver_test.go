package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockClient implements Client using an httptest.Server for full round-trip tests.
type mockClient struct {
	server *httptest.Server
}

func (m *mockClient) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.server.URL+path, body)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func newMockClient(results []CompanyResult) *mockClient {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
	}))
	return &mockClient{server: srv}
}

func strPtr(s string) *string { return &s }

func TestAutoResolveUnambiguous(t *testing.T) {
	// Apple Inc. 4127 vs some small company 312 = 13.2x -> unambiguous
	results := []CompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127, IssuerKey: strPtr("aapl_us")},
		{CompanyName: "Apple Hospitality", Symbol: "APPL:CA", Exchange: "TSX", FilingCount: 312, IssuerKey: strPtr("appl_to")},
	}
	mc := newMockClient(results)
	defer mc.server.Close()

	res, err := AutoResolve(context.Background(), mc, "Apple", "")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if res.IssuerKey != "aapl_us" {
		t.Errorf("expected IssuerKey=aapl_us, got %q", res.IssuerKey)
	}
	if res.CompanyName != "Apple Inc." {
		t.Errorf("expected CompanyName=Apple Inc., got %q", res.CompanyName)
	}
	if res.Country != "US" {
		t.Errorf("expected Country=US, got %q", res.Country)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("expected no Candidates on unambiguous result")
	}
}

func TestAutoResolveAmbiguous(t *testing.T) {
	// 4127 vs 2800 = 1.47x -> ambiguous (both major banks)
	results := []CompanyResult{
		{CompanyName: "Citigroup", Symbol: "C:US", Exchange: "NYE", FilingCount: 4127, IssuerKey: strPtr("c_us")},
		{CompanyName: "Citizens Financial", Symbol: "CFG:US", Exchange: "NYE", FilingCount: 2800, IssuerKey: strPtr("cfg_us")},
	}
	mc := newMockClient(results)
	defer mc.server.Close()

	res, err := AutoResolve(context.Background(), mc, "Citi", "archivist chat \"\" --company")
	if err == nil {
		t.Fatal("expected AmbiguousError, got nil")
	}
	var ambigErr *AmbiguousError
	if !errors.As(err, &ambigErr) {
		t.Fatalf("expected *AmbiguousError, got %T", err)
	}
	if ambigErr.Envelope.Error != "ambiguous_match" {
		t.Errorf("expected envelope error=ambiguous_match, got %q", ambigErr.Envelope.Error)
	}
	if len(ambigErr.Envelope.Candidates) != 2 {
		t.Errorf("expected 2 candidates, got %d", len(ambigErr.Envelope.Candidates))
	}
	if res.IssuerKey != "" {
		t.Errorf("expected empty IssuerKey on ambiguous result")
	}
	if len(res.Candidates) != 2 {
		t.Errorf("expected 2 Candidates in Resolution, got %d", len(res.Candidates))
	}
}

func TestAutoResolveNoResults(t *testing.T) {
	mc := newMockClient([]CompanyResult{})
	defer mc.server.Close()

	_, err := AutoResolve(context.Background(), mc, "XyzCorpDoesNotExist", "")
	if err == nil {
		t.Fatal("expected AmbiguousError, got nil")
	}
	var ambigErr *AmbiguousError
	if !errors.As(err, &ambigErr) {
		t.Fatalf("expected *AmbiguousError, got %T", err)
	}
	if ambigErr.Envelope.Error != "not_found" {
		t.Errorf("expected envelope error=not_found, got %q", ambigErr.Envelope.Error)
	}
	if len(ambigErr.Envelope.Candidates) != 0 {
		t.Errorf("expected 0 candidates for not_found")
	}
	if ambigErr.Envelope.Query != "XyzCorpDoesNotExist" {
		t.Errorf("expected query=XyzCorpDoesNotExist, got %q", ambigErr.Envelope.Query)
	}
}

func TestAutoResolveNullIssuerKey(t *testing.T) {
	// Top result has nil issuer_key -> treat as ambiguous
	results := []CompanyResult{
		{CompanyName: "Some Corp", Symbol: "SC:US", Exchange: "NYE", FilingCount: 5000, IssuerKey: nil},
	}
	mc := newMockClient(results)
	defer mc.server.Close()

	_, err := AutoResolve(context.Background(), mc, "some corp", "")
	if err == nil {
		t.Fatal("expected AmbiguousError on null issuer_key, got nil")
	}
	var ambigErr *AmbiguousError
	if !errors.As(err, &ambigErr) {
		t.Fatalf("expected *AmbiguousError, got %T", err)
	}
}

func TestAutoResolveJSONEnvelope(t *testing.T) {
	results := []CompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 100, IssuerKey: strPtr("aapl_us")},
		{CompanyName: "Apple Hospitality", Symbol: "APPL:CA", Exchange: "TSX", FilingCount: 90, IssuerKey: strPtr("appl_to")},
	}
	mc := newMockClient(results)
	defer mc.server.Close()

	verbHint := `archivist chat "" --company`
	_, err := AutoResolve(context.Background(), mc, "Apple", verbHint)
	var ambigErr *AmbiguousError
	if !errors.As(err, &ambigErr) {
		t.Fatalf("expected *AmbiguousError, got %T", err)
	}

	// Verify JSON round-trip of the envelope
	data, err2 := json.Marshal(ambigErr.Envelope)
	if err2 != nil {
		t.Fatalf("marshal envelope: %v", err2)
	}
	var out AmbiguousEnvelope
	if err2 = json.Unmarshal(data, &out); err2 != nil {
		t.Fatalf("unmarshal envelope: %v", err2)
	}
	if out.Error != "ambiguous_match" {
		t.Errorf("got error=%q, want ambiguous_match", out.Error)
	}
	if out.Query != "Apple" {
		t.Errorf("got query=%q, want Apple", out.Query)
	}
	if len(out.Candidates) != 2 {
		t.Errorf("got %d candidates, want 2", len(out.Candidates))
	}
	if out.Candidates[0].IssuerKey != "aapl_us" {
		t.Errorf("first candidate issuer_key=%q, want aapl_us", out.Candidates[0].IssuerKey)
	}
	if out.Suggestion == "" {
		t.Error("expected non-empty suggestion")
	}
}

func TestExchangeMappings(t *testing.T) {
	// Hardcode the expected canonical set from apps/web/lib/exchange-constants.ts
	// (last reconciled 2026-05-01). Update this test when web adds new exchanges.
	wantToCountry := map[string]string{
		"TSX": "CA", "TSV": "CA", "CSE": "CA", "NEO": "CA",
		"NGS": "US", "NSD": "US", "NSC": "US", "NYE": "US", "AMX": "US",
	}
	wantDisplayNames := map[string]string{
		"TSX": "TSX", "TSV": "TSX-V", "CSE": "CSE", "NEO": "NEO",
		"NGS": "NASDAQ GS", "NSD": "NASDAQ GM", "NSC": "NASDAQ CM",
		"NYE": "NYSE", "AMX": "NYSE American",
	}

	for code, country := range wantToCountry {
		if got := CountryFor(code); got != country {
			t.Errorf("CountryFor(%q) = %q, want %q", code, got, country)
		}
	}
	// Check no extra codes slipped in
	for code := range exchangeToCountry {
		if _, ok := wantToCountry[code]; !ok {
			t.Errorf("unexpected code %q in exchangeToCountry (not in canonical set)", code)
		}
	}

	for code, name := range wantDisplayNames {
		if got := DisplayNameFor(code); got != name {
			t.Errorf("DisplayNameFor(%q) = %q, want %q", code, got, name)
		}
	}
	for code := range exchangeDisplayNames {
		if _, ok := wantDisplayNames[code]; !ok {
			t.Errorf("unexpected code %q in exchangeDisplayNames (not in canonical set)", code)
		}
	}
}

func TestIsUnambiguous(t *testing.T) {
	cases := []struct {
		name    string
		results []CompanyResult
		want    bool
	}{
		{
			name:    "empty results",
			results: []CompanyResult{},
			want:    false,
		},
		{
			name: "single result with key",
			results: []CompanyResult{
				{FilingCount: 5, IssuerKey: strPtr("abc_us")},
			},
			want: true,
		},
		{
			name: "single result with nil key",
			results: []CompanyResult{
				{FilingCount: 100, IssuerKey: nil},
			},
			want: false,
		},
		{
			name: "top < 10 filings",
			results: []CompanyResult{
				{FilingCount: 8, IssuerKey: strPtr("abc_us")},
				{FilingCount: 1, IssuerKey: strPtr("xyz_us")},
			},
			want: false,
		},
		{
			name: "ratio exactly 5x",
			results: []CompanyResult{
				{FilingCount: 50, IssuerKey: strPtr("abc_us")},
				{FilingCount: 10, IssuerKey: strPtr("xyz_us")},
			},
			want: true,
		},
		{
			name: "ratio below 5x",
			results: []CompanyResult{
				{FilingCount: 49, IssuerKey: strPtr("abc_us")},
				{FilingCount: 10, IssuerKey: strPtr("xyz_us")},
			},
			want: false,
		},
		{
			name: "13x ratio (Apple Inc. vs Apple Hospitality)",
			results: []CompanyResult{
				{FilingCount: 4127, IssuerKey: strPtr("aapl_us")},
				{FilingCount: 312, IssuerKey: strPtr("appl_to")},
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnambiguous(tc.results); got != tc.want {
				t.Errorf("isUnambiguous() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterByCountry(t *testing.T) {
	results := []CompanyResult{
		{CompanyName: "Apple Inc.", Exchange: "NGS", IssuerKey: strPtr("aapl_us")},
		{CompanyName: "RBC", Exchange: "TSX", IssuerKey: strPtr("ry_ca")},
		{CompanyName: "Microsoft", Exchange: "NSD", IssuerKey: strPtr("msft_us")},
		{CompanyName: "Shopify", Exchange: "TSX", IssuerKey: strPtr("shop_ca")},
	}

	ca := FilterByCountry(results, "CA")
	if len(ca) != 2 {
		t.Errorf("CA filter: expected 2 results, got %d", len(ca))
	}
	for _, r := range ca {
		if CountryFor(r.Exchange) != "CA" {
			t.Errorf("CA filter returned non-CA exchange %q", r.Exchange)
		}
	}

	us := FilterByCountry(results, "US")
	if len(us) != 2 {
		t.Errorf("US filter: expected 2 results, got %d", len(us))
	}
	for _, r := range us {
		if CountryFor(r.Exchange) != "US" {
			t.Errorf("US filter returned non-US exchange %q", r.Exchange)
		}
	}
}

func TestCountryFor(t *testing.T) {
	cases := []struct{ exchange, want string }{
		{"TSX", "CA"}, {"TSV", "CA"}, {"CSE", "CA"}, {"NEO", "CA"},
		{"NGS", "US"}, {"NSD", "US"}, {"NSC", "US"}, {"NYE", "US"}, {"AMX", "US"},
		{"UNKNOWN", ""},
	}
	for _, tc := range cases {
		if got := CountryFor(tc.exchange); got != tc.want {
			t.Errorf("CountryFor(%q) = %q, want %q", tc.exchange, got, tc.want)
		}
	}
}

func TestDisplayNameFor(t *testing.T) {
	cases := []struct{ exchange, want string }{
		{"NGS", "NASDAQ GS"}, {"NYE", "NYSE"}, {"TSX", "TSX"}, {"TSV", "TSX-V"},
		{"UNKNOWN", "UNKNOWN"}, // unknown codes return the code itself
	}
	for _, tc := range cases {
		if got := DisplayNameFor(tc.exchange); got != tc.want {
			t.Errorf("DisplayNameFor(%q) = %q, want %q", tc.exchange, got, tc.want)
		}
	}
}

func TestSearchCompaniesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	mc := &mockClient{server: srv}

	_, err := SearchCompanies(context.Background(), mc, "Apple", 5)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestSearchCompaniesUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	mc := &mockClient{server: srv}

	_, err := SearchCompanies(context.Background(), mc, "Apple", 5)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}

// TestBuildSuggestion verifies verbHint injection.
func TestBuildSuggestion(t *testing.T) {
	candidates := []Candidate{{IssuerKey: "aapl_us"}, {IssuerKey: "appl_to"}}
	hint := `archivist chat "" --company`
	want := fmt.Sprintf("Pick one and re-run with: %s aapl_us", hint)
	got := buildSuggestion("Apple", candidates, hint)
	if got != want {
		t.Errorf("buildSuggestion() = %q, want %q", got, want)
	}

	// Empty verbHint returns empty suggestion.
	if got := buildSuggestion("Apple", candidates, ""); got != "" {
		t.Errorf("expected empty suggestion with empty verbHint, got %q", got)
	}
}

// TestIsLiteralIssuerKey covers AC3 + AC4: literal issuer_key forms bypass the
// resolver, free-text inputs do not. Regression test for Story 37.5 — the
// table-verb's old bypass regex `^[a-z0-9_]+$` silently mis-routed `cik:NNN`
// and `uuid:UUID` inputs through AutoResolve, surfacing wrong companies.
func TestIsLiteralIssuerKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// AC3 — four literal forms must short-circuit
		{"cik literal — Apple", "cik:320193", true},
		{"cik literal — Citigroup", "cik:831001", true},
		{"cik literal — short cik", "cik:1", true},
		{"cik literal — leading zeros (real form)", "cik:0002024459", true},
		{"sedar literal — Aritzia", "sedar:000039556", true},
		{"sedar literal — Pambili", "sedar:000023867", true},
		{"sedar literal — Morguard", "sedar:000004260", true},
		{"sedar literal — Lithium Ionic", "sedar:000051325", true},
		{"uuid literal — full v4", "uuid:3162c889-bb75-49cf-b605-3295ae6e092d", true},
		{"uuid literal — minimum length", "uuid:abcdef01", true},
		{"symbol_country — us", "aapl_us", true},
		{"symbol_country — ca", "shop_ca", true},
		{"symbol_country — numeric in symbol", "brk1_us", true},
		{"symbol_country — dot in symbol", "brk.a_us", true},

		// AC4 — free-text MUST fall through
		{"free-text — single word", "Apple", false},
		{"free-text — multi-word", "Apple Inc", false},
		{"free-text — accidental colon", "Foo: Bar", false},

		// Adversarial — near-miss forms that should NOT be treated as literals
		{"cik without prefix", "320193", false},
		{"cik with letters", "cik:abc", false},
		{"cik trailing space", "cik:320193 ", false},
		{"sedar without prefix", "000039556", false},
		{"sedar with letters", "sedar:abc123", false},
		{"sedar uppercase prefix", "SEDAR:000039556", false},
		{"uuid wrong prefix", "guid:abcdef01", false},
		{"uuid too short", "uuid:abc", false},
		{"uuid uppercase hex", "uuid:ABCDEF01", false},
		{"symbol_country wrong country", "aapl_uk", false},
		{"symbol_country uppercase", "AAPL_US", false},
		{"symbol no country suffix", "aapl", false},
		{"empty string", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsLiteralIssuerKey(tc.in)
			if got != tc.want {
				t.Errorf("IsLiteralIssuerKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAutoResolveBypass_LiteralIssuerKeys is an integration-level guard: when
// the table-verb caller short-circuits a literal via IsLiteralIssuerKey BEFORE
// calling AutoResolve, no HTTP call is made and the value passes through
// unchanged. The test enforces the contract by using a server that fails the
// test if hit — a literal must never reach the wire.
func TestAutoResolveBypass_LiteralIssuerKeys(t *testing.T) {
	literals := []string{
		"cik:320193",
		"sedar:000039556",
		"uuid:3162c889-bb75-49cf-b605-3295ae6e092d",
		"aapl_us",
	}
	for _, lit := range literals {
		t.Run(lit, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("AutoResolve fired for literal %q (path=%s) — bypass must short-circuit before HTTP", lit, r.URL.String())
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			mc := &mockClient{server: srv}

			// Caller pattern (mirrors cmd/archivist/table.go:resolveCompanies):
			if IsLiteralIssuerKey(lit) {
				// Bypass — literal passes through unchanged.
				return
			}
			// If we get here the bypass failed; AutoResolve hitting the server fails the test.
			_, _ = AutoResolve(context.Background(), mc, lit, "")
		})
	}
}

