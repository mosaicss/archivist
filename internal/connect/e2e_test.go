//go:build e2e && !windows

// End-to-end test of `archivist connect` against a local `wrangler dev` of
// the 78.14 agent relay, a fake chat-api (78.15 routes, tickets signed with
// the relay's local key) and a Go stand-in for the workspace consumer.
//
//	CONNECT_E2E_RELAY_URL=http://127.0.0.1:8787 \
//	CONNECT_E2E_DEV_VARS=<relay dir>/.dev.vars \
//	go test -tags e2e -run TestE2E -v ./internal/connect/
//
// CONNECT_E2E_LIVE=1 replaces the fake Claude Code with the real one
// (owner's subscription login, real HOME) for TestE2ELive only, with
// CONNECT_E2E_EVIDENCE naming a directory for the transcript.
package connect

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/mosaicevent"
)

type e2eEnv struct {
	relayHTTP string
	relayWS   string
	key       []byte
	secret    string
}

func loadE2E(t *testing.T) e2eEnv {
	t.Helper()
	relay := os.Getenv("CONNECT_E2E_RELAY_URL")
	vars := os.Getenv("CONNECT_E2E_DEV_VARS")
	if relay == "" || vars == "" {
		t.Skip("set CONNECT_E2E_RELAY_URL and CONNECT_E2E_DEV_VARS to run against wrangler dev")
	}
	data, err := os.ReadFile(vars)
	if err != nil {
		t.Fatal(err)
	}
	env := e2eEnv{relayHTTP: strings.TrimRight(relay, "/")}
	env.relayWS = "ws" + strings.TrimPrefix(env.relayHTTP, "http")
	for _, line := range strings.Split(string(data), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "RELAY_TICKET_KEY":
			env.key, err = hex.DecodeString(v)
			if err != nil {
				t.Fatal("RELAY_TICKET_KEY is not hex")
			}
		case "RELAY_CONSUMER_SECRET":
			env.secret = v
		}
	}
	if len(env.key) != 32 || env.secret == "" {
		t.Fatal(".dev.vars lacks the relay key or consumer secret")
	}
	return env
}

// sessionCopy returns a snapshot of a session record.
func (f *fakeChatAPI) sessionCopy(id string) *client.AgentSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *f.session[id]
	return &c
}

// mintCount is every successful relay ticket and task token mint.
func (f *fakeChatAPI) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.mints
	for _, c := range f.tickets {
		n += c
	}
	return n
}

// ticketMints counts relay tickets for a session ("" = user scope).
func (f *fakeChatAPI) ticketMints(sid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tickets[sid]
}

// ─── consumer (the workspace side of the relay) ─────────────────────────────

type consumer struct {
	t      *testing.T
	env    e2eEnv
	parser *mosaicevent.Parser
}

func (c *consumer) headers() http.Header {
	return http.Header{"x-relay-secret": {c.env.secret}, "x-relay-user": {testOwner}}
}

// register records the chat-api session with the relay (POST /create).
func (c *consumer) register(s *client.AgentSession, status string) {
	c.t.Helper()
	rec := map[string]any{"sessionId": s.SessionID, "ownerId": testOwner, "agent": s.Agent, "title": s.Title,
		"status": status, "createdAt": s.CreatedAt, "expiresAt": s.ExpiresAt, "endedAt": nil}
	path := "create"
	if status == "ended" {
		rec["endedAt"] = *s.EndedAt
		path = "end"
	}
	body, _ := json.Marshal(rec)
	req, _ := http.NewRequest("POST", c.env.relayHTTP+"/sessions/"+s.SessionID+"/"+path, bytes.NewReader(body))
	req.Header = c.headers()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		c.t.Fatalf("relay %s: %d %s", path, resp.StatusCode, b.String())
	}
}

type userConsumer struct {
	ws       *websocket.Conn
	mu       sync.Mutex
	presence []map[string]any
	frames   []map[string]any
}

func (c *consumer) user(ctx context.Context) *userConsumer {
	c.t.Helper()
	ws, _, err := websocket.Dial(ctx, c.env.relayWS+"/users/"+testOwner+"/ws?role=consumer", &websocket.DialOptions{HTTPHeader: c.headers()})
	if err != nil {
		c.t.Fatal(err)
	}
	u := &userConsumer{ws: ws}
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var x map[string]any
			if json.Unmarshal(data, &x) != nil {
				continue
			}
			u.mu.Lock()
			u.frames = append(u.frames, x)
			if x["kind"] == "presence" {
				u.presence = append(u.presence, x)
			}
			u.mu.Unlock()
		}
	}()
	c.t.Cleanup(func() { _ = ws.CloseNow() })
	return u
}

func (u *userConsumer) lastPresence() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.presence) == 0 {
		return nil
	}
	return u.presence[len(u.presence)-1]
}

func (u *userConsumer) send(t *testing.T, v any) {
	b, _ := json.Marshal(v)
	if err := u.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// claudeOnline reports a fresh presence frame with a usable claude.
func (u *userConsumer) claudeOnline() bool {
	p := u.lastPresence()
	if p == nil || p["online"] != true {
		return false
	}
	agents, _ := p["agents"].([]any)
	for _, a := range agents {
		m, _ := a.(map[string]any)
		if m["agent"] == "claude" && m["available"] == true && m["loggedIn"] == true {
			return true
		}
	}
	return false
}

// stream is one consumer session socket with replay, validation and acks.
type stream struct {
	t      *testing.T
	ws     *websocket.Conn
	parser *mosaicevent.Parser
	mu     sync.Mutex
	events []map[string]any
	raw    []string
	cursor int64
	errs   []string
	gaps   int
}

func (c *consumer) session(ctx context.Context, sid string) *stream {
	c.t.Helper()
	ws, _, err := websocket.Dial(ctx, c.env.relayWS+"/sessions/"+sid+"/ws?role=consumer&cursor=0", &websocket.DialOptions{HTTPHeader: c.headers()})
	if err != nil {
		c.t.Fatal(err)
	}
	ws.SetReadLimit(32 << 20)
	s := &stream{t: c.t, ws: ws, parser: c.parser}
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var x map[string]any
			if json.Unmarshal(data, &x) != nil {
				continue
			}
			switch x["kind"] {
			case "event":
				_, perr := c.parser.Parse(data, "envelope")
				seq := int64(x["seq"].(float64))
				s.mu.Lock()
				if perr != nil {
					s.errs = append(s.errs, fmt.Sprintf("invalid event %s: %v", data, perr))
				}
				if seq <= s.cursor {
					s.mu.Unlock()
					continue
				}
				if seq != s.cursor+1 {
					s.gaps++
				}
				s.cursor = seq
				s.events = append(s.events, x)
				s.raw = append(s.raw, string(data))
				s.mu.Unlock()
				ack, _ := json.Marshal(map[string]any{"kind": "ack", "seq": seq})
				_ = ws.Write(ctx, websocket.MessageText, ack)
			case "error":
				s.mu.Lock()
				s.errs = append(s.errs, "relay error "+fmt.Sprint(x["code"]))
				s.mu.Unlock()
			}
		}
	}()
	c.t.Cleanup(func() { _ = ws.CloseNow() })
	return s
}

func (s *stream) send(v any) {
	b, _ := json.Marshal(v)
	if err := s.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stream) of(typ string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, e := range s.events {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func (s *stream) statuses() []string {
	var out []string
	for _, e := range s.of("data-session-status") {
		out = append(out, e["payload"].(map[string]any)["data"].(map[string]any)["status"].(string))
	}
	return out
}

func (s *stream) countStatus(status string) int {
	n := 0
	for _, st := range s.statuses() {
		if st == status {
			n++
		}
	}
	return n
}

func (s *stream) text() string {
	var b strings.Builder
	for _, e := range s.of("text-delta") {
		b.WriteString(e["payload"].(map[string]any)["delta"].(string))
	}
	return b.String()
}

func (s *stream) turnsDone() int { return len(s.of("finish")) + len(s.of("abort")) }

func (s *stream) waitTurns(n int, timeout time.Duration) {
	s.t.Helper()
	waitFor(s.t, timeout, fmt.Sprintf("%d turns", n), func() bool { return s.turnsDone() >= n })
}

func (s *stream) check() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) > 0 || s.gaps > 0 {
		s.t.Fatalf("stream errors %v, gaps %d", s.errs, s.gaps)
	}
}

func (s *stream) dump(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.WriteFile(path, []byte(strings.Join(s.raw, "\n")+"\n"), 0o600)
}

// answer resolves the n-th approval request the way the workspace does.
func (s *stream) answer(sid string, n int, decision, scope string) map[string]any {
	s.t.Helper()
	var req map[string]any
	waitFor(s.t, 3*time.Minute, "approval request", func() bool {
		reqs := s.of("tool-approval-request")
		if len(reqs) >= n {
			req = reqs[n-1]
			return true
		}
		return false
	})
	if decision != "" {
		cmd := map[string]any{"kind": "approval_response", "correlationId": req["correlationId"], "sessionId": sid, "decision": decision}
		if scope != "" {
			cmd["scope"] = scope
		}
		s.send(cmd)
	}
	return req
}

func cmdID(kind string) string { return kind + "-" + randomUUID() }

// ─── daemon subprocess ───────────────────────────────────────────────────────

type daemonProc struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

func startDaemon(t *testing.T, bin string, env []string, args ...string) *daemonProc {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"connect"}, args...)...)
	cmd.Env = env
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &daemonProc{cmd: cmd, out: out, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Signal(syscall.SIGINT)
			select {
			case <-p.done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	})
	return p
}

// ctrlC sends SIGINT and returns the exit code.
func (p *daemonProc) ctrlC(t *testing.T) int {
	t.Helper()
	_ = p.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-p.done:
	case <-time.After(45 * time.Second):
		t.Fatalf("daemon ignored Ctrl-C:\n%s", p.out.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

func (p *daemonProc) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		t.Fatalf("daemon still running:\n%s", p.out.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

// sessionProcesses lists processes whose argv contains marker.
func sessionProcesses(marker string) []string {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, marker) && !strings.Contains(line, "ps -A") {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// liveSessionProcesses lists claude and mcp serve processes of archivist
// connect sessions: their argv names the private run dir or a session cwd.
var liveSessionArgv = regexp.MustCompile(`\.archivist/connect/run/|/archivist-connect-[0-9]+`)

func liveSessionProcesses() []string {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if liveSessionArgv.MatchString(line) && !strings.Contains(line, "ps -A") {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// ─── fake-Claude end to end ──────────────────────────────────────────────────

func TestE2EFakeClaude(t *testing.T) {
	env := loadE2E(t)
	claudeBin, archivistBin := testBinaries(t)
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeChatAPI(t, env.key)
	api.ticketTTL = 40 * time.Second // rotate sockets during the test
	home := t.TempDir()
	tmp := t.TempDir()
	evidence := os.Getenv("CONNECT_E2E_EVIDENCE")
	daemonEnv := []string{
		"HOME=" + home, "PATH=" + filepath.Dir(claudeBin) + ":" + os.Getenv("PATH"), "LANG=C.UTF-8", "TMPDIR=" + tmp,
		"ARCHIVIST_TOKEN=ak_0000000000000000e2e", "ARCHIVIST_BASE_URL=" + api.srv.URL, "ARCHIVIST_RELAY_URL=" + env.relayWS,
		"ANTHROPIC_API_KEY=sk-ant-api03-e2e-must-not-pass", "CLAUDE_CODE_OAUTH_TOKEN=must-not-pass",
	}
	c := &consumer{t: t, env: env, parser: parser}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Check row.
	chk := exec.Command(archivistBin, "connect", "--check")
	chk.Env = daemonEnv
	out, err := chk.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "2.1.280") || !strings.Contains(string(out), "status:   usable") {
		t.Fatalf("--check: %v\n%s", err, out)
	}
	// mc_pat_ is refused before any network call.
	refuse := exec.Command(archivistBin, "connect")
	refuse.Env = append(append([]string(nil), daemonEnv...), "ARCHIVIST_TOKEN=mc_pat_0000000000000")
	if out, err := refuse.CombinedOutput(); err == nil || refuse.ProcessState.ExitCode() != 4 || api.mintCount() != 0 {
		t.Fatalf("mc_pat_ connect: %v %s", err, out)
	}

	// Connect row: presence with the claude capability.
	u := c.user(ctx)
	d := startDaemon(t, archivistBin, daemonEnv)
	waitFor(t, 20*time.Second, "claude presence", u.claudeOnline)

	// Start row.
	sess := api.addSession("claude")
	c.register(sess, "active")
	s := c.session(ctx, sess.SessionID)
	sid := sess.SessionID
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sid, "agent": "claude", "prompt": "remember kiwi-7816"})
	s.waitTurns(1, 30*time.Second)
	if st := s.statuses(); len(st) < 2 || st[0] != "starting" || st[1] != "running" {
		t.Fatalf("statuses %v", st)
	}

	// Approval rows: allow, deny, relay timeout.
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "bash touch allowed.txt"})
	s.answer(sid, 1, "allow", "allow_once")
	s.waitTurns(2, 30*time.Second)
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "bash rm denied.txt"})
	s.answer(sid, 2, "deny", "reject_once")
	s.waitTurns(3, 30*time.Second)
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "bash rm timeout.txt"})
	s.answer(sid, 3, "", "")
	s.waitTurns(4, 2*time.Minute) // relay alarm denies after 60 s
	resp := s.of("tool-approval-response")
	if len(resp) != 3 {
		t.Fatalf("approval responses %d", len(resp))
	}
	reasons := []string{}
	for _, r := range resp {
		p := r["payload"].(map[string]any)
		reasons = append(reasons, fmt.Sprint(p["approved"], ":", p["reason"]))
	}
	if !strings.HasPrefix(reasons[0], "true") || !strings.Contains(reasons[1], "user rejected") || !strings.Contains(reasons[2], "timeout") {
		t.Fatalf("approval outcomes %v", reasons)
	}
	if !strings.Contains(s.text(), "denied: Denied: no approval arrived before the approval timeout.") {
		t.Fatalf("harness did not receive the timeout deny: %q", s.text())
	}

	// Interrupt row.
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "slow"})
	waitFor(t, 15*time.Second, "slow output", func() bool { return strings.Contains(s.text(), "\n3\n") || strings.Contains(s.text(), "3\n4") })
	s.send(map[string]any{"kind": "interrupt", "correlationId": cmdID("int"), "sessionId": sid})
	s.waitTurns(5, 20*time.Second)
	// The interrupted status follows the abort event.
	waitFor(t, 10*time.Second, "abort and interrupted status", func() bool {
		return len(s.of("abort")) == 1 && s.countStatus("interrupted") == 1
	})
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "echo alive after interrupt"})
	s.waitTurns(6, 20*time.Second)

	// MCP row.
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "mcp"})
	s.waitTurns(7, 30*time.Second)
	if !strings.Contains(s.text(), "tools: companies_search,read_passage,read_section,search,toc") {
		t.Fatalf("mcp text %q", s.text())
	}
	if b := api.researchBearers(); len(b) != 1 || !strings.HasPrefix(b[0], "Bearer mst_") {
		t.Fatalf("research bearers %v", b)
	}

	// Ticket expiry row: 40 s tickets rotate the sockets; work continues.
	time.Sleep(15 * time.Second)
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "echo after rotation"})
	s.waitTurns(8, 30*time.Second)
	if api.ticketMints(sid) < 2 || api.ticketMints("") < 2 {
		t.Fatalf("tickets were not re-minted: user %d session %d", api.ticketMints(""), api.ticketMints(sid))
	}

	// Ctrl-C row: no process left, tokens revoked.
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
	// Resume row: a restarted daemon resumes the same Claude session.
	d = startDaemon(t, archivistBin, daemonEnv)
	waitFor(t, 20*time.Second, "presence again", u.claudeOnline)
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": "recall"})
	s.waitTurns(9, 30*time.Second)
	if !strings.Contains(s.text(), "kiwi-7816") {
		t.Fatalf("resume did not recall: %q", s.text())
	}

	// Superseded row: a second daemon takes over; the first exits.
	home2 := t.TempDir()
	env2 := append([]string(nil), daemonEnv...)
	env2[0] = "HOME=" + home2
	d2 := startDaemon(t, archivistBin, env2)
	if code := d.wait(t, 30*time.Second); code != 1 || !strings.Contains(d.out.String(), "another archivist connect took over") {
		t.Fatalf("superseded daemon exit %d\n%s", code, d.out.String())
	}
	if left := sessionProcesses(home); len(left) != 0 {
		t.Fatalf("processes left after supersede: %v", left)
	}

	// Stop row on a fresh session served by the second daemon.
	waitFor(t, 20*time.Second, "presence d2", u.claudeOnline)
	sess2 := api.addSession("claude")
	c.register(sess2, "active")
	s2 := c.session(ctx, sess2.SessionID)
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sess2.SessionID, "agent": "claude", "prompt": "echo second daemon"})
	s2.waitTurns(1, 30*time.Second)
	s2.send(map[string]any{"kind": "stop_session", "correlationId": cmdID("stop"), "sessionId": sess2.SessionID})
	waitFor(t, 20*time.Second, "completed", func() bool { return s2.countStatus("completed") == 1 })
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(api.liveTokens()) == 0 })

	// Terminal receipt row: end a session with an open approval; the relay
	// denies it and hands the receipt to the user socket; the daemon acks it
	// and executes nothing.
	sess3 := api.addSession("claude")
	c.register(sess3, "active")
	s3 := c.session(ctx, sess3.SessionID)
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sess3.SessionID, "agent": "claude", "prompt": "bash touch never.txt"})
	s3.answer(sess3.SessionID, 1, "", "")
	api.endSession(sess3.SessionID)
	c.register(api.sessionCopy(sess3.SessionID), "ended")
	waitFor(t, 60*time.Second, "terminal receipt ack", func() bool {
		return strings.Contains(d2.out.String(), "terminal approval receipt") && strings.Contains(d2.out.String(), "relay reports the session ended")
	})

	if code := d2.ctrlC(t); code != 0 {
		t.Fatalf("d2 exit %d\n%s", code, d2.out.String())
	}
	if left := sessionProcesses(home2); len(left) != 0 {
		t.Fatalf("processes left: %v", left)
	}
	if live := api.liveTokens(); len(live) != 0 {
		t.Fatalf("live tokens: %v", live)
	}
	for _, st := range []*stream{s, s2, s3} {
		st.check()
	}
	logs := firstLog + d.out.String() + d2.out.String()
	for _, secret := range []string{"sk-ant-api03", "ak_0000000000000000e2e", "Bearer abc.def", "eyJ"} {
		if strings.Contains(logs, secret) {
			t.Errorf("daemon log leaks %q", secret)
		}
	}
	for _, r := range fakeRuns(t, home) {
		for _, k := range r.EnvKeys {
			if !EnvAllowed(k) {
				t.Errorf("child env carried %s", k)
			}
		}
	}
	if evidence != "" {
		_ = os.MkdirAll(evidence, 0o700)
		s.dump(filepath.Join(evidence, "fake-session-1.events.jsonl"))
		s2.dump(filepath.Join(evidence, "fake-session-2.events.jsonl"))
		s3.dump(filepath.Join(evidence, "fake-session-3.events.jsonl"))
		_ = os.WriteFile(filepath.Join(evidence, "fake-daemon-1.log"), []byte(firstLog), 0o600)
		_ = os.WriteFile(filepath.Join(evidence, "fake-daemon-1-restarted.log"), []byte(d.out.String()), 0o600)
		_ = os.WriteFile(filepath.Join(evidence, "fake-daemon-2.log"), []byte(d2.out.String()), 0o600)
	}
}

// ─── live Claude Code (owner subscription, bounded) ─────────────────────────

func TestE2ELive(t *testing.T) {
	if os.Getenv("CONNECT_E2E_LIVE") != "1" {
		t.Skip("CONNECT_E2E_LIVE=1 runs the real Claude Code")
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
		// Present in the daemon, must not reach Claude (it would turn apiKeySource away from none).
		"ANTHROPIC_API_KEY=sk-ant-api03-live-must-not-pass", "CLAUDE_CODE_OAUTH_TOKEN=must-not-pass", "CLAUDE_CONFIG_DIR=/nonexistent",
	}
	args := []string{"--claude-model", "claude-sonnet-5", "--claude-effort", "low"}
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
	if os.Getenv("CONNECT_E2E_LIVE_ROW") == "restart" {
		liveRestart(t, c, u, api, archivistBin, daemonEnv, args, evidence, ledger, turnTimeout)
		return
	}
	d := startDaemon(t, archivistBin, daemonEnv, args...)
	defer func() { _ = os.WriteFile(filepath.Join(evidence, "live-daemon-1.log"), []byte(d.out.String()), 0o600) }()
	waitFor(t, 60*time.Second, "claude presence", u.claudeOnline)
	_ = os.WriteFile(filepath.Join(evidence, "live-presence.json"), mustJSON(u.lastPresence()), 0o600)

	sess := api.addSession("claude")
	c.register(sess, "active")
	sid := sess.SessionID
	s := c.session(ctx, sid)
	defer s.dump(filepath.Join(evidence, "live-session.events.jsonl"))
	turn := func(n int, text string) {
		ledger(fmt.Sprintf("turn %d: %s", n, text))
		s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid, "text": text})
	}

	ledger("start 1 (daemon run 1) / turn 1: subscription proof + codeword")
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sid, "agent": "claude",
		"prompt": "Remember the codeword kiwi-7816 for later in this conversation. Reply with only OK."})
	s.waitTurns(1, turnTimeout)
	if st := s.statuses(); len(st) < 2 || st[1] != "running" {
		t.Fatalf("statuses %v\n%s", st, d.out.String())
	}

	turn(2, "Call the mcp__archivist__search tool exactly once with query \"revenue\" and reply with only the number of results it returned.")
	s.waitTurns(2, turnTimeout)
	ledger(fmt.Sprintf("research bearers seen by fake chat-api: %d (mst_ prefix: %v)", len(api.researchBearers()), allTask(api.researchBearers())))

	turn(3, "Run exactly this shell command with the Bash tool in the current directory: touch allow-7816.txt . Reply with only done.")
	s.answer(sid, 1, "allow", "allow_once")
	s.waitTurns(3, turnTimeout)

	turn(4, "Run exactly this shell command with the Bash tool in the current directory: touch deny-7816.txt . Reply with only done.")
	s.answer(sid, 2, "deny", "reject_once")
	s.waitTurns(4, turnTimeout)

	turn(5, "Run exactly this shell command with the Bash tool in the current directory: touch timeout-7816.txt . Reply with only done.")
	s.answer(sid, 3, "", "")
	s.waitTurns(5, turnTimeout)

	turn(6, "Count from 1 to 300, one number per line, no other text.")
	waitFor(t, turnTimeout, "counting output", func() bool { return strings.Contains(s.text(), "\n5\n") })
	s.send(map[string]any{"kind": "interrupt", "correlationId": cmdID("int"), "sessionId": sid})
	s.waitTurns(6, time.Minute)
	ledger(fmt.Sprintf("interrupt: aborts %d, interrupted statuses %d", len(s.of("abort")), s.countStatus("interrupted")))

	turn(7, "Reply with only the word alive.")
	s.waitTurns(7, turnTimeout)

	ledger("Ctrl-C daemon run 1")
	if code := d.ctrlC(t); code != 0 {
		t.Fatalf("Ctrl-C exit %d", code)
	}
	left := liveSessionProcesses()
	ledger(fmt.Sprintf("after Ctrl-C: session processes %d, live task tokens %d", len(left), len(api.liveTokens())))
	if len(left) != 0 || len(api.liveTokens()) != 0 {
		t.Fatalf("Ctrl-C left %v, tokens %v", left, api.liveTokens())
	}

	ledger("start 2 (daemon run 2, resume) / turn 8: recall")
	d2 := startDaemon(t, archivistBin, daemonEnv, args...)
	defer func() { _ = os.WriteFile(filepath.Join(evidence, "live-daemon-2.log"), []byte(d2.out.String()), 0o600) }()
	waitFor(t, 60*time.Second, "presence again", u.claudeOnline)
	turn(8, "What codeword did I ask you to remember earlier in this conversation? Reply with only the codeword.")
	s.waitTurns(8, turnTimeout)
	s.send(map[string]any{"kind": "stop_session", "correlationId": cmdID("stop"), "sessionId": sid})
	waitFor(t, time.Minute, "completed", func() bool { return s.countStatus("completed") == 1 })
	if code := d2.ctrlC(t); code != 0 {
		t.Fatalf("d2 exit %d", code)
	}
	left = liveSessionProcesses()
	ledger(fmt.Sprintf("final: session processes %d, live task tokens %d, recalled kiwi: %v", len(left), len(api.liveTokens()), strings.Contains(s.text(), "kiwi-7816")))
	s.check()
}

// liveRestart runs the restart row on its own: a fresh session remembers a
// codeword, the daemon is stopped with Ctrl-C and restarted, and the next
// message resumes the same Claude session (2 process starts, 2 turns).
func liveRestart(t *testing.T, c *consumer, u *userConsumer, api *fakeChatAPI, archivistBin string, daemonEnv, args []string,
	evidence string, ledger func(string), turnTimeout time.Duration) {
	ctx := context.Background()
	sess := api.addSession("claude")
	c.register(sess, "active")
	sid := sess.SessionID
	s := c.session(ctx, sid)
	defer s.dump(filepath.Join(evidence, "live-restart.events.jsonl"))

	ledger(fmt.Sprintf("restart row: start A (daemon run A) / turn R1: codeword, session %s", sid))
	d := startDaemon(t, archivistBin, daemonEnv, args...)
	waitFor(t, 60*time.Second, "presence", u.claudeOnline)
	u.send(t, map[string]any{"kind": "start_session", "correlationId": cmdID("start"), "sessionId": sid, "agent": "claude",
		"prompt": "Remember the codeword plum-7816 for later in this conversation. Reply with only OK."})
	s.waitTurns(1, turnTimeout)
	firstText := s.text()
	if code := d.ctrlC(t); code != 0 {
		t.Fatalf("Ctrl-C exit %d", code)
	}
	_ = os.WriteFile(filepath.Join(evidence, "live-restart-daemon-A.log"), []byte(d.out.String()), 0o600)
	left := liveSessionProcesses()
	ledger(fmt.Sprintf("restart row: after Ctrl-C session processes %d, live task tokens %d", len(left), len(api.liveTokens())))
	if len(left) != 0 || len(api.liveTokens()) != 0 {
		t.Fatalf("Ctrl-C left %v, tokens %v", left, api.liveTokens())
	}

	ledger("restart row: start B (daemon run B, --resume) / turn R2: recall")
	d = startDaemon(t, archivistBin, daemonEnv, args...)
	waitFor(t, 60*time.Second, "presence again", u.claudeOnline)
	s.send(map[string]any{"kind": "user_message", "correlationId": cmdID("msg"), "sessionId": sid,
		"text": "What codeword did I ask you to remember earlier in this conversation? Reply with only the codeword."})
	s.waitTurns(2, turnTimeout)
	recalled := strings.Contains(strings.TrimPrefix(s.text(), firstText), "plum-7816")
	s.send(map[string]any{"kind": "stop_session", "correlationId": cmdID("stop"), "sessionId": sid})
	waitFor(t, time.Minute, "completed", func() bool { return s.countStatus("completed") == 1 })
	if code := d.ctrlC(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_ = os.WriteFile(filepath.Join(evidence, "live-restart-daemon-B.log"), []byte(d.out.String()), 0o600)
	left = liveSessionProcesses()
	ledger(fmt.Sprintf("restart row: recalled plum-7816 %v; resumed with --resume: %v; session processes %d; live task tokens %d",
		recalled, strings.Contains(d.out.String(), "resume=true"), len(left), len(api.liveTokens())))
	if !recalled || len(left) != 0 || len(api.liveTokens()) != 0 {
		t.Fatalf("restart row failed: recalled %v, left %v, tokens %v", recalled, left, api.liveTokens())
	}
	s.check()
}

func allTask(bearers []string) bool {
	for _, b := range bearers {
		if !strings.HasPrefix(b, "Bearer mst_") {
			return false
		}
	}
	return len(bearers) > 0
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}
