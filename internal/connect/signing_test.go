package connect

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shared vectors made in a browser runtime with WebCrypto (Story 94.7):
// one P-256 key, two envelopes it signed, a JCS input with its canonical
// form and card digest, and a text digest. The workspace, the relay and the
// daemon all check these same values.
const (
	vecFingerprint = "KnakR2lnF3Vw9e7hsxlc4cpMuAP1Elyam2CFBVVjVnY"
	vecKey         = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAExzG1l8iR-gJZU9ukMA_TgDZQx9sT3jwVXo4xndfK0vTJvmZ2-S6CeDJw_g5ZBQreXNSOX4w-fjP5Or5CMHGnEg"
	vecDescriptor  = `{"tool_name":"Bash","input":{"command":"ls -la ~/x","description":"List files"},"z":[1,2.5,-0,1e21,1e-7,0.000001,123456789012345678,1.0],"é":"café <b>&amp; ` + "\u2028" + ` \u0001 \"q\" \\","😀":true,"｡":null,"a":{"b":{},"a":[]},"archivist_session_scope":{"kind":"prefix","value":"ls"}}`
	vecJCS         = `{"a":{"a":[],"b":{}},"archivist_session_scope":{"kind":"prefix","value":"ls"},"input":{"command":"ls -la ~/x","description":"List files"},"tool_name":"Bash","z":[1,2.5,0,1e+21,1e-7,0.000001,123456789012345680,1],"é":"café <b>&amp; ` + "\u2028" + ` \u0001 \"q\" \\","😀":true,"｡":null}`
	vecCard        = "h10B895mnRRss5Z3nyM7Gnbn-_vQbo8LyHRZn26Palg"
	vecText        = "What changed in the last 10-K? café"
	vecTextHash    = "yHbxPe0ofG_46qa2wj-fy7MBxtaVuthcaNndCslykA0"
	vecSession     = "0b8f3f5e-8a3c-4c39-9d1a-2f5d7a1e6b10"

	vecApprovalPayload = "eyJ2IjoxLCJraW5kIjoiYXBwcm92YWxfcmVzcG9uc2UiLCJjb3VudGVyIjo3LCJpYXQiOjE3OTI1NDA4MDAwMDAsInNlc3Npb25JZCI6IjBiOGYzZjVlLThhM2MtNGMzOS05ZDFhLTJmNWQ3YTFlNmIxMCIsImFwcHJvdmFsSWQiOiJyZXEtMSIsImRlY2lzaW9uIjoiYWxsb3ciLCJzY29wZSI6IiIsImNhcmRTaGEyNTYiOiJoMTBCODk1bW5SUnNzNVozbnlNN0duYm4tX3ZRYm84THlIUlpuMjZQYWxnIn0"
	vecApprovalSig     = "BjJwtxrc4t0LbuORJ38ys_9vpn4uGq2xZXyYk6JdKFyRdOOlccY_2JIV6Bb25ixKgQP4ndf_PKkjVIrXb1UUYQ"
	vecTurnPayload     = "eyJ2IjoxLCJraW5kIjoidHVybiIsImNvdW50ZXIiOjgsImlhdCI6MTc5MjU0MDgwMDAwMSwiYWdlbnQiOiJjbGF1ZGUiLCJ0ZXh0U2hhMjU2IjoieUhieFBlMG9mR180NnFhMndqLWZ5N01CeHRhVnV0aGNhTm5kQ3NseWtBMCIsIm1vZGUiOiJhc2sifQ"
	vecTurnSig         = "Rswzs0ONnt_W7pRMEt3BNUtMDogWkhq92akFA-s-PBTqnQHuwlyFqn2soVxL7xlZljagG3JTKSRc-H5k8xn1ZA"
)

func vecEnvelope(payload, sig string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"v": 1, "key": vecKey, "payload": payload, "sig": sig})
	return b
}

func TestSigningJCSSharedVector(t *testing.T) {
	var v any
	if err := decodeNumbers([]byte(vecDescriptor), &v); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != vecJCS {
		t.Fatalf("JCS\n got %s\nwant %s", got, vecJCS)
	}
	if d := sha256b64u(got); d != vecCard {
		t.Fatalf("card digest %s, want %s", d, vecCard)
	}
	// The daemon's digest of the same descriptor as it emits it.
	if d, err := cardDigest(v); err != nil || d != vecCard {
		t.Fatalf("cardDigest %s %v", d, err)
	}
	// The same value read with float64 numbers (as the relay and browser
	// read it) has the same canonical form.
	var f any
	if err := json.Unmarshal([]byte(vecDescriptor), &f); err != nil {
		t.Fatal(err)
	}
	if got, _ := canonicalJSON(f); string(got) != vecJCS {
		t.Fatalf("float64 JCS %s", got)
	}
	if textHash(vecText) != vecTextHash {
		t.Fatalf("text digest %s", textHash(vecText))
	}
}

func TestSigningJCSNumbersAndStrings(t *testing.T) {
	for in, want := range map[float64]string{
		0: "0", math.Copysign(0, -1): "0", 1: "1", -1: "-1", 1.5: "1.5", 100: "100", 1e20: "100000000000000000000",
		1e21: "1e+21", 1.5e21: "1.5e+21", 1e-6: "0.000001", 1e-7: "1e-7", 1.25e-7: "1.25e-7", 123e-20: "1.23e-18",
		0.1: "0.1", 0.3: "0.3", 1.0 / 3: "0.3333333333333333", 9007199254740993: "9007199254740992",
		5e-324: "5e-324", 1.7976931348623157e308: "1.7976931348623157e+308", -1e-7: "-1e-7", 4.35: "4.35",
		123456789012345678: "123456789012345680", 0.000001234: "0.000001234", 12345.678: "12345.678",
	} {
		got, err := appendJSNumber(nil, in)
		if err != nil || string(got) != want {
			t.Errorf("%v: got %s (%v), want %s", in, got, err, want)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := appendJSNumber(nil, bad); err == nil {
			t.Errorf("%v has no JSON form", bad)
		}
	}
	if _, err := canonicalJSON(json.Number("1e400")); err == nil {
		t.Error("1e400 is not finite")
	}
	for in, want := range map[string]string{
		"a\"b\\c":      `"a\"b\\c"`,
		"\b\f\n\r\t":   `"\b\f\n\r\t"`,
		"\x00\x01\x1f": `"\u0000\u0001\u001f"`,
		"<>&  \x7f":    "\"<>&  \x7f\"",
		"é😀":           `"é😀"`,
		"/":            `"/"`,
	} {
		if got := string(appendJSString(nil, in)); got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
	// Keys sort by UTF-16 code units: U+FF61 (one unit) after U+1F600
	// (a surrogate pair starting 0xD83D).
	got, _ := canonicalJSON(map[string]any{"｡": 1.0, "😀": 2.0, "é": 3.0, "a": 4.0, "B": 5.0})
	if string(got) != `{"B":5,"a":4,"é":3,"😀":2,"｡":1}` {
		t.Fatalf("key order %s", got)
	}
}

func TestSigningWebCryptoVector(t *testing.T) {
	key, _ := base64.RawURLEncoding.DecodeString(vecKey)
	if fp := BrowserFingerprint(key); fp != vecFingerprint {
		t.Fatalf("fingerprint %s", fp)
	}
	for name, e := range map[string]json.RawMessage{
		"approval": vecEnvelope(vecApprovalPayload, vecApprovalSig),
		"turn":     vecEnvelope(vecTurnPayload, vecTurnSig),
	} {
		env, err := decodeSigEnvelope(e)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := verifySignature(env.key, env.payload, env.sig); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p, err := decodeSignedPayload(env.payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		switch name {
		case "approval":
			if p.Kind != "approval_response" || p.Counter != 7 || p.SessionID != vecSession || p.ApprovalID != "req-1" ||
				p.Decision != "allow" || !p.has["scope"] || p.Scope != "" || p.CardSha256 != vecCard {
				t.Fatalf("approval payload %+v", p)
			}
		case "turn":
			if p.Kind != "turn" || p.Counter != 8 || p.Agent != "claude" || p.TextSha256 != vecTextHash || p.Mode != "ask" || p.has["sessionId"] {
				t.Fatalf("turn payload %+v", p)
			}
		}
		// Any change to the payload or the signature fails.
		bad := append([]byte(nil), env.payload...)
		bad[len(bad)-2] ^= 1
		if verifySignature(env.key, bad, env.sig) == nil {
			t.Fatalf("%s: changed payload verified", name)
		}
		sig := append([]byte(nil), env.sig...)
		sig[5] ^= 1
		if verifySignature(env.key, env.payload, sig) == nil {
			t.Fatalf("%s: changed signature verified", name)
		}
	}
	// The bindings of the shared approval: this card, this answer.
	in := &Inbound{Kind: "approval_response", SessionID: vecSession, ApprovalID: "req-1", Decision: "allow"}
	env, _ := decodeSigEnvelope(vecEnvelope(vecApprovalPayload, vecApprovalSig))
	p, _ := decodeSignedPayload(env.payload)
	if why := bindApproval(vecSession, vecCard, in)(p); why != "" {
		t.Fatalf("binding: %s", why)
	}
	for why, mut := range map[string]func(*Inbound) (string, string){
		"card":     func(in *Inbound) (string, string) { return vecSession, textHash("other") },
		"session":  func(in *Inbound) (string, string) { return randomUUID(), vecCard },
		"scope":    func(in *Inbound) (string, string) { in.Scope = "allow_always"; return vecSession, vecCard },
		"approval": func(in *Inbound) (string, string) { in.ApprovalID = "req-2"; return vecSession, vecCard },
	} {
		c := *in
		sid, card := mut(&c)
		if got := bindApproval(sid, card, &c)(p); got != why {
			t.Errorf("%s: got %q", why, got)
		}
	}
	// The shared turn starts a claude session with the shared text.
	env, _ = decodeSigEnvelope(vecEnvelope(vecTurnPayload, vecTurnSig))
	p, _ = decodeSignedPayload(env.payload)
	if why := bindStart(&Inbound{Agent: "claude", Prompt: vecText})(p); why != "" {
		t.Fatalf("start binding: %s", why)
	}
	if why := bindStart(&Inbound{Agent: "codex", Prompt: vecText})(p); why != "agent" {
		t.Fatalf("start binding for codex: %s", why)
	}
}

func TestSigningEnvelopeAndPayloadShapes(t *testing.T) {
	b := newTestBrowser(t)
	good := b.sign("set_mode", map[string]any{"sessionId": vecSession, "mode": "ask"})
	mk := func(mut func(m map[string]any)) json.RawMessage {
		m := map[string]any{}
		for k, v := range good {
			m[k] = v
		}
		mut(m)
		raw, _ := json.Marshal(m)
		return raw
	}
	if _, err := decodeSigEnvelope(mk(func(map[string]any) {})); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{
		"extra field":   mk(func(m map[string]any) { m["x"] = 1 }),
		"missing key":   mk(func(m map[string]any) { delete(m, "key") }),
		"version 2":     mk(func(m map[string]any) { m["v"] = 2 }),
		"version text":  mk(func(m map[string]any) { m["v"] = "1" }),
		"padded":        mk(func(m map[string]any) { m["payload"] = m["payload"].(string) + "=" }),
		"short sig":     mk(func(m map[string]any) { m["sig"] = m["sig"].(string)[:85] }),
		"long key":      mk(func(m map[string]any) { m["key"] = strings.Repeat("A", 201) }),
		"long payload":  mk(func(m map[string]any) { m["payload"] = strings.Repeat("A", 2049) }),
		"not an object": json.RawMessage(`"sig"`),
		"null field":    json.RawMessage(`{"v":1,"key":null,"payload":"e30","sig":"` + strings.Repeat("A", 86) + `"}`),
		"duplicate":     json.RawMessage(`{"v":1,"v":1,"key":"AA","payload":"e30","sig":"` + strings.Repeat("A", 86) + `"}`),
	} {
		if _, err := decodeSigEnvelope(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, payload := range map[string]string{
		"extra":         `{"v":1,"kind":"set_mode","counter":1,"iat":0,"sessionId":"` + vecSession + `","mode":"ask","x":1}`,
		"missing":       `{"v":1,"kind":"set_mode","counter":1,"iat":0,"sessionId":"` + vecSession + `"}`,
		"null":          `{"v":1,"kind":"set_mode","counter":1,"iat":0,"sessionId":"` + vecSession + `","mode":null}`,
		"zero counter":  `{"v":1,"kind":"set_mode","counter":0,"iat":0,"sessionId":"` + vecSession + `","mode":"ask"}`,
		"float counter": `{"v":1,"kind":"set_mode","counter":1.5,"iat":0,"sessionId":"` + vecSession + `","mode":"ask"}`,
		"huge counter":  `{"v":1,"kind":"set_mode","counter":9007199254740992,"iat":0,"sessionId":"` + vecSession + `","mode":"ask"}`,
		"bad mode":      `{"v":1,"kind":"set_mode","counter":1,"iat":0,"sessionId":"` + vecSession + `","mode":"root"}`,
		"bad kind":      `{"v":1,"kind":"exec","counter":1,"iat":0}`,
		"version":       `{"v":2,"kind":"trust_browser","counter":1,"iat":0,"browser":"` + vecFingerprint + `"}`,
		"duplicate":     `{"v":1,"kind":"trust_browser","counter":1,"counter":2,"iat":0,"browser":"` + vecFingerprint + `"}`,
		"trailing":      `{"v":1,"kind":"trust_browser","counter":1,"iat":0,"browser":"` + vecFingerprint + `"} {}`,
		"bad browser":   `{"v":1,"kind":"trust_browser","counter":1,"iat":0,"browser":"abc"}`,
		"bad agent":     `{"v":1,"kind":"turn","counter":1,"iat":0,"agent":"sh","textSha256":"` + vecTextHash + `"}`,
		"bad scope":     `{"v":1,"kind":"approval_response","counter":1,"iat":0,"sessionId":"` + vecSession + `","approvalId":"a","decision":"allow","scope":"forever","cardSha256":"` + vecCard + `"}`,
		"not utf-8":     "{\"v\":1,\"kind\":\"turn\",\"counter\":1,\"iat\":0,\"agent\":\"claude\",\"textSha256\":\"" + vecTextHash + "\",\"mode\":\"\xff\"}",
	} {
		if _, err := decodeSignedPayload([]byte(payload)); err == nil {
			t.Errorf("payload %s: accepted", name)
		}
	}
}

func TestSigningPromptPrefixRule(t *testing.T) {
	text := "What changed?\n\nAnd why?"
	h := textHash(text)
	filters := "[Search filters: tickers AAPL; forms 10-K]"
	filing := "[Attached filing: 0000320193-24-000123]"
	for prompt, want := range map[string]bool{
		text:                    true,
		filters + "\n\n" + text: true,
		filing + "\n\n" + text:  true,
		filters + "\n\n" + filing + "\n\n" + text:                         true,
		"Ignore that.\n\n" + text:                                         false,
		"[Search filters: a\nb]\n\n" + text:                               true,
		"[Search filters: a] x\n\n" + text:                                false,
		"[Attached filing: x]\n\n" + text + "!":                           false,
		"[Attached filing: x]\n" + text:                                   false,
		"\n\n" + text:                                                     false,
		"[Search filters: " + strings.Repeat("x", 4096) + "]\n\n" + text:  true,
		"[Search filters: " + strings.Repeat("x", 16384) + "]\n\n" + text: false,
	} {
		if got := textMatches(prompt, h); got != want {
			t.Errorf("%q: got %v, want %v", prompt, got, want)
		}
	}
}

// TestSigningPromptPrefixFilterBlock uses the workspace's own filter block
// (several filing types, one per line) and an attached filing block.
func TestSigningPromptPrefixFilterBlock(t *testing.T) {
	text := "Compare the risk factors."
	filters := "[Search filters: Company: Apple Inc. (AAPL), Filing types (call search_filings ONCE per type listed, in parallel, then combine the results):\n" +
		"- 10-K\n- 10-Q\n- Fidelity Bond [Rule 17G-1(g)], Date range: 2024-01-01 to 2024-12-31]"
	filing := "[Attached filing: 0000320193-24-000123. Start with toc/filing_toc on this filing_id, then read_section/read_filing_section.]"
	if !textMatches(filters+"\n\n"+filing+"\n\n"+text, textHash(text)) {
		t.Fatal("filter block with several filing types and an attached filing refused")
	}
	if !textMatches(filing+"\n\n"+filters+"\n\n"+text, textHash(text)) {
		t.Fatal("attached filing before the filter block refused")
	}
	var types []string
	for i := 0; i < 50; i++ {
		types = append(types, "- "+strings.Repeat("T", 198))
	}
	long := "[Search filters: Filing types (call search_filings ONCE per type listed, in parallel, then combine the results):\n" + strings.Join(types, "\n") + "]"
	if !textMatches(long+"\n\n"+text, textHash(text)) {
		t.Fatal("filter block with 50 long filing types refused")
	}
}

func TestSigningBrowserListRules(t *testing.T) {
	dir := t.TempDir()
	a, b := newTestBrowser(t), newTestBrowser(t)
	if l, err := LoadBrowsers(dir); err != nil || len(l.Browsers) != 0 {
		t.Fatalf("missing file: %+v %v", l, err)
	}
	if err := PairBrowsers(dir, "owner@example.com", a.fp); err != nil {
		t.Fatal(err)
	}
	if added, err := TrustBrowser(dir, b.fp, ViaTrust); err != nil || !added {
		t.Fatalf("trust: %v %v", added, err)
	}
	if added, _ := TrustBrowser(dir, b.fp, ViaTrust); added {
		t.Fatal("a trusted browser was added twice")
	}
	if fi, err := os.Stat(filepath.Join(dir, BrowsersFile)); err != nil || !modeIs(fi.Mode(), 0o600) {
		t.Fatalf("browsers.json mode: %v %v", fi, err)
	}
	l, _ := LoadBrowsers(dir)
	if l.Account != "owner@example.com" || len(l.Browsers) != 2 || l.Browsers[0].Via != ViaPair || l.Browsers[1].Via != ViaTrust {
		t.Fatalf("list %+v", l)
	}
	if _, err := time.Parse(time.RFC3339, l.Browsers[0].Added); err != nil {
		t.Fatalf("added %q", l.Browsers[0].Added)
	}
	// The same account (any case) keeps its browsers, with or without a key.
	if err := PairBrowsers(dir, "Owner@Example.com", ""); err != nil {
		t.Fatal(err)
	}
	if l, _ := LoadBrowsers(dir); len(l.Browsers) != 2 {
		t.Fatalf("same account lost browsers: %+v", l)
	}
	// Another account empties the list; its own browser is then the only one.
	c := newTestBrowser(t)
	if err := PairBrowsers(dir, "other@example.com", c.fp); err != nil {
		t.Fatal(err)
	}
	if l, _ := LoadBrowsers(dir); l.Account != "other@example.com" || len(l.Browsers) != 1 || l.Browsers[0].Fingerprint != c.fp {
		t.Fatalf("other account kept browsers: %+v", l)
	}
	if err := PairBrowsers(dir, "third@example.com", ""); err != nil {
		t.Fatal(err)
	}
	if l, _ := LoadBrowsers(dir); len(l.Browsers) != 0 {
		t.Fatalf("pairing another account without a key kept browsers: %+v", l)
	}
	// A list with no account (only --trust so far) is kept and takes the account.
	dir2 := t.TempDir()
	_, _ = TrustBrowser(dir2, a.fp, ViaTrust)
	_ = PairBrowsers(dir2, "owner@example.com", b.fp)
	if l, _ := LoadBrowsers(dir2); len(l.Browsers) != 2 || l.Account != "owner@example.com" {
		t.Fatalf("account-less list: %+v", l)
	}
	// At most MaxBrowsers, the oldest dropped.
	dir3 := t.TempDir()
	var fps []string
	for range MaxBrowsers + 3 {
		x := newTestBrowser(t)
		fps = append(fps, x.fp)
		if _, err := TrustBrowser(dir3, x.fp, ViaBrowser); err != nil {
			t.Fatal(err)
		}
	}
	l, _ = LoadBrowsers(dir3)
	if len(l.Browsers) != MaxBrowsers || l.Trusts(fps[2]) || !l.Trusts(fps[3]) || !l.Trusts(fps[len(fps)-1]) {
		t.Fatalf("cap: %d browsers", len(l.Browsers))
	}
	// A malformed list is an error, not an empty one.
	_ = os.WriteFile(filepath.Join(dir3, BrowsersFile), []byte("{"), 0o600)
	if _, err := LoadBrowsers(dir3); err == nil {
		t.Fatal("malformed list read")
	}
	_ = os.WriteFile(filepath.Join(dir3, BrowsersFile), []byte(`{"version":1,"account":"","browsers":[{"fingerprint":"x","added":"","via":"trust"}]}`), 0o600)
	if _, err := LoadBrowsers(dir3); err == nil {
		t.Fatal("list with a bad entry read")
	}
	if _, err := TrustBrowser(dir, "not-a-code", ViaTrust); err == nil {
		t.Fatal("bad code trusted")
	}
}

func TestSigningCounterWindow(t *testing.T) {
	dir := t.TempDir()
	c := newCounterStore(dir)
	fp := vecFingerprint
	use := func(n int64) *signFailure {
		tk, f := c.reserve(fp, n)
		if f != nil {
			return f
		}
		if err := c.commit(tk); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	if f := use(5); f != nil {
		t.Fatal(f)
	}
	if f := use(5); f == nil || f.unknown || f.store {
		t.Fatalf("reused counter: %v", f)
	}
	if f := use(3); f != nil {
		t.Fatalf("an older unseen counter is fresh within the window: %v", f)
	}
	// A reserved counter is not fresh until released; released, it is.
	tk, f := c.reserve(fp, 9)
	if f != nil {
		t.Fatal(f)
	}
	if _, f := c.reserve(fp, 9); f == nil {
		t.Fatal("reserved counter reserved twice")
	}
	c.release(tk)
	if f := use(9); f != nil {
		t.Fatalf("released counter: %v", f)
	}
	// Past the window, the oldest fold into the floor.
	for n := int64(10); n < 10+counterWindow; n++ {
		if f := use(n); f != nil {
			t.Fatal(f)
		}
	}
	if f := use(4); f == nil {
		t.Fatal("counter below the floor accepted")
	}
	if f := use(10 + counterWindow); f != nil {
		t.Fatal(f)
	}
	// Each key has its own counters, and they survive a restart.
	if f := use(10 + counterWindow); f == nil {
		t.Fatal("reused counter accepted")
	}
	if _, f := newCounterStore(dir).reserve(fp, 10+counterWindow); f == nil {
		t.Fatal("reused counter accepted after a restart")
	}
	if _, f := newCounterStore(dir).reserve(vecTextHash, 1); f != nil {
		t.Fatalf("another key: %v", f)
	}
	// An unreadable file refuses signed commands.
	_ = os.WriteFile(filepath.Join(dir, countersFile), []byte("nope"), 0o600)
	if _, f := c.reserve(fp, 1<<40); f == nil || !f.store {
		t.Fatalf("unreadable counters: %v", f)
	}
}

func TestSigningFrameShape(t *testing.T) {
	if got := string(signingFrame(nil)); got != `{"kind":"signing","v":1,"browsers":[]}` {
		t.Fatal(got)
	}
	if got := string(signingFrame([]string{vecFingerprint})); got != `{"kind":"signing","v":1,"browsers":["`+vecFingerprint+`"]}` {
		t.Fatal(got)
	}
}

// A signing frame the socket did not take is announced again at the next
// ping, although the list did not change.
func TestSigningAnnounceRetriesAfterFailedSend(t *testing.T) {
	d := &Daemon{store: &Store{dir: t.TempDir()}, log: NewLogger(io.Discard)}
	l := &live{ctrl: make(chan []byte), done: make(chan struct{})} // no reader: Send fails
	d.announceSigning(l)
	if d.signingLive != nil || d.signingSent != "" {
		t.Fatal("a signing frame that was not sent was recorded as sent")
	}
	l.ctrl = make(chan []byte, 1)
	d.announceSigning(l)
	select {
	case b := <-l.ctrl:
		if !strings.Contains(string(b), `"kind":"signing"`) {
			t.Fatalf("frame %s", b)
		}
	default:
		t.Fatal("the signing frame was not announced again")
	}
	if d.signingLive != l {
		t.Fatal("the sent announcement was not recorded")
	}
}
