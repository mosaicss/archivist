package cmd

// companies.go implements `archivist companies search` and `archivist companies get`.
//
// Two-pass approach for `companies get`: first try GET /research/companies?q=<issuer_key>&limit=5
// and match by issuer_key field. If no match (e.g., short keys that fall back to ILIKE prefix
// search on company name, not issuer_key), fall back to GET /companies (full catalog, server-side
// 1h cache) and scan locally. Future devs: do NOT "optimize" this to a single pass without first
// confirming the endpoint returns reliable issuer_key exact-match results for short keys.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/resolver"
	"github.com/spf13/cobra"
)

func newCompaniesCmd(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "companies",
		Short: "Look up companies: find a symbol to pass to 'search --symbol'",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,2,3,4,5,7",
			"mcp:read-only":       "true",
		},
	}
	cmd.AddCommand(newCompaniesSearchCmd(version))
	cmd.AddCommand(newCompaniesGetCmd(version))
	return cmd
}

// companiesSearchFlags holds the parsed flags for the search subcommand.
type companiesSearchFlags struct {
	limit   int
	country string
	format  string
	compact bool
	dryRun  bool
	quiet   bool
	noColor bool
	stdin   bool
}

func newCompaniesSearchCmd(version string) *cobra.Command {
	var f companiesSearchFlags

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search companies by name or symbol",
		Long: `Search companies by name or symbol. Each result carries the symbol to pass
to 'archivist search --symbol', the exchange and country, and the filing count.`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,2,3,4,5,7",
			"mcp:read-only":       "true",
			"mcp:title":           "Find company",
		},
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompaniesSearch(cmd, args, version, &f)
		},
	}

	cmd.Flags().IntVar(&f.limit, "limit", 5, "Maximum results to return (1-20). Clamped to 20.")
	cmd.Flags().StringVar(&f.country, "country", "", "Filter results client-side by country: CA or US. Requests limit=20 from server for headroom.")
	cmd.Flags().StringVar(&f.format, "format", "", "Output format: table (default on TTY) or json")
	cmd.Flags().BoolVar(&f.compact, "compact", false, "Omit earliest/latest filing fields; reduce table columns")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request URL without making a network call. Exit 0 on valid flags.")
	cmd.Flags().BoolVar(&f.quiet, "quiet", false, "Suppress stderr progress messages")
	cmd.Flags().BoolVar(&f.noColor, "no-color", false, "Disable ANSI color in output")
	cmd.Flags().BoolVar(&f.stdin, "stdin", false, "Read the query from stdin instead of the positional argument")

	return cmd
}

func runCompaniesSearch(cmd *cobra.Command, args []string, version string, f *companiesSearchFlags) error {
	// Resolve query from args or --stdin.
	var query string
	if f.stdin {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error reading stdin: %v\n", err)
			return &ExitError{Code: ExitUsageError}
		}
		query = strings.TrimSpace(string(data))
	} else if len(args) > 0 {
		query = strings.TrimSpace(args[0])
	}
	if query == "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error: query argument is required (or use --stdin)")
		return &ExitError{Code: ExitUsageError}
	}

	// Validate --limit.
	if f.limit < 1 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error: --limit must be at least 1")
		return &ExitError{Code: ExitUsageError}
	}
	serverLimit := f.limit
	if serverLimit > 20 {
		serverLimit = 20 // silently clamp
	}
	// When country filter is set, request 20 from server for headroom.
	if f.country != "" {
		if f.country != "CA" && f.country != "US" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: --country must be CA or US (got %q)\n", f.country)
			return &ExitError{Code: ExitUsageError}
		}
		serverLimit = 20
	}

	// Resolve format: auto-json when stdout is not a TTY.
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")

	// Build the request URL for --dry-run or actual call.
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", fmt.Sprintf("%d", serverLimit))
	requestURL := client.ResolveBaseURL() + "/research/companies?" + q.Encode()

	if f.dryRun {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] GET %s\n", requestURL)
		return nil
	}

	c, err := newResearchClient(cmd, version, format)
	if err != nil {
		return err
	}
	c.SetQuiet(f.quiet)

	results, err := resolver.SearchCompanies(cmd.Context(), c, query, serverLimit)
	if err != nil {
		return failFromDo(cmd, err, format)
	}

	// Apply country filter.
	if f.country != "" {
		results = resolver.FilterByCountry(results, f.country)
	}

	if len(results) == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "No companies found for %q\n", query)
		return &ExitError{Code: ExitNotFound}
	}

	// Render output.
	if format == "json" {
		return renderSearchJSON(cmd, results, f.compact)
	}
	return renderSearchTable(cmd, results, f.compact)
}

func renderSearchTable(cmd *cobra.Command, results []resolver.CompanyResult, compact bool) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if compact {
		_, _ = fmt.Fprintln(w, "ISSUER_KEY\tNAME\tCOUNTRY\tFILINGS")
		for _, r := range results {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				r.IssuerKeyStr(), r.CompanyName,
				countryOrDash(r.Exchange), formatCount(r.FilingCount))
		}
	} else {
		_, _ = fmt.Fprintln(w, "ISSUER_KEY\tNAME\tEXCHANGE\tCOUNTRY\tFILINGS")
		for _, r := range results {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				r.IssuerKeyStr(), r.CompanyName, r.Exchange,
				countryOrDash(r.Exchange), formatCount(r.FilingCount))
		}
	}
	return w.Flush()
}

// companyResultJSON is the JSON output shape: CompanyResult + derived country field.
type companyResultJSON struct {
	CompanyName    string  `json:"company_name"`
	Symbol         string  `json:"symbol"`
	Exchange       string  `json:"exchange"`
	Country        string  `json:"country"`
	FilingCount    int     `json:"filing_count"`
	EarliestFiling *string `json:"earliest_filing,omitempty"`
	LatestFiling   *string `json:"latest_filing,omitempty"`
	IssuerKey      *string `json:"issuer_key"`
}

func toJSONResult(r resolver.CompanyResult, compact bool) companyResultJSON {
	out := companyResultJSON{
		CompanyName: r.CompanyName,
		Symbol:      r.Symbol,
		Exchange:    r.Exchange,
		Country:     resolver.CountryFor(r.Exchange),
		FilingCount: r.FilingCount,
		IssuerKey:   r.IssuerKey,
	}
	if !compact {
		out.EarliestFiling = r.EarliestFiling
		out.LatestFiling = r.LatestFiling
	}
	return out
}

func renderSearchJSON(cmd *cobra.Command, results []resolver.CompanyResult, compact bool) error {
	out := make([]companyResultJSON, len(results))
	for i, r := range results {
		out[i] = toJSONResult(r, compact)
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// companiesGetFlags holds the parsed flags for the get subcommand.
type companiesGetFlags struct {
	format  string
	compact bool
	dryRun  bool
	quiet   bool
	noColor bool
}

func newCompaniesGetCmd(version string) *cobra.Command {
	var f companiesGetFlags

	cmd := &cobra.Command{
		Use:   "get <issuer_key>",
		Short: "Get details for a single company by issuer key",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,2,3,4,5,7",
			"mcp:read-only":       "true",
			"mcp:title":           "Get company",
		},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompaniesGet(cmd, args[0], version, &f)
		},
	}

	cmd.Flags().StringVar(&f.format, "format", "", "Output format: table (default on TTY) or json")
	cmd.Flags().BoolVar(&f.compact, "compact", false, "Omit earliest/latest filing fields")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request URL without making a network call. Exit 0 on valid flags.")
	cmd.Flags().BoolVar(&f.quiet, "quiet", false, "Suppress stderr progress messages")
	cmd.Flags().BoolVar(&f.noColor, "no-color", false, "Disable ANSI color in output")

	return cmd
}

func runCompaniesGet(cmd *cobra.Command, issuerKey, version string, f *companiesGetFlags) error {
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")

	q := url.Values{}
	q.Set("q", issuerKey)
	q.Set("limit", "5")
	requestURL := client.ResolveBaseURL() + "/research/companies?" + q.Encode()

	if f.dryRun {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] GET %s\n", requestURL)
		return nil
	}

	c, err := newResearchClient(cmd, version, format)
	if err != nil {
		return err
	}
	c.SetQuiet(f.quiet)

	// Pass 1: search by issuer_key, check for exact match in results.
	match, err := getByIssuerKeyPass1(cmd.Context(), c, issuerKey)
	if err != nil {
		return failFromDo(cmd, err, format)
	}

	// Pass 2: if no match, fall back to GET /companies (full catalog, server-cached).
	if match == nil {
		match, err = getByIssuerKeyPass2(cmd.Context(), c, issuerKey)
		if err != nil {
			return failFromDo(cmd, err, format)
		}
	}

	if match == nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "No company found with issuer_key %q\n", issuerKey)
		return &ExitError{Code: ExitNotFound}
	}

	if format == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(toJSONResult(*match, f.compact))
	}
	return renderGetTable(cmd, *match)
}

// getByIssuerKeyPass1 calls GET /research/companies?q=<key>&limit=5 and looks for an exact match.
func getByIssuerKeyPass1(ctx context.Context, c *client.Client, issuerKey string) (*resolver.CompanyResult, error) {
	results, err := resolver.SearchCompanies(ctx, c, issuerKey, 5)
	if err != nil {
		return nil, err
	}
	for _, r := range results {
		if r.IssuerKeyStr() == issuerKey {
			r2 := r
			return &r2, nil
		}
	}
	return nil, nil
}

// getByIssuerKeyPass2 calls GET /companies (full list, server-cached 1h) and scans for the key.
func getByIssuerKeyPass2(ctx context.Context, c *client.Client, issuerKey string) (*resolver.CompanyResult, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/companies", nil)
	if err != nil {
		return nil, fmt.Errorf("companies list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, client.ParseAPIError(resp)
	}

	var all []resolver.CompanyResult
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, fmt.Errorf("decode companies list: %w", err)
	}
	for _, r := range all {
		if r.IssuerKeyStr() == issuerKey {
			r2 := r
			return &r2, nil
		}
	}
	return nil, nil
}

func renderGetTable(cmd *cobra.Command, r resolver.CompanyResult) error {
	displayName := resolver.DisplayNameFor(r.Exchange)
	country := resolver.CountryFor(r.Exchange)
	if country == "" {
		country = "--"
	}

	var filingLine string
	if r.EarliestFiling != nil && r.LatestFiling != nil {
		filingLine = fmt.Sprintf("%s (earliest %s, latest %s)", formatCount(r.FilingCount), *r.EarliestFiling, *r.LatestFiling)
	} else {
		filingLine = formatCount(r.FilingCount)
	}

	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"%s  (%s)\nExchange:   %s (%s)\nCountry:    %s\nFilings:    %s\n",
		r.CompanyName, r.IssuerKeyStr(),
		r.Exchange, displayName,
		country,
		filingLine,
	)
	return err
}

// countryOrDash returns the country code or "--" for unknown exchanges.
func countryOrDash(exchange string) string {
	c := resolver.CountryFor(exchange)
	if c == "" {
		return "--"
	}
	return c
}

// formatCount formats an integer with comma thousands separators.
func formatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	// Insert commas from the right.
	var b strings.Builder
	offset := len(s) % 3
	for i, ch := range s {
		if i > 0 && i%3 == offset {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	return b.String()
}
