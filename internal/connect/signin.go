package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// login in a browser. The prompt is a mosaic-event/2 data-auth-prompt: it
// carries the Codex device code as code and the sign-in deadline as
// expiresAt, and its message keeps the full text (code and expiry included)
// so a renderer that ignores the v2 fields still works. The login result is
// never trusted on its own: the session's normal subscription proof (claude
// auth status, Codex account/read) runs afterwards.

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

// emitAuthPrompt sends the sign-in card (a mosaic-event/2 data-auth-prompt;
// the outbox stamps that version on this event type only). code is set only
// when it fits the contract; expiresAt is the sign-in deadline.
func (s *session) emitAuthPrompt(provider, promptID, link, message, code string, deadline time.Time) {
	s.flushCoalesced()
	s.outbox.Emit(authPromptChunk(provider, promptID, link, message, code, deadline))
}

// authPromptChunk builds the data-auth-prompt chunk. A code the contract
// would refuse is left out (the message still carries it) rather than
// dropping the whole prompt as an invalid event.
func authPromptChunk(provider, promptID, link, message, code string, deadline time.Time) Chunk {
	data := map[string]any{"promptId": promptID, "provider": provider, "message": message,
		"expiresAt": deadline.UnixMilli()}
	if link != "" {
		data["url"] = link
	}
	if authCode(code) {
		data["code"] = code
	}
	return Chunk{"type": "data-auth-prompt", "data": data}
}

// authCode reports whether code fits the mosaic-event/2 data-auth-prompt
// code: 1 to 64 printable ASCII characters, no whitespace ("!" to "~").
func authCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '!' || code[i] > '~' {
			return false
		}
	}
	return true
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
		start.UserCode, expiryText(deadline)), start.UserCode, deadline)
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

// ─── Claude Code: claude auth login under a PTY ─────────────────────────────

// claudeLoginArgs is the fixed sign-in argv (a claude.ai subscription).
var claudeLoginArgs = []string{"auth", "login", "--claudeai"}

// claudeConsoleLoginArgs is the Console sign in argv (Story 78.38, sandbox
// only, on the user's request). Verified on Claude Code 2.1.289
// (2026-10-05): it prints the same "visit: <url>" and paste prompt as the
// claude.ai login, with a platform.claude.com link.
var claudeConsoleLoginArgs = []string{"auth", "login", "--console"}

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

// ─── sandbox sign in methods (Story 78.38) ──────────────────────────────────

// Anthropic's hosting condition for Claude Code forbids removing a built in
// sign in method, so in the session-bound sandbox mode the Claude sign in
// card also offers a Console account and the user's own API key. The
// sandbox Worker writes the user's choice to the sign in request file
// (ARCHIVIST_SIGNIN_FILE, atomically): {"id", "method": "console"} or
// {"id", "method": "api_key", "key"}. In the Mosaic sandbox the key is a
// fixed placeholder that the Worker's egress swaps for the user's key on
// api.anthropic.com only. While a Claude sign in waits the session reads
// the file about every second and acts once per new id; an unreadable,
// unparseable or unknown request, or a seen id, is ignored. console
// restarts the login as `claude auth login --console` with a new prompt
// and a fresh sign in window (signInTimeout from the switch, but never past
// twice signInTimeout from the sign in's start); api_key ends the login and
// runs the session in API key mode. A login that already exited or reported
// success is settled first: a request never replaces a completed login.

// signinPollEvery is how often a waiting Claude sign in reads the request
// file (a var so tests can shorten it).
var signinPollEvery = time.Second

// maxSigninFile bounds the request file read.
const maxSigninFile = 4096

// signinRequest is one sign in request. Key is never logged.
type signinRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Key    string `json:"key"`
}

// readSigninRequest reads the sign in request file. ok is false when the
// file is missing, unreadable, too large or not a valid request; nothing
// read from the file ever reaches an error or a log.
func readSigninRequest(path string) (req signinRequest, present, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return req, false, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSigninFile+1))
	if err != nil || len(b) > maxSigninFile || json.Unmarshal(b, &req) != nil || !signinIDOK(req.ID) {
		return signinRequest{}, true, false
	}
	switch req.Method {
	case "console":
		ok = req.Key == ""
	case "api_key":
		ok = ValidAPIKey(req.Key)
	}
	if !ok {
		return signinRequest{}, true, false
	}
	return req, true, true
}

// signinIDOK accepts 1 to 128 printable ASCII characters, no whitespace.
func signinIDOK(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '!' || id[i] > '~' {
			return false
		}
	}
	return true
}

// pollSignin returns a sign in request not acted on before (sandbox mode
// with a request file only). An invalid file is logged once per change of
// its content, without the content.
func (s *session) pollSignin() (signinRequest, bool) {
	path := s.d.claude.SignInFile
	if !s.d.sandbox || path == "" {
		return signinRequest{}, false
	}
	req, present, ok := readSigninRequest(path)
	if !ok {
		if present {
			if st, err := os.Stat(path); err == nil && !st.ModTime().Equal(s.signinBad) {
				s.signinBad = st.ModTime()
				s.log.Printf("sign in request file ignored: not a valid request")
			}
		}
		return signinRequest{}, false
	}
	if s.signinSeen[req.ID] {
		return signinRequest{}, false
	}
	if s.signinSeen == nil {
		s.signinSeen = map[string]bool{}
	}
	s.signinSeen[req.ID] = true
	return req, true
}

// claudeLogin is the Claude login this session runs on: always a claude.ai
// subscription outside the session-bound sandbox mode.
func (s *session) claudeLogin() ClaudeLogin {
	if !s.d.sandbox {
		return ClaudeLoginSubscription
	}
	return s.login
}

// claudeEnv adds the API key mode key to a Claude child environment (only
// in API key mode; the value is never logged).
func (s *session) claudeEnv(env []string) ([]string, error) {
	if s.claudeLogin() != ClaudeLoginAPIKey {
		return env, nil
	}
	return ClaudeAPIKeyEnv(env, s.apiKey)
}

// claudePrompt is the sign in card text for a login link.
func claudePrompt(console bool, deadline time.Time) string {
	if console {
		return "Sign in to Claude Code with your Claude Console account: open the link and sign in, " +
			"then send the code the page shows as your next message. Usage is billed to your Console organization. " +
			expiryText(deadline)
	}
	return "Sign in to Claude Code with your Claude subscription: open the link and sign in, " +
		"then send the code the page shows as your next message. " + expiryText(deadline)
}

// signInClaude runs `claude auth login --claudeai` under a PTY, sends its
// link as the prompt, writes the next user_message as the pasted code and,
// after the command exits, re-runs the claude auth status proof. The PTY
// output echoes the code, so none of it is logged. In the sandbox it also
// follows the sign in request file (Story 78.38): a Console request
// restarts the login as `claude auth login --console` with a new prompt,
// an API key request ends the login in API key mode.
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
	defer func() { pt.kill() }() // pt changes on a Console switch

	var poll <-chan time.Time
	if s.d.sandbox && s.d.claude.SignInFile != "" {
		tick := time.NewTicker(signinPollEvery)
		defer tick.Stop()
		poll = tick.C
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	limit := deadline.Add(signInTimeout) // a Console switch never runs past it
	console, consoles := false, 0
	prompted, codes, entered := false, 0, false
	var mark int // output length when the last code was written
	for {
		select {
		case <-pt.changed:
			out := pt.output()
			if !prompted {
				if link := claudeLoginURL(out); link != "" {
					prompted = true
					id := "claude-signin-" + s.outbox.RunID()
					if console {
						id = fmt.Sprintf("claude-console-%s-%d", s.outbox.RunID(), consoles)
					}
					s.emitAuthPrompt("claude", id, link, claudePrompt(console, deadline), "", deadline)
					if console {
						s.log.Printf("claude sign-in prompt sent (console)")
					} else {
						s.log.Printf("claude sign-in prompt sent")
					}
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
			login := ClaudeLoginSubscription
			if console {
				login = ClaudeLoginConsole
			}
			st, err := ClaudeAuthStatus(ctx, s.d.runner, env, s.rec.Cwd, bin)
			if err != nil {
				return signInFailed, fmt.Errorf("claude auth status after the sign-in: %w", err)
			}
			if problem := st.LoginProblem(s.d.sandbox, login); problem != "" {
				return signInFailed, errors.New("after the sign-in, " + problem)
			}
			s.login = login
			return signedIn, nil
		case <-poll:
			// Settle a login that exited or succeeded before any request.
			select {
			case <-pt.done:
				continue // the next select takes pt.done
			default:
			}
			if codes > 0 && strings.Contains(strings.ToLower(string(pt.output()[mark:])), "login successful") {
				continue
			}
			req, ok := s.pollSignin()
			if !ok {
				continue
			}
			if req.Method == "api_key" {
				s.log.Printf("sign in request %s: using the API key from the sign in request file", Scrub(req.ID))
				pt.kill()
				kenv, err := ClaudeAPIKeyEnv(env, req.Key)
				if err != nil {
					return signInFailed, err
				}
				st, err := ClaudeAuthStatus(ctx, s.d.runner, kenv, s.rec.Cwd, bin)
				if err != nil {
					return signInFailed, fmt.Errorf("claude auth status with the API key: %w", err)
				}
				if problem := st.LoginProblem(true, ClaudeLoginAPIKey); problem != "" {
					return signInFailed, errors.New("with the API key, " + problem)
				}
				s.login, s.apiKey = ClaudeLoginAPIKey, req.Key
				return signedIn, nil
			}
			s.log.Printf("sign in request %s: switching to a Console login", Scrub(req.ID))
			pt.kill()
			next, err := startPTY(bin, claudeConsoleLoginArgs, env, s.rec.Cwd)
			if err != nil {
				return signInFailed, fmt.Errorf("start claude auth login --console: %w", err)
			}
			pt = next
			s.log.Printf("claude auth login --console started under a PTY (pid %d, env keys %v)", pt.pid(), EnvKeys(env))
			console, prompted, codes, entered, mark = true, false, 0, false, 0
			consoles++
			// A fresh sign in window from the switch, capped at twice
			// signInTimeout from the start; the new prompt names it.
			deadline = time.Now().Add(signInTimeout)
			if deadline.After(limit) {
				deadline = limit
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Until(deadline))
		case <-timer.C:
			if consoles > 0 {
				return signInFailed, fmt.Errorf("no sign-in by %s UTC, the end of the Console sign-in window; the login was stopped",
					deadline.UTC().Format("15:04:05"))
			}
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
