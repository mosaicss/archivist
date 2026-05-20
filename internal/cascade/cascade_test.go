package cascade_test

import (
	"testing"

	"github.com/mosaicss/archivist/internal/cascade"
)

// TestCascadeValidation_CustomFilings verifies that a custom row + filings column
// returns a CascadeViolation with the correct rule key and exits 8.
func TestCascadeValidation_CustomFilings(t *testing.T) {
	rows := []cascade.Row{
		{Custom: "Goldman Sachs private wealth"},
	}
	cols := []cascade.Column{
		{Name: "Net interest margin", Source: "filings"},
	}
	violations := cascade.ValidateSpec(rows, cols)
	if len(violations) == 0 {
		t.Fatal("expected at least one cascade violation, got none")
	}
	v := violations[0]
	if v.Rule != "custom_x_filings" {
		t.Errorf("rule: want custom_x_filings, got %q", v.Rule)
	}
	if v.Row != "Goldman Sachs private wealth" {
		t.Errorf("row: want 'Goldman Sachs private wealth', got %q", v.Row)
	}
	if v.Col != "Net interest margin" {
		t.Errorf("col: want 'Net interest margin', got %q", v.Col)
	}
	if v.Message == "" {
		t.Error("message should not be empty")
	}
}

// TestCascadeValidation_CustomWebOK verifies that custom row + web column returns
// no violations.
func TestCascadeValidation_CustomWebOK(t *testing.T) {
	rows := []cascade.Row{
		{Custom: "Goldman Sachs private wealth"},
	}
	cols := []cascade.Column{
		{Name: "Wealth strategy", Source: "web"},
	}
	violations := cascade.ValidateSpec(rows, cols)
	if len(violations) != 0 {
		t.Errorf("expected no violations for custom+web, got %d: %+v", len(violations), violations)
	}
}

// TestCascadeValidation_DefaultSourceIsFilings verifies that an empty Source
// defaults to "filings" and triggers the custom_x_filings rule.
func TestCascadeValidation_DefaultSourceIsFilings(t *testing.T) {
	rows := []cascade.Row{
		{Custom: "some custom entity"},
	}
	cols := []cascade.Column{
		{Name: "Some column", Source: ""},
	}
	violations := cascade.ValidateSpec(rows, cols)
	if len(violations) == 0 {
		t.Fatal("expected violation for custom row + empty source (defaults to filings)")
	}
	if violations[0].Rule != "custom_x_filings" {
		t.Errorf("rule: want custom_x_filings, got %q", violations[0].Rule)
	}
}

// TestCascadeValidation_CompanyRowNoViolation verifies that a company row +
// filings column is fine.
func TestCascadeValidation_CompanyRowNoViolation(t *testing.T) {
	rows := []cascade.Row{
		{IssuerKey: "aapl_us"},
	}
	cols := []cascade.Column{
		{Name: "Revenue", Source: "filings"},
	}
	violations := cascade.ValidateSpec(rows, cols)
	if len(violations) != 0 {
		t.Errorf("expected no violations for company+filings, got %d", len(violations))
	}
}
