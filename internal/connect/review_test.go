//go:build !windows

package connect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the 78.16 review round: durability of handled ids, slot
// reservation, failure settlement, token lifecycle, startup sweep, frame
// guards and reconnect behaviour. All run in plain go test -race.

func (h *harness) store() *Store {
	h.t.Helper()
	return h.d.store
}

// A failed save must not remember the command id: the command stays unacked,
// and the relay's redelivery runs it.
func TestReviewHandledIDOnlyAfterDurableSave(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	st := h.store()
	st.mu.Lock()
	st.failSaves = 1
	st.mu.Unlock()
	cid := "msg-durable"
	raw := `{"kind":"user_message","correlationId":"` + cid + `","sessionId":"` + sid + `","text":"echo two"}`
	h.relay.sendRaw(sid, raw)
	time.Sleep(500 * time.Millisecond)
	if h.relay.acked(cid) || len(h.relay.eventsOf(sid, "finish")) != 1 {
		t.Fatal("a command whose record was not saved was acked or run")
	}
	if h.record(sid).HasHandled(cid) {
		t.Fatal("unsaved id on disk")
	}
	h.relay.sendRaw(sid, raw) // redelivery
	h.waitFinishes(sid, 2)
	waitFor(t, 5*time.Second, "ack after durable save", func() bool { return h.relay.acked(cid) })
	if !strings.Contains(h.relay.text(sid), "two") {
		t.Fatal("redelivered command did not run")
	}
}

// Concurrent starts cannot exceed the live-process cap.
func TestReviewConcurrentStartsRespectCap(t *testing.T) {
	h := newHarness(t, nil)
	h.max = 2
	h.start()
	var sids []string
	var cids []string
	for i := 0; i < 5; i++ {
		s := h.api.addSession("claude")
		sids = append(sids, s.SessionID)
		cids = append(cids, fmt.Sprintf("start-c-%d", i))
	}
	var wg sync.WaitGroup
	for i := range sids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.relay.send("", map[string]any{"kind": "start_session", "correlationId": cids[i], "sessionId": sids[i], "agent": "claude", "prompt": "slow"})
		}(i)
	}
	wg.Wait()
	for _, cid := range cids {
		waitFor(t, 15*time.Second, "ack "+cid, func() bool { return h.relay.acked(cid) })
	}
	failed := 0
	for _, sid := range sids {
		waitFor(t, 15*time.Second, "outcome", func() bool {
			st := h.relay.statuses(sid)
			return len(st) > 0 && (st[len(st)-1] == "failed" || st[len(st)-1] == "running")
		})
		if st := h.relay.statuses(sid); st[len(st)-1] == "failed" {
			failed++
		}
	}
	if runs := len(fakeRuns(t, h.home)); runs != 2 || failed != 3 || h.d.slotsInUse() != 2 {
		t.Fatalf("runs %d, refused %d, slots %d", runs, failed, h.d.slotsInUse())
	}
	// A stopped session frees its slot; slots never go negative.
	for _, sid := range sids {
		h.command(sid, "stop_session")
	}
	waitFor(t, 20*time.Second, "slots released", func() bool { return h.d.slotsInUse() == 0 })
}

// A fresh start failing before any Claude session id exists fails the
// session for good: record failed, token revoked, slot released, and later
// messages refused with "start a new session".
func TestReviewFreshStartFailureFailsSession(t *testing.T) {
	h := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiKeySource": "none",
		"permissionMode": "default", "exitBeforeInit": true})
	h.start()
	sid := h.startSession("echo never")
	h.relay.waitStatus(t, sid, "failed", 1)
	waitFor(t, 10*time.Second, "record failed", func() bool { return h.record(sid).Status == "failed" })
	st := h.relay.statuses(sid)
	if msg := h.relay.eventsOf(sid, "data-session-status"); !strings.Contains(fmt.Sprint(msg[len(msg)-1]["payload"]), "Start a new session") {
		t.Fatalf("statuses %v / %v", st, msg[len(msg)-1])
	}
	waitFor(t, 10*time.Second, "token revoked, slot free", func() bool { return len(h.api.liveTokens()) == 0 && h.d.slotsInUse() == 0 })
	h.message(sid, "echo again")
	h.relay.waitStatus(t, sid, "failed", 2)
	if len(fakeRuns(t, h.home)) != 1 {
		t.Fatal("a failed session started Claude again")
	}

	// A mint failure at start is settled the same way.
	h2 := newHarness(t, nil)
	h2.api.set(func(f *fakeChatAPI) { f.failMints = 100 })
	h2.start()
	sid2 := h2.startSession("echo never")
	h2.relay.waitStatus(t, sid2, "failed", 1)
	waitFor(t, 10*time.Second, "record failed", func() bool { return h2.record(sid2).Status == "failed" })
	if len(fakeRuns(t, h2.home)) != 0 || h2.d.slotsInUse() != 0 {
		t.Fatal("mint failure started claude or kept the slot")
	}
}

// (c) Claude exiting mid-turn: error + failed, token revoked while no
// process exists, the next message resumes with --resume=<id>.
func TestReviewClaudeExitMidTurnResumes(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	claudeID := h.record(sid).ClaudeSessionID
	h.message(sid, "exit")
	h.relay.waitStatus(t, sid, "failed", 1)
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "exited before the turn finished") {
		t.Fatalf("errors %v", errs)
	}
	waitFor(t, 10*time.Second, "token revoked without a process", func() bool { return len(h.api.liveTokens()) == 0 })
	if h.record(sid).Status != "active" || h.d.slotsInUse() != 0 {
		t.Fatal("a resumable session was failed or kept its slot")
	}
	h.message(sid, "echo back again")
	waitFor(t, 20*time.Second, "resumed turn", func() bool { return strings.Contains(h.relay.text(sid), "back again") })
	var resumed bool
	for _, r := range fakeRuns(t, h.home) {
		for _, a := range r.Args {
			resumed = resumed || a == "--resume="+claudeID
		}
	}
	if !resumed || len(h.api.liveTokens()) != 1 {
		t.Fatalf("resume %v, live tokens %d", resumed, len(h.api.liveTokens()))
	}
}

// The interrupt timeout also revokes the token (no process, no refresh).
func TestReviewInterruptTimeoutRevokesToken(t *testing.T) {
	setDuration(t, &interruptTimeout, 300*time.Millisecond)
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	h.message(sid, "stuck")
	waitFor(t, 10*time.Second, "stuck output", func() bool { return strings.Contains(h.relay.text(sid), "2\n") })
	h.command(sid, "interrupt")
	h.relay.waitStatus(t, sid, "failed", 1)
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
}

// A stdout line over maxLine ends the process so the session settles.
func TestReviewOversizedLineTerminatesClaude(t *testing.T) {
	old := maxLine
	maxLine = 4096
	t.Cleanup(func() { maxLine = old })
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	pid := fakeRuns(t, h.home)[0].PID
	h.message(sid, "big 20000")
	h.relay.waitStatus(t, sid, "failed", 1)
	waitFor(t, 10*time.Second, "claude gone", func() bool { return !processAlive(pid) })
	if !strings.Contains(h.log.String(), "harness stdout reader stopped") {
		t.Fatal("reader error not logged")
	}
}

// Refresh: a malformed token is revoked; a token whose revoke fails stays
// tracked and is revoked at exit.
func TestReviewRefreshRejectsMalformedAndTracksFailedRevoke(t *testing.T) {
	setDuration(t, &tokenRetryBase, 200*time.Millisecond)
	h := newHarness(t, nil)
	h.api.tokenTTL = 3 * time.Second
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	before := h.api.tokenIDs()
	h.api.set(func(f *fakeChatAPI) { f.badTokens = 1; f.failRevokes = 2 }) // the client retries a 503 DELETE once
	var badID string
	waitFor(t, 15*time.Second, "malformed mint", func() bool {
		for id, tok := range h.api.tokenIDs() {
			if _, old := before[id]; !old && strings.HasPrefix(tok, "badtoken_") {
				badID = id
				return true
			}
		}
		return false
	})
	waitFor(t, 5*time.Second, "malformed token tracked after a failed revoke", func() bool {
		for _, id := range h.d.LiveTokens() {
			if id == badID {
				return true
			}
		}
		return false
	})
	b, _ := os.ReadFile(filepath.Join(h.home, ".archivist", "connect", "run", sid, "task-token"))
	if strings.Contains(string(b), "badtoken_") {
		t.Fatal("malformed token reached the token file")
	}
	waitFor(t, 5*time.Second, "malformed refresh logged", func() bool {
		return strings.Contains(h.log.String(), "malformed task token")
	})
	if h.api.revokedByID(badID) {
		t.Fatal("the injected revoke failure did not apply")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !h.api.revokedByID(badID) || len(h.api.liveTokens()) != 0 {
		t.Fatal("exit did not revoke the tracked malformed token")
	}
}

func TestReviewRetryDelayCappedAtOneMinute(t *testing.T) {
	setDuration(t, &tokenRetryBase, 5*time.Second)
	var got []time.Duration
	d := time.Duration(0)
	for i := 0; i < 7; i++ {
		d = nextRetry(d)
		got = append(got, d)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute, time.Minute}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delays %v", got)
	}
}

// A fatal session socket (ticket refused 403) fails the record.
func TestReviewFatalSessionLinkFailsRecord(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	pid := fakeRuns(t, h.home)[0].PID
	h.api.set(func(f *fakeChatAPI) { f.sessionTicketStatus = 403 })
	h.relay.dropSession(sid)
	waitFor(t, 15*time.Second, "record failed", func() bool { return h.record(sid).Status == "failed" })
	waitFor(t, 10*time.Second, "claude stopped", func() bool { return !processAlive(pid) })
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
}

// Startup sweep: expired records end and lose cwd and run dir; inactive
// records lose their run dir; corrupt records are refused with an ack.
func TestReviewStartupSweepAndCorruptRecord(t *testing.T) {
	h := newHarness(t, nil)
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	expired, ended := randomUUID(), randomUUID()
	expiredCwd := filepath.Join(h.tmp, cwdPrefix+"expired")
	endedCwd := filepath.Join(h.tmp, cwdPrefix+"ended")
	for _, d := range []string{expiredCwd, endedCwd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Save(&SessionRecord{SessionID: expired, Agent: "claude", Cwd: expiredCwd, ExpiresAt: 1, Status: "active", CreatedAt: 1})
	_ = st.Save(&SessionRecord{SessionID: ended, Agent: "claude", Cwd: endedCwd, Status: "ended", CreatedAt: 2})
	for _, id := range []string{expired, ended} {
		dir, _ := st.RunDir(id)
		_ = os.WriteFile(filepath.Join(dir, "task-token"), []byte("stale"), 0o600)
	}
	corrupt := h.api.addSession("claude")
	if err := os.WriteFile(filepath.Join(st.Dir(), "sessions", corrupt.SessionID+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	for _, id := range []string{expired, ended} {
		if _, err := os.Stat(filepath.Join(st.Dir(), "run", id)); !os.IsNotExist(err) {
			t.Errorf("run dir of %s kept", id[:8])
		}
	}
	for _, d := range []string{expiredCwd, endedCwd} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("cwd of inactive record kept: %s", d)
		}
	}
	if rec, _ := st.Load(expired); rec.Status != "ended" {
		t.Errorf("expired record %s", rec.Status)
	}
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "start-corrupt", "sessionId": corrupt.SessionID, "agent": "claude", "prompt": "echo x"})
	waitFor(t, 10*time.Second, "corrupt start acked", func() bool { return h.relay.acked("start-corrupt") })
	h.relay.waitStatus(t, corrupt.SessionID, "failed", 1)
	time.Sleep(200 * time.Millisecond)
	if len(fakeRuns(t, h.home)) != 0 {
		t.Fatal("corrupt record started claude")
	}
}

// (a) The spawned argv carries --setting-sources with the empty default.
func TestReviewSettingSourcesPinned(t *testing.T) {
	if DefaultSettingSources != "" {
		t.Fatalf("DefaultSettingSources = %q, want empty", DefaultSettingSources)
	}
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	args := fakeRuns(t, h.home)[0].Args
	for i, a := range args {
		if a == "--setting-sources" {
			if i+1 >= len(args) || args[i+1] != "" {
				t.Fatalf("--setting-sources value %q", args[i+1:])
			}
			return
		}
	}
	t.Fatalf("no --setting-sources in %v", args)
}

// (b) A message sent while a turn streams is queued and runs next.
func TestReviewMessageDuringTurnIsQueued(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("wait 800")
	waitFor(t, 10*time.Second, "turn running", func() bool { return len(h.relay.eventsOf(sid, "start")) == 1 })
	cid := h.message(sid, "echo queued")
	waitFor(t, 5*time.Second, "second message acked", func() bool { return h.relay.acked(cid) })
	if n := len(h.relay.eventsOf(sid, "finish")); n != 0 {
		t.Fatalf("first turn already finished (%d) when the second message was accepted", n)
	}
	h.waitFinishes(sid, 2)
	text := h.relay.text(sid)
	if !strings.Contains(text, "waited") || strings.Index(text, "queued") < strings.Index(text, "waited") {
		t.Fatalf("turn order %q", text)
	}
	var order []string
	for _, p := range h.relay.payloads(sid) {
		if p["type"] == "start" || p["type"] == "finish" {
			order = append(order, p["type"].(string))
		}
	}
	if strings.Join(order, ",") != "start,finish,start,finish" || len(fakeRuns(t, h.home)) != 1 {
		t.Fatalf("order %v", order)
	}
}

// (d) Reconnect: a dropped session socket re-mints and redials, an event
// left unacked across the drop is resent byte-identical; a 4001 on the user
// socket redials and re-sends capabilities; pings flow.
func TestReviewReconnectResendsAndPings(t *testing.T) {
	setDuration(t, &pingInterval, 100*time.Millisecond)
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	waitFor(t, 5*time.Second, "pings", func() bool { return h.relay.pingCount() >= 2 })

	h.relay.set(func(r *fakeRelay) { r.withhold[sid] = true })
	h.message(sid, "echo unacked")
	h.waitFinishes(sid, 2)
	finish := h.relay.eventsOf(sid, "finish")[1]
	cid := finish["correlationId"].(string)
	ticketsBefore, dialsBefore := h.api.ticketMints(sid), h.relay.dialCount(sid)
	h.relay.set(func(r *fakeRelay) { r.withhold[sid] = false })
	h.relay.dropSession(sid)
	waitFor(t, 15*time.Second, "resend after redial", func() bool { return len(h.relay.framesOf(cid)) >= 2 })
	frames := h.relay.framesOf(cid)
	if frames[0] != frames[1] {
		t.Fatalf("resend differs:\n%s\n%s", frames[0], frames[1])
	}
	if h.api.ticketMints(sid) <= ticketsBefore || h.relay.dialCount(sid) <= dialsBefore {
		t.Fatal("no re-mint and redial")
	}

	caps := h.relay.capsCount()
	userTickets := h.api.ticketMints("")
	h.relay.closeUser(closeExpired)
	waitFor(t, 15*time.Second, "capabilities re-sent", func() bool { return h.relay.capsCount() > caps })
	if h.api.ticketMints("") <= userTickets {
		t.Fatal("user ticket not re-minted")
	}
	h.message(sid, "echo after reconnect")
	h.waitFinishes(sid, 3)
}
