package cmd

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestFormatUsageHuman_Pro(t *testing.T) {
	limit := (*int)(nil) // unlimited for pro
	u := &UsageResponse{
		Tier:      "pro",
		TierLabel: "Pro ($30/mo, active)",
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
	if strings.Contains(out, "Bundle") {
		t.Errorf("the Bundle line is gone since 73.7:\n%s", out)
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
	if !strings.Contains(out, "12 / 25") {
		t.Errorf("missing monthly count in output:\n%s", out)
	}
}

// TestUsageMapsHTTPFailures routes non-2xx /account/usage answers through the
// shared mapper: a free account's 403 PRO_REQUIRED is exit 4 with the
// account URL, a 401 is exit 4.
func TestUsageMapsHTTPFailures(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		wantExit int
		wantErr  string
	}{
		{http.StatusForbidden, `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`, ExitAuthError, "https://mosaic-finance.com/en/pricing/"},
		{http.StatusUnauthorized, ``, ExitAuthError, "authentication failed. Run 'archivist auth status'"},
		{http.StatusPaymentRequired, `{"error":"Upgrade needed","code":"PAYMENT_REQUIRED"}`, ExitAuthError, "Upgrade needed"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
		root := NewRootCmd("0.2.22", "abc", "today")
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs([]string{"usage"})
		err := root.Execute()
		srv.Close()
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != tc.wantExit {
			t.Errorf("status %d: want exit %d, got %v", tc.status, tc.wantExit, err)
		}
		if !strings.Contains(stderr.String(), tc.wantErr) {
			t.Errorf("status %d: stderr missing %q:\n%s", tc.status, tc.wantErr, stderr.String())
		}
		if strings.Contains(stderr.String(), "quota exceeded") {
			t.Errorf("status %d: must never print 'quota exceeded'", tc.status)
		}
	}
}
