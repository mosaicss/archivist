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

