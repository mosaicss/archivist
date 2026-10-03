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
		det := DetectWith(context.Background(), DetectOptions{LookPath: look, Run: ExecRunner,
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
	if n := len(h.relay.eventsOf(sid, "data-usage")); n != 3 {
		t.Fatalf("usage events %d, want one per turn", n)
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
