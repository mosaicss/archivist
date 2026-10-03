// Command fakecodex imitates the codex-cli 0.160.0 app-server surface used
// by archivist connect (newline-delimited JSON-RPC on stdio), for tests
// only. It never calls a model.
//
// Configuration comes from $HOME/.fakecodex/config.json (HOME is one of the
// few variables the daemon passes through). Every app-server run records
// its argv, environment keys, CODEX_HOME and working directory under
// $HOME/.fakecodex/runs.
//
// Turn scenarios are chosen by the user text:
//
//	echo <text>      stream <text> back
//	remember <w>     store a codeword in the thread's rollout
//	recall           reply with the stored codeword
//	cmd <command>    ask a command approval; acceptForSession is cached
//	file <name>      ask a file change approval; accept writes the file
//	mcp              call the archivist MCP search tool (MCP approval when configured)
//	elicit           send a non-approval MCP elicitation
//	askuser          send item/tool/requestUserInput
//	perms            send item/permissions/requestApproval
//	unknown          send a server request archivist connect does not support
//	slow             stream numbers until interrupted (60 s cap)
//	stuck            stream numbers and ignore interrupts (60 s cap)
//	sleepcmd         approve-gated command that starts a setsid background
//	                 terminal (sleep 300) and runs until interrupted
//	grandchild       start a setsid sleep 300 child, then finish
//	orphan           start a setsid sleep 300 whose parent exits at once
//	usage            two token usage reports in one turn
//	plan             a plan update, then finish
//	reason           reasoning summary deltas, then finish
//	account          report an account/updated switch to API key auth
//	garbage          print a non-JSON stdout line, then finish
//	exit             start streaming, then exit 3 mid-turn
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type config struct {
	Version            string   `json:"version"`
	LoggedIn           *bool    `json:"loggedIn"`
	AccountType        string   `json:"accountType"`
	CodexHome          string   `json:"codexHome"`
	ModelProvider      string   `json:"modelProvider"`
	ExtraRoot          string   `json:"extraRoot"`
	InstructionSources []string `json:"instructionSources"`
	ExtraMCP           bool     `json:"extraMCP"`
	MCPApproval        bool     `json:"mcpApproval"`
}

var (
	home   = os.Getenv("HOME")
	base   = filepath.Join(home, ".fakecodex")
	cfg    = loadConfig()
	outMu  sync.Mutex
	stdout = bufio.NewWriter(os.Stdout)
)

func loadConfig() config {
	c := config{Version: "0.160.0", AccountType: "chatgpt", ModelProvider: "openai"}
	if b, err := os.ReadFile(filepath.Join(base, "config.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func write(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	defer outMu.Unlock()
	_, _ = stdout.Write(append(b, '\n'))
	_ = stdout.Flush()
}

func notify(method string, params any) {
	write(map[string]any{"method": method, "params": params})
}

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli " + cfg.Version)
		return
	}
	if len(args) == 2 && args[0] == "login" && args[1] == "status" {
		if cfg.LoggedIn != nil && !*cfg.LoggedIn {
			fmt.Fprintln(os.Stderr, "Not logged in")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "Logged in using ChatGPT")
		return
	}
	if len(args) == 0 || args[0] != "app-server" {
		fmt.Fprintln(os.Stderr, "fakecodex: only --version, login status and app-server are supported")
		os.Exit(2)
	}
	overrides, stdio, strict := parseArgs(args[1:])
	record(args)
	if !stdio || !strict {
		fmt.Fprintln(os.Stderr, "fakecodex: --stdio and --strict-config are required")
		os.Exit(2)
	}
	for k := range overrides {
		if k == "approval_policy" || strings.HasPrefix(k, "approval_policy.") {
			fmt.Fprintln(os.Stderr, "Error: approval_policy is not allowed in -c")
			os.Exit(2)
		}
	}
	s := &server{overrides: overrides, waits: map[int]chan json.RawMessage{}, cache: map[string]bool{}}
	s.run()
}

func parseArgs(args []string) (map[string]string, bool, bool) {
	out := map[string]string{}
	stdio, strict := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--stdio":
			stdio = true
		case "--strict-config":
			strict = true
		case "-c":
			if i+1 < len(args) {
				k, v, _ := strings.Cut(args[i+1], "=")
				out[k] = v
				i++
			}
		}
	}
	return out, stdio, strict
}

func record(args []string) {
	dir := filepath.Join(base, "runs")
	_ = os.MkdirAll(dir, 0o700)
	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cwd, _ := os.Getwd()
	b, _ := json.MarshalIndent(map[string]any{"args": args, "envKeys": keys, "codexHome": os.Getenv("CODEX_HOME"),
		"cwd": cwd, "pid": os.Getpid()}, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())), b, 0o600)
}

type msg struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  json.RawMessage  `json:"error"`
}

type server struct {
	overrides map[string]string

	mu        sync.Mutex
	nextID    int
	waits     map[int]chan json.RawMessage
	threadID  string
	turnID    string
	interrupt chan struct{}
	cache     map[string]bool // acceptForSession commands
	terminals []int
	mcp       *mcp.ClientSession
	tools     []string
	cwd       string
}

func (s *server) run() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for sc.Scan() {
		var m msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil {
			id, _ := strconv.Atoi(string(*m.ID))
			s.mu.Lock()
			ch := s.waits[id]
			delete(s.waits, id)
			s.mu.Unlock()
			if ch != nil {
				if len(m.Error) > 0 {
					ch <- json.RawMessage(`{"__error":` + string(m.Error) + `}`)
				} else {
					ch <- m.Result
				}
			}
			continue
		}
		if m.ID == nil {
			continue // initialized and other notifications
		}
		s.handle(*m.ID, m.Method, m.Params)
	}
	// stdin EOF: graceful shutdown.
	if s.mcp != nil {
		_ = s.mcp.Close()
	}
}

func reply(id json.RawMessage, result any) {
	write(map[string]any{"id": id, "result": result})
}

func replyErr(id json.RawMessage, code int, message string) {
	write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
}

// ask sends a server request and waits for the daemon's reply.
func (s *server) ask(method string, params any) json.RawMessage {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	ch := make(chan json.RawMessage, 1)
	s.waits[id] = ch
	s.mu.Unlock()
	write(map[string]any{"id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		notify("serverRequest/resolved", map[string]any{"threadId": s.threadID, "requestId": id})
		return r
	case <-time.After(5 * time.Minute):
		return nil
	}
}

func (s *server) handle(id json.RawMessage, method string, params json.RawMessage) {
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	switch method {
	case "initialize":
		ch := os.Getenv("CODEX_HOME")
		if cfg.CodexHome != "" {
			ch = cfg.CodexHome
		}
		reply(id, map[string]any{"userAgent": "fakecodex/" + cfg.Version, "codexHome": ch, "platformFamily": "unix", "platformOs": "linux"})
		notify("remoteControl/status/changed", map[string]any{"status": "disabled"})
	case "account/read":
		if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")); err != nil {
			reply(id, map[string]any{"account": nil, "requiresOpenaiAuth": true})
			return
		}
		acct := map[string]any{"type": cfg.AccountType}
		if cfg.AccountType == "chatgpt" {
			acct["email"], acct["planType"] = "owner@example.test", "pro"
		}
		reply(id, map[string]any{"account": acct, "requiresOpenaiAuth": true})
	case "model/list":
		reply(id, map[string]any{"data": []any{
			map[string]any{"id": "fake-other", "model": "fake-other", "isDefault": false},
			map[string]any{"id": "fake-default", "model": "fake-default", "isDefault": true},
		}, "nextCursor": nil})
	case "thread/start", "thread/resume":
		_ = os.MkdirAll(base, 0o700)
		_ = os.WriteFile(filepath.Join(base, strings.ReplaceAll(method, "/", "-")+".json"), params, 0o600)
		tid, _ := p["threadId"].(string)
		if method == "thread/start" {
			tid = uuid()
		} else if _, err := os.Stat(rollout(tid)); err != nil {
			replyErr(id, -32600, "no rollout found for thread id "+tid)
			return
		}
		_ = os.MkdirAll(filepath.Dir(rollout(tid)), 0o700)
		if _, err := os.Stat(rollout(tid)); err != nil {
			_ = os.WriteFile(rollout(tid), []byte(`{}`), 0o600)
		}
		s.threadID = tid
		s.cwd, _ = p["cwd"].(string)
		roots := []string{}
		if cfg.ExtraRoot != "" {
			roots = append(roots, cfg.ExtraRoot)
		}
		sources := cfg.InstructionSources
		if sources == nil {
			sources = []string{}
		}
		model, _ := p["model"].(string)
		reply(id, map[string]any{"thread": map[string]any{"id": tid, "cwd": s.cwd}, "model": model,
			"modelProvider": cfg.ModelProvider, "cwd": s.cwd, "instructionSources": sources,
			"approvalPolicy": p["approvalPolicy"], "approvalsReviewer": p["approvalsReviewer"],
			"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": false,
				"excludeTmpdirEnvVar": false, "excludeSlashTmp": false},
			"reasoningEffort": nil})
		s.startMCP()
	case "turn/start":
		input, _ := p["input"].([]any)
		text := ""
		if len(input) > 0 {
			in, _ := input[0].(map[string]any)
			text, _ = in["text"].(string)
		}
		s.mu.Lock()
		s.turnID = uuid()
		s.interrupt = make(chan struct{})
		turn, stop := s.turnID, s.interrupt
		s.mu.Unlock()
		reply(id, map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress", "error": nil}})
		go s.turn(turn, text, stop)
	case "turn/interrupt":
		s.mu.Lock()
		if s.interrupt != nil {
			close(s.interrupt)
			s.interrupt = nil
		}
		s.mu.Unlock()
		reply(id, map[string]any{})
	case "thread/backgroundTerminals/clean":
		s.mu.Lock()
		for _, pid := range s.terminals {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		s.terminals = nil
		s.mu.Unlock()
		reply(id, map[string]any{})
	default:
		replyErr(id, -32601, "method not found: "+method)
	}
}

func rollout(tid string) string {
	return filepath.Join(os.Getenv("CODEX_HOME"), "sessions", tid+".json")
}

var envRe = regexp.MustCompile(`([A-Z_]+)="([^"]*)"`)

func (s *server) startMCP() {
	status := func(name, st string, errText any) {
		notify("mcpServer/startupStatus/updated", map[string]any{"threadId": s.threadID, "name": name, "status": st,
			"error": errText, "failureReason": nil})
	}
	if cfg.ExtraMCP {
		status("codex_apps", "starting", nil)
	}
	if s.mcp != nil {
		status("archivist", "ready", nil)
		return
	}
	status("archivist", "starting", nil)
	var command string
	var args, enabled []string
	_ = json.Unmarshal([]byte(s.overrides["mcp_servers.archivist.command"]), &command)
	_ = json.Unmarshal([]byte(s.overrides["mcp_servers.archivist.args"]), &args)
	_ = json.Unmarshal([]byte(s.overrides["mcp_servers.archivist.enabled_tools"]), &enabled)
	c := exec.Command(command, args...)
	c.Env = os.Environ()
	for _, m := range envRe.FindAllStringSubmatch(s.overrides["mcp_servers.archivist.env"], -1) {
		c.Env = append(c.Env, m[1]+"="+m[2])
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fakecodex", Version: cfg.Version}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: c}, nil)
	if err != nil {
		status("archivist", "failed", err.Error())
		return
	}
	s.mcp = cs
	if res, err := cs.ListTools(context.Background(), nil); err == nil {
		for _, t := range res.Tools {
			for _, e := range enabled {
				if e == t.Name {
					s.tools = append(s.tools, t.Name)
				}
			}
		}
	}
	sort.Strings(s.tools)
	status("archivist", "ready", nil)
}

// ─── turns ──────────────────────────────────────────────────────────────────

type turnCtx struct {
	s      *server
	turn   string
	stop   <-chan struct{}
	itemN  int
	tokens int
}

func (t *turnCtx) base() map[string]any {
	return map[string]any{"threadId": t.s.threadID, "turnId": t.turn}
}

func (t *turnCtx) with(kv ...any) map[string]any {
	m := t.base()
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func (t *turnCtx) item(method string, item map[string]any) {
	notify(method, t.with("item", item))
}

func (t *turnCtx) say(text string) {
	t.itemN++
	id := fmt.Sprintf("msg_%s_%d", t.turn[:8], t.itemN)
	t.item("item/started", map[string]any{"type": "agentMessage", "id": id, "text": ""})
	for rest := text; len(rest) > 0; {
		n := min(len(rest), 7)
		notify("item/agentMessage/delta", t.with("itemId", id, "delta", rest[:n]))
		rest = rest[n:]
	}
	t.item("item/completed", map[string]any{"type": "agentMessage", "id": id, "text": text})
}

func (t *turnCtx) usage() {
	t.tokens += 10
	notify("thread/tokenUsage/updated", t.with("tokenUsage", map[string]any{
		"total": map[string]any{"totalTokens": t.tokens + 5, "inputTokens": t.tokens, "cachedInputTokens": 0, "outputTokens": 5, "reasoningOutputTokens": 0},
		"last":  map[string]any{"totalTokens": 15, "inputTokens": 10, "cachedInputTokens": 0, "outputTokens": 5, "reasoningOutputTokens": 0}}))
}

func (t *turnCtx) complete(status string) {
	t.s.mu.Lock()
	if t.s.turnID == t.turn {
		t.s.interrupt = nil
	}
	t.s.mu.Unlock()
	notify("turn/completed", map[string]any{"threadId": t.s.threadID, "turn": map[string]any{"id": t.turn, "items": []any{},
		"status": status, "error": nil}})
}

func decisionOf(r json.RawMessage) string {
	var d struct {
		Decision string `json:"decision"`
		Action   string `json:"action"`
	}
	_ = json.Unmarshal(r, &d)
	if d.Decision != "" {
		return d.Decision
	}
	return d.Action
}

func (s *server) turn(turn, text string, stop <-chan struct{}) {
	t := &turnCtx{s: s, turn: turn, stop: stop}
	notify("turn/started", map[string]any{"threadId": s.threadID, "turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress"}})
	t.item("item/started", map[string]any{"type": "userMessage", "id": "u-" + turn[:8], "content": []any{map[string]any{"type": "text", "text": text}}})
	t.item("item/completed", map[string]any{"type": "userMessage", "id": "u-" + turn[:8], "content": []any{map[string]any{"type": "text", "text": text}}})
	cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	switch cmd {
	case "echo":
		t.say(arg)
	case "remember":
		b, _ := json.Marshal(map[string]string{"codeword": arg})
		_ = os.WriteFile(rollout(s.threadID), b, 0o600)
		t.say("ok, remembered")
	case "recall":
		var r map[string]string
		b, _ := os.ReadFile(rollout(s.threadID))
		_ = json.Unmarshal(b, &r)
		if r["codeword"] == "" {
			t.say("nothing remembered")
		} else {
			t.say(r["codeword"])
		}
	case "cmd", "sleepcmd":
		if cmd == "sleepcmd" {
			arg = "sleep 300"
		}
		if t.command(arg, cmd == "sleepcmd") == "interrupted" {
			t.usage()
			t.complete("interrupted")
			return
		}
	case "file":
		t.fileChange(arg)
	case "mcp":
		t.mcpCall()
	case "elicit":
		r := s.ask("mcpServer/elicitation/request", t.with("serverName", "archivist", "mode", "form", "message", "Pick one",
			"requestedSchema", map[string]any{"type": "object", "properties": map[string]any{}}, "_meta", nil))
		t.say("elicitation: " + decisionOf(r))
	case "askuser":
		r := s.ask("item/tool/requestUserInput", t.with("itemId", "q1", "questions", []any{map[string]any{"id": "q", "question": "Which?"}}))
		t.say("answers: " + string(r))
	case "perms":
		r := s.ask("item/permissions/requestApproval", t.with("itemId", "p1", "environmentId", nil, "startedAtMs", 1,
			"cwd", s.cwd, "reason", "more", "permissions", map[string]any{"network": map[string]any{"enabled": true}}))
		t.say("permissions: " + string(r))
	case "unknown":
		r := s.ask("item/tool/call", t.with("callId", "c1", "tool", "x", "arguments", map[string]any{}))
		t.say("unknown: " + string(r))
	case "slow", "stuck":
		if t.stream(cmd == "slow") {
			t.usage()
			t.complete("interrupted")
			return
		}
	case "grandchild":
		c := exec.Command("sleep", "300")
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = c.Start()
		_ = os.WriteFile(filepath.Join(base, "grandchild.pid"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		t.say("spawned")
	case "orphan":
		pidFile := filepath.Join(base, "orphan.pid")
		c := exec.Command("sh", "-c", "setsid sleep 300 </dev/null >/dev/null 2>&1 & echo $! > "+pidFile)
		_ = c.Run()
		t.say("orphaned")
	case "usage":
		t.usage()
		t.say("two reports")
	case "plan":
		notify("turn/plan/updated", t.with("explanation", nil, "plan", []any{
			map[string]any{"step": "look", "status": "completed"}, map[string]any{"step": "write", "status": "inProgress"}}))
		t.say("planned")
	case "reason":
		for _, d := range []string{"Thinking ", "about it"} {
			notify("item/reasoning/summaryTextDelta", t.with("itemId", "rs_"+turn[:8], "delta", d, "summaryIndex", 0))
		}
		t.item("item/completed", map[string]any{"type": "reasoning", "id": "rs_" + turn[:8], "summary": []any{"Thinking about it"}})
		t.say("reasoned")
	case "account":
		notify("account/updated", map[string]any{"authMode": "apikey", "planType": nil})
		t.say("switched")
	case "garbage":
		outMu.Lock()
		_, _ = stdout.WriteString("this is not json\n")
		_ = stdout.Flush()
		outMu.Unlock()
		t.say("after garbage")
	case "exit":
		t.itemN++
		id := fmt.Sprintf("msg_%s_%d", turn[:8], t.itemN)
		t.item("item/started", map[string]any{"type": "agentMessage", "id": id, "text": ""})
		notify("item/agentMessage/delta", t.with("itemId", id, "delta", "about to exit"))
		os.Exit(3)
	default:
		t.say("unknown scenario: " + text)
	}
	t.usage()
	t.complete("completed")
}

// command runs one approval-gated command; it returns "interrupted" when
// the turn ended (cancel or interrupt).
func (t *turnCtx) command(command string, background bool) string {
	t.itemN++
	id := fmt.Sprintf("exec-%s-%d", t.turn[:8], t.itemN)
	item := map[string]any{"type": "commandExecution", "id": id, "command": command, "cwd": t.s.cwd,
		"status": "inProgress", "commandActions": []any{}, "aggregatedOutput": nil, "exitCode": nil, "durationMs": nil}
	t.item("item/started", item)
	decision := "accept"
	t.s.mu.Lock()
	cached := t.s.cache[command]
	t.s.mu.Unlock()
	if !cached {
		decision = decisionOf(t.s.ask("item/commandExecution/requestApproval", t.with("kind", "command", "itemId", id,
			"startedAtMs", time.Now().UnixMilli(), "environmentId", "local", "command", command, "cwd", t.s.cwd,
			"commandActions", []any{}, "reason", "fake needs approval",
			"availableDecisions", []any{"accept", "acceptForSession", "decline", "cancel"})))
	}
	switch decision {
	case "acceptForSession":
		t.s.mu.Lock()
		t.s.cache[command] = true
		t.s.mu.Unlock()
		fallthrough
	case "accept":
		if background {
			c := exec.Command("sleep", "300")
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			_ = c.Start()
			go func() { _ = c.Wait() }()
			t.s.mu.Lock()
			t.s.terminals = append(t.s.terminals, c.Process.Pid)
			t.s.mu.Unlock()
			_ = os.WriteFile(filepath.Join(base, "terminal.pid"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
			<-t.stop // runs until interrupted; the terminal outlives the turn
			item["status"] = "failed"
			t.item("item/completed", item)
			return "interrupted"
		}
		item["status"], item["aggregatedOutput"], item["exitCode"], item["durationMs"] = "completed", "ran: "+command+"\n", 0, 1
		t.item("item/completed", item)
		t.say("ran " + command)
	case "cancel":
		item["status"] = "declined"
		t.item("item/completed", item)
		return "interrupted"
	default:
		item["status"] = "declined"
		t.item("item/completed", item)
		t.say("declined " + command)
	}
	return ""
}

func (t *turnCtx) fileChange(name string) {
	t.itemN++
	id := fmt.Sprintf("patch-%s-%d", t.turn[:8], t.itemN)
	path := filepath.Join(t.s.cwd, name)
	changes := []any{map[string]any{"path": path, "kind": map[string]any{"type": "add"}, "diff": "+hello\n"}}
	item := map[string]any{"type": "fileChange", "id": id, "changes": changes, "status": "inProgress"}
	t.item("item/started", item)
	decision := decisionOf(t.s.ask("item/fileChange/requestApproval", t.with("itemId", id, "startedAtMs", time.Now().UnixMilli(),
		"reason", "create "+name)))
	if decision == "accept" || decision == "acceptForSession" {
		_ = os.WriteFile(path, []byte("hello\n"), 0o600)
		item["status"] = "completed"
		t.item("item/completed", item)
		t.say("wrote " + name)
		return
	}
	item["status"] = "declined"
	t.item("item/completed", item)
	t.say("file declined")
}

func (t *turnCtx) mcpCall() {
	t.itemN++
	id := fmt.Sprintf("mcp-%s-%d", t.turn[:8], t.itemN)
	args := map[string]any{"query": "revenue"}
	item := map[string]any{"type": "mcpToolCall", "id": id, "server": "archivist", "tool": "search", "status": "inProgress",
		"arguments": args, "result": nil, "error": nil}
	t.item("item/started", item)
	if cfg.MCPApproval {
		r := t.s.ask("mcpServer/elicitation/request", t.with("serverName", "archivist", "mode", "form",
			"message", "Allow archivist search?", "requestedSchema", map[string]any{"type": "object", "properties": map[string]any{}},
			"_meta", map[string]any{"codex_approval_kind": "mcp_tool_call", "persist": []any{"session", "always"},
				"tool_name": "search", "tool_params": args}))
		if decisionOf(r) != "accept" {
			item["status"] = "failed"
			item["error"] = map[string]any{"message": "user rejected MCP tool call"}
			t.item("item/completed", item)
			t.say("mcp declined")
			return
		}
	}
	if t.s.mcp == nil {
		item["status"], item["error"] = "failed", map[string]any{"message": "mcp unavailable"}
		t.item("item/completed", item)
		t.say("mcp unavailable")
		return
	}
	res, err := t.s.mcp.CallTool(context.Background(), &mcp.CallToolParams{Name: "search", Arguments: args})
	text := ""
	if err != nil {
		text = err.Error()
	} else {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
	}
	item["status"] = "completed"
	item["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	if err != nil || (res != nil && res.IsError) {
		item["status"] = "failed"
	}
	t.item("item/completed", item)
	t.say("tools: " + strings.Join(t.s.tools, ","))
}

// stream sends numbers; honour=false ignores interrupts. It reports
// whether the turn was interrupted.
func (t *turnCtx) stream(honour bool) bool {
	t.itemN++
	id := fmt.Sprintf("msg_%s_%d", t.turn[:8], t.itemN)
	t.item("item/started", map[string]any{"type": "agentMessage", "id": id, "text": ""})
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(60 * time.Second)
	stop := t.stop
	if !honour {
		stop = nil
	}
	n := 0
	text := ""
	for {
		select {
		case <-stop:
			return true
		case <-tick.C:
			n++
			d := fmt.Sprintf("%d\n", n)
			text += d
			notify("item/agentMessage/delta", t.with("itemId", id, "delta", d))
		case <-deadline:
			t.item("item/completed", map[string]any{"type": "agentMessage", "id": id, "text": text})
			return false
		}
	}
}
