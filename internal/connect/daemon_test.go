//go:build !windows

package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/client"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const testOwnerKey = "ak_test_owner_key_0001"

// harness runs a daemon in-process against the fake relay and chat-api, with
// the fake Claude Code and the real archivist binary for `mcp serve`.
type harness struct {
	t      *testing.T
	home   string
	tmp    string
	api    *fakeChatAPI
	relay  *fakeRelay
	log    *syncBuffer
	max    int
	d      *Daemon
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T, fakeCfg map[string]any) *harness {
	t.Helper()
	home := t.TempDir()
	if fakeCfg != nil {
		writeFakeClaudeConfig(t, home, fakeCfg)
	}
	return &harness{t: t, home: home, tmp: t.TempDir(), api: newFakeChatAPI(t, randomHexKey()), relay: newFakeRelay(t), log: &syncBuffer{}}
}

// daemonEnv is the daemon's own environment: allowed keys plus provider and
// archivist credentials that must never reach Claude.
func (h *harness) daemonEnv() []string {
	return []string{
		"HOME=" + h.home, "PATH=" + os.Getenv("PATH"), "USER=tester", "LANG=C.UTF-8", "LC_ALL=C",
		"XDG_CONFIG_HOME=" + filepath.Join(h.home, ".config"), "TMPDIR=" + h.tmp,
		"ANTHROPIC_API_KEY=sk-ant-api03-must-not-pass", "CLAUDE_CODE_OAUTH_TOKEN=oauth-must-not-pass",
		"CLAUDE_CONFIG_DIR=/nonexistent", "ARCHIVIST_TOKEN=" + testOwnerKey, "ARCHIVIST_BASE_URL=http://wrong.invalid",
		"OPENAI_API_KEY=must-not-pass", "CODEX_HOME=/nonexistent", "HERDR_ENV=1", "AWS_SECRET_ACCESS_KEY=must-not-pass",
	}
}

func (h *harness) start() {
	h.t.Helper()
	claudeBin, archivistBin := testBinaries(h.t)
	api := client.New(testOwnerKey, "test")
	api.BaseURL = h.api.srv.URL
	env := h.daemonEnv()
	childEnv, err := BuildChildEnv(env, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	lookPath := func(file string) (string, error) {
		if file == "claude" {
			return claudeBin, nil
		}
		return "", exec.ErrNotFound
	}
	d, err := New(Config{
		API:      api,
		RelayURL: h.relay.url(),
		StateDir: filepath.Join(h.home, ".archivist", "connect"),
		Claude: ClaudeConfig{Bin: claudeBin, SettingSources: DefaultSettingSources, Executable: archivistBin,
			BaseURL: h.api.srv.URL},
		MaxSessions: h.max,
		Log:         NewLogger(h.log),
		Detect: func(ctx context.Context) Detection {
			return Detect(ctx, lookPath, ExecRunner, childEnv, h.home)
		},
		Environ: func() []string { return env },
		TempDir: h.tmp,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.d, h.cancel, h.done = d, cancel, make(chan error, 1)
	go func() { h.done <- d.Run(ctx) }()
	waitFor(h.t, 10*time.Second, "user socket", func() bool { return h.relay.lastCaps() != nil })
	h.t.Cleanup(func() { _ = h.stop() })
}

// stop is Ctrl-C: cancel and wait for Run to return.
func (h *harness) stop() error {
	if h.cancel == nil {
		return nil
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		return err
	case <-time.After(30 * time.Second):
		h.t.Fatal("daemon did not stop within 30 s")
		return nil
	}
}

func (h *harness) startSession(prompt string) string {
	h.t.Helper()
	s := h.api.addSession("claude")
	cid := "start-" + randomUUID()
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": cid, "sessionId": s.SessionID,
		"agent": "claude", "prompt": prompt})
	waitFor(h.t, 15*time.Second, "start ack", func() bool { return h.relay.acked(cid) })
	return s.SessionID
}

func (h *harness) message(sid, text string) string {
	cid := "msg-" + randomUUID()
	h.relay.send(sid, map[string]any{"kind": "user_message", "correlationId": cid, "sessionId": sid, "text": text})
	return cid
}

func (h *harness) command(sid, kind string) string {
	cid := kind + "-" + randomUUID()
	h.relay.send(sid, map[string]any{"kind": kind, "correlationId": cid, "sessionId": sid})
	return cid
}

func (h *harness) waitFinishes(sid string, n int) {
	h.t.Helper()
	waitFor(h.t, 30*time.Second, fmt.Sprintf("%d finished turns", n), func() bool {
		return len(h.relay.eventsOf(sid, "finish"))+len(h.relay.eventsOf(sid, "abort")) >= n
	})
}

// approve answers the newest open approval request the way the relay does.
func (h *harness) approve(sid string, n int, decision, reason, scope string) map[string]any {
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
	payload := req["payload"].(map[string]any)
	cmd := map[string]any{"kind": "approval_response", "correlationId": "resolved:" + req["correlationId"].(string),
		"sessionId": sid, "approvalId": payload["approvalId"], "decision": decision, "reason": reason}
	if scope != "" {
		cmd["scope"] = scope
	}
	h.relay.send(sid, cmd)
	return payload
}

func (h *harness) record(sid string) *SessionRecord {
	h.t.Helper()
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		h.t.Fatal(err)
	}
	rec, err := st.Load(sid)
	if err != nil {
		h.t.Fatal(err)
	}
	return rec
}

// strayProcesses lists processes whose command line mentions this test's
// HOME (fake claude config, mcp serve token file) or TMPDIR.
func (h *harness) strayProcesses() []string {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	var stray []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, h.home) || strings.Contains(line, h.tmp) {
			if !strings.Contains(line, "ps -A") {
				stray = append(stray, strings.TrimSpace(line))
			}
		}
	}
	return stray
}

var allowedChildKeys = map[string]bool{"HOME": true, "PATH": true, "USER": true, "LANG": true, "LC_ALL": true,
	"XDG_CONFIG_HOME": true, "TMPDIR": true}

func TestDaemonSessionLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	caps := h.relay.lastCaps()
	if len(caps) != 1 || caps[0] != (Capability{Agent: "claude", Version: "2.1.280", Available: true, LoggedIn: true}) {
		t.Fatalf("capabilities %+v", caps)
	}

	sid := h.startSession("echo hello world")
	h.waitFinishes(sid, 1)
	if got := h.relay.statuses(sid); len(got) < 2 || got[0] != "starting" || got[1] != "running" {
		t.Fatalf("statuses %v", got)
	}
	if !strings.Contains(h.relay.text(sid), "hello world") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	first := h.relay.payloads(sid)
	if first[0]["type"] != "data-session-status" || first[2]["type"] != "start" {
		t.Fatalf("event order %v", first[:3])
	}

	// Child environment and fixed flags.
	runs := fakeRuns(t, h.home)
	if len(runs) != 1 {
		t.Fatalf("claude runs %d", len(runs))
	}
	for _, k := range runs[0].EnvKeys {
		if !allowedChildKeys[k] {
			t.Errorf("child env carries %s", k)
		}
	}
	rec := h.record(sid)
	args := strings.Join(runs[0].Args, " ")
	for _, want := range []string{"--permission-prompt-tool stdio", "--strict-mcp-config", "--tools " + ClaudeBuiltinTools,
		"--allowedTools mcp__archivist__companies_search,mcp__archivist__read_passage,mcp__archivist__read_section,mcp__archivist__search,mcp__archivist__toc",
		"--add-dir " + rec.Cwd} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if runs[0].Cwd != rec.Cwd || !strings.HasPrefix(rec.Cwd, h.tmp) {
		t.Errorf("cwd %q, record %q", runs[0].Cwd, rec.Cwd)
	}
	runDir := filepath.Join(h.home, ".archivist", "connect", "run", sid)
	for _, f := range []string{"task-token", "mcp.json"} {
		st, err := os.Stat(filepath.Join(runDir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, st, err)
		}
	}
	if strings.HasPrefix(runDir, rec.Cwd) {
		t.Error("private files inside the session cwd")
	}

	// Approvals: allow, timeout deny, then reject_always remembered locally.
	h.message(sid, "bash touch a")
	h.approve(sid, 1, "allow", "user", "allow_once")
	h.waitFinishes(sid, 2)
	h.message(sid, "bash ls")
	h.approve(sid, 2, "deny", "timeout", "")
	h.waitFinishes(sid, 3)
	h.message(sid, "bash rm b")
	h.approve(sid, 3, "deny", "user", "reject_always")
	h.waitFinishes(sid, 4)
	h.message(sid, "bash rm c")
	h.waitFinishes(sid, 5)
	if n := len(h.relay.eventsOf(sid, "tool-approval-request")); n != 3 {
		t.Errorf("approval requests %d, want 3 (the fourth answered locally)", n)
	}
	var responses []map[string]any
	for _, e := range h.relay.eventsOf(sid, "tool-approval-response") {
		responses = append(responses, e["payload"].(map[string]any))
	}
	if len(responses) != 4 || responses[0]["approved"] != true || responses[1]["approved"] != false ||
		!strings.Contains(fmt.Sprint(responses[1]["reason"]), "timeout") || !strings.Contains(fmt.Sprint(responses[3]["reason"]), "for this session") {
		t.Errorf("approval responses %v", responses)
	}
	if n := len(h.relay.eventsOf(sid, "data-permission-scope")); n != 2 {
		t.Errorf("permission scope events %d", n)
	}
	outputs := h.relay.eventsOf(sid, "tool-output-available")
	if len(outputs) == 0 || outputs[0]["payload"].(map[string]any)["output"] != "ran: touch a" {
		t.Errorf("tool outputs %v", outputs)
	}

	// Interrupt: the turn aborts, the process survives, the next turn runs.
	h.message(sid, "slow")
	waitFor(t, 10*time.Second, "slow output", func() bool { return strings.Contains(h.relay.text(sid), "3\n") })
	icid := h.command(sid, "interrupt")
	h.waitFinishes(sid, 6)
	waitFor(t, 5*time.Second, "interrupt ack", func() bool { return h.relay.acked(icid) })
	if len(h.relay.eventsOf(sid, "abort")) != 1 {
		t.Fatal("no abort event")
	}
	h.relay.waitStatus(t, sid, "interrupted", 1)
	h.message(sid, "echo after interrupt")
	h.waitFinishes(sid, 7)
	if !strings.Contains(h.relay.text(sid), "after interrupt") || len(fakeRuns(t, h.home)) != 1 {
		t.Fatal("the process did not survive the interrupt")
	}
	// An interrupt with no live turn is acknowledged only.
	icid = h.command(sid, "interrupt")
	waitFor(t, 5*time.Second, "idle interrupt ack", func() bool { return h.relay.acked(icid) })

	// MCP: task mode tools only, and the call carries the task token.
	h.message(sid, "mcp")
	h.waitFinishes(sid, 8)
	if !strings.Contains(h.relay.text(sid), "tools: companies_search,read_passage,read_section,search,toc") {
		t.Fatalf("mcp tools text: %q", h.relay.text(sid))
	}
	bearers := h.api.researchBearers()
	if len(bearers) != 1 || !strings.HasPrefix(bearers[0], "Bearer mst_") {
		t.Fatalf("research bearers %v", bearers)
	}

	// Stop: completed, token revoked, process gone, local state ended.
	pid := runs[0].PID
	scid := h.command(sid, "stop_session")
	h.relay.waitStatus(t, sid, "completed", 1)
	waitFor(t, 10*time.Second, "stop ack", func() bool { return h.relay.acked(scid) })
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
	if processAlive(pid) {
		t.Error("claude still running after stop")
	}
	if rec := h.record(sid); rec.Status != "ended" {
		t.Errorf("record status %s", rec.Status)
	}
	if _, err := os.Stat(rec.Cwd); !os.IsNotExist(err) {
		t.Error("cwd not removed")
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Error("run dir not removed")
	}
	if len(h.relay.invalid) != 0 {
		t.Errorf("relay saw invalid events: %v", h.relay.invalid)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logs := h.log.String()
	for _, secret := range []string{"sk-ant-api03-SECRET", "Bearer abc.def", testOwnerKey, "eyJ"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q", secret)
		}
	}
	if !strings.Contains(logs, "harness stderr") || !strings.Contains(logs, "[redacted]") {
		t.Error("harness stderr not forwarded scrubbed")
	}
}

func TestDaemonRestartResumesAndCtrlCCleansUp(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("remember kiwi-7816")
	h.waitFinishes(sid, 1)
	h.message(sid, "mcp")
	h.waitFinishes(sid, 2)
	h.message(sid, "spawn")
	h.waitFinishes(sid, 3)
	spawned, err := os.ReadFile(filepath.Join(h.home, ".fakeclaude", "spawned.pid"))
	if err != nil {
		t.Fatal(err)
	}
	sleepPID, _ := strconv.Atoi(string(spawned))
	claudeID := h.record(sid).ClaudeSessionID
	if claudeID == "" {
		t.Fatal("no claude session id recorded")
	}
	if len(h.strayProcesses()) == 0 {
		t.Fatal("expected live session processes before Ctrl-C")
	}

	// Ctrl-C.
	if err := h.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stray := h.strayProcesses(); len(stray) != 0 {
		t.Fatalf("processes left after Ctrl-C: %v", stray)
	}
	if processAlive(sleepPID) {
		t.Fatal("detached grandchild survived")
	}
	if live := h.api.liveTokens(); len(live) != 0 {
		t.Fatalf("task tokens not revoked: %v", live)
	}
	h.relay.waitStatus(t, sid, "disconnected", 1)
	if rec := h.record(sid); rec.Status != "active" {
		t.Fatalf("record %s after Ctrl-C", rec.Status)
	}

	// Restart: the session socket reattaches; a message resumes Claude.
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sid) >= 2 })
	h.message(sid, "recall")
	h.waitFinishes(sid, 4)
	if !strings.Contains(h.relay.text(sid), "kiwi-7816") {
		t.Fatalf("resumed session did not recall: %q", h.relay.text(sid))
	}
	runs := fakeRuns(t, h.home)
	resumed := false
	for _, r := range runs {
		for _, a := range r.Args {
			if a == "--resume="+claudeID {
				resumed = true
			}
		}
	}
	if !resumed {
		t.Fatalf("no run resumed %s", claudeID)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if stray := h.strayProcesses(); len(stray) != 0 {
		t.Fatalf("processes left: %v", stray)
	}
}

func TestDaemonRefusesFramesOutsideClosedSet(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sess := h.api.addSession("claude")
	sid := sess.SessionID
	// Executable data in any form is refused before anything runs.
	for i, raw := range []string{
		`{"kind":"exec","correlationId":"x-1","sessionId":"` + sid + `","binary":"/bin/sh"}`,
		`{"kind":"start_session","correlationId":"x-2","sessionId":"` + sid + `","agent":"claude","prompt":"hi","binary":"/bin/sh"}`,
		`{"kind":"start_session","correlationId":"x-3","sessionId":"` + sid + `","agent":"claude","prompt":"hi","args":["--dangerously-skip-permissions"]}`,
		`{"kind":"start_session","correlationId":"x-4","sessionId":"` + sid + `","agent":"/usr/bin/claude","prompt":"hi"}`,
		`{"kind":"user_message","correlationId":"x-5","sessionId":"` + sid + `","text":"hi"}`,
		`{"kind":"approval_response","correlationId":"resolved:x","sessionId":"` + sid + `","approvalId":"a","decision":"allow","reason":"user"}`,
		`not json`,
	} {
		h.relay.sendRaw("", raw)
		if i < 6 {
			cid := fmt.Sprintf("x-%d", i+1)
			if i == 5 {
				cid = "resolved:x"
			}
			waitFor(t, 5*time.Second, "ack "+cid, func() bool { return h.relay.acked(cid) })
		}
	}
	// A terminal receipt is acknowledged with its session id, never executed.
	h.relay.sendRaw("", `{"kind":"approval_response","correlationId":"resolved:req-1","sessionId":"`+sid+`","approvalId":"a","decision":"allow","reason":"user","terminalReceipt":true}`)
	waitFor(t, 5*time.Second, "receipt ack", func() bool { return h.relay.acked("resolved:req-1") })
	time.Sleep(300 * time.Millisecond)
	if runs := fakeRuns(t, h.home); len(runs) != 0 {
		t.Fatalf("refused frames started claude: %v", runs)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".archivist", "connect", "sessions", sid+".json")); !os.IsNotExist(err) {
		t.Fatal("refused start wrote a session record")
	}
	if n := strings.Count(h.log.String(), "not executed"); n < 7 {
		t.Fatalf("refusals logged %d times:\n%s", n, h.log.String())
	}

	// Session socket: extra fields and unknown kinds are refused too.
	sid2 := h.startSession("echo one")
	h.waitFinishes(sid2, 1)
	h.relay.sendRaw(sid2, `{"kind":"user_message","correlationId":"y-1","sessionId":"`+sid2+`","text":"echo two","argv":["sh"]}`)
	h.relay.sendRaw(sid2, `{"kind":"start_session","correlationId":"y-2","sessionId":"`+sid2+`","agent":"claude","prompt":"x"}`)
	waitFor(t, 5*time.Second, "session refusal acks", func() bool { return h.relay.acked("y-1") && h.relay.acked("y-2") })
	time.Sleep(300 * time.Millisecond)
	if len(h.relay.eventsOf(sid2, "finish")) != 1 {
		t.Fatal("refused session frame ran a turn")
	}
	// A duplicate delivery of an executed command is acknowledged, not rerun.
	cid := h.message(sid2, "echo three")
	h.waitFinishes(sid2, 2)
	h.relay.sendRaw(sid2, `{"kind":"user_message","correlationId":"`+cid+`","sessionId":"`+sid2+`","text":"echo three"}`)
	time.Sleep(500 * time.Millisecond)
	if len(h.relay.eventsOf(sid2, "finish")) != 2 {
		t.Fatal("duplicate command ran twice")
	}
}

func TestDaemonSubscriptionProofFailsClosed(t *testing.T) {
	h := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiKeySource": "ANTHROPIC_API_KEY", "permissionMode": "default"})
	h.start()
	sid := h.startSession("echo should not run")
	h.relay.waitStatus(t, sid, "failed", 1)
	if strings.Contains(h.relay.text(sid), "should not run") || len(h.relay.eventsOf(sid, "error")) == 0 {
		t.Fatal("output escaped a failed proof")
	}
	runs := fakeRuns(t, h.home)
	waitFor(t, 10*time.Second, "claude killed", func() bool { return len(runs) == 1 && !processAlive(runs[0].PID) })
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
	h.message(sid, "echo again")
	h.relay.waitStatus(t, sid, "failed", 2)
	if len(fakeRuns(t, h.home)) != 1 {
		t.Fatal("a failed session ran another turn")
	}
	if h.record(sid).Status != "failed" {
		t.Fatal("record not failed")
	}

	// permissionMode other than default fails the same way.
	h2 := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiKeySource": "none", "permissionMode": "auto"})
	h2.start()
	sid2 := h2.startSession("echo nope")
	h2.relay.waitStatus(t, sid2, "failed", 1)

	// A non-subscription login is reported unavailable and refused at start.
	h3 := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "console", "apiKeySource": "none", "permissionMode": "default"})
	h3.start()
	if caps := h3.relay.lastCaps(); len(caps) != 1 || caps[0].Available || !caps[0].LoggedIn {
		t.Fatalf("console login capabilities %+v", caps)
	}
	sid3 := h3.startSession("echo nope")
	h3.relay.waitStatus(t, sid3, "failed", 1)
	if len(fakeRuns(t, h3.home)) != 0 {
		t.Fatal("claude started without a subscription login")
	}
}

func TestDaemonStartRefusals(t *testing.T) {
	h := newHarness(t, nil)
	h.max = 1
	h.start()
	// Unsupported agent: acked, failed status on the session socket, no process.
	codex := h.api.addSession("codex")
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "c-1", "sessionId": codex.SessionID, "agent": "codex", "prompt": "hi"})
	waitFor(t, 10*time.Second, "codex ack", func() bool { return h.relay.acked("c-1") })
	h.relay.waitStatus(t, codex.SessionID, "failed", 1)

	// Unknown session: acked, nothing else.
	unknown := randomUUID()
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "c-2", "sessionId": unknown, "agent": "claude", "prompt": "hi"})
	waitFor(t, 10*time.Second, "unknown ack", func() bool { return h.relay.acked("c-2") })

	// Over the cap: the second live session is refused with a failed status.
	busy := h.startSession("slow")
	waitFor(t, 10*time.Second, "slow output", func() bool { return strings.Contains(h.relay.text(busy), "1\n") })
	over := h.startSession("echo over")
	h.relay.waitStatus(t, over, "failed", 1)
	if len(fakeRuns(t, h.home)) != 1 {
		t.Fatal("cap exceeded")
	}
	// Redelivery of an executed start is acknowledged only.
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "dup-" + busy, "sessionId": busy, "agent": "claude", "prompt": "slow"})
	waitFor(t, 5*time.Second, "dup ack", func() bool { return h.relay.acked("dup-" + busy) })
	if len(fakeRuns(t, h.home)) != 1 {
		t.Fatal("redelivered start spawned again")
	}
}

func TestDaemonSupersededAndFeatureDisabled(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.relay.closeUser(closeSuperseed)
	select {
	case err := <-h.done:
		h.cancel = nil
		if !errors.Is(err, ErrSuperseded) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon kept running after 4000")
	}

	h2 := newHarness(t, nil)
	h2.api.off = true
	claudeBin, archivistBin := testBinaries(t)
	api := client.New(testOwnerKey, "test")
	api.BaseURL = h2.api.srv.URL
	d, err := New(Config{API: api, RelayURL: h2.relay.url(), StateDir: filepath.Join(h2.home, "state"),
		Claude: ClaudeConfig{Bin: claudeBin, Executable: archivistBin}, Log: NewLogger(h2.log),
		Detect: func(context.Context) Detection { return Detection{} }, Environ: h2.daemonEnv})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Run(context.Background())
	var fatal *FatalError
	if !errors.As(err, &fatal) || fatal.Code != "FEATURE_DISABLED" {
		t.Fatalf("Run = %v", err)
	}
}

func TestDaemonSessionEndedRemotely(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	pid := fakeRuns(t, h.home)[0].PID
	h.api.endSession(sid)
	h.relay.closeSession(sid, closeExpired)
	waitFor(t, 15*time.Second, "local stop", func() bool {
		st, _ := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
		rec, err := st.Load(sid)
		return err == nil && rec.Status == "ended"
	})
	waitFor(t, 10*time.Second, "claude stopped", func() bool { return !processAlive(pid) })
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
}
