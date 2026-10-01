package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testChunkID  = "11111111-2222-4333-8444-555555555555"
	testFilingID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// stubServer answers every request with status, headers and body, and
// counts requests.
type stubServer struct {
	*httptest.Server
	calls   atomic.Int32
	lastURL atomic.Value
}

func newStub(t *testing.T, status int, headers map[string]string, body string) *stubServer {
	t.Helper()
	s := &stubServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastURL.Store(r.URL.String())
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubServer) last() string {
	v, _ := s.lastURL.Load().(string)
	return v
}

// runVerb executes argv against baseURL with a test credential and returns
// stdout, stderr and the exit code (0 on success, -1 on a non-typed error).
func runVerb(t *testing.T, baseURL string, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	t.Setenv("ARCHIVIST_BASE_URL", baseURL)
	root := NewRootCmd("0.2.22", "abc1234", "2026-10-01")
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(argv)
	err := root.Execute()
	code = 0
	if err != nil {
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.Code
		} else {
			code = -1
			errBuf.WriteString(err.Error())
		}
	}
	return out.String(), errBuf.String(), code
}

func passageJSON(id string, idx int, url any) map[string]any {
	return map[string]any{
		"id":              id,
		"filing_id":       testFilingID,
		"company_name":    "Apple Inc.",
		"symbol":          "AAPL:US",
		"exchange":        "NGS",
		"formtype":        "10-K",
		"formdescription": "Annual Report",
		"datefiled":       "2025-11-01",
		"section_header":  "Item 1A. Risk Factors and other very long section headers",
		"chunk_index":     idx,
		"snippet":         "The Company's business\n\tcan be affected by many   factors, including supply chain disruption and more text past sixty runes.",
		"url":             url,
		"source_url":      nil,
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const testPermalink = "https://mosaic-finance.com/filings/" + testFilingID + "/?c=" + testChunkID + "&t=k1.mac"

func searchBody(t *testing.T, results []map[string]any, truncated bool, cursor any, er any) string {
	t.Helper()
	if results == nil {
		results = []map[string]any{}
	}
	return mustJSON(t, map[string]any{
		"results":           results,
		"entity_resolution": er,
		"truncated":         truncated,
		"next_cursor":       cursor,
	})
}

// ─── search: happy path, table and JSON ──────────────────────────────────────

func TestSearchTableOutput(t *testing.T) {
	body := searchBody(t, []map[string]any{passageJSON(testChunkID, 3, testPermalink)}, false, nil, nil)
	srv := newStub(t, 200, nil, body)

	stdout, stderr, code := runVerb(t, srv.URL, "search", "risk factors", "--symbol", "AAPL", "--format", "table")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	header := strings.Fields(strings.SplitN(stdout, "\n", 2)[0])
	want := []string{"CHUNK_ID", "FILING_ID", "SYMBOL", "FORM", "DATE", "SECTION", "SNIPPET", "URL"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Errorf("header: got %v, want %v", header, want)
	}
	for _, s := range []string{testChunkID, testFilingID, "AAPL:US", "10-K", "2025-11-01", testPermalink} {
		if !strings.Contains(stdout, s) {
			t.Errorf("table missing %q:\n%s", s, stdout)
		}
	}
	// SECTION clipped to 32 runes, SNIPPET collapsed and clipped to 60.
	if !strings.Contains(stdout, "Item 1A. Risk Factors and other…") {
		t.Errorf("section not clipped to 32 runes:\n%s", stdout)
	}
	if !strings.Contains(stdout, "The Company's business can be affected by many factors, inc…") {
		t.Errorf("snippet not collapsed and clipped to 60 runes:\n%s", stdout)
	}
	u := srv.last()
	for _, s := range []string{"/research/search?", "q=risk+factors", "symbol=AAPL", "mode=semantic", "limit=10"} {
		if !strings.Contains(u, s) {
			t.Errorf("request %q missing %q", u, s)
		}
	}
}

func TestSearchJSONOffTTYIsServerBodyReindented(t *testing.T) {
	body := searchBody(t, []map[string]any{passageJSON(testChunkID, 3, testPermalink)}, false, nil, map[string]any{"state": "resolved_canonical"})
	srv := newStub(t, 200, nil, body)

	stdout, _, code := runVerb(t, srv.URL, "search", "risk factors")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var want bytes.Buffer
	if err := json.Indent(&want, []byte(body), "", "  "); err != nil {
		t.Fatal(err)
	}
	if stdout != want.String()+"\n" {
		t.Errorf("JSON output is not the indented server body\ngot:\n%s\nwant:\n%s", stdout, want.String())
	}
}

func TestSearchTruncatedPrintsCursorHint(t *testing.T) {
	body := searchBody(t, []map[string]any{passageJSON(testChunkID, 1, testPermalink)}, true, "abc", nil)
	srv := newStub(t, 200, nil, body)

	stdout, stderr, code := runVerb(t, srv.URL, "search", "x")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "More results: rerun with --cursor abc") {
		t.Errorf("stderr missing hint:\n%s", stderr)
	}
	if strings.Contains(stdout, "More results") {
		t.Errorf("the hint is a diagnostic and belongs on stderr")
	}
}

func TestSearchNullPermalinkRendersDash(t *testing.T) {
	rec := passageJSON(testChunkID, 1, nil)
	rec["symbol"] = nil
	body := searchBody(t, []map[string]any{rec}, false, nil, nil)
	srv := newStub(t, 200, nil, body)

	stdout, _, code := runVerb(t, srv.URL, "search", "x", "--format", "table")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	row := strings.Fields(strings.Split(stdout, "\n")[1])
	if row[len(row)-1] != "-" {
		t.Errorf("null url must render as '-': %q", strings.Split(stdout, "\n")[1])
	}
	if row[2] != "-" {
		t.Errorf("null symbol must render as '-': %q", strings.Split(stdout, "\n")[1])
	}
}

func TestSearchNoResults(t *testing.T) {
	er := map[string]any{"state": "not_in_corpus", "suggestion": "We don't have filings for 'Zzz'."}
	body := searchBody(t, nil, false, nil, er)
	srv := newStub(t, 200, nil, body)

	stdout, stderr, code := runVerb(t, srv.URL, "search", "x", "--format", "json")
	if code != ExitNotFound {
		t.Errorf("exit: got %d, want %d", code, ExitNotFound)
	}
	if !strings.Contains(stderr, "No passages found") || !strings.Contains(stderr, "We don't have filings for 'Zzz'.") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if !json.Valid([]byte(stdout)) || !strings.Contains(stdout, `"not_in_corpus"`) {
		t.Errorf("JSON mode must still print the body:\n%s", stdout)
	}
}

func TestSearchAmbiguousResolutionExit6(t *testing.T) {
	er := map[string]any{"state": "resolved_ambiguous", "suggestion": "Did you mean: A, B? Please specify."}
	srv := newStub(t, 200, nil, searchBody(t, nil, false, nil, er))

	_, stderr, code := runVerb(t, srv.URL, "search", "x", "--format", "table")
	if code != ExitAmbiguousMatch {
		t.Errorf("exit: got %d, want %d", code, ExitAmbiguousMatch)
	}
	if !strings.Contains(stderr, "Did you mean: A, B?") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestSearchLastAllowedCallSucceeds(t *testing.T) {
	body := searchBody(t, []map[string]any{passageJSON(testChunkID, 1, testPermalink)}, false, nil, nil)
	srv := newStub(t, 200, map[string]string{"X-Queries-Remaining": "0"}, body)

	stdout, _, code := runVerb(t, srv.URL, "search", "x")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, testChunkID) {
		t.Errorf("success output missing:\n%s", stdout)
	}
}

func TestSearchStdinQueryAndCursor(t *testing.T) {
	srv := newStub(t, 200, nil, searchBody(t, []map[string]any{passageJSON(testChunkID, 1, testPermalink)}, false, nil, nil))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	root := NewRootCmd("0.2.22", "x", "y")
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("gross margin\n"))
	root.SetArgs([]string{"search", "--stdin", "--cursor", "c1", "--limit", "25", "--mode", "broad"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	u := srv.last()
	for _, s := range []string{"q=gross+margin", "cursor=c1", "limit=25", "mode=broad"} {
		if !strings.Contains(u, s) {
			t.Errorf("request %q missing %q", u, s)
		}
	}
}

func TestSearchDryRunSendsNothing(t *testing.T) {
	srv := newStub(t, 200, nil, "{}")
	stdout, _, code := runVerb(t, srv.URL, "search", "a b", "--formtype", "10-K", "--date-from", "2024-01-01", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if srv.calls.Load() != 0 {
		t.Errorf("dry run sent %d requests", srv.calls.Load())
	}
	want := "[dry-run] GET " + srv.URL + "/research/search?date_from=2024-01-01&formtype=10-K&limit=10&mode=semantic&q=a+b\n"
	if stdout != want {
		t.Errorf("dry run:\ngot  %q\nwant %q", stdout, want)
	}
}

// ─── client-side validation: exit 2 before any request ───────────────────────

func TestResearchValidationExit2WithoutRequest(t *testing.T) {
	cases := [][]string{
		{"search"},
		{"search", "   "},
		{"search", strings.Repeat("q", 2001)},
		{"search", "x", "--limit", "0"},
		{"search", "x", "--limit", "26"},
		{"search", "x", "--mode", "fuzzy"},
		{"search", "x", "--mode", "broad", "--symbol", "AAPL"},
		{"search", "x", "--symbol", strings.Repeat("S", 21)},
		{"search", "x", "--formtype", strings.Repeat("F", 51)},
		{"search", "x", "--date-from", "2024-13-01"},
		{"search", "x", "--date-to", "2024-02-30"},
		{"search", "x", "--date-from", "2025-01-02", "--date-to", "2025-01-01"},
		{"search", "x", "--format", "csv"},
		{"read", "passage", "not-a-uuid"},
		{"read", "passage", testChunkID, "--window", "3"},
		{"read", "passage", testChunkID, "--window", "-1"},
		{"read", "section", "nope", "Risk Factors"},
		{"read", "section", testFilingID, "  "},
		{"read", "section", testFilingID, strings.Repeat("h", 256)},
		{"toc", "123"},
	}
	srv := newStub(t, 200, nil, "{}")
	for _, argv := range cases {
		_, stderr, code := runVerb(t, srv.URL, argv...)
		if code != ExitUsageError {
			t.Errorf("%v: exit %d, want 2 (stderr %q)", short(argv), code, stderr)
		}
	}
	if n := srv.calls.Load(); n != 0 {
		t.Errorf("validation failures sent %d requests", n)
	}
}

// TestResearchLengthChecksCountUTF16: the server's bounds are JavaScript
// string lengths, so a non-BMP character (two UTF-16 code units) counts twice.
func TestResearchLengthChecksCountUTF16(t *testing.T) {
	srv := newStub(t, 200, nil, searchBody(t, []map[string]any{passageJSON(testChunkID, 1, testPermalink)}, false, nil, nil))
	// 1001 emoji: 1001 runes, 2002 UTF-16 code units, over the 2000 bound.
	over := strings.Repeat("\U0001F4C8", 1001)
	if _, stderr, code := runVerb(t, srv.URL, "search", over); code != ExitUsageError {
		t.Errorf("1001 emoji query: exit %d, want 2 (stderr %q)", code, stderr)
	}
	// 11 emoji symbol: 22 code units, over the 20 bound.
	if _, _, code := runVerb(t, srv.URL, "search", "x", "--symbol", strings.Repeat("\U0001F4C8", 11)); code != ExitUsageError {
		t.Errorf("11 emoji symbol: exit %d, want 2", code)
	}
	if n := srv.calls.Load(); n != 0 {
		t.Errorf("over-length input sent %d requests", n)
	}
	// 1000 emoji is exactly 2000 code units and passes.
	if _, stderr, code := runVerb(t, srv.URL, "search", strings.Repeat("\U0001F4C8", 1000)); code != 0 {
		t.Errorf("1000 emoji query: exit %d, want 0 (stderr %q)", code, stderr)
	}
}

func short(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if len(a) > 24 {
			a = a[:24] + "..."
		}
		out[i] = a
	}
	return out
}

// ─── read passage / read section / toc ───────────────────────────────────────

func TestReadPassageTableThenTextsInChunkOrder(t *testing.T) {
	const prev = "11111111-2222-4333-8444-000000000001"
	const next = "11111111-2222-4333-8444-000000000003"
	head := passageJSON(testChunkID, 2, testPermalink)
	head["snippet"] = "HEAD TEXT"
	p1 := passageJSON(prev, 1, nil)
	p1["snippet"] = "PREV TEXT"
	p3 := passageJSON(next, 3, nil)
	p3["snippet"] = "NEXT TEXT"
	body := mustJSON(t, map[string]any{
		"passage":     head,
		"neighbours":  []any{p3, p1},
		"truncated":   false,
		"next_cursor": nil,
	})
	srv := newStub(t, 200, nil, body)

	stdout, stderr, code := runVerb(t, srv.URL, "read", "passage", testChunkID, "--window", "2", "--format", "table")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, "CHUNK_ID") {
		t.Errorf("expected the table first:\n%s", stdout)
	}
	i1 := strings.Index(stdout, "--- [1] "+prev+" ---\nPREV TEXT")
	i2 := strings.Index(stdout, "--- [2] "+testChunkID+" ---\nHEAD TEXT")
	i3 := strings.Index(stdout, "--- [3] "+next+" ---\nNEXT TEXT")
	if i1 < 0 || i2 < 0 || i3 < 0 || i1 >= i2 || i2 >= i3 {
		t.Errorf("passages not in chunk_index order (%d, %d, %d):\n%s", i1, i2, i3, stdout)
	}
	if u := srv.last(); u != "/research/passages/"+testChunkID+"?window=2" {
		t.Errorf("request: %q", u)
	}
}

func TestReadPassageJSON(t *testing.T) {
	body := mustJSON(t, map[string]any{"passage": passageJSON(testChunkID, 2, testPermalink), "neighbours": []any{}, "truncated": false, "next_cursor": nil})
	srv := newStub(t, 200, nil, body)
	stdout, _, code := runVerb(t, srv.URL, "read", "passage", testChunkID)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got, want any
	_ = json.Unmarshal([]byte(stdout), &got)
	_ = json.Unmarshal([]byte(body), &want)
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Errorf("JSON changed:\n%s", stdout)
	}
	if !strings.Contains(srv.last(), "window=1") {
		t.Errorf("default window must be 1: %q", srv.last())
	}
}

func TestReadSectionEscapesAndRenders(t *testing.T) {
	p0 := passageJSON(testChunkID, 7, testPermalink)
	p0["snippet"] = "SECTION TEXT"
	body := mustJSON(t, map[string]any{
		"filing_id": testFilingID, "section_header": "Risk & Factors", "chunk_count": 1,
		"passages": []any{p0}, "truncated": true, "next_cursor": "nx",
	})
	srv := newStub(t, 200, nil, body)

	stdout, stderr, code := runVerb(t, srv.URL, "read", "section", testFilingID, "Risk & Factors", "--format", "table", "--cursor", "c0")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if u := srv.last(); u != "/research/filings/"+testFilingID+"/sections?cursor=c0&section_header=Risk+%26+Factors" {
		t.Errorf("request: %q", u)
	}
	if !strings.Contains(stdout, "--- [7] "+testChunkID+" ---\nSECTION TEXT") {
		t.Errorf("missing passage text:\n%s", stdout)
	}
	if !strings.Contains(stderr, "More results: rerun with --cursor nx") {
		t.Errorf("missing hint:\n%s", stderr)
	}
}

func TestTocNumberedHeaders(t *testing.T) {
	body := mustJSON(t, map[string]any{"filing_id": testFilingID, "sections": []string{"Item 1. Business", "Item 1A. Risk Factors"}, "truncated": false, "next_cursor": nil})
	srv := newStub(t, 200, nil, body)

	stdout, _, code := runVerb(t, srv.URL, "toc", testFilingID, "--format", "table")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != "  1  Item 1. Business\n  2  Item 1A. Risk Factors\n" {
		t.Errorf("toc output: %q", stdout)
	}
	if u := srv.last(); u != "/research/filings/"+testFilingID+"/toc" {
		t.Errorf("request: %q", u)
	}
}
