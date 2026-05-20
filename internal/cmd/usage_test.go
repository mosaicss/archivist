package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderBar(t *testing.T) {
	tests := []struct {
		name     string
		used     int
		total    int
		width    int
		wantLen  int
		wantFull bool // all filled
	}{
		{name: "zero/zero unlimited", used: 0, total: 0, width: 20, wantLen: 20, wantFull: true},
		{name: "half", used: 5, total: 10, width: 20, wantLen: 20},
		{name: "full", used: 10, total: 10, width: 20, wantLen: 20},
		{name: "over limit clamps to full", used: 15, total: 10, width: 20, wantLen: 20, wantFull: true},
		{name: "zero used", used: 0, total: 10, width: 20, wantLen: 20},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bar := RenderBar(tc.used, tc.total, tc.width)
			// Measure rune count (each block char is multi-byte).
			runeCount := 0
			for range bar {
				runeCount++
			}
			if runeCount != tc.wantLen {
				t.Errorf("RenderBar(%d,%d,%d) rune length = %d, want %d; bar=%q",
					tc.used, tc.total, tc.width, runeCount, tc.wantLen, bar)
			}
			if tc.wantFull {
				if strings.ContainsRune(bar, '░') {
					t.Errorf("expected all filled, got partial bar: %q", bar)
				}
			}
		})
	}
}

func TestRenderBar_HalfFilledContent(t *testing.T) {
	bar := RenderBar(5, 10, 20)
	filled := 0
	empty := 0
	for _, r := range bar {
		switch r {
		case '█':
			filled++
		case '░':
			empty++
		}
	}
	if filled != 10 || empty != 10 {
		t.Errorf("50%% bar: want 10 filled + 10 empty, got filled=%d empty=%d", filled, empty)
	}
}

func TestFormatUsageHuman_ProWithBundle(t *testing.T) {
	limit := (*int)(nil) // unlimited for pro
	u := &UsageResponse{
		Tier:      "pro",
		TierLabel: "Pro ($30/mo, active)",
		Bundle: &UsageBundle{
			Name:       "Archivist CLI",
			PriceLabel: "$15/mo bolt-on, active",
			Active:     true,
		},
		Usage: UsageStats{
			ThisMonth:    47,
			WebThisMonth: 30,
			CLIThisMonth: 17,
			Limit:        limit,
			ResetDate:    "2026-06-01",
		},
		RateLimit: UsageRateLimit{
			WindowSeconds:  60,
			Limit:          100,
			Used:           3,
			Remaining:      97,
			ResetInSeconds: 23,
		},
		Last7Days: []int{5, 8, 7, 9, 6, 7, 5},
	}

	var buf bytes.Buffer
	formatUsageHuman(&buf, u)
	out := buf.String()

	if !strings.Contains(out, "Pro ($30/mo, active)") {
		t.Errorf("missing tier label in output:\n%s", out)
	}
	if !strings.Contains(out, "Archivist CLI") {
		t.Errorf("missing bundle name in output:\n%s", out)
	}
	if !strings.Contains(out, "47 / unlimited") {
		t.Errorf("missing monthly count in output:\n%s", out)
	}
	if !strings.Contains(out, "web: 30, CLI: 17") {
		t.Errorf("missing web/cli breakdown in output:\n%s", out)
	}
	if !strings.Contains(out, "97 / 100 requests/min") {
		t.Errorf("missing rate limit in output:\n%s", out)
	}
	if !strings.Contains(out, "Last 7 days:") {
		t.Errorf("missing bar chart in output:\n%s", out)
	}
}

func TestFormatUsageHuman_FreeTier(t *testing.T) {
	limit := 25
	u := &UsageResponse{
		Tier:      "free",
		TierLabel: "Free",
		Bundle:    nil,
		Usage: UsageStats{
			ThisMonth:    12,
			WebThisMonth: 12,
			CLIThisMonth: 0,
			Limit:        &limit,
			ResetDate:    "2026-06-01",
		},
		RateLimit: UsageRateLimit{
			WindowSeconds:  60,
			Limit:          100,
			Used:           0,
			Remaining:      100,
			ResetInSeconds: 60,
		},
		Last7Days: []int{2, 1, 3, 2, 1, 2, 1},
	}

	var buf bytes.Buffer
	formatUsageHuman(&buf, u)
	out := buf.String()

	if !strings.Contains(out, "Free") {
		t.Errorf("missing tier in output:\n%s", out)
	}
	if !strings.Contains(out, "Bundle:      none") {
		t.Errorf("missing bundle:none in output:\n%s", out)
	}
	if !strings.Contains(out, "12 / 25") {
		t.Errorf("missing monthly count in output:\n%s", out)
	}
}
