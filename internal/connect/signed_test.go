package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Story 94.7: commands from the browser are signed. These rows drive a
// user daemon through the fake relay with the harness's trusted browser key
// (newHarness trusts it; the fake relay signs with it) and with commands
// left unsigned or signed by another key.

func (h *harness) stateDir() string { return filepath.Join(h.home, ".archivist", "connect") }

// errorCount counts the session's error chunks with text.
func (h *harness) errorCount(sid, text string) int {
	n := 0
	for _, e := range h.errorTexts(sid) {
		if e == text {
			n++
		}
	}
	return n
}

// allowCmd is the relay's approval_response allow for the n-th card.
func (h *harness) allowCmd(sid string, n int) map[string]any {
	h.t.Helper()
	req := h.waitCardEvent(sid, n)
	payload := req["payload"].(map[string]any)
	return map[string]any{"kind": "approval_response", "correlationId": "resolved:" + req["correlationId"].(string),
		"sessionId": sid, "approvalId": payload["approvalId"], "decision": "allow", "reason": "user"}
}

func (h *harness) waitCardEvent(sid string, n int) map[string]any {
	h.t.Helper()
	var req map[string]any
	waitFor(h.t, 20*time.Second, "approval request", func() bool {
		reqs := h.relay.eventsOf(sid, "tool-approval-request")
		if len(reqs) >= n {
			req = reqs[n-1]
			return true
		}
		return false
	})
	return req
}

func cloneCmd(cmd map[string]any) map[string]any {
	out := make(map[string]any, len(cmd))
	for k, v := range cmd {
		out[k] = v
	}
	return out
}

// An allowing answer runs only when the trusted browser signed it for this
// card: unsigned, signed by another key, signed for another card, scope,
// approval, session or decision, or with a counter already used, it is
// acknowledged, not run, reported, and the tool call is denied (the relay
// has already recorded the answer, so no timeout deny would follow).
func TestSignedApprovalAllow(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("bash touch a.txt")
	stranger := newTestBrowser(t)
	fields := func(cmd map[string]any, card string) map[string]any {
		return map[string]any{"sessionId": cmd["sessionId"], "approvalId": cmd["approvalId"], "decision": "allow", "scope": "",
			"cardSha256": card}
	}
	with := func(f map[string]any, k string, v any) map[string]any { f[k] = v; return f }
	used := h.browser.nextCounter() // the counter of the allow that runs below
	cases := []struct {
		name string
		edit func(cmd map[string]any, card string)
		deny string
	}{
		{"unsigned", func(map[string]any, string) {}, denyUnknownMessage},
		{"another key", func(c map[string]any, card string) {
			c["sig"] = stranger.sign("approval_response", fields(c, card))
		}, denyUnknownMessage},
		{"another card", func(c map[string]any, card string) {
			c["sig"] = h.browser.sign("approval_response", with(fields(c, card), "cardSha256", textHash("another card")))
		}, denyMismatchMessage},
		{"another scope", func(c map[string]any, card string) {
			c["scope"] = "allow_always" // signed for allow_once ("")
			c["sig"] = h.browser.sign("approval_response", fields(c, card))
		}, denyMismatchMessage},
		{"another approval", func(c map[string]any, card string) {
			c["sig"] = h.browser.sign("approval_response", with(fields(c, card), "approvalId", "another-approval"))
		}, denyMismatchMessage},
		{"another session", func(c map[string]any, card string) {
			c["sig"] = h.browser.sign("approval_response", with(fields(c, card), "sessionId", randomUUID()))
		}, denyMismatchMessage},
		{"another decision", func(c map[string]any, card string) {
			c["sig"] = h.browser.sign("approval_response", with(fields(c, card), "decision", "deny"))
		}, denyMismatchMessage},
	}
	n := 0
	next := func() (map[string]any, string) {
		if n > 0 {
			h.message(sid, "bash touch a.txt")
		}
		n++
		allow := h.allowCmd(sid, n)
		card := h.relay.cardHash(sid, allow["approvalId"].(string))
		if card == "" {
			t.Fatal("no card digest")
		}
		return allow, card
	}
	denied := map[string]int{}
	for _, c := range cases {
		allow, card := next()
		c.edit(allow, card)
		h.relay.sendUnsigned(sid, allow)
		waitFor(t, 10*time.Second, "ack "+c.name, func() bool { return h.relay.acked(allow["correlationId"].(string)) })
		h.waitFinishes(sid, n)
		denied[c.deny]++
		if got := strings.Count(h.relay.text(sid), "denied: "+c.deny); got != denied[c.deny] {
			t.Fatalf("%s: tool call not denied with %q: %q", c.name, c.deny, h.relay.text(sid))
		}
	}
	if h.toolOutput(sid, "ran: touch a.txt") {
		t.Fatal("an answer that was not signed for its card ran the tool")
	}
	if h.errorCount(sid, signUnknownMessage) != 2 || h.errorCount(sid, signMismatchMessage) != 5 {
		t.Fatalf("refusals reported: %v", h.errorTexts(sid))
	}
	// Signed for its card, the allow runs.
	allow, card := next()
	allow["sig"] = h.browser.signWithCounter("approval_response", used, fields(allow, card))
	h.relay.sendUnsigned(sid, allow)
	h.waitFinishes(sid, n)
	if !h.toolOutput(sid, "ran: touch a.txt") {
		t.Fatalf("signed allow did not run: %q", h.relay.text(sid))
	}
	// The same counter on the next card is refused, and that call denied.
	allow, card = next()
	allow["sig"] = h.browser.signWithCounter("approval_response", used, fields(allow, card))
	h.relay.sendUnsigned(sid, allow)
	h.waitFinishes(sid, n)
	if strings.Count(h.relay.text(sid), "denied: "+denyMismatchMessage) != denied[denyMismatchMessage]+1 ||
		h.errorCount(sid, signMismatchMessage) != 6 {
		t.Fatalf("reused counter: %q, errors %v", h.relay.text(sid), h.errorTexts(sid))
	}
	if !strings.Contains(h.log.String(), "was already used") {
		t.Fatalf("reused counter not logged:\n%s", h.log.String())
	}
	if n := len(h.relay.eventsOf(sid, "data-permission-scope")); n != 0 {
		t.Fatalf("a refused allow kept a session rule: %d scope events", n)
	}
}

// A message runs only when signed for this session and text; a signed
// message cannot be sent again with another correlation id.
func TestSignedUserMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	other := newTestBrowser(t)
	refused := []map[string]any{
		{"text": "echo unsigned"},
		{"text": "echo stranger", "sig": other.sign("turn", map[string]any{"agent": "claude", "sessionId": sid, "textSha256": textHash("echo stranger")})},
		{"text": "echo changed", "sig": h.browser.sign("turn", map[string]any{"agent": "claude", "sessionId": sid, "textSha256": textHash("echo original")})},
		{"text": "echo elsewhere", "sig": h.browser.sign("turn", map[string]any{"agent": "claude", "sessionId": randomUUID(), "textSha256": textHash("echo elsewhere")})},
		{"text": "echo codex", "sig": h.browser.sign("turn", map[string]any{"agent": "codex", "sessionId": sid, "textSha256": textHash("echo codex")})},
		{"text": "echo nosession", "sig": h.browser.sign("turn", map[string]any{"agent": "claude", "textSha256": textHash("echo nosession")})},
		{"text": "echo mode", "sig": h.browser.sign("set_mode", map[string]any{"sessionId": sid, "mode": "ask"})},
	}
	for i, r := range refused {
		cid := "msg-refused-" + string(rune('a'+i))
		cmd := map[string]any{"kind": "user_message", "correlationId": cid, "sessionId": sid, "text": r["text"]}
		if r["sig"] != nil {
			cmd["sig"] = r["sig"]
		}
		h.relay.sendUnsigned(sid, cmd)
		waitFor(t, 10*time.Second, "ack "+cid, func() bool { return h.relay.acked(cid) })
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(h.relay.eventsOf(sid, "finish")); n != 1 {
		t.Fatalf("refused messages ran %d turns: %q", n-1, h.relay.text(sid))
	}
	if h.errorCount(sid, signUnknownMessage) != 2 || h.errorCount(sid, signMismatchMessage) != 5 {
		t.Fatalf("refusals reported: %v", h.errorTexts(sid))
	}
	if h.record(sid).HasHandled("msg-refused-a") {
		t.Fatal("a refused message was recorded as handled")
	}
	// Signed as a turn and as a sign in code, it runs.
	cmd := map[string]any{"kind": "user_message", "correlationId": "msg-signed", "sessionId": sid, "text": "echo two"}
	h.relay.send(sid, cmd)
	h.waitFinishes(sid, 2)
	code := map[string]any{"kind": "user_message", "correlationId": "msg-code", "sessionId": sid, "text": "echo three",
		"sig": h.browser.sign("user_message", map[string]any{"sessionId": sid, "textSha256": textHash("echo three")})}
	h.relay.sendUnsigned(sid, code)
	h.waitFinishes(sid, 3)
	if !strings.Contains(h.relay.text(sid), "two") || !strings.Contains(h.relay.text(sid), "three") {
		t.Fatalf("signed messages did not run: %q", h.relay.text(sid))
	}
	// The same signature again (a new correlation id) is refused; the same
	// command again (its correlation id) is a duplicate, acknowledged only.
	replay := cloneCmd(cmd)
	replay["correlationId"] = "msg-replay"
	h.relay.sendUnsigned(sid, replay)
	h.relay.sendUnsigned(sid, cmd)
	waitFor(t, 10*time.Second, "replay ack", func() bool { return h.relay.acked("msg-replay") })
	time.Sleep(300 * time.Millisecond)
	if n := len(h.relay.eventsOf(sid, "finish")); n != 3 {
		t.Fatalf("a replayed message ran: %d turns", n)
	}
	if !strings.Contains(h.log.String(), "was already used") {
		t.Fatalf("replay not logged:\n%s", h.log.String())
	}
}

// Stopping, interrupting and denying run unsigned.
func TestSignedDenyStopInterruptUnsigned(t *testing.T) {
	h := newHarness(t, nil)
	h.relayUnsigned = true
	h.start()
	// The start itself must be signed.
	s := h.api.addSession("claude")
	start := map[string]any{"kind": "start_session", "correlationId": "start-1", "sessionId": s.SessionID, "agent": "claude",
		"prompt": "bash rm x"}
	start["sig"] = h.browser.sign("turn", map[string]any{"agent": "claude", "textSha256": textHash("bash rm x")})
	h.relay.sendUnsigned("", start)
	sid := s.SessionID
	h.approve(sid, 1, "deny", "user", "")
	h.waitFinishes(sid, 1)
	if !strings.Contains(h.relay.text(sid), "denied: ") {
		t.Fatalf("unsigned deny did not run: %q", h.relay.text(sid))
	}
	msg := map[string]any{"kind": "user_message", "correlationId": "msg-slow", "sessionId": sid, "text": "slow",
		"sig": h.browser.sign("turn", map[string]any{"agent": "claude", "sessionId": sid, "textSha256": textHash("slow")})}
	h.relay.sendUnsigned(sid, msg)
	waitFor(t, 15*time.Second, "slow output", func() bool { return strings.Contains(h.relay.text(sid), "3\n") })
	h.command(sid, "interrupt")
	h.waitFinishes(sid, 2)
	stop := map[string]any{"kind": "stop_session", "correlationId": "stop-1", "sessionId": sid, "sig": map[string]any{"v": 1}}
	h.relay.sendUnsigned(sid, stop) // a stop runs whatever it carries
	h.relay.waitStatus(t, sid, "completed", 1)
	if len(h.errorTexts(sid)) != 0 {
		t.Fatalf("errors: %v", h.errorTexts(sid))
	}
}

// A start runs only when signed for its agent and prompt; the workspace's
// context blocks may precede the signed text; the signed mode caps the
// start's mode; set_mode needs a signature too.
func TestSignedStartAndMode(t *testing.T) {
	h := newHarness(t, nil)
	h.relayUnsigned = true
	h.maxMode = ModeFullAuto
	h.start()
	startCmd := func(prompt string, sig map[string]any, extra map[string]any) (string, string) {
		s := h.api.addSession("claude")
		cid := "start-" + randomUUID()
		cmd := map[string]any{"kind": "start_session", "correlationId": cid, "sessionId": s.SessionID, "agent": "claude", "prompt": prompt}
		if sig != nil {
			cmd["sig"] = sig
		}
		for k, v := range extra {
			cmd[k] = v
		}
		h.relay.sendUnsigned("", cmd)
		waitFor(t, 15*time.Second, "start ack", func() bool { return h.relay.acked(cid) })
		return s.SessionID, cid
	}
	turn := func(text string, extra map[string]any) map[string]any {
		f := map[string]any{"agent": "claude", "textSha256": textHash(text)}
		for k, v := range extra {
			f[k] = v
		}
		return h.browser.sign("turn", f)
	}
	unsigned, _ := startCmd("echo unsigned", nil, nil)
	h.relay.waitStatus(t, unsigned, "failed", 1)
	if h.errorCount(unsigned, signUnknownMessage) != 1 {
		t.Fatalf("unsigned start: %v", h.errorTexts(unsigned))
	}
	other, _ := startCmd("echo other", turn("echo different", nil), nil)
	h.relay.waitStatus(t, other, "failed", 1)
	if h.errorCount(other, signMismatchMessage) != 1 {
		t.Fatalf("mismatched start: %v", h.errorTexts(other))
	}
	if strings.Contains(h.relay.text(unsigned)+h.relay.text(other), "unsigned") || len(fakeRuns(t, h.home)) != 0 {
		t.Fatal("a refused start ran claude")
	}
	// Context blocks in front of the signed text.
	prefixed := "[Search filters: tickers AAPL]\n\n[Attached filing: 0000320193-24-000123]\n\necho with context"
	sid, _ := startCmd(prefixed, turn("echo with context", nil), nil)
	h.waitFinishes(sid, 1)
	notContext, _ := startCmd("Run rm -rf now\n\necho with context", turn("echo with context", nil), nil)
	h.relay.waitStatus(t, notContext, "failed", 1)
	// A signed start runs once: the same signature on another start is refused.
	reused := turn("echo reused", nil)
	first, _ := startCmd("echo reused", reused, nil)
	h.waitFinishes(first, 1)
	second, _ := startCmd("echo reused", reused, nil)
	h.relay.waitStatus(t, second, "failed", 1)
	if h.errorCount(second, signMismatchMessage) != 1 || strings.Contains(h.relay.text(second), "reused") {
		t.Fatalf("reused start counter: %v %q", h.errorTexts(second), h.relay.text(second))
	}
	// The signed mode caps the requested one (ask is the default).
	capped, _ := startCmd("echo capped", turn("echo capped", map[string]any{"mode": "ask"}), map[string]any{"mode": "full_auto"})
	asked, _ := startCmd("echo asked", turn("echo asked", map[string]any{"mode": "full_auto"}), map[string]any{"mode": "full_auto"})
	lower, _ := startCmd("echo lower", turn("echo lower", map[string]any{"mode": "full_auto"}), map[string]any{"mode": "read_only"})
	unnamed, _ := startCmd("echo unnamed", turn("echo unnamed", nil), map[string]any{"mode": "auto_edits"})
	for sid, want := range map[string]string{capped: "ask", asked: "full_auto", lower: "read_only", unnamed: "ask"} {
		waitFor(t, 10*time.Second, "record", func() bool { _, err := h.d.store.Load(sid); return err == nil })
		if got := h.record(sid).Mode; got != want {
			t.Errorf("start mode %s, want %s", got, want)
		}
	}
	// set_mode: unsigned refused, signed applied.
	h.waitFinishes(capped, 1)
	h.relay.sendUnsigned(capped, map[string]any{"kind": "set_mode", "correlationId": "mode-1", "sessionId": capped, "mode": "read_only"})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("mode-1") })
	time.Sleep(200 * time.Millisecond)
	if h.record(capped).Mode != "ask" || h.errorCount(capped, signUnknownMessage) != 1 {
		t.Fatalf("unsigned set_mode: mode %s, errors %v", h.record(capped).Mode, h.errorTexts(capped))
	}
	h.relay.sendUnsigned(capped, map[string]any{"kind": "set_mode", "correlationId": "mode-2", "sessionId": capped, "mode": "read_only",
		"sig": h.browser.sign("set_mode", map[string]any{"sessionId": capped, "mode": "full_auto"})})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("mode-2") })
	h.relay.sendUnsigned(capped, map[string]any{"kind": "set_mode", "correlationId": "mode-3", "sessionId": capped, "mode": "read_only",
		"sig": h.browser.sign("set_mode", map[string]any{"sessionId": capped, "mode": "read_only"})})
	waitFor(t, 10*time.Second, "read_only committed", func() bool { return h.record(capped).Mode == "read_only" })
	if h.errorCount(capped, signMismatchMessage) != 1 {
		t.Fatalf("mismatched set_mode: %v", h.errorTexts(capped))
	}
}

// The daemon announces its trusted browsers before the capabilities, again
// when the list changes, and a trusted browser can add another.
func TestSignedTrustBrowser(t *testing.T) {
	setDuration(t, &pingInterval, 200*time.Millisecond)
	h := newHarness(t, nil)
	h.start()
	first := h.relay.lastSigning()
	if first == nil || !slices.Equal(anyStrings(first["browsers"]), []string{h.browser.fp}) || first["v"] != 1.0 {
		t.Fatalf("signing frame %v", first)
	}
	if order := h.relay.userFrameKinds(); len(order) < 2 || order[0] != "signing" || order[1] != "capabilities" {
		t.Fatalf("user socket frames %v", order)
	}
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	b := newTestBrowser(t)
	asB := func(cid, text string) {
		h.relay.sendUnsigned(sid, map[string]any{"kind": "user_message", "correlationId": cid, "sessionId": sid, "text": text,
			"sig": b.sign("turn", map[string]any{"agent": "claude", "sessionId": sid, "textSha256": textHash(text)})})
		waitFor(t, 10*time.Second, "ack "+cid, func() bool { return h.relay.acked(cid) })
	}
	asB("b-1", "echo before")
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(h.relay.text(sid), "before") {
		t.Fatal("an unknown browser's message ran")
	}
	// An unknown browser cannot add itself.
	h.relay.sendUnsigned("", map[string]any{"kind": "trust_browser", "correlationId": "trust-self", "browser": b.fp,
		"sig": b.sign("trust_browser", map[string]any{"browser": b.fp})})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("trust-self") })
	if l, _ := LoadBrowsers(h.stateDir()); l.Trusts(b.fp) {
		t.Fatal("an unknown browser added itself")
	}
	// Signed for another browser than the one named: refused.
	h.relay.sendUnsigned("", map[string]any{"kind": "trust_browser", "correlationId": "trust-other", "browser": b.fp,
		"sig": h.browser.sign("trust_browser", map[string]any{"browser": newTestBrowser(t).fp})})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("trust-other") })
	if l, _ := LoadBrowsers(h.stateDir()); l.Trusts(b.fp) {
		t.Fatal("a mismatched trust_browser ran")
	}
	// The trusted browser adds b.
	h.relay.sendUnsigned("", map[string]any{"kind": "trust_browser", "correlationId": "trust-b", "browser": b.fp,
		"sig": h.browser.sign("trust_browser", map[string]any{"browser": b.fp})})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("trust-b") })
	l, _ := LoadBrowsers(h.stateDir())
	if !l.Trusts(b.fp) || l.Browsers[len(l.Browsers)-1].Via != ViaBrowser {
		t.Fatalf("list %+v", l)
	}
	waitFor(t, 5*time.Second, "re-announced", func() bool {
		return slices.Equal(anyStrings(h.relay.lastSigning()["browsers"]), []string{h.browser.fp, b.fp})
	})
	asB("b-2", "echo after")
	h.waitFinishes(sid, 2)
	// --trust from the terminal: announced at the next ping.
	c := newTestBrowser(t)
	if _, err := TrustBrowser(h.stateDir(), c.fp, ViaTrust); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "announced at a ping", func() bool {
		return slices.Equal(anyStrings(h.relay.lastSigning()["browsers"]), []string{h.browser.fp, b.fp, c.fp})
	})
	// A browser already trusted is acknowledged only.
	h.relay.sendUnsigned("", map[string]any{"kind": "trust_browser", "correlationId": "trust-again", "browser": c.fp,
		"sig": map[string]any{"v": 1}})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("trust-again") })
}

// An unreadable trusted list announces none and refuses signed commands.
func TestSignedUnreadableBrowserList(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	if err := os.WriteFile(filepath.Join(h.stateDir(), BrowsersFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.message(sid, "echo two")
	waitFor(t, 10*time.Second, "refusal", func() bool { return h.errorCount(sid, signStoreMessage) == 1 })
	if err := os.WriteFile(filepath.Join(h.stateDir(), countersFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.browser.trust(t, t.TempDir()) // no effect on this daemon
	_ = os.Remove(filepath.Join(h.stateDir(), BrowsersFile))
	h.browser.trust(t, h.stateDir())
	h.message(sid, "echo three")
	waitFor(t, 10*time.Second, "refusal", func() bool { return h.errorCount(sid, signStoreMessage) == 2 })
	if n := len(h.relay.eventsOf(sid, "finish")); n != 1 {
		t.Fatalf("%d turns", n)
	}
}

// The session-bound sandbox mode never checks signatures.
func TestSignedSandboxExempt(t *testing.T) {
	h := newHarness(t, nil)
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "", "bash touch a.txt")
	h.approve(s.SessionID, 1, "allow", "user", "")
	h.message(s.SessionID, "echo unsigned turn")
	waitFor(t, 30*time.Second, "unsigned turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "unsigned turn") })
	if !h.toolOutput(s.SessionID, "ran: touch a.txt") {
		t.Fatal("unsigned allow did not run in the sandbox")
	}
	r.stop()
	if _, err := os.Stat(filepath.Join(h.stateDir(), countersFile)); !os.IsNotExist(err) {
		t.Fatal("the sandbox recorded counters")
	}
}

// A relay that predates signing refuses the signing frame (INVALID_FRAME):
// swallowed once, logged once, and commands keep working.
func TestSignedOlderRelay(t *testing.T) {
	h := newHarness(t, nil)
	h.relay.set(func(r *fakeRelay) { r.oldRelay = true })
	h.start()
	waitFor(t, 10*time.Second, "refusals logged", func() bool {
		return strings.Contains(h.log.String(), "refused the signing frame") && strings.Contains(h.log.String(), "refused the controls frame")
	})
	sid := h.startSession("echo still works")
	h.waitFinishes(sid, 1)
	if strings.Contains(h.log.String(), "relay error INVALID_FRAME") {
		t.Fatalf("a refusal took the error path:\n%s", h.log.String())
	}
	h.relay.closeUser(4999)
	waitFor(t, 15*time.Second, "second capabilities", func() bool { return h.relay.capsCount() >= 2 })
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(h.log.String(), "refused the signing frame"); n != 1 {
		t.Fatalf("refusal logged %d times", n)
	}
}

func anyStrings(v any) []string {
	var out []string
	raw, _ := json.Marshal(v)
	_ = json.Unmarshal(raw, &out)
	return out
}

// Codex cards carry their digest too: an unsigned allow is refused and the
// request declined like a deny; the signed one is accepted.
func TestSignedCodexApproval(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("file notes.txt")
	unsigned := h.allowCmd(sid, 1)
	h.relay.sendUnsigned(sid, unsigned)
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked(unsigned["correlationId"].(string)) })
	h.waitFinishes(sid, 1)
	resp := h.relay.eventsOf(sid, "tool-approval-response")
	if len(resp) != 1 || resp[0]["payload"].(map[string]any)["approved"] != false ||
		!strings.Contains(h.relay.text(sid), "file declined") || h.errorCount(sid, signUnknownMessage) != 1 {
		t.Fatalf("unsigned codex allow: responses %v, text %q, errors %v", resp, h.relay.text(sid), h.errorTexts(sid))
	}
	if _, err := os.Stat(filepath.Join(h.record(sid).Cwd, "notes.txt")); err == nil {
		t.Fatal("an unsigned codex allow wrote the file")
	}
	h.message(sid, "file notes.txt")
	h.relay.send(sid, h.allowCmd(sid, 2))
	h.waitFinishes(sid, 2)
	if _, err := os.Stat(filepath.Join(h.record(sid).Cwd, "notes.txt")); err != nil {
		t.Fatalf("signed codex allow did not run: %v", err)
	}
}
