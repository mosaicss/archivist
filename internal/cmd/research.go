package cmd

// research.go implements the model-free research verbs over chat-api's
// /research routes (Story 73.2, reference/archivist/api-contracts.md §10):
//
//	archivist search <query>                         GET /research/search
//	archivist read passage <chunk_id>                GET /research/passages/:chunkId
//	archivist read section <filing_id> <section>     GET /research/filings/:id/sections
//	archivist toc <filing_id>                        GET /research/filings/:id/toc
//	archivist filings <symbol>                       GET /research/filings (Story 81.4)
//	archivist find <filing_id> <term>                GET /research/filings/:id/find (Story 81.4)
//
// JSON output (the default off a TTY) is the server body re-indented, keys
// and values unchanged. Compact output (compact.go) is a minified projection
// fitted to a byte budget. Table output lists PassageRecords with their
// permalink. Inputs are checked against the server's bounds before any
// request (exit 2), and every HTTP failure goes through apierror.go.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/spf13/cobra"
)

// Server bounds (research-service.ts), mirrored so bad input fails before a
// request is spent against the fair use ceiling.
const (
	maxQueryChars         = 2000
	maxSymbolChars        = 20
	maxFormtypeChars      = 50
	maxListFormtypeChars  = 200
	maxFindTermChars      = 200
	minFindLimit          = 1
	maxFindLimit          = 10
	defaultFindLimit      = 5
	maxSectionHeaderChars = 255
	minSearchLimit        = 1
	maxSearchLimit        = 25
	defaultSearchLimit    = 10
	maxPassageWindow      = 2
	defaultPassageWindow  = 1
	sectionColumnRunes    = 32
	snippetColumnRunes    = 60
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// researchAnnotations are shared by every research verb.
func researchAnnotations(title, exitCodes string) map[string]string {
	return map[string]string{
		"pp:typed-exit-codes": exitCodes,
		"mcp:read-only":       "true",
		"mcp:title":           title,
	}
}

// PassageRecord is one passage as chat-api returns it (§10.2). Unknown
// values arrive as null. url is the only link; when held, the server also
// sends exchange_document_id and exchange_document_kind (an SEC accession
// number or a KAP disclosure index, plain text), which only the table view
// ignores: --format json and the MCP tools print the server body as is.
type PassageRecord struct {
	ID              *string `json:"id"`
	FilingID        *string `json:"filing_id"`
	CompanyName     *string `json:"company_name"`
	Symbol          *string `json:"symbol"`
	Exchange        *string `json:"exchange"`
	Formtype        *string `json:"formtype"`
	Formdescription *string `json:"formdescription"`
	Datefiled       *string `json:"datefiled"`
	SectionHeader   *string `json:"section_header"`
	ChunkIndex      *int    `json:"chunk_index"`
	Snippet         *string `json:"snippet"`
	URL             *string `json:"url"`
}

// page carries the continuation fields every research response has.
type page struct {
	Truncated  bool    `json:"truncated"`
	NextCursor *string `json:"next_cursor"`
}

type entityResolution struct {
	State      string  `json:"state"`
	Suggestion *string `json:"suggestion"`
}

type searchResponse struct {
	page
	Results          []PassageRecord   `json:"results"`
	EntityResolution *entityResolution `json:"entity_resolution"`
}

type passageResponse struct {
	page
	Passage    *PassageRecord  `json:"passage"`
	Neighbours []PassageRecord `json:"neighbours"`
}

type sectionResponse struct {
	page
	FilingID      string          `json:"filing_id"`
	SectionHeader string          `json:"section_header"`
	ChunkCount    int             `json:"chunk_count"`
	Passages      []PassageRecord `json:"passages"`
}

type tocResponse struct {
	page
	FilingID string   `json:"filing_id"`
	Sections []string `json:"sections"`
}

// jsLen is a string's JavaScript length (UTF-16 code units), the unit the
// server's character bounds are measured in.
func jsLen(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// usageErr prints a usage diagnostic and returns exit 2.
func usageErr(cmd *cobra.Command, format string, a ...any) error {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", cmd.CommandPath(), fmt.Sprintf(format, a...))
	return &ExitError{Code: ExitUsageError}
}

func checkFormat(cmd *cobra.Command, format string) error {
	switch format {
	case "", "json", "table", formatCompact:
		return nil
	}
	return usageErr(cmd, "--format must be table, json or compact (got %q)", format)
}

// cursorUsage is the --cursor help every research verb shares (Story 81.4:
// agents changed other arguments with a cursor, which the server refuses).
const cursorUsage = "next_cursor from the previous call; keep the other arguments unchanged"

// formatUsage is the --format help every research verb shares.
const formatUsage = "Output format: table (default on TTY), json (default off a TTY) or compact (minified, fitted to about 6k tokens)"

// pagedPath builds the request path from base and q, reading --cursor: a
// server cursor is sent as is; a compact CLI cursor (c1.) must belong to this
// request (its hash covers the path and query without the cursor) and sends
// the server cursor it wraps.
func pagedPath(cmd *cobra.Command, base string, q url.Values, rawCursor, format string) (string, pageCursor, error) {
	path := base
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	cur, err := parsePageCursor(cmd, rawCursor, format, requestHash(path))
	if err != nil {
		return "", cur, err
	}
	if cur.Server != "" {
		q.Set("cursor", cur.Server)
		path = base + "?" + q.Encode()
	}
	return path, cur, nil
}

// cursorHint tells the caller how to fetch the next page whenever the
// response carries a next_cursor (filings and find page by next_cursor alone).
func cursorHint(cmd *cobra.Command, next *string) {
	if next != nil && *next != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "More results: rerun with --cursor %s\n", *next)
	}
}

func checkUUID(cmd *cobra.Command, name, value string) error {
	if !uuidRE.MatchString(value) {
		return usageErr(cmd, "%s must be a UUID (got %q)", name, value)
	}
	return nil
}

func checkDate(cmd *cobra.Command, flag, value string) error {
	if value == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return usageErr(cmd, "--%s must be a date as YYYY-MM-DD (got %q)", flag, value)
	}
	return nil
}

// newResearchClient resolves the credential and builds a client whose
// diagnostics go to the command's stderr. A missing or malformed credential
// is exit 4.
func newResearchClient(cmd *cobra.Command, version, format string) (*client.Client, error) {
	tokenFlag, _ := cmd.Root().PersistentFlags().GetString("token")
	token, err := auth.ResolveToken(tokenFlag)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, auth.ErrNoToken) {
			msg = "no credential found. Run 'archivist auth login --token ak_...' to save one, or set ARCHIVIST_TOKEN."
		}
		return nil, reportFailure(cmd, failure{exitCode: ExitAuthError, code: "NO_CREDENTIAL", message: msg}, format)
	}
	c := client.New(token, version)
	c.SetStderr(cmd.ErrOrStderr())
	return c, nil
}

// searchTimeoutSuggestion replaces the server's suggestion on a search 504:
// a narrower search takes the cheap plan (hf-date-fix).
const searchTimeoutSuggestion = "Search one period at a time with --date-from and --date-to, " +
	"or add --formtype; repeating the same search times out again."

// fetchResearch runs one GET against chat-api and returns the 2xx body.
// --dry-run prints the request instead. A nil body with a nil error means the
// dry run already printed.
func fetchResearch(cmd *cobra.Command, version, path, format string, dryRun bool) ([]byte, error) {
	return fetchResearchGet(cmd, version, path, format, dryRun, false)
}

// fetchSearch is fetchResearch for GET /research/search: no retry after a 504
// or a client timeout (client.DoNoReplay), and a 504 suggests one period per
// search. The search verb and the mcp serve search tool both run it.
func fetchSearch(cmd *cobra.Command, version, path, format string, dryRun bool) ([]byte, error) {
	return fetchResearchGet(cmd, version, path, format, dryRun, true)
}

func fetchResearchGet(cmd *cobra.Command, version, path, format string, dryRun, search bool) ([]byte, error) {
	if dryRun {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] GET %s%s\n", client.ResolveBaseURL(), path)
		return nil, nil
	}
	c, err := newResearchClient(cmd, version, format)
	if err != nil {
		return nil, err
	}
	do := c.Do
	if search {
		do = c.DoNoReplay
	}
	resp, err := do(cmd.Context(), http.MethodGet, path, nil)
	if err != nil {
		var exitErr *client.ExitCodeError
		if search && errors.As(err, &exitErr) && exitErr.HTTPStatus == http.StatusGatewayTimeout {
			exitErr.Suggestion = searchTimeoutSuggestion
		}
		return nil, failFromDo(cmd, err, format)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, failFromResponse(cmd, resp, format)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, reportFailure(cmd, failure{
			exitCode: ExitServerError, code: "UNREADABLE_RESPONSE",
			message: fmt.Sprintf("could not read the response: %v", err),
		}, format)
	}
	return body, nil
}

// decodeBody decodes a 2xx body; an unparsable one is a server failure.
func decodeBody(cmd *cobra.Command, body []byte, v any, format string) error {
	if err := json.Unmarshal(body, v); err != nil {
		return reportFailure(cmd, failure{
			exitCode: ExitServerError, code: "UNREADABLE_RESPONSE",
			message: fmt.Sprintf("could not parse the response: %v", err),
		}, format)
	}
	return nil
}

// writeIndentedJSON writes the server body re-indented, keys and values
// unchanged.
func writeIndentedJSON(w io.Writer, body []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(body), "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// emitJSON writes the body to stdout, mapping a malformed body to exit 5.
func emitJSON(cmd *cobra.Command, body []byte, format string) error {
	if err := writeIndentedJSON(cmd.OutOrStdout(), body); err != nil {
		return reportFailure(cmd, failure{
			exitCode: ExitServerError, code: "UNREADABLE_RESPONSE",
			message: fmt.Sprintf("could not parse the response: %v", err),
		}, format)
	}
	return nil
}

// truncationHint tells the caller how to fetch the next page.
func truncationHint(cmd *cobra.Command, p page) {
	if p.Truncated && p.NextCursor != nil && *p.NextCursor != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "More results: rerun with --cursor %s\n", *p.NextCursor)
	}
}

// ─── table rendering ─────────────────────────────────────────────────────────

func cell(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// clipRunes collapses whitespace runs to one space and clips to max runes,
// marking a cut with "…".
func clipRunes(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes-1]) + "…"
}

func clipCell(s *string, maxRunes int) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return "-"
	}
	return clipRunes(*s, maxRunes)
}

// renderPassageTable writes the PassageRecord table:
// CHUNK_ID FILING_ID SYMBOL FORM DATE SECTION SNIPPET URL.
func renderPassageTable(w io.Writer, records []PassageRecord) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHUNK_ID\tFILING_ID\tSYMBOL\tFORM\tDATE\tSECTION\tSNIPPET\tURL")
	for _, r := range records {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			cell(r.ID), cell(r.FilingID), cell(r.Symbol), cell(r.Formtype), cell(r.Datefiled),
			clipCell(r.SectionHeader, sectionColumnRunes), clipCell(r.Snippet, snippetColumnRunes),
			cell(r.URL))
	}
	return tw.Flush()
}

// renderPassageTexts writes each passage's full snippet under a
// "--- [<chunk_index>] <id> ---" header, in the order given (callers sort
// with sortByChunkIndex first).
func renderPassageTexts(w io.Writer, records []PassageRecord) {
	for _, r := range records {
		idx := "-"
		if r.ChunkIndex != nil {
			idx = fmt.Sprintf("%d", *r.ChunkIndex)
		}
		_, _ = fmt.Fprintf(w, "\n--- [%s] %s ---\n", idx, cell(r.ID))
		if r.Snippet != nil {
			_, _ = fmt.Fprintln(w, *r.Snippet)
		}
	}
}

func chunkIndexLess(a, b PassageRecord) bool {
	switch {
	case a.ChunkIndex == nil:
		return false
	case b.ChunkIndex == nil:
		return true
	default:
		return *a.ChunkIndex < *b.ChunkIndex
	}
}

// ─── search ──────────────────────────────────────────────────────────────────

type searchFlags struct {
	symbol, formtype, dateFrom, dateTo, mode, cursor, format string
	limit                                                    int
	dryRun, stdin, latestOnly                                bool
}

func newSearchCmd(version string) *cobra.Command {
	var f searchFlags
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Search filing passages; every result carries a permalink to cite",
		Long: `Search global filing passages (SEC, SEDAR+, KAP and expanding). Each result
is a passage with its filing, company, form, date, section and a permalink url
that opens the passage in Mosaic's filing viewer. Filter with --symbol (e.g.
AAPL for a US listing, ABX:CA for a Canadian one; find one with 'companies
search'), --formtype, --date-from and --date-to. Set --latest-only with
--symbol and one --formtype to search only the newest filing of that form;
latest_filing names it. Page with --cursor when a response is truncated.
--mode broad searches without filters. For several years, search once per
period with --date-from/--date-to. Exit 3 when nothing matched, 6 when the symbol matches several
issuers (rerun with the exact symbol 'companies search' returns: bare for US
listings, ending in :CA or :TR otherwise).`,
		Args:        cobra.MaximumNArgs(1),
		Annotations: researchAnnotations("Search filings", "0,2,3,4,5,6,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd, args, version, &f)
		},
	}
	c.Flags().StringVar(&f.symbol, "symbol", "", "Limit to one company by symbol, e.g. AAPL (US) or ABX:CA (at most 20 characters)")
	c.Flags().StringVar(&f.formtype, "formtype", "", "Limit to one form type, e.g. 10-K (at most 50 characters)")
	c.Flags().StringVar(&f.dateFrom, "date-from", "", "Earliest filing date, YYYY-MM-DD")
	c.Flags().StringVar(&f.dateTo, "date-to", "", "Latest filing date, YYYY-MM-DD")
	c.Flags().BoolVar(&f.latestOnly, "latest-only", false, "Search only the newest filing of the formtype; needs symbol, takes no dates, semantic mode only")
	c.Flags().StringVar(&f.mode, "mode", "semantic", "Search mode: semantic (accepts filters) or broad (no filters)")
	c.Flags().IntVar(&f.limit, "limit", defaultSearchLimit, "Passages to return (1-25)")
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	c.Flags().BoolVar(&f.stdin, "stdin", false, "Read the query from stdin instead of the positional argument")
	return c
}

func runSearch(cmd *cobra.Command, args []string, version string, f *searchFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	var query string
	if f.stdin {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return usageErr(cmd, "reading stdin: %v", err)
		}
		query = strings.TrimSpace(string(data))
	} else if len(args) > 0 {
		query = strings.TrimSpace(args[0])
	}
	if query == "" {
		return usageErr(cmd, "a query is required (or use --stdin)")
	}
	if jsLen(query) > maxQueryChars {
		return usageErr(cmd, "the query must be at most %d characters", maxQueryChars)
	}
	if f.mode != "semantic" && f.mode != "broad" {
		return usageErr(cmd, "--mode must be semantic or broad (got %q)", f.mode)
	}
	if f.limit < minSearchLimit || f.limit > maxSearchLimit {
		return usageErr(cmd, "--limit must be from %d to %d (got %d)", minSearchLimit, maxSearchLimit, f.limit)
	}
	symbol := strings.TrimSpace(f.symbol)
	formtype := strings.TrimSpace(f.formtype)
	if jsLen(symbol) > maxSymbolChars {
		return usageErr(cmd, "--symbol must be at most %d characters", maxSymbolChars)
	}
	if jsLen(formtype) > maxFormtypeChars {
		return usageErr(cmd, "--formtype must be at most %d characters", maxFormtypeChars)
	}
	if err := checkDate(cmd, "date-from", f.dateFrom); err != nil {
		return err
	}
	if err := checkDate(cmd, "date-to", f.dateTo); err != nil {
		return err
	}
	if f.dateFrom != "" && f.dateTo != "" && f.dateFrom > f.dateTo {
		return usageErr(cmd, "--date-from must not be later than --date-to")
	}
	// Story 81.4: the server's own latest_only rules and wording, checked
	// before any request.
	if f.latestOnly {
		switch {
		case f.mode == "broad":
			return usageErr(cmd, "Filters (symbol, formtype, date_from, date_to, latest_only) are not supported with mode=broad.")
		case symbol == "":
			return usageErr(cmd, "'latest_only' needs symbol.")
		case f.dateFrom != "" || f.dateTo != "":
			return usageErr(cmd, "'latest_only' cannot be combined with date_from or date_to.")
		}
	}
	hasFilter := symbol != "" || formtype != "" || f.dateFrom != "" || f.dateTo != ""
	if f.mode == "broad" && hasFilter {
		return usageErr(cmd, "--mode broad takes no filters (--symbol, --formtype, --date-from, --date-to)")
	}

	q := url.Values{}
	q.Set("q", query)
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	if formtype != "" {
		q.Set("formtype", formtype)
	}
	if f.dateFrom != "" {
		q.Set("date_from", f.dateFrom)
	}
	if f.dateTo != "" {
		q.Set("date_to", f.dateTo)
	}
	if f.latestOnly {
		q.Set("latest_only", "true")
	}
	q.Set("mode", f.mode)
	q.Set("limit", fmt.Sprintf("%d", f.limit))

	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/search", q, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchSearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp searchResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}

	if len(resp.Results) == 0 {
		switch format {
		case "json":
			if err := emitJSON(cmd, body, format); err != nil {
				return err
			}
		case formatCompact:
			if err := emitCompact(cmd, body, "results", cur); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No passages found")
		if er := resp.EntityResolution; er != nil && er.Suggestion != nil && *er.Suggestion != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), *er.Suggestion)
		}
		if er := resp.EntityResolution; er != nil && er.State == "resolved_ambiguous" {
			return &ExitError{Code: ExitAmbiguousMatch}
		}
		return &ExitError{Code: ExitNotFound}
	}

	switch format {
	case formatCompact:
		return emitCompact(cmd, body, "results", cur)
	case "json":
		if err := emitJSON(cmd, body, format); err != nil {
			return err
		}
	default:
		if err := renderPassageTable(cmd.OutOrStdout(), resp.Results); err != nil {
			return err
		}
	}
	truncationHint(cmd, resp.page)
	return nil
}

// ─── read passage / read section ─────────────────────────────────────────────

func newReadCmd(version string) *cobra.Command {
	c := &cobra.Command{
		Use:   "read",
		Short: "Read a passage with its neighbours, or a whole filing section",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2",
		},
	}
	c.AddCommand(newReadPassageCmd(version))
	c.AddCommand(newReadSectionCmd(version))
	return c
}

type readPassageFlags struct {
	window         int
	cursor, format string
	dryRun         bool
}

func newReadPassageCmd(version string) *cobra.Command {
	var f readPassageFlags
	c := &cobra.Command{
		Use:   "passage <chunk_id>",
		Short: "Read one passage by chunk id, with its neighbouring passages",
		Long: `Read one passage (the id from a search result) together with up to
--window passages on each side, in filing order. Every passage carries its
permalink url.`,
		Args:        cobra.ExactArgs(1),
		Annotations: researchAnnotations("Read passage", "0,2,3,4,5,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReadPassage(cmd, args[0], version, &f)
		},
	}
	c.Flags().IntVar(&f.window, "window", defaultPassageWindow, "Neighbouring passages on each side (0-2)")
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	return c
}

func runReadPassage(cmd *cobra.Command, chunkID, version string, f *readPassageFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	chunkID = strings.TrimSpace(chunkID)
	if err := checkUUID(cmd, "chunk_id", chunkID); err != nil {
		return err
	}
	if f.window < 0 || f.window > maxPassageWindow {
		return usageErr(cmd, "--window must be from 0 to %d (got %d)", maxPassageWindow, f.window)
	}
	q := url.Values{}
	q.Set("window", fmt.Sprintf("%d", f.window))
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/passages/"+url.PathEscape(chunkID), q, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp passageResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}
	if format == formatCompact {
		// The head passage stays on every page; the neighbours list is fitted.
		return emitCompact(cmd, body, "neighbours", cur)
	}
	if format == "json" {
		if err := emitJSON(cmd, body, format); err != nil {
			return err
		}
	} else {
		var records []PassageRecord
		if resp.Passage != nil {
			records = append(records, *resp.Passage)
		}
		records = append(records, resp.Neighbours...)
		sortByChunkIndex(records)
		if err := renderPassageTable(cmd.OutOrStdout(), records); err != nil {
			return err
		}
		renderPassageTexts(cmd.OutOrStdout(), records)
	}
	truncationHint(cmd, resp.page)
	return nil
}

// sortByChunkIndex orders records by chunk_index (a stable insertion sort;
// records without an index go last, in their original order).
func sortByChunkIndex(records []PassageRecord) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && chunkIndexLess(records[j], records[j-1]); j-- {
			records[j], records[j-1] = records[j-1], records[j]
		}
	}
}

type readSectionFlags struct {
	cursor, format string
	dryRun         bool
}

func newReadSectionCmd(version string) *cobra.Command {
	var f readSectionFlags
	c := &cobra.Command{
		Use:   "section <filing_id> <section_header>",
		Short: "Read a whole filing section as its passages, in order",
		Long: `Read every passage of one filing section, in filing order. Take the
section_header from 'toc' or from a search result. Every passage carries its
permalink url.`,
		Args:        cobra.ExactArgs(2),
		Annotations: researchAnnotations("Read filing section", "0,2,3,4,5,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReadSection(cmd, args[0], args[1], version, &f)
		},
	}
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	return c
}

func runReadSection(cmd *cobra.Command, filingID, sectionHeader, version string, f *readSectionFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	filingID = strings.TrimSpace(filingID)
	if err := checkUUID(cmd, "filing_id", filingID); err != nil {
		return err
	}
	if strings.TrimSpace(sectionHeader) == "" || jsLen(sectionHeader) > maxSectionHeaderChars {
		return usageErr(cmd, "section_header must be 1 to %d characters", maxSectionHeaderChars)
	}
	q := url.Values{}
	q.Set("section_header", sectionHeader)
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/filings/"+url.PathEscape(filingID)+"/sections", q, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp sectionResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}
	if format == formatCompact {
		return emitCompact(cmd, body, "passages", cur)
	}
	if format == "json" {
		if err := emitJSON(cmd, body, format); err != nil {
			return err
		}
	} else {
		records := append([]PassageRecord(nil), resp.Passages...)
		sortByChunkIndex(records)
		if err := renderPassageTable(cmd.OutOrStdout(), records); err != nil {
			return err
		}
		renderPassageTexts(cmd.OutOrStdout(), records)
	}
	truncationHint(cmd, resp.page)
	return nil
}

// ─── toc ─────────────────────────────────────────────────────────────────────

type tocFlags struct {
	cursor, format string
	dryRun         bool
}

func newTocCmd(version string) *cobra.Command {
	var f tocFlags
	c := &cobra.Command{
		Use:   "toc <filing_id>",
		Short: "List the section headers of a filing",
		Long: `List a filing's section headers in order. Pass one to 'read section' to
read that section.`,
		Args:        cobra.ExactArgs(1),
		Annotations: researchAnnotations("Filing table of contents", "0,2,3,4,5,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runToc(cmd, args[0], version, &f)
		},
	}
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	return c
}

func runToc(cmd *cobra.Command, filingID, version string, f *tocFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	filingID = strings.TrimSpace(filingID)
	if err := checkUUID(cmd, "filing_id", filingID); err != nil {
		return err
	}
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/filings/"+url.PathEscape(filingID)+"/toc", url.Values{}, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp tocResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}
	if format == formatCompact {
		if err := emitCompact(cmd, body, "sections", cur); err != nil {
			return err
		}
		if len(resp.Sections) == 0 {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No sections found")
		}
		return nil
	}
	if format == "json" {
		if err := emitJSON(cmd, body, format); err != nil {
			return err
		}
	} else {
		for i, s := range resp.Sections {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%3d  %s\n", i+1, s)
		}
	}
	if len(resp.Sections) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No sections found")
	}
	truncationHint(cmd, resp.page)
	return nil
}

// ─── filings (Story 81.4) ────────────────────────────────────────────────────

// filingRow is one /research/filings row (Story 81.2).
type filingRow struct {
	FilingID  *string `json:"filing_id"`
	Form      *string `json:"form"`
	Datefiled *string `json:"datefiled"`
	Period    *string `json:"period"`
	URL       *string `json:"url"`
}

type filingsResponse struct {
	page
	Company *struct {
		Name     *string `json:"name"`
		Symbol   *string `json:"symbol"`
		Exchange *string `json:"exchange"`
	} `json:"company"`
	Filings []filingRow `json:"filings"`
}

type filingsFlags struct {
	formtype, dateFrom, dateTo, cursor, format string
	limit                                      int
	dryRun                                     bool
}

func newFilingsCmd(version string) *cobra.Command {
	var f filingsFlags
	c := &cobra.Command{
		Use:   "filings <symbol>",
		Short: "List a company's filings, newest first",
		Long: `List one company's readable filings newest first, by symbol (AAPL for a
US listing, ABX:CA for a Canadian one, AKBNK:TR for a Turkish one). Each row
carries the filing_id that toc, read section and find take, the form, the
filing date, the period and the filing url to cite. Filter by form type and
filing dates. An empty list means no filing matched, not an error. Exit 3
when no company has the symbol.`,
		Args:        cobra.ExactArgs(1),
		Annotations: researchAnnotations("List filings", "0,2,3,4,5,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFilings(cmd, args[0], version, &f)
		},
	}
	c.Flags().StringVar(&f.formtype, "formtype", "", "Limit to one form, as a row's form shows it (at most 200 characters)")
	c.Flags().StringVar(&f.dateFrom, "date-from", "", "Earliest filing date, YYYY-MM-DD")
	c.Flags().StringVar(&f.dateTo, "date-to", "", "Latest filing date, YYYY-MM-DD")
	c.Flags().IntVar(&f.limit, "limit", defaultSearchLimit, "Filings to return (1 to 25)")
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	return c
}

func runFilings(cmd *cobra.Command, symbol, version string, f *filingsFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	symbol = strings.TrimSpace(symbol)
	if symbol == "" || jsLen(symbol) > maxSymbolChars {
		return usageErr(cmd, "symbol must be 1 to %d characters", maxSymbolChars)
	}
	formtype := strings.TrimSpace(f.formtype)
	if jsLen(formtype) > maxListFormtypeChars {
		return usageErr(cmd, "--formtype must be at most %d characters", maxListFormtypeChars)
	}
	if f.limit < minSearchLimit || f.limit > maxSearchLimit {
		return usageErr(cmd, "--limit must be from %d to %d (got %d)", minSearchLimit, maxSearchLimit, f.limit)
	}
	if err := checkDate(cmd, "date-from", f.dateFrom); err != nil {
		return err
	}
	if err := checkDate(cmd, "date-to", f.dateTo); err != nil {
		return err
	}
	if f.dateFrom != "" && f.dateTo != "" && f.dateFrom > f.dateTo {
		return usageErr(cmd, "--date-from must not be later than --date-to")
	}

	q := url.Values{}
	q.Set("symbol", symbol)
	if formtype != "" {
		q.Set("formtype", formtype)
	}
	if f.dateFrom != "" {
		q.Set("date_from", f.dateFrom)
	}
	if f.dateTo != "" {
		q.Set("date_to", f.dateTo)
	}
	q.Set("limit", fmt.Sprintf("%d", f.limit))

	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/filings", q, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp filingsResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}
	switch format {
	case formatCompact:
		err = emitCompact(cmd, body, "filings", cur)
	case "json":
		if err = emitJSON(cmd, body, format); err == nil {
			cursorHint(cmd, resp.NextCursor)
		}
	default:
		err = renderFilingsTable(cmd.OutOrStdout(), resp.Filings)
		cursorHint(cmd, resp.NextCursor)
	}
	if err != nil {
		return err
	}
	// The hosted contract: a known company with no matching filing is an
	// empty list, not an error.
	if len(resp.Filings) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No filings found")
	}
	return nil
}

// renderFilingsTable writes FILING_ID FORM DATE PERIOD URL.
func renderFilingsTable(w io.Writer, rows []filingRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FILING_ID\tFORM\tDATE\tPERIOD\tURL")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			cell(r.FilingID), clipCell(r.Form, sectionColumnRunes), cell(r.Datefiled), cell(r.Period), cell(r.URL))
	}
	return tw.Flush()
}

// ─── find (Story 81.4) ───────────────────────────────────────────────────────

type findMatch struct {
	ChunkID       *string `json:"chunk_id"`
	SectionHeader *string `json:"section_header"`
	Snippet       *string `json:"snippet"`
	URL           *string `json:"url"`
}

type findResponse struct {
	Term         string      `json:"term"`
	Found        bool        `json:"found"`
	TotalMatches int         `json:"total_matches"`
	Matches      []findMatch `json:"matches"`
	NextCursor   *string     `json:"next_cursor"`
}

type findFlags struct {
	cursor, format string
	limit          int
	dryRun         bool
}

func newFindCmd(version string) *cobra.Command {
	var f findFlags
	c := &cobra.Command{
		Use:   "find <filing_id> <term>",
		Short: "Check whether one filing mentions a term",
		Long: `Check whether one filing mentions a term, by filing_id (from search,
filings or toc). The result says found, total_matches and the best matching
passages with their section, a short snippet and the url to cite. Every word
of the term must appear in a passage; put the term in double quotes for an
exact phrase. found false is an answer, not an error: use it instead of
repeated searches to check that a term is absent. Exit 3 when the filing id
is unknown.`,
		Args:        cobra.ExactArgs(2),
		Annotations: researchAnnotations("Find in filing", "0,2,3,4,5,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFind(cmd, args[0], args[1], version, &f)
		},
	}
	c.Flags().IntVar(&f.limit, "limit", defaultFindLimit, "Matches to return (1 to 10)")
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	return c
}

func runFind(cmd *cobra.Command, filingID, term, version string, f *findFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	filingID = strings.TrimSpace(filingID)
	if err := checkUUID(cmd, "filing_id", filingID); err != nil {
		return err
	}
	// The server's own rule: trimmed, 1 to 200 characters, at least one
	// letter or digit. Quotes pass through unchanged (an exact phrase).
	term = strings.TrimSpace(term)
	if term == "" || jsLen(term) > maxFindTermChars || !hasLetterOrDigit(term) {
		return usageErr(cmd, "term must be 1 to %d characters with at least one letter or digit", maxFindTermChars)
	}
	if f.limit < minFindLimit || f.limit > maxFindLimit {
		return usageErr(cmd, "--limit must be from %d to %d (got %d)", minFindLimit, maxFindLimit, f.limit)
	}
	q := url.Values{}
	q.Set("term", term)
	q.Set("limit", fmt.Sprintf("%d", f.limit))

	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/filings/"+url.PathEscape(filingID)+"/find", q, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp findResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}
	switch format {
	case formatCompact:
		return emitCompact(cmd, body, "matches", cur)
	case "json":
		if err := emitJSON(cmd, body, format); err != nil {
			return err
		}
	default:
		if err := renderFindTable(cmd.OutOrStdout(), resp); err != nil {
			return err
		}
	}
	cursorHint(cmd, resp.NextCursor)
	return nil
}

// renderFindTable writes a found line, then CHUNK_ID SECTION SNIPPET URL.
func renderFindTable(w io.Writer, resp findResponse) error {
	if !resp.Found {
		_, err := fmt.Fprintf(w, "Not found: no passage mentions %s\n", resp.Term)
		return err
	}
	if _, err := fmt.Fprintf(w, "Found: %d matching passages for %s\n", resp.TotalMatches, resp.Term); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHUNK_ID\tSECTION\tSNIPPET\tURL")
	for _, m := range resp.Matches {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			cell(m.ChunkID), clipCell(m.SectionHeader, sectionColumnRunes), clipCell(m.Snippet, snippetColumnRunes), cell(m.URL))
	}
	return tw.Flush()
}
