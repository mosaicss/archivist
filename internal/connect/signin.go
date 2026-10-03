package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/creack/pty"
	"github.com/sourcegraph/jsonrpc2"
)

// Posture-1 sign-in (Story 78.22). In the session-bound sandbox mode a
// harness that is installed but logged out signs in before the first turn:
// the user gets a data-auth-prompt on the session socket and completes the
// login in a browser. mosaic-event/1 has no code or expiry fields, so the
// code (Codex) and the expiry travel in the prompt's message. The login
// result is never trusted on its own: the session's normal subscription
// proof (claude auth status, Codex account/read) runs afterwards.

// signInTimeout bounds one sign-in (a var so tests can shorten it).
var signInTimeout = 5 * time.Minute

// signInResult is how a sign-in ended.
type signInResult int

const (
	signedIn      signInResult = iota // proceed to the normal start and its proof
	signInFailed                      // reported; the session failed
	signInStopped                     // stop_session arrived; the session ended
	signInAborted                     // daemon stop or session socket end; nothing reported
)

// needsSignIn reports whether this session's harness must sign in first.
func (s *session) needsSignIn() bool {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return s.d.signIn != "" && s.d.signIn == s.rec.Agent
}

// signIn runs the harness's posture-1 login and settles the session on
// failure or stop.
func (s *session) signIn(ctx context.Context) signInResult {
	deadline := time.Now().Add(signInTimeout)
	s.log.Printf("%s is not logged in; starting the sign-in (%v)", s.harness(), signInTimeout)
	var res signInResult
	var err error
	if s.isCodex() {
		res, err = s.signInCodex(ctx, deadline)
	} else {
		res, err = s.signInClaude(ctx, deadline)
	}
	switch res {
	case signedIn:
		s.log.Printf("%s sign-in completed; running the login proof", s.harness())
		s.d.signedIn(s.rec.Agent)
	case signInFailed:
		msg := Scrub(s.harness() + " sign-in failed: " + err.Error())
		s.log.Printf("%s", msg)
		s.authFailed = true
		s.emitError(msg)
		s.failSession(msg)
		s.emitStatus("failed", msg)
	case signInStopped:
		s.log.Printf("sign-in abandoned: stop_session")
		s.stop(true)
	case signInAborted:
		s.log.Printf("sign-in abandoned: %v", err)
	}
	return res
}

// signInCommand runs one relay command during a sign-in. It returns the
// text of a user_message, or stop for stop_session; interrupts and
// approvals have nothing to act on yet and are only acknowledged.
func (s *session) signInCommand(c sessionCmd) (text string, stop bool) {
	if !s.admit(c) {
		return "", false
	}
	switch c.in.Kind {
	case "user_message":
		return c.in.Text, false
	case "stop_session":
		return "", true
	}
	return "", false
}

// emitAuthPrompt sends the sign-in card (mosaic-event/1 data-auth-prompt).
func (s *session) emitAuthPrompt(provider, promptID, link, message string) {
	s.flushCoalesced()
	data := map[string]any{"promptId": promptID, "provider": provider, "message": message}
	if link != "" {
		data["url"] = link
	}
	s.outbox.Emit(Chunk{"type": "data-auth-prompt", "data": data})
}

// expiryText is the plain-English expiry carried in a prompt message.
func expiryText(deadline time.Time) string {
	mins := int(time.Until(deadline).Round(time.Minute) / time.Minute)
	in := "less than a minute"
	switch {
	case mins == 1:
		in = "1 minute"
	case mins > 1:
		in = fmt.Sprintf("%d minutes", mins)
	}
	return fmt.Sprintf("It expires at %s UTC (in %s).", deadline.UTC().Format("15:04"), in)
}

// httpsURL accepts an absolute https URL whose host is hostSuffix or one of
// its subdomains ("" accepts any host).
func httpsURL(raw string, hostSuffixes ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 4000 {
		return false
	}
	if len(hostSuffixes) == 0 {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hostSuffixes {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// ─── Codex: ChatGPT device code over app-server ─────────────────────────────

// codexLoginHosts are the hosts a Codex device code link may point at.
var codexLoginHosts = []string{"openai.com", "chatgpt.com"}

// codexLoginArgs is the login app-server's fixed argv: the ChatGPT login
// stored as a file (auth.json in the owner's home, where session homes link
// it), and nothing else switched on.
func codexLoginArgs() []string {
	set := [][2]string{
		{"forced_login_method", `"chatgpt"`},
		{"cli_auth_credentials_store", `"file"`},
		{"check_for_update_on_startup", "false"},
		{"analytics.enabled", "false"},
	}
	args := []string{"app-server", "--stdio", "--strict-config"}
	for _, kv := range set {
		args = append(args, "-c", kv[0]+"="+kv[1])
	}
	return args
}

// signInCodex runs account/login/start {type: chatgptDeviceCode} in an
// app-server whose CODEX_HOME is the owner's Codex home, so auth.json lands
// where every session home links it. It waits for account/login/completed
// and cancels the login (account/login/cancel) at the deadline.
func (s *session) signInCodex(ctx context.Context, deadline time.Time) (signInResult, error) {
	cfg := s.d.codex
	home := cfg.OwnerHome
	if home == "" || !filepath.IsAbs(home) {
		return signInFailed, errors.New("the owner's Codex home is unknown")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return signInFailed, fmt.Errorf("the owner's Codex home: %w", err)
	}
	env, err := BuildChildEnv(s.d.environ(), nil)
	if err != nil {
		return signInFailed, err
	}
	// The one adapter-set key; BuildChildEnv refuses CODEX_ overrides.
	env = append(env, "CODEX_HOME="+home)
	proc, err := StartProc(ProcSpec{Bin: cfg.Bin, Args: codexLoginArgs(), Env: env, Dir: s.rec.Cwd, Log: s.log})
	if err != nil {
		return signInFailed, fmt.Errorf("start codex: %w", err)
	}
	s.log.Printf("codex login app-server started (%s, env keys %v)", proc, EnvKeys(env))
	rpc := newCodexRPC(proc, s.log)
	defer func() {
		rpc.close()
		proc.Stop(stopGrace)
	}()
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	failed := func(what string, err error) (signInResult, error) {
		if ctx.Err() != nil {
			return signInAborted, ctx.Err()
		}
		return signInFailed, fmt.Errorf("%s: %w", what, err)
	}
	var init codexInitializeResult
	if err := rpc.conn.Call(hctx, "initialize", codexInitializeParams{
		ClientInfo:   codexClientInfo{Name: "archivist_connect", Title: "archivist connect", Version: s.d.appVersion},
		Capabilities: codexCapabilities{ExperimentalAPI: true},
	}, &init); err != nil {
		return failed("initialize", err)
	}
	if !samePath(init.CodexHome, home) {
		return signInFailed, fmt.Errorf("the login app-server runs with home %q, not the owner's Codex home", init.CodexHome)
	}
	if err := rpc.conn.Notify(hctx, "initialized", nil); err != nil {
		return failed("initialized", err)
	}
	var start codexLoginStartResult
	if err := rpc.conn.Call(hctx, "account/login/start", codexLoginStartParams{Type: "chatgptDeviceCode"}, &start); err != nil {
		return failed("account/login/start", err)
	}
	if start.Type != "chatgptDeviceCode" || start.LoginID == "" || start.UserCode == "" || len(start.UserCode) > 64 ||
		!httpsURL(start.VerificationURL, codexLoginHosts...) {
		return signInFailed, errors.New("the device code login Codex returned is unusable")
	}
	cancelLogin := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		var r codexLoginCancelResult
		if err := rpc.conn.Call(cctx, "account/login/cancel", codexLoginCancelParams{LoginID: start.LoginID}, &r); err != nil {
			s.log.Printf("account/login/cancel failed: %v", err)
			return
		}
		s.log.Printf("codex login cancelled (%s)", r.Status)
	}
	s.emitAuthPrompt("codex", start.LoginID, start.VerificationURL, fmt.Sprintf(
		"Sign in to Codex with ChatGPT: open the link, sign in, then enter the code %s. %s "+
			"Device code login must be enabled in your ChatGPT security settings.",
		start.UserCode, expiryText(deadline)))
	s.log.Printf("codex device code sign-in prompt sent (login %s)", start.LoginID)

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-rpc.q.wake:
			for _, ev := range rpc.q.take() {
				switch {
				case ev.Notif && ev.Method == "account/login/completed":
					var done codexLoginCompleted
					if json.Unmarshal(ev.Params, &done) != nil {
						continue
					}
					if done.LoginID != nil && *done.LoginID != start.LoginID {
						continue // another login's completion
					}
					if !done.Success {
						msg := "ChatGPT sign-in did not complete"
						if done.Error != nil && *done.Error != "" {
							msg += ": " + truncateString(*done.Error, 300)
						}
						return signInFailed, errors.New(msg)
					}
					if st, err := os.Stat(filepath.Join(home, "auth.json")); err != nil || !st.Mode().IsRegular() {
						return signInFailed, fmt.Errorf("the login completed but no auth.json was written in %s", home)
					}
					return signedIn, nil
				case !ev.Notif && ev.Call == "":
					// A server request: nothing is granted during a sign-in.
					_ = rpc.replyError(ev.ID, jsonrpc2.CodeMethodNotFound, "not supported during archivist connect sign-in: "+ev.Method)
				}
			}
		case <-timer.C:
			cancelLogin()
			return signInFailed, fmt.Errorf("no sign-in within %v; the login was cancelled", signInTimeout)
		case <-ctx.Done():
			cancelLogin()
			return signInAborted, ctx.Err()
		case c := <-s.cmds:
			text, stop := s.signInCommand(c)
			if stop {
				cancelLogin()
				return signInStopped, nil
			}
			if text != "" {
				s.early = append(s.early, text) // sent after the first turn
			}
		case err := <-s.linkErr:
			s.linkErr <- err // the run loop settles the socket end
			cancelLogin()
			return signInAborted, err
		case <-proc.Done():
			return signInFailed, errors.New("codex exited during the sign-in")
		case <-rpc.conn.DisconnectNotify():
			return signInFailed, errors.New("the connection to codex closed during the sign-in")
		}
	}
}

// ─── Claude Code: claude auth login --claudeai under a PTY ──────────────────

// claudeLoginArgs is the fixed sign-in argv (a claude.ai subscription).
var claudeLoginArgs = []string{"auth", "login", "--claudeai"}

// claudeLoginHosts are the hosts a Claude sign-in link may point at.
var claudeLoginHosts = []string{"claude.com", "claude.ai", "anthropic.com"}

var (
	// ansiRe matches the terminal escapes Claude Code prints: CSI sequences
	// and OSC sequences (the hyperlink wrapper) ended by BEL or ST.
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[=>()][0-9A-Za-z]?`)
	// loginURLRe finds the first https URL after the escapes are removed.
	loginURLRe = regexp.MustCompile(`https://[^\s"'<>]+`)
)

// claudeLoginURL returns the sign-in link Claude Code printed, or "".
// Verified on Claude Code 2.1.285 (2026-10-03): it prints "If the browser
// didn't open, visit: <url>" (also as an OSC 8 hyperlink), then "Paste code
// here if prompted >".
func claudeLoginURL(out []byte) string {
	plain := ansiRe.ReplaceAllString(string(out), "")
	for _, m := range loginURLRe.FindAllString(plain, -1) {
		m = strings.TrimRight(m, ".,;)")
		if httpsURL(m, claudeLoginHosts...) {
			return m
		}
	}
	return ""
}

// signInCode is a pasted sign-in code: one token of printable text, so an
// ordinary chat message is never typed into the login as a code.
func signInCode(text string) (string, bool) {
	code := strings.TrimSpace(text)
	if code == "" || len(code) > 2000 {
		return "", false
	}
	for _, r := range code {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return "", false
		}
	}
	return code, true
}

// signInClaude runs `claude auth login --claudeai` under a PTY, sends its
// link as the prompt, writes the next user_message as the pasted code and,
// after the command exits, re-runs the claude auth status proof. The PTY
// output echoes the code, so none of it is logged.
func (s *session) signInClaude(ctx context.Context, deadline time.Time) (signInResult, error) {
	bin := s.d.claude.Bin
	env, err := BuildChildEnv(s.d.environ(), nil)
	if err != nil {
		return signInFailed, err
	}
	pt, err := startPTY(bin, claudeLoginArgs, env, s.rec.Cwd)
	if err != nil {
		return signInFailed, fmt.Errorf("start claude auth login: %w", err)
	}
	s.log.Printf("claude auth login started under a PTY (pid %d, env keys %v)", pt.pid(), EnvKeys(env))
	defer pt.kill()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	prompted, codes, entered := false, 0, false
	var mark int // output length when the last code was written
	for {
		select {
		case <-pt.changed:
			out := pt.output()
			if !prompted {
				if link := claudeLoginURL(out); link != "" {
					prompted = true
					s.emitAuthPrompt("claude", "claude-signin-"+s.outbox.RunID(), link,
						"Sign in to Claude Code with your Claude subscription: open the link and sign in, "+
							"then send the code the page shows as your next message. "+expiryText(deadline))
					s.log.Printf("claude sign-in prompt sent")
				}
			}
			if codes > 0 && !entered && strings.Contains(strings.ToLower(string(out[mark:])), "login successful") {
				// Some versions wait for Enter after a successful login.
				entered = true
				_ = pt.write("\r")
			}
		case <-pt.done:
			exitErr := pt.exitErr()
			switch {
			case !prompted:
				return signInFailed, fmt.Errorf("claude auth login exited before printing a sign-in link (%v)", exitErr)
			case codes == 0:
				return signInFailed, fmt.Errorf("claude auth login exited before a code was sent (%v)", exitErr)
			case exitErr != nil:
				return signInFailed, fmt.Errorf("claude auth login did not accept the code (%v)", exitErr)
			}
			st, err := ClaudeAuthStatus(ctx, s.d.runner, env, s.rec.Cwd, bin)
			if err != nil {
				return signInFailed, fmt.Errorf("claude auth status after the sign-in: %w", err)
			}
			if !st.Subscription() {
				return signInFailed, fmt.Errorf("after the sign-in, Claude Code is not logged in with a claude.ai subscription (loggedIn=%v, authMethod=%q)",
					st.LoggedIn, st.AuthMethod)
			}
			return signedIn, nil
		case <-timer.C:
			return signInFailed, fmt.Errorf("no sign-in within %v; the login was stopped", signInTimeout)
		case <-ctx.Done():
			return signInAborted, ctx.Err()
		case c := <-s.cmds:
			text, stop := s.signInCommand(c)
			if stop {
				return signInStopped, nil
			}
			if text == "" {
				continue
			}
			code, ok := signInCode(text)
			if !prompted || !ok {
				s.emitError("That message is not a sign-in code; send only the code the sign-in page shows.")
				continue
			}
			mark = len(pt.output())
			if err := pt.write(code + "\r"); err != nil {
				return signInFailed, fmt.Errorf("write the code to claude auth login: %w", err)
			}
			codes++
			entered = false
			s.log.Printf("sign-in code %d written to claude auth login", codes)
		case err := <-s.linkErr:
			s.linkErr <- err // the run loop settles the socket end
			return signInAborted, err
		}
	}
}

// ─── PTY child ──────────────────────────────────────────────────────────────

// maxPTYOutput bounds the retained PTY output.
const maxPTYOutput = 256 << 10

// ptyProc is a child on a pseudo-terminal in its own session (and process
// group), registered like a harness so the orphan sweep never takes it.
type ptyProc struct {
	cmd     *exec.Cmd
	f       *os.File
	changed chan struct{}
	done    chan struct{}

	mu   sync.Mutex
	buf  []byte
	exit error
	once sync.Once
}

// startPTY starts bin with exactly env and dir on a new PTY. The terminal is
// wide so a long sign-in link is never wrapped.
func startPTY(bin string, args, env []string, dir string) (*ptyProc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	procRegistry.mu.Lock()
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 4000})
	if err != nil {
		procRegistry.mu.Unlock()
		return nil, err
	}
	procRegistry.live[cmd.Process.Pid] = &Proc{cmd: cmd}
	procRegistry.mu.Unlock()
	p := &ptyProc{cmd: cmd, f: f, changed: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		b := make([]byte, 32<<10)
		for {
			n, err := f.Read(b)
			if n > 0 {
				p.mu.Lock()
				if len(p.buf)+n <= maxPTYOutput {
					p.buf = append(p.buf, b[:n]...)
				}
				p.mu.Unlock()
				select {
				case p.changed <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return // EIO once the child side is closed
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		procRegistry.mu.Lock()
		delete(procRegistry.live, cmd.Process.Pid)
		procRegistry.mu.Unlock()
		// Let the reader take the child's last output before Done.
		time.Sleep(100 * time.Millisecond)
		p.mu.Lock()
		p.exit = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (p *ptyProc) pid() int { return p.cmd.Process.Pid }

// output is everything the child printed so far (bounded).
func (p *ptyProc) output() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.buf...)
}

func (p *ptyProc) exitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

// write types s into the terminal.
func (p *ptyProc) write(s string) error {
	_, err := p.f.Write([]byte(s))
	return err
}

// kill ends the child's process group (TERM, then KILL) and closes the PTY.
// Safe to call twice and after the child exited.
func (p *ptyProc) kill() {
	p.once.Do(func() {
		pgid := p.pid()
		select {
		case <-p.done:
		default:
			if groupAlive(pgid) {
				signalGroup(pgid, sigTerm)
				select {
				case <-p.done:
				case <-time.After(3 * time.Second):
				}
			}
		}
		if groupAlive(pgid) {
			signalGroup(pgid, sigKill)
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
		_ = p.f.Close()
	})
}
