package cascade

// CascadeViolation describes a single rule violation found during pre-validation.
// The Violations slice supports future accumulation without breaking the call site.
type CascadeViolation struct {
	Row     string // custom entity value or issuer_key
	Col     string // column name
	Rule    string // short rule key: custom_x_filings, filing_type, company_lock, country_projection
	Message string // human-readable error message
}

// Spec is the minimal interface ValidateSpec needs — mirrors the TableSpec
// fields that cascade rules operate on.
type Spec interface {
	GetRows() []Row
	GetColumns() []Column
}

// Row is a lightweight row descriptor for cascade validation.
type Row struct {
	// Custom holds the free-text custom entity value when the row is a custom row.
	Custom string
	// IssuerKey holds the resolved issuer key for company rows.
	IssuerKey string
}

// Column is a lightweight column descriptor for cascade validation.
type Column struct {
	Name   string
	Source string // "filings" | "web"
}

// ValidateSpec runs the cascade rule pre-validation.
//
// Story 36.8 fills in the full rule set (custom×filings, filing_type catalog,
// company_lock, country_projection) by loading cascade-rules.json via embed.
// This stub implements only the custom_x_filings check so that 36.4 tests are
// meaningful and the call site is already correct for 36.8.
func ValidateSpec(rows []Row, cols []Column) []CascadeViolation {
	var violations []CascadeViolation
	for _, row := range rows {
		if row.Custom == "" {
			continue
		}
		for _, col := range cols {
			src := col.Source
			if src == "" {
				src = "filings"
			}
			if src == "filings" {
				violations = append(violations, CascadeViolation{
					Row:  row.Custom,
					Col:  col.Name,
					Rule: "custom_x_filings",
					Message: "Custom row '" + row.Custom + "' cannot be used with filings column '" +
						col.Name + "'. Re-pick a web column or swap the row for a company row.",
				})
				return violations // fail-fast
			}
		}
	}
	return violations
}
