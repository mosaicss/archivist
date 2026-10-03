//go:build e2e && !windows

// Codex rows (Story 78.17) of the end-to-end test against a local
// `wrangler dev` of the agent relay; see e2e_test.go for the setup.
//
// CONNECT_E2E_LIVE=1 CONNECT_E2E_LIVE_AGENT=codex runs TestE2ELiveCodex
// with the real Codex on the owner's ChatGPT login (bounded and ledgered);
// CONNECT_E2E_LIVE_ROW=restart runs only the restart and resume row.
package connect

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// codexOnline reports a fresh presence frame with a usable codex.
func (u *userConsumer) codexOnline() bool {
	p := u.lastPresence()
	if p == nil || p["online"] != true {
		return false
	}
	agents, _ := p["agents"].([]any)
	for _, a := range agents {
		m, _ := a.(map[string]any)
		if m["agent"] == "codex" && m["available"] == true && m["loggedIn"] == true {
			return true
		}
	}
	return false
}

// reasons lists the reason of every tool-approval-response.
func (s *stream) reasons() []string {
	var out []string
	for _, e := range s.of("tool-approval-response") {
		out = append(out, fmt.Sprint(e["payload"].(map[string]any)["reason"]))
	}
	return out
}

// ─── fake-Codex end to end ───────────────────────────────────────────────────

func TestE2EFakeCodex(t *testing.T) {
	env := loadE2E(t)
	_, archivistBin := testBinaries(t)
	codexBin := testCodexBinary(t)
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeChatAPI(t, env.key)
	home := t.TempDir()
	tmp := t.TempDir()
	owner := filepath.Join(home, "owner-codex")
	if err := os.MkdirAll(filepath.Join(owner, "rules"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"auth.json": `{"tokens":{}}`, "config.toml": "model = \"x\"\n", "rules/default.rules": "# owner\n"} {
		if err := os.WriteFile(filepath.Join(owner, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ownerSum := func() string {
		h := sha256.New()
		for _, n := range []string{"auth.json", "config.toml", "rules/default.rules"} {
			b, _ := os.ReadFile(filepath.Join(owner, n))
			h.Write(b)
		}
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	before := ownerSum()
	evidence := os.Getenv("CONNECT_E2E_EVIDENCE")
	daemonEnv := []string{
		"HOME=" + home, "PATH=" + filepath.Dir(codexBin) + ":" + os.Getenv("PATH"), "LANG=C.UTF-8", "TMPDIR=" + tmp,
		"ARCHIVIST_TOKEN=ak_0000000000000000e2e", "ARCHIVIST_BASE_URL=" + api.srv.URL, "ARCHIVIST_RELAY_URL=" + env.relayWS,
		"CODEX_HOME=" + owner, "OPENAI_API_KEY=sk-e2e-must-not-pass-0000000000", "CODEX_API_KEY=must-not-pass",
		"CODEX_ACCESS_TOKEN=must-not-pass",
	}
	c := &consumer{t: t, env: env, parser: parser}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Check row: the codex block is usable.
	chk := exec.Command(archivistBin, "connect", "--check")
	chk.Env = daemonEnv
	out, err := chk.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "codex\n  path:     "+codexBin) || !strings.Contains(string(out), "floor:    0.160.0 (ok)") ||
		strings.Count(string(out), "status:   usable") != 2 {
		t.Fatalf("--check: %v\n%s", err, out)
	}

	u := c.user(ctx)
	d := startDaemon(t, archivistBin, daemonEnv, "--codex-effort", "low")
	waitFor(t, 20*time.Second, "codex presence", u.codexOnline)

	sess := api.addSession("codex")
	c.register(sess, "active")
	sid := sess.SessionID
	s := c.session(ctx, sid)
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sid, "agent": "codex", "prompt": "remember kiwi-7817"})
	s.waitTurns(1, 30*time.Second)
	if st := s.statuses(); len(st) < 2 || st[0] != "starting" || st[1] != "running" {
		t.Fatalf("statuses %v", st)
	}
	msg := func(text string) {
		s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": text})
	}

	// Approval rows: accept, accept for session (+ identical command unprompted),
	// decline, relay timeout, cancel.
	msg("cmd touch a.txt")
	s.answer(sid, 1, "allow", "allow_once")
	s.waitTurns(2, 30*time.Second)
	msg("cmd touch b.txt")
	s.answer(sid, 2, "allow", "allow_always")
	s.waitTurns(3, 30*time.Second)
	msg("cmd touch b.txt")
	s.waitTurns(4, 30*time.Second)
	if n := len(s.of("tool-approval-request")); n != 2 {
		t.Fatalf("accept for session: %d requests", n)
	}
	msg("cmd rm a.txt")
	s.answer(sid, 3, "deny", "reject_once")
	s.waitTurns(5, 30*time.Second)
	msg("cmd rm b.txt")
	s.answer(sid, 4, "", "")
	s.waitTurns(6, 2*time.Minute) // relay alarm denies after 60 s
	msg("cmd rm c.txt")
	s.answer(sid, 5, "deny", "reject_always")
	s.waitTurns(7, 30*time.Second)
	waitFor(t, 10*time.Second, "cancel interrupts", func() bool { return len(s.of("abort")) == 1 && s.countStatus("interrupted") == 1 })
	if got := strings.Join(s.reasons(), ","); got != "accept,acceptForSession,decline,decline,cancel" {
		t.Fatalf("decisions %s", got)
	}

	// File change row.
	msg("file notes.txt")
	s.answer(sid, 6, "allow", "")
	s.waitTurns(8, 30*time.Second)
	if len(s.of("data-patch")) != 1 || !strings.Contains(s.text(), "wrote notes.txt") {
		t.Fatalf("file change row: %q", s.text())
	}

	// MCP row under the task token.
	msg("mcp")
	s.waitTurns(9, 30*time.Second)
	if !strings.Contains(s.text(), "tools: companies_search,read_passage,read_section,search,toc") {
		t.Fatalf("mcp text %q", s.text())
	}
	if b := api.researchBearers(); len(b) != 1 || !strings.HasPrefix(b[0], "Bearer mst_") {
		t.Fatalf("research bearers %v", b)
	}

	// Interrupt rows: streaming, then mid-command with a background terminal.
	msg("slow")
	waitFor(t, 15*time.Second, "slow output", func() bool { return strings.Contains(s.text(), "\n3\n") || strings.Contains(s.text(), "3\n4") })
	s.send(map[string]any{"kind": "interrupt", "correlationId": cmdID("int"), "sessionId": sid})
	s.waitTurns(10, 20*time.Second)
	msg("sleepcmd")
	s.answer(sid, 7, "allow", "")
	termPID := 0
	waitFor(t, 15*time.Second, "terminal pid", func() bool {
		b, err := os.ReadFile(filepath.Join(home, ".fakecodex", "terminal.pid"))
		_, _ = fmt.Sscan(string(b), &termPID)
		return err == nil && termPID > 0
	})
	s.send(map[string]any{"kind": "interrupt", "correlationId": cmdID("int"), "sessionId": sid})
	s.waitTurns(11, 20*time.Second)
	waitFor(t, 10*time.Second, "terminal killed", func() bool { return !processAlive(termPID) })
	msg("echo alive after interrupt")
	s.waitTurns(12, 20*time.Second)

	// Ctrl-C row.
	if code := d.ctrlC(t); code != 0 {
		t.Fatalf("Ctrl-C exit %d\n%s", code, d.out.String())
	}
	if left := append(sessionProcesses(home), sessionProcesses(tmp)...); len(left) != 0 {
		t.Fatalf("processes left: %v", left)
	}
	if live := api.liveTokens(); len(live) != 0 {
		t.Fatalf("live task tokens after Ctrl-C: %v", live)
	}
	waitFor(t, 10*time.Second, "disconnected status", func() bool { return s.countStatus("disconnected") == 1 })
	firstLog := d.out.String()

	// Resume row: a restarted daemon resumes the thread from the kept home.
	d = startDaemon(t, archivistBin, daemonEnv)
	waitFor(t, 20*time.Second, "presence again", u.codexOnline)
	msg("recall")
	s.waitTurns(13, 30*time.Second)
	if !strings.Contains(s.text(), "kiwi-7817") {
		t.Fatalf("resume did not recall: %q", s.text())
	}

	// Stop row: completed, session home removed, nothing left.
	sessionHome := filepath.Join(home, ".archivist", "connect", "codex-home", sid)
	if _, err := os.Stat(sessionHome); err != nil {
		t.Fatalf("session home missing before stop: %v", err)
	}
	s.send(map[string]any{"kind": "stop_session", "correlationId": cmdID("stop"), "sessionId": sid})
	waitFor(t, 20*time.Second, "completed", func() bool { return s.countStatus("completed") == 1 })
	waitFor(t, 10*time.Second, "home removed", func() bool { _, err := os.Stat(sessionHome); return os.IsNotExist(err) })
	if code := d.ctrlC(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, d.out.String())
	}
	if left := append(sessionProcesses(home), sessionProcesses(tmp)...); len(left) != 0 {
		t.Fatalf("processes left: %v", left)
	}
	s.check()
	if ownerSum() != before {
		t.Fatal("owner Codex files changed")
	}
	logs := firstLog + d.out.String()
	for _, secret := range []string{"sk-e2e-must-not-pass", "ak_0000000000000000e2e", "eyJ"} {
		if strings.Contains(logs, secret) {
			t.Errorf("daemon log leaks %q", secret)
		}
	}
	for _, r := range codexRuns(t, home) {
		for _, k := range r.EnvKeys {
			if k != "CODEX_HOME" && !EnvAllowed(k) {
				t.Errorf("child env carried %s", k)
			}
		}
		if r.CodexHome != sessionHome {
			t.Errorf("CODEX_HOME %s", r.CodexHome)
		}
	}
	if evidence != "" {
		_ = os.MkdirAll(evidence, 0o700)
		s.dump(filepath.Join(evidence, "fake-codex-session.events.jsonl"))
		_ = os.WriteFile(filepath.Join(evidence, "fake-codex-daemon-1.log"), []byte(firstLog), 0o600)
		_ = os.WriteFile(filepath.Join(evidence, "fake-codex-daemon-2.log"), []byte(d.out.String()), 0o600)
	}
}

// ─── live Codex (owner ChatGPT login, bounded) ──────────────────────────────

// codexProcesses lists Codex, mcp serve and command processes of
// archivist connect sessions, and any process still running a live row's
// marker command.
func codexProcesses(marker string) []string {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "ps -A") {
			continue
		}
		if liveSessionArgv.MatchString(line) || strings.Contains(line, "/.archivist/connect/codex-home/") ||
			(marker != "" && strings.Contains(line, marker)) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// countProcs counts processes whose argv contains substr.
func countProcs(substr string) int {
	out, _ := exec.Command("ps", "-A", "-o", "args=").Output()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, substr) && !strings.Contains(line, "ps -A") {
			n++
		}
	}
	return n
}

func TestE2ELiveCodex(t *testing.T) {
	if os.Getenv("CONNECT_E2E_LIVE") != "1" || os.Getenv("CONNECT_E2E_LIVE_AGENT") != "codex" {
		t.Skip("CONNECT_E2E_LIVE=1 CONNECT_E2E_LIVE_AGENT=codex runs the real Codex")
	}
	env := loadE2E(t)
	_, archivistBin := testBinaries(t)
	evidence := os.Getenv("CONNECT_E2E_EVIDENCE")
	if evidence == "" {
		t.Fatal("CONNECT_E2E_EVIDENCE is required for live runs")
	}
	parser, _ := mosaicevent.New()
	api := newFakeChatAPI(t, env.key)
	home, _ := os.UserHomeDir()
	daemonEnv := []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER"), "LOGNAME=" + os.Getenv("LOGNAME"),
		"SHELL=" + os.Getenv("SHELL"), "LANG=" + os.Getenv("LANG"), "TERM=" + os.Getenv("TERM"),
		"ARCHIVIST_TOKEN=ak_0000000000000000e2e", "ARCHIVIST_BASE_URL=" + api.srv.URL, "ARCHIVIST_RELAY_URL=" + env.relayWS,
		// Present in the daemon, must not reach Codex (they would replace the ChatGPT login).
		"OPENAI_API_KEY=sk-live-must-not-pass-000000000000", "CODEX_API_KEY=must-not-pass", "CODEX_ACCESS_TOKEN=must-not-pass",
	}
	args := []string{"--codex-model", "gpt-6-luna", "--codex-effort", "low"}
	c := &consumer{t: t, env: env, parser: parser}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ledger := func(row string) {
		f, err := os.OpenFile(filepath.Join(evidence, "live-ledger.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), row)
			_ = f.Close()
		}
		t.Log(row)
	}
	turnTimeout := 5 * time.Minute
	u := c.user(ctx)

	sess := api.addSession("codex")
	c.register(sess, "active")
	sid := sess.SessionID
	s := c.session(ctx, sid)
	defer s.dump(filepath.Join(evidence, "live-codex-session.events.jsonl"))
	n := 0
	turn := func(text string) {
		n++
		ledger(fmt.Sprintf("turn %d: %s", n, text))
		s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": text})
	}
	restartOnly := os.Getenv("CONNECT_E2E_LIVE_ROW") == "restart"

	ledger("app-server start 1 (daemon run 1) / turn 1: proof, isolation, codeword")
	d := startDaemon(t, archivistBin, daemonEnv, args...)
	defer func() {
		_ = os.WriteFile(filepath.Join(evidence, "live-codex-daemon-1.log"), []byte(d.out.String()), 0o600)
	}()
	waitFor(t, 60*time.Second, "codex presence", u.codexOnline)
	_ = os.WriteFile(filepath.Join(evidence, "live-codex-presence.json"), mustJSON(u.lastPresence()), 0o600)
	n++
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sid, "agent": "codex",
		"prompt": "Remember the codeword plum-7817 for later in this conversation. Do not run any command. Reply with only OK."})
	s.waitTurns(1, turnTimeout)
	if st := s.statuses(); len(st) < 2 || st[1] != "running" {
		t.Fatalf("statuses %v\n%s", st, d.out.String())
	}
	ledger(fmt.Sprintf("proof log line present: %v", strings.Contains(d.out.String(), "codex proof ok")))

	if !restartOnly {
		turn("Call the archivist MCP tool named search exactly once with query \"revenue\". Do not run shell commands. Reply with only the number of results it returned.")
		s.waitTurns(2, turnTimeout)
		ledger(fmt.Sprintf("research bearers seen by fake chat-api: %d (mst_ prefix: %v)", len(api.researchBearers()), allTask(api.researchBearers())))

		turn("Run exactly this shell command in the current directory: touch accept-7817.txt . Do not run any other command. Reply with only done.")
		s.answer(sid, 1, "allow", "allow_once")
		s.waitTurns(3, turnTimeout)

		turn("Run exactly this shell command in the current directory: touch session-7817.txt . Do not run any other command. Reply with only done.")
		s.answer(sid, 2, "allow", "allow_always")
		s.waitTurns(4, turnTimeout)
		turn("Run exactly the same shell command again: touch session-7817.txt . Do not run any other command. Reply with only done.")
		s.waitTurns(5, turnTimeout)
		ledger(fmt.Sprintf("accept for session: approval requests after the repeat %d (want 2)", len(s.of("tool-approval-request"))))

		turn("Run exactly this shell command in the current directory: touch decline-7817.txt . Do not run any other command. Reply with only done or declined.")
		s.answer(sid, len(s.of("tool-approval-request"))+1, "deny", "reject_once")
		s.waitTurns(6, turnTimeout)

		turn("Run exactly this shell command in the current directory: touch timeout-7817.txt . Do not run any other command. Reply with only done or declined.")
		s.answer(sid, len(s.of("tool-approval-request"))+1, "", "")
		s.waitTurns(7, turnTimeout)

		turn("Run exactly this shell command in the current directory: touch cancel-7817.txt . Do not run any other command. Reply with only done or declined.")
		s.answer(sid, len(s.of("tool-approval-request"))+1, "deny", "reject_always")
		s.waitTurns(8, turnTimeout)
		ledger(fmt.Sprintf("decisions %v; aborts %d; interrupted statuses %d", s.reasons(), len(s.of("abort")), s.countStatus("interrupted")))

		turn("Create a new file named notes-7817.txt containing the single line hello, by editing files (apply_patch), not with a shell command. Reply with only done.")
		reqs := len(s.of("tool-approval-request"))
		waitFor(t, turnTimeout, "file change approval or turn end", func() bool {
			return len(s.of("tool-approval-request")) > reqs || s.turnsDone() >= 9
		})
		if len(s.of("tool-approval-request")) > reqs {
			s.answer(sid, reqs+1, "allow", "allow_once")
		}
		s.waitTurns(9, turnTimeout)
		ledger(fmt.Sprintf("file change: approval requests %d -> %d, data-patch events %d", reqs, len(s.of("tool-approval-request")), len(s.of("data-patch"))))

		turn("Run exactly this shell command in the current directory: sleep 120 && echo slept-7817 . Then reply with its output.")
		s.answer(sid, len(s.of("tool-approval-request"))+1, "allow", "allow_once")
		time.Sleep(5 * time.Second)
		ledger(fmt.Sprintf("before interrupt: sleep 120 processes %d", countProcs("sleep 120")))
		s.send(map[string]any{"kind": "interrupt", "correlationId": cmdID("int"), "sessionId": sid})
		s.waitTurns(10, time.Minute)
		time.Sleep(3 * time.Second)
		left := countProcs("sleep 120")
		ledger(fmt.Sprintf("interrupt mid-command: aborts %d, interrupted statuses %d, sleep 120 processes left %d", len(s.of("abort")), s.countStatus("interrupted"), left))

		turn("Reply with only the word alive.")
		s.waitTurns(11, turnTimeout)
	}

	ledger("Ctrl-C daemon run 1")
	if code := d.ctrlC(t); code != 0 {
		t.Fatalf("Ctrl-C exit %d", code)
	}
	left := codexProcesses("7817")
	ledger(fmt.Sprintf("after Ctrl-C: session processes %d %v, live task tokens %d", len(left), left, len(api.liveTokens())))
	if len(left) != 0 || len(api.liveTokens()) != 0 {
		t.Fatalf("Ctrl-C left %v, tokens %v", left, api.liveTokens())
	}

	ledger(fmt.Sprintf("app-server start 2 (daemon run 2, thread/resume) / turn %d: recall", n+1))
	d2 := startDaemon(t, archivistBin, daemonEnv, args...)
	defer func() {
		_ = os.WriteFile(filepath.Join(evidence, "live-codex-daemon-2.log"), []byte(d2.out.String()), 0o600)
	}()
	waitFor(t, 60*time.Second, "presence again", u.codexOnline)
	before := s.text()
	turn("What codeword did I ask you to remember earlier in this conversation? Do not run any command. Reply with only the codeword.")
	s.waitTurns(n, turnTimeout)
	recalled := strings.Contains(strings.TrimPrefix(s.text(), before), "plum-7817")
	s.send(map[string]any{"kind": "stop_session", "correlationId": cmdID("stop"), "sessionId": sid})
	waitFor(t, time.Minute, "completed", func() bool { return s.countStatus("completed") == 1 })
	if code := d2.ctrlC(t); code != 0 {
		t.Fatalf("d2 exit %d", code)
	}
	left = codexProcesses("7817")
	_, homeErr := os.Stat(filepath.Join(home, ".archivist", "connect", "codex-home", sid))
	ledger(fmt.Sprintf("final: recalled plum-7817 %v; resumed: %v; session home removed %v; session processes %d; live task tokens %d; statuses %v",
		recalled, strings.Contains(d2.out.String(), "resume=true"), os.IsNotExist(homeErr), len(left), len(api.liveTokens()),
		slices.Compact(s.statuses())))
	if !recalled || len(left) != 0 || len(api.liveTokens()) != 0 {
		t.Fatalf("restart row failed: recalled %v, left %v, tokens %v", recalled, left, api.liveTokens())
	}
	s.check()
}
