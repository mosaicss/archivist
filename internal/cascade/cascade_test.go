package cascade

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func specWith(rows []SpecRow, cols []SpecColumn) *Spec {
	return &Spec{Rows: rows, Columns: cols}
}

func filingsCol(name string) SpecColumn { return SpecColumn{Name: name, Source: "filings"} }
func webCol(name string) SpecColumn     { return SpecColumn{Name: name, Source: "web"} }
func defaultCol(name string) SpecColumn { return SpecColumn{Name: name} }

func rowWithFilters(m map[string]interface{}) SpecRow { return SpecRow{Filters: m} }

// ── AC2: custom-entity row x filings column ───────────────────────────────

func TestCustomXFilings_Rejects(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"custom_entity": "Goldman Sachs"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected CascadeError, got nil")
	}
	ce, ok := err.(*CascadeError)
	if !ok {
		t.Fatalf("expected *CascadeError, got %T", err)
	}
	if ce.Rule != "custom_x_filings" {
		t.Errorf("expected rule custom_x_filings, got %s", ce.Rule)
	}
}

func TestCustomXFilings_Rejects_DefaultSourceColumn(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"custom_entity": "Acme Corp"})},
		[]SpecColumn{defaultCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected CascadeError for default (filings) column")
	}
	ce, _ := err.(*CascadeError)
	if ce.Rule != "custom_x_filings" {
		t.Errorf("got rule %s, want custom_x_filings", ce.Rule)
	}
}

func TestCustomXFilings_Allows_WebColumn(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"custom_entity": "Goldman Sachs"})},
		[]SpecColumn{webCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// ── AC3: country projection ───────────────────────────────────────────────

func TestCountryProjection_CA(t *testing.T) {
	out := ProjectCountry(map[string]interface{}{"country": "CA"})
	exch, ok := out["exchange"]
	if !ok {
		t.Fatal("expected exchange key after projection")
	}
	want := []string{"TSX", "TSV", "CSE", "NEO"}
	got, _ := exch.([]string)
	if !slicesEqual(got, want) {
		t.Errorf("CA exchange got %v, want %v", got, want)
	}
	if _, hasCountry := out["country"]; hasCountry {
		t.Error("country key should be dropped after projection")
	}
}

func TestCountryProjection_US(t *testing.T) {
	out := ProjectCountry(map[string]interface{}{"country": "US"})
	exch, ok := out["exchange"]
	if !ok {
		t.Fatal("expected exchange key after projection")
	}
	want := []string{"NGS", "NSD", "NSC", "NYE", "AMX"}
	got, _ := exch.([]string)
	if !slicesEqual(got, want) {
		t.Errorf("US exchange got %v, want %v", got, want)
	}
}

func TestCountryProjection_ExplicitExchangeWins(t *testing.T) {
	out := ProjectCountry(map[string]interface{}{"country": "CA", "exchange": []string{"NGS"}})
	exch, _ := out["exchange"].([]string)
	if len(exch) != 1 || exch[0] != "NGS" {
		t.Errorf("explicit exchange should win; got %v", exch)
	}
}

// ── AC4: company lock (Phase 1 soft warn) ────────────────────────────────

func TestCompanyLock_SoftWarn(t *testing.T) {
	var buf strings.Builder
	// Include formtype so issuer_date_span rule does not fire — this test
	// is specifically about the company_lock soft-warning behavior.
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{
			"issuer_key": "aapl_us",
			"country":    "CA",
			"formtype":   "10-K",
		})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpecWithWriter(spec, &buf)
	if err != nil {
		t.Errorf("Phase 1 company_lock must return nil; got %v", err)
	}
	if !strings.Contains(buf.String(), "country will be ignored") {
		t.Errorf("expected soft-warn message; got: %s", buf.String())
	}
}

// ── AC5: filing type validation ───────────────────────────────────────────

func TestFilingType_Rejects_Unknown(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"formtype": "10K"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected CascadeError for unknown filing type 10K")
	}
	ce, _ := err.(*CascadeError)
	if ce.Rule != "filing_type" {
		t.Errorf("expected filing_type rule, got %s", ce.Rule)
	}
	if !strings.Contains(ce.Message, "10-K") {
		t.Errorf("expected Did you mean 10-K in message; got: %s", ce.Message)
	}
}

func TestFilingType_Rejects_FarMiss(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"formtype": "xyz123"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected CascadeError for xyz123")
	}
	ce, _ := err.(*CascadeError)
	if ce.Rule != "filing_type" {
		t.Errorf("expected filing_type rule, got %s", ce.Rule)
	}
	if strings.Contains(ce.Message, "Did you mean") {
		t.Errorf("no near-match should be suggested for xyz123; got: %s", ce.Message)
	}
}

func TestFilingType_Allows_ValidSEC(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"formtype": "10-K"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("expected nil for valid SEC type; got %v", err)
	}
}

func TestFilingType_Allows_ValidSEC_Array(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"formtype": []interface{}{"10-K", "8-K"}})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("expected nil for valid SEC array; got %v", err)
	}
}

func TestFilingType_Allows_ValidSEDAR(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"formtype": "MD&A"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("expected nil for valid SEDAR type; got %v", err)
	}
}

// ── AC5b: issuer date span ─────────────────────────────────────────────────

func TestIssuerDateSpan_Rejects_NoDates(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"issuer_key": "aapl_us"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected CascadeError for issuer without date or formtype")
	}
	ce, _ := err.(*CascadeError)
	if ce.Rule != "issuer_date_span" {
		t.Errorf("expected issuer_date_span rule, got %s", ce.Rule)
	}
}

func TestIssuerDateSpan_Rejects_TooWide(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{
			"issuer_key": "aapl_us",
			"date_from":  "2020-01-01",
			"date_to":    "2026-01-01",
		})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected CascadeError for >730 day span")
	}
	ce, _ := err.(*CascadeError)
	if ce.Rule != "issuer_date_span" {
		t.Errorf("expected issuer_date_span rule, got %s", ce.Rule)
	}
}

func TestIssuerDateSpan_Allows_NarrowRange(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{
			"issuer_key": "aapl_us",
			"date_from":  "2025-01-01",
			"date_to":    "2026-01-01",
		})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("expected nil for narrow date range; got %v", err)
	}
}

func TestIssuerDateSpan_Allows_FormtypeFilter(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{
			"issuer_key": "aapl_us",
			"formtype":   "10-K",
		})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("expected nil when formtype present; got %v", err)
	}
}

func TestIssuerDateSpan_NoIssuer_NotBlocked(t *testing.T) {
	spec := specWith(
		[]SpecRow{rowWithFilters(map[string]interface{}{"country": "CA"})},
		[]SpecColumn{filingsCol("Revenue")},
	)
	if err := ValidateSpec(spec); err != nil {
		t.Errorf("country-only row should not be blocked; got %v", err)
	}
}

// ── Multi-row: fail-fast on first violation ────────────────────────────────

func TestValidateSpec_MultiRow_FirstViolation(t *testing.T) {
	spec := specWith(
		[]SpecRow{
			rowWithFilters(map[string]interface{}{"issuer_key": "aapl_us", "formtype": "10-K"}), // OK
			rowWithFilters(map[string]interface{}{"custom_entity": "Acme"}),                     // violation
			rowWithFilters(map[string]interface{}{"issuer_key": "msft_us", "formtype": "8-K"}),  // OK
		},
		[]SpecColumn{filingsCol("Revenue")},
	)
	err := ValidateSpec(spec)
	if err == nil {
		t.Fatal("expected error from row 2")
	}
	ce, _ := err.(*CascadeError)
	if ce.Rule != "custom_x_filings" {
		t.Errorf("expected first-failing rule custom_x_filings, got %s", ce.Rule)
	}
}

// ── ExplainRules ──────────────────────────────────────────────────────────

func TestExplainRules_NonEmpty(t *testing.T) {
	s := ExplainRules()
	if s == "" {
		t.Fatal("ExplainRules returned empty string")
	}
	for _, ruleName := range []string{"custom_x_filings", "country_projection", "company_lock", "filing_type", "issuer_date_span"} {
		if !strings.Contains(s, ruleName) {
			t.Errorf("ExplainRules missing rule %s", ruleName)
		}
	}
}

// ── JSON parseable ────────────────────────────────────────────────────────

func TestRulesJSON_Parseable(t *testing.T) {
	var out CascadeRulesFile
	if err := json.Unmarshal(cascadeRulesJSON, &out); err != nil {
		t.Fatalf("cascade-rules.json parse error: %v", err)
	}
	if out.Version == "" {
		t.Error("version field should be non-empty")
	}
	if len(out.SecFilingTypes) == 0 {
		t.Error("sec_filing_types should be non-empty")
	}
	if len(out.SedarFilingTypes) == 0 {
		t.Error("sedar_filing_types should be non-empty")
	}
}

func TestSchemaJSON_Parseable(t *testing.T) {
	var out map[string]interface{}
	if err := json.Unmarshal(tableSearchSchemaJSON, &out); err != nil {
		t.Fatalf("table-search-schema.json parse error: %v", err)
	}
	if out["$schema"] == nil {
		t.Error("$schema field should be present")
	}
}

// ── helper ────────────────────────────────────────────────────────────────

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
