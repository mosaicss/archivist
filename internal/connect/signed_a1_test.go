package connect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Story 94.7 Amendment A1 rows: signed context, the computer id, the
// signing query flag and filing source links in approval cards.

// A turn whose filter and attached filing blocks the browser signed runs,
// at the start and as a message, with the whole prompt reaching Claude.
// The daemon dials with the signing flag.
func TestSignedTurnWithContext(t *testing.T) {
	context := "[Attached filing: 0b8f3f5e-8a3c-4c39-9d1a-2f5d7a1e6b10. Start with toc/filing_toc on this filing_id, then read_section/read_filing_section.]\n\n" +
		"[Search filters: Company: Apple Inc. (AAPL), Filing types (call search_filings ONCE per type listed, in parallel, then combine the results):\n- 10-K\n- 8-K]\n\n"
	h := newHarness(t, nil)
	h.relay.set(func(r *fakeRelay) { r.context = context })
	h.start()
	sid := h.startSession(context + "what changed")
	h.waitFinishes(sid, 1)
	h.message(sid, context+"and why")
	h.waitFinishes(sid, 2)
	text := h.relay.text(sid)
	if !strings.Contains(text, "unknown scenario: "+context+"what changed") || !strings.Contains(text, "unknown scenario: "+context+"and why") {
		t.Fatalf("signed context turns did not run: %q", text)
	}
	if len(h.errorTexts(sid)) != 0 {
		t.Fatalf("errors: %v", h.errorTexts(sid))
	}
	h.relay.mu.Lock()
	queries := append([]string(nil), h.relay.queries...)
	h.relay.mu.Unlock()
	if len(queries) < 2 {
		t.Fatalf("dials %v", queries)
	}
	for _, q := range queries {
		if q != "role=daemon&signing=1" {
			t.Fatalf("dial query %q", q)
		}
	}
}

// A relay that does not know the signing flag answers the upgrade HTTP
// 400: each socket dials again without it at once, logs that once and
// keeps it off for the rest of its life (a reconnect included).
func TestSignedRelayWithoutSigningFlag(t *testing.T) {
	h := newHarness(t, nil)
	h.relay.set(func(r *fakeRelay) { r.noSigningFlag = true })
	h.start()
	sid := h.startSession("echo still works")
	h.waitFinishes(sid, 1)
	if !strings.Contains(h.relay.text(sid), "still works") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	h.relay.closeUser(4999)
	waitFor(t, 15*time.Second, "second capabilities", func() bool { return h.relay.capsCount() >= 2 })
	h.relay.mu.Lock()
	queries := append([]string(nil), h.relay.queries...)
	h.relay.mu.Unlock()
	flagged, plain := 0, 0
	for _, q := range queries {
		switch q {
		case "role=daemon&signing=1":
			flagged++
		case "role=daemon":
			plain++
		default:
			t.Fatalf("dial query %q", q)
		}
	}
	// One refused flagged dial per socket (user and session), then plain.
	if flagged != 2 || plain < 3 {
		t.Fatalf("dials %v", queries)
	}
	log := h.log.String()
	if strings.Count(log, "user socket: the relay refused the signing flag") != 1 ||
		strings.Count(log, "session socket: the relay refused the signing flag") != 1 {
		t.Fatalf("refusal not logged once per socket:\n%s", log)
	}
}

// A Bash call that names a filing source URL: the card shows the command
// with the link removed, the browser signs that card, and on allow Claude
// runs its own command.
func TestSignedFilingLinkCard(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	command := "curl -s https://www.sec.gov/Archives/edgar/data/320193/000032019324000123/aapl-20240928.htm"
	sid := h.startSession("bash " + command)
	req := h.waitCardEvent(sid, 1)
	d := req["payload"].(map[string]any)["approvalDescriptor"].(map[string]any)
	if got := d["input"].(map[string]any)["command"]; got != "curl -s (source link removed)" {
		t.Fatalf("card command %q", got)
	}
	if raw := h.relay.framesOf(req["correlationId"].(string)); len(raw) == 0 || strings.Contains(strings.Join(raw, ""), "sec.gov") {
		t.Fatalf("the card frame names the filing source: %v", raw)
	}
	h.relay.send(sid, h.allowCmd(sid, 1)) // signed over the card as received
	h.waitFinishes(sid, 1)
	if !h.toolOutput(sid, "ran: "+command) {
		t.Fatalf("the allowed call did not run its own command: %q, errors %v", h.relay.text(sid), h.errorTexts(sid))
	}
	if len(h.errorTexts(sid)) != 0 {
		t.Fatalf("errors: %v", h.errorTexts(sid))
	}
}

// The same for a Codex command card.
func TestSignedCodexFilingLinkCard(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	command := "curl -s https://www.sedarplus.ca/csa-party/records/document.html?id=1"
	sid := h.startCodexSession("cmd " + command)
	req := h.waitCardEvent(sid, 1)
	d := req["payload"].(map[string]any)["approvalDescriptor"].(map[string]any)
	if got := d["command"]; got != "curl -s (source link removed)" {
		t.Fatalf("card command %q", got)
	}
	h.relay.send(sid, h.allowCmd(sid, 1))
	h.waitFinishes(sid, 1)
	if !strings.Contains(h.relay.text(sid), "ran "+command) || len(h.errorTexts(sid)) != 0 {
		t.Fatalf("the allowed command did not run: %q, errors %v", h.relay.text(sid), h.errorTexts(sid))
	}
}

// browsers.json written before Amendment A1 (no computer) gets an id the
// first time it is written or needed, and its browsers keep working.
func TestSignedComputerIDAddedToOldList(t *testing.T) {
	h := newHarness(t, nil)
	h.browser = newTestBrowser(t)
	dir := filepath.Join(h.home, ".archivist", "connect")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"version":1,"account":"","browsers":[{"fingerprint":"` + h.browser.fp + `","added":"2026-10-01T00:00:00Z","via":"pair"}]}`
	if err := os.WriteFile(filepath.Join(dir, BrowsersFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start() // trusts h.browser again: already listed
	l, _ := LoadBrowsers(dir)
	if !ValidComputerID(l.Computer) || h.relay.lastSigning()["computer"] != l.Computer || !l.Trusts(h.browser.fp) {
		t.Fatalf("list %+v, frame %v", l, h.relay.lastSigning())
	}
	sid := h.startSession("echo ok")
	h.waitFinishes(sid, 1)
	if !strings.Contains(h.relay.text(sid), "ok") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
}
