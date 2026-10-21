package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// screenBody is a /research/screen answer in the server's member order.
func screenBody(t *testing.T, rows []json.RawMessage, next any, note any) string {
	t.Helper()
	kv := []any{
		"as_of_used", "2026-10-20", "earliest_as_of", "2026-06-01", "total_matched", len(rows) + 10,
		"rows", rawList(rows...),
		"excluded", json.RawMessage(ordered(t,
			"pe", json.RawMessage(ordered(t, "count", 4, "reason", "no P/E: loss making or no earnings data")),
			"inactive", json.RawMessage(ordered(t, "count", 2, "reason", "inactive or delisted")))),
		"interpretation", json.RawMessage(ordered(t, "industry", rawList(json.RawMessage(ordered(t, "matched_term", "mining", "slug", "metals_mining"))))),
		"coverage_note", "TSX, TSX Venture, NYSE, Nasdaq, NYSE American and Borsa Istanbul; active companies only.",
		"disclosures", []string{"BIST bank figures are solo statements."},
		"cohort_id", "cs1.abc", "has_more", next != nil, "truncated", false, "next_cursor", next,
	}
	if note != nil {
		kv = append(kv, "note", note)
	}
	return ordered(t, kv...)
}

func screenRow(t *testing.T, name, symbol string, pe any) json.RawMessage {
	t.Helper()
	return json.RawMessage(ordered(t, "name", name, "symbol", symbol, "exchange", "TSX", "country", "CA",
		"sector", "Materials", "industry", "Metals and Mining", "pe", pe, "market_cap_usd_m", 812.5))
}

func TestScreenHappyPathRequestAndTable(t *testing.T) {
	rows := []json.RawMessage{screenRow(t, "Cameco Corporation", "CCO:CA", 12.4), screenRow(t, "Lundin Mining", "LUN:CA", nil)}
	srv := newStub(t, 200, nil, screenBody(t, rows, "nx1", "Showing the first 10 rows."))
	stdout, stderr, code := runVerb(t, srv.URL, "screen", "--industry", " mining ", "--max", "pe=15", "--min", "market_cap_usd_m=500", "--format", "table")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("requests %d", n)
	}
	if u := srv.last(); u != "/research/screen?industry=mining&market_cap_usd_m_min=500&pe_max=15" {
		t.Errorf("request %q", u)
	}
	for _, want := range []string{
		"Matched 12 companies (as of 2026-10-20)\n",
		"NAME                SYMBOL  EXCHANGE  COUNTRY  SECTOR     INDUSTRY           PE    MARKET_CAP_USD_M\n",
		"Cameco Corporation  CCO:CA  TSX       CA       Materials  Metals and Mining  12.4  812.5\n",
		"Lundin Mining       LUN:CA  TSX       CA       Materials  Metals and Mining  -     812.5\n",
		"\nExcluded:\n  pe: 4 (no P/E: loss making or no earnings data)\n  inactive: 2 (inactive or delisted)\n",
		`Interpretation: {"industry":[{"matched_term":"mining","slug":"metals_mining"}]}`,
		"\nDisclosures:\n  BIST bank figures are solo statements.\n",
		"\nCoverage: TSX, TSX Venture",
		"\nNote: Showing the first 10 rows.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "More results: rerun with --cursor nx1") {
		t.Errorf("stderr %q", stderr)
	}

	// No note, no cursor: neither line appears.
	plain := newStub(t, 200, nil, screenBody(t, rows, nil, nil))
	stdout, stderr, _ = runVerb(t, plain.URL, "screen", "--format", "table")
	if strings.Contains(stdout, "Note:") || strings.Contains(stderr, "More results") {
		t.Errorf("unexpected note or hint:\n%s\n%s", stdout, stderr)
	}
	if u := plain.last(); u != "/research/screen" {
		t.Errorf("a screen with no flags sends no parameters: %q", u)
	}
}

func TestScreenListValuesAndScopeFlags(t *testing.T) {
	srv := newStub(t, 200, nil, screenBody(t, nil, nil, nil))
	_, stderr, code := runVerb(t, srv.URL, "screen", "--sector", " materials,energy ", "--exchange", "TSX,TSXV",
		"--market", "CA", "--country", "CA,TR", "--currency", "cad", "--min", "fy_revenue_m=10", "--as-of", "2026-09-30",
		"--sort-by", "pe", "--sort-dir", "asc", "--fields", "return_1y_pct,eps", "--limit", "50", "--cursor", "srv1", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := "/research/screen?as_of=2026-09-30&country=CA%2CTR&currency=cad&cursor=srv1&exchange=TSX%2CTSXV&fields=return_1y_pct%2Ceps" +
		"&fy_revenue_m_min=10&limit=50&market=CA&sector=materials%2Cenergy&sort_by=pe&sort_dir=asc"
	if u := srv.last(); u != want {
		t.Errorf("request\ngot  %q\nwant %q", u, want)
	}
	if strings.Contains(stderr, "No companies matched") {
		t.Errorf("stderr %q", stderr)
	}
	// Nothing matched is an answer (exit 0), said on stderr.
	none := newStub(t, 200, nil, `{"as_of_used":"2026-10-20","total_matched":0,"rows":[],"excluded":{},"interpretation":{},"coverage_note":"c","disclosures":[],"next_cursor":null}`)
	stdout, stderr, code := runVerb(t, none.URL, "screen", "--industry", "gold", "--format", "table")
	if code != 0 || !strings.Contains(stderr, "No companies matched") || stdout != "Matched 0 companies (as of 2026-10-20)\n\nCoverage: c\n" {
		t.Errorf("none: exit %d %q %q", code, stdout, stderr)
	}
}

func TestScreenIncludeInactiveSentOnlyWhenSet(t *testing.T) {
	srv := newStub(t, 200, nil, screenBody(t, nil, nil, nil))
	for argv, want := range map[string]string{
		"--include-inactive":       "/research/screen?include_inactive=true",
		"--include-inactive=false": "/research/screen?include_inactive=false",
		"":                         "/research/screen",
	} {
		args := []string{"screen", "--format", "json"}
		if argv != "" {
			args = append(args, argv)
		}
		if _, stderr, code := runVerb(t, srv.URL, args...); code != 0 {
			t.Fatalf("%s: exit %d %s", argv, code, stderr)
		}
		if u := srv.last(); u != want {
			t.Errorf("%q: request %q, want %q", argv, u, want)
		}
	}
}

func TestScreenLocalRefusalsSendNothing(t *testing.T) {
	cases := [][]string{
		{"screen", "--max", "pe"},
		{"screen", "--min", "=5"},
		{"screen", "--min", "PE=5"},
		{"screen", "--min", "1pe=5"},
		{"screen", "--min", "p-e=5"},
		{"screen", "--min", strings.Repeat("k", 65) + "=5"},
		{"screen", "--max", "pe=15", "--max", "pe=20"},
		{"screen", "--min", "pe="},
		{"screen", "--min", "pe=  "},
		{"screen", "--min", "pe=" + strings.Repeat("9", 33)},
		{"screen", "--limit", "0"},
		{"screen", "--limit", "51"},
		{"screen", "--as-of", "2026-13-01"},
		{"screen", "--sort-dir", "up"},
		{"screen", "--format", "csv"},
	}
	srv := newStub(t, 200, nil, screenBody(t, nil, nil, nil))
	for _, argv := range cases {
		_, stderr, code := runVerb(t, srv.URL, argv...)
		if code != ExitUsageError {
			t.Errorf("%v: exit %d, want 2 (stderr %q)", short(argv), code, stderr)
		}
	}
	if n := srv.calls.Load(); n != 0 {
		t.Errorf("local refusals sent %d requests", n)
	}
	// The same key on both sides is a range, not a repeat; 32 characters pass.
	ok := newStub(t, 200, nil, screenBody(t, nil, nil, nil))
	if _, stderr, code := runVerb(t, ok.URL, "screen", "--min", "pe=5", "--max", "pe="+strings.Repeat("9", 32), "--format", "json"); code != 0 {
		t.Fatalf("range: exit %d %s", code, stderr)
	}
	if u := ok.last(); u != "/research/screen?pe_max="+strings.Repeat("9", 32)+"&pe_min=5" {
		t.Errorf("range request %q", u)
	}
}

func TestScreenServerRefusalsMapToExits(t *testing.T) {
	cases := []struct {
		status int
		body   string
		exit   int
		want   string
	}{
		{400, `{"error":"No Mosaic industry matches 'zzz'.","code":"SCREEN_REFUSED"}`, ExitUsageError, "[SCREEN_REFUSED]"},
		{400, `{"error":"Unknown screen parameter 'zz_min'.","code":"BAD_REQUEST"}`, ExitUsageError, "[BAD_REQUEST]"},
		{403, `{"error":"The free company screens on this account are used.","code":"PLAN_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`, ExitAuthError, "Account: https://mosaic-finance.com/en/pricing/"},
		{429, `{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA"}`, ExitRateLimit, "[CLI_QUOTA]"},
		{503, `{"error":"The company screen is not ready yet.","code":"SCREEN_NOT_READY"}`, ExitServerError, "SCREEN_NOT_READY"},
	}
	for _, tc := range cases {
		srv := newStub(t, tc.status, nil, tc.body)
		_, stderr, code := runVerb(t, srv.URL, "screen", "--industry", "zzz", "--format", "table")
		if code != tc.exit || !strings.Contains(stderr, tc.want) {
			t.Errorf("%d: exit %d (want %d), stderr %q", tc.status, code, tc.exit, stderr)
		}
	}
}

func TestScreenCompactFitsRows(t *testing.T) {
	var rows []json.RawMessage
	for i := 0; i < 50; i++ {
		rows = append(rows, json.RawMessage(ordered(t, "name", fmt.Sprintf("Company %d %s", i, strings.Repeat("n", 600)),
			"symbol", fmt.Sprintf("C%d:CA", i), "exchange", "TSX", "pe", nil, "market_cap_usd_m", i)))
	}
	srv := newStub(t, 200, nil, screenBody(t, rows, nil, nil))
	stdout, stderr, code := runVerb(t, srv.URL, "screen", "--format", "compact")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	m := decodeCompact(t, stdout)
	got := m["rows"].([]any)
	if len(got) == 0 || len(got) >= 50 || m["truncated"] != true || printedSize(stdout) > compactFitBytes {
		t.Fatalf("fit: %d rows, truncated %v, %d bytes", len(got), m["truncated"], printedSize(stdout))
	}
	if _, ok := got[0].(map[string]any)["pe"]; ok {
		t.Error("compact drops null members")
	}
	cur, _ := m["next_cursor"].(string)
	if !strings.HasPrefix(cur, compactCursorPrefix) || !strings.Contains(stderr, "--cursor "+cur) {
		t.Fatalf("cursor %q stderr %q", cur, stderr)
	}
	// The cursor continues the same request.
	stdout, _, code = runVerb(t, srv.URL, "screen", "--format", "compact", "--cursor", cur)
	if next := decodeCompact(t, stdout)["rows"].([]any); code != 0 || next[0].(map[string]any)["symbol"] != fmt.Sprintf("C%d:CA", len(got)) {
		t.Fatalf("second page: exit %d first row %v", code, next[0])
	}
}

func TestScreenDryRunPrintsTheRequest(t *testing.T) {
	srv := newStub(t, 200, nil, "{}")
	stdout, _, code := runVerb(t, srv.URL, "screen", "--industry", "gold", "--max", "pe=15", "--dry-run")
	if code != 0 || srv.calls.Load() != 0 {
		t.Fatalf("exit %d, %d requests", code, srv.calls.Load())
	}
	if want := "[dry-run] GET " + srv.URL + "/research/screen?industry=gold&pe_max=15\n"; stdout != want {
		t.Errorf("dry run %q, want %q", stdout, want)
	}
}

// The help an agent reads: no hyphen or dash in the long text, and the
// reporting duties of the hosted screen tool.
func TestScreenHelpText(t *testing.T) {
	c := newScreenCmd("dev")
	if strings.ContainsAny(c.Short+c.Long, "-\u2013\u2014") {
		t.Fatalf("help carries a hyphen or dash:\n%s", c.Long)
	}
	for _, want := range []string{"total_matched", "excluded", "interpretation", "as_of_used", "coverage_note",
		"disclosures", "note", "data, not citations", "_usd_m", "_pct", "currency"} {
		if !strings.Contains(c.Long, want) {
			t.Errorf("long help lacks %q", want)
		}
	}
	if c.Annotations["pp:typed-exit-codes"] != "0,2,4,5,7" || c.Annotations["mcp:read-only"] != "true" || c.Annotations["mcp:title"] != "Screen companies" {
		t.Errorf("annotations %v", c.Annotations)
	}
}
