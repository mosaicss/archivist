//go:build !windows

package connect

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Second review round: run dir and cwd cleanup, leftover token revocation,
// resume slots and failures, relay UNAVAILABLE, durable start, malformed
// first mint, PID start-time guard.

func TestRemoveSessionCwdGuard(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, cwdPrefix+"123")
	other := filepath.Join(base, "projects")
	for _, d := range []string{good, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	home, _ := os.UserHomeDir()
	for _, bad := range []string{"", "relative/" + cwdPrefix + "x", other, "/", home, base} {
		if removeSessionCwd(bad) {
			t.Errorf("removed %q", bad)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated directory removed")
	}
	if !removeSessionCwd(good) {
		t.Fatal("session cwd not removed")
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatal("session cwd still exists")
	}
}

// A session failed for good loses its cwd.
func TestFailedSessionRemovesCwd(t *testing.T) {
	h := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiKeySource": "ANTHROPIC_API_KEY", "permissionMode": "default"})
	h.start()
	sid := h.startSession("echo nope")
	h.relay.waitStatus(t, sid, "failed", 1)
	rec := h.record(sid)
	waitFor(t, 10*time.Second, "cwd removed", func() bool {
		_, err := os.Stat(rec.Cwd)
		return os.IsNotExist(err)
	})
}

// A crashed run's task tokens (active and inactive records) are revoked at
// startup before their run dirs go; a failed revoke is retried at exit.
func TestStartupRevokesLeftoverTokens(t *testing.T) {
	h := newHarness(t, nil)
	active := h.api.addSession("claude")
	ended := h.api.addSession("claude")
	flaky := h.api.addSession("claude")
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, s := range []struct {
		id, status string
	}{{active.SessionID, "active"}, {ended.SessionID, "ended"}, {flaky.SessionID, "failed"}} {
		_ = st.Save(&SessionRecord{SessionID: s.id, Agent: "claude", ClaudeSessionID: "c-" + s.id[:8],
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: s.status, CreatedAt: 1})
		tok := h.mintLeftover(s.id)
		ids[s.id] = tok
		dir, _ := st.RunDir(s.id)
		_ = os.WriteFile(filepath.Join(dir, "task-token"), []byte(h.api.tokenIDs()[tok]+"\n"), 0o600)
	}
	// Every revoke of the flaky session's token fails until cleared.
	h.api.set(func(f *fakeChatAPI) { f.failRevokesFor = ids[flaky.SessionID] })
	h.start()
	for sid, tok := range ids {
		if sid == flaky.SessionID {
			continue
		}
		if !h.api.revokedByID(tok) {
			t.Errorf("leftover token of %s not revoked", sid[:8])
		}
		if _, err := os.Stat(filepath.Join(st.Dir(), "run", sid)); !os.IsNotExist(err) {
			t.Errorf("run dir of %s kept", sid[:8])
		}
	}
	found := false
	for _, id := range h.d.LiveTokens() {
		found = found || id == ids[flaky.SessionID]
	}
	if !found || h.api.revokedByID(ids[flaky.SessionID]) {
		t.Fatal("failed leftover revoke not tracked")
	}
	h.api.set(func(f *fakeChatAPI) { f.failRevokesFor = "" })
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !h.api.revokedByID(ids[flaky.SessionID]) {
		t.Fatal("exit did not retry the leftover revoke")
	}
}

// mintLeftover mints a token through the fake as a crashed run would have.
func (h *harness) mintLeftover(sid string) string {
	h.t.Helper()
	before := h.api.tokenIDs()
	h.api.set(func(f *fakeChatAPI) {
		id := randomUUID()
		f.tokens[id] = &fakeToken{id: id, token: "mst_" + id + "." + strings.Repeat("a", 43), session: sid}
	})
	for id := range h.api.tokenIDs() {
		if _, ok := before[id]; !ok {
			return id
		}
	}
	h.t.Fatal("no token minted")
	return ""
}

// (a) A resumed session holds a slot; a resume on a full daemon is refused
// with only the capacity status and no Claude start.
func TestResumeHoldsSlotAndRespectsCap(t *testing.T) {
	h := newHarness(t, nil)
	h.max = 1
	h.start()
	b := h.startSession("echo b")
	h.waitFinishes(b, 1)
	h.message(b, "exit")
	h.relay.waitStatus(t, b, "failed", 1)
	waitFor(t, 10*time.Second, "slot released", func() bool { return h.d.slotsInUse() == 0 })
	h.message(b, "echo b resumed")
	waitFor(t, 20*time.Second, "resumed", func() bool { return strings.Contains(h.relay.text(b), "b resumed") })
	if h.d.slotsInUse() != 1 {
		t.Fatalf("slots %d after resume", h.d.slotsInUse())
	}
	h.message(b, "exit")
	h.relay.waitStatus(t, b, "failed", 2)
	waitFor(t, 10*time.Second, "slot released", func() bool { return h.d.slotsInUse() == 0 })

	a := h.startSession("slow")
	waitFor(t, 10*time.Second, "a running", func() bool { return strings.Contains(h.relay.text(a), "1\n") })
	runs := len(fakeRuns(t, h.home))
	statuses := len(h.relay.statuses(b))
	errs := len(h.errorTexts(b))
	h.message(b, "echo refused")
	h.relay.waitStatus(t, b, "failed", 3)
	st := h.relay.statuses(b)[statuses:]
	if len(st) != 1 || st[0] != "failed" || len(h.errorTexts(b)) != errs || len(fakeRuns(t, h.home)) != runs || h.d.slotsInUse() != 1 {
		t.Fatalf("capacity refusal: statuses %v, errors %d->%d, runs %d->%d, slots %d", st, errs, len(h.errorTexts(b)), runs, len(fakeRuns(t, h.home)), h.d.slotsInUse())
	}
	last := h.relay.eventsOf(b, "data-session-status")
	if !strings.Contains(last[len(last)-1]["payload"].(map[string]any)["data"].(map[string]any)["message"].(string), "live sessions") {
		t.Fatal("not the capacity status")
	}
}

// (b) A resume whose token mint fails keeps the session resumable.
func TestResumeMintFailureStaysResumable(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	h.message(sid, "exit")
	h.relay.waitStatus(t, sid, "failed", 1)
	h.api.set(func(f *fakeChatAPI) { f.failMints = 100 })
	h.message(sid, "echo try")
	h.relay.waitStatus(t, sid, "failed", 2)
	last := h.relay.eventsOf(sid, "data-session-status")
	if msg := last[len(last)-1]["payload"].(map[string]any)["data"].(map[string]any)["message"].(string); !strings.Contains(msg, "tries to resume") {
		t.Fatalf("status %q", msg)
	}
	if h.record(sid).Status != "active" || h.d.slotsInUse() != 0 {
		t.Fatal("session not left resumable")
	}
	h.api.set(func(f *fakeChatAPI) { f.failMints = 0 })
	h.message(sid, "echo recovered")
	waitFor(t, 20*time.Second, "resumed", func() bool { return strings.Contains(h.relay.text(sid), "recovered") })
}

// (c) A relay UNAVAILABLE for an in-flight event: the daemon redials and
// resends byte-identical, in order.
func TestRelayUnavailableResendsInOrder(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	dials := h.relay.dialCount(sid)
	h.relay.set(func(r *fakeRelay) { r.unavailable[sid] = true })
	h.message(sid, "echo after unavailable")
	h.waitFinishes(sid, 2)
	if h.relay.dialCount(sid) <= dials {
		t.Fatal("no redial after UNAVAILABLE")
	}
	refused := h.relay.refusedCID(sid)
	frames := h.relay.framesOf(refused)
	if refused == "" || len(frames) < 2 || frames[0] != frames[1] {
		t.Fatalf("refused %q frames %d", refused, len(frames))
	}
	var last float64
	for _, e := range h.relay.eventsOf(sid, "") {
		seq := e["seq"].(float64)
		if seq <= last {
			t.Fatalf("stored events out of order at seq %v", seq)
		}
		last = seq
	}
	if !strings.Contains(h.relay.text(sid), "after unavailable") {
		t.Fatal("turn text lost")
	}
}

// (d) A start whose record cannot be saved is not acked, runs nothing and
// keeps no slot; the redelivery starts it.
func TestStartSaveFailureAwaitsRedelivery(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	st := h.d.store
	st.mu.Lock()
	st.failSaves = 1
	st.mu.Unlock()
	s := h.api.addSession("claude")
	cmd := map[string]any{"kind": "start_session", "correlationId": "start-durable", "sessionId": s.SessionID, "agent": "claude", "prompt": "echo started"}
	raw, _ := json.Marshal(cmd)
	h.relay.sendRaw("", string(raw))
	time.Sleep(500 * time.Millisecond)
	if h.relay.acked("start-durable") || len(fakeRuns(t, h.home)) != 0 || h.d.slotsInUse() != 0 {
		t.Fatalf("acked %v runs %d slots %d", h.relay.acked("start-durable"), len(fakeRuns(t, h.home)), h.d.slotsInUse())
	}
	h.relay.sendRaw("", string(raw))
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("start-durable") })
	h.waitFinishes(s.SessionID, 1)
}

// (e) A malformed first mint whose revoke fails is tracked and revoked at stop.
func TestMalformedFirstMintTrackedAndRevokedAtStop(t *testing.T) {
	h := newHarness(t, nil)
	h.api.set(func(f *fakeChatAPI) { f.badTokens = 1; f.failRevokes = 2 }) // the client retries a 503 DELETE once
	h.start()
	sid := h.startSession("echo never")
	h.relay.waitStatus(t, sid, "failed", 1)
	var badID string
	for id, tok := range h.api.tokenIDs() {
		if strings.HasPrefix(tok, "badtoken_") {
			badID = id
		}
	}
	tracked := false
	for _, id := range h.d.LiveTokens() {
		tracked = tracked || id == badID
	}
	if badID == "" || !tracked || h.api.revokedByID(badID) {
		t.Fatalf("bad %q tracked %v", badID, tracked)
	}
	if len(fakeRuns(t, h.home)) != 0 {
		t.Fatal("claude started with a malformed token")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !h.api.revokedByID(badID) {
		t.Fatal("not revoked at stop")
	}
}

// (f) killPID leaves a live process alone when the start time differs.
func TestKillPIDChecksStartTime(t *testing.T) {
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() { _ = c.Process.Kill(); <-done })
	pid := c.Process.Pid
	start := startTime(pid)
	if start == "" {
		t.Fatal("no start time")
	}
	killPID(procID{pid: pid, start: "Thu Jan  1 00:00:00 1970"})
	killPID(procID{pid: pid, start: ""})
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("a process with another start time was killed")
	default:
	}
	killPID(procID{pid: pid, start: start})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("matching start time not killed")
	}
}
