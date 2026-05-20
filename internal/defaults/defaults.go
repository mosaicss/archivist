// Package defaults mirrors the "Last 6 months" auto-fill that the web Table Mode
// applies in apps/web/components/table/row-filter-picker.tsx:632-646 (Story 33.43).
package defaults

import (
	"time"

	"github.com/mosaicss/archivist/internal/tablespec"
)

// Applied describes what ApplyTableRowDefaults filled in for one row.
type Applied struct {
	Filled    bool
	DateFrom  string
	DateTo    string
	DateLabel string
}

// ApplyTableRowDefaults fills date_from / date_to with "Last 6 months" when the
// row is issuer-locked (Company set) and both date fields are absent.
//
// Calendar arithmetic uses time.AddDate(0, -6, 0) which matches the JS
// Date.setMonth(today.getMonth()-6) semantics used in row-filter-picker.tsx.
// Both Go and JS overflow the date forward when the target month is shorter
// (e.g. Aug 31 minus 6 months → Mar 3, because Feb 31 overflows to Mar 3).
func ApplyTableRowDefaults(row tablespec.SpecRow, now time.Time) (tablespec.SpecRow, Applied) {
	// No issuer pin: no-op.
	if row.Company == "" {
		return row, Applied{}
	}

	// Both dates already provided: no-op.
	if row.DateFrom != "" && row.DateTo != "" {
		return row, Applied{}
	}

	// Inverted range (only one field set but we can detect to/from mismatch
	// after a partial fill): if one is empty but the other is not, fill both.
	// For the case where both are empty OR only one is missing we always fill
	// both so the cascade validator gets a consistent picture.
	// Edge case: if date_to < date_from after a hypothetical fill we leave it
	// to the 36.8 validator. We never fill partial to produce an inverted range.

	today := now.UTC()
	dateTo := today.Format("2006-01-02")
	dateFrom := today.AddDate(0, -6, 0).Format("2006-01-02")

	row.DateFrom = dateFrom
	row.DateTo = dateTo

	return row, Applied{
		Filled:    true,
		DateFrom:  dateFrom,
		DateTo:    dateTo,
		DateLabel: "Last 6 months",
	}
}
