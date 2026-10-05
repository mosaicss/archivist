// Command fakeclaude imitates the Claude Code 2.1.280 stream-json surface
// used by archivist connect, for tests only. It never calls a model.
//
// Configuration comes from $HOME/.fakeclaude/config.json (HOME is one of the
// few variables the daemon passes through). Every -p run records its argv,
// environment keys and working directory under $HOME/.fakeclaude/runs.
//
// Turn scenarios are chosen by the user text:
//
//	echo <text>     stream <text> back
//	bash <cmd>      ask can_use_tool for Bash and report the decision
//	write <path>    ask can_use_tool for Write and report the decision
//	mcp             list the archivist MCP tools and call search
//	publish <path>  call publish_artifact; asks can_use_tool unless the tool
//	                is in --allowedTools (as Claude Code does)
//	slow            stream numbers until interrupted (60 s cap)
//	stuck           stream numbers and ignore interrupts (60 s cap)
//	remember <w>    store a codeword in this Claude session's transcript
//	                ($HOME/.claude/projects/<cwd slug>/<session>.jsonl)
//	recall          reply with the stored codeword (the transcript is found
//	                in any project folder, as --resume finds it)
//	spawn           start a detached `sleep 300` in its own process group
//	orphanexit      start a detached `sleep 0.3` whose parent exits at once
//	exit            start streaming, then exit 3 mid-turn
//	wait <ms>       sleep, then reply "waited"
//	big <bytes>     write one stdout line of that many bytes
//	askmcp <tool>   ask can_use_tool for mcp__archivist__<tool> (a harness
//	                that prompts for an allowed tool) and report the decision
//	edit <path>     ask can_use_tool for Edit and report the decision
//	websearch <q>   run a WebSearch tool_use with a text tool_result (Story
//	                78.33); asks can_use_tool first unless WebSearch is in
//	                --allowedTools (as Claude Code 2.1.285 does), and refuses
//	                when WebSearch is not in --tools
//	askweb <q>      ask can_use_tool for WebSearch whatever the flags (a
//	                harness that prompts anyway) and report the decision
//
// config.json "exitBeforeInit": true exits 3 on the first turn before init.
// "resumeNotFound": true makes every --resume run report "No conversation
// found with session ID: <id>" and exit 1 before init, as Claude Code does
// when no stored transcript matches the id (Story 78.37).
//
// The init frame's tools are the --tools list plus the archivist MCP tools
// (Story 78.33); config.json "extraInitTools" adds names and
// "dropInitTools" removes them, for the tool proof tests.
//
// Permission modes (Story 78.32): --permission-mode must be one of Claude
// Code's modes; init reports it (config.json "permissionMode" overrides
// the report, for proof tests). A set_permission_mode control request is
// answered at once (success with the mode, then a system/status frame) and
// recorded in $HOME/.fakeclaude/modes.jsonl; switching to bypassPermissions
// needs --allow-dangerously-skip-permissions (or a bypassPermissions
// launch), config.json "setModeError" refuses every switch and
// "setModeHang" never answers one; "setModeSilent" applies the switch
// without answering it.
//
// `auth login --claudeai` imitates Claude Code 2.1.285's sign-in (Story
// 78.22): it requires a terminal, prints the link (OSC 8 hyperlink and
// plain) and "Paste code here if prompted > ", reads one line, and on the
// code config.json "loginCode" (default "good-code") records a login with
// "loginAuthMethod" (default claude.ai) and exits 0; any other code exits 1.
// "loginNoURL" exits 1 without a link; "loginEnter" waits for Enter after
// "Login successful". Each login records its environment keys in
// $HOME/.fakeclaude/login.json (and appends it to logins.jsonl).
// "loginLoggedOut" leaves the login logged out after a good code.
//
// `auth login --console` (Story 78.38) imitates Claude Code 2.1.289's
// Console sign-in: the same terminal flow with a platform.claude.com link;
// a good code records a login with "consoleAuthMethod" (default api_key)
// and apiKeySource "/login managed key" (the init frame reports it).
//
// ANTHROPIC_API_KEY in the environment wins over any stored login, as in
// Claude Code: auth status reports {loggedIn: true, authMethod: api_key,
// apiKeySource: ANTHROPIC_API_KEY} and the init frame apiKeySource
// ANTHROPIC_API_KEY. Neither run nor status ever prints the key: each -p run
// records only "apiKeyFingerprint" (hex sha256 of the value), and each
// status call appends {"apiKeyFingerprint"} to statuses.jsonl.
// "statusIgnoresKey" makes auth status report the stored login even with
// the key set (an API key proof that must fail). Each login records its pid.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"
)

type config struct {
	LoggedIn        bool   `json:"loggedIn"`
	AuthMethod      string `json:"authMethod"`
	APIKeySource    string `json:"apiKeySource"`
	PermissionMode  string `json:"permissionMode"`
	ExitBeforeInit  bool   `json:"exitBeforeInit"`
	ResumeNotFound  bool   `json:"resumeNotFound"`
	LoginCode       string `json:"loginCode"`
	LoginAuthMethod string `json:"loginAuthMethod"`
	LoginNoURL      bool   `json:"loginNoURL"`
	LoginEnter      bool   `json:"loginEnter"`
	LoginLoggedOut  bool   `json:"loginLoggedOut"`
	// ConsoleAuthMethod is the authMethod a Console login records.
	ConsoleAuthMethod string `json:"consoleAuthMethod"`
	StatusIgnoresKey  bool   `json:"statusIgnoresKey"`
	SetModeError      bool   `json:"setModeError"`
	SetModeHang       bool   `json:"setModeHang"`
	SetModeSilent     bool   `json:"setModeSilent"`
	// ExtraInitTools and DropInitTools edit the init frame's tools (Story 78.33).
	ExtraInitTools []string `json:"extraInitTools"`
	DropInitTools  []string `json:"dropInitTools"`
}

var (
	home    = os.Getenv("HOME")
	base    = filepath.Join(home, ".fakeclaude")
	outMu   sync.Mutex
	stdout  = bufio.NewWriter(os.Stdout)
	cfg     = loadConfig()
	session string
	// mode is the live permission mode (guarded by modeMu).
	modeMu      sync.Mutex
	mode        string
	allowBypass bool
)

var claudeModes = map[string]bool{"default": true, "manual": true, "acceptEdits": true, "plan": true,
	"bypassPermissions": true, "auto": true, "dontAsk": true}

func currentMode() string {
	modeMu.Lock()
	defer modeMu.Unlock()
	return mode
}

// setPermissionMode answers a set_permission_mode control request.
func setPermissionMode(f map[string]any) {
	req, _ := f["request"].(map[string]any)
	want, _ := req["mode"].(string)
	_ = os.MkdirAll(base, 0o700)
	if fh, err := os.OpenFile(filepath.Join(base, "modes.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(map[string]any{"mode": want, "request_id": f["request_id"]})
		_, _ = fh.Write(append(b, '\n'))
		_ = fh.Close()
	}
	fail := func(msg string) {
		emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error",
			"request_id": f["request_id"], "error": msg}})
	}
	modeMu.Lock()
	bypassOK := allowBypass
	modeMu.Unlock()
	switch {
	case cfg.SetModeHang:
		return // never answered (a process about to stop)
	case cfg.SetModeError:
		fail("fakeclaude: set_permission_mode refused")
		return
	case !claudeModes[want]:
		fail("Invalid permission mode: " + want)
		return
	case want == "bypassPermissions" && !bypassOK:
		fail("Cannot set permission mode to bypassPermissions because the session was not launched with --dangerously-skip-permissions")
		return
	}
	modeMu.Lock()
	mode = want
	modeMu.Unlock()
	if cfg.SetModeSilent {
		return // applied, but the answer never comes (or comes late)
	}
	emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success",
		"request_id": f["request_id"], "response": map[string]any{"mode": want}}})
	emit(map[string]any{"type": "system", "subtype": "status", "permissionMode": want, "session_id": session})
}

func loadConfig() config {
	c := config{LoggedIn: true, AuthMethod: "claude.ai", APIKeySource: "none"}
	if b, err := os.ReadFile(filepath.Join(base, "config.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func emit(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	defer outMu.Unlock()
	_, _ = stdout.Write(append(b, '\n'))
	_ = stdout.Flush()
}

func main() {
	if helperMode() {
		return
	}
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Println("2.1.280 (Claude Code)")
		return
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		st := map[string]any{"loggedIn": cfg.LoggedIn, "authMethod": cfg.AuthMethod,
			"apiProvider": "firstParty", "subscriptionType": "max"}
		if cfg.APIKeySource != "" && cfg.APIKeySource != "none" {
			st["apiKeySource"] = cfg.APIKeySource
		}
		fp := apiKeyFingerprint()
		if fp != "" && !cfg.StatusIgnoresKey {
			st = map[string]any{"loggedIn": true, "authMethod": "api_key", "apiProvider": "firstParty",
				"apiKeySource": "ANTHROPIC_API_KEY"}
		}
		appendJSON("statuses.jsonl", map[string]any{"apiKeyFingerprint": fp})
		b, _ := json.Marshal(st)
		fmt.Println(string(b))
		if st["loggedIn"] != true {
			os.Exit(1)
		}
		return
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "login" {
		login(args[2:])
		return
	}
	if len(args) == 0 || args[0] != "-p" {
		fmt.Fprintln(os.Stderr, "fakeclaude: only --version, auth status and -p are supported")
		os.Exit(2)
	}
	flags := parseFlags(args[1:])
	record(args, flags)
	for _, required := range []string{"--input-format", "--output-format", "--verbose", "--include-partial-messages",
		"--strict-mcp-config", "--mcp-config", "--permission-prompt-tool", "--setting-sources", "--tools", "--allowedTools", "--add-dir",
		"--settings"} {
		if _, ok := flags[required]; !ok {
			fmt.Fprintln(os.Stderr, "fakeclaude: missing "+required)
			os.Exit(2)
		}
	}
	for _, banned := range []string{"--bare", "--dangerously-skip-permissions"} {
		if _, ok := flags[banned]; ok {
			fmt.Fprintln(os.Stderr, "fakeclaude: refused "+banned)
			os.Exit(2)
		}
	}
	mode = "default"
	if m, ok := flags["--permission-mode"]; ok {
		if !claudeModes[m] {
			fmt.Fprintln(os.Stderr, "fakeclaude: invalid --permission-mode "+m)
			os.Exit(2)
		}
		mode = m
	}
	if mode == "manual" {
		mode = "default"
	}
	var flagSettings map[string]any
	if json.Unmarshal([]byte(flags["--settings"]), &flagSettings) != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude: --settings is not a JSON object")
		os.Exit(2)
	}
	_, allowBypass = flags["--allow-dangerously-skip-permissions"]
	allowBypass = allowBypass || mode == "bypassPermissions"
	session = uuid()
	if id := flags["--resume"]; id != "" {
		if cfg.ResumeNotFound {
			fmt.Fprintln(os.Stderr, "No conversation found with session ID: "+id)
			os.Exit(1)
		}
		session = id
	}
	fmt.Fprintln(os.Stderr, "fakeclaude: stderr line with a secret Bearer abc.def and sk-ant-api03-SECRET")

	frames := make(chan map[string]any, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 1<<20), 32<<20)
		for sc.Scan() {
			var f map[string]any
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				if req, _ := f["request"].(map[string]any); f["type"] == "control_request" && req["subtype"] == "set_permission_mode" {
					setPermissionMode(f) // answered at once, even mid-turn
					continue
				}
				frames <- f
			}
		}
	}()
	r := &runner{frames: frames, flags: flags}
	defer r.close()
	for f := range frames {
		if f["type"] != "user" {
			continue
		}
		msg, _ := f["message"].(map[string]any)
		text, _ := msg["content"].(string)
		if !r.turn(text) {
			return
		}
	}
}

// The per session conversation lives where Claude Code keeps it:
// $HOME/.claude/projects/<cwd slug>/<session>.jsonl, the slug being the
// working directory with every character that is not a letter or a digit
// replaced by '-' (Story 78.37: the sandbox resume archive carries
// .claude/projects, so a resume test proves exactly that set).
func projectsDir() string { return filepath.Join(home, ".claude", "projects") }

func cwdSlug() string {
	cwd, _ := os.Getwd()
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, cwd)
}

// rememberTranscript appends the codeword to this session's transcript.
func rememberTranscript(word string) string {
	dir := filepath.Join(projectsDir(), cwdSlug())
	line, _ := json.Marshal(map[string]any{"type": "remember", "sessionId": session, "text": word})
	if os.MkdirAll(dir, 0o700) != nil {
		return "transcript not written"
	}
	f, err := os.OpenFile(filepath.Join(dir, session+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "transcript not written"
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
	return "ok, remembered"
}

// recallTranscript finds this session's transcript in any project folder
// (as --resume does since Claude Code 2.1.223) and returns the last codeword.
func recallTranscript() string {
	matches, _ := filepath.Glob(filepath.Join(projectsDir(), "*", session+".jsonl"))
	if len(matches) != 1 {
		return "nothing remembered"
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		return "nothing remembered"
	}
	word := ""
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct {
			Type, Text string
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Type == "remember" {
			word = e.Text
		}
	}
	if word == "" {
		return "nothing remembered"
	}
	return word
}

func parseFlags(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		if k, v, ok := strings.Cut(a, "="); ok {
			out[k] = v
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[a] = args[i+1]
			i++
		} else {
			out[a] = ""
		}
	}
	return out
}

func record(args []string, flags map[string]string) {
	dir := filepath.Join(base, "runs")
	_ = os.MkdirAll(dir, 0o700)
	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cwd, _ := os.Getwd()
	run := map[string]any{"args": args, "envKeys": keys, "cwd": cwd, "pid": os.Getpid(), "apiKeyFingerprint": apiKeyFingerprint()}
	// Story 78.31: the appended system prompt file's content, as Claude reads it.
	if path, ok := flags["--append-system-prompt-file"]; ok {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude: --append-system-prompt-file:", err)
			os.Exit(1)
		}
		run["appendSystemPrompt"] = string(b)
	}
	b, _ := json.MarshalIndent(run, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())), b, 0o600)
}

type runner struct {
	frames <-chan map[string]any
	flags  map[string]string
	mcp    *mcp.ClientSession
	msgN   int
}

func (r *runner) close() {
	if r.mcp != nil {
		_ = r.mcp.Close()
	}
}

// listFlag splits a comma separated flag value.
func (r *runner) listFlag(name string) []string {
	var out []string
	for _, t := range strings.Split(r.flags[name], ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (r *runner) initFrame() {
	var tools []string
	for _, t := range r.listFlag("--tools") {
		if !slices.Contains(cfg.DropInitTools, t) {
			tools = append(tools, t)
		}
	}
	tools = append(tools, cfg.ExtraInitTools...)
	var servers []map[string]any
	if r.mcp != nil {
		if res, err := r.mcp.ListTools(context.Background(), nil); err == nil {
			for _, t := range res.Tools {
				tools = append(tools, "mcp__archivist__"+t.Name)
			}
		}
		servers = append(servers, map[string]any{"name": "archivist", "status": "connected"})
	}
	reported := currentMode()
	if cfg.PermissionMode != "" {
		reported = cfg.PermissionMode
	}
	keySource := cfg.APIKeySource
	if apiKeyFingerprint() != "" {
		keySource = "ANTHROPIC_API_KEY"
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": session, "cwd": mustCwd(),
		"apiKeySource": keySource, "permissionMode": reported, "tools": tools,
		"mcp_servers": servers, "plugins": []any{}, "claude_code_version": "2.1.280", "model": "fake-model"})
}

func mustCwd() string { d, _ := os.Getwd(); return d }

// turn runs one user turn; false ends the process (stdin closed).
func (r *runner) turn(text string) bool {
	if (strings.TrimSpace(text) == "mcp" || strings.HasPrefix(strings.TrimSpace(text), "publish ")) && r.mcp == nil {
		r.connectMCP()
	}
	if cfg.ExitBeforeInit {
		os.Exit(3)
	}
	r.initFrame()
	r.msgN++
	mid := fmt.Sprintf("msg_fake_%s_%d", session[:8], r.msgN)
	emit(stream(map[string]any{"type": "message_start", "message": map[string]any{"id": mid, "type": "message", "role": "assistant", "content": []any{}}}))
	cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	switch cmd {
	case "echo":
		r.say(0, arg)
	case "remember":
		r.say(0, rememberTranscript(arg))
	case "recall":
		r.say(0, recallTranscript())
	case "bash":
		return r.ask("Bash", map[string]any{"command": arg})
	case "askmcp":
		return r.ask("mcp__archivist__"+arg, map[string]any{"query": "revenue"})
	case "edit":
		return r.ask("Edit", map[string]any{"file_path": arg, "old_string": "a", "new_string": "b"})
	case "write":
		return r.ask("Write", map[string]any{"file_path": arg, "content": "x"})
	case "websearch":
		return r.webSearch(arg)
	case "askweb":
		return r.ask("WebSearch", map[string]any{"query": arg})
	case "mcp":
		r.callMCP()
	case "publish":
		return r.publish(arg)
	case "orphanexit":
		// A detached grandchild whose parent exits at once and which exits
		// itself shortly after: an orphan for the subreaper to reap.
		pidFile := filepath.Join(base, "orphanexit.pid")
		spawnOrphan(pidFile, "0.3")
		r.say(0, "orphaned")
	case "spawn":
		c := groupSleeper()
		_ = c.Start()
		_ = os.WriteFile(filepath.Join(base, "spawned.pid"), []byte(fmt.Sprint(c.Process.Pid)), 0o600)
		r.say(0, "spawned")
	case "exit":
		emit(stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}))
		emit(stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "about to exit"}}))
		os.Exit(3)
	case "wait":
		ms, _ := strconv.Atoi(arg)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		r.say(0, "waited")
	case "big":
		n, _ := strconv.Atoi(arg)
		outMu.Lock()
		_, _ = stdout.WriteString(`{"type":"system","subtype":"padding","x":"` + strings.Repeat("x", n) + "\"}\n")
		_ = stdout.Flush()
		outMu.Unlock()
		r.say(0, "big done")
	case "slow":
		return r.slow(true)
	case "stuck":
		return r.slow(false)
	default:
		r.say(0, "unknown scenario: "+text)
	}
	emit(stream(map[string]any{"type": "message_stop"}))
	r.result(false, "completed", "done")
	return true
}

func stream(event map[string]any) map[string]any {
	return map[string]any{"type": "stream_event", "event": event, "session_id": session, "parent_tool_use_id": nil, "uuid": uuid()}
}

func (r *runner) say(index int, text string) {
	emit(stream(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}}))
	for len(text) > 0 {
		n := min(len(text), 7)
		emit(stream(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text[:n]}}))
		text = text[n:]
	}
	emit(stream(map[string]any{"type": "content_block_stop", "index": index}))
}

func (r *runner) toolUse(index int, id, name string, input map[string]any) {
	emit(stream(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}}))
	b, _ := json.Marshal(input)
	emit(stream(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(b)}}))
	emit(stream(map[string]any{"type": "content_block_stop", "index": index}))
}

func (r *runner) toolResult(id string, content any, isErr bool) {
	emit(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isErr}}}})
}

func (r *runner) result(isErr bool, reason, text string) {
	// The main loop assistant snapshot with its usage (partials already
	// streamed it, so the daemon only reads the context use from it).
	emit(map[string]any{"type": "assistant", "session_id": session, "parent_tool_use_id": nil,
		"message": map[string]any{"id": fmt.Sprintf("msg_fake_%s_%d", session[:8], r.msgN), "type": "message",
			"role": "assistant", "model": "fake-model", "content": []any{}, "usage": map[string]any{
				"input_tokens": 3, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 200, "output_tokens": 5}}})
	subtype := "success"
	if isErr {
		subtype = "error_during_execution"
	}
	emit(map[string]any{"type": "result", "subtype": subtype, "is_error": isErr, "terminal_reason": reason,
		"session_id": session, "result": text, "usage": map[string]any{"input_tokens": 3, "output_tokens": 5,
			"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, "modelUsage": map[string]any{"fake-model": map[string]any{"contextWindow": 1000000}}})
}

func (r *runner) ask(tool string, input map[string]any) bool {
	toolID := "toolu_" + strings.ReplaceAll(uuid(), "-", "")[:20]
	r.toolUse(0, toolID, tool, input)
	reqID := uuid()
	emit(map[string]any{"type": "control_request", "request_id": reqID, "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": tool, "display_name": tool, "input": input,
		"description": tool, "tool_use_id": toolID}})
	for f := range r.frames {
		if f["type"] != "control_response" {
			continue
		}
		resp, _ := f["response"].(map[string]any)
		if resp["request_id"] != reqID {
			continue
		}
		body, _ := resp["response"].(map[string]any)
		if body["behavior"] == "allow" {
			in, _ := body["updatedInput"].(map[string]any)
			arg := in["command"]
			if tool != "Bash" {
				arg = in["file_path"]
			}
			if tool == "WebSearch" {
				r.toolResult(toolID, webSearchResult(fmt.Sprint(in["query"])), false)
				r.say(1, "allowed")
				emit(stream(map[string]any{"type": "message_stop"}))
				r.result(false, "completed", "done")
				return true
			}
			r.toolResult(toolID, fmt.Sprintf("ran: %v", arg), false)
			r.say(1, "allowed")
		} else {
			r.toolResult(toolID, fmt.Sprint(body["message"]), true)
			r.say(1, "denied: "+fmt.Sprint(body["message"]))
		}
		emit(stream(map[string]any{"type": "message_stop"}))
		r.result(false, "completed", "done")
		return true
	}
	return false
}

// webSearch imitates Claude Code's WebSearch (Story 78.33, probed on
// 2.1.285): a plain tool_use {query}, then a text tool_result listing the
// links and a reminder to hyperlink sources; the answer links an ordinary
// page and a filing source page. Without WebSearch in --allowedTools it
// asks can_use_tool first.
func (r *runner) webSearch(query string) bool {
	if !slices.Contains(r.listFlag("--tools"), "WebSearch") {
		r.say(0, "No such tool available: WebSearch")
		emit(stream(map[string]any{"type": "message_stop"}))
		r.result(false, "completed", "done")
		return true
	}
	input := map[string]any{"query": query}
	if !slices.Contains(r.listFlag("--allowedTools"), "WebSearch") {
		return r.ask("WebSearch", input)
	}
	toolID := "toolu_" + strings.ReplaceAll(uuid(), "-", "")[:20]
	r.toolUse(0, toolID, "WebSearch", input)
	r.toolResult(toolID, webSearchResult(query), false)
	r.say(1, "searched: [Reuters](https://www.reuters.com/markets/x) and [10-K](https://www.sec.gov/Archives/x.htm)")
	emit(stream(map[string]any{"type": "message_stop"}))
	r.result(false, "completed", "done")
	return true
}

func webSearchResult(query string) string {
	return fmt.Sprintf("Web search results for query: %q\n\nLinks: [{\"title\":\"Reuters\",\"url\":\"https://www.reuters.com/markets/x\"},"+
		"{\"title\":\"10-K\",\"url\":\"https://www.sec.gov/Archives/x.htm\"}]\n\nREMINDER: You MUST include the sources above in your response to the user using markdown hyperlinks.", query)
}

// slow streams numbers; honour=false ignores interrupt requests (a hung harness).
func (r *runner) slow(honour bool) bool {
	emit(stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}))
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(60 * time.Second)
	n := 0
	for {
		select {
		case f, ok := <-r.frames:
			if !ok {
				return false
			}
			req, _ := f["request"].(map[string]any)
			if honour && f["type"] == "control_request" && req["subtype"] == "interrupt" {
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success",
					"request_id": f["request_id"], "response": map[string]any{"still_queued": []any{}}}})
				emit(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"role": "user",
					"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
				r.result(true, "aborted_streaming", "")
				return true
			}
		case <-tick.C:
			n++
			emit(stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": fmt.Sprintf("%d\n", n)}}))
		case <-deadline:
			emit(stream(map[string]any{"type": "content_block_stop", "index": 0}))
			r.result(false, "completed", "done")
			return true
		}
	}
}

func (r *runner) connectMCP() {
	path := r.flags["--mcp-config"]
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude: mcp config:", err)
		return
	}
	var conf struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &conf); err != nil || len(conf.MCPServers) != 1 {
		fmt.Fprintln(os.Stderr, "fakeclaude: bad mcp config")
		return
	}
	s := conf.MCPServers["archivist"]
	c := exec.Command(s.Command, s.Args...)
	c.Env = os.Environ()
	for k, v := range s.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fakeclaude", Version: "2.1.280"}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: c}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude: mcp connect:", err)
		return
	}
	r.mcp = cs
}

// publish runs mcp__archivist__publish_artifact the way Claude Code does:
// a tool in --allowedTools runs at once, any other asks can_use_tool first.
func (r *runner) publish(path string) bool {
	const tool = "mcp__archivist__publish_artifact"
	toolID := "toolu_pub" + strings.ReplaceAll(uuid(), "-", "")[:16]
	input := map[string]any{"path": path}
	r.toolUse(0, toolID, tool, input)
	allowed := false
	for _, t := range strings.Split(r.flags["--allowedTools"], ",") {
		if t == tool {
			allowed = true
		}
	}
	if !allowed {
		reqID := uuid()
		emit(map[string]any{"type": "control_request", "request_id": reqID, "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": tool, "display_name": "publish_artifact", "input": input,
			"description": "Publish artifact", "tool_use_id": toolID}})
		var body map[string]any
		for f := range r.frames {
			resp, _ := f["response"].(map[string]any)
			if f["type"] == "control_response" && resp["request_id"] == reqID {
				body, _ = resp["response"].(map[string]any)
				break
			}
		}
		if body == nil {
			return false
		}
		if body["behavior"] != "allow" {
			r.toolResult(toolID, fmt.Sprint(body["message"]), true)
			r.say(1, "publish denied")
			emit(stream(map[string]any{"type": "message_stop"}))
			r.result(false, "completed", "done")
			return true
		}
		if in, ok := body["updatedInput"].(map[string]any); ok {
			input = in
		}
	}
	text, isErr := "mcp unavailable", true
	if r.mcp != nil {
		call, err := r.mcp.CallTool(context.Background(), &mcp.CallToolParams{Name: "publish_artifact", Arguments: input})
		if err != nil {
			text = err.Error()
		} else {
			text, isErr = "", call.IsError
			for _, c := range call.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					text += tc.Text
				}
			}
		}
	}
	r.toolResult(toolID, []any{map[string]any{"type": "text", "text": text}}, isErr)
	if isErr {
		r.say(1, "publish failed")
	} else {
		r.say(1, "published")
	}
	emit(stream(map[string]any{"type": "message_stop"}))
	r.result(false, "completed", "done")
	return true
}

func (r *runner) callMCP() {
	if r.mcp == nil {
		r.say(0, "mcp unavailable")
		return
	}
	res, err := r.mcp.ListTools(context.Background(), nil)
	if err != nil {
		r.say(0, "tools/list failed")
		return
	}
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	toolID := "toolu_mcp" + strings.ReplaceAll(uuid(), "-", "")[:16]
	r.toolUse(0, toolID, "mcp__archivist__search", map[string]any{"query": "revenue"})
	call, err := r.mcp.CallTool(context.Background(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "revenue"}})
	text := ""
	isErr := err != nil
	if err == nil {
		isErr = call.IsError
		for _, c := range call.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
	} else {
		text = err.Error()
	}
	r.toolResult(toolID, []any{map[string]any{"type": "text", "text": text}}, isErr)
	r.say(1, "tools: "+strings.Join(names, ","))
}

const loginURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=fake&response_type=code" +
	"&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&state=fakestate"

const consoleLoginURL = "https://platform.claude.com/oauth/authorize?code=true&client_id=fake&response_type=code" +
	"&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Ainference&state=fakestate"

// apiKeyFingerprint is the hex sha256 of ANTHROPIC_API_KEY ("" when unset):
// tests compare it, the key itself is never printed or stored.
func apiKeyFingerprint() string {
	v, ok := os.LookupEnv("ANTHROPIC_API_KEY")
	if !ok {
		return ""
	}
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

// appendJSON appends one JSON line to a file under $HOME/.fakeclaude.
func appendJSON(name string, v any) {
	_ = os.MkdirAll(base, 0o700)
	if fh, err := os.OpenFile(filepath.Join(base, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(v)
		_, _ = fh.Write(append(b, '\n'))
		_ = fh.Close()
	}
}

// login imitates `claude auth login --claudeai` (or --console) on a terminal.
func login(args []string) {
	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_ = os.MkdirAll(base, 0o700)
	entry := map[string]any{"args": args, "envKeys": keys, "tty": term.IsTerminal(int(os.Stdin.Fd())), "pid": os.Getpid()}
	rec, _ := json.Marshal(entry)
	_ = os.WriteFile(filepath.Join(base, "login.json"), rec, 0o600)
	appendJSON("logins.jsonl", entry)
	if len(args) != 1 || (args[0] != "--claudeai" && args[0] != "--console") {
		fmt.Fprintln(os.Stderr, "fakeclaude: auth login needs exactly --claudeai or --console")
		os.Exit(2)
	}
	console := args[0] == "--console"
	link := loginURL
	if console {
		link = consoleLoginURL
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "fakeclaude: auth login needs a terminal")
		os.Exit(2)
	}
	if cfg.LoginNoURL {
		fmt.Print("Something went wrong\r\n")
		os.Exit(1)
	}
	fmt.Print("Opening browser to sign in\u2026\r\nIf the browser didn't open, visit: \x1b]8;;" + link + "\x07\x1b[94m" +
		link + "\x1b[39m\x1b]8;;\x07\r\nPaste code here if prompted > ")
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadString('\n')
	if err != nil {
		os.Exit(3)
	}
	want := cfg.LoginCode
	if want == "" {
		want = "good-code"
	}
	if strings.TrimSpace(line) != want {
		fmt.Print("\r\nOAuth error: Invalid code\r\n")
		os.Exit(1)
	}
	method := cfg.LoginAuthMethod
	if method == "" {
		method = "claude.ai"
	}
	c := loadConfig()
	if console {
		method = cfg.ConsoleAuthMethod
		if method == "" {
			method = "api_key"
		}
		c.APIKeySource = "/login managed key"
	}
	c.LoggedIn, c.AuthMethod = !cfg.LoginLoggedOut, method
	b, _ := json.Marshal(c)
	_ = os.WriteFile(filepath.Join(base, "config.json"), b, 0o600)
	fmt.Print("\r\nLogin successful.\r\n")
	if cfg.LoginEnter {
		fmt.Print("Press Enter to continue\u2026")
		if _, err := in.ReadString('\n'); err != nil {
			os.Exit(3)
		}
	}
}
