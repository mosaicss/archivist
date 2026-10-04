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
//	remember <w>    store a codeword for this Claude session
//	recall          reply with the stored codeword
//	spawn           start a detached `sleep 300` in its own process group
//	orphanexit      start a detached `sleep 0.3` whose parent exits at once
//	exit            start streaming, then exit 3 mid-turn
//	wait <ms>       sleep, then reply "waited"
//	big <bytes>     write one stdout line of that many bytes
//
// config.json "exitBeforeInit": true exits 3 on the first turn before init.
//
// `auth login --claudeai` imitates Claude Code 2.1.285's sign-in (Story
// 78.22): it requires a terminal, prints the link (OSC 8 hyperlink and
// plain) and "Paste code here if prompted > ", reads one line, and on the
// code config.json "loginCode" (default "good-code") records a login with
// "loginAuthMethod" (default claude.ai) and exits 0; any other code exits 1.
// "loginNoURL" exits 1 without a link; "loginEnter" waits for Enter after
// "Login successful". Each login records its environment keys in
// $HOME/.fakeclaude/login.json.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	LoginCode       string `json:"loginCode"`
	LoginAuthMethod string `json:"loginAuthMethod"`
	LoginNoURL      bool   `json:"loginNoURL"`
	LoginEnter      bool   `json:"loginEnter"`
}

var (
	home    = os.Getenv("HOME")
	base    = filepath.Join(home, ".fakeclaude")
	outMu   sync.Mutex
	stdout  = bufio.NewWriter(os.Stdout)
	cfg     = loadConfig()
	session string
)

func loadConfig() config {
	c := config{LoggedIn: true, AuthMethod: "claude.ai", APIKeySource: "none", PermissionMode: "default"}
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
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Println("2.1.280 (Claude Code)")
		return
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		b, _ := json.Marshal(map[string]any{"loggedIn": cfg.LoggedIn, "authMethod": cfg.AuthMethod,
			"apiProvider": "firstParty", "subscriptionType": "max"})
		fmt.Println(string(b))
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
		"--strict-mcp-config", "--mcp-config", "--permission-prompt-tool", "--setting-sources", "--tools", "--allowedTools", "--add-dir"} {
		if _, ok := flags[required]; !ok {
			fmt.Fprintln(os.Stderr, "fakeclaude: missing "+required)
			os.Exit(2)
		}
	}
	for _, banned := range []string{"--permission-mode", "--bare", "--dangerously-skip-permissions"} {
		if _, ok := flags[banned]; ok {
			fmt.Fprintln(os.Stderr, "fakeclaude: refused "+banned)
			os.Exit(2)
		}
	}
	session = uuid()
	if id := flags["--resume"]; id != "" {
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
	run := map[string]any{"args": args, "envKeys": keys, "cwd": cwd, "pid": os.Getpid()}
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

func (r *runner) initFrame() {
	tools := []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep"}
	var servers []map[string]any
	if r.mcp != nil {
		if res, err := r.mcp.ListTools(context.Background(), nil); err == nil {
			for _, t := range res.Tools {
				tools = append(tools, "mcp__archivist__"+t.Name)
			}
		}
		servers = append(servers, map[string]any{"name": "archivist", "status": "connected"})
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": session, "cwd": mustCwd(),
		"apiKeySource": cfg.APIKeySource, "permissionMode": cfg.PermissionMode, "tools": tools,
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
		_ = os.MkdirAll(filepath.Join(base, "sessions"), 0o700)
		_ = os.WriteFile(filepath.Join(base, "sessions", session), []byte(arg), 0o600)
		r.say(0, "ok, remembered")
	case "recall":
		b, err := os.ReadFile(filepath.Join(base, "sessions", session))
		if err != nil {
			r.say(0, "nothing remembered")
		} else {
			r.say(0, string(b))
		}
	case "bash":
		return r.ask("Bash", map[string]any{"command": arg})
	case "write":
		return r.ask("Write", map[string]any{"file_path": arg, "content": "x"})
	case "mcp":
		r.callMCP()
	case "publish":
		return r.publish(arg)
	case "orphanexit":
		// A detached grandchild whose parent exits at once and which exits
		// itself shortly after: an orphan for the subreaper to reap.
		pidFile := filepath.Join(base, "orphanexit.pid")
		_ = exec.Command("sh", "-c", "setsid sleep 0.3 </dev/null >/dev/null 2>&1 & echo $! > "+pidFile).Run()
		r.say(0, "orphaned")
	case "spawn":
		c := exec.Command("sleep", "300")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	subtype := "success"
	if isErr {
		subtype = "error_during_execution"
	}
	emit(map[string]any{"type": "result", "subtype": subtype, "is_error": isErr, "terminal_reason": reason,
		"session_id": session, "result": text, "usage": map[string]any{"input_tokens": 3, "output_tokens": 5,
			"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, "modelUsage": map[string]any{"fake-model": map[string]any{}}})
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

// login imitates `claude auth login --claudeai` on a terminal.
func login(args []string) {
	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_ = os.MkdirAll(base, 0o700)
	rec, _ := json.Marshal(map[string]any{"args": args, "envKeys": keys, "tty": term.IsTerminal(int(os.Stdin.Fd()))})
	_ = os.WriteFile(filepath.Join(base, "login.json"), rec, 0o600)
	if len(args) != 1 || args[0] != "--claudeai" {
		fmt.Fprintln(os.Stderr, "fakeclaude: auth login needs exactly --claudeai")
		os.Exit(2)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "fakeclaude: auth login needs a terminal")
		os.Exit(2)
	}
	if cfg.LoginNoURL {
		fmt.Print("Something went wrong\r\n")
		os.Exit(1)
	}
	fmt.Print("Opening browser to sign in\u2026\r\nIf the browser didn't open, visit: \x1b]8;;" + loginURL + "\x07\x1b[94m" +
		loginURL + "\x1b[39m\x1b]8;;\x07\r\nPaste code here if prompted > ")
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
	c.LoggedIn, c.AuthMethod = true, method
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
