package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/guidance"
)

// Story 78.33: provider side web search. Claude Code's WebSearch and Codex's
// web_search are on with Config.WebSearch (archivist connect --web-search,
// on by default with --session) and off otherwise; a search is a read that
// never asks; Claude's WebFetch is never enabled; the guidance web variant
// goes only to sessions with web search.

// ─── units ──────────────────────────────────────────────────────────────────

func TestClaudeArgsWebSearch(t *testing.T) {
	cfg := ClaudeConfig{Bin: "/usr/bin/claude", Executable: "/opt/archivist"}
	off := claudeArgs(cfg, "/m", "/g.md", "/tmp/cwd", "")
	cfg.WebSearch = true
	on := claudeArgs(cfg, "/m", "/g.md", "/tmp/cwd", "")
	flag := func(args []string, name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("%s missing: %v", name, args)
		}
		return args[i+1]
	}
	// Off: exactly as before 78.33.
	if flag(off, "--tools") != "Bash,Read,Edit,Write,Glob,Grep" || flag(off, "--allowedTools") != archivistAllowedTools() {
		t.Fatalf("web off: %v", off)
	}
	if flag(on, "--tools") != "Bash,Read,Edit,Write,Glob,Grep,WebSearch" ||
		flag(on, "--allowedTools") != archivistAllowedTools()+",WebSearch" {
		t.Fatalf("web on: %v", on)
	}
	// Nothing else differs.
	if len(on) != len(off) {
		t.Fatalf("argv lengths %d and %d", len(on), len(off))
	}
	for i := range on {
		if on[i] != off[i] && (i == 0 || off[i-1] != "--tools" && off[i-1] != "--allowedTools") {
			t.Errorf("argument %d differs: %q and %q", i, on[i], off[i])
		}
	}
	// WebFetch never, in any form.
	for _, args := range [][]string{on, off} {
		if strings.Contains(strings.Join(args, " "), "WebFetch") {
			t.Fatalf("WebFetch in argv: %v", args)
		}
	}
}

func TestInitToolsProblem(t *testing.T) {
	base := []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "mcp__archivist__search"}
	with := func(extra ...string) []string { return append(slices.Clone(base), extra...) }
	for _, c := range []struct {
		name           string
		tools          []string
		web            bool
		problem, warns bool
	}{
		{"off, plain", base, false, false, false},
		{"on, search reported", with("WebSearch"), true, false, false},
		{"on, search missing", base, true, false, true},
		{"off, search reported", with("WebSearch"), false, true, false},
		{"off, fetch reported", with("WebFetch"), false, true, false},
		{"on, fetch reported", with("WebSearch", "WebFetch"), true, true, false},
		{"on, fetch without search", with("WebFetch"), true, true, true},
	} {
		problem, warning := initToolsProblem(c.tools, c.web)
		if (problem != "") != c.problem || (warning != "") != c.warns {
			t.Errorf("%s: problem %q warning %q", c.name, problem, warning)
		}
		if strings.ContainsAny(problem+warning, "—") {
			t.Errorf("%s: em dash", c.name)
		}
	}
}

func TestClaudeSessionEnv(t *testing.T) {
	env, err := BuildChildEnv([]string{"HOME=/h", "PATH=/bin", "CLAUDE_CODE_DISABLE_WEB_FETCH=0", "TMPDIR=/t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_") }) {
		t.Fatalf("the daemon's CLAUDE_ key passed: %v", env)
	}
	got := ClaudeSessionEnv(env)
	want := []string{"CLAUDE_CODE_DISABLE_WEB_FETCH=1", "HOME=/h", "PATH=/bin", "TMPDIR=/t"}
	if !slices.Equal(got, want) {
		t.Fatalf("env %v, want %v", got, want)
	}
	if again := ClaudeSessionEnv(got); !slices.Equal(again, want) {
		t.Fatalf("not idempotent: %v", again)
	}
	// Still never an override.
	if _, err := BuildChildEnv(nil, map[string]string{ClaudeDisableWebFetchKey: "1"}); err == nil {
		t.Fatal("CLAUDE_CODE_ override accepted")
	}
}

func TestClaudeWebSearchIsAReadOnlyWithWebSearch(t *testing.T) {
	d := &Daemon{webSearch: true}
	s := &session{d: d}
	if !s.claudeWebSearch("WebSearch") || s.claudeWebSearch("WebFetch") || s.claudeWebSearch("websearch") ||
		s.claudeWebSearch("mcp__archivist__WebSearch") {
		t.Fatal("claudeWebSearch with web search on")
	}
	d.webSearch = false
	if s.claudeWebSearch("WebSearch") {
		t.Fatal("WebSearch is a read with web search off")
	}
}

// ─── Claude sessions ────────────────────────────────────────────────────────

// claudeArgOf returns the value after name in the joined argv of run i.
func claudeArgOf(t *testing.T, h *harness, i int, name string) string {
	t.Helper()
	runs := fakeRuns(t, h.home)
	if len(runs) <= i {
		t.Fatalf("claude runs %d", len(runs))
	}
	j := slices.Index(runs[i].Args, name)
	if j < 0 || j+1 >= len(runs[i].Args) {
		t.Fatalf("%s missing: %v", name, runs[i].Args)
	}
	return runs[i].Args[j+1]
}

// Matrix rows "Sandbox start" and "Local daemon" for Claude: the argv,
// environment and guidance per setting; a search runs without a card.
func TestClaudeWebSearchLaunch(t *testing.T) {
	for _, web := range []bool{false, true} {
		h := newHarness(t, nil)
		h.webSearch = web
		h.start()
		sid := h.startSession("websearch brent crude price")
		h.waitFinishes(sid, 1)
		runs := fakeRuns(t, h.home)
		if len(runs) != 1 {
			t.Fatalf("web %v: runs %d", web, len(runs))
		}
		tools, allowed := claudeArgOf(t, h, 0, "--tools"), claudeArgOf(t, h, 0, "--allowedTools")
		joined := strings.Join(runs[0].Args, " ")
		if strings.Contains(joined, "WebFetch") {
			t.Fatalf("web %v: WebFetch in argv", web)
		}
		if !slices.Contains(runs[0].EnvKeys, ClaudeDisableWebFetchKey) {
			t.Fatalf("web %v: env keys %v", web, runs[0].EnvKeys)
		}
		h.api.mu.Lock()
		seen := slices.Clone(h.api.guidanceSeen)
		h.api.mu.Unlock()
		text := h.relay.text(sid)
		if !web {
			if tools != ClaudeBuiltinTools || allowed != archivistAllowedTools() {
				t.Fatalf("web off: --tools %s --allowedTools %s", tools, allowed)
			}
			if *runs[0].AppendSystemPrompt != guidance.Embedded(guidance.SurfaceMosaicUI, guidance.FormFull).Body ||
				!slices.Equal(seen, []string{"mosaic-ui/full auth="}) {
				t.Fatalf("web off: guidance requests %v", seen)
			}
			if !strings.Contains(text, "No such tool available: WebSearch") {
				t.Fatalf("web off: text %q", text)
			}
			if !strings.Contains(h.log.String(), "research guidance embedded (") {
				t.Fatalf("web off: log\n%s", h.log.String())
			}
			continue
		}
		if tools != ClaudeBuiltinTools+",WebSearch" || allowed != archivistAllowedTools()+",WebSearch" {
			t.Fatalf("web on: --tools %s --allowedTools %s", tools, allowed)
		}
		if *runs[0].AppendSystemPrompt != guidance.EmbeddedWeb(guidance.SurfaceMosaicUI, guidance.FormFull).Body ||
			!slices.Equal(seen, []string{"mosaic-ui/full/web auth="}) {
			t.Fatalf("web on: guidance requests %v", seen)
		}
		if !strings.Contains(h.log.String(), "research guidance web embedded (") {
			t.Fatalf("web on: log\n%s", h.log.String())
		}
		if h.cards(sid) != 0 || !strings.Contains(text, "searched:") {
			t.Fatalf("web on: cards %d text %q", h.cards(sid), text)
		}
		var call map[string]any
		for _, e := range h.relay.eventsOf(sid, "tool-input-available") {
			if p := e["payload"].(map[string]any); p["toolName"] == "WebSearch" {
				call = p
			}
		}
		if call == nil || call["input"].(map[string]any)["query"] != "brent crude price" {
			t.Fatalf("web on: no WebSearch tool call: %v", h.payloadTypes(sid))
		}
	}
}

// Rows "Claude WebSearch, any mode" and "Claude WebSearch while off": a
// can_use_tool for WebSearch is allowed by the backstop without a card in
// every mode with web search on, and decided by the mode with it off.
func TestClaudeWebSearchBackstop(t *testing.T) {
	cases := []struct {
		web   bool
		mode  string
		cards int
		text  string
	}{
		{true, "read_only", 0, "allowed"},
		{true, "ask", 0, "allowed"},
		{true, "auto_edits", 0, "allowed"},
		{false, "read_only", 0, "denied: " + readOnlyDenied},
		{false, "ask", 1, ""},
	}
	for _, c := range cases {
		h := newHarness(t, nil)
		h.webSearch = c.web
		h.start()
		sid := h.startWith("claude", "askweb latest cpi", map[string]any{"mode": c.mode})
		if c.cards > 0 {
			h.approve(sid, 1, "deny", "user", "")
		}
		h.waitFinishes(sid, 1)
		if n := h.cards(sid); n != c.cards {
			t.Errorf("web %v %s: cards %d, want %d", c.web, c.mode, n, c.cards)
		}
		if c.text != "" && !strings.Contains(h.relay.text(sid), c.text) {
			t.Errorf("web %v %s: text %q", c.web, c.mode, h.relay.text(sid))
		}
		if c.web && len(h.relay.eventsOf(sid, "tool-approval-response")) != 0 {
			t.Errorf("web %v %s: an approval event for a web search", c.web, c.mode)
		}
		// Not remembered: a session rule never answers a web search in the mode's place.
		if c.web {
			h.message(sid, "websearch rates")
			h.waitFinishes(sid, 2)
			if h.cards(sid) != 0 {
				t.Errorf("web %v %s: card on the second search", c.web, c.mode)
			}
		}
	}
}

// Row "Claude init proof": WebFetch reported, or WebSearch reported with
// web search off, fails the session closed; WebSearch missing with web
// search on runs and logs one warning.
func TestClaudeInitToolsProof(t *testing.T) {
	for _, c := range []struct {
		name string
		web  bool
		cfg  map[string]any
	}{
		{"fetch reported", true, map[string]any{"extraInitTools": []string{"WebFetch"}}},
		{"fetch reported, web off", false, map[string]any{"extraInitTools": []string{"WebFetch"}}},
		{"search reported, web off", false, map[string]any{"extraInitTools": []string{"WebSearch"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.cfg)
			h.webSearch = c.web
			h.start()
			sid := h.startSession("echo must not run")
			h.relay.waitStatus(t, sid, "failed", 1)
			if strings.Contains(h.relay.text(sid), "must not run") {
				t.Fatal("output escaped a failed tool proof")
			}
			errs := strings.Join(h.errorTexts(sid), " ")
			if !strings.Contains(errs, "Claude Code did not start with the tools archivist connect allows") {
				t.Fatalf("errors %q", errs)
			}
			runs := fakeRuns(t, h.home)
			waitFor(t, 10*time.Second, "claude killed", func() bool { return len(runs) == 1 && !processAlive(runs[0].PID) })
			if h.record(sid).Status != "failed" {
				t.Fatal("record not failed")
			}
		})
	}
	h := newHarness(t, map[string]any{"dropInitTools": []string{"WebSearch"}})
	h.webSearch = true
	h.start()
	sid := h.startSession("echo still runs")
	h.waitFinishes(sid, 1)
	h.message(sid, "echo twice")
	h.waitFinishes(sid, 2)
	if !strings.Contains(h.relay.text(sid), "still runs") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	if n := strings.Count(h.log.String(), "did not report its WebSearch tool"); n != 1 {
		t.Fatalf("warning lines %d:\n%s", n, h.log.String())
	}
}

// Row "Guidance fetch, web": a chat-api that answers web=1 with "web": true
// gives the live web text; one that ignores web=1 (no web field) gives the
// embedded web variant.
func TestWebGuidanceFetch(t *testing.T) {
	const liveWeb = "live mosaic-ui web guidance for this test\n"
	h := newHarness(t, nil)
	h.webSearch = true
	h.api.guidance = map[string]string{"mosaic-ui/full/web": liveWeb, "mosaic-ui/full": liveGuidance}
	h.start()
	sid := h.startSession("echo hello")
	h.waitFinishes(sid, 1)
	runs := fakeRuns(t, h.home)
	if len(runs) != 1 || *runs[0].AppendSystemPrompt != liveWeb {
		t.Fatalf("runs %+v", runs)
	}
	if !strings.Contains(h.log.String(), "research guidance web live ("+guidance.Digest(liveWeb)+")") {
		t.Fatalf("log:\n%s", h.log.String())
	}

	old := newHarness(t, nil)
	old.webSearch = true
	old.api.guidance = map[string]string{"mosaic-ui/full": liveGuidance}
	old.api.guidanceIgnoresWeb = true
	old.start()
	sid = old.startSession("echo hello")
	old.waitFinishes(sid, 1)
	runs = fakeRuns(t, old.home)
	want := guidance.EmbeddedWeb(guidance.SurfaceMosaicUI, guidance.FormFull)
	if len(runs) != 1 || *runs[0].AppendSystemPrompt != want.Body {
		t.Fatal("an old chat-api's plain text replaced the web variant")
	}
	if !strings.Contains(old.log.String(), "research guidance web embedded ("+want.Digest+"; live fetch: not the web variant)") {
		t.Fatalf("log:\n%s", old.log.String())
	}
}

// ─── Codex sessions ─────────────────────────────────────────────────────────

func codexOverride(t *testing.T, args []string, key string) string {
	t.Helper()
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			if k, v, ok := strings.Cut(args[i+1], "="); ok && k == key {
				return v
			}
		}
	}
	t.Fatalf("-c %s missing: %v", key, args)
	return ""
}

// Matrix rows "Sandbox start", "Local daemon", "Codex proof" and "Codex
// search item": web_search per setting, proven through config/read; a
// search becomes a web_search tool call with no approval, in the modes
// that sandbox it most (read_only: never/read-only; ask: untrusted/workspace-write).
func TestCodexWebSearch(t *testing.T) {
	for _, web := range []bool{false, true} {
		for _, mode := range []string{"read_only", "ask"} {
			h := newCodexHarness(t, nil)
			h.webSearch = web
			h.start()
			sid := h.startWith("codex", "websearch brent crude price", map[string]any{"mode": mode})
			h.waitFinishes(sid, 1)
			runs := codexRuns(t, h.home)
			if len(runs) != 1 {
				t.Fatalf("web %v %s: runs %d", web, mode, len(runs))
			}
			wantMode := `"disabled"`
			wantGuidance := guidance.Embedded(guidance.SurfaceMosaicUI, guidance.FormFull).Body
			if web {
				wantMode = `"live"`
				wantGuidance = guidance.EmbeddedWeb(guidance.SurfaceMosaicUI, guidance.FormFull).Body
			}
			if got := codexOverride(t, runs[0].Args, "web_search"); got != wantMode {
				t.Fatalf("web %v: web_search=%s", web, got)
			}
			if _, err := os.Stat(filepath.Join(h.home, ".fakecodex", "config-read.json")); err != nil {
				t.Fatalf("web %v: config/read not called: %v", web, err)
			}
			if h.threadStart()["developerInstructions"] != wantGuidance {
				t.Fatalf("web %v: developerInstructions are not the expected variant", web)
			}
			if !strings.Contains(h.log.String(), "web_search "+strings.Trim(wantMode, `"`)+", no instruction sources") {
				t.Fatalf("web %v: proof log\n%s", web, h.log.String())
			}
			if n := h.cards(sid); n != 0 {
				t.Fatalf("web %v %s: cards %d", web, mode, n)
			}
			types := strings.Join(h.payloadTypes(sid), " ")
			if !web {
				if strings.Contains(types, "tool-input") || !strings.Contains(h.relay.text(sid), "no web search") {
					t.Fatalf("web off: types %s text %q", types, h.relay.text(sid))
				}
				continue
			}
			if !strings.Contains(types, "tool-input-start tool-input-available tool-output-available") {
				t.Fatalf("web on %s: types %s", mode, types)
			}
			start := h.relay.eventsOf(sid, "tool-input-start")[0]["payload"].(map[string]any)
			avail := h.relay.eventsOf(sid, "tool-input-available")[0]["payload"].(map[string]any)
			out := h.relay.eventsOf(sid, "tool-output-available")[0]["payload"].(map[string]any)
			if start["toolName"] != "web_search" || avail["toolName"] != "web_search" ||
				avail["toolCallId"] != start["toolCallId"] || out["toolCallId"] != start["toolCallId"] {
				t.Fatalf("web on: %v %v %v", start, avail, out)
			}
			if in := avail["input"].(map[string]any); in["query"] != "brent crude price" || in["action"] == nil {
				t.Fatalf("web on: input %v", in)
			}
			if o := out["output"].(map[string]any); len(o["results"].([]any)) != 2 || o["query"] != "brent crude price" {
				t.Fatalf("web on: output %v", o)
			}
		}
	}
}

// Row "Codex search item", completed without started: all three chunks at completion.
func TestCodexWebSearchCompletedWithoutStart(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.webSearch = true
	h.start()
	sid := h.startCodexSession("websearchnostart cpi")
	h.waitFinishes(sid, 1)
	types := strings.Join(h.payloadTypes(sid), " ")
	if !strings.Contains(types, "tool-input-start tool-input-available tool-output-available") {
		t.Fatalf("types %s", types)
	}
	b, _ := json.Marshal(h.relay.eventsOf(sid, "tool-input-start"))
	if !strings.Contains(string(b), `"toolName":"web_search"`) {
		t.Fatalf("start %s", b)
	}
}

// Row "Codex proof" with web search on: config/read reporting another value
// fails the session closed before any turn.
func TestCodexWebSearchProofFailsClosed(t *testing.T) {
	h := newCodexHarness(t, map[string]any{"configReadWebSearch": "cached"})
	h.webSearch = true
	h.start()
	sid := h.startCodexSession("echo must not run")
	h.relay.waitStatus(t, sid, "failed", 1)
	if strings.Contains(h.relay.text(sid), "must not run") {
		t.Fatal("a turn ran past a failed web search proof")
	}
	if errs := strings.Join(h.errorTexts(sid), " "); !strings.Contains(errs, `web_search is "cached", not "live"`) {
		t.Fatalf("errors %q", errs)
	}
}
