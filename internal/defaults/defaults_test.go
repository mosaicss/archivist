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

func TestApplyTableRowDefaults_IssuerOnlyDateFrom_FillsDateTo(t *testing.T) {
	// User provided date_from only — keep it, fill date_to with today.
	// Story 37.8 — previously this overwrote BOTH dates; now preserves user input.
	row := tablespec.SpecRow{
		Company:  "aapl_us",
		DateFrom: "2025-01-01",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if !applied.Filled {
		t.Fatal("expected Filled=true when date_to is missing")
	}
	if got.DateFrom != "2025-01-01" {
		t.Errorf("DateFrom must be preserved: want 2025-01-01, got %q", got.DateFrom)
	}
	if got.DateTo != "2026-05-20" {
		t.Errorf("DateTo: want 2026-05-20 (today), got %q", got.DateTo)
	}
	if applied.DateFrom != "" {
		t.Errorf("Applied.DateFrom must be empty (user provided it): got %q", applied.DateFrom)
	}
	if applied.DateTo != "2026-05-20" {
		t.Errorf("Applied.DateTo: want 2026-05-20, got %q", applied.DateTo)
	}
	if applied.DateLabel != "today" {
		t.Errorf("DateLabel: want %q, got %q", "today", applied.DateLabel)
	}
}

func TestApplyTableRowDefaults_IssuerOnlyDateTo_FillsDateFrom(t *testing.T) {
	// User provided date_to only — keep it, anchor date_from = date_to - 6mo.
	// Anchoring on date_to (not today) avoids inverted windows when date_to is
	// historical (e.g. date_to=2024-12-31 with today=2026-05-20 would give
	// from=2025-11-20 to=2024-12-31 — broken).
	row := tablespec.SpecRow{
		Company: "aapl_us",
		DateTo:  "2024-12-31",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if !applied.Filled {
		t.Fatal("expected Filled=true when date_from is missing")
	}
	if got.DateTo != "2024-12-31" {
		t.Errorf("DateTo must be preserved: want 2024-12-31, got %q", got.DateTo)
	}
	// 2024-12-31 minus 6 months in Go's AddDate(0, -6, 0) overflows day 31 of
	// June (30 days) forward to 2024-07-01 — matches the existing Aug 31 edge
	// case test's normalisation semantics.
	if got.DateFrom != "2024-07-01" {
		t.Errorf("DateFrom: want 2024-07-01 (6 months before user's date_to with calendar overflow), got %q", got.DateFrom)
	}
	if applied.DateTo != "" {
		t.Errorf("Applied.DateTo must be empty (user provided it): got %q", applied.DateTo)
	}
	if applied.DateFrom != "2024-07-01" {
		t.Errorf("Applied.DateFrom: want 2024-07-01, got %q", applied.DateFrom)
	}
	if applied.DateLabel != "6 months before date_to" {
		t.Errorf("DateLabel: want %q, got %q", "6 months before date_to", applied.DateLabel)
	}
}

func TestApplyTableRowDefaults_IssuerOnlyDateTo_MalformedDateNoOp(t *testing.T) {
	// User provided a malformed date_to — defaults function bails to no-op so
	// the cascade validator can surface the parse error with its own message.
	row := tablespec.SpecRow{
		Company: "aapl_us",
		DateTo:  "not-a-date",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	got, applied := defaults.ApplyTableRowDefaults(row, now)

	if applied.Filled {
		t.Error("expected Filled=false for malformed date_to")
	}
	if got.DateFrom != "" || got.DateTo != "not-a-date" {
		t.Errorf("expected row unchanged on parse error; got from=%q to=%q", got.DateFrom, got.DateTo)
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
