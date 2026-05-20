package cascade

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// CascadeError is the typed error returned on rule violations. Exit code 8.
type CascadeError struct {
	Rule    string // short machine key: "custom_x_filings", "country_lock", "company_lock", "filing_type", "issuer_date_span"
	Message string // human-readable, safe for stderr
}

func (e *CascadeError) Error() string { return e.Message }

// Spec is a minimal interface for the table spec consumed by ValidateSpec.
// Story 36.4 defines the full tablespec.Spec; this package defines the
// interface it needs so the cascade package has no circular dep on tablespec.
type Spec struct {
	Rows    []SpecRow    `yaml:"rows"`
	Columns []SpecColumn `yaml:"columns"`
}

// SpecRow mirrors the YAML spec row shape. Filters is passthrough.
type SpecRow struct {
	Filters map[string]interface{} `yaml:"filters"`
}

// SpecColumn mirrors the YAML spec column shape.
type SpecColumn struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"` // "filings" | "web" | "" (defaults to filings)
}

// ValidateSpec checks all rows x columns in spec for cascade violations.
// Returns *CascadeError on first violation, nil on clean.
// Callers (archivist table run, archivist table --dry-run) check the error
// type to determine exit code 8 vs other codes.
func ValidateSpec(spec *Spec) error {
	for _, row := range spec.Rows {
		// Rule 1: custom-entity row x filings-source column (hard reject)
		if err := checkCustomXFilings(row, spec.Columns); err != nil {
			return err
		}
		// Rule 3: company locks country (soft warn in Phase 1, returns nil)
		checkCompanyLock(row, os.Stderr)
		// Rule 4: filing-type catalog validation
		if err := checkFilingType(row); err != nil {
			return err
		}
		// Rule 5: issuer-locked row date-span cap
		if err := checkIssuerDateSpan(row); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSpecWithWriter is like ValidateSpec but writes company_lock warnings
// to the supplied writer (for testability).
func ValidateSpecWithWriter(spec *Spec, warnW io.Writer) error {
	for _, row := range spec.Rows {
		if err := checkCustomXFilings(row, spec.Columns); err != nil {
			return err
		}
		checkCompanyLock(row, warnW)
		if err := checkFilingType(row); err != nil {
			return err
		}
		if err := checkIssuerDateSpan(row); err != nil {
			return err
		}
	}
	return nil
}

func checkCustomXFilings(row SpecRow, columns []SpecColumn) *CascadeError {
	customEntity := stringVal(row.Filters, "custom_entity")
	if customEntity == "" {
		return nil
	}
	rowName := customEntity
	if len(rowName) > 40 {
		rowName = rowName[:40]
	}
	for _, col := range columns {
		if col.Source == "" || col.Source == "filings" {
			return &CascadeError{
				Rule: "custom_x_filings",
				Message: fmt.Sprintf(
					`Custom-entity rows can only be used with web-source columns. Row "%s" has custom_entity="%s" but column "%s" is source=filings.`,
					rowName, customEntity, col.Name,
				),
			}
		}
	}
	return nil
}

func checkCompanyLock(row SpecRow, w io.Writer) {
	issuerKey := stringVal(row.Filters, "issuer_key")
	country := stringVal(row.Filters, "country")
	if issuerKey == "" || country == "" {
		return
	}
	// Phase 1: soft warning only — cannot validate the combination without
	// a local company catalog. Phase 2 upgrades this to a hard reject once
	// the CLI has the cached company exchange map.
	_, _ = fmt.Fprintf(w, "warning: row specifies both issuer_key and country -- country will be ignored; the company's exchange determines the country.\n")
}

func checkFilingType(row SpecRow) *CascadeError {
	formtype := row.Filters["formtype"]
	if formtype == nil {
		return nil
	}
	var values []string
	switch v := formtype.(type) {
	case string:
		if v != "" {
			values = []string{v}
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				values = append(values, s)
			}
		}
	case []string:
		for _, s := range v {
			if s != "" {
				values = append(values, s)
			}
		}
	}
	all := allFilingTypes
	allSlice := append(rules.SecFilingTypes, rules.SedarFilingTypes...)
	for _, val := range values {
		if _, ok := all[val]; ok {
			continue
		}
		// Unknown — find near-match
		suggestion := nearMatch(val, allSlice)
		first5 := firstN(allSlice, 5)
		if suggestion != "" {
			return &CascadeError{
				Rule: "filing_type",
				Message: fmt.Sprintf(
					`Unknown filing type: "%s". Did you mean "%s"? Valid filing types include: %s`,
					val, suggestion, strings.Join(first5, ", "),
				),
			}
		}
		return &CascadeError{
			Rule: "filing_type",
			Message: fmt.Sprintf(
				`Unknown filing type: "%s". Valid filing types include: %s`,
				val, strings.Join(first5, ", "),
			),
		}
	}
	return nil
}

func checkIssuerDateSpan(row SpecRow) *CascadeError {
	issuerKey := stringVal(row.Filters, "issuer_key")
	if issuerKey == "" {
		return nil
	}
	// If a formtype or formdescription filter is present, the row is narrow enough.
	if hasNonEmpty(row.Filters, "formtype") || hasNonEmpty(row.Filters, "formdescription") {
		return nil
	}
	dateFrom := stringVal(row.Filters, "date_from")
	dateTo := stringVal(row.Filters, "date_to")
	maxDays := rules.DateSpanMaxDays
	if maxDays == 0 {
		maxDays = 730
	}
	span := dateSpanDays(dateFrom, dateTo)
	if span != nil && *span <= maxDays {
		return nil
	}
	var nDays string
	if span == nil {
		nDays = "missing or invalid"
	} else {
		nDays = fmt.Sprintf("%d", *span)
	}
	return &CascadeError{
		Rule: "issuer_date_span",
		Message: fmt.Sprintf(
			"Row pinned to issuer \"%s\" with no filing-type filter must narrow with a date range of 2 years or less; got %s days. Add a formtype filter OR shrink date_from..date_to.",
			issuerKey, nDays,
		),
	}
}

// ProjectCountry applies the country->exchange[] wire-send projection.
// Called at wire-send time (not at validation time) by archivist table.
// Returns a copy of filters with country replaced by exchange[].
func ProjectCountry(filters map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(filters))
	for k, v := range filters {
		out[k] = v
	}
	country, _ := out["country"].(string)
	delete(out, "country")
	// Explicit exchange always wins.
	if hasNonEmpty(out, "exchange") {
		return out
	}
	if country == "CA" || country == "US" {
		exchanges := rules.ExchangesByCountry[country]
		if len(exchanges) > 0 {
			out["exchange"] = exchanges
		}
	}
	return out
}

// ExplainRules returns the human-readable rule documentation string.
// Used by `archivist explain cascade`.
func ExplainRules() string {
	r := loadedRules()
	sb := &strings.Builder{}
	fmt.Fprintf(sb, "Cascade rules enforced by archivist CLI (source: cascade-rules.json v%s, generated %s)\n\n", r.Version, r.Generated)

	fmt.Fprintf(sb, "RULE: custom_x_filings (hard reject, exit 8)\n")
	fmt.Fprintf(sb, "  Custom-entity rows can only be used with web-source columns.\n")
	fmt.Fprintf(sb, "  A custom row has a free-text custom_entity filter (e.g., \"Goldman Sachs private wealth\").\n")
	fmt.Fprintf(sb, "  Filings columns require an issuer_key. The combination always produces empty cells.\n\n")

	caExchanges := strings.Join(r.ExchangesByCountry["CA"], ", ")
	usExchanges := strings.Join(r.ExchangesByCountry["US"], ", ")
	fmt.Fprintf(sb, "RULE: country_projection (wire transformation, no exit code change)\n")
	fmt.Fprintf(sb, "  country=CA projects to exchange=[%s] at wire-send time.\n", caExchanges)
	fmt.Fprintf(sb, "  country=US projects to exchange=[%s] at wire-send time.\n", usExchanges)
	fmt.Fprintf(sb, "  The country key is dropped from the wire payload in both cases.\n\n")

	fmt.Fprintf(sb, "RULE: company_lock (soft warning in Phase 1)\n")
	fmt.Fprintf(sb, "  Specifying both issuer_key and country in the same row is redundant.\n")
	fmt.Fprintf(sb, "  The company's exchange determines its country. The country filter is ignored.\n\n")

	secTypes := r.SecFilingTypes
	sedarTypes := r.SedarFilingTypes
	fmt.Fprintf(sb, "RULE: filing_type (hard reject, exit 8)\n")
	fmt.Fprintf(sb, "  The formtype filter must be a known SEC code (e.g., 10-K, 10-Q, 8-K)\n")
	fmt.Fprintf(sb, "  or a known SEDAR label (e.g., MD&A, Annual information form).\n\n")
	fmt.Fprintf(sb, "  SEC filing types (%d total):\n    %s\n\n", len(secTypes), strings.Join(secTypes, ", "))
	fmt.Fprintf(sb, "  SEDAR filing types (%d total):\n    %s\n\n", len(sedarTypes), strings.Join(sedarTypes, ", "))

	fmt.Fprintf(sb, "RULE: issuer_date_span (hard reject, exit 8)\n")
	fmt.Fprintf(sb, "  Issuer-locked rows without a filing-type filter must carry a date range of\n")
	fmt.Fprintf(sb, "  %d days (~2 years) or less.\n", r.DateSpanMaxDays)
	fmt.Fprintf(sb, "  Deep-corpus issuers (AAPL, MSFT, etc.) have ~30 years of filings which can\n")
	fmt.Fprintf(sb, "  take 30+ seconds on cold cache. Add a formtype filter OR narrow the date range.\n")

	return sb.String()
}

// ExplainRulesJSON returns the raw cascade-rules.json content as a string.
func ExplainRulesJSON() string {
	return string(cascadeRulesJSON)
}

// TableSearchSchemaJSON returns the raw table-search-schema.json content.
func TableSearchSchemaJSON() []byte {
	return tableSearchSchemaJSON
}

// CascadeRulesJSONBytes returns the raw cascade-rules.json bytes for embedding verification.
func CascadeRulesJSONBytes() []byte {
	return cascadeRulesJSON
}

// CascadeRulesVersion returns the version field from the loaded JSON.
func CascadeRulesVersion() string {
	return rules.Version
}

// ParseSpec parses a JSON-like map into a Spec. This is a convenience for
// callers that have the spec as map[string]interface{} (e.g., from YAML parse).
func ParseSpec(raw map[string]interface{}) (*Spec, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// helpers

func stringVal(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func hasNonEmpty(m map[string]interface{}, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	switch val := v.(type) {
	case string:
		return val != ""
	case []interface{}:
		return len(val) > 0
	case []string:
		return len(val) > 0
	}
	return false
}

func dateSpanDays(from, to string) *int {
	if from == "" || to == "" {
		return nil
	}
	layout := "2006-01-02"
	fromT, err1 := time.Parse(layout, from)
	toT, err2 := time.Parse(layout, to)
	if err1 != nil || err2 != nil {
		return nil
	}
	if toT.Before(fromT) {
		return nil
	}
	days := int(math.Floor(toT.Sub(fromT).Hours() / 24))
	return &days
}

func firstN(slice []string, n int) []string {
	if len(slice) <= n {
		return slice
	}
	return slice[:n]
}
