package tablespec_test

import (
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/tablespec"
)

// TestTableSpecFileParsing_Valid parses a valid YAML spec and verifies fields.
func TestTableSpecFileParsing_Valid(t *testing.T) {
	yaml := `
top_n: 5
rows:
  - company: aapl_us
    filing-type:
      - 10-K
      - 10-Q
    date-from: 2024-01-01
    date-to: 2024-12-31
  - company: msft_us
    filing-type: 10-K
  - custom: "Goldman Sachs private wealth"
columns:
  - name: Net interest margin
    source: filings
    mode: rrf
    query: net interest margin Q4 2024
  - name: Wealth strategy
    source: web
    web-query: wealth management strategy 2024
`
	spec, err := tablespec.ParseYAML(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if spec.TopN != 5 {
		t.Errorf("top_n: want 5, got %d", spec.TopN)
	}
	if len(spec.Rows) != 3 {
		t.Fatalf("rows: want 3, got %d", len(spec.Rows))
	}
	if spec.Rows[0].Company != "aapl_us" {
		t.Errorf("rows[0].Company: want aapl_us, got %q", spec.Rows[0].Company)
	}
	if len(spec.Rows[0].FilingTypes) != 2 {
		t.Errorf("rows[0].FilingTypes: want 2 elements, got %d", len(spec.Rows[0].FilingTypes))
	}
	if spec.Rows[1].FilingTypes[0] != "10-K" {
		t.Errorf("rows[1].FilingTypes[0]: want 10-K, got %q", spec.Rows[1].FilingTypes[0])
	}
	if spec.Rows[2].Custom != "Goldman Sachs private wealth" {
		t.Errorf("rows[2].Custom: want 'Goldman Sachs private wealth', got %q", spec.Rows[2].Custom)
	}
	if len(spec.Columns) != 2 {
		t.Fatalf("columns: want 2, got %d", len(spec.Columns))
	}
	if spec.Columns[1].Source != "web" {
		t.Errorf("columns[1].Source: want web, got %q", spec.Columns[1].Source)
	}
	if spec.Columns[1].WebQuery != "wealth management strategy 2024" {
		t.Errorf("columns[1].WebQuery: want 'wealth management strategy 2024', got %q", spec.Columns[1].WebQuery)
	}
}

// TestTableSpecFileParsing_UnknownKey verifies that unknown YAML keys cause exit 2.
func TestTableSpecFileParsing_UnknownKey(t *testing.T) {
	yaml := `
top_n: 5
rows:
  - company: aapl_us
    unknown_field: bad
columns:
  - name: Revenue
    source: filings
    query: annual revenue
`
	_, err := tablespec.ParseYAML(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected parse error for unknown field, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unknown") &&
		!strings.Contains(strings.ToLower(err.Error()), "field") {
		t.Errorf("error message should mention unknown field, got: %v", err)
	}
}

// TestTableSpecFileParsing_YAMLAnchor verifies that YAML anchors are rejected.
func TestTableSpecFileParsing_YAMLAnchor(t *testing.T) {
	// This uses a scalar anchor to test rejection.
	yaml := `
top_n: 5
rows:
  - company: &myanchor aapl_us
columns:
  - name: Revenue
    source: filings
    query: annual revenue
`
	_, err := tablespec.ParseYAML(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected parse error for YAML anchor, got nil")
	}
	if !strings.Contains(err.Error(), "anchor") {
		t.Errorf("error should mention anchors, got: %v", err)
	}
}

// TestTableSpecFileParsing_YAMLMergeKey verifies that YAML merge keys are rejected.
func TestTableSpecFileParsing_YAMLMergeKey(t *testing.T) {
	yaml := `
base: &base
  source: filings
  mode: rrf

top_n: 5
rows:
  - company: aapl_us
columns:
  - <<: *base
    name: Revenue
    query: annual revenue
`
	_, err := tablespec.ParseYAML(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected parse error for YAML merge key, got nil")
	}
	// Should catch anchor in the base definition or merge key tag.
	if !strings.Contains(err.Error(), "anchor") && !strings.Contains(err.Error(), "merge") {
		t.Errorf("error should mention anchor or merge key, got: %v", err)
	}
}

// TestTableSpecFileParsing_NodeBudget verifies that oversized specs are rejected.
func TestTableSpecFileParsing_NodeBudget(t *testing.T) {
	// Generate a spec with many rows to exceed the 10,000 node budget.
	var sb strings.Builder
	sb.WriteString("top_n: 5\nrows:\n")
	for i := 0; i < 2000; i++ {
		sb.WriteString("  - company: aapl_us\n")
		sb.WriteString("    filing-type: 10-K\n")
		sb.WriteString("    date-from: 2024-01-01\n")
		sb.WriteString("    date-to: 2024-12-31\n")
		sb.WriteString("    exchange: NGS\n")
	}
	sb.WriteString("columns:\n  - name: Revenue\n    source: filings\n    query: annual revenue\n")

	_, err := tablespec.ParseYAML(strings.NewReader(sb.String()))
	if err == nil {
		t.Fatal("expected parse error for oversized spec, got nil")
	}
	if !strings.Contains(err.Error(), "10,000") && !strings.Contains(strings.ToLower(err.Error()), "large") {
		t.Errorf("error should mention size limit, got: %v", err)
	}
}
