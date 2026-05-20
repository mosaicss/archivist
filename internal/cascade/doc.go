// Package cascade holds the filter cascade rules ported from
// apps/web/lib/filter-cascade.ts + filter-taxonomy.ts in the Mosaic monorepo.
//
// Filled in by Story 36.8: custom-entity x filings-source rejection at
// spec-parse time, country -> exchange[] projection, company-locks-country-exchange,
// SEC vs SEDAR filing-type catalog split. The rules ship via go:embed of a
// cascade-rules.json codegen artifact maintained in the web monorepo so the
// CLI and web UI stay in lockstep.
package cascade
