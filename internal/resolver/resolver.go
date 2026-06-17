// Package resolver implements company entity resolution for the archivist CLI.
// It wraps the chat-api GET /companies/search endpoint and applies the
// unambiguous-match heuristic (filing-count ratio) before returning a resolved
// issuer_key or a candidate list with exit-6 semantics.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Literal issuer_key forms recognized by NormalizeIssuerKey / IsLiteralIssuerKey.
// Inputs matching any of these MUST bypass AutoResolve — they are already
// canonical issuer references, not free-text search prompts.
//
// Story 37.5 added cikLiteralRE and uuidLiteralRE after the table verb's old
// `^[a-z0-9_]+$` bypass missed `cik:NNN` (Apple → resolved to Citigroup).
// Session 2026-05-21 added sedarLiteralRE after discovering Canadian-only
// filers (Aritzia, Pambili, etc.) use the `sedar:NNNNNNNNN` form which was
// falling through to AutoResolve and fuzzy-matching to preferred-stock series.
// Story 41.10 added symbolCountryColonRE: the human-typed ticker:country
// shorthand `SYMBOL:CA` (the market/QuoteMedia convention, e.g. "GSKR:CA")
// fuzzed into the ambiguity list because only the underscore wire form
// `gskr_ca` was recognized. It is normalized to the underscore form rather
// than matched as-is — the backend wire form is lowercase-underscore.
var (
	cikLiteralRE         = regexp.MustCompile(`^cik:[0-9]+$`)
	sedarLiteralRE       = regexp.MustCompile(`^sedar:[0-9]+$`)
	uuidLiteralRE        = regexp.MustCompile(`^uuid:[0-9a-f-]{8,}$`)
	symbolCountryRE      = regexp.MustCompile(`^[a-z0-9.]+_(us|ca)$`)
	symbolCountryColonRE = regexp.MustCompile(`^[a-z0-9.]+:(us|ca)$`)
)

// NormalizeIssuerKey reports whether s is (or normalizes to) a canonical literal
// issuer_key and returns the wire form. Five forms are recognized:
//
//   - cik:NNN              — SEC Central Index Key (US + cross-listed Canadian)
//   - sedar:NNN            — SEDAR Canadian-only filer (e.g. "sedar:000039556")
//   - uuid:HEX             — UUID for issuers without a CIK/SEDAR (CDRs, funds)
//   - <symbol>_(us|ca)     — legacy symbol+country wire form (e.g. "aapl_us")
//   - <SYMBOL>:(US|CA)     — colon ticker:country shorthand (e.g. "GSKR:CA")
//
// The first four pass through byte-for-byte (canonical == input). The colon
// shorthand is normalized to the underscore wire form (lowercased, single
// `:` → `_`): "GSKR:CA" → "gskr_ca". Returns ("", false) for free-text, which
// the caller resolves via AutoResolve. The cik:/sedar:/uuid: forms are matched
// before the colon branch, so their colons are preserved.
func NormalizeIssuerKey(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if cikLiteralRE.MatchString(s) || sedarLiteralRE.MatchString(s) ||
		uuidLiteralRE.MatchString(s) || symbolCountryRE.MatchString(s) {
		return s, true
	}
	if lower := strings.ToLower(s); symbolCountryColonRE.MatchString(lower) {
		return strings.Replace(lower, ":", "_", 1), true
	}
	return "", false
}

// IsLiteralIssuerKey reports whether s is already (or normalizes to) a canonical
// issuer_key literal and should bypass AutoResolve. It is a thin wrapper over
// NormalizeIssuerKey; callers that need the normalized wire form (e.g. the
// colon shorthand SYMBOL:CA → symbol_ca) should use NormalizeIssuerKey directly.
//
// Callers (chat verb, table verb) should check this BEFORE calling AutoResolve
// to avoid sending a literal id through the fuzzy company-search path.
func IsLiteralIssuerKey(s string) bool {
	_, ok := NormalizeIssuerKey(s)
	return ok
}

// Client is the minimal interface the resolver needs from the HTTP client.
// The concrete *client.Client from internal/client satisfies this interface.
type Client interface {
	Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
}

// CompanyResult is the shape returned by GET /companies/search.
type CompanyResult struct {
	CompanyName    string  `json:"company_name"`
	Symbol         string  `json:"symbol"`
	Exchange       string  `json:"exchange"`
	FilingCount    int     `json:"filing_count"`
	EarliestFiling *string `json:"earliest_filing"`
	LatestFiling   *string `json:"latest_filing"`
	IssuerKey      *string `json:"issuer_key"`
}

// IssuerKeyStr returns the IssuerKey as a string, or "" if nil.
func (r CompanyResult) IssuerKeyStr() string {
	if r.IssuerKey == nil {
		return ""
	}
	return *r.IssuerKey
}

// Resolution is the output of AutoResolve. Exactly one of IssuerKey or
// Candidates is populated.
type Resolution struct {
	// Unambiguous: IssuerKey is set, Candidates is nil.
	IssuerKey   string // e.g. "aapl_us"
	CompanyName string // e.g. "Apple Inc."
	Exchange    string // e.g. "NGS"
	Country     string // "CA" or "US" (derived from exchange)

	// Ambiguous or not-found: Candidates is set, IssuerKey is "".
	Candidates []Candidate
}

// Candidate is one entry in an ambiguous or not-found resolution result.
type Candidate struct {
	IssuerKey   string // may be empty for nil issuer_key rows
	CompanyName string
	Symbol      string
	Exchange    string
	Country     string // "CA" or "US"
	FilingCount int
}

// AmbiguousEnvelope is the JSON structure emitted to stdout on exit 6 in JSON mode.
type AmbiguousEnvelope struct {
	Error      string      `json:"error"`
	Query      string      `json:"query"`
	Candidates []Candidate `json:"candidates"`
	Suggestion string      `json:"suggestion"`
}

// AutoResolve resolves a free-text company name to a canonical issuer_key.
//
// It calls GET /companies/search?q=<name>&limit=5, applies the filing-count
// ratio heuristic, and returns either a resolved IssuerKey (unambiguous) or a
// Candidates list (ambiguous / not-found). Callers should check whether
// Resolution.IssuerKey is non-empty to distinguish the two outcomes.
//
// verbHint is injected into the suggestion field of the ambiguous envelope so
// chat and table can provide contextual rerun suggestions. Pass "" from
// companies search (no suggestion needed; the table output is the resolution surface).
func AutoResolve(ctx context.Context, c Client, name, verbHint string) (Resolution, error) {
	results, err := searchCompanies(ctx, c, name, 5)
	if err != nil {
		return Resolution{}, err
	}

	if len(results) == 0 {
		suggestion := fmt.Sprintf("No companies match %q. Try a different name or use --country to filter.", name)
		return Resolution{
			Candidates: []Candidate{},
		}, &AmbiguousError{
			Envelope: AmbiguousEnvelope{
				Error:      "not_found",
				Query:      name,
				Candidates: []Candidate{},
				Suggestion: suggestion,
			},
		}
	}

	if isUnambiguous(results) {
		top := results[0]
		return Resolution{
			IssuerKey:   top.IssuerKeyStr(),
			CompanyName: top.CompanyName,
			Exchange:    top.Exchange,
			Country:     CountryFor(top.Exchange),
		}, nil
	}

	// Ambiguous: build candidate list.
	candidates := make([]Candidate, 0, len(results))
	for _, r := range results {
		candidates = append(candidates, Candidate{
			IssuerKey:   r.IssuerKeyStr(),
			CompanyName: r.CompanyName,
			Symbol:      r.Symbol,
			Exchange:    r.Exchange,
			Country:     CountryFor(r.Exchange),
			FilingCount: r.FilingCount,
		})
	}

	suggestion := buildSuggestion(name, candidates, verbHint)
	return Resolution{
		Candidates: candidates,
	}, &AmbiguousError{
		Envelope: AmbiguousEnvelope{
			Error:      "ambiguous_match",
			Query:      name,
			Candidates: candidates,
			Suggestion: suggestion,
		},
	}
}

// AmbiguousError is returned by AutoResolve when resolution is ambiguous or
// not-found. Callers check errors.As(err, &AmbiguousError{}) to distinguish
// from network/server errors. The Envelope field contains the full exit-6 payload.
type AmbiguousError struct {
	Envelope AmbiguousEnvelope
}

func (e *AmbiguousError) Error() string {
	if e.Envelope.Error == "not_found" {
		return fmt.Sprintf("no companies match %q", e.Envelope.Query)
	}
	return fmt.Sprintf("ambiguous match: %d candidates for %q", len(e.Envelope.Candidates), e.Envelope.Query)
}

// SearchCompanies calls GET /companies/search and returns the results.
// Exported so companies.go can use it directly for the search subcommand.
func SearchCompanies(ctx context.Context, c Client, query string, limit int) ([]CompanyResult, error) {
	return searchCompanies(ctx, c, query, limit)
}

func searchCompanies(ctx context.Context, c Client, query string, limit int) ([]CompanyResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", fmt.Sprintf("%d", limit))
	path := "/companies/search?" + q.Encode()

	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("companies search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("auth error: token invalid or revoked")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("rate limit exceeded")
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("server error %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var results []CompanyResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return results, nil
}

// isUnambiguous applies the filing-count ratio heuristic.
// Top result must have >= 10 filings AND be at least 5x the second result's
// filing count (or be the only result with a non-nil issuer_key).
func isUnambiguous(results []CompanyResult) bool {
	if len(results) == 0 {
		return false
	}
	top := results[0]
	// Null issuer_key means we can't auto-resolve.
	if top.IssuerKey == nil || *top.IssuerKey == "" {
		return false
	}
	if len(results) == 1 {
		return true
	}
	if top.FilingCount < 10 {
		return false
	}
	second := results[1]
	return top.FilingCount >= 5*second.FilingCount
}

// FilterByCountry filters results to those matching the given country code ("CA" or "US").
func FilterByCountry(results []CompanyResult, country string) []CompanyResult {
	exchanges := ExchangesForCountry(country)
	exchangeSet := make(map[string]bool, len(exchanges))
	for _, ex := range exchanges {
		exchangeSet[ex] = true
	}
	filtered := results[:0:0] // share no backing array with results
	for _, r := range results {
		if exchangeSet[r.Exchange] {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func buildSuggestion(query string, candidates []Candidate, verbHint string) string {
	if verbHint == "" || len(candidates) == 0 {
		return ""
	}
	// Use the first candidate's issuer_key as the example.
	exampleKey := candidates[0].IssuerKey
	if exampleKey == "" && len(candidates) > 1 {
		exampleKey = candidates[1].IssuerKey
	}
	return fmt.Sprintf("Pick one and re-run with: %s %s", verbHint, exampleKey)
}
