// Package resolver provides company name auto-resolution backed by the
// /companies/search endpoint. This stub is fulfilled by Story 36.5;
// the interface here matches the confirmed API from the OQ2 resolution.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Client is the minimal HTTP interface resolver needs. *client.Client satisfies
// this interface — the dependency is inverted so tests can inject a fake.
type Client interface {
	Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
}

// Candidate is a single company match returned by /companies/search.
type Candidate struct {
	IssuerKey   string `json:"issuer_key"`
	CompanyName string `json:"company_name"`
	Symbol      string `json:"symbol"`
	Exchange    string `json:"exchange"`
	Country     string // derived client-side from exchange code
	FilingCount int    `json:"filing_count"`
}

// Resolution holds the outcome of a single name resolution.
type Resolution struct {
	IssuerKey   string
	CompanyName string
	Exchange    string
	Country     string
	Candidates  []Candidate
}

// AutoResolve resolves a company name or issuer_key.
//
// Issuer-key bypass: if name has no spaces and matches ^[a-z0-9_]+$, the
// value is returned as-is without a network call.
//
// Unambiguous rule: top result filing_count >= 10 AND >= 5x the second result
// (or only 1 result). Ambiguous otherwise — callers should exit 6.
//
// verbHint is included in the rerun suggestion on ambiguous match so the
// error message reads correctly.
func AutoResolve(ctx context.Context, c Client, name, verbHint string) (Resolution, error) {
	// Issuer-key bypass: no spaces, only [a-z0-9_].
	if isIssuerKey(name) {
		return Resolution{IssuerKey: name, CompanyName: name}, nil
	}

	path := "/companies/search?q=" + url.QueryEscape(name)
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Resolution{}, fmt.Errorf("companies/search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Resolution{}, fmt.Errorf("companies/search returned %d", resp.StatusCode)
	}

	var candidates []Candidate
	if err := json.NewDecoder(resp.Body).Decode(&candidates); err != nil {
		return Resolution{}, fmt.Errorf("decode companies/search: %w", err)
	}

	// Derive country from exchange code (CA exchanges: TSX, TSXV, CSE, NEO).
	for i := range candidates {
		candidates[i].Country = deriveCountry(candidates[i].Exchange)
	}

	if len(candidates) == 0 {
		return Resolution{Candidates: candidates}, ErrNoMatch
	}

	top := candidates[0]
	if len(candidates) == 1 {
		return Resolution{
			IssuerKey:   top.IssuerKey,
			CompanyName: top.CompanyName,
			Exchange:    top.Exchange,
			Country:     top.Country,
			Candidates:  candidates,
		}, nil
	}

	// Ambiguous if top doesn't have enough filing count dominance.
	second := candidates[1]
	if top.FilingCount >= 10 && second.FilingCount > 0 && top.FilingCount >= 5*second.FilingCount {
		return Resolution{
			IssuerKey:   top.IssuerKey,
			CompanyName: top.CompanyName,
			Exchange:    top.Exchange,
			Country:     top.Country,
			Candidates:  candidates,
		}, nil
	}
	if top.FilingCount >= 10 && second.FilingCount == 0 {
		return Resolution{
			IssuerKey:   top.IssuerKey,
			CompanyName: top.CompanyName,
			Exchange:    top.Exchange,
			Country:     top.Country,
			Candidates:  candidates,
		}, nil
	}

	return Resolution{Candidates: candidates}, ErrAmbiguous{Query: name, VerbHint: verbHint, Candidates: candidates}
}

// ErrNoMatch is returned when AutoResolve finds zero candidates.
var ErrNoMatch = fmt.Errorf("no company found")

// ErrAmbiguous is returned when AutoResolve finds multiple candidates with no
// clear winner.
type ErrAmbiguous struct {
	Query      string
	VerbHint   string
	Candidates []Candidate
}

func (e ErrAmbiguous) Error() string {
	return fmt.Sprintf("ambiguous match for %q — use an explicit issuer_key", e.Query)
}

// isIssuerKey returns true when name contains only lowercase alphanumeric and
// underscore characters (no spaces), indicating it's already an issuer_key.
func isIssuerKey(name string) bool {
	if name == "" {
		return false
	}
	for _, ch := range name {
		lower := ch >= 'a' && ch <= 'z'
		digit := ch >= '0' && ch <= '9'
		under := ch == '_'
		if !lower && !digit && !under {
			return false
		}
	}
	return true
}

// deriveCountry maps an exchange code to a two-letter country code.
func deriveCountry(exchange string) string {
	switch exchange {
	case "TSX", "TSXV", "CSE", "NEO":
		return "CA"
	default:
		return "US"
	}
}
