//go:build !windows

package connect

import (
	"context"
	"encoding/json"
	"errors"
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
		Environ:   func() []string { return env },
		TempDir:   h.tmp,
		SignIn:    signIn,
		MaxMode:   h.maxMode,
		WebSearch: h.webSearch,
	}
	if agent == "claude" {
		cfg.Claude = ClaudeConfig{Bin: claudeBin, SettingSources: DefaultSettingSources, Executable: archivistBin,
			BaseURL: h.api.srv.URL}
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
			Model: h.sessionModel, Effort: h.sessionEffort})
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
	h := newHarness(t, map[string]any{"loggedIn": false, "authMethod": "none", "loginEnter": true})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "claude", "echo entered")
	h.authPrompt(s.SessionID)
	h.message(s.SessionID, "good-code")
	waitFor(t, 30*time.Second, "turn", func() bool { return strings.Contains(h.relay.text(s.SessionID), "entered") })
	r.stop()
}

func TestSessionModeClaudeSignInFailures(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		code string // pasted code ("" = none)
		want string
	}{
		{"wrong code", map[string]any{"loggedIn": false}, "bad-code", "did not accept the code"},
		{"proof fails", map[string]any{"loggedIn": false, "loginAuthMethod": "oauth_token"}, "good-code",
			"not logged in with a claude.ai subscription"},
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
	// Logged in, but not with a subscription: no sign-in, the proof fails.
	h := newHarness(t, map[string]any{"loggedIn": true, "authMethod": "api_key"})
	s := h.api.addSession("claude")
	r := h.runSession(s.SessionID, "claude", "", "echo never")
	wantFatal(t, r.wait(), "SESSION_FAILED", true)
	h.relay.waitStatus(t, s.SessionID, "failed", 1)
}

func TestSessionModeStopDuringSignIn(t *testing.T) {
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
