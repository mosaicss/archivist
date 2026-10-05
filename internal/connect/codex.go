package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/taskscope"
	"github.com/sourcegraph/jsonrpc2"
)

// Codex adapter (Story 78.17): one `codex app-server --stdio` per session,
// spoken to over JSON-RPC (sourcegraph/jsonrpc2). The binary comes from
// local detection, the argv and every -c override from this file, the model
// and effort from local flags or the session's checked choice; the relay
// supplies only prompts, decisions and session control ids (Story 78.32:
// approval policy and sandbox are mapped from the session's mode).

// CodexConfig is the local, non-relay configuration of the Codex adapter.
type CodexConfig struct {
	Bin     string
	Version string
	// Model is passed to thread/start; "" resolves model/list's default.
	Model string
	// Effort is the model_reasoning_effort for the thread ("" = Codex default).
	Effort string
	// OwnerHome is the owner's Codex home whose auth.json each session home
	// links to (OwnerCodexHome).
	OwnerHome string
	// Executable is this archivist binary, run as the MCP server.
	Executable string
	// BaseURL is passed to `mcp serve` only when ARCHIVIST_BASE_URL is set.
	BaseURL string
	// WebSearch is web_search "live" instead of "disabled" (Story 78.33; set
	// per session by spawnCodex from the daemon's Config.WebSearch).
	WebSearch bool
}

// codexWebSearchMode is the web_search value of a launch (Story 78.33):
// "live", Codex's own search in the OpenAI Responses backend with live
// access (news and prices need it; "cached" is an index without it), or
// "disabled". Codex never asks an approval for a search. Its standalone
// page fetch feature stays off (never set).
func codexWebSearchMode(cfg CodexConfig) string {
	if cfg.WebSearch {
		return "live"
	}
	return "disabled"
}

// CodexEfforts are the reasoning effort values of the pinned protocol
// (codex-rs protocol ReasoningEffort, 0.160.0).
var CodexEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "persistent"}

// Timings tests shorten; production values never change at runtime.
var (
	// codexHandshakeTimeout bounds initialize through archivist MCP ready.
	codexHandshakeTimeout = 60 * time.Second
	// codexSnapshotEvery is the descendant snapshot period while Codex runs.
	codexSnapshotEvery = 2 * time.Second
)

// codexShellEnv is what Codex passes to the commands it runs
// (shell_environment_policy include_only), on top of "core"; the CA bundle
// keys (CACertKeys) are listed too.
var codexShellEnv = append([]string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_*", "TERM", "TMPDIR", "TZ"},
	CACertKeys...)

// tomlValue renders v as a TOML value for -c (JSON strings and arrays of
// strings are valid TOML).
func tomlValue(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// codexArgs builds the fixed argv for one session's app-server. Each key
// was checked on codex-cli 0.160.0 with --strict-config (an unknown key is
// a startup error). approval_policy never appears here: it travels in
// thread/start and turn/start, the only places "untrusted" is accepted.
// Archivist MCP tools are approved without asking (Mosaic read tools in
// every mode), except the approval-required ones (publish_artifact), which
// prompt and so follow the session's mode.
func codexArgs(cfg CodexConfig, tokenFile string, publish []string) []string {
	set := [][2]string{
		// Login: the ChatGPT subscription from the linked auth.json only.
		{"forced_login_method", `"chatgpt"`},
		{"cli_auth_credentials_store", `"file"`},
		{"model_provider", `"openai"`},
		// Owner features, plugins, hooks, apps and memories stay off.
		{"features.apps", "false"},
		{"features.plugins", "false"},
		{"features.hooks", "false"},
		{"features.memories", "false"},
		{"features.multi_agent", "false"},
		{"features.goals", "false"},
		{"features.browser_use", "false"},
		{"features.computer_use", "false"},
		{"features.image_generation", "false"},
		{"features.tool_suggest", "false"},
		{"features.skill_mcp_dependency_install", "false"},
		{"features.remote_plugin", "false"},
		{"features.realtime_conversation", "false"},
		{"skills.include_instructions", "false"},
		{"skills.bundled.enabled", "false"},
		{"notify", "[]"},
		{"web_search", tomlValue(codexWebSearchMode(cfg))},
		{"history.persistence", `"none"`},
		{"check_for_update_on_startup", "false"},
		{"analytics.enabled", "false"},
		// Commands see only a core environment.
		{"shell_environment_policy.inherit", `"core"`},
		{"shell_environment_policy.include_only", tomlValue(codexShellEnv)},
		// Sandbox: the session cwd only (not /tmp or $TMPDIR, where other
		// sessions' directories live), no network.
		{"sandbox_workspace_write.writable_roots", "[]"},
		{"sandbox_workspace_write.network_access", "false"},
		{"sandbox_workspace_write.exclude_slash_tmp", "true"},
		{"sandbox_workspace_write.exclude_tmpdir_env_var", "true"},
		// No project root detection: no trust entries are ever written.
		{"project_root_markers", "[]"},
		// The archivist MCP server under the session task token.
		{"mcp_servers.archivist.command", tomlValue(cfg.Executable)},
		{"mcp_servers.archivist.args", tomlValue(mcpServeArgs(tokenFile, publish))},
		{"mcp_servers.archivist.enabled_tools", tomlValue(taskscope.Tools())},
		{"mcp_servers.archivist.default_tools_approval_mode", `"approve"`},
		{"mcp_servers.archivist.startup_timeout_sec", "20"},
		{"mcp_servers.archivist.tool_timeout_sec", "120"},
	}
	// Approval-required tools (publish_artifact) always ask: the per-tool
	// override wins over the server default (Codex config reference,
	// mcp_servers.<id>.tools.<tool>.approval_mode).
	for _, tool := range taskscope.Tools() {
		if taskscope.ApprovalRequired(tool) {
			set = append(set, [2]string{"mcp_servers.archivist.tools." + tool + ".approval_mode", `"prompt"`})
		}
	}
	if cfg.BaseURL != "" {
		set = append(set, [2]string{"mcp_servers.archivist.env", "{ARCHIVIST_BASE_URL=" + tomlValue(cfg.BaseURL) + "}"})
	}
	args := []string{"app-server", "--stdio", "--strict-config"}
	for _, kv := range set {
		args = append(args, "-c", kv[0]+"="+kv[1])
	}
	return args
}

// codexHome prepares the session's private CODEX_HOME: a 0700 directory
// whose only seeded entry is auth.json, a symlink to the owner's auth.json.
// Codex rewrites auth.json in place on refresh, so the link keeps the
// owner's single login current; a copy would split the single-use refresh
// token between two homes. A resume needs the existing home (rollouts and
// thread state live there).
func (s *session) codexHome(resume bool) (string, error) {
	home, err := s.d.store.CodexHomePath(s.id)
	if err != nil {
		return "", err
	}
	owner := s.d.codex.OwnerHome
	if owner == "" || !filepath.IsAbs(owner) {
		return "", errors.New("the owner's Codex home is unknown")
	}
	if filepath.Clean(owner) == filepath.Clean(home) {
		return "", errors.New("the session home would be the owner's Codex home")
	}
	ownerAuth := filepath.Join(owner, "auth.json")
	if st, err := os.Stat(ownerAuth); err != nil || !st.Mode().IsRegular() {
		// Transient (the owner may log in again): a resumable session stays so.
		return "", fmt.Errorf("no Codex login file at %s; run 'codex login' and sign in with ChatGPT", ownerAuth)
	}
	if _, err := os.Stat(home); err != nil {
		if resume {
			return "", &lostError{"the Codex home of this session is gone"}
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return "", err
	}
	link := filepath.Join(home, "auth.json")
	target, err := os.Readlink(link)
	switch {
	case err == nil && target == ownerAuth:
	case err == nil:
		return "", &proofError{fmt.Sprintf("the session's auth.json links to %s, not the owner's login", target)}
	case errors.Is(err, os.ErrNotExist):
		if err := os.Symlink(ownerAuth, link); err != nil {
			return "", err
		}
	default:
		// Not a symlink: something wrote a separate login into the home.
		return "", &proofError{"the session's auth.json is not a link to the owner's login"}
	}
	return home, nil
}

// lostError means the session cannot continue (its Codex thread or home
// is gone): the session fails and later messages ask for a new session.
type lostError struct{ msg string }

func (e *lostError) Error() string { return e.msg }

// codexRun is the live state of one app-server process.
type codexRun struct {
	rpc      *codexRPC
	threadID string
	turnID   string
	// interruptWanted defers an interrupt until the turn id is known.
	interruptWanted bool
	// mcpItem is the latest in-progress mcpToolCall item (approval cards).
	mcpItem string
	// mcpSession is an MCP "allow for session": later MCP approvals are
	// accepted without a card, after the mode's backstop (Story 78.32).
	mcpSession bool
	// model and effort are sent with every turn/start: the session's, the
	// model replaced by the one thread/start or thread/resume reported.
	model  string
	effort string
	// turnSeq numbers turn/start calls; a late reply of an older call is
	// ignored (call results are queued apart from notifications).
	turnSeq int
	// started is the turn whose start chunk was sent (a repeated
	// turn/started is not a new turn).
	started string
	pending map[string]*codexPending
}

// disconnected reports a closed JSON-RPC connection.
func (c *codexRun) disconnected() bool {
	select {
	case <-c.rpc.conn.DisconnectNotify():
		return true
	default:
		return false
	}
}

// codexPending is a server approval request waiting for the relay.
type codexPending struct {
	id         jsonrpc2.ID
	kind       string // command | file | mcp
	toolCallID string
}

// spawnCodex starts the app-server and runs the handshake and the
// subscription proof; nothing reaches the relay before it passes.
func (s *session) spawnCodex(ctx context.Context, threadID, instructions string) error {
	cfg := s.d.codex
	cfg.WebSearch = s.d.webSearch
	home, err := s.codexHome(threadID != "")
	if err != nil {
		return err
	}
	// Temp files go inside the cwd (the sandbox excludes /tmp and $TMPDIR).
	tmp := filepath.Join(s.rec.Cwd, ".tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("session temp directory: %w", err)
	}
	env, err := BuildChildEnv(s.d.environ(), map[string]string{"TMPDIR": tmp})
	if err != nil {
		return err
	}
	// The one adapter-set key; BuildChildEnv refuses CODEX_ overrides.
	env = append(env, "CODEX_HOME="+home)
	proc, err := StartProc(ProcSpec{Bin: cfg.Bin, Args: codexArgs(cfg, s.tokenFile, s.publishArgsFor(s.token)), Env: env, Dir: s.rec.Cwd, Log: s.log})
	if err != nil {
		return fmt.Errorf("start codex: %w", err)
	}
	s.log.Printf("codex started (%s, env keys %v, resume=%v)", proc, EnvKeys(env), threadID != "")
	s.proc, s.procDone, s.procLines = proc, proc.Done(), nil
	s.cx = &codexRun{rpc: newCodexRPC(proc, s.log), pending: map[string]*codexPending{}}
	s.running, s.turnActive = false, false
	s.ctr.reset()
	s.art.reset()
	if err := s.codexHandshake(ctx, home, threadID, instructions, codexWebSearchMode(cfg)); err != nil {
		s.stopProcess(true)
		return err
	}
	s.running = true
	s.emitStatus("running", "")
	s.emitControls()
	s.snapTimer = time.NewTimer(codexSnapshotEvery)
	return nil
}

// codexHandshake is initialize, the subscription and isolation proof,
// thread/start or thread/resume and the archivist MCP server reaching
// ready. Notifications and requests that arrive meanwhile stay queued, in
// order, for the session loop.
// instructions (Mosaic's research guidance) travel as developerInstructions on
// both thread/start and thread/resume. webSearch is the web_search value the
// launch passed, which config/read must report (Story 78.33).
func (s *session) codexHandshake(ctx context.Context, home, threadID, instructions, webSearch string) error {
	c := s.cx
	hctx, cancel := context.WithTimeout(ctx, codexHandshakeTimeout)
	defer cancel()
	var init codexInitializeResult
	if err := c.rpc.conn.Call(hctx, "initialize", codexInitializeParams{
		ClientInfo:   codexClientInfo{Name: "archivist_connect", Title: "archivist connect", Version: s.d.appVersion},
		Capabilities: codexCapabilities{ExperimentalAPI: true},
	}, &init); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if !samePath(init.CodexHome, home) {
		return &proofError{fmt.Sprintf("Codex runs with home %q, not the session home", init.CodexHome)}
	}
	if err := c.rpc.conn.Notify(hctx, "initialized", nil); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	// Story 78.33: the effective web_search must be the launch's, set by its
	// -c flag (origin sessionFlags), so neither a config layer nor a default
	// turns search on or off behind the session's back.
	var conf codexConfigRead
	if err := c.rpc.conn.Call(hctx, "config/read", codexConfigReadParams{}, &conf); err != nil {
		var rpcErr *jsonrpc2.Error
		if errors.As(err, &rpcErr) {
			// Codex answered and refused: the setting cannot be proven.
			return &proofError{"config/read: " + rpcErr.Message}
		}
		return fmt.Errorf("config/read: %w", err)
	}
	if problem := configProblem(conf, webSearch); problem != "" {
		return &proofError{problem}
	}
	var acct codexAccountRead
	if err := c.rpc.conn.Call(hctx, "account/read", map[string]any{"refreshToken": false}, &acct); err != nil {
		return fmt.Errorf("account/read: %w", err)
	}
	if acct.Account == nil || acct.Account.Type != "chatgpt" {
		kind := "no account"
		if acct.Account != nil {
			kind = fmt.Sprintf("a %q account", acct.Account.Type)
		}
		return &proofError{fmt.Sprintf("Codex reports %s, not a ChatGPT login", kind)}
	}
	model, effort := s.codexModelEffort(hctx)
	if model == "" {
		var err error
		if model, err = s.codexDefaultModel(hctx); err != nil {
			return err
		}
	}
	mode := s.mode()
	params := codexThreadParams{ThreadID: threadID, Model: model, Cwd: s.rec.Cwd, ApprovalPolicy: codexApprovalPolicy(mode),
		ApprovalsReviewer: "user", Sandbox: codexSandboxMode(mode), DeveloperInstructions: instructions}
	if effort != "" {
		params.Config = map[string]any{"model_reasoning_effort": effort}
	}
	c.model, c.effort = model, effort
	var th codexThreadResult
	if threadID != "" {
		params.ExcludeTurns = true // no history in the reply (a long thread could exceed a line)
	}
	if threadID == "" {
		persist := false
		params.Ephemeral = &persist
		if err := c.rpc.conn.Call(hctx, "thread/start", params, &th); err != nil {
			return fmt.Errorf("thread/start: %w", err)
		}
	} else if err := c.rpc.conn.Call(hctx, "thread/resume", params, &th); err != nil {
		var rpcErr *jsonrpc2.Error
		if errors.As(err, &rpcErr) && threadGone(rpcErr.Message) {
			return &lostError{"Codex could not resume the thread: " + rpcErr.Message}
		}
		// Anything else may pass: the session stays resumable.
		return fmt.Errorf("thread/resume: %w", err)
	}
	if problem := threadProblem(th, s.rec.Cwd, threadID, mode); problem != "" {
		return &proofError{problem}
	}
	c.threadID = th.Thread.ID
	if th.Model != "" {
		// Codex's answer is authoritative (a resumed thread or a substitute
		// model): later turns and data-session-controls carry it.
		s.ctr.Model, c.model = th.Model, th.Model
	}
	if s.rec.CodexThreadID != c.threadID {
		s.rec.CodexThreadID = c.threadID
		s.save()
	}
	if err := s.codexAwaitMCP(hctx); err != nil {
		return err
	}
	network := "network off"
	if codexSandboxType(mode) == "dangerFullAccess" {
		network = "full access, network on"
	}
	s.log.Printf("codex proof ok: home %s, chatgpt account, model %s, provider %s, mode %s (approvals %s/user, sandbox %s, %s, roots %v), web_search %s, no instruction sources, archivist MCP ready",
		home, th.Model, th.ModelProvider, mode, codexApprovalPolicy(mode), codexSandboxType(mode), network, th.Sandbox.WritableRoots, webSearch)
	return nil
}

// configProblem checks the config/read response (Story 78.33): web_search
// must equal want, and its origin must be the session flags (-c). A missing
// field fails.
func configProblem(conf codexConfigRead, want string) string {
	var problems []string
	switch {
	case conf.Config.WebSearch == nil:
		problems = append(problems, "web_search not reported")
	case *conf.Config.WebSearch != want:
		problems = append(problems, fmt.Sprintf("web_search is %q, not %q", *conf.Config.WebSearch, want))
	}
	origin, ok := conf.Origins["web_search"]
	if !ok || origin.Name.Type != "sessionFlags" {
		problems = append(problems, fmt.Sprintf("web_search origin %q, not \"sessionFlags\"", origin.Name.Type))
	}
	if len(problems) == 0 {
		return ""
	}
	return "Codex web search setting: " + strings.Join(problems, "; ")
}

// threadGone reports a thread/resume error that means the thread cannot
// come back (its rollout is missing), as opposed to a passing failure. The
// two messages are codex-rs 0.160.0's ("no rollout found for thread id",
// "thread not found: <id>"); a wider match such as a missing model would
// delete a session that a restart with another --codex-model could resume.
func threadGone(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no rollout found") || strings.Contains(m, "thread not found")
}

// threadProblem checks the thread/start (or resume) response against the
// isolation the session requires: the approval policy and sandbox mapped
// from its mode (Story 78.32), and for workspaceWrite and readOnly the
// isolation fields. Missing fields fail.
func threadProblem(th codexThreadResult, cwd, resumed string, mode Mode) string {
	var problems []string
	if th.Thread.ID == "" {
		problems = append(problems, "no thread id")
	}
	if resumed != "" && th.Thread.ID != resumed {
		problems = append(problems, fmt.Sprintf("resumed thread %q, not %q", th.Thread.ID, resumed))
	}
	if th.ModelProvider != "openai" {
		problems = append(problems, fmt.Sprintf("model provider %q, not \"openai\"", th.ModelProvider))
	}
	wantPolicy, wantSandbox := codexApprovalPolicy(mode), codexSandboxType(mode)
	var policy string
	if json.Unmarshal(th.ApprovalPolicy, &policy) != nil || policy != wantPolicy {
		problems = append(problems, fmt.Sprintf("approval policy %s, not %q", string(th.ApprovalPolicy), wantPolicy))
	}
	if th.ApprovalsReviewer != "user" {
		problems = append(problems, fmt.Sprintf("approvals reviewer %q, not \"user\"", th.ApprovalsReviewer))
	}
	if !samePath(th.Cwd, cwd) {
		problems = append(problems, fmt.Sprintf("thread cwd %q, not the session directory", th.Cwd))
	}
	if th.Sandbox.Type != wantSandbox {
		problems = append(problems, fmt.Sprintf("sandbox %q, not %q", th.Sandbox.Type, wantSandbox))
	}
	flag := func(v *bool, want bool, what string) {
		switch {
		case v == nil:
			problems = append(problems, "sandbox "+what+" not reported")
		case *v != want:
			problems = append(problems, fmt.Sprintf("sandbox %s is %v", what, *v))
		}
	}
	switch wantSandbox {
	case "workspaceWrite":
		flag(th.Sandbox.NetworkAccess, false, "networkAccess")
		flag(th.Sandbox.ExcludeSlashTmp, true, "excludeSlashTmp")
		flag(th.Sandbox.ExcludeTmpdirEnvVar, true, "excludeTmpdirEnvVar")
	case "readOnly":
		flag(th.Sandbox.NetworkAccess, false, "networkAccess")
	}
	for _, root := range th.Sandbox.WritableRoots {
		if !samePath(root, cwd) {
			problems = append(problems, fmt.Sprintf("extra writable root %q", root))
		}
	}
	if len(th.InstructionSources) > 0 {
		problems = append(problems, fmt.Sprintf("instruction sources loaded: %v", th.InstructionSources))
	}
	return strings.Join(problems, "; ")
}

// codexAwaitMCP waits for the archivist MCP server to report ready. Any
// other MCP server starting, archivist failing, or a non-ChatGPT account
// update fails the proof. Other events go back to the queue, in order.
func (s *session) codexAwaitMCP(ctx context.Context) error {
	c := s.cx
	var keep []codexEvent
	defer func() { c.rpc.q.unshift(keep) }()
	for {
		batch := c.rpc.q.take()
		for i, ev := range batch {
			if ev.Notif && ev.Method == "mcpServer/startupStatus/updated" {
				var st codexMCPStatus
				_ = json.Unmarshal(ev.Params, &st)
				if st.Name != "archivist" {
					return &proofError{fmt.Sprintf("an MCP server other than archivist started (%q)", st.Name)}
				}
				switch st.Status {
				case "ready":
					keep = append(keep, batch[i+1:]...)
					return nil
				case "failed", "cancelled":
					msg := ""
					if st.Error != nil {
						msg = ": " + *st.Error
					}
					return &proofError{"the archivist MCP server " + st.Status + msg}
				}
				continue
			}
			if ev.Notif && ev.Method == "account/updated" {
				if problem := accountProblem(ev.Params); problem != "" {
					return &proofError{problem}
				}
			}
			keep = append(keep, ev)
		}
		select {
		case <-c.rpc.q.wake:
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ctx.Err() // the daemon is stopping: not a proof failure
			}
			return &proofError{"the archivist MCP server did not report ready"}
		case <-s.procDone:
			return errors.New("codex exited during startup")
		case <-c.rpc.conn.DisconnectNotify():
			return errors.New("the connection to codex closed during startup")
		}
	}
}

// accountProblem reports an account/updated that leaves the ChatGPT login.
func accountProblem(params json.RawMessage) string {
	var u codexAccountUpdated
	_ = json.Unmarshal(params, &u)
	if u.AuthMode == nil {
		return "Codex reports it is no longer logged in"
	}
	if *u.AuthMode != "chatgpt" {
		return fmt.Sprintf("Codex switched to %q authentication", *u.AuthMode)
	}
	return ""
}

// codexDefaultModel is model/list's default entry, passed explicitly.
func (s *session) codexDefaultModel(ctx context.Context) (string, error) {
	var cursor *string
	for range 10 {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		var list codexModelList
		if err := s.cx.rpc.conn.Call(ctx, "model/list", params, &list); err != nil {
			return "", fmt.Errorf("model/list: %w", err)
		}
		for _, m := range list.Data {
			if m.IsDefault && m.ID != "" {
				return m.ID, nil
			}
		}
		if list.NextCursor == nil || *list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	return "", errors.New("model/list names no default model; pass --codex-model")
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// ─── turns ──────────────────────────────────────────────────────────────────

// codexSendUser starts a turn; the start chunk follows turn/started.
func (s *session) codexSendUser(text string) {
	c := s.cx
	if c.disconnected() {
		// Codex is gone; the exit path reports the unsent message.
		s.queue = append(s.queue, text)
		return
	}
	c.turnSeq++
	// Every turn carries the session's current mode (Story 78.32): Codex
	// applies it to this turn and the ones after.
	mode := s.mode()
	c.rpc.dispatch("turn/start", codexTurnStartParams{ThreadID: c.threadID,
		Input:          []codexTextInput{{Type: "text", Text: text, TextElements: []any{}}},
		ApprovalPolicy: codexApprovalPolicy(mode), SandboxPolicy: codexSandboxPolicy(mode),
		Model: c.model, Effort: c.effort}, "turn/start:"+strconv.Itoa(c.turnSeq))
	c.turnID = ""
	c.interruptWanted = false
	s.turnActive = true
}

// codexInterrupt asks Codex to end the turn, then cleans the thread's
// background terminals (turn/interrupt leaves them running).
func (s *session) codexInterrupt() {
	c := s.cx
	if c.turnID == "" {
		c.interruptWanted = true
		return
	}
	c.interruptWanted = false
	c.rpc.dispatch("turn/interrupt", codexTurnRef{ThreadID: c.threadID, TurnID: c.turnID}, "turn/interrupt")
	c.rpc.dispatch("thread/backgroundTerminals/clean", codexThreadRef{ThreadID: c.threadID}, "clean")
}

// drainCodex handles every queued Codex event in order.
func (s *session) drainCodex() {
	for s.cx != nil {
		evs := s.cx.rpc.q.take()
		if len(evs) == 0 {
			return
		}
		for i, ev := range evs {
			if s.cx == nil {
				return
			}
			s.codexEvent(ev)
			if s.cx == nil && i+1 < len(evs) {
				s.log.Printf("codex stopped; %d queued message(s) discarded", len(evs)-i-1)
			}
		}
	}
}

func (s *session) codexEvent(ev codexEvent) {
	switch {
	case ev.Call != "":
		s.codexCallDone(ev)
	case ev.Notif:
		s.codexNotification(ev)
	default:
		s.codexServerRequest(ev)
	}
}

func (s *session) codexCallDone(ev codexEvent) {
	c := s.cx
	call := ev.Call
	if seq, ok := strings.CutPrefix(call, "turn/start:"); ok {
		if seq != strconv.Itoa(c.turnSeq) {
			return // a reply to an earlier turn/start: that turn is settled
		}
		call = "turn/start"
	}
	switch call {
	case "turn/start":
		if ev.Err != nil {
			s.log.Printf("turn/start failed: %v", ev.Err)
			s.emitError(Scrub("Codex did not start the turn: " + ev.Err.Error()))
			s.turnActive = false
			c.interruptWanted = false
			s.stopInterruptTimer()
			s.nextQueued()
			return
		}
		var r codexTurnResult
		if json.Unmarshal(ev.Result, &r) == nil && r.Turn.ID != "" && c.turnID == "" {
			c.turnID = r.Turn.ID
			if c.interruptWanted {
				s.codexInterrupt()
			}
		}
	case "turn/interrupt", "clean":
		if ev.Err != nil {
			s.log.Printf("%s failed: %v", ev.Call, ev.Err)
		}
	case "clean-after-interrupt":
		if ev.Err != nil {
			s.log.Printf("thread/backgroundTerminals/clean failed: %v", ev.Err)
		}
		// Anything an interrupted command left behind is gone with the turn.
		if s.proc != nil {
			s.proc.SweepOrphans()
		}
	}
}

func (s *session) codexNotification(ev codexEvent) {
	c := s.cx
	switch ev.Method {
	case "account/updated":
		if problem := accountProblem(ev.Params); problem != "" {
			s.failProof("Codex left the ChatGPT subscription login: " + problem)
		}
		return
	case "turn/started":
		var r codexTurnResult
		if json.Unmarshal(ev.Params, &r) == nil && r.Turn.ID != "" {
			if r.Turn.ID == c.started {
				return // repeated: the turn's start chunk was sent already
			}
			c.started = r.Turn.ID
			s.lastUsage = nil
			c.turnID = r.Turn.ID
			if c.interruptWanted {
				s.codexInterrupt()
			}
		}
	case "item/started", "item/completed":
		var env codexItemEnvelope
		var head codexItemHead
		if json.Unmarshal(ev.Params, &env) == nil && json.Unmarshal(env.Item, &head) == nil {
			if head.Type == "mcpToolCall" && ev.Method == "item/started" {
				c.mcpItem = head.ID
			}
			if ev.Method == "item/completed" && head.ID == c.mcpItem {
				c.mcpItem = "" // a later elicitation is not this call's
			}
			if head.Type == "commandExecution" && s.proc != nil {
				s.proc.Snapshot()
			}
		}
	case "serverRequest/resolved":
		var r codexResolved
		if json.Unmarshal(ev.Params, &r) == nil {
			id := strings.Trim(string(r.RequestID), `"`)
			for aid, p := range c.pending {
				if rpcIDString(p.id) == id {
					delete(c.pending, aid)
				}
			}
		}
		return
	case "mcpServer/startupStatus/updated":
		var st codexMCPStatus
		if json.Unmarshal(ev.Params, &st) == nil {
			if st.Name != "archivist" {
				s.failProof(fmt.Sprintf("Codex started an MCP server other than archivist (%q)", st.Name))
				return
			}
			if st.Status != "ready" {
				s.log.Printf("codex MCP server %q is %s", st.Name, st.Status)
			}
		}
		return
	}
	s.emitCodex(s.ctr.In(ev.Method, "", ev.Params)...)
	if ev.Method == "turn/completed" {
		var r codexTurnResult
		_ = json.Unmarshal(ev.Params, &r)
		s.turnActive = false
		c.turnID = ""
		c.interruptWanted = false
		c.mcpItem = ""
		s.stopInterruptTimer()
		s.log.Debugf("%s", s.ctr.Context)
		if r.Turn.Status == "interrupted" {
			s.emitStatus("interrupted", "")
			c.rpc.dispatch("thread/backgroundTerminals/clean", codexThreadRef{ThreadID: c.threadID}, "clean-after-interrupt")
		}
		s.flushCoalesced()
		if s.proc != nil {
			s.proc.Snapshot()
		}
		s.nextQueued()
	}
}

// codexServerRequest answers or forwards one server request. Every request
// gets exactly one reply: approvals after the relay decides, everything
// else at once (empty answers, nothing granted, or method-not-found).
func (s *session) codexServerRequest(ev codexEvent) {
	c := s.cx
	rpcID := rpcIDString(ev.ID)
	reply := func(result any) {
		if err := c.rpc.reply(ev.ID, result); err != nil {
			s.log.Printf("reply to codex %s failed: %v", ev.Method, err)
		}
	}
	switch ev.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		var h codexApprovalHead
		_ = json.Unmarshal(ev.Params, &h)
		if !s.running {
			reply(map[string]any{"decision": "decline"})
			return
		}
		inCwd := ev.Method == "item/fileChange/requestApproval" && s.mode() == ModeAutoEdits &&
			codexFileInCwd(ev.Params, s.ctr.files, s.rec.Cwd)
		if s.codexBackstop(reply, false, inCwd) {
			return // decided by the permission mode (Story 78.32): no card
		}
		kind := "command"
		if ev.Method == "item/fileChange/requestApproval" {
			kind = "file"
		}
		chunks := s.ctr.In(ev.Method, rpcID, ev.Params)
		if len(chunks) == 0 {
			s.log.Printf("unreadable %s declined", ev.Method)
			reply(map[string]any{"decision": "decline"})
			return
		}
		approvalID, _ := chunks[0]["approvalId"].(string)
		if s.proc != nil {
			s.proc.Snapshot()
		}
		if !s.emitApproval(chunks[0]) {
			reply(map[string]any{"decision": "decline"})
			return
		}
		c.pending[approvalID] = &codexPending{id: ev.ID, kind: kind, toolCallID: h.ItemID}
	case "mcpServer/elicitation/request":
		var e codexElicitation
		_ = json.Unmarshal(ev.Params, &e)
		if !s.running || e.Meta == nil || e.Meta.ApprovalKind != "mcp_tool_call" {
			s.log.Printf("codex elicitation from %q (mode %q) declined: only MCP tool approvals are forwarded", e.ServerName, e.Mode)
			reply(map[string]any{"action": "decline", "content": nil, "_meta": nil})
			return
		}
		// Never a read tool: Codex approves those itself (approve mode), and
		// with overlapping calls the latest item need not be this call.
		if s.codexBackstop(reply, true, false) {
			return // decided by the permission mode (Story 78.32): no card
		}
		if c.mcpSession {
			s.log.Printf("codex MCP approval accepted: allowed for this session")
			reply(map[string]any{"action": "accept", "content": map[string]any{}, "_meta": nil})
			return
		}
		turnID := c.turnID
		if e.TurnID != nil && *e.TurnID != "" {
			turnID = *e.TurnID
		}
		approvalID := codexApprovalID(turnID, rpcID)
		toolCallID := c.mcpItem
		if toolCallID == "" {
			toolCallID = approvalID
		}
		var descriptor any
		if decodeNumbers(ev.Params, &descriptor) != nil {
			reply(map[string]any{"action": "decline", "content": nil, "_meta": nil})
			return
		}
		if !s.emitApproval(Chunk{"type": "tool-approval-request", "approvalId": approvalID, "toolCallId": toolCallID,
			"approvalDescriptor": descriptor}) {
			reply(map[string]any{"action": "decline", "content": nil, "_meta": nil})
			return
		}
		c.pending[approvalID] = &codexPending{id: ev.ID, kind: "mcp", toolCallID: toolCallID}
	case "item/tool/requestUserInput":
		s.log.Printf("codex asked for user input; answered with no answers")
		reply(map[string]any{"answers": map[string]any{}})
	case "item/permissions/requestApproval":
		s.log.Printf("codex asked for extra permissions; granted nothing")
		reply(map[string]any{"permissions": map[string]any{}, "scope": "turn"})
	default:
		s.log.Printf("codex request %s refused: not supported by archivist connect", ev.Method)
		if err := c.rpc.replyError(ev.ID, jsonrpc2.CodeMethodNotFound, "not supported by archivist connect: "+ev.Method); err != nil {
			s.log.Printf("reply to codex %s failed: %v", ev.Method, err)
		}
	}
}

// codexDecision maps a relay answer onto Codex's decisions one to one.
// Policy amendments (acceptWithExecpolicyAmendment,
// applyNetworkPolicyAmendment) are never sent: they persist rules.
func codexDecision(decision, scope string) string {
	if decision == "allow" {
		if scope == "allow_always" {
			return "acceptForSession"
		}
		return "accept"
	}
	if scope == "reject_always" {
		return "cancel"
	}
	return "decline"
}

// codexAnswerApproval turns a relay resolution into Codex's reply.
func (s *session) codexAnswerApproval(in *Inbound) {
	c := s.cx
	p, ok := c.pending[in.ApprovalID]
	if !ok {
		return // unknown or already answered: ack only
	}
	delete(c.pending, in.ApprovalID)
	decision := codexDecision(in.Decision, in.Scope)
	var result any = map[string]any{"decision": decision}
	if p.kind == "mcp" {
		// Elicitation actions: accept, decline, cancel. "Allow for session"
		// is kept here (mcpSession), never as Codex persistence: Codex checks
		// a remembered approval before its approval policy, so a later
		// read_only could not decline it (codex-rs rust-v0.160.0
		// core/src/mcp_tool_call.rs mcp_tool_approval_is_remembered). Never
		// persist "always" either (it writes the owner's config).
		switch decision {
		case "accept":
			result = map[string]any{"action": "accept", "content": map[string]any{}, "_meta": nil}
		case "acceptForSession":
			c.mcpSession = true
			result = map[string]any{"action": "accept", "content": map[string]any{}, "_meta": nil}
		default:
			result = map[string]any{"action": decision, "content": nil, "_meta": nil}
		}
	}
	if err := c.rpc.reply(p.id, result); err != nil {
		s.log.Printf("approval reply to codex failed: %v", err)
		return
	}
	s.emitCodex(s.ctr.ApprovalResponse(in.ApprovalID, decision))
	if in.Scope != "" {
		data := map[string]any{"approvalId": in.ApprovalID, "scope": in.Scope}
		if p.toolCallID != "" {
			data["toolCallId"] = p.toolCallID
		}
		s.emit(Chunk{"type": "data-permission-scope", "data": data})
	}
}

// emitApproval queues an approval card; false means the card could not be
// sent (it does not fit a relay frame even reduced), so the request is
// declined instead of waiting for an answer that cannot come.
func (s *session) emitApproval(c Chunk) bool {
	s.flushCoalesced()
	if s.outbox.Emit(c) == 0 {
		s.log.Printf("approval card %v could not be sent; declined", c["approvalId"])
		return false
	}
	return true
}

// emitCodex sends translated chunks, holding data-usage back so a turn
// carries only its last usage report, just before finish or abort.
func (s *session) emitCodex(chunks ...Chunk) {
	for _, c := range s.art.observe(chunks) {
		switch c["type"] {
		case "data-usage":
			s.lastUsage = withContext(c, s.ctr.Context) // mosaic-event/3 context fields
			continue
		case "finish", "abort":
			if s.lastUsage != nil {
				s.emit(s.lastUsage)
				s.lastUsage = nil
			}
		}
		s.emit(c)
	}
}
