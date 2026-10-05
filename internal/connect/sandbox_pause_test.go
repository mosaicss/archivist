//go:build !windows

package connect

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Story 78.37: the session-bound mode's activity file (idle pause), the
// deny of pending approvals on SIGTERM, and --resume-from.

// activityFile reads the activity file: present reports a readable file.
func readActivity(t *testing.T, path string) (rec activityRecord, raw string, present bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, "", false
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("activity file %q: %v", b, err)
	}
	return rec, string(b), true
}

// waitActivity waits for an activity file matching cond and returns it.
func (h *harness) waitActivity(what string, cond func(a activityRecord) bool) activityRecord {
	h.t.Helper()
	var got activityRecord
	waitFor(h.t, 30*time.Second, "activity: "+what, func() bool {
		a, _, ok := readActivity(h.t, h.activityFile)
		got = a
		return ok && cond(a)
	})
	return got
}

func idle(a activityRecord) bool { return a.IdleSince != nil && a.ApprovalSince == nil }
func busy(a activityRecord) bool { return a.IdleSince == nil && a.ApprovalSince == nil }

func TestSandboxActivityFileClaude(t *testing.T) {
	h := newHarness(t, nil)
	h.sessionMode = ModeAsk // Bash asks
	h.activityFile = filepath.Join(t.TempDir(), "run", "archivist", "activity.json")
	s := h.api.addSession("claude")
	began := time.Now().UnixMilli()
	r := h.runSession(s.SessionID, "claude", "", "echo first")
	waitFor(t, 30*time.Second, "first turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "first") })

	// Quiet after the turn: idleSince set, no approval; exact shape, 0600.
	a := h.waitActivity("idle after the first turn", idle)
	if *a.IdleSince < began || *a.IdleSince > time.Now().UnixMilli() {
		t.Fatalf("idleSince %d outside the run", *a.IdleSince)
	}
	_, raw, _ := readActivity(t, h.activityFile)
	// Story 78.38 adds the login the Claude session runs on.
	if want := `{"v":1,"idleSince":` + jsonInt(*a.IdleSince) + `,"approvalSince":null,"login":"claude.ai"}`; raw != want {
		t.Fatalf("activity file %s, want %s", raw, want)
	}
	if st, err := os.Stat(h.activityFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("activity file mode %v (%v)", st.Mode(), err)
	}
	// Stays the same while it stays quiet (no rewrite).
	before, _ := os.Stat(h.activityFile)
	time.Sleep(300 * time.Millisecond)
	if after, _ := os.Stat(h.activityFile); !os.SameFile(before, after) {
		t.Fatal("activity file rewritten while nothing changed")
	}

	// A streaming turn is never idle; the interrupt ends it, a new period starts.
	h.message(s.SessionID, "slow")
	waitFor(t, 30*time.Second, "slow turn", func() bool { return len(h.relay.eventsOf(s.SessionID, "text-delta")) > 1 })
	h.waitActivity("busy during slow", busy)
	h.command(s.SessionID, "interrupt")
	b := h.waitActivity("idle after the interrupt", idle)
	if *b.IdleSince <= *a.IdleSince {
		t.Fatalf("idleSince %d after a new turn, want later than %d", *b.IdleSince, *a.IdleSince)
	}

	// A pending approval: approvalSince set, not idle; cleared after the answer.
	h.message(s.SessionID, "bash ls")
	h.waitCard(s.SessionID, 1)
	c := h.waitActivity("approval pending", func(a activityRecord) bool { return a.ApprovalSince != nil && a.IdleSince == nil })
	if *c.ApprovalSince < *b.IdleSince {
		t.Fatalf("approvalSince %d before the turn", *c.ApprovalSince)
	}
	h.approve(s.SessionID, 1, "allow", "user", "")
	h.waitFinishes(s.SessionID, 3)
	h.waitActivity("idle after the answer", idle)

	// A stuck turn (ignores interrupts, silent between deltas) stays busy.
	h.message(s.SessionID, "stuck")
	waitFor(t, 30*time.Second, "stuck turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "3\n") })
	h.waitActivity("busy during stuck", busy)
	time.Sleep(500 * time.Millisecond)
	if a, _, _ := readActivity(t, h.activityFile); !busy(a) {
		t.Fatalf("activity %+v during a stuck turn", a)
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("RunSession after the cancel: %v", err)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSandboxActivityFileCodexApproval(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.sessionMode = ModeAsk
	h.activityFile = filepath.Join(t.TempDir(), "activity.json")
	s := h.api.addSession("codex")
	r := h.runSession(s.SessionID, "codex", "", "cmd touch a.txt")
	h.waitCard(s.SessionID, 1)
	h.waitActivity("codex approval pending", func(a activityRecord) bool { return a.ApprovalSince != nil && a.IdleSince == nil })
	h.approve(s.SessionID, 1, "allow", "user", "")
	h.waitFinishes(s.SessionID, 1)
	h.waitActivity("codex idle after the answer", idle)
	r.stop()
}

// Without ARCHIVIST_ACTIVITY_FILE (or outside the session-bound mode) no
// file is written: the Worker treats that as busy.
func TestSandboxActivityFileOff(t *testing.T) {
	h := newHarness(t, nil)
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "", "echo quiet")
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "quiet") })
	r.stop()
	if strings.Contains(h.log.String(), "activity file") {
		t.Fatalf("activity file mentioned without one configured:\n%s", h.log.String())
	}
}

// writeActivity transitions on a bare session: only changes are written.
func TestWriteActivityTransitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	s := &session{d: &Daemon{sandbox: true, activityFile: path}, log: NewLogger(io.Discard),
		pending: map[string]*pendingApproval{}}
	t0 := time.UnixMilli(1_800_000_000_000)
	s.turnActive = true
	s.writeActivity(t0)
	if a, raw, ok := readActivity(t, path); !ok || !busy(a) || raw != `{"v":1,"idleSince":null,"approvalSince":null}` {
		t.Fatalf("turn in flight: %s", raw)
	}
	s.turnActive = false
	s.writeActivity(t0.Add(time.Second))
	first, _ := os.Stat(path)
	s.writeActivity(t0.Add(time.Hour)) // still quiet: same period, no write
	if a, _, _ := readActivity(t, path); a.IdleSince == nil || *a.IdleSince != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("idleSince %v", a.IdleSince)
	}
	if again, _ := os.Stat(path); !os.SameFile(first, again) {
		t.Fatal("rewritten without a change")
	}
	s.pending["req"] = &pendingApproval{toolName: "Bash"}
	s.writeActivity(t0.Add(2 * time.Hour))
	if a, _, _ := readActivity(t, path); a.IdleSince != nil || a.ApprovalSince == nil || *a.ApprovalSince != t0.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("approval: %+v", a)
	}
	s.writeActivity(t0.Add(3 * time.Hour)) // same approval period
	if a, _, _ := readActivity(t, path); *a.ApprovalSince != t0.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("approvalSince moved: %+v", a)
	}
	delete(s.pending, "req")
	for _, busyState := range []func(){
		func() { s.queue = []string{"x"} },
		func() { s.interruptTimer = time.NewTimer(time.Hour) },
		func() { s.held = []Chunk{{"type": "start"}} },
		func() { s.stopping = true },
	} {
		s.queue, s.interruptTimer, s.held, s.stopping = nil, nil, nil, false
		s.writeActivity(t0.Add(4 * time.Hour))
		if a, _, _ := readActivity(t, path); !idle(a) {
			t.Fatalf("not idle: %+v", a)
		}
		busyState()
		s.writeActivity(t0.Add(5 * time.Hour))
		if a, _, _ := readActivity(t, path); !busy(a) {
			t.Fatalf("busy state reads %+v", a)
		}
	}
	// Outside the session-bound mode nothing is written.
	other := filepath.Join(t.TempDir(), "a.json")
	(&session{d: &Daemon{activityFile: other}, log: NewLogger(io.Discard)}).writeActivity(t0)
	if _, err := os.Stat(other); err == nil {
		t.Fatal("activity file written outside the session-bound mode")
	}
}

// ─── deny on SIGTERM ────────────────────────────────────────────────────────

func TestSandboxStopDeniesPendingApprovals(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			h, prompt := newHarness(t, nil), "bash rm -rf build"
			if agent == "codex" {
				h, prompt = newCodexHarness(t, nil), "cmd rm -rf build"
			}
			h.sessionMode = ModeAsk
			h.activityFile = filepath.Join(t.TempDir(), "activity.json")
			s := h.api.addSession(agent)
			r := h.runSession(s.SessionID, agent, "", prompt)
			req := h.waitCard(s.SessionID, 1)
			h.waitActivity("approval pending", func(a activityRecord) bool { return a.ApprovalSince != nil })
			r.cancel()
			if err := r.wait(); err != nil {
				t.Fatalf("RunSession after the cancel: %v", err)
			}
			resp := approvalResponses(h, s.SessionID)
			if len(resp) != 1 || resp[0]["approvalId"] != req["approvalId"] || resp[0]["approved"] != false {
				t.Fatalf("approval responses %v", resp)
			}
			want := sandboxStopDeny
			if agent == "codex" {
				want = "decline"
			}
			if resp[0]["reason"] != want {
				t.Fatalf("deny reason %v, want %q", resp[0]["reason"], want)
			}
			h.relay.waitStatus(t, s.SessionID, "disconnected", 1)
			// Claude reports a command that ran as a tool output, Codex as text.
			if h.toolOutput(s.SessionID, "ran: rm -rf build") || strings.Contains(h.relay.text(s.SessionID), "ran ") {
				t.Fatal("the command ran after the stop")
			}
		})
	}
}

// ─── --resume-from ──────────────────────────────────────────────────────────

// firstRun runs session A to a remembered codeword and SIGTERMs it (a
// pause), leaving its record and harness files in HOME.
func (h *harness) firstRun(agent string) (string, *SessionRecord) {
	h.t.Helper()
	a := h.api.addSession(agent)
	r := h.runSession(a.SessionID, agent, "", "remember bluebird")
	waitFor(h.t, 30*time.Second, "remembered", func() bool { return strings.Contains(h.relay.text(a.SessionID), "ok, remembered") })
	r.cancel()
	if err := r.wait(); err != nil {
		h.t.Fatalf("first run: %v", err)
	}
	rec := h.record(a.SessionID)
	if rec.Status != "active" || rec.Cwd == "" {
		h.t.Fatalf("paused record %+v", rec)
	}
	return a.SessionID, rec
}

// asRestored turns HOME into what a fresh sandbox holds after the Worker's
// restores, and removes A's working directory (a fresh container has none).
// Kept: the home archive set (.claude without projects, sessions, logs and
// shell-snapshots at its top; .claude.json; .codex) and the resume archive
// set (.claude/projects, .archivist/connect/sessions,
// .archivist/connect/codex-home), regular files and directories only, so
// symlinks such as the Codex auth.json link are dropped. Everything else
// goes (the store's run dir, the fakes' own state), except the harness
// fixtures that stand outside a sandbox home: the fakes' config.json files
// and the owner's Codex login (.codex-owner, the harness's stand-in for the
// archived .codex).
func (h *harness) asRestored(aID string, a *SessionRecord) {
	h.t.Helper()
	if err := os.RemoveAll(a.Cwd); err != nil {
		h.t.Fatal(err)
	}
	kept := func(rel string) bool {
		switch {
		case under(rel, ".claude/projects"), under(rel, ".archivist/connect/sessions"),
			under(rel, ".archivist/connect/codex-home"):
			return true
		case under(rel, ".claude/sessions"), under(rel, ".claude/logs"), under(rel, ".claude/shell-snapshots"):
			return false
		case under(rel, ".claude"), rel == ".claude.json", under(rel, ".codex"), under(rel, ".codex-owner"):
			return true
		case rel == ".fakeclaude/config.json", rel == ".fakecodex/config.json":
			return true
		}
		return false
	}
	// Parents of kept paths stay as directories; everything else is removed.
	parent := func(rel string) bool {
		for _, p := range []string{".claude", ".archivist", ".archivist/connect", ".fakeclaude", ".fakecodex"} {
			if rel == p {
				return true
			}
		}
		return false
	}
	var drop []string
	err := filepath.WalkDir(h.home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == h.home {
			return err
		}
		rel, _ := filepath.Rel(h.home, path)
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			if !under(rel, ".codex-owner") {
				drop = append(drop, path)
			}
			return nil
		}
		if kept(rel) || parent(rel) {
			return nil
		}
		drop = append(drop, path)
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range drop {
		if err := os.RemoveAll(p); err != nil {
			h.t.Fatal(err)
		}
	}
}

// under reports whether the slash path rel is p or inside it.
func under(rel, p string) bool { return rel == p || strings.HasPrefix(rel, p+"/") }

// resumedRun starts session B with --resume-from from and prompt.
func (h *harness) resumedRun(agent, from, prompt string) (string, *sandboxRun) {
	h.t.Helper()
	b := h.api.addSession(agent)
	h.resumeFrom = from
	return b.SessionID, h.runSession(b.SessionID, agent, "", prompt)
}

func TestSandboxResumeClaude(t *testing.T) {
	h := newHarness(t, nil)
	aID, a := h.firstRun("claude")
	h.asRestored(aID, a)
	bID, r := h.resumedRun("claude", aID, "recall")
	waitFor(t, 30*time.Second, "recall", func() bool { return strings.Contains(h.relay.text(bID), "bluebird") })
	if strings.Contains(h.relay.text(bID), resumeMissNote) {
		t.Fatal("the restore note on a resumed conversation")
	}
	b := h.record(bID)
	if b.ClaudeSessionID != a.ClaudeSessionID || b.Cwd != a.Cwd {
		t.Fatalf("resumed record %+v, want Claude session %s in %s", b, a.ClaudeSessionID, a.Cwd)
	}
	if old := h.record(aID); old.Status != "ended" || old.Cwd != "" || old.ClaudeSessionID != "" {
		t.Fatalf("earlier record not retired: %+v", old)
	}
	runs := fakeRuns(t, h.home)
	last := runs[len(runs)-1]
	if !slices.Contains(last.Args, "--resume="+a.ClaudeSessionID) || last.Cwd != a.Cwd {
		t.Fatalf("resumed run %+v", last)
	}
	r.stop()
}

func TestSandboxResumeCodex(t *testing.T) {
	h := newCodexHarness(t, nil)
	aID, a := h.firstRun("codex")
	st := h.store()
	homeA, _ := st.CodexHomePath(aID)
	// The resume archive holds no symlinks: the auth.json link is relinked.
	if err := os.Remove(filepath.Join(homeA, "auth.json")); err != nil {
		t.Fatal(err)
	}
	h.asRestored(aID, a)
	bID, r := h.resumedRun("codex", aID, "recall")
	waitFor(t, 30*time.Second, "recall", func() bool { return strings.Contains(h.relay.text(bID), "bluebird") })
	if strings.Contains(h.relay.text(bID), resumeMissNote) {
		t.Fatal("the restore note on a resumed thread")
	}
	b := h.record(bID)
	if b.CodexThreadID != a.CodexThreadID || b.Cwd != a.Cwd {
		t.Fatalf("resumed record %+v, want thread %s in %s", b, a.CodexThreadID, a.Cwd)
	}
	homeB, _ := st.CodexHomePath(bID)
	if _, err := os.Stat(homeA); !os.IsNotExist(err) {
		t.Fatalf("codex home of A still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "sessions", a.CodexThreadID+".json")); err != nil {
		t.Fatalf("rollout not in B's codex home: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(homeB, "auth.json")); err != nil || target != filepath.Join(h.codexOwner(), "auth.json") {
		t.Fatalf("auth.json link %q (%v)", target, err)
	}
	var resume map[string]any
	raw, err := os.ReadFile(filepath.Join(h.home, ".fakecodex", "thread-resume.json"))
	if err != nil || json.Unmarshal(raw, &resume) != nil || resume["threadId"] != a.CodexThreadID || resume["cwd"] != a.Cwd {
		t.Fatalf("thread/resume params %s (%v)", raw, err)
	}
	r.stop()
}

// A missing record, a Claude Code that cannot find the transcript (exits
// before init) and a Codex thread/resume error all start fresh with the
// note before the first answer.
func TestSandboxResumeFallsBackWithNote(t *testing.T) {
	cases := []struct {
		name, agent, log string
		setup            func(h *harness) string // returns the session to resume from
	}{
		{"missing record", "claude", "cannot be resumed (no record of it)", func(h *harness) string { return randomUUID() }},
		{"claude transcript gone", "claude", "claude exited before resuming the earlier conversation", func(h *harness) string {
			aID, _ := h.firstRun("claude")
			writeFakeClaudeConfig(h.t, h.home, map[string]any{"resumeNotFound": true})
			return aID
		}},
		{"codex rollout gone", "codex", "Codex could not resume the earlier conversation, starting fresh", func(h *harness) string {
			aID, _ := h.firstRun("codex")
			home, _ := h.store().CodexHomePath(aID)
			if err := os.RemoveAll(filepath.Join(home, "sessions")); err != nil {
				h.t.Fatal(err)
			}
			return aID
		}},
		{"other agent", "codex", "it ran the claude agent, not codex", func(h *harness) string {
			aID, _ := h.firstRun("claude")
			return aID
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil)
			if c.agent == "codex" || c.name == "other agent" {
				h = newCodexHarness(t, nil)
			}
			from := c.setup(h)
			bID, r := h.resumedRun(c.agent, from, "echo fresh start")
			waitFor(t, 30*time.Second, "answer", func() bool { return strings.Contains(h.relay.text(bID), "fresh start") })
			text := h.relay.text(bID)
			if !strings.HasPrefix(text, resumeMissNote+"\n\n") {
				t.Fatalf("answer %q, want it to start with the note and a blank line", text)
			}
			if !strings.Contains(h.log.String(), c.log) {
				t.Fatalf("log lacks %q:\n%s", c.log, h.log.String())
			}
			if strings.Count(text, resumeMissNote) != 1 {
				t.Fatalf("note repeated: %q", text)
			}
			// The note sits inside the turn, after its start chunk.
			types := h.payloadTypes(bID)
			start, note := slices.Index(types, "start"), slices.Index(types, "text-start")
			if start < 0 || note < start {
				t.Fatalf("payloads %v", types)
			}
			if got := h.relay.statuses(bID); slices.Contains(got, "failed") {
				t.Fatalf("statuses %v", got)
			}
			if b := h.record(bID); b.Status != "active" || (b.ClaudeSessionID == "" && b.CodexThreadID == "") {
				t.Fatalf("fresh record %+v", b)
			}
			// A later turn carries no note.
			h.message(bID, "echo second")
			waitFor(t, 30*time.Second, "second", func() bool { return strings.Contains(h.relay.text(bID), "second") })
			if strings.Count(h.relay.text(bID), resumeMissNote) != 1 {
				t.Fatal("note on a later turn")
			}
			r.stop()
		})
	}
}

// RunSession refuses a resume source that is not another session UUID.
func TestSandboxResumeFromValidated(t *testing.T) {
	for _, from := range []string{"not-a-uuid", "self"} {
		h := newHarness(t, nil)
		s := h.api.addSession("claude")
		h.resumeFrom = from
		if from == "self" {
			h.resumeFrom = s.SessionID
		}
		r := h.runSession(s.SessionID, "claude", "", "echo x")
		wantFatal(t, r.wait(), "BAD_RESUME", false)
	}
}

// The 78.37 activity file names the login a Claude session runs on once
// its spawn proof passed (claude.ai, Console, API key mode) and never for
// Codex (Story 78.38).
func TestActivityFileCarriesLogin(t *testing.T) {
	type run func(t *testing.T, h *harness, sid string) *sandboxRun
	cases := []struct {
		name, agent, want string
		cfg               map[string]any
		start             run
	}{
		{"claude.ai", "claude", "claude.ai", nil, func(t *testing.T, h *harness, sid string) *sandboxRun {
			return h.runSession(sid, "claude", "", "echo hi")
		}},
		{"console", "claude", "console", map[string]any{"loggedIn": true, "authMethod": "api_key", "apiKeySource": "/login managed key"},
			func(t *testing.T, h *harness, sid string) *sandboxRun {
				return h.runSessionWith(sid, "claude", "", "echo hi", sessionOpts{login: ClaudeLoginConsole})
			}},
		{"api key", "claude", "api_key", map[string]any{"loggedIn": false, "authMethod": "none"},
			func(t *testing.T, h *harness, sid string) *sandboxRun {
				file := filepath.Join(h.tmp, "signin.json")
				r := h.runSessionWith(sid, "claude", "claude", "echo hi", sessionOpts{signinFile: file})
				h.waitPrompts(sid, 1)
				writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "api_key", "key": testSigninKey})
				return r
			}},
		{"codex", "codex", "", nil, func(t *testing.T, h *harness, sid string) *sandboxRun {
			return h.runSession(sid, "codex", "", "echo hi")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setDuration(t, &signinPollEvery, 50*time.Millisecond)
			var h *harness
			if c.agent == "codex" {
				h = newCodexHarness(t, nil)
			} else {
				h = newHarness(t, c.cfg)
			}
			h.activityFile = filepath.Join(t.TempDir(), "run", "activity.json")
			s := h.api.addSession(c.agent)
			r := c.start(t, h, s.SessionID)
			waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "hi") })
			a := h.waitActivity("idle after the turn", idle)
			if a.Login != c.want {
				t.Fatalf("activity login %q, want %q", a.Login, c.want)
			}
			if _, raw, _ := readActivity(t, h.activityFile); c.want == "" && strings.Contains(raw, `"login"`) {
				t.Fatalf("activity file %s names a login", raw)
			}
			r.stop()
		})
	}
}
