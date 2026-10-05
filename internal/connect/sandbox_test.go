package connect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/client"
)

// Session-bound sandbox mode (Story 78.22): RunSession against the fake
// relay and chat-api, with the fake Claude Code / Codex and their fake
// sign-in flows. No test completes a real login.

const testCABundle = "/etc/cloudflare/certs/cloudflare-containers-ca.crt"

// sandboxRun is one RunSession, as `archivist connect --session` runs it.
type sandboxRun struct {
	h       *harness
	sid     string
	done    chan error
	cancel  context.CancelFunc
	detects *atomic.Int32
}

// runSession starts RunSession for sid. signIn names the harness that
// starts logged out; extraEnv joins the daemon environment.
func (h *harness) runSession(sid, agent, signIn, prompt string, extraEnv ...string) *sandboxRun {
	h.t.Helper()
	return h.runSessionWith(sid, agent, signIn, prompt, sessionOpts{}, extraEnv...)
}

// sessionOpts are the Story 78.38 Claude settings of a session-bound run:
// the sign in request file and the login the session starts on.
type sessionOpts struct {
	signinFile string
	login      ClaudeLogin
}

// runSessionWith is runSession with the Claude sign in settings.
func (h *harness) runSessionWith(sid, agent, signIn, prompt string, o sessionOpts, extraEnv ...string) *sandboxRun {
	h.t.Helper()
	claudeBin, archivistBin := testBinaries(h.t)
	api := client.New(testOwnerKey, "test")
	api.BaseURL = h.api.srv.URL
	env := append(h.daemonEnv(), extraEnv...)
	detects := &atomic.Int32{}
	cfg := Config{
		API:      api,
		RelayURL: h.relay.url(),
		StateDir: filepath.Join(h.home, ".archivist", "connect"),
		Log:      NewLogger(h.log),
		Detect: func(context.Context) Detection {
			detects.Add(1)
			return Detection{}
		},
		Environ:      func() []string { return env },
		TempDir:      h.tmp,
		SignIn:       signIn,
		MaxMode:      h.maxMode,
		WebSearch:    h.webSearch,
		ActivityFile: h.activityFile,
	}
	if agent == "claude" {
		cfg.Claude = ClaudeConfig{Bin: claudeBin, SettingSources: DefaultSettingSources, Executable: archivistBin,
			BaseURL: h.api.srv.URL, Login: o.login, SignInFile: o.signinFile}
	} else {
		cfg.Codex = CodexConfig{Bin: testCodexBinary(h.t), Version: "0.160.0", OwnerHome: h.codexOwner(),
			Executable: archivistBin, BaseURL: h.api.srv.URL}
	}
	d, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &sandboxRun{h: h, sid: sid, done: make(chan error, 1), cancel: cancel, detects: detects}
	h.d = d
	go func() {
		r.done <- d.RunSession(ctx, SessionStart{SessionID: sid, Agent: agent, Prompt: prompt, Mode: h.sessionMode,
			Model: h.sessionModel, Effort: h.sessionEffort, ResumeFrom: h.resumeFrom})
	}()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(30 * time.Second):
		}
	})
	return r
}

// wait returns RunSession's result.
func (r *sandboxRun) wait() error {
	r.h.t.Helper()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(40 * time.Second):
		r.h.t.Fatalf("RunSession did not return\n%s", r.h.log.String())
		return nil
	}
}

// noUserSocket checks the sandbox daemon never touched the user socket.
func (r *sandboxRun) noUserSocket() {
	r.h.t.Helper()
	if n := r.h.relay.dialCount(""); n != 0 {
		r.h.t.Errorf("user socket dialed %d times", n)
	}
	if n := r.h.api.ticketMints(""); n != 0 {
		r.h.t.Errorf("%d user-scope relay tickets minted", n)
	}
	if n := r.h.relay.capsCount(); n != 0 {
		r.h.t.Errorf("%d capability reports sent", n)
	}
	if n := r.detects.Load(); n != 0 {
		r.h.t.Errorf("capability detection ran %d times", n)
	}
}

func (r *sandboxRun) stop() {
	r.h.command(r.sid, "stop_session")
	if err := r.wait(); err != nil {
		r.h.t.Fatalf("RunSession after stop_session: %v\n%s", err, r.h.log.String())
	}
}

// authPrompt waits for the session's data-auth-prompt payload data.
func (h *harness) authPrompt(sid string) map[string]any {
	h.t.Helper()
	var data map[string]any
	waitFor(h.t, 20*time.Second, "data-auth-prompt", func() bool {
		for _, p := range h.relay.payloads(sid) {
			if p["type"] == "data-auth-prompt" {
				data, _ = p["data"].(map[string]any)
				return true
			}
		}
		return false
	})
	return data
}

// checkPromptContract checks the auth prompt's v2 fields and that only
// data-auth-prompt envelopes are stamped mosaic-event/2.
func (h *harness) checkPromptContract(sid string, data map[string]any, code string, sent time.Time) {
	h.t.Helper()
	if code == "" {
		if _, ok := data["code"]; ok {
			h.t.Errorf("auth prompt has a code: %v", data)
		}
	} else if data["code"] != code {
		h.t.Errorf("auth prompt code %v, want %s", data["code"], code)
	}
	// expiresAt is the 5-minute sign-in deadline, in epoch milliseconds.
	exp, _ := data["expiresAt"].(float64)
	if lo, hi := sent.Add(signInTimeout).UnixMilli(), time.Now().Add(signInTimeout).UnixMilli(); exp < float64(lo) || exp > float64(hi) {
		h.t.Errorf("auth prompt expiresAt %v, want within [%d, %d]", data["expiresAt"], lo, hi)
	}
	for _, e := range h.relay.eventsOf(sid, "") {
		want := "mosaic-event/1"
		switch e["type"] {
		case "data-auth-prompt":
			want = "mosaic-event/2"
		case "data-session-controls":
			want = "mosaic-event/3"
		case "data-usage":
			d, _ := e["payload"].(map[string]any)["data"].(map[string]any)
			_, tokens := d["contextTokens"]
			_, window := d["contextWindow"]
			if tokens || window {
				want = "mosaic-event/3"
			}
		}
		if e["schemaVersion"] != want {
			h.t.Errorf("%v event stamped %v, want %s", e["type"], e["schemaVersion"], want)
		}
	}
}

// sessionTimeline lists the session's payload types (status values inline).
func (h *harness) sessionTimeline(sid string) []string {
	var out []string
	for _, p := range h.relay.payloads(sid) {
		typ, _ := p["type"].(string)
		if typ == "data-session-status" {
			d, _ := p["data"].(map[string]any)
			typ += ":" + d["status"].(string)
		}
		out = append(out, typ)
	}
	return out
}

// wantFatal checks a *FatalError and returns its message.
func wantFatal(t *testing.T, err error, code string, auth bool) string {
	t.Helper()
	var fatal *FatalError
	if !errors.As(err, &fatal) || fatal.Code != code || fatal.Auth != auth {
		t.Fatalf("got %v (%#v), want FatalError %s auth=%v", err, fatal, code, auth)
	}
	return fatal.Message
}

// ─── session mode ───────────────────────────────────────────────────────────

func TestSessionModeRunsOnSessionSocketOnly(t *testing.T) {
	h := newHarness(t, nil)
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "", "echo hello sandbox")
	waitFor(t, 30*time.Second, "echo", func() bool { return strings.Contains(h.relay.text(s.SessionID), "hello sandbox") })
	if got := h.relay.statuses(s.SessionID); len(got) < 2 || got[0] != "starting" || got[1] != "running" {
		t.Fatalf("statuses %v", got)
	}
	for _, p := range h.relay.payloads(s.SessionID) {
		if p["type"] == "data-auth-prompt" {
			t.Fatal("a logged-in harness got a sign-in prompt")
		}
	}
	// Later messages arrive on the session socket.
	h.message(s.SessionID, "echo second turn")
	waitFor(t, 30*time.Second, "second echo", func() bool { return strings.Contains(h.relay.text(s.SessionID), "second turn") })
	r.stop()
	h.relay.waitStatus(t, s.SessionID, "completed", 1)
	r.noUserSocket()
	if live := h.api.liveTokens(); len(live) != 0 {
		t.Fatalf("live task tokens after stop: %v", live)
	}
}

func TestSessionModeExitsZeroWhenSessionEnds(t *testing.T) {
	h := newHarness(t, nil)
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "", "echo ending")
	waitFor(t, 30*time.Second, "echo", func() bool { return strings.Contains(h.relay.text(s.SessionID), "ending") })
	h.api.endSession(s.SessionID)
	h.relay.closeSession(s.SessionID, closeExpired)
	if err := r.wait(); err != nil {
		t.Fatalf("RunSession after the session ended: %v", err)
	}
	r.noUserSocket()
}

func TestSessionModeValidatesTheSession(t *testing.T) {
	setDuration(t, &sessionLookupBackoff, 10*time.Millisecond)
	cases := []struct {
		name, agent, code string
		setup             func(h *harness) string
	}{
		{"unknown", "claude", "SESSION_UNKNOWN", func(h *harness) string { return randomUUID() }},
		{"ended", "claude", "SESSION_INACTIVE", func(h *harness) string {
			s := h.api.addSession("claude")
			h.api.endSession(s.SessionID)
			return s.SessionID
		}},
		{"other agent", "claude", "SESSION_AGENT", func(h *harness) string { return h.api.addSession("codex").SessionID }},
		{"not a uuid", "claude", "BAD_SESSION", func(h *harness) string { return "not-a-uuid" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil)
			sid := c.setup(h)
			r := h.runSession(sid, c.agent, "", "echo x")
			wantFatal(t, r.wait(), c.code, false)
			if n := h.relay.dialCount(sid); n != 0 {
				t.Fatalf("session socket dialed %d times before validation passed", n)
			}
			r.noUserSocket()
		})
	}
}

func TestSessionModeLookupFailures(t *testing.T) {
	setDuration(t, &sessionLookupBackoff, 10*time.Millisecond)
	h := newHarness(t, nil)
	s := h.api.addSession("claude")
	h.api.set(func(f *fakeChatAPI) { f.off = true })
	r := h.runSession(s.SessionID, "claude", "", "echo x")
	wantFatal(t, r.wait(), "FEATURE_DISABLED", false)
	r.noUserSocket()
}

// SIGTERM (the sandbox's stop or deadline) cancels RunSession's context: it
// returns nil promptly and the relay sees disconnected "The sandbox stopped.".
func TestSessionModeContextCancel(t *testing.T) {
	for _, c := range []struct {
		name   string
		cfg    map[string]any
		signIn string
		prompt string
		ready  func(h *harness, sid string)
	}{
		{"mid-turn", nil, "", "slow", func(h *harness, sid string) {
			waitFor(h.t, 30*time.Second, "streaming turn", func() bool { return len(h.relay.eventsOf(sid, "text-delta")) > 0 })
		}},
		{"during sign-in", map[string]any{"loggedIn": false, "authMethod": "none"}, "claude", "echo never",
			func(h *harness, sid string) { h.authPrompt(sid) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.signIn == "claude" {
				skipOnWindows(t, noPTY)
			}
			h := newHarness(t, c.cfg)
			s := h.api.addSession("claude")
			r := h.runSession(s.SessionID, "claude", c.signIn, c.prompt)
			c.ready(h, s.SessionID)
			began := time.Now()
			r.cancel()
			select {
			case err := <-r.done:
				r.done <- err
				if err != nil {
					t.Fatalf("RunSession after cancel: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("RunSession did not return within 20 s of the cancel\n%s", h.log.String())
			}
			t.Logf("RunSession returned %v after the cancel", time.Since(began).Round(time.Millisecond))
			waitFor(t, 10*time.Second, "disconnected status", func() bool {
				for _, p := range h.relay.payloads(s.SessionID) {
					d, _ := p["data"].(map[string]any)
					if p["type"] == "data-session-status" && d["status"] == "disconnected" && d["message"] == "The sandbox stopped." {
						return true
					}
				}
				return false
			})
			r.noUserSocket()
		})
	}
}

// ─── Claude Code sign-in ────────────────────────────────────────────────────

func TestSessionModeClaudeSignIn(t *testing.T) {
	skipOnWindows(t, noPTY)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	s := h.api.addSession("claude")
	started := time.Now()
	r := h.runSession(s.SessionID, "claude", "claude", "echo after sign in",
		"NODE_EXTRA_CA_CERTS="+testCABundle, "SSL_CERT_FILE="+testCABundle)
	data := h.authPrompt(s.SessionID)
	if data["provider"] != "claude" || !strings.HasPrefix(data["url"].(string), "https://claude.com/cai/oauth/authorize?") ||
		!strings.Contains(data["message"].(string), "expires at") {
		t.Fatalf("auth prompt %v", data)
	}
	if types := h.sessionTimeline(s.SessionID); types[0] != "data-session-status:starting" || types[1] != "data-auth-prompt" {
		t.Fatalf("payloads %v, want starting then the auth prompt", types)
	}
	h.message(s.SessionID, "  good-code \n")
	waitFor(t, 30*time.Second, "turn after sign in", func() bool {
		return strings.Contains(h.relay.text(s.SessionID), "after sign in")
	})
	// Claude has no device code (the user pastes one); the turn stays v1.
	h.checkPromptContract(s.SessionID, data, "", started)
	if got := h.relay.statuses(s.SessionID); !slices.Contains(got, "running") || slices.Contains(got, "failed") {
		t.Fatalf("statuses %v", got)
	}
	// The login ran on a terminal with only allowlisted keys (CA keys included).
	var login struct {
		Args    []string `json:"args"`
		EnvKeys []string `json:"envKeys"`
		TTY     bool     `json:"tty"`
	}
	b, err := os.ReadFile(filepath.Join(h.home, ".fakeclaude", "login.json"))
	if err != nil || json.Unmarshal(b, &login) != nil {
		t.Fatalf("login record: %v", err)
	}
	if !login.TTY || strings.Join(login.Args, " ") != "--claudeai" {
		t.Fatalf("login %+v", login)
	}
	for _, k := range login.EnvKeys {
		if !EnvAllowed(k) {
			t.Errorf("login child got %s", k)
		}
	}
	for _, k := range []string{"HOME", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE"} {
		if !slices.Contains(login.EnvKeys, k) {
			t.Errorf("login child lacks %s: %v", k, login.EnvKeys)
		}
	}
	// The pasted code never reaches the log.
	if strings.Contains(h.log.String(), "good-code") {
		t.Fatal("the sign-in code was logged")
	}
	r.stop()
	r.noUserSocket()
}

func TestSessionModeClaudeSignInRefusesChatMessage(t *testing.T) {
	skipOnWindows(t, noPTY)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "claude", "echo signed in later")
	h.authPrompt(s.SessionID)
	h.message(s.SessionID, "what is the code supposed to be?")
	waitFor(t, 20*time.Second, "refusal", func() bool {
		for _, p := range h.relay.payloads(s.SessionID) {
			if p["type"] == "error" && strings.Contains(p["errorText"].(string), "not a sign-in code") {
				return true
			}
		}
		return false
	})
	if slices.Contains(h.relay.statuses(s.SessionID), "failed") {
		t.Fatal("a chat message failed the sign-in")
	}
	h.message(s.SessionID, "good-code")
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "signed in later") })
	r.stop()
}

func TestSessionModeClaudeSignInWaitsForEnter(t *testing.T) {
	skipOnWindows(t, noPTY)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none", "loginEnter": true})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "claude", "echo entered")
	h.authPrompt(s.SessionID)
	h.message(s.SessionID, "good-code")
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "entered") })
	r.stop()
}

func TestSessionModeClaudeSignInFailures(t *testing.T) {
	skipOnWindows(t, noPTY)
	cases := []struct {
		name string
		cfg  map[string]any
		code string // pasted code ("" = none)
		want string
	}{
		{"wrong code", map[string]any{"loggedIn": false}, "bad-code", "did not accept the code"},
		{"proof fails", map[string]any{"loggedIn": false, "loginLoggedOut": true}, "good-code",
			"after the sign-in, Claude Code is not logged in"},
		{"no link", map[string]any{"loggedIn": false, "loginNoURL": true}, "", "before printing a sign-in link"},
		{"timeout", map[string]any{"loggedIn": false}, "", "no sign-in within"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "timeout" {
				setDuration(t, &signInTimeout, time.Second)
			}
			h := newHarness(t, c.cfg)
			s := h.api.addSession("claude")
			r := h.runSession(s.SessionID, "claude", "claude", "echo never")
			if c.code != "" {
				h.authPrompt(s.SessionID)
				h.message(s.SessionID, c.code)
			}
			if msg := wantFatal(t, r.wait(), "SESSION_FAILED", true); !strings.Contains(msg, c.want) {
				t.Fatalf("message %q, want %q", msg, c.want)
			}
			h.relay.waitStatus(t, s.SessionID, "failed", 1)
			if strings.Contains(h.relay.text(s.SessionID), "never") || len(fakeRuns(t, h.home)) != 0 {
				t.Fatal("a turn ran after a failed sign-in")
			}
			r.noUserSocket()
		})
	}
}

func TestSessionModeClaudeProofFailureExitsNonZero(t *testing.T) {
	// Logged in with a key while the session runs on the claude.ai login
	// (Story 78.38: no Console login known): the init proof fails.
	h := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "api_key", "apiKeySource": "/login managed key"})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "", "echo never")
	wantFatal(t, r.wait(), "SESSION_FAILED", true)
	h.relay.waitStatus(t, s.SessionID, "failed", 1)
}

func TestSessionModeStopDuringSignIn(t *testing.T) {
	skipOnWindows(t, noPTY)
	h := newHarness(t, map[string]any{"loggedIn": false})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "claude", "echo never")
	h.authPrompt(s.SessionID)
	r.stop()
	h.relay.waitStatus(t, s.SessionID, "completed", 1)
}

// ─── Codex sign-in ──────────────────────────────────────────────────────────

// newSignInCodexHarness is a Codex harness whose owner home has no login.
func newSignInCodexHarness(t *testing.T, fakeCfg map[string]any) *harness {
	h := newCodexHarness(t, fakeCfg)
	if err := os.Remove(filepath.Join(h.codexOwner(), "auth.json")); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSessionModeCodexDeviceCodeSignIn(t *testing.T) {
	h := newSignInCodexHarness(t, nil)
	s := h.api.addSession("codex")
	started := time.Now()
	r := h.runSession(s.SessionID, "codex", "codex", "echo codex after sign in", "SSL_CERT_FILE="+testCABundle)
	data := h.authPrompt(s.SessionID)
	msg, _ := data["message"].(string)
	if data["provider"] != "codex" || data["url"] != "https://auth.openai.com/codex/device" || data["promptId"] == "" ||
		!strings.Contains(msg, "FAKE-1234") || !strings.Contains(msg, "expires at") ||
		!strings.Contains(msg, "Device code login must be enabled in your ChatGPT security settings") {
		t.Fatalf("auth prompt %v", data)
	}
	if types := h.sessionTimeline(s.SessionID); types[0] != "data-session-status:starting" || types[1] != "data-auth-prompt" {
		t.Fatalf("payloads %v, want starting then the auth prompt", types)
	}
	// A message during the sign-in follows the first prompt.
	h.message(s.SessionID, "echo early message")
	waitFor(t, 30*time.Second, "turns after sign in", func() bool {
		txt := h.relay.text(s.SessionID)
		return strings.Contains(txt, "codex after sign in") && strings.Contains(txt, "early message")
	})
	// The device code travels as the v2 code too; the turns stay v1.
	h.checkPromptContract(s.SessionID, data, "FAKE-1234", started)
	b, err := os.ReadFile(filepath.Join(h.home, ".fakecodex", "login-start.json"))
	if err != nil || string(b) != `{"type":"chatgptDeviceCode"}` {
		t.Fatalf("login start params %s (%v)", b, err)
	}
	// The login landed in the owner's home, which the session home links.
	if st, err := os.Lstat(filepath.Join(h.codexOwner(), "auth.json")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("owner auth.json: %v", err)
	}
	login, sessions := loginRuns(t, h)
	if len(login) != 1 || len(sessions) != 1 || login[0].CodexHome != h.codexOwner() || sessions[0].CodexHome == h.codexOwner() {
		t.Fatalf("login runs %+v, session runs %+v: want one login app-server in the owner's home", login, sessions)
	}
	for _, r := range append(login, sessions...) {
		if !slices.Contains(r.EnvKeys, "SSL_CERT_FILE") {
			t.Fatalf("app-server env %v lacks SSL_CERT_FILE", r.EnvKeys)
		}
	}
	if _, err := os.Stat(filepath.Join(h.home, ".fakecodex", "login-cancel.json")); err == nil {
		t.Fatal("a completed login was cancelled")
	}
	r.stop()
	r.noUserSocket()
}

func TestSessionModeCodexSignInFailures(t *testing.T) {
	cases := []struct {
		name, login, want string
		cancel            bool
	}{
		{"failure", "failure", "device code login is disabled", false},
		{"timeout", "hang", "no sign-in within", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setDuration(t, &signInTimeout, 1500*time.Millisecond)
			h := newSignInCodexHarness(t, map[string]any{"login": c.login})
			s := h.api.addSession("codex")
			r := h.runSession(s.SessionID, "codex", "codex", "echo never")
			h.authPrompt(s.SessionID)
			if msg := wantFatal(t, r.wait(), "SESSION_FAILED", true); !strings.Contains(msg, c.want) {
				t.Fatalf("message %q, want %q", msg, c.want)
			}
			h.relay.waitStatus(t, s.SessionID, "failed", 1)
			_, err := os.Stat(filepath.Join(h.home, ".fakecodex", "login-cancel.json"))
			if (err == nil) != c.cancel {
				t.Fatalf("login cancelled %v, want %v", err == nil, c.cancel)
			}
			if c.cancel {
				var p map[string]any
				b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "login-cancel.json"))
				if json.Unmarshal(b, &p) != nil || p["loginId"] == "" || p["loginId"] == nil {
					t.Fatalf("cancel params %s", b)
				}
			}
			if _, sessions := loginRuns(t, h); strings.Contains(h.relay.text(s.SessionID), "never") || len(sessions) != 0 {
				t.Fatal("a session app-server ran after a failed sign-in")
			}
		})
	}
}

func TestSessionModeCodexSignInRefusesForeignLink(t *testing.T) {
	h := newSignInCodexHarness(t, map[string]any{"verificationUrl": "https://auth.openai.com.evil.example/codex/device"})
	s := h.api.addSession("codex")
	r := h.runSession(s.SessionID, "codex", "codex", "echo never")
	if msg := wantFatal(t, r.wait(), "SESSION_FAILED", true); !strings.Contains(msg, "device code login Codex returned is unusable") {
		t.Fatalf("message %q", msg)
	}
	h.relay.waitStatus(t, s.SessionID, "failed", 1)
	for _, p := range h.relay.payloads(s.SessionID) {
		if p["type"] == "data-auth-prompt" {
			t.Fatal("a foreign device code link reached the relay")
		}
	}
}

func TestSessionModeCodexStopDuringSignIn(t *testing.T) {
	h := newSignInCodexHarness(t, map[string]any{"login": "hang"})
	s := h.api.addSession("codex")
	r := h.runSession(s.SessionID, "codex", "codex", "echo never")
	h.authPrompt(s.SessionID)
	r.stop()
	h.relay.waitStatus(t, s.SessionID, "completed", 1)
	if _, err := os.Stat(filepath.Join(h.home, ".fakecodex", "login-cancel.json")); err != nil {
		t.Fatal("the login was not cancelled on stop_session")
	}
}

// loginRuns splits the fake Codex app-server runs into login app-servers
// (no archivist MCP server) and session app-servers.
func loginRuns(t *testing.T, h *harness) (login, sessions []codexRunRecord) {
	t.Helper()
	for _, r := range codexRuns(t, h.home) {
		if strings.Contains(strings.Join(r.Args, " "), "mcp_servers.archivist.command") {
			sessions = append(sessions, r)
		} else {
			login = append(login, r)
		}
	}
	return login, sessions
}

// ─── units ──────────────────────────────────────────────────────────────────

func TestCACertEnvAllowlist(t *testing.T) {
	parent := []string{"HOME=/h", "PATH=/bin",
		"NODE_EXTRA_CA_CERTS=" + testCABundle, "SSL_CERT_FILE=" + testCABundle, "SSL_CERT_DIR=/etc/ssl/certs",
		"CURL_CA_BUNDLE=" + testCABundle, "GIT_SSL_CAINFO=" + testCABundle, "REQUESTS_CA_BUNDLE=" + testCABundle,
		"ANTHROPIC_API_KEY=k", "CLAUDE_CODE_OAUTH_TOKEN=o", "OPENAI_API_KEY=k", "CODEX_CA_CERTIFICATE=/x",
		"ARCHIVIST_TOKEN=ak_x", "ARCHIVIST_ORIGIN=sandbox", "HERDR_ENV=1", "NODE_OPTIONS=--require x",
	}
	env, err := BuildChildEnv(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "CURL_CA_BUNDLE,GIT_SSL_CAINFO,HOME,NODE_EXTRA_CA_CERTS,PATH,REQUESTS_CA_BUNDLE,SSL_CERT_DIR,SSL_CERT_FILE"
	if got := strings.Join(EnvKeys(env), ","); got != want {
		t.Fatalf("child keys %s, want %s", got, want)
	}
	if !contains(env, "NODE_EXTRA_CA_CERTS="+testCABundle) {
		t.Fatal("CA value changed")
	}
	for _, bad := range []string{"ANTHROPIC_API_KEY", "CODEX_CA_CERTIFICATE", "ARCHIVIST_ORIGIN", "CLAUDE_CODE_X"} {
		if EnvAllowed(bad) {
			t.Errorf("%s allowed", bad)
		}
	}
	args := strings.Join(codexArgs(CodexConfig{Executable: "/a"}, "/t", nil), " ")
	for _, k := range CACertKeys {
		if !strings.Contains(args, `shell_environment_policy.include_only=`) || !strings.Contains(args, `"`+k+`"`) {
			t.Errorf("codex include_only lacks %s", k)
		}
	}
}

func TestCodexLoginArgsAreFixed(t *testing.T) {
	got := strings.Join(codexLoginArgs(), " ")
	want := `app-server --stdio --strict-config -c forced_login_method="chatgpt" -c cli_auth_credentials_store="file" ` +
		`-c check_for_update_on_startup=false -c analytics.enabled=false`
	if got != want {
		t.Fatalf("login argv\n%s\nwant\n%s", got, want)
	}
}

func TestClaudeLoginURL(t *testing.T) {
	// The shape Claude Code 2.1.285 printed in the 2026-10-03 probe (an
	// OSC 8 hyperlink, then the plain link, then the paste prompt).
	link := "https://claude.com/cai/oauth/authorize?code=true&client_id=abc&response_type=code" +
		"&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&state=s-1_x"
	probe := "Opening browser to sign in…\r\nIf the browser didn't open, visit: \x1b]8;;" + link + "\x07\x1b[94m" + link +
		"\x1b[39m\x1b]8;;\x07\r\nPaste code here if prompted > "
	for name, out := range map[string]string{
		"probe": probe,
		"plain": "If the browser didn't open, visit: " + link + "\r\nPaste code here if prompted > ",
	} {
		if got := claudeLoginURL([]byte(out)); got != link {
			t.Errorf("%s: %q", name, got)
		}
	}
	for _, out := range []string{"visit: http://claude.com/x", "visit: https://evil.example/claude.com", "nothing yet"} {
		if got := claudeLoginURL([]byte(out)); got != "" {
			t.Errorf("%q accepted as %q", out, got)
		}
	}
	for in, ok := range map[string]bool{" abc#def \n": true, "": false, "a\nb": false, "a\x1bb": false,
		"what is the code": false, "a\tb": false, "a\u00a0b": false} {
		if _, got := signInCode(in); got != ok {
			t.Errorf("signInCode(%q) = %v", in, got)
		}
	}
}

// ─── sandbox sign in methods (Story 78.38) ─────────────────────────────────

// testSigninKey is the sign in request file's key: the shape of the sandbox
// Worker's placeholder (never a real key).
const testSigninKey = "sk-ant-mosaic-sandbox-placeholder-0000000000000000"

// writeSignin writes the sign in request file as the Worker does: a
// temporary file, then a rename.
func writeSignin(t *testing.T, path string, req map[string]any) {
	t.Helper()
	b, _ := json.Marshal(req)
	writeSigninRaw(t, path, b)
}

func writeSigninRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// authPrompts lists the session's data-auth-prompt payload data in order.
func (h *harness) authPrompts(sid string) []map[string]any {
	var out []map[string]any
	for _, p := range h.relay.payloads(sid) {
		if p["type"] == "data-auth-prompt" {
			d, _ := p["data"].(map[string]any)
			out = append(out, d)
		}
	}
	return out
}

// waitPrompts waits for n auth prompts and returns them.
func (h *harness) waitPrompts(sid string, n int) []map[string]any {
	h.t.Helper()
	waitFor(h.t, 20*time.Second, fmt.Sprintf("%d auth prompts", n), func() bool { return len(h.authPrompts(sid)) >= n })
	return h.authPrompts(sid)
}

// fakeLogins lists the argv of every fake `claude auth login`.
func fakeLogins(t *testing.T, home string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, ".fakeclaude", "logins.jsonl"))
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec struct {
			Args []string `json:"args"`
		}
		if line != "" && json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, strings.Join(rec.Args, " "))
		}
	}
	return out
}

// fakeKeyRuns lists each fake -p run's ANTHROPIC_API_KEY fingerprint ("" =
// unset) and whether the key name was in its environment.
func fakeKeyRuns(t *testing.T, home string) (fps []string, keyed []bool) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(home, ".fakeclaude", "runs"))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(home, ".fakeclaude", "runs", e.Name()))
		if err != nil {
			continue
		}
		var r struct {
			EnvKeys []string `json:"envKeys"`
			FP      string   `json:"apiKeyFingerprint"`
		}
		if json.Unmarshal(b, &r) == nil {
			fps = append(fps, r.FP)
			keyed = append(keyed, slices.Contains(r.EnvKeys, ClaudeAPIKeyEnvKey))
		}
	}
	return fps, keyed
}

func keyFingerprint(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

// A Console request during the claude.ai sign in restarts the login as
// `claude auth login --console`: a second auth prompt (new promptId, the
// Console link and wording, a fresh window) and the pasted code signs in;
// the session runs on the Console login.
func TestSessionModeClaudeConsoleSwitch(t *testing.T) {
	skipOnWindows(t, noPTY)
	setDuration(t, &signinPollEvery, 50*time.Millisecond)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	file := filepath.Join(h.tmp, "signin.json")
	s := h.api.addSession("claude")
	r := h.runSessionWith(s.SessionID, "claude", "claude", "echo after console", sessionOpts{signinFile: file})
	first := h.waitPrompts(s.SessionID, 1)[0]
	if !strings.HasPrefix(first["url"].(string), "https://claude.com/cai/oauth/authorize?") {
		t.Fatalf("first prompt %v", first)
	}
	switched := time.Now()
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "console"})
	prompts := h.waitPrompts(s.SessionID, 2)
	second := prompts[1]
	msg, _ := second["message"].(string)
	if second["provider"] != "claude" || second["promptId"] == first["promptId"] || second["promptId"] == "" ||
		!strings.HasPrefix(second["url"].(string), "https://platform.claude.com/oauth/authorize?") ||
		!strings.Contains(msg, "Claude Console account") || !strings.Contains(msg, "expires at") ||
		strings.ContainsAny(msg, "-—") {
		t.Fatalf("console prompt %v", second)
	}
	// The Console prompt carries a fresh window from the switch.
	h.checkPromptContract(s.SessionID, second, "", switched)
	h.message(s.SessionID, "good-code")
	waitFor(t, 30*time.Second, "turn after console", func() bool {
		return strings.Contains(h.relay.text(s.SessionID), "after console")
	})
	if got := fakeLogins(t, h.home); len(got) != 2 || got[0] != "--claudeai" || got[1] != "--console" {
		t.Fatalf("logins %v, want --claudeai then --console", got)
	}
	if log := h.log.String(); !strings.Contains(log, "switching to a Console login") ||
		!strings.Contains(log, "console login, apiKeySource /login managed key") || strings.Contains(log, "good-code") {
		t.Fatalf("log:\n%s", log)
	}
	if fps, keyed := fakeKeyRuns(t, h.home); len(fps) != 1 || fps[0] != "" || keyed[0] {
		t.Fatalf("a Console session got ANTHROPIC_API_KEY: %v %v", fps, keyed)
	}
	r.stop()
	r.noUserSocket()
}

// An API key request ends the login: Claude Code runs with
// ANTHROPIC_API_KEY set to the file's key (and the proof needs that key in
// use); the key never reaches the log or the relay.
func TestSessionModeClaudeAPIKeySwitch(t *testing.T) {
	skipOnWindows(t, noPTY)
	setDuration(t, &signinPollEvery, 50*time.Millisecond)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	file := filepath.Join(h.tmp, "signin.json")
	s := h.api.addSession("claude")
	r := h.runSessionWith(s.SessionID, "claude", "claude", "echo keyed turn", sessionOpts{signinFile: file})
	h.waitPrompts(s.SessionID, 1)
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "api_key", "key": testSigninKey})
	waitFor(t, 30*time.Second, "keyed turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "keyed turn") })
	// A respawn keeps the key: the next turn runs in the same process here,
	// so check the spawn record and the proof calls.
	fps, keyed := fakeKeyRuns(t, h.home)
	if len(fps) != 1 || fps[0] != keyFingerprint(testSigninKey) || !keyed[0] {
		t.Fatalf("claude runs %v %v, want one with the sign in key", fps, keyed)
	}
	b, _ := os.ReadFile(filepath.Join(h.home, ".fakeclaude", "statuses.jsonl"))
	if !strings.Contains(string(b), keyFingerprint(testSigninKey)) {
		t.Fatalf("no claude auth status ran with the key: %s", b)
	}
	if len(h.authPrompts(s.SessionID)) != 1 {
		t.Fatal("an API key switch sent another auth prompt")
	}
	if got := h.relay.statuses(s.SessionID); !slices.Contains(got, "running") || slices.Contains(got, "failed") {
		t.Fatalf("statuses %v", got)
	}
	r.stop()
	// The logger scrubs keys, so "[redacted]" would mean a line tried to
	// print one: neither may appear.
	if log := h.log.String(); strings.Contains(log, testSigninKey) || strings.Contains(log, "placeholder") ||
		redactedLines(log) != 0 ||
		!strings.Contains(log, "api key login, apiKeySource ANTHROPIC_API_KEY") ||
		!strings.Contains(log, ClaudeAPIKeyEnvKey) {
		t.Fatalf("log (the key must never appear, its name must):\n%s", log)
	}
	for _, p := range h.relay.payloads(s.SessionID) {
		if b, _ := json.Marshal(p); strings.Contains(string(b), testSigninKey) || strings.Contains(string(b), "[redacted]") ||
			strings.Contains(string(b), "placeholder") {
			t.Fatalf("the key reached the relay: %s", b)
		}
	}
	r.noUserSocket()
}

// Each request id acts once: a rewritten seen id, an unparseable file, an
// unknown method or a malformed key are ignored. A later new id (Console,
// then API key) still acts.
func TestSessionModeClaudeSigninRequestsIgnored(t *testing.T) {
	skipOnWindows(t, noPTY)
	setDuration(t, &signinPollEvery, 50*time.Millisecond)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	file := filepath.Join(h.tmp, "signin.json")
	s := h.api.addSession("claude")
	r := h.runSessionWith(s.SessionID, "claude", "claude", "echo finally", sessionOpts{signinFile: file})
	h.waitPrompts(s.SessionID, 1)
	seen := randomUUID()
	writeSignin(t, file, map[string]any{"id": seen, "method": "console"})
	h.waitPrompts(s.SessionID, 2)
	for _, raw := range []string{
		`{"id":"` + seen + `","method":"console"}`,
		`{"id":"` + seen + `","method":"api_key","key":"` + testSigninKey + `"}`,
		`not json`,
		`{"id":"` + randomUUID() + `","method":"bedrock"}`,
		`{"id":"` + randomUUID() + `","method":"api_key","key":"sk-ant-short"}`,
		`{"id":"` + randomUUID() + `","method":"api_key"}`,
		`{"id":"` + randomUUID() + `","method":"console","key":"` + testSigninKey + `"}`,
		`{"method":"console"}`,
	} {
		writeSigninRaw(t, file, []byte(raw))
		time.Sleep(200 * time.Millisecond) // several polls
	}
	if n := len(h.authPrompts(s.SessionID)); n != 2 {
		t.Fatalf("%d auth prompts after ignored requests, want 2", n)
	}
	if got := fakeLogins(t, h.home); len(got) != 2 {
		t.Fatalf("logins %v after ignored requests", got)
	}
	if !strings.Contains(h.log.String(), "sign in request file ignored: not a valid request") {
		t.Fatalf("no ignored request logged:\n%s", h.log.String())
	}
	// The user changes their mind again: an API key with a new id.
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "api_key", "key": testSigninKey})
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "finally") })
	if fps, _ := fakeKeyRuns(t, h.home); len(fps) != 1 || fps[0] != keyFingerprint(testSigninKey) {
		t.Fatalf("claude runs %v", fps)
	}
	r.stop()
	if log := h.log.String(); strings.Contains(log, testSigninKey) || redactedLines(log) != 0 {
		t.Fatalf("the key was logged:\n%s", log)
	}
}

// Without a sign in request file (an older Worker) a request never acts.
func TestSessionModeClaudeNoSigninFile(t *testing.T) {
	skipOnWindows(t, noPTY)
	setDuration(t, &signinPollEvery, 50*time.Millisecond)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	file := filepath.Join(h.tmp, "signin.json")
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "console"})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "claude", "echo plain")
	h.waitPrompts(s.SessionID, 1)
	time.Sleep(300 * time.Millisecond)
	if n := len(h.authPrompts(s.SessionID)); n != 1 {
		t.Fatalf("%d prompts without a configured file", n)
	}
	h.message(s.SessionID, "good-code")
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "plain") })
	r.stop()
}

// A sandbox home holding a Console login (a remembered sign in) starts in
// Console login mode without a sign in; the claude.ai mode refuses the same
// home at the init proof (TestSessionModeClaudeProofFailureExitsNonZero).
func TestSessionModeClaudeRememberedConsoleLogin(t *testing.T) {
	h := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "api_key", "apiKeySource": "/login managed key"})
	s := h.api.addSession("claude")
	r := h.runSessionWith(s.SessionID, "claude", "", "echo remembered", sessionOpts{login: ClaudeLoginConsole})
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "remembered") })
	r.stop()
}

// The local daemon keeps its claude.ai only gate whatever the session's
// login field says: no Console or API key login passes, and no key is ever
// added to a child (the sandbox rules apply only in the session-bound mode).
func TestLocalModeKeepsClaudeAIGate(t *testing.T) {
	s := &session{d: &Daemon{}, login: ClaudeLoginAPIKey, apiKey: testSigninKey}
	if s.claudeLogin() != ClaudeLoginSubscription {
		t.Fatalf("local login %v", s.claudeLogin())
	}
	if env, err := s.claudeEnv([]string{"HOME=/h"}); err != nil || strings.Join(env, ",") != "HOME=/h" {
		t.Fatalf("local env %v %v", env, err)
	}
	if req, ok := s.pollSignin(); ok {
		t.Fatalf("local mode read a sign in request %v", req.Method)
	}
	console := &AuthStatus{LoggedIn: true, AuthMethod: "api_key", APIKeySource: "/login managed key"}
	keyed := &AuthStatus{LoggedIn: true, AuthMethod: "api_key", APIKeySource: ClaudeAPIKeySource}
	sub := &AuthStatus{LoggedIn: true, AuthMethod: "claude.ai"}
	for _, login := range []ClaudeLogin{ClaudeLoginSubscription, ClaudeLoginConsole, ClaudeLoginAPIKey} {
		for _, st := range []*AuthStatus{console, keyed, {LoggedIn: true, AuthMethod: "oauth_token"}, nil} {
			if p := st.LoginProblem(false, login); !strings.Contains(p, "not logged in with a claude.ai subscription") {
				t.Errorf("local %v accepted %+v (%q)", login, st, p)
			}
		}
		if p := sub.LoginProblem(false, login); p != "" {
			t.Errorf("local refused a subscription: %s", p)
		}
	}
	init := claudeFrame{APIKeySource: "/login managed key", PermissionMode: "default"}
	if initLoginProblem(init, []string{"default"}, s.claudeLogin()) == "" {
		t.Fatal("local init proof accepted a Console key")
	}
	// Sandbox rules, for contrast.
	cases := []struct {
		st    *AuthStatus
		login ClaudeLogin
		ok    bool
	}{
		{console, ClaudeLoginSubscription, false}, {console, ClaudeLoginConsole, true}, {console, ClaudeLoginAPIKey, false},
		{&AuthStatus{LoggedIn: true, AuthMethod: "oauth_token"}, ClaudeLoginSubscription, false},
		{keyed, ClaudeLoginAPIKey, true}, {sub, ClaudeLoginSubscription, true}, {sub, ClaudeLoginAPIKey, false},
		{&AuthStatus{AuthMethod: "none"}, ClaudeLoginConsole, false}, {nil, ClaudeLoginSubscription, false},
	}
	for _, c := range cases {
		if got := c.st.LoginProblem(true, c.login) == ""; got != c.ok {
			t.Errorf("sandbox %v %+v: ok %v, want %v", c.login, c.st, got, c.ok)
		}
	}
	inits := []struct {
		source string
		login  ClaudeLogin
		ok     bool
	}{
		{"none", ClaudeLoginSubscription, true}, {"/login managed key", ClaudeLoginSubscription, false},
		{ClaudeAPIKeySource, ClaudeLoginSubscription, false}, {"/login managed key", ClaudeLoginConsole, true},
		{"none", ClaudeLoginConsole, false}, {"", ClaudeLoginConsole, false}, {ClaudeAPIKeySource, ClaudeLoginAPIKey, true},
		{"/login managed key", ClaudeLoginAPIKey, false}, {"none", ClaudeLoginAPIKey, false},
	}
	for _, c := range inits {
		f := claudeFrame{APIKeySource: c.source, PermissionMode: "default"}
		if got := initLoginProblem(f, []string{"default"}, c.login) == ""; got != c.ok {
			t.Errorf("init %q %v: ok %v, want %v", c.source, c.login, got, c.ok)
		}
	}
	// A Console init must name the source the spawn's auth status reported.
	f := claudeFrame{APIKeySource: "/login managed key", PermissionMode: "default"}
	if initLoginProblem(f, []string{"default"}, ClaudeLoginConsole, "/login managed key") != "" ||
		initLoginProblem(f, []string{"default"}, ClaudeLoginConsole, "") != "" ||
		initLoginProblem(f, []string{"default"}, ClaudeLoginConsole, "apiKeyHelper") == "" {
		t.Fatal("Console init source check")
	}
}

func TestReadSigninRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signin.json")
	if _, present, ok := readSigninRequest(path); present || ok {
		t.Fatal("a missing file read as a request")
	}
	for raw, want := range map[string]string{
		`{"id":"a1","method":"console"}`:                                                    "console",
		`{"id":"a1","method":"api_key","key":"` + testSigninKey + `"}`:                      "api_key",
		`{"id":"a1","method":"console","extra":true}`:                                       "console",
		`{"id":"a 1","method":"console"}`:                                                   "",
		`{"id":"","method":"console"}`:                                                      "",
		`{"id":"` + strings.Repeat("a", 129) + `","method":"console"}`:                      "",
		`{"id":"a1","method":"api_key","key":"sk-ant-has space 0123456789"}`:                "",
		`{"id":"a1","method":"api_key","key":"sk-other-0123456789abcdef"}`:                  "",
		`{"id":"a1","method":"api_key","key":"sk-ant-0123456789.abcdef"}`:                   "",
		`{"id":"a1","method":"api_key","key":"sk-ant-0123456789abcde"}`:                     "",
		`{"id":"a1","method":"Console"}`:                                                    "",
		`[1]`:                                                                               "",
		`{"id":"a1","method":"console","pad":"` + strings.Repeat("x", maxSigninFile) + `"}`: "",
	} {
		writeSigninRaw(t, path, []byte(raw))
		req, present, ok := readSigninRequest(path)
		if !present || ok != (want != "") || (ok && req.Method != want) {
			t.Errorf("%.60s: present %v ok %v method %q, want %q", raw, present, ok, req.Method, want)
		}
	}
}

func TestClaudeAPIKeyEnv(t *testing.T) {
	env := ClaudeSessionEnv([]string{"PATH=/bin", "HOME=/h", ClaudeAPIKeyEnvKey + "=old"})
	got, err := ClaudeAPIKeyEnv(env, testSigninKey)
	if err != nil {
		t.Fatal(err)
	}
	want := "ANTHROPIC_API_KEY,CLAUDE_CODE_DISABLE_WEB_FETCH,HOME,PATH"
	if strings.Join(EnvKeys(got), ",") != want || !slices.Contains(got, ClaudeAPIKeyEnvKey+"="+testSigninKey) {
		t.Fatalf("env keys %v", EnvKeys(got))
	}
	bad := "sk-ant-x\nINJECTED=1 0123456789"
	if _, err := ClaudeAPIKeyEnv(env, bad); err == nil || strings.Contains(err.Error(), "INJECTED") {
		t.Fatalf("bad key: %v", err)
	}
	// The deny prefix still keeps the daemon's own key out.
	if EnvAllowed(ClaudeAPIKeyEnvKey) {
		t.Fatal("ANTHROPIC_API_KEY allowed from the daemon environment")
	}
	if _, err := BuildChildEnv(nil, map[string]string{ClaudeAPIKeyEnvKey: testSigninKey}); err == nil {
		t.Fatal("ANTHROPIC_API_KEY accepted as an override")
	}
}

// redactedLines counts log lines where Scrub removed something, except the
// fake Claude's deliberate secret stderr line: the logger scrubs keys, so
// such a line would mean the daemon tried to print one.
func redactedLines(log string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "[redacted]") && !strings.Contains(line, "fakeclaude: stderr line with a secret") {
			n++
		}
	}
	return n
}

// lastLoginPID is the pid of the newest fake `claude auth login`.
func lastLoginPID(t *testing.T, home string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, ".fakeclaude", "logins.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var rec struct {
		PID int `json:"pid"`
	}
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &rec) != nil || rec.PID == 0 {
		t.Fatalf("no login pid in %s", b)
	}
	return rec.PID
}

// noLiveLogin checks the login process (and its group) is gone.
func noLiveLogin(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, 10*time.Second, "login process gone", func() bool { return !processAlive(pid) && !groupAlive(pid) })
}

// Proof failures after a switch fail the sign in closed: an API key that
// claude auth status does not report in use, and a Console login that
// leaves Claude Code logged out. No turn runs and the key is never logged.
func TestSessionModeClaudeSwitchProofFailures(t *testing.T) {
	skipOnWindows(t, noPTY)
	cases := []struct {
		name string
		cfg  map[string]any
		req  map[string]any
		code bool
		want string
	}{
		{"api key not in use", map[string]any{"loggedIn": false, "authMethod": "none", "statusIgnoresKey": true},
			map[string]any{"method": "api_key", "key": testSigninKey}, false,
			"with the API key, Claude Code is not logged in"},
		{"console still logged out", map[string]any{"loggedIn": false, "authMethod": "none", "loginLoggedOut": true},
			map[string]any{"method": "console"}, true, "after the sign-in, Claude Code is not logged in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setDuration(t, &signinPollEvery, 50*time.Millisecond)
			h := newHarness(t, c.cfg)
			file := filepath.Join(h.tmp, "signin.json")
			s := h.api.addSession("claude")
			r := h.runSessionWith(s.SessionID, "claude", "claude", "echo never", sessionOpts{signinFile: file})
			h.waitPrompts(s.SessionID, 1)
			c.req["id"] = randomUUID()
			writeSignin(t, file, c.req)
			if c.code {
				h.waitPrompts(s.SessionID, 2)
				h.message(s.SessionID, "good-code")
			}
			if msg := wantFatal(t, r.wait(), "SESSION_FAILED", true); !strings.Contains(msg, c.want) {
				t.Fatalf("message %q, want %q", msg, c.want)
			}
			h.relay.waitStatus(t, s.SessionID, "failed", 1)
			if len(fakeRuns(t, h.home)) != 0 {
				t.Fatal("a turn ran after a failed proof")
			}
			if log := h.log.String(); strings.Contains(log, testSigninKey) || redactedLines(log) != 0 {
				t.Fatalf("the key was logged:\n%s", log)
			}
			noLiveLogin(t, lastLoginPID(t, h.home))
		})
	}
}

// stop_session after a Console switch ends the second login's PTY too.
func TestSessionModeClaudeStopAfterConsoleSwitch(t *testing.T) {
	skipOnWindows(t, noPTY)
	setDuration(t, &signinPollEvery, 50*time.Millisecond)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	file := filepath.Join(h.tmp, "signin.json")
	s := h.api.addSession("claude")
	r := h.runSessionWith(s.SessionID, "claude", "claude", "echo never", sessionOpts{signinFile: file})
	h.waitPrompts(s.SessionID, 1)
	first := lastLoginPID(t, h.home)
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "console"})
	h.waitPrompts(s.SessionID, 2)
	second := lastLoginPID(t, h.home)
	if second == first {
		t.Fatal("no second login")
	}
	noLiveLogin(t, first)
	r.stop()
	h.relay.waitStatus(t, s.SessionID, "completed", 1)
	noLiveLogin(t, second)
}

// Console switches get a fresh window, capped at twice signInTimeout from
// the sign in's start; the capped prompt names the capped expiry and the
// timeout then fails the sign in, leaving no live login.
func TestSessionModeClaudeConsoleWindowCap(t *testing.T) {
	skipOnWindows(t, noPTY)
	setDuration(t, &signinPollEvery, 50*time.Millisecond)
	setDuration(t, &signInTimeout, 2*time.Second)
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none"})
	file := filepath.Join(h.tmp, "signin.json")
	s := h.api.addSession("claude")
	r := h.runSessionWith(s.SessionID, "claude", "claude", "echo never", sessionOpts{signinFile: file})
	p1 := h.waitPrompts(s.SessionID, 1)[0]
	exp1, _ := p1["expiresAt"].(float64)
	limit := exp1 + float64(signInTimeout.Milliseconds())
	time.Sleep(1200 * time.Millisecond)
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "console"})
	p2 := h.waitPrompts(s.SessionID, 2)[1]
	if exp2, _ := p2["expiresAt"].(float64); exp2 <= exp1 || exp2 > limit {
		t.Fatalf("second expiry %v, want in (%v, %v]", exp2, exp1, limit)
	}
	time.Sleep(1200 * time.Millisecond)
	writeSignin(t, file, map[string]any{"id": randomUUID(), "method": "console"})
	p3 := h.waitPrompts(s.SessionID, 3)[2]
	if exp3, _ := p3["expiresAt"].(float64); exp3 != limit {
		t.Fatalf("capped expiry %v, want %v", exp3, limit)
	}
	if msg := wantFatal(t, r.wait(), "SESSION_FAILED", true); !strings.Contains(msg, "end of the Console sign-in window") {
		t.Fatalf("message %q", msg)
	}
	if now := float64(time.Now().UnixMilli()); now > limit+3000 {
		t.Fatalf("the sign in ran %v ms past the cap", now-limit)
	}
	noLiveLogin(t, lastLoginPID(t, h.home))
}

// Scrub covers every printable character an Anthropic key may hold (the
// sign in request file's rule), stopping at quotes and JSON delimiters.
func TestScrubAnthropicKeys(t *testing.T) {
	full := "sk-ant-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	if !ValidAPIKey(full) || !ValidAPIKey(testSigninKey) {
		t.Fatal("the fixtures must be valid keys (sk-ant- plus the assumed base64url body)")
	}
	for _, key := range []string{testSigninKey, full, "sk-ant-api03-a.b~c|d^e=f?g@h!i#j$k%l&m*n+o/p:q;r", "sk-ant-XYZ_123-abc"} {
		for _, in := range []string{key, "key " + key + " end", `{"key":"` + key + `"}`, "(" + key + ")", "<" + key + ">"} {
			out := Scrub(in)
			if strings.Contains(out, key[7:]) || !strings.Contains(out, "[redacted]") {
				t.Errorf("Scrub(%q) = %q", in, out)
			}
		}
	}
	if got := Scrub(`{"key":"sk-ant-abc","next":1}`); got != `{"key":"[redacted]","next":1}` {
		t.Errorf("JSON delimiters: %q", got)
	}
}
