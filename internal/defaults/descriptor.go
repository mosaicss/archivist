package defaults

// ExplainDefaultsText is the human-readable descriptor for 'archivist explain defaults'.
// No hyphens or em dashes in user-facing copy (CLAUDE.md rule).
const ExplainDefaultsText = `DEFAULT DATE WINDOW

  When you pin a row to an issuer (issuer_key) without specifying date_from
  / date_to, archivist fills "Last 6 months" automatically:

    date_to   = today (UTC)
    date_from = today minus 6 calendar months
    label     = "Last 6 months"

  This matches the web Table Mode default (Story 33.43).

RECOMMENDED WINDOWS

  Use case            Window
  ------------------  ----------------------------------------
  Recent filings      Last 6 months  (the default, fits most)
  Annual cycle        Last 12 months
  Multi-year compare  Up to 2 years  (hard cap, see below)

HARD CAP (cascade rule)

  Issuer-locked rows without a filing-type filter are capped at a
  730-day (~2 year) date span. Longer spans are rejected at spec-
  parse time (exit code 8). Add a formtype filter to scan farther
  back, or shrink the window.

  See 'archivist explain cascade' for the full rule.

CASCADE PATTERN (filing types)

  The same narrowing pattern that ties issuer to date defaults also
  ties issuer to FILING TYPES. SEC issuers file SEC types (10-K,
  10-Q, 8-K, etc.); SEDAR issuers file SEDAR types (MD&A, AIF, etc.).
  archivist rejects invalid type-for-issuer combinations at spec-parse
  time via the same cascade validator.
`

// ExplainDefaultsJSONSections is a structured equivalent used when --format json
// is requested. Keys match the section headings in ExplainDefaultsText.
var ExplainDefaultsJSONSections = map[string]interface{}{
	"default_date_window": "When you pin a row to an issuer (issuer_key) without specifying date_from / date_to, archivist fills \"Last 6 months\" automatically: date_to = today (UTC), date_from = today minus 6 calendar months, label = \"Last 6 months\". This matches the web Table Mode default (Story 33.43).",
	"recommended_windows": []map[string]string{
		{"use_case": "Recent filings", "window": "Last 6 months (the default, fits most)"},
		{"use_case": "Annual cycle", "window": "Last 12 months"},
		{"use_case": "Multi-year compare", "window": "Up to 2 years (hard cap)"},
	},
	"hard_cap": "Issuer-locked rows without a filing-type filter are capped at a 730-day (~2 year) date span. Longer spans are rejected at spec-parse time (exit code 8). Add a formtype filter to scan farther back, or shrink the window. See 'archivist explain cascade' for the full rule.",
	"cascade_pattern": "The same narrowing pattern that ties issuer to date defaults also ties issuer to FILING TYPES. SEC issuers file SEC types (10-K, 10-Q, 8-K, etc.); SEDAR issuers file SEDAR types (MD&A, AIF, etc.). archivist rejects invalid type-for-issuer combinations at spec-parse time via the same cascade validator.",
}
