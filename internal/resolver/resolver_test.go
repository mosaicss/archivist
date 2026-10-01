package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mosaicss/archivist/internal/client"
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

func strPtr(s string) *string { return &s }

// TestSearchCompaniesResearchRoute asserts the lookup calls
// GET /research/companies?q=&limit= and decodes the {results} envelope.
func TestSearchCompaniesResearchRoute(t *testing.T) {
	var gotPath, gotQ, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQ = r.URL.Query().Get("q")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []CompanyResult{
				{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127, IssuerKey: strPtr("cik:320193")},
			},
			"truncated":   false,
			"next_cursor": nil,
		})
	}))
	defer srv.Close()

	results, err := SearchCompanies(context.Background(), &mockClient{server: srv}, "Apple & Co", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/research/companies" {
		t.Errorf("path: got %q, want /research/companies", gotPath)
	}
	if gotQ != "Apple & Co" || gotLimit != "7" {
		t.Errorf("query: got q=%q limit=%q", gotQ, gotLimit)
	}
	if len(results) != 1 || results[0].Symbol != "AAPL:US" || results[0].IssuerKeyStr() != "cik:320193" {
		t.Errorf("unexpected results: %+v", results)
	}
}

// TestSearchCompaniesHTTPErrorIsAPIError asserts a non-2xx response comes back
// as a *client.APIError carrying the status and the body's code.
func TestSearchCompaniesHTTPErrorIsAPIError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   string
	}{
		{http.StatusUnauthorized, ``, ""},
		{http.StatusForbidden, `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`, "PRO_REQUIRED"},
		{http.StatusTooManyRequests, `{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA","reset_date":"2026-11-01"}`, "CLI_QUOTA"},
		{http.StatusBadRequest, `{"error":"'q' is required","code":"BAD_REQUEST"}`, "BAD_REQUEST"},
		{http.StatusBadGateway, `not json`, ""},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := SearchCompanies(context.Background(), &mockClient{server: srv}, "Apple", 5)
		srv.Close()
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("status %d: want *client.APIError, got %T (%v)", tc.status, err, err)
		}
		if apiErr.Status != tc.status || apiErr.Code != tc.code {
			t.Errorf("status %d: got status=%d code=%q, want code %q", tc.status, apiErr.Status, apiErr.Code, tc.code)
		}
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
