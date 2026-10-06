package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Story 81.4: filings, find, search --latest-only and --format compact.

// pagedStub serves /research/search pages keyed by the cursor parameter and
// records every request's raw query.
type pagedStub struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func newPagedStub(t *testing.T, pages map[string]string) *pagedStub {
	t.Helper()
	s := &pagedStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.RawQuery)
		s.mu.Unlock()
		body, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"The cursor does not belong to this request.","code":"BAD_CURSOR"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *pagedStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

// ordered builds a JSON object with members in the order given (a Go map
// would sort them), the way chat-api serializes a response.
func ordered(t *testing.T, kv ...any) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(mustJSON(t, kv[i].(string)))
		b.WriteByte(':')
		if raw, ok := kv[i+1].(json.RawMessage); ok {
			b.Write(raw)
		} else {
			b.WriteString(mustJSON(t, kv[i+1]))
		}
	}
	b.WriteByte('}')
	return b.String()
}

// passageOrdered is a PassageRecord in chat-api's member order.
func passageOrdered(t *testing.T, id, symbol, formtype, formdescription, snippet string, url any, extra ...any) json.RawMessage {
	t.Helper()
	var sym any = symbol
	if symbol == "" {
		sym = nil
	}
	kv := []any{"id", id, "filing_id", testFilingID, "company_name", "Apple Inc.", "symbol", sym, "exchange", "NGS",
		"formtype", formtype, "formdescription", formdescription, "datefiled", "2025-11-01", "section_header", "Risk Factors",
		"chunk_index", 3, "snippet", snippet, "url", url}
	return json.RawMessage(ordered(t, append(kv, extra...)...))
}

func rawList(items ...json.RawMessage) json.RawMessage {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = string(it)
	}
	return json.RawMessage("[" + strings.Join(parts, ",") + "]")
}

func bigPassage(i, snippetChars int) map[string]any {
	p := passageJSON(fmt.Sprintf("11111111-2222-4333-8444-%012d", i), i, testPermalink)
	p["snippet"] = fmt.Sprintf("passage %d ", i) + strings.Repeat("x", snippetChars)
	p["symbol"] = "AAPL"
	p["exchange_document_id"] = "0000320193-25-000079"
	p["exchange_document_kind"] = "sec_accession_number"
	return p
}

func decodeCompact(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(stdout, "\n") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("compact output is not one line: %q", stdout)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("compact output is not JSON: %v\n%s", err, stdout)
	}
	return m
}

func resultIDs(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	rows, _ := m[key].([]any)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	return ids
}

func cursorPayload(t *testing.T, c string) cliCursorJSON {
	t.Helper()
	if !strings.HasPrefix(c, "c1.") {
		t.Fatalf("not a CLI cursor: %q", c)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(c, "c1."))
	if err != nil {
		t.Fatal(err)
	}
	var p cliCursorJSON
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ─── projection ──────────────────────────────────────────────────────────────

func TestCompactProjection(t *testing.T) {
	const id2, id3 = "11111111-2222-4333-8444-000000000002", "11111111-2222-4333-8444-000000000003"
	sec := passageOrdered(t, testChunkID, "AAPL", "10-K", "Annual report", "Revenue & margin <rose>.", testPermalink,
		"exchange_document_id", "0000320193-25-000079", "exchange_document_kind", "sec_accession_number")
	sedar := passageOrdered(t, id2, "ABX:CA", "002-002-001119-001014", "Technical report (NI 43-101)", "Water.", nil)
	kap := passageOrdered(t, id3, "AKBNK:TR", "FR:8aca490d502dd03b01502deede79010a", "Finansal Rapor", "Kur riski.", testPermalink)
	er := ordered(t, "input", "AAPL", "symbol", "AAPL", "issuer_key", "cik:320193", "company_name", "Apple Inc.", "confidence", 1,
		"alternatives", []any{}, "state", "resolved_canonical", "warning", nil, "suggestion", nil, "candidates", []any{})
	latest := ordered(t, "applied", true, "filing_id", testFilingID, "datefiled", "2025-10-31", "form", "10-K")
	body := ordered(t, "results", rawList(sec, sedar, kap), "entity_resolution", json.RawMessage(er),
		"latest_filing", json.RawMessage(latest), "truncated", false, "next_cursor", nil)
	srv := newStub(t, 200, nil, body)
	stdout, stderr, code := runVerb(t, srv.URL, "search", "margin", "--symbol", "AAPL", "--format", "compact")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// Server order kept; nulls, truncated:false, chunk_index, exchange, the
	// 10-K formdescription and the plain canonical resolution dropped; no
	// HTML escaping; SEDAR and KAP keep formtype and formdescription.
	want := `{"results":[` +
		`{"id":"` + testChunkID + `","filing_id":"` + testFilingID + `","company_name":"Apple Inc.","symbol":"AAPL",` +
		`"formtype":"10-K","datefiled":"2025-11-01","section_header":"Risk Factors","snippet":"Revenue & margin <rose>.",` +
		`"url":"` + testPermalink + `","exchange_document_id":"0000320193-25-000079","exchange_document_kind":"sec_accession_number"},` +
		`{"id":"` + id2 + `","filing_id":"` + testFilingID + `","company_name":"Apple Inc.","symbol":"ABX:CA",` +
		`"formtype":"002-002-001119-001014","formdescription":"Technical report (NI 43-101)","datefiled":"2025-11-01",` +
		`"section_header":"Risk Factors","snippet":"Water."},` +
		`{"id":"` + id3 + `","filing_id":"` + testFilingID + `","company_name":"Apple Inc.","symbol":"AKBNK:TR",` +
		`"formtype":"FR:8aca490d502dd03b01502deede79010a","formdescription":"Finansal Rapor","datefiled":"2025-11-01",` +
		`"section_header":"Risk Factors","snippet":"Kur riski.","url":"` + testPermalink + `"}],` +
		`"latest_filing":{"applied":true,"filing_id":"` + testFilingID + `","datefiled":"2025-10-31","form":"10-K"}}` + "\n"
	if stdout != want {
		t.Fatalf("compact output\ngot  %s\nwant %s", stdout, want)
	}
	if strings.Contains(stderr, "More results") {
		t.Errorf("no next cursor, no hint: %q", stderr)
	}
	// The json format is unchanged: the server body re-indented.
	stdout, _, _ = runVerb(t, srv.URL, "search", "margin", "--symbol", "AAPL", "--format", "json")
	var buf strings.Builder
	_ = writeIndentedJSON(&buf, []byte(body))
	if stdout != buf.String() {
		t.Errorf("json output changed:\n%s", stdout)
	}
}

// Only a US listing's short form code drops formdescription: KAP rows carry
// bare short formtypes (FR, ODA, DG) whose formdescription is the only form
// name, and a row without a symbol keeps both.
func TestCompactFormDescriptionOnlyDroppedForUSListings(t *testing.T) {
	cases := []struct {
		symbol, formtype, formdescription string
		keep                              bool
	}{
		{"AKBNK:TR", "FR", "Faaliyet Raporu (Konsolide)", true},
		{"AKBNK:TR", "ODA", "Genel Kurul İşlemlerine İlişkin Bildirim", true},
		{"AKBNK:TR", "DG", "Finansal Takvim", true},
		{"ABX:CA", "6-K", "Report of foreign private issuer", true},
		{"B", "40-F", "Annual report", false},
		{"AAPL", "10-K", "Annual report", false},
		{"", "10-K", "Annual report", true},
	}
	for _, c := range cases {
		body := ordered(t, "results", rawList(passageOrdered(t, testChunkID, c.symbol, c.formtype, c.formdescription, "s", nil)),
			"truncated", false, "next_cursor", nil)
		text, _, err := compactPage([]byte(body), "results", pageCursor{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(text, `"formdescription":`+mustJSON(t, c.formdescription)); got != c.keep {
			t.Errorf("symbol %q formtype %q: formdescription kept %v, want %v\n%s", c.symbol, c.formtype, got, c.keep, text)
		}
		if !strings.Contains(text, `"formtype":`+mustJSON(t, c.formtype)) {
			t.Errorf("formtype dropped: %s", text)
		}
	}
}

func TestCompactEntityResolutionKeptWhenActionable(t *testing.T) {
	er := `{"input":"ABX","symbol":null,"issuer_key":null,"company_name":null,"confidence":0.4,"resolved":false,` +
		`"alternatives":[],"state":"resolved_ambiguous","source":"x","warning":"Several issuers match ABX.",` +
		`"suggestion":"Retry with ABX:CA.","candidates":[{"symbol":"ABX:CA","company_name":"Barrick","issuer_key":null}]}`
	srv := newStub(t, 200, nil, `{"results":[],"entity_resolution":`+er+`,"truncated":false,"next_cursor":null}`)
	stdout, stderr, code := runVerb(t, srv.URL, "search", "x", "--symbol", "ABX", "--format", "compact")
	if code != ExitAmbiguousMatch {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := `{"results":[],"entity_resolution":{"state":"resolved_ambiguous","warning":"Several issuers match ABX.",` +
		`"suggestion":"Retry with ABX:CA.","candidates":[{"symbol":"ABX:CA","company_name":"Barrick"}]}}` + "\n"
	if stdout != want {
		t.Fatalf("got  %s\nwant %s", stdout, want)
	}
	if !strings.Contains(stderr, "No passages found") || !strings.Contains(stderr, "Retry with ABX:CA.") {
		t.Errorf("stderr %q", stderr)
	}

	// A canonical resolution with a warning is kept too.
	er2 := `{"state":"resolved_canonical","symbol":"AAPL","warning":"Mapped AAPL:US to AAPL.","suggestion":null,"candidates":[]}`
	srv2 := newStub(t, 200, nil, `{"results":[{"id":"c1","filing_id":"f","snippet":"s"}],"entity_resolution":`+er2+`,"truncated":false,"next_cursor":null}`)
	stdout, _, _ = runVerb(t, srv2.URL, "search", "x", "--format", "compact")
	if !strings.Contains(stdout, `"entity_resolution":{"state":"resolved_canonical","symbol":"AAPL","warning":"Mapped AAPL:US to AAPL."}`) {
		t.Errorf("canonical with a warning: %s", stdout)
	}
}

func TestPrintedSize(t *testing.T) {
	cases := map[string]int{
		"":            2,
		"abc":         5,
		`a"b`:         6,
		`a\b`:         6,
		"a\nb":        6,
		"\x01":        8,
		"ş":           4, // two UTF-8 bytes
		"{\"k\":1}\n": 2 + 1 + 2 + 1 + 2 + 3 + 2,
	}
	for in, want := range cases {
		if got := printedSize(in); got != want {
			t.Errorf("printedSize(%q) = %d, want %d", in, got, want)
		}
	}
	// It equals the length of the text JSON encoded without HTML escaping.
	text := `{"snippet":"Revenue & <margin> \"quoted\" \\ path` + "\n\t" + `ş"}` + "\n"
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(text)
	if got, want := printedSize(text), len(strings.TrimRight(b.String(), "\n")); got != want {
		t.Errorf("printedSize %d, encoded %d", got, want)
	}
}

// ─── fit and the c1 cursor ───────────────────────────────────────────────────

func TestCompactFitPagesWithoutOverlapOrGap(t *testing.T) {
	var rows []map[string]any
	for i := 1; i <= 10; i++ {
		rows = append(rows, bigPassage(i, 4500))
	}
	page1 := searchBody(t, rows, false, nil, nil)
	srv := newPagedStub(t, map[string]string{"": page1})
	argv := []string{"search", "supply chain", "--symbol", "AAPL", "--limit", "10", "--format", "compact"}

	var seen []string
	cursor := ""
	for call := 0; call < 10; call++ {
		args := append([]string{}, argv...)
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		stdout, stderr, code := runVerb(t, srv.URL, args...)
		if code != 0 {
			t.Fatalf("call %d exit %d: %s", call, code, stderr)
		}
		if n := printedSize(stdout); n > compactFitBytes {
			t.Fatalf("call %d printed %d bytes", call, n)
		}
		m := decodeCompact(t, stdout)
		seen = append(seen, resultIDs(t, m, "results")...)
		next, _ := m["next_cursor"].(string)
		if next == "" {
			if _, ok := m["truncated"]; ok {
				t.Fatalf("last page carries truncated: %v", m["truncated"])
			}
			break
		}
		if m["truncated"] != true {
			t.Fatalf("a cut page must say truncated:true: %v", m["truncated"])
		}
		if !strings.Contains(stderr, "More results: rerun with --cursor "+next) {
			t.Errorf("hint missing: %q", stderr)
		}
		p := cursorPayload(t, next)
		if p.S != "" || p.K != len(seen) {
			t.Fatalf("cursor %+v after %d rows", p, len(seen))
		}
		if call == 0 && (len(seen) < 3 || len(seen) > 5) {
			t.Errorf("first page kept %d rows of about 4.6k", len(seen))
		}
		cursor = next
	}
	var want []string
	for _, r := range rows {
		want = append(want, r["id"].(string))
	}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("pages overlap or leave a gap:\ngot  %v\nwant %v", seen, want)
	}
	// Every continuation refetched the first server page (no server cursor sent).
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, q := range srv.queries {
		if strings.Contains(q, "cursor=") {
			t.Errorf("a continuation of page 1 sent a server cursor: %s", q)
		}
	}
}

func TestCompactFitLongestPrefixIsMaximal(t *testing.T) {
	var rows []map[string]any
	for i := 1; i <= 6; i++ {
		rows = append(rows, bigPassage(i, 4500))
	}
	srv := newPagedStub(t, map[string]string{"": searchBody(t, rows, false, nil, nil)})
	stdout, _, code := runVerb(t, srv.URL, "search", "x", "--format", "compact")
	if code != 0 {
		t.Fatal(code)
	}
	m := decodeCompact(t, stdout)
	k := len(resultIDs(t, m, "results"))
	// One more whole row would not fit.
	body, _ := json.Marshal(map[string]any{"results": rows[:k+1], "truncated": true, "next_cursor": encodeCLICursor("", k+1, "0123456789abcdef")})
	text, _, err := compactPage(body, "", pageCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if printedSize(text) <= compactFitBytes {
		t.Fatalf("kept %d rows but %d would fit (%d bytes)", k, k+1, printedSize(text))
	}
}

func TestCompactFitWrapsServerCursorAndPassesItThrough(t *testing.T) {
	var p1, p2 []map[string]any
	for i := 1; i <= 4; i++ {
		p1 = append(p1, bigPassage(i, 9000))
	}
	for i := 5; i <= 6; i++ {
		p2 = append(p2, bigPassage(i, 100))
	}
	srv := newPagedStub(t, map[string]string{
		"S1": searchBody(t, p1, true, "S2", nil),
		"S2": searchBody(t, p2, false, nil, nil),
	})
	// Page from server cursor S1: about 2 rows of 9k fit.
	stdout, _, code := runVerb(t, srv.URL, "search", "x", "--format", "compact", "--cursor", "S1")
	if code != 0 {
		t.Fatal(code)
	}
	m := decodeCompact(t, stdout)
	ids := resultIDs(t, m, "results")
	next := m["next_cursor"].(string)
	if p := cursorPayload(t, next); p.S != "S1" || p.K != len(ids) {
		t.Fatalf("cursor %+v after %d rows", p, len(ids))
	}
	// Continue until the server page ends: the server's own S2 passes through.
	for {
		stdout, _, code = runVerb(t, srv.URL, "search", "x", "--format", "compact", "--cursor", next)
		if code != 0 {
			t.Fatal(code)
		}
		m = decodeCompact(t, stdout)
		ids = append(ids, resultIDs(t, m, "results")...)
		next = m["next_cursor"].(string)
		if !strings.HasPrefix(next, "c1.") {
			break
		}
	}
	if next != "S2" || m["truncated"] != true {
		t.Fatalf("server cursor not passed through: %v %v", next, m["truncated"])
	}
	stdout, _, _ = runVerb(t, srv.URL, "search", "x", "--format", "compact", "--cursor", next)
	ids = append(ids, resultIDs(t, decodeCompact(t, stdout), "results")...)
	var want []string
	for _, r := range append(p1, p2...) {
		want = append(want, r["id"].(string))
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v\nwant %v", ids, want)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, q := range srv.queries {
		if u, _ := url.ParseQuery(q); u.Get("cursor") != "S1" && u.Get("cursor") != "S2" {
			t.Errorf("request without the server cursor: %s", q)
		}
	}
}

func TestCompactHugeFirstRowReturnedWholeAndCursorAdvances(t *testing.T) {
	rows := []map[string]any{bigPassage(1, 30000), bigPassage(2, 100)}
	srv := newPagedStub(t, map[string]string{"": searchBody(t, rows, false, nil, nil)})
	stdout, _, code := runVerb(t, srv.URL, "search", "x", "--format", "compact")
	if code != 0 {
		t.Fatal(code)
	}
	if printedSize(stdout) <= compactFitBytes {
		t.Fatal("the huge row should exceed the budget on its own")
	}
	m := decodeCompact(t, stdout)
	ids := resultIDs(t, m, "results")
	snippet := m["results"].([]any)[0].(map[string]any)["snippet"].(string)
	if len(ids) != 1 || !strings.HasSuffix(snippet, strings.Repeat("x", 30000)) {
		t.Fatalf("huge row not returned whole: %v", ids)
	}
	if p := cursorPayload(t, m["next_cursor"].(string)); p.K != 1 || m["truncated"] != true {
		t.Fatalf("cursor did not advance: %+v", p)
	}
	stdout, _, _ = runVerb(t, srv.URL, "search", "x", "--format", "compact", "--cursor", m["next_cursor"].(string))
	m = decodeCompact(t, stdout)
	if ids := resultIDs(t, m, "results"); len(ids) != 1 || ids[0] != rows[1]["id"] {
		t.Fatalf("second call %v", ids)
	}
	// A lone huge row is the whole page: no cursor.
	srv2 := newPagedStub(t, map[string]string{"": searchBody(t, rows[:1], false, nil, nil)})
	stdout, _, _ = runVerb(t, srv2.URL, "search", "x", "--format", "compact")
	if m := decodeCompact(t, stdout); m["next_cursor"] != nil || m["truncated"] != nil {
		t.Fatalf("lone huge row: %v %v", m["next_cursor"], m["truncated"])
	}
}

func TestCompactCursorRefusals(t *testing.T) {
	var rows []map[string]any
	for i := 1; i <= 10; i++ {
		rows = append(rows, bigPassage(i, 4500))
	}
	srv := newPagedStub(t, map[string]string{"": searchBody(t, rows, false, nil, nil)})
	stdout, _, _ := runVerb(t, srv.URL, "search", "supply chain", "--limit", "10", "--format", "compact")
	next := decodeCompact(t, stdout)["next_cursor"].(string)
	before := srv.count()
	cases := map[string][]string{
		"changed query":  {"search", "supply chains", "--limit", "10", "--format", "compact", "--cursor", next},
		"changed limit":  {"search", "supply chain", "--limit", "9", "--format", "compact", "--cursor", next},
		"json format":    {"search", "supply chain", "--limit", "10", "--format", "json", "--cursor", next},
		"table format":   {"search", "supply chain", "--limit", "10", "--format", "table", "--cursor", next},
		"default format": {"search", "supply chain", "--limit", "10", "--cursor", next},
		"malformed":      {"search", "supply chain", "--limit", "10", "--format", "compact", "--cursor", "c1.%%%"},
		"not JSON":       {"search", "supply chain", "--limit", "10", "--format", "compact", "--cursor", "c1." + base64.RawURLEncoding.EncodeToString([]byte("nope"))},
		"zero skip":      {"search", "supply chain", "--limit", "10", "--format", "compact", "--cursor", encodeCLICursor("", 0, cursorPayload(t, next).H)},
		"other verb":     {"toc", testFilingID, "--format", "compact", "--cursor", next},
	}
	for name, argv := range cases {
		_, stderr, code := runVerb(t, srv.URL, argv...)
		if code != ExitUsageError {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, stderr)
		}
	}
	if srv.count() != before {
		t.Errorf("refused cursors sent %d requests", srv.count()-before)
	}
	// The dry run of a continuation prints the server request it wraps.
	stdout, _, code := runVerb(t, srv.URL, "search", "supply chain", "--limit", "10", "--format", "compact", "--cursor", next, "--dry-run")
	if code != 0 || stdout != "[dry-run] GET "+srv.URL+"/research/search?limit=10&mode=semantic&q=supply+chain\n" {
		t.Fatalf("dry run %d %q", code, stdout)
	}
}

// The neighbours list is fitted like the other lists; the head passage
// stays on every page.
func TestCompactReadPassageFitsNeighbours(t *testing.T) {
	head := bigPassage(1, 9000)
	var neighbours []any
	for i := 2; i <= 5; i++ {
		neighbours = append(neighbours, bigPassage(i, 6000))
	}
	body := ordered(t, "passage", head, "neighbours", neighbours, "truncated", false, "next_cursor", nil)
	srv := newStub(t, 200, nil, body)
	var seen []string
	cursor := ""
	for call := 0; call < 6; call++ {
		args := []string{"read", "passage", testChunkID, "--window", "2", "--format", "compact"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		stdout, stderr, code := runVerb(t, srv.URL, args...)
		if code != 0 {
			t.Fatalf("call %d exit %d: %s", call, code, stderr)
		}
		if printedSize(stdout) > compactFitBytes {
			t.Fatalf("call %d printed %d bytes", call, printedSize(stdout))
		}
		m := decodeCompact(t, stdout)
		p := m["passage"].(map[string]any)
		if p["id"] != head["id"] {
			t.Fatalf("call %d lost the head passage", call)
		}
		for _, dropped := range []string{"chunk_index", "exchange", "formdescription"} {
			if _, ok := p[dropped]; ok {
				t.Errorf("passage kept %s", dropped)
			}
		}
		seen = append(seen, resultIDs(t, m, "neighbours")...)
		next, _ := m["next_cursor"].(string)
		if next == "" {
			break
		}
		if call == 0 && m["truncated"] != true {
			t.Fatal("a cut page must say truncated:true")
		}
		cursorPayload(t, next)
		cursor = next
	}
	var want []string
	for _, n := range neighbours {
		want = append(want, n.(map[string]any)["id"].(string))
	}
	if cursor == "" || strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("neighbours %v, want each of %v once over several pages", seen, want)
	}
}

// Following every c1 cursor of read section and toc shows each passage and
// section exactly once.
func TestCompactSectionAndTocContinuation(t *testing.T) {
	var passages []any
	var wantP []string
	for i := 1; i <= 8; i++ {
		p := bigPassage(i, 6000)
		passages = append(passages, p)
		wantP = append(wantP, p["id"].(string))
	}
	section := ordered(t, "filing_id", testFilingID, "section_header", "Risk Factors", "chunk_count", 8,
		"passages", passages, "truncated", false, "next_cursor", nil)
	var sections []string
	for i := 0; i < 400; i++ {
		sections = append(sections, fmt.Sprintf("Section %d %s", i, strings.Repeat("h", 100)))
	}
	toc := ordered(t, "filing_id", testFilingID, "sections", sections, "truncated", false, "next_cursor", nil)
	cases := []struct {
		name, body, key string
		argv            []string
		want            []string
	}{
		{"read section", section, "passages", []string{"read", "section", testFilingID, "Risk Factors"}, wantP},
		{"toc", toc, "sections", []string{"toc", testFilingID}, sections},
	}
	for _, c := range cases {
		srv := newStub(t, 200, nil, c.body)
		var seen []string
		cursor, pages := "", 0
		for ; pages < 50; pages++ {
			args := append(append([]string{}, c.argv...), "--format", "compact")
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			stdout, stderr, code := runVerb(t, srv.URL, args...)
			if code != 0 {
				t.Fatalf("%s page %d exit %d: %s", c.name, pages, code, stderr)
			}
			if printedSize(stdout) > compactFitBytes {
				t.Fatalf("%s page %d printed %d bytes", c.name, pages, printedSize(stdout))
			}
			m := decodeCompact(t, stdout)
			for _, row := range m[c.key].([]any) {
				if r, ok := row.(map[string]any); ok {
					seen = append(seen, r["id"].(string))
				} else {
					seen = append(seen, row.(string))
				}
			}
			next, _ := m["next_cursor"].(string)
			if next == "" {
				break
			}
			cursor = next
		}
		if pages < 1 || strings.Join(seen, "\n") != strings.Join(c.want, "\n") {
			t.Fatalf("%s: %d pages, %d rows, want each of %d once", c.name, pages+1, len(seen), len(c.want))
		}
	}
}

// filings and find pass the server's own next_cursor back unchanged: the
// next request carries it and returns the next rows (compact and json).
func TestFilingsAndFindForwardServerCursor(t *testing.T) {
	filings := map[string]string{
		"":   filingsBody(t, []map[string]any{filingRowJSON("aaaaaaaa-0000-4000-8000-000000000002", "2025-10-31", nil)}, "F2"),
		"F2": filingsBody(t, []map[string]any{filingRowJSON("aaaaaaaa-0000-4000-8000-000000000001", "2024-11-01", nil)}, nil),
	}
	match := func(id string) map[string]any {
		return map[string]any{"chunk_id": id, "section_header": "Item 1", "snippet": "TSMC", "url": testPermalink}
	}
	findPage := func(id string, next any) string {
		return ordered(t, "filing_id", testFilingID, "term", "TSMC", "found", true, "total_matches", 2,
			"matches", []any{match(id)}, "next_cursor", next)
	}
	finds := map[string]string{"": findPage("11111111-2222-4333-8444-000000000001", "M2"), "M2": findPage("11111111-2222-4333-8444-000000000002", nil)}
	for _, format := range []string{"compact", "json"} {
		fsrv := newPagedStub(t, filings)
		var got []string
		for _, cursor := range []string{"", "F2"} {
			args := []string{"filings", "AAPL", "--limit", "1", "--format", format}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			stdout, stderr, code := runVerb(t, fsrv.URL, args...)
			if code != 0 {
				t.Fatalf("%s filings exit %d: %s", format, code, stderr)
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(stdout), &m)
			for _, r := range m["filings"].([]any) {
				got = append(got, r.(map[string]any)["filing_id"].(string))
			}
			if cursor == "" && (m["next_cursor"] != "F2" || !strings.Contains(stderr, "--cursor F2")) {
				t.Fatalf("%s filings next_cursor %v, stderr %q", format, m["next_cursor"], stderr)
			}
		}
		if strings.Join(got, ",") != "aaaaaaaa-0000-4000-8000-000000000002,aaaaaaaa-0000-4000-8000-000000000001" {
			t.Errorf("%s filings rows %v", format, got)
		}
		fsrv.mu.Lock()
		if q, _ := url.ParseQuery(fsrv.queries[1]); q.Get("cursor") != "F2" || q.Get("limit") != "1" || q.Get("symbol") != "AAPL" {
			t.Errorf("%s filings second request %q", format, fsrv.queries[1])
		}
		fsrv.mu.Unlock()

		msrv := newPagedStub(t, finds)
		got = nil
		for _, cursor := range []string{"", "M2"} {
			args := []string{"find", testFilingID, "TSMC", "--limit", "1", "--format", format}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			stdout, stderr, code := runVerb(t, msrv.URL, args...)
			if code != 0 {
				t.Fatalf("%s find exit %d: %s", format, code, stderr)
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(stdout), &m)
			for _, r := range m["matches"].([]any) {
				got = append(got, r.(map[string]any)["chunk_id"].(string))
			}
			if cursor == "" && m["next_cursor"] != "M2" {
				t.Fatalf("%s find next_cursor %v", format, m["next_cursor"])
			}
		}
		if strings.Join(got, ",") != "11111111-2222-4333-8444-000000000001,11111111-2222-4333-8444-000000000002" {
			t.Errorf("%s find rows %v", format, got)
		}
		msrv.mu.Lock()
		if q, _ := url.ParseQuery(msrv.queries[1]); q.Get("cursor") != "M2" || q.Get("term") != "TSMC" || q.Get("limit") != "1" {
			t.Errorf("%s find second request %q", format, msrv.queries[1])
		}
		msrv.mu.Unlock()
	}
}

func TestCompactSectionAndTocFit(t *testing.T) {
	var passages []any
	for i := 1; i <= 8; i++ {
		passages = append(passages, bigPassage(i, 6000))
	}
	section := ordered(t, "filing_id", testFilingID, "section_header", "Risk Factors", "chunk_count", 8,
		"passages", passages, "truncated", false, "next_cursor", nil)
	srv := newStub(t, 200, nil, section)
	stdout, _, code := runVerb(t, srv.URL, "read", "section", testFilingID, "Risk Factors", "--format", "compact")
	if code != 0 {
		t.Fatal(code)
	}
	m := decodeCompact(t, stdout)
	if n := len(m["passages"].([]any)); n < 2 || n >= 8 || m["truncated"] != true || printedSize(stdout) > compactFitBytes {
		t.Fatalf("section fit %d rows, %d bytes", n, printedSize(stdout))
	}
	if !strings.HasPrefix(stdout, `{"filing_id":"`+testFilingID+`","section_header":"Risk Factors","chunk_count":8,"passages":[`) {
		t.Errorf("member order changed: %.120s", stdout)
	}

	var sections []string
	for i := 0; i < 400; i++ {
		sections = append(sections, fmt.Sprintf("Section %d %s", i, strings.Repeat("h", 100)))
	}
	srv2 := newStub(t, 200, nil, mustJSON(t, map[string]any{"filing_id": testFilingID, "sections": sections, "truncated": false, "next_cursor": nil}))
	stdout, _, code = runVerb(t, srv2.URL, "toc", testFilingID, "--format", "compact")
	m = decodeCompact(t, stdout)
	if code != 0 || len(m["sections"].([]any)) >= 400 || m["truncated"] != true || printedSize(stdout) > compactFitBytes {
		t.Fatalf("toc fit: exit %d, %d sections", code, len(m["sections"].([]any)))
	}
}

// ─── filings ─────────────────────────────────────────────────────────────────

func filingsBody(t *testing.T, rows []map[string]any, next any) string {
	t.Helper()
	if rows == nil {
		rows = []map[string]any{}
	}
	items := make([]json.RawMessage, len(rows))
	for i, r := range rows {
		items[i] = json.RawMessage(ordered(t, "filing_id", r["filing_id"], "form", r["form"], "datefiled", r["datefiled"],
			"period", r["period"], "exchange_document_id", r["exchange_document_id"],
			"exchange_document_kind", r["exchange_document_kind"], "url", r["url"]))
	}
	return ordered(t, "company", json.RawMessage(ordered(t, "name", "Apple Inc.", "symbol", "AAPL", "exchange", "NGS")),
		"filings", rawList(items...), "truncated", false, "next_cursor", next)
}

func filingRowJSON(id, date string, period any) map[string]any {
	return map[string]any{"filing_id": id, "form": "10-K", "datefiled": date, "period": period,
		"exchange_document_id": "0000320193-25-000079", "exchange_document_kind": "sec_accession_number",
		"url": "https://mosaic-finance.com/filings/" + id + "/f/k1.tok/"}
}

func TestFilingsCompactAndRequest(t *testing.T) {
	rows := []map[string]any{
		filingRowJSON("aaaaaaaa-0000-4000-8000-000000000003", "2025-10-31", "2025-09-27"),
		filingRowJSON("aaaaaaaa-0000-4000-8000-000000000002", "2024-11-01", nil),
		filingRowJSON("aaaaaaaa-0000-4000-8000-000000000001", "2023-11-03", "2023-09-30"),
	}
	srv := newStub(t, 200, nil, filingsBody(t, rows, "nx"))
	stdout, stderr, code := runVerb(t, srv.URL, "filings", " aapl ", "--formtype", "10-K", "--date-from", "2020-01-01", "--limit", "3", "--format", "compact")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if u := srv.last(); u != "/research/filings?date_from=2020-01-01&formtype=10-K&limit=3&symbol=aapl" {
		t.Errorf("request %q", u)
	}
	want := `{"company":{"name":"Apple Inc.","symbol":"AAPL","exchange":"NGS"},"filings":[` +
		`{"filing_id":"aaaaaaaa-0000-4000-8000-000000000003","form":"10-K","datefiled":"2025-10-31","period":"2025-09-27",` +
		`"exchange_document_id":"0000320193-25-000079","exchange_document_kind":"sec_accession_number","url":"https://mosaic-finance.com/filings/aaaaaaaa-0000-4000-8000-000000000003/f/k1.tok/"},` +
		`{"filing_id":"aaaaaaaa-0000-4000-8000-000000000002","form":"10-K","datefiled":"2024-11-01",` +
		`"exchange_document_id":"0000320193-25-000079","exchange_document_kind":"sec_accession_number","url":"https://mosaic-finance.com/filings/aaaaaaaa-0000-4000-8000-000000000002/f/k1.tok/"},` +
		`{"filing_id":"aaaaaaaa-0000-4000-8000-000000000001","form":"10-K","datefiled":"2023-11-03","period":"2023-09-30",` +
		`"exchange_document_id":"0000320193-25-000079","exchange_document_kind":"sec_accession_number","url":"https://mosaic-finance.com/filings/aaaaaaaa-0000-4000-8000-000000000001/f/k1.tok/"}],` +
		`"next_cursor":"nx"}` + "\n"
	if stdout != want {
		t.Fatalf("got  %s\nwant %s", stdout, want)
	}
	if !strings.Contains(stderr, "More results: rerun with --cursor nx") {
		t.Errorf("stderr %q", stderr)
	}

	// JSON is the body re-indented; the table lists the rows.
	stdout, stderr, _ = runVerb(t, srv.URL, "filings", "AAPL", "--format", "json")
	var got, wantBody any
	_ = json.Unmarshal([]byte(stdout), &got)
	_ = json.Unmarshal([]byte(filingsBody(t, rows, "nx")), &wantBody)
	if mustJSON(t, got) != mustJSON(t, wantBody) || !strings.Contains(stdout, "\n  \"company\"") || !strings.Contains(stderr, "--cursor nx") {
		t.Errorf("json output:\n%s\n%s", stdout, stderr)
	}
	stdout, _, _ = runVerb(t, srv.URL, "filings", "AAPL", "--format", "table")
	if !strings.HasPrefix(stdout, "FILING_ID") || !strings.Contains(stdout, "2024-11-01  -") {
		t.Errorf("table:\n%s", stdout)
	}
	if u := srv.last(); u != "/research/filings?limit=10&symbol=AAPL" {
		t.Errorf("default limit request %q", u)
	}
}

func TestFilingsEmptyIsExit0AndUnknownSymbolIsExit3(t *testing.T) {
	srv := newStub(t, 200, nil, filingsBody(t, nil, nil))
	stdout, stderr, code := runVerb(t, srv.URL, "filings", "AAPL", "--formtype", "S-1", "--format", "compact")
	if code != 0 || stdout != `{"company":{"name":"Apple Inc.","symbol":"AAPL","exchange":"NGS"},"filings":[]}`+"\n" ||
		!strings.Contains(stderr, "No filings found") {
		t.Fatalf("empty: exit %d %q %q", code, stdout, stderr)
	}
	nf := newStub(t, 404, nil, `{"error":"No company with the symbol ZZZZQ.","code":"NOT_FOUND","suggestion":"Look the company up with /research/companies and retry with the symbol it returns."}`)
	_, stderr, code = runVerb(t, nf.URL, "filings", "ZZZZQ", "--format", "compact")
	if code != ExitNotFound || !strings.Contains(stderr, "Look the company up") {
		t.Fatalf("unknown symbol: exit %d %q", code, stderr)
	}
}

// ─── find ────────────────────────────────────────────────────────────────────

func findBody(t *testing.T, term string, found bool, matches []map[string]any) string {
	t.Helper()
	if matches == nil {
		matches = []map[string]any{}
	}
	return ordered(t, "filing_id", testFilingID, "company_name", "NVIDIA Corporation", "symbol", "NVDA", "form", "10-K",
		"datefiled", "2026-02-25", "exchange_document_id", "0001045810-26-000021", "exchange_document_kind", "sec_accession_number",
		"term", term, "found", found, "total_matches", len(matches), "matches", matches, "next_cursor", nil)
}

func TestFindPresentAndAbsent(t *testing.T) {
	m := map[string]any{"chunk_id": testChunkID, "section_header": "Item 1", "snippet": "foundries such as TSMC", "url": testPermalink}
	srv := newStub(t, 200, nil, findBody(t, "TSMC", true, []map[string]any{m}))
	stdout, stderr, code := runVerb(t, srv.URL, "find", testFilingID, " TSMC ", "--format", "compact")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if u := srv.last(); u != "/research/filings/"+testFilingID+"/find?limit=5&term=TSMC" {
		t.Errorf("request %q", u)
	}
	got := decodeCompact(t, stdout)
	if got["found"] != true || got["total_matches"] != float64(1) || len(got["matches"].([]any)) != 1 ||
		got["matches"].([]any)[0].(map[string]any)["url"] != testPermalink {
		t.Fatalf("found: %s", stdout)
	}
	if _, ok := got["next_cursor"]; ok {
		t.Error("a null next_cursor must be dropped")
	}
	stdout, _, _ = runVerb(t, srv.URL, "find", testFilingID, "TSMC", "--format", "table")
	if !strings.HasPrefix(stdout, "Found: 1 matching passages for TSMC\nCHUNK_ID") {
		t.Errorf("table:\n%s", stdout)
	}

	absent := newStub(t, 200, nil, findBody(t, "zzqx", false, nil))
	stdout, _, code = runVerb(t, absent.URL, "find", testFilingID, "zzqx", "--format", "compact")
	if code != 0 || !strings.Contains(stdout, `"term":"zzqx","found":false,"total_matches":0,"matches":[]}`) {
		t.Fatalf("absent: exit %d %s", code, stdout)
	}
	stdout, _, code = runVerb(t, absent.URL, "find", testFilingID, "zzqx", "--format", "table")
	if code != 0 || stdout != "Not found: no passage mentions zzqx\n" {
		t.Fatalf("absent table: %d %q", code, stdout)
	}

	// Quotes pass through unchanged: an exact phrase.
	_, _, _ = runVerb(t, srv.URL, "find", testFilingID, `"gross margin"`, "--limit", "10")
	if u := srv.last(); u != "/research/filings/"+testFilingID+"/find?limit=10&term=%22gross+margin%22" {
		t.Errorf("quoted request %q", u)
	}
	nf := newStub(t, 404, nil, `{"error":"No filing with that id.","code":"NOT_FOUND"}`)
	if _, _, code := runVerb(t, nf.URL, "find", testFilingID, "TSMC"); code != ExitNotFound {
		t.Errorf("unknown filing: exit %d", code)
	}
}

// ─── validation: exit 2 before any request ───────────────────────────────────

func TestFilingsFindLatestOnlyValidation(t *testing.T) {
	cases := map[string][]string{
		"filings no symbol":        {"filings", "  "},
		"filings long symbol":      {"filings", strings.Repeat("S", 21)},
		"filings long formtype":    {"filings", "AAPL", "--formtype", strings.Repeat("F", 201)},
		"filings limit 0":          {"filings", "AAPL", "--limit", "0"},
		"filings limit 26":         {"filings", "AAPL", "--limit", "26"},
		"filings bad date":         {"filings", "AAPL", "--date-from", "2024-02-30"},
		"filings dates reversed":   {"filings", "AAPL", "--date-from", "2025-01-02", "--date-to", "2025-01-01"},
		"filings format":           {"filings", "AAPL", "--format", "csv"},
		"find non uuid":            {"find", "nope", "TSMC"},
		"find empty term":          {"find", testFilingID, "   "},
		"find no letter":           {"find", testFilingID, "!!!"},
		"find quoted no letter":    {"find", testFilingID, `"!!"`},
		"find long term":           {"find", testFilingID, strings.Repeat("t", 201)},
		"find long UTF-16":         {"find", testFilingID, strings.Repeat("\U0001F4C8", 100) + "a"},
		"find limit 0":             {"find", testFilingID, "TSMC", "--limit", "0"},
		"find limit 11":            {"find", testFilingID, "TSMC", "--limit", "11"},
		"latest without symbol":    {"search", "x", "--latest-only"},
		"latest with date_from":    {"search", "x", "--symbol", "AAPL", "--latest-only", "--date-from", "2024-01-01"},
		"latest with date_to":      {"search", "x", "--symbol", "AAPL", "--latest-only", "--date-to", "2024-01-01"},
		"latest broad":             {"search", "x", "--latest-only", "--mode", "broad"},
		"latest broad with symbol": {"search", "x", "--symbol", "AAPL", "--latest-only", "--mode", "broad"},
	}
	srv := newStub(t, 200, nil, "{}")
	wording := map[string]string{
		"latest without symbol": "'latest_only' needs symbol.",
		"latest with date_from": "'latest_only' cannot be combined with date_from or date_to.",
		"latest with date_to":   "'latest_only' cannot be combined with date_from or date_to.",
		"latest broad":          "Filters (symbol, formtype, date_from, date_to, latest_only) are not supported with mode=broad.",
	}
	for name, argv := range cases {
		_, stderr, code := runVerb(t, srv.URL, argv...)
		if code != ExitUsageError {
			t.Errorf("%s: exit %d, want 2 (%q)", name, code, stderr)
		}
		if w := wording[name]; w != "" && !strings.Contains(stderr, w) {
			t.Errorf("%s: stderr %q lacks the server wording %q", name, stderr, w)
		}
	}
	if n := srv.calls.Load(); n != 0 {
		t.Errorf("validation failures sent %d requests", n)
	}
	// 200 characters and a letter pass; a digit alone passes.
	ok := newStub(t, 200, nil, findBody(t, "x", false, nil))
	for _, term := range []string{strings.Repeat("t", 200), "2025", "ş"} {
		if _, stderr, code := runVerb(t, ok.URL, "find", testFilingID, term); code != 0 {
			t.Errorf("term %.12q: exit %d %s", term, code, stderr)
		}
	}
}

func TestSearchLatestOnlySendsTheFlag(t *testing.T) {
	body := `{"results":[{"id":"c1","filing_id":"f1","snippet":"s"}],"entity_resolution":null,` +
		`"latest_filing":{"applied":true,"filing_id":"f1","datefiled":"2025-10-31","form":"10-K"},"truncated":false,"next_cursor":null}`
	srv := newStub(t, 200, nil, body)
	stdout, stderr, code := runVerb(t, srv.URL, "search", "revenue", "--symbol", "AAPL", "--formtype", "10-K", "--latest-only", "--format", "compact")
	if code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	if u := srv.last(); u != "/research/search?formtype=10-K&latest_only=true&limit=10&mode=semantic&q=revenue&symbol=AAPL" {
		t.Errorf("request %q", u)
	}
	if !strings.Contains(stdout, `"latest_filing":{"applied":true,"filing_id":"f1","datefiled":"2025-10-31","form":"10-K"}`) {
		t.Errorf("latest_filing dropped: %s", stdout)
	}
	// Without the flag the request is exactly as before.
	_, _, _ = runVerb(t, srv.URL, "search", "revenue", "--symbol", "AAPL")
	if u := srv.last(); strings.Contains(u, "latest_only") {
		t.Errorf("latest_only sent without the flag: %q", u)
	}
}

// ─── companies ───────────────────────────────────────────────────────────────

func TestCompaniesCompact(t *testing.T) {
	srv := serveCompanies([]mockCompanyResult{
		{CompanyName: "Apple & Co", Symbol: "AAPL", Exchange: "NGS", FilingCount: 4127, IssuerKey: nil},
	})
	defer srv.Close()
	stdout, stderr, err := runCompaniesCmd([]string{"companies", "search", "Apple", "--format", "compact"}, srv)
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	if stdout != `[{"company_name":"Apple & Co","symbol":"AAPL","exchange":"NGS","country":"US","filing_count":4127}]`+"\n" {
		t.Fatalf("companies search compact %q", stdout)
	}
}

func TestCompaniesGetCompact(t *testing.T) {
	earliest := "1994-01-01"
	results := []mockCompanyResult{
		{CompanyName: "Apple & Co", Symbol: "AAPL", Exchange: "NGS", FilingCount: 4127, EarliestFiling: &earliest, IssuerKey: strPtrCmd("cik:320193")},
	}
	// Pass 1: a direct issuer_key match from /research/companies.
	srv := serveCompanies(results)
	defer srv.Close()
	stdout, stderr, err := runCompaniesCmd([]string{"companies", "get", "cik:320193", "--format", "compact"}, srv)
	want := `{"company_name":"Apple & Co","symbol":"AAPL","exchange":"NGS","country":"US","filing_count":4127,` +
		`"earliest_filing":"1994-01-01","issuer_key":"cik:320193"}` + "\n"
	if err != nil || stdout != want {
		t.Fatalf("pass 1: %v %q %s", err, stdout, stderr)
	}
	// Pass 2: no match in the typeahead, found in the GET /companies catalog.
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/research/companies" {
			_ = json.NewEncoder(w).Encode(researchCompaniesBody(nil))
			return
		}
		_ = json.NewEncoder(w).Encode(results)
	}))
	defer catalog.Close()
	stdout, stderr, err = runCompaniesCmd([]string{"companies", "get", "cik:320193", "--format", "compact"}, catalog)
	if err != nil || stdout != want {
		t.Fatalf("pass 2: %v %q %s", err, stdout, stderr)
	}
	if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout, "null") || strings.Contains(stdout, "latest_filing") {
		t.Errorf("not one line without nulls: %q", stdout)
	}
}
