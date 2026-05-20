package defaults_test

import (
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/defaults"
	"github.com/mosaicss/archivist/internal/tablespec"
)

func TestApplyTableRowDefaults_NoIssuer_NoOp(t *testing.T) {
	row := tablespec.SpecRow{}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if applied.Filled {
		t.Error("expected Filled=false for row without issuer_key")
	}
	if got.DateFrom != "" || got.DateTo != "" {
		t.Errorf("expected empty dates, got from=%q to=%q", got.DateFrom, got.DateTo)
	}
}

func TestApplyTableRowDefaults_IssuerWithDates_NoOp(t *testing.T) {
	row := tablespec.SpecRow{
		Company:  "aapl_us",
		DateFrom: "2025-01-01",
		DateTo:   "2025-12-31",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if applied.Filled {
		t.Error("expected Filled=false when both dates are already set")
	}
	if got.DateFrom != "2025-01-01" || got.DateTo != "2025-12-31" {
		t.Errorf("dates should be unchanged; got from=%q to=%q", got.DateFrom, got.DateTo)
	}
}

func TestApplyTableRowDefaults_IssuerOnly_FillsLastSixMonths(t *testing.T) {
	row := tablespec.SpecRow{Company: "aapl_us"}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if !applied.Filled {
		t.Fatal("expected Filled=true")
	}
	if applied.DateTo != "2026-05-20" {
		t.Errorf("DateTo: want 2026-05-20, got %q", applied.DateTo)
	}
	if applied.DateFrom != "2025-11-20" {
		t.Errorf("DateFrom: want 2025-11-20, got %q", applied.DateFrom)
	}
	if applied.DateLabel != "Last 6 months" {
		t.Errorf("DateLabel: want %q, got %q", "Last 6 months", applied.DateLabel)
	}
	if got.DateFrom != "2025-11-20" || got.DateTo != "2026-05-20" {
		t.Errorf("row not updated: from=%q to=%q", got.DateFrom, got.DateTo)
	}
}

func TestApplyTableRowDefaults_IssuerPartialDates_FillsBoth(t *testing.T) {
	// Only date_from set (date_to missing) — fills both.
	row := tablespec.SpecRow{
		Company:  "aapl_us",
		DateFrom: "2025-01-01",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if !applied.Filled {
		t.Fatal("expected Filled=true when date_to is missing")
	}
	if got.DateTo != "2026-05-20" {
		t.Errorf("DateTo: want 2026-05-20, got %q", got.DateTo)
	}
	// Both from and to overwritten with the standard 6-month window.
	if got.DateFrom != "2025-11-20" {
		t.Errorf("DateFrom: want 2025-11-20, got %q", got.DateFrom)
	}
}

func TestApplyTableRowDefaults_InvertedRange_NoOp(t *testing.T) {
	// date_to < date_from: leave for the 36.8 cascade validator; no-op here.
	row := tablespec.SpecRow{
		Company:  "aapl_us",
		DateFrom: "2026-05-20",
		DateTo:   "2025-01-01",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if applied.Filled {
		t.Error("expected Filled=false for inverted range (cascade validator handles it)")
	}
	if got.DateFrom != "2026-05-20" || got.DateTo != "2025-01-01" {
		t.Errorf("dates should be unchanged; got from=%q to=%q", got.DateFrom, got.DateTo)
	}
}

func TestApplyTableRowDefaults_CalendarEdgeAug31(t *testing.T) {
	// Aug 31 minus 6 months using time.AddDate(0, -6, 0).
	// Go (and JS) overflow Feb 31 to Mar 3 in non-leap years; result is 2026-03-03.
	// This matches JS Date.setMonth() semantics — both platforms normalise the overflow
	// to the next valid day rather than clamping to Feb 28.
	row := tablespec.SpecRow{Company: "td_ca"}
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if !applied.Filled {
		t.Fatal("expected Filled=true")
	}
	if got.DateTo != "2026-08-31" {
		t.Errorf("DateTo: want 2026-08-31, got %q", got.DateTo)
	}
	// time.AddDate(0, -6, 0) on Aug 31 → Mar 3 (Feb overflow normalises forward).
	if got.DateFrom != "2026-03-03" {
		t.Errorf("DateFrom: want 2026-03-03, got %q", got.DateFrom)
	}
}
