//go:build !windows

package connect

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

// ─── harness helpers (Story 78.17) ──────────────────────────────────────────

// newCodexHarness is a harness whose daemon also drives the fake Codex,
// with an owner Codex home holding auth.json, config.toml and rules.
func newCodexHarness(t *testing.T, fakeCfg map[string]any) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.codex = true
	if fakeCfg != nil {
		dir := filepath.Join(h.home, ".fakecodex")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(fakeCfg)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	owner := h.codexOwner()
	if err := os.MkdirAll(filepath.Join(owner, "rules"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"auth.json":           `{"auth_mode":"chatgpt","tokens":{"refresh_token":"single-use"}}`,
		"config.toml":         "model = \"owner-model\"\n[mcp_servers.owner]\ncommand = \"owner-mcp\"\n",
		"AGENTS.md":           "owner instructions\n",
		"rules/default.rules": "prefix_rule(pattern=[\"touch\"], decision=\"allow\")\n",
	} {
		if err := os.WriteFile(filepath.Join(owner, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// codexOwner is the owner's Codex home in a harness.
func (h *harness) codexOwner() string { return filepath.Join(h.home, ".codex-owner") }

// ownerDigest hashes the owner's Codex files that must never change.
func (h *harness) ownerDigest() string {
	sum := sha256.New()
	for _, name := range []string{"auth.json", "config.toml", "AGENTS.md", "rules/default.rules"} {
		b, _ := os.ReadFile(filepath.Join(h.codexOwner(), name))
		_, _ = fmt.Fprintf(sum, "%s:%x\n", name, sha256.Sum256(b))
	}
	entries, _ := os.ReadDir(h.codexOwner())
	for _, e := range entries {
		_, _ = fmt.Fprintf(sum, "entry:%s\n", e.Name())
	}
	return fmt.Sprintf("%x", sum.Sum(nil))
}

func (h *harness) startCodexSession(prompt string) string {
	h.t.Helper()
	s := h.api.addSession("codex")
	cid := "start-" + randomUUID()
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": cid, "sessionId": s.SessionID,
		"agent": "codex", "prompt": prompt})
	waitFor(h.t, 15*time.Second, "start ack", func() bool { return h.relay.acked(cid) })
	return s.SessionID
}

type codexRunRecord struct {
	Args      []string `json:"args"`
	EnvKeys   []string `json:"envKeys"`
	CodexHome string   `json:"codexHome"`
	TmpDir    string   `json:"tmpdir"`
	Cwd       string   `json:"cwd"`
	PID       int      `json:"pid"`
}

func codexRuns(t *testing.T, home string) []codexRunRecord {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(home, ".fakecodex", "runs"))
	var out []codexRunRecord
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(home, ".fakecodex", "runs", e.Name()))
		if err != nil {
			continue
		}
		var r codexRunRecord
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, 10*time.Second, "pid file "+filepath.Base(path), func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	return pid
}

// gone reports that pid no longer exists (zombies count as alive).
func gone(pid int) bool { return !processAlive(pid) }

// payloadTypes lists the event types of a session in order.
func (h *harness) payloadTypes(sid string) []string {
	var out []string
	for _, p := range h.relay.payloads(sid) {
		out = append(out, p["type"].(string))
	}
	return out
}

// ─── capabilities, check data, argv and environment ─────────────────────────

func TestCodexCapabilitiesReportUsable(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	caps := h.relay.lastCaps()
	if len(caps) != 2 || caps[1] != (Capability{Agent: "codex", Version: "0.160.0", Available: true, LoggedIn: true}) {
		t.Fatalf("capabilities %+v", caps)
	}

	// Detection: below the floor, logged out or without auth.json is not usable.
	bin := testCodexBinary(t)
	look := func(file string) (string, error) {
		if file == "codex" {
			return bin, nil
		}
		return "", os.ErrNotExist
	}
	for name, c := range map[string]struct {
		cfg     map[string]any
		noAuth  bool
		problem string
	}{
		"too old":    {map[string]any{"version": "0.159.2"}, false, "older than the supported floor 0.160.0"},
		"logged out": {map[string]any{"loggedIn": false}, false, "not logged in"},
		"no auth":    {nil, true, "no auth.json"},
		"newer":      {map[string]any{"version": "0.162.1"}, false, ""},
	} {
		home := t.TempDir()
		owner := filepath.Join(home, "owner")
		_ = os.MkdirAll(filepath.Join(home, ".fakecodex"), 0o700)
		_ = os.MkdirAll(owner, 0o700)
		if c.cfg != nil {
			b, _ := json.Marshal(c.cfg)
			_ = os.WriteFile(filepath.Join(home, ".fakecodex", "config.json"), b, 0o600)
		}
		if !c.noAuth {
			_ = os.WriteFile(filepath.Join(owner, "auth.json"), []byte("{}"), 0o600)
		}
		det := DetectWith(context.Background(), DetectOptions{LookPath: look, Run: ExecRunner, RunCombined: ExecCombinedRunner,
			Env: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, Dir: home, CodexHome: owner})
		if c.problem == "" {
			if !det.Codex.Usable() {
				t.Errorf("%s: not usable: %s", name, det.Codex.Problem)
			}
			continue
		}
		if det.Codex.Usable() || !strings.Contains(det.Codex.Problem, c.problem) {
			t.Errorf("%s: usable=%v problem %q", name, det.Codex.Usable(), det.Codex.Problem)
		}
		if caps := det.Capabilities(); len(caps) != 1 || caps[0].Available {
			t.Errorf("%s: capabilities %+v", name, caps)
		}
	}
	if got := OwnerCodexHome([]string{"CODEX_HOME=relative"}, "/home/u"); got != "/home/u/.codex" {
		t.Errorf("relative CODEX_HOME: %s", got)
	}
	if got := OwnerCodexHome([]string{"CODEX_HOME=/srv/codex/"}, "/home/u"); got != "/srv/codex" {
		t.Errorf("absolute CODEX_HOME: %s", got)
	}
}

func TestCodexArgsAreFixed(t *testing.T) {
	cfg := CodexConfig{Bin: "/usr/bin/codex", Executable: "/opt/archivist"}
	args := codexArgs(cfg, "/state/run/x/task-token")
	if args[0] != "app-server" || args[1] != "--stdio" || args[2] != "--strict-config" {
		t.Fatalf("args %v", args[:3])
	}
	set := map[string]string{}
	for i := 3; i < len(args); i += 2 {
		if args[i] != "-c" {
			t.Fatalf("argument %d is %q, not -c", i, args[i])
		}
		k, v, _ := strings.Cut(args[i+1], "=")
		set[k] = v
	}
	want := map[string]string{
		"forced_login_method":                               `"chatgpt"`,
		"cli_auth_credentials_store":                        `"file"`,
		"model_provider":                                    `"openai"`,
		"features.apps":                                     "false",
		"features.plugins":                                  "false",
		"features.hooks":                                    "false",
		"features.memories":                                 "false",
		"notify":                                            "[]",
		"web_search":                                        `"disabled"`,
		"history.persistence":                               `"none"`,
		"shell_environment_policy.inherit":                  `"core"`,
		"shell_environment_policy.include_only":             `["PATH","HOME","USER","LOGNAME","SHELL","LANG","LC_*","TERM","TMPDIR","TZ"]`,
		"sandbox_workspace_write.writable_roots":            "[]",
		"sandbox_workspace_write.network_access":            "false",
		"project_root_markers":                              "[]",
		"mcp_servers.archivist.command":                     `"/opt/archivist"`,
		"mcp_servers.archivist.args":                        `["mcp","serve","--token-file","/state/run/x/task-token"]`,
		"mcp_servers.archivist.enabled_tools":               `["companies_search","read_passage","read_section","search","toc"]`,
		"mcp_servers.archivist.default_tools_approval_mode": `"auto"`,
	}
	for k, v := range want {
		if set[k] != v {
			t.Errorf("-c %s=%s, want %s", k, set[k], v)
		}
	}
	for k := range set {
		if strings.HasPrefix(k, "approval_policy") || strings.HasPrefix(k, "model_reasoning") || k == "model" {
			t.Errorf("-c %s must not be set in argv", k)
		}
	}
	if _, ok := set["mcp_servers.archivist.env"]; ok {
		t.Error("env set without ARCHIVIST_BASE_URL")
	}
	cfg.BaseURL = "http://127.0.0.1:9"
	if !slices.Contains(codexArgs(cfg, "/t"), `mcp_servers.archivist.env={ARCHIVIST_BASE_URL="http://127.0.0.1:9"}`) {
		t.Error("ARCHIVIST_BASE_URL not passed to the MCP server")
	}
}

func TestCodexDecisionMapping(t *testing.T) {
	for _, c := range []struct{ decision, scope, want string }{
		{"allow", "", "accept"}, {"allow", "allow_once", "accept"}, {"allow", "allow_always", "acceptForSession"},
		{"deny", "", "decline"}, {"deny", "reject_once", "decline"}, {"deny", "reject_always", "cancel"},
	} {
		if got := codexDecision(c.decision, c.scope); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.decision, c.scope, got, c.want)
		}
	}
}

func TestReduceDescriptorKeepsCodexKeys(t *testing.T) {
	d := reduceDescriptor(map[string]any{"kind": "command", "command": strings.Repeat("x", 5000), "cwd": "/w",
		"reason": "why", "availableDecisions": []any{"accept", "decline"}, "itemId": "i1", "turnId": "t1",
		"changes":        []any{map[string]any{"path": "/w/a", "kind": map[string]any{"type": "add"}, "diff": strings.Repeat("+", 90000)}},
		"commandActions": []any{"dropped"}}).(map[string]any)
	for _, k := range []string{"kind", "command", "cwd", "reason", "availableDecisions", "itemId", "turnId", "changes", "truncated"} {
		if _, ok := d[k]; !ok {
			t.Errorf("descriptor lost %s", k)
		}
	}
	if _, ok := d["commandActions"]; ok {
		t.Error("descriptor kept commandActions")
	}
	ch := d["changes"].([]any)[0].(map[string]any)
	if ch["path"] != "/w/a" || ch["diff"] != nil {
		t.Errorf("changes %v", ch)
	}
}

func TestJSONRPCFrameFilter(t *testing.T) {
	for line, want := range map[string]bool{
		`{"method":"turn/started","params":{}}`:          true,
		`{"id":3,"method":"item/tool/call","params":{}}`: true,
		`{"id":"a","result":{}}`:                         true,
		`{"id":1,"error":{"code":-1,"message":"x"}}`:     true,
		`{"id":1,"result":null}`:                         true,
		`not json`:                                       false,
		`[1,2]`:                                          false,
		`{"id":1}`:                                       false,
		`{"id":-1,"method":"x"}`:                         false,
		`{"id":1,"method":"x","result":{}}`:              false,
		`{"method":""}`:                                  false,
		`{"result":{}}`:                                  false,
	} {
		if got := jsonrpcFrame([]byte(line)); got != want {
			t.Errorf("%s: %v", line, got)
		}
	}
}

// The jsonrpc2 handler runs inline in the read loop: it must hand every
// message off without waiting, however far behind the session is.
func TestCodexHandlerNeverBlocks(t *testing.T) {
	q := newCodexQueue()
	h := codexHandler{q: q}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 20000 {
			params := json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))
			h.Handle(context.Background(), nil, &jsonrpc2.Request{Method: "item/agentMessage/delta", Notif: true, Params: &params})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler blocked without a consumer")
	}
	evs := q.take()
	if len(evs) != 20000 || string(evs[19999].Params) != `{"n":19999}` {
		t.Fatalf("queued %d", len(evs))
	}
}

// ─── session rows ───────────────────────────────────────────────────────────

func TestCodexSessionLifecycle(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.codexEffort = "low"
	before := h.ownerDigest()
	h.start()
	sid := h.startCodexSession("echo hello codex")
	h.waitFinishes(sid, 1)
	if st := h.relay.statuses(sid); len(st) < 2 || st[0] != "starting" || st[1] != "running" {
		t.Fatalf("statuses %v", st)
	}
	types := slices.Compact(h.payloadTypes(sid))
	want := []string{"data-session-status", "start", "text-start", "text-delta", "text-end", "data-usage", "finish"}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("events %v", types)
	}
	if !strings.Contains(h.relay.text(sid), "hello codex") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	rec := h.record(sid)
	if rec.Agent != "codex" || rec.CodexThreadID == "" || rec.ClaudeSessionID != "" {
		t.Fatalf("record %+v", rec)
	}

	// Child environment: allowlisted keys plus the adapter's CODEX_HOME only.
	runs := codexRuns(t, h.home)
	if len(runs) != 1 {
		t.Fatalf("runs %d", len(runs))
	}
	run := runs[0]
	sessionHome := filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)
	if run.CodexHome != sessionHome || run.Cwd != rec.Cwd {
		t.Fatalf("CODEX_HOME %s cwd %s", run.CodexHome, run.Cwd)
	}
	for _, k := range run.EnvKeys {
		if k != "CODEX_HOME" && !allowedChildKeys[k] {
			t.Errorf("child env carried %s", k)
		}
	}
	if !slices.Contains(run.EnvKeys, "CODEX_HOME") {
		t.Error("CODEX_HOME not set")
	}
	if strings.Contains(strings.Join(run.Args, " "), "approval_policy") {
		t.Error("approval_policy in argv")
	}
	// Temp files stay in the session cwd; the sandbox excludes /tmp and $TMPDIR.
	if run.TmpDir != filepath.Join(rec.Cwd, ".tmp") {
		t.Fatalf("TMPDIR %q", run.TmpDir)
	}
	if st, err := os.Stat(run.TmpDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("session temp dir %v %v", st, err)
	}
	for _, kv := range []string{"sandbox_workspace_write.exclude_slash_tmp=true", "sandbox_workspace_write.exclude_tmpdir_env_var=true"} {
		if !slices.Contains(run.Args, kv) {
			t.Errorf("argv lacks -c %s", kv)
		}
	}
	// Session home: 0700 with auth.json linked (never copied) to the owner's.
	st, err := os.Stat(sessionHome)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("session home %v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(sessionHome, "auth.json")); err != nil || target != filepath.Join(h.codexOwner(), "auth.json") {
		t.Fatalf("auth.json link %q %v", target, err)
	}
	// thread/start: default model passed explicitly, effort in config, untrusted policy.
	var params map[string]any
	b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "thread-start.json"))
	_ = json.Unmarshal(b, &params)
	if params["model"] != "fake-default" || params["approvalPolicy"] != "untrusted" || params["approvalsReviewer"] != "user" ||
		params["sandbox"] != "workspace-write" || params["cwd"] != rec.Cwd ||
		params["config"].(map[string]any)["model_reasoning_effort"] != "low" {
		t.Fatalf("thread/start params %v", params)
	}

	// MCP task tools under the session task token.
	h.message(sid, "mcp")
	h.waitFinishes(sid, 2)
	if !strings.Contains(h.relay.text(sid), "tools: companies_search,read_passage,read_section,search,toc") {
		t.Fatalf("mcp text %q", h.relay.text(sid))
	}
	if b := h.api.researchBearers(); len(b) != 1 || !strings.HasPrefix(b[0], "Bearer mst_") {
		t.Fatalf("research bearers %v", b)
	}
	input := h.relay.eventsOf(sid, "tool-input-available")
	if len(input) != 1 || input[0]["payload"].(map[string]any)["toolName"] != "archivist.search" {
		t.Fatalf("mcp tool input %v", input)
	}

	// Two usage reports in one turn: only the last reaches the relay.
	h.message(sid, "usage")
	h.waitFinishes(sid, 3)
	usage := h.relay.eventsOf(sid, "data-usage")
	if len(usage) != 3 {
		t.Fatalf("usage events %d, want one per turn", len(usage))
	}
	// The fake reports 10 then 20 input tokens in the usage turn: the last wins.
	if got := usage[2]["payload"].(map[string]any)["data"].(map[string]any)["inputTokens"]; got != float64(20) {
		t.Fatalf("usage turn reported inputTokens %v, want the last report (20)", got)
	}

	// Stop: completed, home, cwd and token gone, no process left.
	h.command(sid, "stop_session")
	h.relay.waitStatus(t, sid, "completed", 1)
	waitFor(t, 15*time.Second, "codex stopped", func() bool { return gone(run.PID) })
	waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
	if _, err := os.Stat(sessionHome); !os.IsNotExist(err) {
		t.Fatalf("session home kept after stop: %v", err)
	}
	if _, err := os.Stat(rec.Cwd); !os.IsNotExist(err) {
		t.Fatalf("cwd kept after stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.codexOwner(), "auth.json")); err != nil {
		t.Fatalf("owner auth.json removed: %v", err)
	}
	if stray := h.strayProcesses(); len(stray) != 0 {
		t.Fatalf("processes left: %v", stray)
	}
	if h.ownerDigest() != before {
		t.Fatal("owner Codex files changed")
	}
	logs := h.log.String()
	for _, secret := range []string{"must-not-pass", "single-use"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q", secret)
		}
	}
}

func TestCodexApprovalRows(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("cmd touch a.txt")
	req := h.approve(sid, 1, "allow", "user", "allow_once")
	h.waitFinishes(sid, 1)
	turnStart := h.relay.eventsOf(sid, "start")[0]["payload"].(map[string]any)["messageId"].(string)
	if req["approvalId"] != turnStart+":approval:0" || !strings.HasPrefix(req["toolCallId"].(string), "exec-") {
		t.Fatalf("approval request %v", req)
	}
	d := req["approvalDescriptor"].(map[string]any)
	if d["command"] != "touch a.txt" || d["kind"] != "command" || d["cwd"] == nil || len(d["availableDecisions"].([]any)) != 4 {
		t.Fatalf("descriptor %v", d)
	}
	resp := h.relay.eventsOf(sid, "tool-approval-response")
	if len(resp) != 1 || resp[0]["payload"].(map[string]any)["reason"] != "accept" || resp[0]["payload"].(map[string]any)["approved"] != true {
		t.Fatalf("response %v", resp)
	}
	if sc := h.relay.eventsOf(sid, "data-permission-scope"); len(sc) != 1 {
		t.Fatalf("scope events %v", sc)
	}
	if !strings.Contains(h.relay.text(sid), "ran touch a.txt") {
		t.Fatalf("text %q", h.relay.text(sid))
	}

	// Allow for session: acceptForSession; the identical command runs unprompted.
	h.message(sid, "cmd ls -la")
	h.approve(sid, 2, "allow", "user", "allow_always")
	h.waitFinishes(sid, 2)
	h.message(sid, "cmd ls -la")
	h.waitFinishes(sid, 3)
	if n := len(h.relay.eventsOf(sid, "tool-approval-request")); n != 2 {
		t.Fatalf("approval requests %d after accept for session", n)
	}
	if strings.Count(h.relay.text(sid), "ran ls -la") != 2 {
		t.Fatalf("text %q", h.relay.text(sid))
	}

	// Deny once and relay timeout: decline, the turn continues.
	h.message(sid, "cmd rm x")
	h.approve(sid, 3, "deny", "user", "reject_once")
	h.waitFinishes(sid, 4)
	h.message(sid, "cmd rm y")
	h.approve(sid, 4, "deny", "timeout", "")
	h.waitFinishes(sid, 5)
	if strings.Count(h.relay.text(sid), "declined rm") != 2 {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	if n := len(h.relay.eventsOf(sid, "tool-output-denied")); n != 2 {
		t.Fatalf("denied outputs %d", n)
	}

	// Deny for session: cancel, the turn ends interrupted; the next message runs.
	h.message(sid, "cmd rm z")
	h.approve(sid, 5, "deny", "user", "reject_always")
	h.waitFinishes(sid, 6)
	waitFor(t, 10*time.Second, "interrupted status", func() bool {
		return len(h.relay.eventsOf(sid, "abort")) == 1 && slices.Contains(h.relay.statuses(sid), "interrupted")
	})
	h.message(sid, "echo still here")
	h.waitFinishes(sid, 7)
	reasons := []string{}
	for _, r := range h.relay.eventsOf(sid, "tool-approval-response") {
		reasons = append(reasons, r["payload"].(map[string]any)["reason"].(string))
	}
	if strings.Join(reasons, ",") != "accept,acceptForSession,decline,decline,cancel" {
		t.Fatalf("decisions %v", reasons)
	}
	// An unknown or already answered approval id is acknowledged only.
	cid := "resolved:" + randomUUID()
	h.relay.send(sid, map[string]any{"kind": "approval_response", "correlationId": cid, "sessionId": sid,
		"approvalId": req["approvalId"], "decision": "allow", "reason": "user"})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked(cid) })
	if n := len(h.relay.eventsOf(sid, "tool-approval-response")); n != 5 {
		t.Fatalf("a stale answer produced a response (%d)", n)
	}
}

func TestCodexFileChangeApproval(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("file notes.txt")
	req := h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 1)
	d := req["approvalDescriptor"].(map[string]any)
	changes, _ := d["changes"].([]any)
	if d["itemId"] != req["toolCallId"] || d["reason"] != "create notes.txt" || len(changes) != 1 {
		t.Fatalf("file descriptor %v", d)
	}
	patch := h.relay.eventsOf(sid, "data-patch")
	if len(patch) != 1 || patch[0]["payload"].(map[string]any)["data"].(map[string]any)["operation"] != "add" {
		t.Fatalf("data-patch %v", patch)
	}
	if _, err := os.Stat(filepath.Join(h.record(sid).Cwd, "notes.txt")); err != nil {
		t.Fatalf("accepted file not written: %v", err)
	}
	h.message(sid, "file other.txt")
	h.approve(sid, 2, "deny", "user", "")
	h.waitFinishes(sid, 2)
	if !strings.Contains(h.relay.text(sid), "file declined") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
}

func TestCodexMCPApprovalAndOtherServerRequests(t *testing.T) {
	h := newCodexHarness(t, map[string]any{"mcpApproval": true})
	h.start()
	sid := h.startCodexSession("mcp")
	req := h.approve(sid, 1, "allow", "user", "allow_once")
	h.waitFinishes(sid, 1)
	if !strings.HasPrefix(req["toolCallId"].(string), "mcp-") || !strings.Contains(req["approvalId"].(string), ":approval:") {
		t.Fatalf("mcp approval %v", req)
	}
	if !strings.Contains(h.relay.text(sid), "tools: ") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	h.message(sid, "mcp")
	h.approve(sid, 2, "deny", "user", "reject_once")
	h.waitFinishes(sid, 2)
	if !strings.Contains(h.relay.text(sid), "mcp declined") {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	for i, row := range []struct{ text, want string }{
		{"elicit", "elicitation: decline"},
		{"askuser", `answers: {"answers":{}}`},
		{"perms", `permissions: {"permissions":{},"scope":"turn"}`},
		{"unknown", `"code":-32601`},
	} {
		h.message(sid, row.text)
		h.waitFinishes(sid, 3+i)
		if !strings.Contains(h.relay.text(sid), row.want) {
			t.Fatalf("%s: text %q", row.text, h.relay.text(sid))
		}
	}
	if n := len(h.relay.eventsOf(sid, "tool-approval-request")); n != 2 {
		t.Fatalf("only MCP tool approvals become cards, got %d", n)
	}
	// A non-JSON stdout line is skipped, not fatal.
	h.message(sid, "garbage")
	h.waitFinishes(sid, 7)
	if !strings.Contains(h.relay.text(sid), "after garbage") || !strings.Contains(h.log.String(), "not a JSON-RPC message") {
		t.Fatalf("garbage row: %q", h.relay.text(sid))
	}
	// Plan and reasoning map onto existing schemas.
	h.message(sid, "plan")
	h.waitFinishes(sid, 8)
	h.message(sid, "reason")
	h.waitFinishes(sid, 9)
	if len(h.relay.eventsOf(sid, "data-plan")) != 1 || len(h.relay.eventsOf(sid, "reasoning-end")) != 1 {
		t.Fatalf("plan/reasoning events %v", h.payloadTypes(sid))
	}
}

func TestCodexInterruptRows(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("slow")
	waitFor(t, 15*time.Second, "slow output", func() bool { return strings.Contains(h.relay.text(sid), "2\n") })
	pid := codexRuns(t, h.home)[0].PID
	h.command(sid, "interrupt")
	h.waitFinishes(sid, 1)
	waitFor(t, 10*time.Second, "interrupted", func() bool {
		return len(h.relay.eventsOf(sid, "abort")) == 1 && slices.Contains(h.relay.statuses(sid), "interrupted")
	})
	h.message(sid, "echo after interrupt")
	h.waitFinishes(sid, 2)
	if gone(pid) || len(codexRuns(t, h.home)) != 1 {
		t.Fatal("the app-server did not stay for the next turn")
	}

	// Interrupt mid-command: the background terminal does not survive.
	h.message(sid, "sleepcmd")
	h.approve(sid, 1, "allow", "user", "")
	term := readPID(t, filepath.Join(h.home, ".fakecodex", "terminal.pid"))
	h.command(sid, "interrupt")
	h.waitFinishes(sid, 3)
	waitFor(t, 10*time.Second, "terminal killed", func() bool { return gone(term) })
	h.message(sid, "echo alive")
	h.waitFinishes(sid, 4)

	// No completion within the interrupt timeout: stop, failed, resumable.
	setDuration(t, &interruptTimeout, 500*time.Millisecond)
	seen := len(h.relay.text(sid))
	h.message(sid, "stuck")
	waitFor(t, 15*time.Second, "stuck output", func() bool { return strings.Contains(h.relay.text(sid)[seen:], "2\n") })
	h.command(sid, "interrupt")
	h.relay.waitStatus(t, sid, "failed", 1)
	waitFor(t, 15*time.Second, "codex killed", func() bool { return gone(pid) })
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "Codex did not stop after the interrupt") {
		t.Fatalf("errors %v", errs)
	}
	h.message(sid, "echo resumed")
	waitFor(t, 20*time.Second, "resumed turn", func() bool { return strings.Contains(h.relay.text(sid), "resumed") })
	if _, err := os.Stat(filepath.Join(h.home, ".fakecodex", "thread-resume.json")); err != nil {
		t.Fatal("the next message did not resume the thread")
	}
}

func TestCodexResumeAfterRestart(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("remember kiwi-7817")
	h.waitFinishes(sid, 1)
	thread := h.record(sid).CodexThreadID
	sessionHome := filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)
	pid := codexRuns(t, h.home)[0].PID
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	h.relay.waitStatus(t, sid, "disconnected", 1)
	if !gone(pid) {
		t.Fatal("codex survived Ctrl-C")
	}
	if _, err := os.Stat(sessionHome); err != nil {
		t.Fatalf("Ctrl-C removed the session home: %v", err)
	}
	if live := h.api.liveTokens(); len(live) != 0 {
		t.Fatalf("tokens after Ctrl-C %v", live)
	}

	// A restarted daemon resumes the same thread with the same home.
	h.start()
	h.message(sid, "recall")
	waitFor(t, 20*time.Second, "recall", func() bool { return strings.Contains(h.relay.text(sid), "kiwi-7817") })
	var params map[string]any
	b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "thread-resume.json"))
	_ = json.Unmarshal(b, &params)
	if params["threadId"] != thread || params["approvalPolicy"] != "untrusted" {
		t.Fatalf("thread/resume %v", params)
	}
	runs := codexRuns(t, h.home)
	if len(runs) != 2 || runs[0].CodexHome != runs[1].CodexHome {
		t.Fatalf("runs %+v", runs)
	}
	if !slices.Contains(h.relay.statuses(sid), "running") {
		t.Fatal("no running status after resume")
	}
}

func TestCodexResumeWithoutHomeFails(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(h.home, ".archivist", "connect", "codex-home", sid))
	h.start()
	h.message(sid, "echo two")
	waitFor(t, 20*time.Second, "failed", func() bool {
		st := h.relay.statuses(sid)
		return len(st) > 0 && st[len(st)-1] == "failed"
	})
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "Codex home of this session is gone") {
		t.Fatalf("errors %v", errs)
	}
	if h.record(sid).Status != "failed" || len(codexRuns(t, h.home)) != 1 {
		t.Fatal("a session without its home must fail without starting Codex")
	}
}

func TestCodexProofFailuresFailClosed(t *testing.T) {
	cases := map[string]map[string]any{
		"api key account":     {"accountType": "apiKey"},
		"other codex home":    {"codexHome": "/tmp/not-the-session-home"},
		"extra writable root": {"extraRoot": "/home/owner/project"},
		"instruction sources": {"instructionSources": []string{"/home/owner/.codex/AGENTS.md"}},
		"other provider":      {"modelProvider": "custom"},
		"other mcp server":    {"extraMCP": true},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			h := newCodexHarness(t, cfg)
			h.start()
			sid := h.startCodexSession("echo must not run")
			h.relay.waitStatus(t, sid, "failed", 1)
			types := h.payloadTypes(sid)
			if strings.Join(types, " ") != "data-session-status error data-session-status" || h.relay.statuses(sid)[0] != "starting" {
				t.Fatalf("events %v statuses %v", types, h.relay.statuses(sid))
			}
			runs := codexRuns(t, h.home)
			if len(runs) != 1 {
				t.Fatalf("runs %d", len(runs))
			}
			waitFor(t, 10*time.Second, "codex killed", func() bool { return gone(runs[0].PID) })
			waitFor(t, 10*time.Second, "token revoked", func() bool { return len(h.api.liveTokens()) == 0 })
			if h.record(sid).Status != "failed" {
				t.Fatal("record not failed")
			}
			if _, err := os.Stat(filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)); !os.IsNotExist(err) {
				t.Fatal("home kept after a failed proof")
			}
			h.message(sid, "echo again")
			h.relay.waitStatus(t, sid, "failed", 2)
			if len(codexRuns(t, h.home)) != 1 {
				t.Fatal("a failed session started Codex again")
			}
		})
	}

	// A later account/updated away from ChatGPT ends the session.
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("account")
	h.relay.waitStatus(t, sid, "failed", 1)
	pid := codexRuns(t, h.home)[0].PID
	waitFor(t, 10*time.Second, "codex killed", func() bool { return gone(pid) })
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], `"apikey"`) {
		t.Fatalf("errors %v", errs)
	}
	if len(h.relay.eventsOf(sid, "finish")) != 0 {
		t.Fatal("the turn finished after the account switch")
	}
}

func TestCodexReapsDetachedAndOrphanedProcesses(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("grandchild")
	h.waitFinishes(sid, 1)
	grandchild := readPID(t, filepath.Join(h.home, ".fakecodex", "grandchild.pid"))
	h.message(sid, "orphan")
	h.waitFinishes(sid, 2)
	orphan := readPID(t, filepath.Join(h.home, ".fakecodex", "orphan.pid"))
	if gone(grandchild) || gone(orphan) {
		t.Fatal("test processes did not start")
	}
	h.command(sid, "stop_session")
	h.relay.waitStatus(t, sid, "completed", 1)
	waitFor(t, 15*time.Second, "setsid grandchild killed", func() bool { return gone(grandchild) })
	waitFor(t, 15*time.Second, "orphan killed", func() bool { return gone(orphan) })

	// Crash mid-turn: the exit path cleans up, the session stays resumable.
	sid2 := h.startCodexSession("grandchild")
	h.waitFinishes(sid2, 1)
	grandchild2 := readPID(t, filepath.Join(h.home, ".fakecodex", "grandchild.pid"))
	h.message(sid2, "exit")
	waitFor(t, 15*time.Second, "failed after crash", func() bool { return slices.Contains(h.relay.statuses(sid2), "failed") })
	waitFor(t, 15*time.Second, "crash cleanup", func() bool { return gone(grandchild2) })
	if errs := h.errorTexts(sid2); len(errs) != 1 || !strings.Contains(errs[0], "Codex exited before the turn finished") {
		t.Fatalf("errors %v", errs)
	}

	// Ctrl-C: nothing of any session remains.
	sid3 := h.startCodexSession("grandchild")
	h.waitFinishes(sid3, 1)
	grandchild3 := readPID(t, filepath.Join(h.home, ".fakecodex", "grandchild.pid"))
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !gone(grandchild3) {
		t.Fatal("grandchild survived Ctrl-C")
	}
	if stray := h.strayProcesses(); len(stray) != 0 {
		t.Fatalf("processes left: %v", stray)
	}
}

func TestCodexStartRefusalsAndSweep(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	// A claude session started as codex is refused.
	claudeSess := h.api.addSession("claude")
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "c-x", "sessionId": claudeSess.SessionID, "agent": "codex", "prompt": "hi"})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked("c-x") })
	h.relay.waitStatus(t, claudeSess.SessionID, "failed", 1)
	if len(codexRuns(t, h.home)) != 0 {
		t.Fatal("codex started for a claude session")
	}

	// The startup sweep removes the home of an inactive record.
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	rec := h.record(sid)
	rec.Status = "failed"
	if err := h.store().Save(rec); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, 10*time.Second, "home swept", func() bool {
		_, err := os.Stat(home)
		return os.IsNotExist(err)
	})
}

// ─── review rows ────────────────────────────────────────────────────────────

func TestCodexCapabilityOnlyWhenDriven(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.codexOff = true
	h.start()
	caps := h.relay.lastCaps()
	if len(caps) != 2 || caps[1].Agent != "codex" || caps[1].Available || !caps[1].LoggedIn {
		t.Fatalf("an undriven Codex was offered: %+v", caps)
	}
}

func TestCodexStaleTurnStartReplyIgnored(t *testing.T) {
	c := &codexRun{turnSeq: 2, pending: map[string]*codexPending{}}
	s := &session{cx: c}
	s.codexCallDone(codexEvent{Call: "turn/start:1", Result: json.RawMessage(`{"turn":{"id":"old-turn"}}`)})
	if c.turnID != "" {
		t.Fatalf("a stale turn/start reply set turn %q", c.turnID)
	}
}

func TestJSONRPCFrameFilterRejectsWhatClosesTheConn(t *testing.T) {
	for _, line := range []string{
		`{"id":null,"method":"item/commandExecution/requestApproval","params":{}}`,
		`{"id":9223372036854775808,"method":"x"}`,
		`{"id":1,"error":"boom"}`,
	} {
		if jsonrpcFrame([]byte(line)) {
			t.Errorf("%s accepted", line)
		}
	}
	if !jsonrpcFrame([]byte(`{"id":9223372036854775807,"method":"x"}`)) {
		t.Error("max int64 id refused")
	}
}

func TestOversizedCodexCardsAndPatchesFit(t *testing.T) {
	o := newTestOutbox(t)
	big := strings.Repeat("echo x; ", 20000)
	card := Chunk{"type": "tool-approval-request", "approvalId": "t:approval:1", "toolCallId": "exec-1",
		"approvalDescriptor": map[string]any{"kind": "command", "command": big, "cwd": "/w",
			"availableDecisions": []any{"accept", map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []any{big}}}, "cancel"},
			"commandActions":     []any{map[string]any{"command": big}}}}
	if o.Emit(card) != 1 {
		t.Fatal("an oversized command card was dropped")
	}
	patch := Chunk{"type": "data-patch", "data": map[string]any{"toolCallId": "p1", "path": "/w/a", "diff": strings.Repeat("+x\n", 40000), "operation": "add"}}
	if o.Emit(patch) != 1 {
		t.Fatal("an oversized patch was dropped")
	}
	raw := o.After(0)
	if len(raw) != 2 || !strings.Contains(string(raw[1].raw), "truncated by archivist connect") {
		t.Fatalf("outbox %d", len(raw))
	}
}

func TestCodexMoreServerRequestRows(t *testing.T) {
	h := newCodexHarness(t, map[string]any{"mcpApproval": true})
	h.start()
	// kind:"writeStdin" takes the command approval path.
	sid := h.startCodexSession("stdin")
	req := h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 1)
	if req["approvalDescriptor"].(map[string]any)["kind"] != "writeStdin" || !strings.Contains(h.relay.text(sid), "stdin: accept") {
		t.Fatalf("writeStdin row: %v %q", req, h.relay.text(sid))
	}
	// MCP allow for session: accept with a session-only persist, never "always".
	h.message(sid, "mcp")
	h.approve(sid, 2, "allow", "user", "allow_always")
	h.waitFinishes(sid, 2)
	b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "elicitation-reply.json"))
	if !strings.Contains(string(b), `"action":"accept"`) || !strings.Contains(string(b), `"persist":"session"`) || strings.Contains(string(b), "always") {
		t.Fatalf("mcp allow for session reply %s", b)
	}
	// MCP deny for session: cancel.
	h.message(sid, "mcp")
	h.approve(sid, 3, "deny", "user", "reject_always")
	h.waitFinishes(sid, 3)
	b, _ = os.ReadFile(filepath.Join(h.home, ".fakecodex", "elicitation-reply.json"))
	if !strings.Contains(string(b), `"action":"cancel"`) {
		t.Fatalf("mcp deny for session reply %s", b)
	}
	reasons := []string{}
	for _, r := range h.relay.eventsOf(sid, "tool-approval-response") {
		reasons = append(reasons, r["payload"].(map[string]any)["reason"].(string))
	}
	if strings.Join(reasons, ",") != "accept,acceptForSession,cancel" {
		t.Fatalf("decisions %v", reasons)
	}
}

func TestCodexInterruptBeforeTurnStartReply(t *testing.T) {
	h := newCodexHarness(t, map[string]any{"turnStartDelayMs": 700})
	h.start()
	sid := h.startCodexSession("echo first")
	h.waitFinishes(sid, 1)
	h.message(sid, "slow")
	time.Sleep(150 * time.Millisecond) // turn/start sent, its reply still pending
	h.command(sid, "interrupt")
	h.waitFinishes(sid, 2)
	if len(h.relay.eventsOf(sid, "abort")) != 1 {
		t.Fatalf("deferred interrupt did not end the turn: %v", h.payloadTypes(sid))
	}
	h.message(sid, "echo next")
	h.waitFinishes(sid, 3)
	if len(h.relay.eventsOf(sid, "abort")) != 1 || !strings.Contains(h.relay.text(sid), "next") {
		t.Fatal("the interrupt carried over to the next turn")
	}
}

func TestCodexHandshakeProofRows(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"account switch during handshake": {"handshakeAccount": "apikey"},
		"archivist MCP failed":            {"mcpFail": true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCodexHarness(t, cfg)
			h.start()
			sid := h.startCodexSession("echo must not run")
			h.relay.waitStatus(t, sid, "failed", 1)
			if types := strings.Join(h.payloadTypes(sid), " "); types != "data-session-status error data-session-status" {
				t.Fatalf("events %s", types)
			}
		})
	}
}

func TestCodexResumeEdgeRows(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	home := filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}

	// Owner logged out: the resume fails but the session stays resumable.
	ownerAuth := filepath.Join(h.codexOwner(), "auth.json")
	saved, _ := os.ReadFile(ownerAuth)
	_ = os.Remove(ownerAuth)
	h.start()
	h.message(sid, "echo two")
	h.relay.waitStatus(t, sid, "failed", 1)
	if h.record(sid).Status != "active" {
		t.Fatal("a logged-out owner ended a resumable session")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal("home removed after a transient failure")
	}
	_ = os.WriteFile(ownerAuth, saved, 0o600)
	h.message(sid, "echo three")
	waitFor(t, 20*time.Second, "resumed", func() bool { return strings.Contains(h.relay.text(sid), "three") })
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}

	// The auth link replaced by a regular file: fail closed.
	link := filepath.Join(home, "auth.json")
	_ = os.Remove(link)
	_ = os.WriteFile(link, []byte(`{"copy":true}`), 0o600)
	h.start()
	h.message(sid, "echo four")
	h.relay.waitStatus(t, sid, "failed", 2)
	if errs := h.errorTexts(sid); !strings.Contains(errs[len(errs)-1], "not a link to the owner's login") {
		t.Fatalf("errors %v", errs)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}

	// Missing rollout with the home present: start a new session.
	h2 := newCodexHarness(t, nil)
	h2.start()
	sid2 := h2.startCodexSession("echo one")
	h2.waitFinishes(sid2, 1)
	thread2 := h2.record(sid2).CodexThreadID
	if err := h2.stop(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(h2.home, ".archivist", "connect", "codex-home", sid2, "sessions", thread2+".json"))
	h2.start()
	h2.message(sid2, "echo two")
	waitFor(t, 20*time.Second, "failed", func() bool { return h2.record(sid2).Status == "failed" })
	if errs := h2.errorTexts(sid2); len(errs) != 1 || !strings.Contains(errs[0], "could not resume the thread") {
		t.Fatalf("errors %v", errs)
	}
}

func TestCodexOrphanAttributionWithTwoSessions(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	a := h.startCodexSession("grandchild")
	h.waitFinishes(a, 1)
	grandA := readPID(t, filepath.Join(h.home, ".fakecodex", "grandchild.pid"))
	_ = os.Remove(filepath.Join(h.home, ".fakecodex", "grandchild.pid"))
	b := h.startCodexSession("grandchild")
	h.waitFinishes(b, 1)
	grandB := readPID(t, filepath.Join(h.home, ".fakecodex", "grandchild.pid"))
	// Stopping A kills A's detached child, never B's.
	h.command(a, "stop_session")
	h.relay.waitStatus(t, a, "completed", 1)
	waitFor(t, 15*time.Second, "A's grandchild killed", func() bool { return gone(grandA) })
	time.Sleep(300 * time.Millisecond)
	if gone(grandB) {
		t.Fatal("stopping session A killed session B's process")
	}
	h.message(b, "echo still running")
	h.waitFinishes(b, 2)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !gone(grandB) {
		t.Fatal("B's grandchild survived Ctrl-C")
	}
}

// ─── coordinator review rows ────────────────────────────────────────────────

func TestThreadProblemFailsClosed(t *testing.T) {
	yes, no := true, false
	good := func() codexThreadResult {
		var th codexThreadResult
		th.Thread.ID = "th-1"
		th.ModelProvider = "openai"
		th.Cwd = "/w/session"
		th.ApprovalPolicy = json.RawMessage(`"untrusted"`)
		th.ApprovalsReviewer = "user"
		th.Sandbox.Type = "workspaceWrite"
		th.Sandbox.NetworkAccess, th.Sandbox.ExcludeSlashTmp, th.Sandbox.ExcludeTmpdirEnvVar = &no, &yes, &yes
		return th
	}
	if p := threadProblem(good(), "/w/session", ""); p != "" {
		t.Fatalf("compliant thread failed: %s", p)
	}
	if p := threadProblem(good(), "/w/session", "th-1"); p != "" {
		t.Fatalf("compliant resume failed: %s", p)
	}
	for name, c := range map[string]struct {
		mutate  func(*codexThreadResult)
		resumed string
		want    string
	}{
		"no thread id":        {func(th *codexThreadResult) { th.Thread.ID = "" }, "", "no thread id"},
		"other resumed id":    {func(*codexThreadResult) {}, "th-other", "resumed thread"},
		"provider":            {func(th *codexThreadResult) { th.ModelProvider = "custom" }, "", "model provider"},
		"policy":              {func(th *codexThreadResult) { th.ApprovalPolicy = json.RawMessage(`"never"`) }, "", "approval policy"},
		"granular policy":     {func(th *codexThreadResult) { th.ApprovalPolicy = json.RawMessage(`{"granular":{}}`) }, "", "approval policy"},
		"missing policy":      {func(th *codexThreadResult) { th.ApprovalPolicy = nil }, "", "approval policy"},
		"reviewer":            {func(th *codexThreadResult) { th.ApprovalsReviewer = "auto_review" }, "", "approvals reviewer"},
		"sandbox type":        {func(th *codexThreadResult) { th.Sandbox.Type = "dangerFullAccess" }, "", "sandbox \"dangerFullAccess\""},
		"network on":          {func(th *codexThreadResult) { th.Sandbox.NetworkAccess = &yes }, "", "networkAccess is true"},
		"network missing":     {func(th *codexThreadResult) { th.Sandbox.NetworkAccess = nil }, "", "networkAccess not reported"},
		"slash tmp writable":  {func(th *codexThreadResult) { th.Sandbox.ExcludeSlashTmp = &no }, "", "excludeSlashTmp is false"},
		"slash tmp missing":   {func(th *codexThreadResult) { th.Sandbox.ExcludeSlashTmp = nil }, "", "excludeSlashTmp not reported"},
		"tmpdir writable":     {func(th *codexThreadResult) { th.Sandbox.ExcludeTmpdirEnvVar = &no }, "", "excludeTmpdirEnvVar is false"},
		"tmpdir missing":      {func(th *codexThreadResult) { th.Sandbox.ExcludeTmpdirEnvVar = nil }, "", "excludeTmpdirEnvVar not reported"},
		"extra root":          {func(th *codexThreadResult) { th.Sandbox.WritableRoots = []string{"/home/owner"} }, "", "extra writable root"},
		"cwd differs":         {func(th *codexThreadResult) { th.Cwd = "/elsewhere" }, "", "thread cwd"},
		"cwd missing":         {func(th *codexThreadResult) { th.Cwd = "" }, "", "thread cwd"},
		"instruction sources": {func(th *codexThreadResult) { th.InstructionSources = []string{"/h/AGENTS.md"} }, "", "instruction sources"},
	} {
		th := good()
		c.mutate(&th)
		if p := threadProblem(th, "/w/session", c.resumed); !strings.Contains(p, c.want) {
			t.Errorf("%s: problem %q, want %q", name, p, c.want)
		}
	}
	// The cwd itself as a writable root is fine.
	th := good()
	th.Sandbox.WritableRoots = []string{"/w/session"}
	if p := threadProblem(th, "/w/session", ""); p != "" {
		t.Errorf("cwd root: %s", p)
	}
}

func TestCodexLateOtherMCPServerEndsSession(t *testing.T) {
	h := newCodexHarness(t, map[string]any{"lateExtraMCP": true})
	h.start()
	sid := h.startCodexSession("echo hi")
	h.relay.waitStatus(t, sid, "failed", 1)
	pid := codexRuns(t, h.home)[0].PID
	waitFor(t, 10*time.Second, "codex killed", func() bool { return gone(pid) })
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], `other than archivist ("codex_apps")`) {
		t.Fatalf("errors %v", errs)
	}
	if !slices.Contains(h.relay.statuses(sid), "running") || h.record(sid).Status != "failed" {
		t.Fatalf("statuses %v", h.relay.statuses(sid))
	}
}

func TestCodexResolvedApprovalIsNotAnsweredLater(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("cmd touch never.txt")
	var req map[string]any
	waitFor(t, 15*time.Second, "approval request", func() bool {
		reqs := h.relay.eventsOf(sid, "tool-approval-request")
		if len(reqs) == 1 {
			req = reqs[0]
		}
		return req != nil
	})
	// Interrupted while the card is open: Codex resolves the request itself.
	h.command(sid, "interrupt")
	h.waitFinishes(sid, 1)
	time.Sleep(200 * time.Millisecond)
	cid := "resolved:" + req["correlationId"].(string)
	h.relay.send(sid, map[string]any{"kind": "approval_response", "correlationId": cid, "sessionId": sid,
		"approvalId": req["payload"].(map[string]any)["approvalId"], "decision": "allow", "reason": "user"})
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked(cid) })
	h.message(sid, "echo after")
	h.waitFinishes(sid, 2)
	if n := len(h.relay.eventsOf(sid, "tool-approval-response")); n != 0 {
		t.Fatalf("a resolved approval produced %d responses", n)
	}
	if b, err := os.ReadFile(filepath.Join(h.home, ".fakecodex", "late-replies.jsonl")); err == nil {
		t.Fatalf("codex got a reply to a resolved request: %s", b)
	}
}

func TestCodexResumeErrorKinds(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	home := filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	setFake := func(cfg map[string]any) {
		b, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(h.home, ".fakecodex", "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A passing internal error keeps the session resumable.
	setFake(map[string]any{"resumeError": "internal error: database is locked"})
	h.start()
	h.message(sid, "echo two")
	h.relay.waitStatus(t, sid, "failed", 1)
	if h.record(sid).Status != "active" {
		t.Fatal("a transient resume error ended the session")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal("home removed after a transient resume error")
	}
	// The thread is gone: the session fails for good.
	setFake(map[string]any{"resumeError": "no rollout found for thread id x"})
	h.message(sid, "echo three")
	waitFor(t, 20*time.Second, "failed record", func() bool { return h.record(sid).Status == "failed" })
	// failSession saves the record before it removes the home.
	waitFor(t, 5*time.Second, "home removed after the thread was gone", func() bool {
		_, err := os.Stat(home)
		return os.IsNotExist(err)
	})
}

func TestResumeForUndrivenHarnessStaysResumable(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	// Restart without a usable Codex: the reattached session cannot run.
	h.codexOff = true
	h.start()
	h.message(sid, "echo two")
	h.relay.waitStatus(t, sid, "failed", 1)
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "restart archivist connect") {
		t.Fatalf("errors %v", errs)
	}
	rec := h.record(sid)
	if rec.Status != "active" || len(codexRuns(t, h.home)) != 1 {
		t.Fatal("an undriven harness failed the session or started Codex")
	}
	if _, err := os.Stat(rec.Cwd); err != nil {
		t.Fatal("cwd removed")
	}
	if _, err := os.Stat(filepath.Join(h.home, ".archivist", "connect", "codex-home", sid)); err != nil {
		t.Fatal("home removed")
	}
}

// threadGone: only a missing thread or rollout ends the session; any other
// resume error (a missing model included) keeps it resumable.
func TestThreadGoneMatchesOnlyMissingThread(t *testing.T) {
	for msg, want := range map[string]bool{
		"no rollout found for thread id 019da1a1": true,
		"thread not found: 019da1a1":              true,
		"model not found: gpt-x":                  false,
		"internal error: database is locked":      false,
	} {
		if got := threadGone(msg); got != want {
			t.Errorf("threadGone(%q) = %v, want %v", msg, got, want)
		}
	}
}
