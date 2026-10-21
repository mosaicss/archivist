package cmd

// screen.go implements `archivist screen` over chat-api's company screen:
//
//	archivist screen [flags]                         GET /research/screen
//
// The server owns the field registry and every rule on keys and values; the
// CLI checks only the shape of each bound (KEY=VALUE), the paging and the
// date, so a bad call fails before a screen is spent. Every flag is sent only
// when it was set on the command line: an unset flag is not a default.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const (
	minScreenLimit     = 1
	maxScreenLimit     = 50
	defaultScreenLimit = 25
	maxBoundValueChars = 32
	screenCellRunes    = 32
)

// screenKeyRE is the shape of a registry field key; the server decides
// whether the key exists and can be filtered.
var screenKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type screenFlags struct {
	industry, sector, exchange, market, country, currency string
	asOf, sortBy, sortDir, fields, cursor, format         string
	mins, maxes                                           []string
	limit                                                 int
	includeInactive, dryRun                               bool
}

// screenListFlags are the scope flags passed through trimmed, in the
// server's parameter names.
var screenListFlags = []struct{ flag, param string }{
	{"industry", "industry"},
	{"sector", "sector"},
	{"exchange", "exchange"},
	{"market", "market"},
	{"country", "country"},
	{"currency", "currency"},
	{"sort-by", "sort_by"},
	{"fields", "fields"},
}

func newScreenCmd(version string) *cobra.Command {
	var f screenFlags
	c := &cobra.Command{
		Use:   "screen",
		Short: "Screen companies by ratios, size and returns on the nightly snapshot",
		Long: `Screen covered companies (TSX, TSX Venture, NYSE, Nasdaq, NYSE American and
Borsa Istanbul) on the nightly snapshot by ratios, size, returns, annual
figures and filing counts. A bound is min or max with KEY=VALUE, such as max
pe=15 or min market_cap_usd_m=500; repeat either for more fields. Suffixes:
_usd_m is millions of US dollars, _pct is percent, _m is millions. Figures in
each company's own reporting unit (share_price, eps, market_cap_m, every fy_
amount) need currency set to its ISO 4217 code. industry takes plain terms
such as mining; sector, exchange, market and country take comma separated
values. Report total_matched, every excluded reason, interpretation,
as_of_used, coverage_note and any disclosures and note. Screen figures are
data, not citations.`,
		Args:        cobra.NoArgs,
		Annotations: researchAnnotations("Screen companies", "0,2,4,5,7"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScreen(cmd, version, &f)
		},
	}
	c.Flags().StringVar(&f.industry, "industry", "", "Plain industry terms separated by commas, such as mining or banks")
	c.Flags().StringVar(&f.sector, "sector", "", "Mosaic sectors separated by commas, such as materials,energy")
	c.Flags().StringVar(&f.exchange, "exchange", "", "Exchanges separated by commas: TSX, TSXV, NYSE, NASDAQ, NYSE_AMERICAN, BIST")
	c.Flags().StringVar(&f.market, "market", "", "Markets separated by commas: CA, US, TR")
	c.Flags().StringVar(&f.country, "country", "", "Two letter countries of domicile separated by commas, such as CA,TR")
	c.Flags().StringVar(&f.currency, "currency", "", "Three letter ISO 4217 code, needed to filter or sort a figure in each company's own reporting unit")
	c.Flags().StringArrayVar(&f.mins, "min", nil, "Lower bound KEY=VALUE, such as market_cap_usd_m=500 (repeatable)")
	c.Flags().StringArrayVar(&f.maxes, "max", nil, "Upper bound KEY=VALUE, such as pe=15 (repeatable)")
	c.Flags().BoolVar(&f.includeInactive, "include-inactive", false, "Include inactive and delisted companies (default false)")
	c.Flags().StringVar(&f.asOf, "as-of", "", "Snapshot date, YYYY-MM-DD: the newest snapshot on or before it")
	c.Flags().StringVar(&f.sortBy, "sort-by", "", "A field key, or name (default market_cap_usd_m)")
	c.Flags().StringVar(&f.sortDir, "sort-dir", "", "desc (default) or asc; companies without a value sort last")
	c.Flags().StringVar(&f.fields, "fields", "", "More field keys to show in each row, separated by commas")
	c.Flags().IntVar(&f.limit, "limit", defaultScreenLimit, "Companies per page (1 to 50)")
	c.Flags().StringVar(&f.cursor, "cursor", "", cursorUsage)
	c.Flags().StringVar(&f.format, "format", "", formatUsage)
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the request without sending it")
	return c
}

// screenBounds checks the --min or --max values and sets <key>_<suffix> for
// each. side is the flag name, for the usage message.
func screenBounds(cmd *cobra.Command, q url.Values, side string, values []string) error {
	seen := map[string]bool{}
	for _, raw := range values {
		key, value, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if !ok {
			return usageErr(cmd, "--%s takes KEY=VALUE, such as pe=15 (got %q)", side, raw)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !screenKeyRE.MatchString(key) {
			return usageErr(cmd, "--%s key %q must be a lower case field key, such as market_cap_usd_m", side, key)
		}
		if seen[key] {
			return usageErr(cmd, "--%s names %s more than once", side, key)
		}
		seen[key] = true
		if value == "" || jsLen(value) > maxBoundValueChars {
			return usageErr(cmd, "--%s %s needs a value of 1 to %d characters", side, key, maxBoundValueChars)
		}
		q.Set(key+"_"+side, value)
	}
	return nil
}

func runScreen(cmd *cobra.Command, version string, f *screenFlags) error {
	if err := checkFormat(cmd, f.format); err != nil {
		return err
	}
	flags := cmd.Flags()
	q := url.Values{}
	for _, lf := range screenListFlags {
		if !flags.Changed(lf.flag) {
			continue
		}
		v, _ := flags.GetString(lf.flag)
		if v = strings.TrimSpace(v); v != "" {
			q.Set(lf.param, v)
		}
	}
	if err := screenBounds(cmd, q, "min", f.mins); err != nil {
		return err
	}
	if err := screenBounds(cmd, q, "max", f.maxes); err != nil {
		return err
	}
	if flags.Changed("include-inactive") {
		q.Set("include_inactive", fmt.Sprintf("%t", f.includeInactive))
	}
	if flags.Changed("as-of") {
		asOf := strings.TrimSpace(f.asOf)
		if err := checkDate(cmd, "as-of", asOf); err != nil {
			return err
		}
		if asOf != "" {
			q.Set("as_of", asOf)
		}
	}
	if flags.Changed("sort-dir") {
		dir := strings.TrimSpace(f.sortDir)
		if d := strings.ToLower(dir); d != "asc" && d != "desc" {
			return usageErr(cmd, "--sort-dir must be asc or desc (got %q)", f.sortDir)
		}
		q.Set("sort_dir", dir)
	}
	if flags.Changed("limit") {
		if f.limit < minScreenLimit || f.limit > maxScreenLimit {
			return usageErr(cmd, "--limit must be from %d to %d (got %d)", minScreenLimit, maxScreenLimit, f.limit)
		}
		q.Set("limit", fmt.Sprintf("%d", f.limit))
	}

	format := resolveFormat(f.format, cmd.OutOrStdout(), "table")
	path, cur, err := pagedPath(cmd, "/research/screen", q, f.cursor, format)
	if err != nil {
		return err
	}
	body, err := fetchResearch(cmd, version, path, format, f.dryRun)
	if err != nil || body == nil {
		return err
	}
	var resp struct {
		page
		TotalMatched int `json:"total_matched"`
	}
	if err := decodeBody(cmd, body, &resp, format); err != nil {
		return err
	}
	switch format {
	case formatCompact:
		err = emitCompact(cmd, body, "rows", cur)
	case "json":
		if err = emitJSON(cmd, body, format); err == nil {
			cursorHint(cmd, resp.NextCursor)
		}
	default:
		if rerr := renderScreenTable(cmd.OutOrStdout(), body); rerr != nil {
			return reportFailure(cmd, failure{
				exitCode: ExitServerError, code: "UNREADABLE_RESPONSE",
				message: fmt.Sprintf("could not parse the response: %v", rerr),
			}, format)
		}
		cursorHint(cmd, resp.NextCursor)
	}
	if err != nil {
		return err
	}
	if resp.TotalMatched == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No companies matched")
	}
	return nil
}

// screenLeadColumns open the row table; the other row keys follow in the
// order the server sent them.
var screenLeadColumns = []string{"name", "symbol", "exchange"}

// renderScreenTable writes the matched count and snapshot date, the row
// table, then excluded, interpretation, disclosures, coverage_note and note.
func renderScreenTable(w io.Writer, body []byte) error {
	root, err := decodeOrdered(bytes.TrimSpace(body))
	if err != nil {
		return err
	}
	obj, ok := root.(jsonObject)
	if !ok {
		return fmt.Errorf("the screen response is not an object")
	}
	total, _ := obj.get("total_matched")
	asOf, _ := obj.get("as_of_used")
	if _, err := fmt.Fprintf(w, "Matched %s companies (as of %s)\n", screenText(total), screenText(asOf)); err != nil {
		return err
	}

	rowsVal, _ := obj.get("rows")
	rows, _ := rowsVal.([]any)
	columns := append([]string{}, screenLeadColumns...)
	known := map[string]bool{}
	for _, k := range columns {
		known[k] = true
	}
	for _, r := range rows {
		ro, _ := r.(jsonObject)
		for _, m := range ro {
			if !known[m.Key] {
				known[m.Key] = true
				columns = append(columns, m.Key)
			}
		}
	}
	if len(rows) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		head := make([]string, len(columns))
		for i, k := range columns {
			head[i] = strings.ToUpper(k)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(head, "\t"))
		for _, r := range rows {
			ro, _ := r.(jsonObject)
			cells := make([]string, len(columns))
			for i, k := range columns {
				v, _ := ro.get(k)
				cells[i] = screenCell(v)
			}
			_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if ex, _ := obj.get("excluded"); ex != nil {
		if eo, ok := ex.(jsonObject); ok && len(eo) > 0 {
			_, _ = fmt.Fprintln(w, "\nExcluded:")
			for _, m := range eo {
				detail, _ := m.Val.(jsonObject)
				count, _ := detail.get("count")
				reason, _ := detail.get("reason")
				_, _ = fmt.Fprintf(w, "  %s: %s (%s)\n", m.Key, screenText(count), screenText(reason))
			}
		}
	}
	if in, _ := obj.get("interpretation"); in != nil {
		if iobj, ok := in.(jsonObject); !ok || len(iobj) > 0 {
			var buf bytes.Buffer
			encodeCompact(&buf, in)
			_, _ = fmt.Fprintf(w, "\nInterpretation: %s\n", buf.String())
		}
	}
	if ds, _ := obj.get("disclosures"); ds != nil {
		if list, ok := ds.([]any); ok && len(list) > 0 {
			_, _ = fmt.Fprintln(w, "\nDisclosures:")
			for _, d := range list {
				_, _ = fmt.Fprintf(w, "  %s\n", screenText(d))
			}
		}
	}
	if cn, _ := obj.get("coverage_note"); nonEmptyString(cn) {
		_, _ = fmt.Fprintf(w, "\nCoverage: %s\n", screenText(cn))
	}
	if note, _ := obj.get("note"); nonEmptyString(note) {
		_, _ = fmt.Fprintf(w, "\nNote: %s\n", screenText(note))
	}
	return nil
}

// screenText renders a decoded scalar as plain text ("-" for null).
func screenText(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return fmt.Sprintf("%t", x)
	default:
		var buf bytes.Buffer
		encodeCompact(&buf, x)
		return buf.String()
	}
}

// screenCell is one table cell: text clipped to the column width, "-" for
// null or empty.
func screenCell(v any) string {
	s := screenText(v)
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return clipRunes(s, screenCellRunes)
}
