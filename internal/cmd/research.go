package cmd

// research.go implements the model-free research verbs over chat-api's
// /research routes (Story 73.2, reference/archivist/api-contracts.md §10):
//
//	archivist search <query>                         GET /research/search
//	archivist read passage <chunk_id>                GET /research/passages/:chunkId
//	archivist read section <filing_id> <section>     GET /research/filings/:id/sections
//	archivist toc <filing_id>                        GET /research/filings/:id/toc
//
// JSON output (the default off a TTY) is the server body re-indented, keys
// and values unchanged. Table output lists PassageRecords with their
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
	case "", "json", "table":
		return nil
	}
	return usageErr(cmd, "--format must be table or json (got %q)", format)
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

// fetchResearch runs one GET against chat-api and returns the 2xx body.
// --dry-run prints the request instead. A nil body with a nil error means the
// dry run already printed.
func fetchResearch(cmd *cobra.Command, version, path, format string, dryRun bool) ([]byte, error) {
	if dryRun {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] GET %s%s\n", client.ResolveBaseURL(), path)
		return nil, nil
	}
	c, err := newResearchClient(cmd, version, format)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(cmd.Context(), http.MethodGet, path, nil)
	if err != nil {
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
	dryRun, stdin                                            bool
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
search'), --formtype, --date-from and --date-to. Page
with --cursor when a response is truncated. --mode broad searches without
filters. Exit 3 when nothing matched, 6 when the symbol matches several
issuers (rerun with the full TICKER:EXCHANGE symbol).`,
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
	c.Flags().StringVar(&f.mode, "mode", "semantic", "Search mode: semantic (accepts filters) or broad (no filters)")
	c.Flags().IntVar(&f.limit, "limit", defaultSearchLimit, "Passages to return (1-25)")
	c.Flags().StringVar(&f.cursor, "cursor", "", "Continuation cursor from a truncated response")
	c.Flags().StringVar(&f.format, "format", "", "Output format: table (default on TTY) or json (default off a TTY)")
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
	q.Set("mode", f.mode)
	q.Set("limit", fmt.Sprintf("%d", f.limit))
	if f.cursor != "" {
		q.Set("cursor", f.cursor)
	}

	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	body, err := fetchResearch(cmd, version, "/research/search?"+q.Encode(), format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp searchResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}

	if len(resp.Results) == 0 {
		if format == "json" {
			if err := emitJSON(cmd, body, format); err != nil {
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

	if format == "json" {
		if err := emitJSON(cmd, body, format); err != nil {
			return err
		}
	} else if err := renderPassageTable(cmd.OutOrStdout(), resp.Results); err != nil {
		return err
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
	c.Flags().StringVar(&f.cursor, "cursor", "", "Continuation cursor from a truncated response")
	c.Flags().StringVar(&f.format, "format", "", "Output format: table (default on TTY) or json (default off a TTY)")
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
	if f.cursor != "" {
		q.Set("cursor", f.cursor)
	}
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path := "/research/passages/" + url.PathEscape(chunkID) + "?" + q.Encode()
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp passageResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
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
	c.Flags().StringVar(&f.cursor, "cursor", "", "Continuation cursor from a truncated response")
	c.Flags().StringVar(&f.format, "format", "", "Output format: table (default on TTY) or json (default off a TTY)")
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
	if f.cursor != "" {
		q.Set("cursor", f.cursor)
	}
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path := "/research/filings/" + url.PathEscape(filingID) + "/sections?" + q.Encode()
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp sectionResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
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
	c.Flags().StringVar(&f.cursor, "cursor", "", "Continuation cursor from a truncated response")
	c.Flags().StringVar(&f.format, "format", "", "Output format: table (default on TTY) or json (default off a TTY)")
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
	path := "/research/filings/" + url.PathEscape(filingID) + "/toc"
	if f.cursor != "" {
		q := url.Values{}
		q.Set("cursor", f.cursor)
		path += "?" + q.Encode()
	}
	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp tocResponse
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
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
