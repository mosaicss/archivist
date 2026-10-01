// Package resolver wraps the chat-api company lookup (GET /research/companies)
// and the exchange to country helpers the companies verbs use.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/mosaicss/archivist/internal/client"
)

// Client is the minimal interface the resolver needs from the HTTP client.
// The concrete *client.Client from internal/client satisfies this interface.
type Client interface {
	Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
}

// CompanyResult is one company as GET /research/companies (and GET /companies) returns it.
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

// SearchCompanies calls GET /research/companies?q=&limit= and returns its
// results. A non-2xx response is returned as a *client.APIError for the
// caller's exit-code mapper; a Client.Do error is wrapped with %w.
func SearchCompanies(ctx context.Context, c Client, query string, limit int) ([]CompanyResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", fmt.Sprintf("%d", limit))
	path := "/research/companies?" + q.Encode()

	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("companies search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, client.ParseAPIError(resp)
	}

	var body struct {
		Results []CompanyResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return body.Results, nil
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
