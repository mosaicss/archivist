// Package cascade holds the filter cascade rules ported from
// apps/web/lib/filter-cascade.ts + filter-taxonomy.ts in the Mosaic monorepo.
//
// ValidateSpec checks all rows x columns in a tablespec.Spec for cascade
// violations. ProjectCountry applies the country->exchange[] wire-send
// projection. ExplainRules returns a human-readable summary of all rules.
//
// Rule catalog:
//
//	custom_x_filings   - custom-entity row x filings-source column (hard reject, exit 8)
//	country_projection - country=CA/US projects to exchange[] at wire-send time (no reject)
//	company_lock       - issuer_key + country conflicts (soft warn in Phase 1)
//	filing_type        - unknown formtype value (hard reject, exit 8)
//	issuer_date_span   - issuer-locked row without narrowing > 730 days (hard reject, exit 8)
//
// All rule metadata and filing-type catalogs are loaded from cascade-rules.json
// at package init time via go:embed. No hardcoded strings.
package cascade
