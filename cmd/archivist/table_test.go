package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/mosaicss/archivist/internal/output"
	"github.com/mosaicss/archivist/internal/resolver"
	"github.com/mosaicss/archivist/internal/tablespec"
	"github.com/spf13/cobra"
)

// TestTableFluenFlagParsing verifies that --row and --col strings produce the
// expected TableSpec fields.
func TestTableFluenFlagParsing(t *testing.T) {
	rowFlags := []string{
		"company=aapl_us,filing-type=10-K,date-from=2024-01-01",
		"company=msft_us,filing-type=10-K",
		"custom=Goldman Sachs private wealth",
	}
	colFlags := []string{
		"name=Net interest margin,source=filings,mode=rrf,query=net interest margin Q4 2024",
		"name=Wealth strategy,source=web,web-query=wealth management strategy 2024",
	}

	spec, err := buildSpecFromFlags(rowFlags, colFlags, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.TopN != 5 {
		t.Errorf("TopN: want 5, got %d", spec.TopN)
	}
	if len(spec.Rows) != 3 {
		t.Fatalf("Rows: want 3, got %d", len(spec.Rows))
	}
	if spec.Rows[0].Company != "aapl_us" {
		t.Errorf("Rows[0].Company: want aapl_us, got %q", spec.Rows[0].Company)
	}
	if len(spec.Rows[0].FilingTypes) != 1 || spec.Rows[0].FilingTypes[0] != "10-K" {
		t.Errorf("Rows[0].FilingTypes: want [10-K], got %v", spec.Rows[0].FilingTypes)
	}
	if spec.Rows[0].DateFrom != "2024-01-01" {
		t.Errorf("Rows[0].DateFrom: want 2024-01-01, got %q", spec.Rows[0].DateFrom)
	}
	if spec.Rows[2].Custom != "Goldman Sachs private wealth" {
		t.Errorf("Rows[2].Custom: want 'Goldman Sachs private wealth', got %q", spec.Rows[2].Custom)
	}
	if len(spec.Columns) != 2 {
		t.Fatalf("Columns: want 2, got %d", len(spec.Columns))
	}
	if spec.Columns[0].Name != "Net interest margin" {
		t.Errorf("Columns[0].Name: want 'Net interest margin', got %q", spec.Columns[0].Name)
	}
	if spec.Columns[0].Mode != "rrf" {
		t.Errorf("Columns[0].Mode: want rrf, got %q", spec.Columns[0].Mode)
	}
	if spec.Columns[1].WebQuery != "wealth management strategy 2024" {
		t.Errorf("Columns[1].WebQuery: want 'wealth management strategy 2024', got %q", spec.Columns[1].WebQuery)
	}
}

// TestTableFluenFlagParsing_UnknownKey verifies that an unknown --row key
// causes a parse error.
func TestTableFluenFlagParsing_UnknownKey(t *testing.T) {
	_, err := buildSpecFromFlags(
		[]string{"company=aapl_us,unknown_key=bad"},
		[]string{"name=Revenue,source=filings,query=annual revenue"},
		5,
	)
	if err == nil {
		t.Fatal("expected error for unknown row key, got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention unknown key, got: %v", err)
	}
}

// TestDryRunNoop verifies that --dry-run prints the wire payload and makes no
// HTTP call.
func TestDryRunNoop(t *testing.T) {
	rowFlags := []string{"company=aapl_us,filing-type=10-K"}
	colFlags := []string{"name=Revenue,source=filings,query=annual revenue"}

	spec, err := buildSpecFromFlags(rowFlags, colFlags, 5)
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate spec: %v", err)
	}

	cobraCmd := newTableCmd("test")
	var out bytes.Buffer
	cobraCmd.SetOut(&out)
	cobraCmd.SetErr(io.Discard)

	if err := printDryRun(cobraCmd, spec); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("dry-run output is not valid JSON: %v\noutput: %s", err, out.String())
	}
	if _, ok := payload["rows"]; !ok {
		t.Error("dry-run JSON missing 'rows' field")
	}
	if _, ok := payload["columns"]; !ok {
		t.Error("dry-run JSON missing 'columns' field")
	}
}

// TestOutputFormat_CSV verifies that CSV output has the correct header row.
func TestOutputFormat_CSV(t *testing.T) {
	result := &output.TableResult{
		Cells: []output.Cell{
			{RowID: "aapl_us", ColID: "Revenue", Value: "$100B"},
			{RowID: "msft_us", ColID: "Revenue", Value: "$200B"},
		},
	}

	cobraCmd := newTableCmd("test")
	var out bytes.Buffer
	cobraCmd.SetOut(&out)
	cobraCmd.SetErr(io.Discard)

	if err := renderTableResult(cobraCmd, result, output.FormatCSV, "", false); err != nil {
		t.Fatalf("renderTableResult CSV: %v", err)
	}

	csv := out.String()
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) < 1 {
		t.Fatal("CSV output is empty")
	}
	// Header should include "row" and "Revenue" and "Citations".
	header := lines[0]
	if !strings.Contains(header, "row") {
		t.Errorf("CSV header missing 'row': %q", header)
	}
	if !strings.Contains(header, "Revenue") {
		t.Errorf("CSV header missing 'Revenue': %q", header)
	}
	if !strings.Contains(header, "Citations") {
		t.Errorf("CSV header missing 'Citations': %q", header)
	}
}

// TestHelpListsTableVerb verifies that the table verb appears in the root help
// output when registered through the main package entrypoint.
func TestHelpListsTableVerb(t *testing.T) {
	root := buildRootForTest("dev")
	root.SetArgs([]string{"--help"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	_ = root.Execute()
	if !strings.Contains(out.String(), "table") {
		t.Errorf("help output missing 'table' verb:\n%s", out.String())
	}
}

// buildRootForTest mirrors main() setup for use in tests.
func buildRootForTest(version string) *cobra.Command {
	root := cmd.NewRootCmd(version, "test", "test")
	root.AddCommand(newTableCmd(version))
	return root
}

// ─── auto-resolution tests ────────────────────────────────────────────────────

// fakeHTTPClient is a resolver.Client that returns canned HTTP responses.
type fakeHTTPClient struct {
	response []byte
	status   int
}

func (f *fakeHTTPClient) Do(_ context.Context, _, _ string, _ io.Reader) (*http.Response, error) {
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(bytes.NewReader(f.response)),
	}, nil
}

// TestAutoResolution_ExactMatch verifies that a single strong match substitutes
// the issuer_key silently.
func TestAutoResolution_ExactMatch(t *testing.T) {
	aaplUs := "aapl_us"
	results := []resolver.CompanyResult{
		{IssuerKey: &aaplUs, CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127},
	}
	body, _ := json.Marshal(results)
	fc := &fakeHTTPClient{response: body, status: http.StatusOK}

	res, err := resolver.AutoResolve(context.Background(), fc, "Apple", `archivist table --row "company=`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IssuerKey != "aapl_us" {
		t.Errorf("IssuerKey: want aapl_us, got %q", res.IssuerKey)
	}
}

// TestAutoResolution_Ambiguous verifies that multiple similarly-weighted results
// return *AmbiguousError.
func TestAutoResolution_Ambiguous(t *testing.T) {
	appleCa := "apple_ca"
	aaplUs := "aapl_us"
	results := []resolver.CompanyResult{
		{IssuerKey: &appleCa, CompanyName: "Apple Canada Ltd.", Symbol: "APL:TSX", Exchange: "TSX", FilingCount: 5},
		{IssuerKey: &aaplUs, CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 8},
	}
	body, _ := json.Marshal(results)
	fc := &fakeHTTPClient{response: body, status: http.StatusOK}

	_, err := resolver.AutoResolve(context.Background(), fc, "Apple", `archivist table --row "company=`)
	if err == nil {
		t.Fatal("expected ambiguous error, got nil")
	}
	var ambig *resolver.AmbiguousError
	if !errors.As(err, &ambig) {
		t.Fatalf("expected *resolver.AmbiguousError, got %T: %v", err, err)
	}
	if ambig.Envelope.Query != "Apple" {
		t.Errorf("Query: want Apple, got %q", ambig.Envelope.Query)
	}
	if len(ambig.Envelope.Candidates) != 2 {
		t.Errorf("Candidates: want 2, got %d", len(ambig.Envelope.Candidates))
	}
}

// TestAutoResolution_IssuerKeyBypass — removed at Wave-2 merge time. Bypass is
// performed by callers (chat.go and table.go's resolveCompanyRows) before
// calling resolver.AutoResolve, not by the resolver itself. Coverage for the
// caller-side bypass lives in chat_test.go.

// ─── Story 37.5 — literal issuer_key bypass ───────────────────────────────────

// failOnCallClient fails the test if any HTTP method is invoked. Used to prove
// that resolveCompanies bypasses the resolver for literal issuer_keys.
type failOnCallClient struct {
	t *testing.T
}

func (f *failOnCallClient) Do(_ context.Context, method, path string, _ io.Reader) (*http.Response, error) {
	f.t.Errorf("HTTP call must not happen for literal issuer_key — got %s %s", method, path)
	return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// TestBuildWirePayload_PreservesCIKLiteral covers AC6 — the wire layer must
// pass `cik:320193` through to chat-api unchanged. Regression test for the
// 36.14 T10 smoke where the literal silently became `cik:831001` upstream of
// this function.
func TestBuildWirePayload_PreservesCIKLiteral(t *testing.T) {
	spec := mustSpec37_5(t, "cik:320193")
	payload := buildWirePayload(spec)
	if len(payload.Rows) != 1 {
		t.Fatalf("Rows: want 1, got %d", len(payload.Rows))
	}
	if payload.Rows[0].Filters.IssuerKey != "cik:320193" {
		t.Errorf("Filters.IssuerKey: want %q, got %q", "cik:320193", payload.Rows[0].Filters.IssuerKey)
	}
}

// TestBuildWirePayload_PreservesUUIDLiteral mirrors AC6 for the uuid: form.
func TestBuildWirePayload_PreservesUUIDLiteral(t *testing.T) {
	const uuidLit = "uuid:3162c889-bb75-49cf-b605-3295ae6e092d"
	spec := mustSpec37_5(t, uuidLit)
	payload := buildWirePayload(spec)
	if payload.Rows[0].Filters.IssuerKey != uuidLit {
		t.Errorf("Filters.IssuerKey: want %q, got %q", uuidLit, payload.Rows[0].Filters.IssuerKey)
	}
}

// TestResolveCompanies_LiteralBypass covers AC3 at the call-site level —
// resolveCompanies MUST NOT invoke the HTTP client when a row's Company field
// is one of the three literal issuer_key forms.
func TestResolveCompanies_LiteralBypass(t *testing.T) {
	literals := []string{
		"cik:320193",
		"uuid:3162c889-bb75-49cf-b605-3295ae6e092d",
		"aapl_us",
	}
	for _, lit := range literals {
		t.Run(lit, func(t *testing.T) {
			spec := mustSpec37_5(t, lit)
			fc := &failOnCallClient{t: t}

			cobraCmd := newTableCmd("test")
			cobraCmd.SetOut(io.Discard)
			cobraCmd.SetErr(io.Discard)

			if err := resolveCompanies(cobraCmd, fc, spec); err != nil {
				t.Fatalf("resolveCompanies: unexpected error: %v", err)
			}
			// Literal must pass through unchanged.
			if spec.Rows[0].Company != lit {
				t.Errorf("row Company mutated: want %q, got %q", lit, spec.Rows[0].Company)
			}
		})
	}
}

// TestResolveCompanies_FreeTextHitsResolver covers AC4 — free-text inputs
// MUST still flow through AutoResolve. Uses the fakeHTTPClient to return a
// canned unambiguous result and asserts the row's Company is rewritten.
func TestResolveCompanies_FreeTextHitsResolver(t *testing.T) {
	aapl := "aapl_us"
	results := []resolver.CompanyResult{
		{IssuerKey: &aapl, CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127},
	}
	body, _ := json.Marshal(results)
	fc := &fakeHTTPClient{response: body, status: http.StatusOK}

	spec := mustSpec37_5(t, "Apple")
	cobraCmd := newTableCmd("test")
	cobraCmd.SetOut(io.Discard)
	cobraCmd.SetErr(io.Discard)

	if err := resolveCompanies(cobraCmd, fc, spec); err != nil {
		t.Fatalf("resolveCompanies: unexpected error: %v", err)
	}
	if spec.Rows[0].Company != "aapl_us" {
		t.Errorf("free-text not resolved: want %q, got %q", "aapl_us", spec.Rows[0].Company)
	}
}

// mustSpec37_5 builds a minimal valid TableSpec with one row + one filings
// column for the 37.5 regression tests. The Company field is the only
// parameter that varies across these tests.
func mustSpec37_5(t *testing.T, company string) *tablespec.TableSpec {
	t.Helper()
	spec, err := buildSpecFromFlags(
		[]string{"company=" + company + ",filing-type=10-K,date-from=2025-08-01"},
		[]string{"name=FY2025 revenue,source=filings,mode=rrf,query=total net sales revenue annual"},
		3,
	)
	if err != nil {
		t.Fatalf("buildSpecFromFlags: %v", err)
	}
	return spec
}

// ─── Session 2026-05-21 — citation remap (cell-local → global) ─────────────

// TestToTableResult_CitationRemap covers the wire-response translation: each
// phrase's source_number is a 1-based CELL-LOCAL index into cell.results, and
// must be remapped to the GLOBAL source_number stored on each result before
// being stamped on output.Cell.Citations. Without the remap, a Newmont cell's
// phrase indices 1-5 would render as global citations 1-5 (which point to
// Barrick + first Newmont sources) — citation pollution that misled CLI JSON
// consumers even though the web UI did the remap correctly.
//
// Scenario mirrors the 2026-05-21 multi-cell smoke (Barrick + Newmont + FCX
// row, single column). Three cells, each with 5 phrases over its 2-filing
// retrieval. Globally numbered sources: 1-2 Barrick, 3-4 Newmont, 5-6 FCX.
func TestToTableResult_CitationRemap(t *testing.T) {
	mkSummary := func(s string) *string { return &s }

	w := wireTableResponse{
		Sources: []wireSource{
			{SourceNumber: 1, CompanyName: "Barrick Mining Corporation", FormType: "40-F"},
			{SourceNumber: 2, CompanyName: "Barrick Mining Corporation", FormType: "6-K"},
			{SourceNumber: 3, CompanyName: "Newmont Corporation", FormType: "10-K"},
			{SourceNumber: 4, CompanyName: "Newmont Corporation", FormType: "10-Q"},
			{SourceNumber: 5, CompanyName: "Freeport-McMoRan Inc.", FormType: "10-Q"},
			{SourceNumber: 6, CompanyName: "Freeport-McMoRan Inc.", FormType: "10-K"},
		},
		Cells: []wireCell{
			{
				RowIndex: 0, ColumnIndex: 0, State: "loaded",
				Summary: mkSummary("Barrick reported $16.96B in 2025."),
				// Barrick row: 5 chunks across the 2 Barrick filings (global 1, 2).
				Results: []wireResult{{1}, {2}, {1}, {2}, {2}},
				Phrases: []wirePhrase{
					{SourceNumber: 1, Phrase: "BARRICK MINING CORP"},
					{SourceNumber: 2, Phrase: "FY"},
					{SourceNumber: 3, Phrase: "Total $ 16,956 $ 12,922"},
					{SourceNumber: 4, Phrase: "Gold sales"},
					{SourceNumber: 5, Phrase: "Revenue"},
				},
			},
			{
				RowIndex: 1, ColumnIndex: 0, State: "loaded",
				Summary: mkSummary("Newmont reported $22.67B in 2025."),
				// Newmont row: chunks 1-4 from 10-K (global 3), chunk 5 from 10-Q (global 4).
				Results: []wireResult{{3}, {3}, {3}, {3}, {4}},
				Phrases: []wirePhrase{
					{SourceNumber: 1, Phrase: "Newmont Corporation"},
					{SourceNumber: 2, Phrase: "Net revenue"},
					{SourceNumber: 3, Phrase: "39.1"},
					{SourceNumber: 4, Phrase: "stockholders"},
					{SourceNumber: 5, Phrase: "STATEMENTS OF OPERATIONS"},
				},
			},
			{
				RowIndex: 2, ColumnIndex: 0, State: "loaded",
				Summary: mkSummary("Freeport reported $25.92B in 2025."),
				// FCX row: mix of 10-Q (global 5) and 10-K (global 6).
				Results: []wireResult{{5}, {6}, {6}, {5}, {5}},
				Phrases: []wirePhrase{
					{SourceNumber: 1, Phrase: "Consolidated revenues totaled $6.2 billion"},
					{SourceNumber: 2, Phrase: "Atlantic Copper revenues totaled $3.2 billion in 2025"},
					{SourceNumber: 3, Phrase: "Copper Mines Product Revenues"},
					{SourceNumber: 4, Phrase: "Following is a summary"},
					{SourceNumber: 5, Phrase: "CONSOLIDATED STATEMENTS OF INCOME"},
				},
			},
		},
	}

	res := w.toTableResult()

	if len(res.Cells) != 3 {
		t.Fatalf("Cells: want 3, got %d", len(res.Cells))
	}

	// Barrick row must cite ONLY {1, 2}; never {3, 4, 5} (those are Newmont/FCX).
	wantBarrick := []string{"1", "2"}
	if !equalStringSet(res.Cells[0].Citations, wantBarrick) {
		t.Errorf("Barrick row citations: want %v, got %v", wantBarrick, res.Cells[0].Citations)
	}

	// Newmont row must cite ONLY {3, 4}; never {1, 2} (those are Barrick).
	wantNewmont := []string{"3", "4"}
	if !equalStringSet(res.Cells[1].Citations, wantNewmont) {
		t.Errorf("Newmont row citations: want %v, got %v", wantNewmont, res.Cells[1].Citations)
	}

	// FCX row must cite ONLY {5, 6}; never {1, 2, 3, 4} (other companies).
	wantFCX := []string{"5", "6"}
	if !equalStringSet(res.Cells[2].Citations, wantFCX) {
		t.Errorf("FCX row citations: want %v, got %v", wantFCX, res.Cells[2].Citations)
	}
}

func TestToTableResult_CitationRemap_OutOfRangePhrasesDropped(t *testing.T) {
	// Phrases whose source_number indexes past the cell's results array must
	// be silently dropped (defensive — the server should never emit these,
	// but if it does, the CLI shouldn't crash or render garbage).
	mkSummary := func(s string) *string { return &s }
	w := wireTableResponse{
		Sources: []wireSource{{SourceNumber: 1, CompanyName: "Acme", FormType: "10-K"}},
		Cells: []wireCell{
			{
				RowIndex: 0, ColumnIndex: 0, State: "loaded",
				Summary: mkSummary("Summary."),
				Results: []wireResult{{1}}, // only one result
				Phrases: []wirePhrase{
					{SourceNumber: 1, Phrase: "in range"},
					{SourceNumber: 0, Phrase: "zero — invalid"},
					{SourceNumber: 99, Phrase: "out of range"},
					{SourceNumber: -3, Phrase: "negative"},
				},
			},
		},
	}
	res := w.toTableResult()
	if !equalStringSet(res.Cells[0].Citations, []string{"1"}) {
		t.Errorf("expected only valid in-range phrase to surface; got %v", res.Cells[0].Citations)
	}
}

func equalStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}
