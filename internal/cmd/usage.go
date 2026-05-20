package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/spf13/cobra"
)

// UsageResponse mirrors the GET /account/usage JSON shape.
type UsageResponse struct {
	Tier      string          `json:"tier"`
	TierLabel string          `json:"tier_label"`
	Bundle    *UsageBundle    `json:"bundle"`
	Usage     UsageStats      `json:"usage"`
	FreeTrial interface{}     `json:"free_trial"`
	RateLimit UsageRateLimit  `json:"rate_limit"`
	Last7Days []int           `json:"last_7_days"`
}

type UsageBundle struct {
	Name       string `json:"name"`
	PriceLabel string `json:"price_label"`
	Active     bool   `json:"active"`
}

type UsageStats struct {
	ThisMonth    int     `json:"this_month"`
	WebThisMonth int     `json:"web_this_month"`
	CLIThisMonth int     `json:"cli_this_month"`
	Limit        *int    `json:"limit"`
	ResetDate    string  `json:"reset_date"`
}

type UsageRateLimit struct {
	WindowSeconds int `json:"window_seconds"`
	Limit         int `json:"limit"`
	Used          int `json:"used"`
	Remaining     int `json:"remaining"`
	ResetInSeconds int `json:"reset_in_seconds"`
}

// RenderBar renders a Unicode block-character bar of width chars.
// filled = used, total = limit. If total <= 0, all chars are filled (unlimited).
func RenderBar(used, total, width int) string {
	if width <= 0 {
		width = 20
	}
	if total <= 0 {
		// Unlimited — show all filled.
		return strings.Repeat("█", width)
	}
	pct := float64(used) / float64(total)
	if pct > 1.0 {
		pct = 1.0
	}
	filled := int(math.Round(pct * float64(width)))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// formatUsageHuman prints the human-readable usage summary.
func formatUsageHuman(out io.Writer, u *UsageResponse) {
	// Tier line
	_, _ = fmt.Fprintf(out, "Tier:        %s\n", u.TierLabel)

	// Bundle line
	if u.Bundle != nil && u.Bundle.Active {
		_, _ = fmt.Fprintf(out, "Bundle:      + %s (%s)\n", u.Bundle.Name, u.Bundle.PriceLabel)
	} else {
		_, _ = fmt.Fprintf(out, "Bundle:      none\n")
	}

	// Monthly usage line
	limitStr := "unlimited"
	if u.Usage.Limit != nil {
		limitStr = fmt.Sprintf("%d", *u.Usage.Limit)
	}
	_, _ = fmt.Fprintf(out, "This month:  %d / %s queries  (web: %d, CLI: %d)\n",
		u.Usage.ThisMonth, limitStr, u.Usage.WebThisMonth, u.Usage.CLIThisMonth)

	// Free trial line
	if u.FreeTrial != nil {
		_, _ = fmt.Fprintf(out, "Free trial:  %v\n", u.FreeTrial)
	} else if u.Tier == "free" {
		_, _ = fmt.Fprintf(out, "Free trial:  N/A (free tier, not a trial)\n")
	} else {
		_, _ = fmt.Fprintf(out, "Free trial:  N/A (active paid subscription)\n")
	}

	// Rate limit line
	rl := u.RateLimit
	_, _ = fmt.Fprintf(out, "Rate limit:  %d / %d requests/min (resets in %ds)\n",
		rl.Remaining, rl.Limit, rl.ResetInSeconds)

	// Last 7 days bar chart
	total7 := 0
	for _, n := range u.Last7Days {
		total7 += n
	}
	barTotal := u.Usage.Limit
	var barTotalInt int
	if barTotal != nil {
		barTotalInt = *barTotal
	}
	bar := RenderBar(total7, barTotalInt, 20)
	unlimitedSuffix := ""
	if barTotal == nil {
		unlimitedSuffix = "  (unlimited)"
	}
	_, _ = fmt.Fprintf(out, "Last 7 days: %s %d calls%s\n", bar, total7, unlimitedSuffix)
}

// NewUsageCmd returns the `archivist usage` command (replaces stub from Story 36.1).
func NewUsageCmd(version string) *cobra.Command {
	var formatFlag string

	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Report quota and rate limit consumption",
		Args:  cobra.NoArgs,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,4,5",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			tokenFlag, _ := cmd.Root().PersistentFlags().GetString("token")
			token, err := auth.ResolveToken(tokenFlag)
			if err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "archivist: not authenticated -- run 'archivist auth status' to verify your token")
				return &ExitError{Code: ExitAuthError}
			}

			c := client.New(token, version)

			// Auto-JSON when not a TTY.
			if !isTerminal(cmd.OutOrStdout()) && formatFlag == "" {
				formatFlag = "json"
			}

			resp, err := c.Do(cmd.Context(), http.MethodGet, "/account/usage", nil)
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist: server error: %v\n", err)
				return &ExitError{Code: ExitServerError}
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode == http.StatusUnauthorized {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "archivist: not authenticated -- run 'archivist auth status' to verify your token")
				return &ExitError{Code: ExitAuthError}
			}
			if resp.StatusCode >= 500 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist: server error (HTTP %d)\n", resp.StatusCode)
				return &ExitError{Code: ExitServerError}
			}

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist: failed to read response: %v\n", err)
				return &ExitError{Code: ExitServerError}
			}

			if formatFlag == "json" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(bodyBytes))
				return nil
			}

			var usageResp UsageResponse
			if err := json.Unmarshal(bodyBytes, &usageResp); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist: failed to parse response: %v\n", err)
				return &ExitError{Code: ExitServerError}
			}

			formatUsageHuman(cmd.OutOrStdout(), &usageResp)
			return nil
		},
	}

	cmd.Flags().StringVar(&formatFlag, "format", "", "Output format: human (default on TTY) or json")
	return cmd
}
