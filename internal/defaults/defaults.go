// Package defaults mirrors the "Last 6 months" auto-fill that the web Table Mode
// applies in apps/web/components/table/row-filter-picker.tsx:632-646 (Story 33.43).
package defaults

import (
	"time"

	"github.com/mosaicss/archivist/internal/tablespec"
)

// Applied describes what ApplyTableRowDefaults filled in for one row.
// DateFrom / DateTo hold the filled-in values; they are empty for fields the
// user already provided. Filled is true if any field was filled.
type Applied struct {
	Filled    bool
	DateFrom  string
	DateTo    string
	DateLabel string
}

// ApplyTableRowDefaults fills missing date_from / date_to on issuer-locked
// rows. User-provided dates are preserved. Three cases:
//
//   - Neither date set         → fill date_from = today-6mo, date_to = today
//     (label: "Last 6 months")
//   - Only date_from set       → fill date_to = today
//     (label: "today")
//   - Only date_to set         → fill date_from = date_to minus 6 months
//     (label: "6 months before date_to"). Anchoring on the user's date_to
//     avoids producing an inverted window when they specify a historical
//     date_to.
//   - Both set                 → no-op
//
// Calendar arithmetic uses time.AddDate(0, -6, 0) which matches the JS
// Date.setMonth(today.getMonth()-6) semantics used in row-filter-picker.tsx.
// Both Go and JS overflow forward when the target month is shorter
// (e.g. Aug 31 minus 6 months → Mar 3, because Feb 31 overflows to Mar 3).
//
// Story 37.8 changed the partial-fill case: previously both dates were
// overwritten whenever either was missing. Agent-driven callers commonly
// supply only one bound (e.g. "all 10-Ks since 2020"); the override was
// silently dropping their input.
func ApplyTableRowDefaults(row tablespec.SpecRow, now time.Time) (tablespec.SpecRow, Applied) {
	if row.Company == "" {
		return row, Applied{}
	}
	if row.DateFrom != "" && row.DateTo != "" {
		return row, Applied{}
	}

	today := now.UTC()
	todayStr := today.Format("2006-01-02")

	switch {
	case row.DateFrom == "" && row.DateTo == "":
		sixMonthsAgoStr := today.AddDate(0, -6, 0).Format("2006-01-02")
		row.DateFrom = sixMonthsAgoStr
		row.DateTo = todayStr
		return row, Applied{
			Filled:    true,
			DateFrom:  sixMonthsAgoStr,
			DateTo:    todayStr,
			DateLabel: "Last 6 months",
		}

	case row.DateFrom != "" && row.DateTo == "":
		row.DateTo = todayStr
		return row, Applied{
			Filled:    true,
			DateTo:    todayStr,
			DateLabel: "today",
		}

	case row.DateFrom == "" && row.DateTo != "":
		dateToParsed, err := time.Parse("2006-01-02", row.DateTo)
		if err != nil {
			// Malformed date_to — leave to the cascade validator.
			return row, Applied{}
		}
		sixMonthsBefore := dateToParsed.AddDate(0, -6, 0).Format("2006-01-02")
		row.DateFrom = sixMonthsBefore
		return row, Applied{
			Filled:    true,
			DateFrom:  sixMonthsBefore,
			DateLabel: "6 months before date_to",
		}
	}

	return row, Applied{}
}
