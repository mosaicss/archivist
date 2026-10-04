//go:build !windows

package connect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/taskscope"
)

// Session controls (Story 78.32): one test per Phase A matrix row, plus
// the units behind them.

// ─── units ──────────────────────────────────────────────────────────────────

func TestModeOrderParseAndClamp(t *testing.T) {
	if strings.Join([]string{string(Modes[0]), string(Modes[1]), string(Modes[2]), string(Modes[3])}, ",") !=
		"read_only,ask,auto_edits,full_auto" {
		t.Fatalf("mode order %v", Modes)
	}
	for _, bad := range []string{"", "root", "ASK", "full auto", "bypassPermissions", "plan", "auto", "dontAsk"} {
		if _, ok := ParseMode(bad); ok {
			t.Errorf("%q parsed as a mode", bad)
		}
	}
	for _, c := range []struct{ m, ceiling, want Mode }{
		{ModeFullAuto, ModeAutoEdits, ModeAutoEdits}, {ModeAsk, ModeAutoEdits, ModeAsk},
		{ModeReadOnly, ModeFullAuto, ModeReadOnly}, {ModeFullAuto, ModeFullAuto, ModeFullAuto},
		{ModeAutoEdits, ModeReadOnly, ModeReadOnly},
		// Unknown values never mean a higher mode.
		{"root", ModeFullAuto, ModeReadOnly}, {ModeFullAuto, "", ModeReadOnly}, {"", ModeFullAuto, ModeReadOnly},
	} {
		if got := ClampMode(c.m, c.ceiling); got != c.want {
			t.Errorf("ClampMode(%q, %q) = %q, want %q", c.m, c.ceiling, got, c.want)
		}
	}
	if got := fmt.Sprint(ModesUpTo(ModeAutoEdits)); got != "[read_only ask auto_edits]" {
		t.Errorf("ModesUpTo(auto_edits) = %s", got)
	}
	if got := fmt.Sprint(ModesUpTo("bogus")); got != "[read_only]" {
		t.Errorf("ModesUpTo(bogus) = %s", got)
	}
	if ModeHelp() != "read_only (Read only), ask (Ask every time), auto_edits (Auto edits), full_auto (Full auto)" {
		t.Errorf("help %q", ModeHelp())
	}
	for _, m := range Modes {
		if strings.ContainsAny(m.Label(), "-—") {
			t.Errorf("label %q has a hyphen or em dash", m.Label())
		}
	}
	if DefaultStartMode(false) != ModeAsk || DefaultStartMode(true) != ModeFullAuto {
		t.Error("default start modes")
	}
	if DefaultMaxMode != ModeAutoEdits || SandboxMaxMode != ModeFullAuto {
		t.Error("default ceilings")
	}
}

func TestHarnessMappingTable(t *testing.T) {
	for _, c := range []struct {
		m                                   Mode
		claude, policy, sandbox, sandboxTyp string
	}{
		{ModeReadOnly, "plan", "never", "read-only", "readOnly"},
		{ModeAsk, "default", "untrusted", "workspace-write", "workspaceWrite"},
		{ModeAutoEdits, "acceptEdits", "untrusted", "workspace-write", "workspaceWrite"},
		{ModeFullAuto, "bypassPermissions", "never", "danger-full-access", "dangerFullAccess"},
	} {
		if claudePermissionMode(c.m) != c.claude || codexApprovalPolicy(c.m) != c.policy ||
			codexSandboxMode(c.m) != c.sandbox || codexSandboxType(c.m) != c.sandboxTyp || codexSandboxPolicy(c.m)["type"] != c.sandboxTyp {
			t.Errorf("%s: %s %s %s %v", c.m, claudePermissionMode(c.m), codexApprovalPolicy(c.m), codexSandboxMode(c.m), codexSandboxPolicy(c.m))
		}
	}
	ww := codexSandboxPolicy(ModeAsk)
	if ww["networkAccess"] != false || ww["excludeSlashTmp"] != true || ww["excludeTmpdirEnvVar"] != true || len(ww["writableRoots"].([]string)) != 0 {
		t.Errorf("workspaceWrite isolation %v", ww)
	}
	if ro := codexSandboxPolicy(ModeReadOnly); ro["networkAccess"] != false {
		t.Errorf("readOnly %v", ro)
	}
	// Never the auto or dontAsk modes, whatever the input.
	for _, m := range []Mode{ModeReadOnly, ModeAsk, ModeAutoEdits, ModeFullAuto, "bogus"} {
		if p := claudePermissionMode(m); p == "auto" || p == "dontAsk" {
			t.Errorf("%s maps to %s", m, p)
		}
	}
	if claudePermissionMode("bogus") != "plan" || codexApprovalPolicy("bogus") != "never" || codexSandboxMode("bogus") != "read-only" {
		t.Error("an unknown mode must map to the read only harness settings")
	}
}

func TestBackstopTable(t *testing.T) {
	want := map[Mode][3]verdict{ // read tool, publish_artifact, any other tool
		ModeReadOnly:  {verdictAllow, verdictDeny, verdictDeny},
		ModeAsk:       {verdictAllow, verdictCard, verdictCard},
		ModeAutoEdits: {verdictAllow, verdictCard, verdictCard},
		// full_auto: anything the harness still asks is a card, never allowed silently.
		ModeFullAuto: {verdictAllow, verdictCard, verdictCard},
	}
	for m, w := range want {
		got := [3]verdict{backstop(m, claudeMosaicRead("mcp__archivist__search")),
			backstop(m, claudeMosaicRead("mcp__archivist__publish_artifact")), backstop(m, claudeMosaicRead("Bash"))}
		if got != w {
			t.Errorf("%s: %v, want %v", m, got, w)
		}
	}
	for name, read := range map[string]bool{
		"mcp__archivist__search": true, "mcp__archivist__companies_search": true, "mcp__archivist__companies_get": true,
		"mcp__archivist__read_passage": true, "mcp__archivist__read_section": true, "mcp__archivist__toc": true,
		"mcp__archivist__publish_artifact": false, "mcp__archivist__usage": false, "mcp__other__search": false,
		"search": false, "Read": false, "mcp__archivist__": false,
	} {
		if claudeMosaicRead(name) != read {
			t.Errorf("claudeMosaicRead(%q) != %v", name, read)
		}
	}
	if taskscope.IsReadTool(taskscope.PublishTool) {
		t.Error("publish_artifact is not a read tool")
	}
	if strings.ContainsAny(readOnlyDenied, "-—") {
		t.Errorf("denial %q has a hyphen or em dash", readOnlyDenied)
	}
}

func TestDecodeSessionControls(t *testing.T) {
	sid := randomUUID()
	start := func(extra string) string {
		return `{"kind":"start_session","correlationId":"c1","sessionId":"` + sid + `","agent":"claude","prompt":"hi"` + extra + `}`
	}
	in, err := Decode(UserSocket, []byte(start(`,"mode":"read_only","model":"claude-opus-5-5[1m]","effort":"xhigh"`)))
	if err != nil || in.Mode != "read_only" || in.Model != "claude-opus-5-5[1m]" || in.Effort != "xhigh" {
		t.Fatalf("start with controls: %+v %v", in, err)
	}
	if in, err := Decode(UserSocket, []byte(start(""))); err != nil || in.Mode != "" || in.Model != "" || in.Effort != "" {
		t.Fatalf("old consumer start: %+v %v", in, err)
	}
	for _, extra := range []string{`,"mode":"root"`, `,"mode":""`, `,"mode":null`, `,"mode":3`, `,"model":""`,
		`,"model":"opus; rm -rf"`, `,"model":"` + strings.Repeat("a", 129) + `"`, `,"effort":"HIGH"`, `,"effort":""`,
		`,"effort":"high low"`, `,"permissionMode":"bypassPermissions"`, `,"maxMode":"full_auto"`} {
		var de *DecodeError
		if _, err := Decode(UserSocket, []byte(start(extra))); !errors.As(err, &de) || de.CorrelationID != "c1" {
			t.Errorf("%s accepted (%v)", extra, err)
		}
	}
	setMode := `{"kind":"set_mode","correlationId":"m1","sessionId":"` + sid + `","mode":"full_auto"}`
	if in, err := Decode(SessionSocket, []byte(setMode)); err != nil || in.Kind != "set_mode" || in.Mode != "full_auto" {
		t.Fatalf("set_mode: %+v %v", in, err)
	}
	for _, raw := range []string{
		`{"kind":"set_mode","correlationId":"m1","sessionId":"` + sid + `"}`,
		`{"kind":"set_mode","correlationId":"m1","sessionId":"` + sid + `","mode":"root"}`,
		`{"kind":"set_mode","correlationId":"m1","sessionId":"` + sid + `","mode":"ask","model":"opus"}`,
		`{"kind":"set_mode","correlationId":"m1","sessionId":"not-a-uuid","mode":"ask"}`,
	} {
		if _, err := Decode(SessionSocket, []byte(raw)); err == nil {
			t.Errorf("session socket accepted %s", raw)
		}
	}
	if _, err := Decode(UserSocket, []byte(setMode)); err == nil {
		t.Error("set_mode accepted on the user socket")
	}
}

func TestControlsFrameShape(t *testing.T) {
	raw := controlsFrame(ModeAutoEdits, ModeAsk, []agentControls{{Agent: "claude", Models: claudeModels}})
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f["kind"] != "controls" || f["maxMode"] != "auto_edits" || f["defaultMode"] != "ask" ||
		fmt.Sprint(f["modes"]) != "[read_only ask auto_edits]" {
		t.Fatalf("controls %s", raw)
	}
	models := f["agents"].([]any)[0].(map[string]any)["models"].([]any)
	if len(models) != 5 || models[3].(map[string]any)["id"] != "haiku" || len(models[3].(map[string]any)["efforts"].([]any)) != 0 ||
		models[4].(map[string]any)["id"] != "fable" || len(models[4].(map[string]any)["efforts"].([]any)) != 5 {
		t.Fatalf("claude models %v", models)
	}
	if !strings.Contains(string(controlsFrame(ModeReadOnly, ModeReadOnly, nil)), `"agents":[]`) {
		t.Error("agents must be an array")
	}
}

// The controls frame fits the relay's 6144 byte limit (Phase B): Codex
// entries are dropped from the end, the default kept, Claude untouched.
func TestControlsFrameTrimmedToTheRelayLimit(t *testing.T) {
	efforts := make([]string, 16)
	for i := range efforts {
		efforts[i] = fmt.Sprintf("e%02d", i) + strings.Repeat("z", 29)
	}
	big := func(i int, def bool) modelEntry {
		// <, > and & are escaped by Go (6 bytes each), never by the relay.
		return modelEntry{ID: fmt.Sprintf("m%02d-", i) + strings.Repeat("x", 124), Label: strings.Repeat("<&>", 21),
			Efforts: efforts, DefaultEffort: efforts[0], Default: def}
	}
	var codex []modelEntry
	for i := range 64 {
		codex = append(codex, big(i, i == 40))
	}
	claude := append([]modelEntry(nil), claudeModels...)
	agents := []agentControls{{Agent: "claude", Models: claude}, {Agent: "codex", Models: codex}}
	if n := len(controlsFrame(ModeFullAuto, ModeFullAuto, agents)); n <= 4*maxControlsBytes {
		t.Fatalf("fixture too small: %d bytes", n)
	}
	frame, dropped := trimControls(ModeFullAuto, ModeFullAuto, agents)
	if len(frame) > maxControlsBytes || dropped == 0 {
		t.Fatalf("frame %d bytes, %d dropped", len(frame), dropped)
	}
	var f struct {
		Agents []agentControls `json:"agents"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 2 || len(f.Agents[0].Models) != len(claudeModels) {
		t.Fatalf("claude models changed: %+v", f.Agents)
	}
	kept := f.Agents[1].Models
	if len(kept)+dropped != 64 || !kept[len(kept)-1].Default || kept[len(kept)-1].ID != codex[40].ID {
		t.Fatalf("kept %d (last %s default %v), dropped %d", len(kept), kept[len(kept)-1].ID, kept[len(kept)-1].Default, dropped)
	}
	for i, m := range kept[:len(kept)-1] {
		if m.ID != codex[i].ID {
			t.Fatalf("entry %d is %s, want the catalogue order", i, m.ID)
		}
	}
	// One more entry would not fit: the trim stops as soon as the frame fits.
	withOne := append(append([]modelEntry(nil), kept[:len(kept)-1]...), codex[len(kept)-1], codex[40])
	if n := len(controlsFrame(ModeFullAuto, ModeFullAuto, []agentControls{agents[0], {Agent: "codex", Models: withOne}})); n <= maxControlsBytes {
		t.Fatalf("one more entry still fits (%d bytes)", n)
	}
	if len(agents[1].Models) != 64 {
		t.Fatal("the caller's catalogue was modified")
	}
	// A frame that already fits is unchanged.
	small := []agentControls{{Agent: "claude", Models: claude}, {Agent: "codex", Models: codex[:3]}}
	if got, n := trimControls(ModeAsk, ModeAsk, small); n != 0 || string(got) != string(controlsFrame(ModeAsk, ModeAsk, small)) {
		t.Fatalf("small frame changed (%d dropped)", n)
	}
	// Only a default left that still does not fit: returned as is (never in practice).
	huge := []agentControls{{Agent: "codex", Models: []modelEntry{{ID: "d", Label: strings.Repeat("<", 1100), Efforts: []string{}, Default: true}}}}
	if got, n := trimControls(ModeAsk, ModeAsk, huge); n != 0 || len(got) <= maxControlsBytes {
		t.Fatalf("huge default: %d dropped, %d bytes", n, len(got))
	}
}

func TestCodexEntryAndLabels(t *testing.T) {
	eff := func(s ...string) []codexReasoningEffortOption {
		var out []codexReasoningEffortOption
		for _, e := range s {
			out = append(out, codexReasoningEffortOption{ReasoningEffort: e})
		}
		return out
	}
	low := "low"
	e, ok := codexEntry(codexModel{ID: "gpt-6.1-sol", DisplayName: "GPT\x1b-6.1 Sol\n", IsDefault: true, DefaultReasoningEffort: &low,
		SupportedReasoningEfforts: eff("low", "medium", "BAD EFFORT", "low")})
	if !ok || e.Label != "GPT-6.1 Sol" || strings.Join(e.Efforts, ",") != "low,medium" || e.DefaultEffort != "low" || !e.Default {
		t.Fatalf("entry %+v", e)
	}
	if _, ok := codexEntry(codexModel{ID: "x", Hidden: true}); ok {
		t.Error("hidden model listed")
	}
	if _, ok := codexEntry(codexModel{ID: "bad id"}); ok {
		t.Error("id outside the charset listed")
	}
	if e, _ := codexEntry(codexModel{ID: "m"}); e.Label != "m" || e.Efforts == nil {
		t.Errorf("fallback label/efforts %+v", e)
	}
}

func TestStartProblemChecksCatalogues(t *testing.T) {
	d := &Daemon{claude: ClaudeConfig{Bin: "/c"}, codex: CodexConfig{Bin: "/x"}, codexProbed: make(chan struct{}),
		codexCat: []modelEntry{{ID: "gpt-a", Efforts: []string{"low", "high"}, Default: true}, {ID: "gpt-b", Efforts: []string{"medium"}}}}
	d.probeOnce.Do(func() {}) // the probe "ran"
	close(d.codexProbed)
	ctx := context.Background()
	for _, c := range []struct {
		agent, model, effort, want string
	}{
		{"claude", "", "", ""},
		{"claude", "opus", "max", ""},
		{"claude", "default", "low", ""},
		{"claude", "", "xhigh", ""},
		{"claude", "gpt-a", "", "model"},
		{"claude", "claude-opus-5-5", "", "model"},
		{"claude", "haiku", "low", "effort"},
		{"claude", "sonnet", "ultra", "effort"},
		{"codex", "gpt-b", "medium", ""},
		{"codex", "gpt-a", "", ""},
		{"codex", "", "high", ""}, // the catalogue default's efforts
		{"codex", "gpt-z", "", "model"},
		{"codex", "gpt-b", "high", "effort"},
		{"codex", "", "medium", "effort"},
	} {
		got := d.startProblem(ctx, &Inbound{Agent: c.agent, Model: c.model, Effort: c.effort})
		if (c.want == "") != (got == "") || !strings.Contains(strings.ToLower(got), c.want) {
			t.Errorf("%s %q %q: %q, want a %q problem", c.agent, c.model, c.effort, got, c.want)
		}
	}
	// The machine flag model decides an effort without a session model.
	d.claude.Model = "haiku"
	if d.startProblem(ctx, &Inbound{Agent: "claude", Effort: "low"}) == "" {
		t.Error("an effort for the machine's haiku passed")
	}
	// The machine --codex-model decides an effort without a session model.
	d.codex.Model = "gpt-b"
	if p := d.startProblem(ctx, &Inbound{Agent: "codex", Effort: "medium"}); p != "" {
		t.Errorf("an effort the machine's gpt-b offers: %q", p)
	}
	if d.startProblem(ctx, &Inbound{Agent: "codex", Effort: "high"}) == "" {
		t.Error("an effort only the catalogue default offers passed for the machine's gpt-b")
	}
	d.codex.Model = ""
	// An empty catalogue refuses any explicit Codex model or effort.
	d.codexCat = nil
	for _, in := range []*Inbound{{Agent: "codex", Model: "gpt-a"}, {Agent: "codex", Effort: "low"}} {
		if d.startProblem(ctx, in) == "" {
			t.Errorf("%+v passed with an empty catalogue", in)
		}
	}
	if d.startProblem(ctx, &Inbound{Agent: "codex"}) != "" {
		t.Error("a start without a model needs no catalogue")
	}
}

func TestContextUsageExtraction(t *testing.T) {
	tr := NewTranslator()
	for _, line := range []string{
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":90000,"cache_creation_input_tokens":500,"output_tokens":40}}}`,
		// A subagent's message does not count.
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"id":"m2","model":"claude-haiku","usage":{"input_tokens":5,"output_tokens":5}}}`,
		`{"type":"result","is_error":false,"usage":{"input_tokens":1},"modelUsage":{"claude-haiku":{"contextWindow":200000},"claude-opus-5-5":{"contextWindow":1000000}}}`,
	} {
		tr.In([]byte(line))
	}
	if tr.Context.Tokens != 90550 || tr.Context.Window != 1000000 || tr.Context.Model != "claude-opus-5-5" {
		t.Fatalf("claude context %+v", tr.Context)
	}
	if !strings.Contains(tr.Context.String(), "90550 tokens of window 1000000") {
		t.Errorf("string %q", tr.Context.String())
	}
	// No window reported: unknown, never guessed.
	tr2 := NewTranslator()
	tr2.In([]byte(`{"type":"assistant","message":{"id":"m","model":"x","usage":{"input_tokens":7}}}`))
	tr2.In([]byte(`{"type":"result","modelUsage":{"x":{},"y":{}}}`))
	if tr2.Context.Tokens != 7 || tr2.Context.Window != 0 || !strings.Contains(tr2.Context.String(), "window unknown") {
		t.Fatalf("unknown window %+v", tr2.Context)
	}
	ct := NewCodexTranslator()
	ct.In("thread/tokenUsage/updated", "", json.RawMessage(`{"threadId":"t","turnId":"u","tokenUsage":{"total":{"totalTokens":900,"inputTokens":800,"outputTokens":100},"last":{"totalTokens":321,"inputTokens":300,"outputTokens":21},"modelContextWindow":272000}}`))
	if ct.Context.Tokens != 321 || ct.Context.Window != 272000 {
		t.Fatalf("codex context %+v", ct.Context)
	}
	if u, ok := codexContextUsage(json.RawMessage(`{"tokenUsage":{"total":{},"last":{"totalTokens":5},"modelContextWindow":null}}`)); !ok || u.Window != 0 || u.Tokens != 5 {
		t.Fatalf("null window %+v", u)
	}
	if _, ok := codexContextUsage(json.RawMessage(`{"tokenUsage":{"total":{}}}`)); ok {
		t.Error("usage without last accepted")
	}
	// Per turn reset (part 1b): a Claude result with no main loop assistant
	// message after an earlier turn reports no context tokens (the window
	// still comes from modelUsage); a Codex report without last after one
	// with it leaves the context empty.
	tr.In([]byte(`{"type":"result","is_error":false,"usage":{"input_tokens":1},"modelUsage":{"claude-opus-5-5":{"contextWindow":1000000}}}`))
	if tr.Context.Tokens != 0 {
		t.Fatalf("claude context after a turn without an assistant message %+v", tr.Context)
	}
	ct.In("thread/tokenUsage/updated", "", json.RawMessage(`{"threadId":"t","turnId":"v","tokenUsage":{"total":{"totalTokens":950,"inputTokens":840,"outputTokens":110},"modelContextWindow":272000}}`))
	if ct.Context.Tokens != 0 || ct.Context.Window != 0 {
		t.Fatalf("codex context after a report without last %+v", ct.Context)
	}
}

func TestThreadProblemPerMode(t *testing.T) {
	no, yes := false, true
	thread := func(policy string, sandbox string) codexThreadResult {
		var th codexThreadResult
		th.Thread.ID, th.ModelProvider, th.Cwd, th.ApprovalsReviewer = "th", "openai", "/w", "user"
		th.ApprovalPolicy = json.RawMessage(`"` + policy + `"`)
		th.Sandbox.Type = sandbox
		switch sandbox {
		case "workspaceWrite":
			th.Sandbox.NetworkAccess, th.Sandbox.ExcludeSlashTmp, th.Sandbox.ExcludeTmpdirEnvVar = &no, &yes, &yes
		case "readOnly":
			th.Sandbox.NetworkAccess = &no
		}
		return th
	}
	for _, m := range Modes {
		if p := threadProblem(thread(codexApprovalPolicy(m), codexSandboxType(m)), "/w", "", m); p != "" {
			t.Errorf("%s: mapped thread failed: %s", m, p)
		}
	}
	for name, c := range map[string]struct {
		th   codexThreadResult
		mode Mode
		want string
	}{
		"ask with never":           {thread("never", "workspaceWrite"), ModeAsk, "approval policy"},
		"auto_edits with full":     {thread("on-request", "dangerFullAccess"), ModeAutoEdits, "sandbox"},
		"read_only writable":       {thread("never", "workspaceWrite"), ModeReadOnly, "sandbox"},
		"full_auto from auto edit": {thread("on-request", "workspaceWrite"), ModeFullAuto, "approval policy"},
		"read_only network": {func() codexThreadResult {
			th := thread("never", "readOnly")
			th.Sandbox.NetworkAccess = &yes
			return th
		}(), ModeReadOnly, "networkAccess is true"},
		"read_only no network": {func() codexThreadResult { th := thread("never", "readOnly"); th.Sandbox.NetworkAccess = nil; return th }(), ModeReadOnly, "networkAccess not reported"},
	} {
		if p := threadProblem(c.th, "/w", "", c.mode); !strings.Contains(p, c.want) {
			t.Errorf("%s: %q, want %q", name, p, c.want)
		}
	}
}

// ─── daemon rows ────────────────────────────────────────────────────────────

// startWith starts a session with extra start_session fields (controls).
func (h *harness) startWith(agent, prompt string, extra map[string]any) string {
	h.t.Helper()
	s := h.api.addSession(agent)
	cid := "start-" + randomUUID()
	cmd := map[string]any{"kind": "start_session", "correlationId": cid, "sessionId": s.SessionID, "agent": agent, "prompt": prompt}
	for k, v := range extra {
		cmd[k] = v
	}
	h.relay.send("", cmd)
	waitFor(h.t, 15*time.Second, "start ack", func() bool { return h.relay.acked(cid) })
	return s.SessionID
}

// setMode sends set_mode and waits for its ack.
func (h *harness) setMode(sid string, mode Mode) {
	h.t.Helper()
	cid := "mode-" + randomUUID()
	h.relay.send(sid, map[string]any{"kind": "set_mode", "correlationId": cid, "sessionId": sid, "mode": string(mode)})
	waitFor(h.t, 10*time.Second, "set_mode ack", func() bool { return h.relay.acked(cid) })
}

// claudeArgs returns the joined argv of each fake Claude run.
func (h *harness) claudeRunArgs() []string {
	var out []string
	for _, r := range fakeRuns(h.t, h.home) {
		out = append(out, strings.Join(r.Args, " "))
	}
	return out
}

// jsonLines reads a JSON lines file under the fake's base directory.
func jsonLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) modeRequests() []string {
	var out []string
	for _, m := range jsonLines(h.t, filepath.Join(h.home, ".fakeclaude", "modes.jsonl")) {
		out = append(out, m["mode"].(string))
	}
	return out
}

func (h *harness) turnStarts() []map[string]any {
	return jsonLines(h.t, filepath.Join(h.home, ".fakecodex", "turn-start.jsonl"))
}

func (h *harness) threadStart() map[string]any {
	var params map[string]any
	b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "thread-start.json"))
	_ = json.Unmarshal(b, &params)
	return params
}

func (h *harness) cards(sid string) int { return len(h.relay.eventsOf(sid, "tool-approval-request")) }

// toolOutput reports a tool-output-available with this output.
func (h *harness) toolOutput(sid, output string) bool {
	for _, e := range h.relay.eventsOf(sid, "tool-output-available") {
		if e["payload"].(map[string]any)["output"] == output {
			return true
		}
	}
	return false
}

// Row "Start, no controls" and acceptance 1: an old consumer's start runs
// ask, Claude --permission-mode default without the allow flag, and a read
// tool the harness prompts for is allowed with no approval event. The
// controls frame follows the unchanged capabilities.
func TestControlsStartWithoutControls(t *testing.T) {
	h := newHarness(t, nil)
	h.debugLog = true
	h.start()
	if caps := h.relay.lastCaps(); len(caps) != 1 || caps[0] != (Capability{Agent: "claude", Version: "2.1.280", Available: true, LoggedIn: true}) {
		t.Fatalf("capabilities %+v", caps)
	}
	waitFor(t, 10*time.Second, "controls frame", func() bool { return h.relay.lastControls() != nil })
	c := h.relay.lastControls()
	if c["maxMode"] != "auto_edits" || c["defaultMode"] != "ask" || fmt.Sprint(c["modes"]) != "[read_only ask auto_edits]" {
		t.Fatalf("controls %v", c)
	}
	agents := c["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["agent"] != "claude" || len(agents[0].(map[string]any)["models"].([]any)) != 5 {
		t.Fatalf("controls agents %v", agents)
	}

	sid := h.startSession("askmcp search")
	h.waitFinishes(sid, 1)
	args := h.claudeRunArgs()
	if len(args) != 1 || !strings.Contains(args[0], "--permission-mode default") || strings.Contains(args[0], "dangerously") {
		t.Fatalf("argv %v", args)
	}
	if rec := h.record(sid); rec.Mode != "ask" || rec.Model != "" || rec.Effort != "" {
		t.Fatalf("record %+v", rec)
	}
	h.message(sid, "askmcp read_passage")
	h.waitFinishes(sid, 2)
	if n := h.cards(sid); n != 0 || len(h.relay.eventsOf(sid, "tool-approval-response")) != 0 {
		t.Fatalf("read tools produced %d approval events", n)
	}
	if strings.Count(h.relay.text(sid), "allowed") != 2 {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	// ask still shows cards for anything else.
	h.message(sid, "bash ls")
	h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 3)
	// Usage (row "Usage"): the harness context use reaches the debug log.
	waitFor(t, 5*time.Second, "context log", func() bool {
		return strings.Contains(h.log.String(), "context 1208 tokens of window 1000000 (model fake-model)")
	})
	if len(h.relay.invalid) != 0 {
		t.Fatalf("invalid events %v", h.relay.invalid)
	}
}

// Row "Start above ceiling" and acceptance 2 (Claude): full_auto under
// auto_edits runs acceptEdits without the allow flag, and set_mode
// full_auto is refused without touching the harness.
func TestControlsClaudeStartAboveCeiling(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startWith("claude", "echo hi", map[string]any{"mode": "full_auto"})
	h.waitFinishes(sid, 1)
	args := h.claudeRunArgs()
	if len(args) != 1 || !strings.Contains(args[0], "--permission-mode acceptEdits") || strings.Contains(args[0], "bypassPermissions") ||
		strings.Contains(args[0], "dangerously") {
		t.Fatalf("argv %v", args)
	}
	if h.record(sid).Mode != "auto_edits" {
		t.Fatalf("record mode %s", h.record(sid).Mode)
	}
	h.setMode(sid, ModeFullAuto)
	time.Sleep(200 * time.Millisecond)
	if reqs := h.modeRequests(); len(reqs) != 0 {
		t.Fatalf("set_mode above the ceiling reached Claude: %v", reqs)
	}
	if h.record(sid).Mode != "auto_edits" || !strings.Contains(h.log.String(), "refused: above this machine's ceiling") {
		t.Fatal("set_mode above the ceiling changed the mode or was not logged")
	}
	// auto_edits still shows cards.
	h.message(sid, "bash ls")
	h.approve(sid, 1, "deny", "user", "")
	h.waitFinishes(sid, 2)
}

// Row "Start, bad value": Decode refuses an unknown mode (acked, nothing
// runs); handleStart refuses a model or effort outside the catalogue with a
// failed status naming the field.
func TestControlsBadStartValues(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	s := h.api.addSession("claude")
	h.relay.sendRaw("", `{"kind":"start_session","correlationId":"bad-1","sessionId":"`+s.SessionID+`","agent":"claude","prompt":"hi","mode":"root"}`)
	waitFor(t, 5*time.Second, "refusal ack", func() bool { return h.relay.acked("bad-1") })
	if _, err := os.Stat(filepath.Join(h.home, ".archivist", "connect", "sessions", s.SessionID+".json")); !os.IsNotExist(err) {
		t.Fatal("a refused frame wrote a record")
	}
	for _, c := range []struct {
		extra map[string]any
		want  string
	}{
		{map[string]any{"model": "gpt-6.1-sol"}, `model "gpt-6.1-sol"`},
		{map[string]any{"model": "haiku", "effort": "high"}, `effort "high"`},
	} {
		sid := h.startWith("claude", "echo nope", c.extra)
		h.relay.waitStatus(t, sid, "failed", 1)
		var msg string
		for _, p := range h.relay.payloads(sid) {
			if p["type"] == "data-session-status" {
				msg, _ = p["data"].(map[string]any)["message"].(string)
			}
		}
		if !strings.Contains(msg, c.want) {
			t.Errorf("%v: failed status %q, want %q", c.extra, msg, c.want)
		}
	}
	if runs := fakeRuns(t, h.home); len(runs) != 0 {
		t.Fatalf("refused starts ran Claude: %d", len(runs))
	}
	// A catalogue model and effort run, over the machine flags.
	sid := h.startWith("claude", "echo yes", map[string]any{"model": "sonnet", "effort": "low"})
	h.waitFinishes(sid, 1)
	if args := h.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--model sonnet --effort low") {
		t.Fatalf("argv %v", args)
	}
	if rec := h.record(sid); rec.Model != "sonnet" || rec.Effort != "low" {
		t.Fatalf("record %+v", rec)
	}
}

// Rows "Read tool, any mode", "publish_artifact" and "Non read tool" for
// Claude in read_only (acceptance 3): plan mode, denials without cards.
func TestControlsClaudeReadOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startWith("claude", "bash touch x", map[string]any{"mode": "read_only"})
	h.waitFinishes(sid, 1)
	if args := h.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--permission-mode plan") {
		t.Fatalf("argv %v", args)
	}
	h.writeCwd(sid, "report.md", []byte("# report\n"))
	for i, text := range []string{"edit a.txt", "write b.txt", "publish report.md", "askmcp search"} {
		h.message(sid, text)
		h.waitFinishes(sid, i+2)
	}
	if n := h.cards(sid); n != 0 {
		t.Fatalf("read_only produced %d cards", n)
	}
	text := h.relay.text(sid)
	if strings.Count(text, "denied: "+readOnlyDenied) != 3 || !strings.Contains(text, "publish denied") || !strings.Contains(text, "allowed") {
		t.Fatalf("text %q", text)
	}
	if ups, _ := h.api.uploads(); len(ups) != 0 {
		t.Fatal("read_only published an artifact")
	}
}

// Rows "publish_artifact" and "Non read tool" for Claude in full_auto: the
// allow flag at a full_auto ceiling, bypassPermissions; a prompt Claude
// still raises (the fake always asks) is a card, never allowed silently.
func TestControlsClaudeFullAuto(t *testing.T) {
	h := newHarness(t, nil)
	h.maxMode = ModeFullAuto
	h.start()
	waitFor(t, 10*time.Second, "controls frame", func() bool { return h.relay.lastControls() != nil })
	if c := h.relay.lastControls(); c["maxMode"] != "full_auto" || len(c["modes"].([]any)) != 4 {
		t.Fatalf("controls %v", c)
	}
	sid := h.startWith("claude", "bash rm -rf ~", map[string]any{"mode": "full_auto"})
	h.approve(sid, 1, "deny", "user", "")
	h.waitFinishes(sid, 1)
	if args := h.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--permission-mode bypassPermissions --allow-dangerously-skip-permissions") {
		t.Fatalf("argv %v", args)
	}
	if h.toolOutput(sid, "ran: rm -rf ~") {
		t.Fatal("full_auto ran a prompt the harness kept without the user")
	}
	h.message(sid, "bash touch x")
	h.approve(sid, 2, "allow", "user", "")
	h.waitFinishes(sid, 2)
	h.writeCwd(sid, "report.md", []byte("# report\n"))
	h.message(sid, "publish report.md")
	h.approve(sid, 3, "allow", "user", "")
	h.waitFinishes(sid, 3)
	h.message(sid, "askmcp search")
	h.waitFinishes(sid, 4)
	if n := h.cards(sid); n != 3 {
		t.Fatalf("full_auto cards %d, want 3 (read tools never ask)", n)
	}
	if text := h.relay.text(sid); !strings.Contains(text, "allowed") || !strings.Contains(text, "published") || !h.toolOutput(sid, "ran: touch x") {
		t.Fatalf("text %q", text)
	}
	// A session started without a mode under a full_auto ceiling: ask, with
	// the allow flag (so a later full_auto works).
	sid2 := h.startSession("echo plain")
	h.waitFinishes(sid2, 1)
	if h.record(sid2).Mode != "ask" {
		t.Fatalf("default mode %s", h.record(sid2).Mode)
	}
	args := h.claudeRunArgs()
	if len(args) != 2 || !slices.ContainsFunc(args, func(a string) bool {
		return strings.Contains(a, "--permission-mode default --allow-dangerously-skip-permissions")
	}) {
		t.Fatalf("argv %v", args)
	}
}

// Row "set_mode within ceiling" for Claude: set_permission_mode, commit on
// success, same mode a no op; refused by Claude: unchanged.
func TestControlsClaudeSetMode(t *testing.T) {
	h := newHarness(t, nil)
	h.maxMode = ModeFullAuto
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	h.setMode(sid, ModeReadOnly)
	waitFor(t, 10*time.Second, "mode committed", func() bool { return h.record(sid).Mode == "read_only" })
	if reqs := h.modeRequests(); fmt.Sprint(reqs) != "[plan]" {
		t.Fatalf("set_permission_mode requests %v", reqs)
	}
	h.message(sid, "bash ls")
	h.waitFinishes(sid, 2)
	if h.cards(sid) != 0 || !strings.Contains(h.relay.text(sid), readOnlyDenied) {
		t.Fatalf("read_only after set_mode: cards %d, text %q", h.cards(sid), h.relay.text(sid))
	}
	h.setMode(sid, ModeReadOnly) // same mode: no op
	h.setMode(sid, ModeFullAuto)
	waitFor(t, 10*time.Second, "full_auto committed", func() bool { return h.record(sid).Mode == "full_auto" })
	if reqs := h.modeRequests(); fmt.Sprint(reqs) != "[plan bypassPermissions]" {
		t.Fatalf("set_permission_mode requests %v", reqs)
	}
	h.message(sid, "bash rm y")
	h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 3)
	if h.cards(sid) != 1 || !h.toolOutput(sid, "ran: rm y") {
		t.Fatalf("full_auto after set_mode: cards %d", h.cards(sid))
	}
	// The init proof follows the committed mode (every turn's init).
	if h.record(sid).Status != "active" {
		t.Fatal("the proof failed after a mode change")
	}

	// Claude refuses the switch: the mode stays.
	h2 := newHarness(t, map[string]any{"setModeError": true})
	h2.start()
	sid2 := h2.startSession("echo one")
	h2.waitFinishes(sid2, 1)
	h2.setMode(sid2, ModeReadOnly)
	waitFor(t, 10*time.Second, "refusal logged", func() bool { return strings.Contains(h2.log.String(), "refused permission mode plan") })
	if h2.record(sid2).Mode != "ask" {
		t.Fatalf("mode changed after a refusal: %s", h2.record(sid2).Mode)
	}
	if len(h2.relay.eventsOf(sid2, "error")) != 0 {
		t.Fatal("a refused switch emitted an error chunk")
	}
	h2.message(sid2, "bash ls")
	h2.approve(sid2, 1, "allow", "user", "")
	h2.waitFinishes(sid2, 2)
}

// Rows "set_mode within ceiling" (parked session) and "Resume after ceiling
// lowered": the record changes, the next spawn applies it, re-clamped.
func TestControlsParkedSetModeAndLoweredCeiling(t *testing.T) {
	h := newHarness(t, nil)
	h.maxMode = ModeFullAuto
	h.start()
	sid := h.startWith("claude", "echo one", map[string]any{"mode": "full_auto"})
	h.waitFinishes(sid, 1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	// Restart with a lower ceiling: the stored full_auto drops to ask.
	h.maxMode = ModeAsk
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sid) >= 2 })
	if h.record(sid).Mode != "ask" {
		t.Fatalf("record not re-clamped: %s", h.record(sid).Mode)
	}
	h.message(sid, "echo two")
	h.waitFinishes(sid, 2)
	args := h.claudeRunArgs()
	if len(args) != 2 {
		t.Fatalf("runs %d", len(args))
	}
	resumed := ""
	for _, a := range args {
		if strings.Contains(a, "--resume=") {
			resumed = a
		}
	}
	if !strings.Contains(resumed, "--permission-mode default") || strings.Contains(resumed, "dangerously") {
		t.Fatalf("resumed argv %q", resumed)
	}
	// Parked (no process): set_mode updates the record; the next spawn runs it.
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sid) >= 3 })
	h.setMode(sid, ModeReadOnly)
	waitFor(t, 10*time.Second, "record updated", func() bool { return h.record(sid).Mode == "read_only" })
	if len(h.modeRequests()) != 0 {
		t.Fatal("a parked session wrote to a harness")
	}
	h.message(sid, "echo three")
	h.waitFinishes(sid, 3)
	args = h.claudeRunArgs()
	if len(args) != 3 || !slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "--permission-mode plan") }) {
		t.Fatalf("argv after parked set_mode %v", args)
	}
}

// Row "Startup proof": an init permissionMode other than the mapped start
// mode fails the session closed.
func TestControlsStartupProofFollowsMode(t *testing.T) {
	h := newHarness(t, map[string]any{"permissionMode": "default"})
	h.start()
	sid := h.startWith("claude", "echo nope", map[string]any{"mode": "read_only"})
	h.relay.waitStatus(t, sid, "failed", 1)
	if strings.Contains(h.relay.text(sid), "nope") || h.record(sid).Status != "failed" {
		t.Fatal("a mismatched init passed")
	}
	if !strings.Contains(h.log.String(), `permissionMode is "default", not "plan"`) {
		t.Fatalf("proof log:\n%s", h.log.String())
	}
}

// Row "Capability report" with an older relay (acceptance 4): the controls
// frame is refused with INVALID_FRAME, logged once, and sessions still run.
func TestControlsOlderRelay(t *testing.T) {
	h := newHarness(t, nil)
	h.relay.set(func(r *fakeRelay) { r.oldRelay = true })
	h.start()
	waitFor(t, 10*time.Second, "refusal logged", func() bool { return strings.Contains(h.log.String(), "refused the controls frame") })
	sid := h.startSession("echo still works")
	h.waitFinishes(sid, 1)
	if !strings.Contains(h.relay.text(sid), "still works") {
		t.Fatal("session did not run with an older relay")
	}
	h.relay.closeUser(4999) // reconnect: the capabilities and controls again
	waitFor(t, 15*time.Second, "second capabilities", func() bool { return h.relay.capsCount() >= 2 })
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(h.log.String(), "refused the controls frame"); n != 1 {
		t.Fatalf("refusal logged %d times", n)
	}
}

// Codex rows: read_only, set_mode at the next turn/start, full_auto, the
// ceiling clamp, model and effort, and the controls catalogue.
func TestControlsCodexModes(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.maxMode = ModeFullAuto
	h.start()
	waitFor(t, 15*time.Second, "controls frame", func() bool { return h.relay.lastControls() != nil })
	var codex map[string]any
	for _, a := range h.relay.lastControls()["agents"].([]any) {
		if a.(map[string]any)["agent"] == "codex" {
			codex = a.(map[string]any)
		}
	}
	models := codex["models"].([]any)
	if len(models) != 2 || models[1].(map[string]any)["id"] != "fake-default" || models[1].(map[string]any)["default"] != true ||
		models[1].(map[string]any)["label"] != "Fake Default" || models[1].(map[string]any)["defaultEffort"] != "low" ||
		fmt.Sprint(models[0].(map[string]any)["efforts"]) != "[low medium]" {
		t.Fatalf("codex models %v", models)
	}
	if probes := codexProbeRuns(t, h.home); len(probes) != 1 {
		t.Fatalf("probe runs %d", len(probes))
	} else if _, err := os.Stat(probes[0].CodexHome); !os.IsNotExist(err) {
		t.Fatal("probe home kept")
	}

	sid := h.startWith("codex", "cmd ls", map[string]any{"mode": "read_only", "model": "fake-other", "effort": "medium"})
	h.waitFinishes(sid, 1)
	ts := h.threadStart()
	if ts["approvalPolicy"] != "never" || ts["sandbox"] != "read-only" || ts["model"] != "fake-other" ||
		ts["config"].(map[string]any)["model_reasoning_effort"] != "medium" {
		t.Fatalf("thread/start %v", ts)
	}
	h.writeCwd(sid, "a.md", []byte("# a\n"))
	for i, text := range []string{"file a.txt", "mcp", "publish a.md"} {
		h.message(sid, text)
		h.waitFinishes(sid, i+2)
	}
	if n := h.cards(sid); n != 0 {
		t.Fatalf("read_only produced %d cards", n)
	}
	if text := h.relay.text(sid); !strings.Contains(text, "declined ls") || !strings.Contains(text, "file declined") ||
		!strings.Contains(text, "tools: ") || !strings.Contains(text, "publish declined") {
		t.Fatalf("text %q", text)
	}
	if b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "elicitation-reply.json")); !strings.Contains(string(b), `"action":"decline"`) {
		t.Fatalf("read_only publish reply %s", b)
	}
	if ups, _ := h.api.uploads(); len(ups) != 0 {
		t.Fatal("read_only published an artifact")
	}
	starts := h.turnStarts()
	if len(starts) != 4 {
		t.Fatalf("turn/starts %d", len(starts))
	}
	for _, p := range starts {
		sp := p["sandboxPolicy"].(map[string]any)
		if p["approvalPolicy"] != "never" || sp["type"] != "readOnly" || sp["networkAccess"] != false || p["model"] != "fake-other" || p["effort"] != "medium" {
			t.Fatalf("read_only turn/start %v", p)
		}
	}

	// set_mode full_auto: the next turn/start carries never/dangerFullAccess.
	h.setMode(sid, ModeFullAuto)
	waitFor(t, 5*time.Second, "record", func() bool { return h.record(sid).Mode == "full_auto" })
	// What Codex still asks under never (the fake always asks) is a card.
	h.message(sid, "cmd touch b")
	h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 5)
	h.message(sid, "publish a.md")
	h.approve(sid, 2, "allow", "user", "")
	h.waitFinishes(sid, 6)
	if n := h.cards(sid); n != 2 {
		t.Fatalf("full_auto cards %d, want 2", n)
	}
	if text := h.relay.text(sid); !strings.Contains(text, "ran touch b") || !strings.Contains(text, "published") {
		t.Fatalf("text %q", text)
	}
	if p := h.turnStarts()[4]; p["approvalPolicy"] != "never" || p["sandboxPolicy"].(map[string]any)["type"] != "dangerFullAccess" {
		t.Fatalf("full_auto turn/start %v", p)
	}

	// set_mode ask: untrusted/workspaceWrite with the isolation fields, cards again.
	h.setMode(sid, ModeAsk)
	h.message(sid, "cmd rm c")
	h.approve(sid, 3, "deny", "user", "")
	h.waitFinishes(sid, 7)
	p := h.turnStarts()[6]
	sp := p["sandboxPolicy"].(map[string]any)
	if p["approvalPolicy"] != "untrusted" || sp["type"] != "workspaceWrite" || sp["networkAccess"] != false ||
		sp["excludeSlashTmp"] != true || sp["excludeTmpdirEnvVar"] != true || len(sp["writableRoots"].([]any)) != 0 {
		t.Fatalf("ask turn/start %v", p)
	}
	if len(h.relay.invalid) != 0 {
		t.Fatalf("invalid events %v", h.relay.invalid)
	}
}

// Codex acceptance 2: under auto_edits, full_auto at start or by set_mode
// never reaches never/danger-full-access; a bad Codex model is refused.
func TestControlsCodexCeilingAndCatalogue(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startWith("codex", "echo one", map[string]any{"mode": "full_auto"})
	h.waitFinishes(sid, 1)
	h.setMode(sid, ModeFullAuto)
	h.message(sid, "echo two")
	h.waitFinishes(sid, 2)
	if ts := h.threadStart(); ts["approvalPolicy"] != "untrusted" || ts["sandbox"] != "workspace-write" {
		t.Fatalf("thread/start %v", ts)
	}
	for _, p := range h.turnStarts() {
		if p["approvalPolicy"] != "untrusted" || p["sandboxPolicy"].(map[string]any)["type"] != "workspaceWrite" {
			t.Fatalf("turn/start above the ceiling %v", p)
		}
	}
	if h.record(sid).Mode != "auto_edits" {
		t.Fatalf("mode %s", h.record(sid).Mode)
	}
	for _, extra := range []map[string]any{{"model": "fake-hidden"}, {"model": "gpt-nope"}, {"model": "fake-other", "effort": "xhigh"}} {
		bad := h.startWith("codex", "echo nope", extra)
		h.relay.waitStatus(t, bad, "failed", 1)
	}
	if n := len(codexRuns(t, h.home)); n != 1 {
		t.Fatalf("refused Codex starts ran Codex (%d runs)", n)
	}
}

// The session-bound sandbox: full_auto ceiling and default mode, --mode.
func TestControlsSandboxDefaults(t *testing.T) {
	h := newHarness(t, nil)
	h.maxMode = SandboxMaxMode
	sess := h.api.addSession("claude")
	r := h.runSession(sess.SessionID, "claude", "", "bash touch x")
	h.approve(sess.SessionID, 1, "allow", "user", "") // what Claude still asks is a card
	h.waitFinishes(sess.SessionID, 1)
	if args := h.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--permission-mode bypassPermissions --allow-dangerously-skip-permissions") {
		t.Fatalf("argv %v", args)
	}
	r.stop()

	h2 := newHarness(t, nil)
	h2.maxMode, h2.sessionMode = SandboxMaxMode, ModeReadOnly
	sess2 := h2.api.addSession("claude")
	r2 := h2.runSession(sess2.SessionID, "claude", "", "bash touch x")
	h2.waitFinishes(sess2.SessionID, 1)
	if args := h2.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--permission-mode plan") {
		t.Fatalf("argv %v", args)
	}
	if !strings.Contains(h2.relay.text(sess2.SessionID), readOnlyDenied) {
		t.Fatalf("text %q", h2.relay.text(sess2.SessionID))
	}
	r2.stop()
}

// connect --session --model/--effort (Phase B): carried into the synthetic
// start, so handleStart checks them against the catalogue like a relay start.
func TestControlsSandboxModelAndEffort(t *testing.T) {
	h := newHarness(t, nil)
	h.maxMode, h.sessionModel, h.sessionEffort = SandboxMaxMode, "sonnet", "low"
	sess := h.api.addSession("claude")
	r := h.runSession(sess.SessionID, "claude", "", "hello")
	h.waitFinishes(sess.SessionID, 1)
	if args := h.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--model sonnet") || !strings.Contains(args[0], "--effort low") {
		t.Fatalf("argv %v", args)
	}
	r.stop()

	for _, c := range []struct{ model, effort, msg string }{
		{"gpt-6.1-sol", "", `The model "gpt-6.1-sol" is not offered for Claude Code here.`},
		{"haiku", "high", `The effort "high" is not offered for the Claude model "haiku".`},
	} {
		h2 := newHarness(t, nil)
		h2.maxMode, h2.sessionModel, h2.sessionEffort = SandboxMaxMode, c.model, c.effort
		sess2 := h2.api.addSession("claude")
		r2 := h2.runSession(sess2.SessionID, "claude", "", "hello")
		var fe *FatalError
		if err := r2.wait(); !errors.As(err, &fe) || fe.Code != "START_REFUSED" {
			t.Fatalf("%s/%s: RunSession %v", c.model, c.effort, err)
		}
		if !strings.Contains(h2.log.String(), c.msg) || len(h2.claudeRunArgs()) != 0 {
			t.Fatalf("%s/%s: log %s, runs %v", c.model, c.effort, h2.log.String(), h2.claudeRunArgs())
		}
	}
}

// ─── review pass 1 rows ─────────────────────────────────────────────────────

func TestPathInsideAndCodexFileInCwd(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "f"), filepath.Join(cwd, "filelink")); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		filepath.Join(cwd, "a.txt"):            true,
		filepath.Join(cwd, "new/dir/a.txt"):    true,
		filepath.Join(cwd, "x/../a.txt"):       true,
		cwd:                                    false,
		filepath.Join(cwd, "../a.txt"):         false,
		filepath.Join(cwd, "link/a.txt"):       false,
		filepath.Join(cwd, "link"):             false,
		filepath.Join(cwd, "filelink"):         false,
		filepath.Join(outside, "a.txt"):        false,
		"a.txt":                                false,
		"":                                     false,
		filepath.Join(cwd+"-sibling", "a.txt"): false,
	} {
		if got := pathInside(p, cwd); got != want {
			t.Errorf("pathInside(%q) = %v, want %v", p, got, want)
		}
	}
	files := map[string]json.RawMessage{
		"in":      json.RawMessage(`[{"path":"` + filepath.Join(cwd, "a.txt") + `","kind":{"type":"add"},"diff":""}]`),
		"out":     json.RawMessage(`[{"path":"` + filepath.Join(cwd, "a.txt") + `","kind":{"type":"add"}},{"path":"` + filepath.Join(outside, "b") + `","kind":{"type":"add"}}]`),
		"move":    json.RawMessage(`[{"path":"` + filepath.Join(cwd, "a.txt") + `","kind":{"type":"update","move_path":"` + filepath.Join(outside, "b") + `"}}]`),
		"movein":  json.RawMessage(`[{"path":"` + filepath.Join(cwd, "a.txt") + `","kind":{"type":"update","move_path":"` + filepath.Join(cwd, "b") + `"}}]`),
		"relpath": json.RawMessage(`[{"path":"a.txt","kind":{"type":"add"}}]`),
		"empty":   json.RawMessage(`[]`),
	}
	for params, want := range map[string]bool{
		`{"itemId":"in"}`:                  true,
		`{"itemId":"in","grantRoot":null}`: true,
		`{"itemId":"in","grantRoot":""}`:   true,
		`{"itemId":"movein"}`:              true,
		`{"itemId":"in","grantRoot":"/"}`:  false,
		`{"itemId":"out"}`:                 false,
		`{"itemId":"move"}`:                false,
		`{"itemId":"relpath"}`:             false,
		`{"itemId":"empty"}`:               false,
		`{"itemId":"unknown"}`:             false,
		`not json`:                         false,
	} {
		if got := codexFileInCwd(json.RawMessage(params), files, cwd); got != want {
			t.Errorf("codexFileInCwd(%s) = %v, want %v", params, got, want)
		}
	}
}

// Row "Codex Auto edits": untrusted plus the daemon's in directory file
// change acceptance; anything else is a card.
func TestControlsCodexAutoEdits(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startWith("codex", "file a.txt", map[string]any{"mode": "auto_edits"})
	h.waitFinishes(sid, 1)
	if ts := h.threadStart(); ts["approvalPolicy"] != "untrusted" || ts["sandbox"] != "workspace-write" {
		t.Fatalf("thread/start %v", ts)
	}
	if h.cards(sid) != 0 || !strings.Contains(h.relay.text(sid), "wrote a.txt") {
		t.Fatalf("in directory file change: cards %d text %q", h.cards(sid), h.relay.text(sid))
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(h.record(sid).Cwd, "link")); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"file ../outside.txt", "file link/x.txt", "filegrant b.txt", "cmd ls"} {
		h.message(sid, text)
		h.approve(sid, i+1, "deny", "user", "")
		h.waitFinishes(sid, i+2)
	}
	if n := h.cards(sid); n != 4 {
		t.Fatalf("cards %d, want 4", n)
	}
	for _, p := range h.turnStarts() {
		if p["approvalPolicy"] != "untrusted" {
			t.Fatalf("auto_edits turn/start %v", p)
		}
	}
}

// Row "Lower to read_only with cards open", Claude: the pending card is
// answered deny when read_only commits; another change leaves it open.
func TestControlsClaudeReadOnlyDeniesPendingCards(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("bash touch x")
	waitFor(t, 10*time.Second, "card", func() bool { return h.cards(sid) == 1 })
	h.setMode(sid, ModeAutoEdits)
	waitFor(t, 10*time.Second, "auto_edits committed", func() bool { return h.record(sid).Mode == "auto_edits" })
	time.Sleep(200 * time.Millisecond)
	if len(h.relay.eventsOf(sid, "tool-approval-response")) != 0 {
		t.Fatal("a mode change other than read_only answered the pending card")
	}
	h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 1)

	h.message(sid, "bash rm y")
	waitFor(t, 10*time.Second, "second card", func() bool { return h.cards(sid) == 2 })
	h.setMode(sid, ModeReadOnly)
	h.waitFinishes(sid, 2)
	resp := h.relay.eventsOf(sid, "tool-approval-response")
	last := resp[len(resp)-1]["payload"].(map[string]any)
	if len(resp) != 2 || last["approved"] != false || last["reason"] != readOnlyDenied {
		t.Fatalf("responses %v", resp)
	}
	if !strings.Contains(h.relay.text(sid), "denied: "+readOnlyDenied) || h.cards(sid) != 2 {
		t.Fatalf("text %q cards %d", h.relay.text(sid), h.cards(sid))
	}
}

// Row "Lower to read_only with cards open", Codex: command and MCP cards
// are declined when read_only commits; another change leaves them open.
func TestControlsCodexReadOnlyDeniesPendingCards(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("cmd rm a")
	waitFor(t, 10*time.Second, "card", func() bool { return h.cards(sid) == 1 })
	h.setMode(sid, ModeAutoEdits)
	time.Sleep(200 * time.Millisecond)
	if len(h.relay.eventsOf(sid, "tool-approval-response")) != 0 {
		t.Fatal("a mode change other than read_only answered the pending card")
	}
	h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 1)

	h.message(sid, "cmd rm b")
	waitFor(t, 10*time.Second, "command card", func() bool { return h.cards(sid) == 2 })
	h.setMode(sid, ModeReadOnly)
	h.waitFinishes(sid, 2)
	h.setMode(sid, ModeAsk)
	h.writeCwd(sid, "a.md", []byte("# a\n"))
	h.message(sid, "publish a.md")
	waitFor(t, 10*time.Second, "publish card", func() bool { return h.cards(sid) == 3 })
	h.setMode(sid, ModeReadOnly)
	h.waitFinishes(sid, 3)
	text := h.relay.text(sid)
	if !strings.Contains(text, "ran rm a") || !strings.Contains(text, "declined rm b") || !strings.Contains(text, "publish declined") {
		t.Fatalf("text %q", text)
	}
	var reasons []string
	for _, r := range h.relay.eventsOf(sid, "tool-approval-response") {
		reasons = append(reasons, r["payload"].(map[string]any)["reason"].(string))
	}
	if strings.Join(reasons, ",") != "accept,decline,decline" {
		t.Fatalf("responses %v", reasons)
	}
	if ups, _ := h.api.uploads(); len(ups) != 0 {
		t.Fatal("a declined publish uploaded")
	}
}

// Overlapping MCP calls: a publish_artifact approval while a search call
// started later is never taken for a read tool: card in ask and auto_edits.
func TestControlsCodexOverlappingMCPCallsCard(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	for i, mode := range []string{"ask", "auto_edits"} {
		sid := h.startWith("codex", "overlap", map[string]any{"mode": mode})
		h.approve(sid, 1, "deny", "user", "")
		h.waitFinishes(sid, 1)
		if !strings.Contains(h.relay.text(sid), "overlap: decline") {
			t.Fatalf("%d %s: text %q", i, mode, h.relay.text(sid))
		}
	}
}

// Only the first INVALID_FRAME after a controls send is the controls
// refusal; any other takes the usual error path.
func TestControlsOtherInvalidFrameFallsThrough(t *testing.T) {
	h := newHarness(t, nil)
	h.relay.set(func(r *fakeRelay) { r.oldRelay = true })
	h.start()
	waitFor(t, 10*time.Second, "refusal logged", func() bool { return strings.Contains(h.log.String(), "refused the controls frame") })
	h.relay.sendRaw("", `{"kind":"error","code":"INVALID_FRAME"}`)
	waitFor(t, 10*time.Second, "error logged", func() bool { return strings.Contains(h.log.String(), "relay error INVALID_FRAME") })
}

// Default marking: the machine flag's catalogue entry, the catalogue's own
// default without a flag, none when the flag names no entry.
func TestControlsDefaultMarking(t *testing.T) {
	defaults := func(claudeModel, codexModel string) (claude, codex []string) {
		d := &Daemon{claude: ClaudeConfig{Bin: "/c", Model: claudeModel}, codex: CodexConfig{Bin: "/x", Model: codexModel},
			maxMode: ModeAsk, codexProbed: make(chan struct{}),
			codexCat: []modelEntry{{ID: "gpt-a", Default: true}, {ID: "gpt-b"}}}
		d.probeOnce.Do(func() {})
		close(d.codexProbed)
		var f struct {
			Agents []agentControls `json:"agents"`
		}
		if err := json.Unmarshal(d.controls(context.Background()), &f); err != nil {
			t.Fatal(err)
		}
		for _, a := range f.Agents {
			for _, m := range a.Models {
				if !m.Default {
					continue
				}
				if a.Agent == "claude" {
					claude = append(claude, m.ID)
				} else {
					codex = append(codex, m.ID)
				}
			}
		}
		return claude, codex
	}
	for _, c := range []struct {
		claudeModel, codexModel string
		claude, codex           string
	}{
		{"", "", "[default]", "[gpt-a]"},
		{"opus", "gpt-b", "[opus]", "[gpt-b]"},
		{"claude-opus-5-5[1m]", "gpt-zzz", "[]", "[]"},
	} {
		cl, cx := defaults(c.claudeModel, c.codexModel)
		if fmt.Sprint(cl) != c.claude {
			t.Errorf("--claude-model %q: default %v, want %s", c.claudeModel, cl, c.claude)
		}
		if fmt.Sprint(cx) != c.codex {
			t.Errorf("--codex-model %q: default %v, want %s", c.codexModel, cx, c.codex)
		}
	}
}

// d.controls trims a catalogue that does not fit (Phase B): the frame is
// within the relay's cap, the machine flag's model (mid list) is kept and
// marked default, and the log names how many models were left out.
func TestControlsReportTrimsTheCodexCatalogue(t *testing.T) {
	efforts := make([]string, 16)
	for i := range efforts {
		efforts[i] = fmt.Sprintf("e%02d", i) + strings.Repeat("z", 29)
	}
	var cat []modelEntry
	for i := range 64 {
		cat = append(cat, modelEntry{ID: fmt.Sprintf("m%02d-", i) + strings.Repeat("x", 124), Label: strings.Repeat("L", 64),
			Efforts: efforts, DefaultEffort: efforts[0], Default: i == 0})
	}
	var log bytes.Buffer
	d := &Daemon{claude: ClaudeConfig{Bin: "/c"}, codex: CodexConfig{Bin: "/x", Model: cat[30].ID},
		maxMode: ModeAsk, codexProbed: make(chan struct{}), codexCat: cat, log: NewLogger(&log)}
	d.probeOnce.Do(func() {})
	close(d.codexProbed)
	frame := d.controls(context.Background())
	if len(frame) > maxControlsBytes {
		t.Fatalf("frame %d bytes", len(frame))
	}
	var f struct {
		Agents []agentControls `json:"agents"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 2 || f.Agents[1].Agent != "codex" {
		t.Fatalf("agents %+v", f.Agents)
	}
	kept := f.Agents[1].Models
	var defaults []string
	for _, m := range kept {
		if m.Default {
			defaults = append(defaults, m.ID)
		}
	}
	if fmt.Sprint(defaults) != fmt.Sprint([]string{cat[30].ID}) {
		t.Fatalf("defaults %v, want only %s", defaults, cat[30].ID)
	}
	dropped := 64 - len(kept)
	if dropped == 0 || !strings.Contains(log.String(), fmt.Sprintf("controls report: %d Codex models left out to fit", dropped)) {
		t.Fatalf("kept %d, log %q", len(kept), log.String())
	}
}

// A Claude process stopping before it answers set_permission_mode: the
// acked change is recorded and the next spawn applies it.
func TestControlsStopCommitsPendingMode(t *testing.T) {
	h := newHarness(t, map[string]any{"setModeHang": true})
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	h.setMode(sid, ModeReadOnly)
	waitFor(t, 10*time.Second, "request sent", func() bool { return len(h.modeRequests()) == 1 })
	if h.record(sid).Mode != "ask" {
		t.Fatal("committed before Claude answered")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if h.record(sid).Mode != "read_only" {
		t.Fatalf("pending mode lost at stop: %s", h.record(sid).Mode)
	}
	// The stop commit happens after the session left running, so it reports
	// nothing; the running transition of the next spawn carries the mode.
	if got := h.controlsEvents(sid); fmt.Sprint(got) != "[map[maxMode:auto_edits mode:ask]]" {
		t.Fatalf("controls at stop %v", got)
	}
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sid) >= 2 })
	h.message(sid, "echo two")
	h.waitFinishes(sid, 2)
	if got := h.waitControls(sid, 2); fmt.Sprint(got) != "[map[maxMode:auto_edits mode:ask] map[maxMode:auto_edits mode:read_only]]" {
		t.Fatalf("controls after the resume %v", got)
	}
	if args := h.claudeRunArgs(); len(args) != 2 || !slices.ContainsFunc(args, func(a string) bool {
		return strings.Contains(a, "--resume=") && strings.Contains(a, "--permission-mode plan")
	}) {
		t.Fatalf("argv %v", args)
	}
}

// A machine effort the session's model does not offer is left out.
func TestControlsMachineEffortDroppedForSessionModel(t *testing.T) {
	h := newHarness(t, nil)
	h.claudeEffort = "high"
	h.start()
	sid := h.startWith("claude", "echo hi", map[string]any{"model": "haiku"})
	h.waitFinishes(sid, 1)
	sid2 := h.startWith("claude", "echo hi", map[string]any{"model": "opus"})
	h.waitFinishes(sid2, 1)
	args := h.claudeRunArgs()
	var haiku, opus string
	for _, a := range args {
		if strings.Contains(a, "--model haiku") {
			haiku = a
		}
		if strings.Contains(a, "--model opus") {
			opus = a
		}
	}
	if haiku == "" || strings.Contains(haiku, "--effort") || !strings.Contains(opus, "--model opus --effort high") {
		t.Fatalf("argv %v", args)
	}

	hc := newCodexHarness(t, nil)
	hc.codexEffort = "xhigh"
	hc.start()
	sid3 := hc.startWith("codex", "echo hi", map[string]any{"model": "fake-other"})
	hc.waitFinishes(sid3, 1)
	ts := hc.threadStart()
	if ts["model"] != "fake-other" || ts["config"] != nil {
		t.Fatalf("thread/start %v", ts)
	}
	for _, p := range hc.turnStarts() {
		if _, ok := p["effort"]; ok || p["model"] != "fake-other" {
			t.Fatalf("turn/start %v", p)
		}
	}
}

// A record written before 78.32 (no mode) resumes in ask: Claude
// --permission-mode default.
func TestControlsPre7832RecordResumesAsk(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.api.addSession("claude")
	cwd, err := os.MkdirTemp(h.tmp, cwdPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	claudeID := randomUUID()
	if err := st.Save(&SessionRecord{SessionID: sess.SessionID, Agent: "claude", StartCorrelationID: "old-start",
		ClaudeSessionID: claudeID, Cwd: cwd, ExpiresAt: sess.ExpiresAt, Status: "active", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sess.SessionID) >= 1 })
	h.message(sess.SessionID, "echo resumed")
	h.waitFinishes(sess.SessionID, 1)
	if h.record(sess.SessionID).Mode != "ask" {
		t.Fatalf("record mode %q", h.record(sess.SessionID).Mode)
	}
	if args := h.claudeRunArgs(); len(args) != 1 || !strings.Contains(args[0], "--resume="+claudeID) ||
		!strings.Contains(args[0], "--permission-mode default") || strings.Contains(args[0], "dangerously") {
		t.Fatalf("argv %v", args)
	}
}

// ─── review pass 2 rows ─────────────────────────────────────────────────────

// Codex MCP "allow for session" is kept by the daemon: later publishes in
// ask are accepted without a card, and after read_only they are declined.
func TestControlsCodexMCPSessionAllowYieldsToReadOnly(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("echo one")
	h.waitFinishes(sid, 1)
	h.writeCwd(sid, "a.md", []byte("# a\n"))
	h.message(sid, "publish a.md")
	h.approve(sid, 1, "allow", "user", "allow_always")
	h.waitFinishes(sid, 2)
	h.message(sid, "publish a.md")
	h.waitFinishes(sid, 3)
	if n := h.cards(sid); n != 1 {
		t.Fatalf("cards %d after allow for session, want 1", n)
	}
	if ups, _ := h.api.uploads(); len(ups) != 2 {
		t.Fatalf("uploads %d, want 2", len(ups))
	}
	h.setMode(sid, ModeReadOnly)
	h.message(sid, "publish a.md")
	h.waitFinishes(sid, 4)
	if n := h.cards(sid); n != 1 {
		t.Fatalf("read_only produced a card (%d)", n)
	}
	if b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "elicitation-reply.json")); !strings.Contains(string(b), `"action":"decline"`) {
		t.Fatalf("read_only reply %s", b)
	}
	if ups, _ := h.api.uploads(); len(ups) != 2 || !strings.Contains(h.relay.text(sid), "publish declined") {
		t.Fatalf("read_only published (uploads %d), text %q", len(ups), h.relay.text(sid))
	}
}

// An accepting relay answers nothing to controls: an unrelated INVALID_FRAME
// after the window takes the usual error path.
func TestControlsInvalidFrameAfterWindowIsNotARefusal(t *testing.T) {
	setDuration(t, &controlsRefusalWindow, 50*time.Millisecond)
	h := newHarness(t, nil)
	h.start()
	waitFor(t, 10*time.Second, "controls frame", func() bool { return h.relay.lastControls() != nil })
	time.Sleep(150 * time.Millisecond)
	h.relay.sendRaw("", `{"kind":"error","code":"INVALID_FRAME"}`)
	waitFor(t, 10*time.Second, "error logged", func() bool { return strings.Contains(h.log.String(), "relay error INVALID_FRAME") })
	if strings.Contains(h.log.String(), "refused the controls frame") {
		t.Fatal("an unrelated INVALID_FRAME was taken for the controls refusal")
	}
}

// Read only overrides a remembered Claude "allow for session".
func TestControlsReadOnlyOverridesRememberedAllow(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("bash touch a")
	h.approve(sid, 1, "allow", "user", "allow_always")
	h.waitFinishes(sid, 1)
	h.setMode(sid, ModeReadOnly)
	waitFor(t, 10*time.Second, "read_only committed", func() bool { return h.record(sid).Mode == "read_only" })
	h.message(sid, "bash touch b")
	h.waitFinishes(sid, 2)
	if h.cards(sid) != 1 || h.toolOutput(sid, "ran: touch b") || !strings.Contains(h.relay.text(sid), "denied: "+readOnlyDenied) {
		t.Fatalf("cards %d, text %q", h.cards(sid), h.relay.text(sid))
	}
}

// Claude applies a mode but its answer is still pending: the next turn's
// init reports the requested mode, which passes the proof.
func TestControlsInitWithPendingModePasses(t *testing.T) {
	h := newHarness(t, map[string]any{"setModeSilent": true})
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	h.setMode(sid, ModeReadOnly)
	waitFor(t, 10*time.Second, "request sent", func() bool { return len(h.modeRequests()) == 1 })
	h.message(sid, "echo two")
	h.waitFinishes(sid, 2)
	if !strings.Contains(h.relay.text(sid), "two") || slices.Contains(h.relay.statuses(sid), "failed") ||
		strings.Contains(h.log.String(), "did not start on the subscription login") {
		t.Fatalf("statuses %v\n%s", h.relay.statuses(sid), h.log.String())
	}
}

// An unreadable ancestor is never taken for a missing one.
func TestPathInsideUnreadableAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	cwd := t.TempDir()
	locked := filepath.Join(cwd, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if pathInside(filepath.Join(locked, "x"), cwd) {
		t.Fatal("a path under an unreadable directory counted as inside")
	}
}

// ─── Phase B part 1b: mosaic-event/3 emission ───────────────────────────────

// controlsEvents lists the session's data-session-controls payload data,
// each checked to be stamped mosaic-event/3.
func (h *harness) controlsEvents(sid string) []string {
	h.t.Helper()
	var out []string
	for _, e := range h.relay.eventsOf(sid, "data-session-controls") {
		if e["schemaVersion"] != "mosaic-event/3" {
			h.t.Errorf("data-session-controls stamped %v", e["schemaVersion"])
		}
		out = append(out, fmt.Sprint(e["payload"].(map[string]any)["data"]))
	}
	return out
}

// waitControls waits for n data-session-controls events and returns them.
func (h *harness) waitControls(sid string, n int) []string {
	h.t.Helper()
	waitFor(h.t, 10*time.Second, fmt.Sprintf("%d data-session-controls events", n), func() bool { return len(h.controlsEvents(sid)) >= n })
	return h.controlsEvents(sid)
}

// data-session-controls for Claude: sent when the session runs, then after
// each set_mode outcome (committed on Claude's success, unchanged, refused
// above the ceiling, refused by Claude), always with the mode that runs.
// The session's model is carried; the machine effort Haiku lacks is not.
func TestControlsSessionControlsEventsClaude(t *testing.T) {
	h := newHarness(t, nil)
	h.claudeEffort = "max"
	h.start()
	sid := h.startWith("claude", "echo one", map[string]any{"model": "haiku"})
	h.waitFinishes(sid, 1)
	ask := "map[maxMode:auto_edits mode:ask model:haiku]"
	ro := "map[maxMode:auto_edits mode:read_only model:haiku]"
	if got := h.waitControls(sid, 1); fmt.Sprint(got) != fmt.Sprint([]string{ask}) {
		t.Fatalf("running: %v", got)
	}
	h.setMode(sid, ModeReadOnly)
	if got := h.waitControls(sid, 2); got[1] != ro {
		t.Fatalf("committed: %v", got)
	}
	h.setMode(sid, ModeReadOnly) // unchanged: confirmed
	h.setMode(sid, ModeFullAuto) // above the ceiling: refused
	if got := h.waitControls(sid, 4); fmt.Sprint(got[2:]) != fmt.Sprint([]string{ro, ro}) {
		t.Fatalf("unchanged and refused: %v", got)
	}
	if reqs := h.modeRequests(); fmt.Sprint(reqs) != "[plan]" {
		t.Fatalf("set_permission_mode requests %v", reqs)
	}

	h2 := newHarness(t, map[string]any{"setModeError": true})
	h2.start()
	sid2 := h2.startSession("echo one")
	h2.waitFinishes(sid2, 1)
	h2.setMode(sid2, ModeReadOnly)
	if got := h2.waitControls(sid2, 2); fmt.Sprint(got) != "[map[maxMode:auto_edits mode:ask] map[maxMode:auto_edits mode:ask]]" {
		t.Fatalf("refused by Claude: %v", got)
	}
}

// data-session-controls for Codex: the record commits at once, the event
// follows; a mode above the ceiling is refused and reported unchanged. The
// model and effort are what the handshake negotiated: with no model chosen,
// model/list's default and the machine effort.
func TestControlsSessionControlsEventsCodex(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.codexEffort = "low"
	h.start()
	sid := h.startWith("codex", "echo one", map[string]any{"mode": "read_only"})
	h.waitFinishes(sid, 1)
	ro := "map[effort:low maxMode:auto_edits mode:read_only model:fake-default]"
	ae := "map[effort:low maxMode:auto_edits mode:auto_edits model:fake-default]"
	if got := h.waitControls(sid, 1); got[0] != ro {
		t.Fatalf("running: %v", got)
	}
	h.setMode(sid, ModeAutoEdits)
	h.setMode(sid, ModeFullAuto)
	if got := h.waitControls(sid, 3); fmt.Sprint(got) != fmt.Sprint([]string{ro, ae, ae}) {
		t.Fatalf("committed and refused: %v", got)
	}
	// A session model and effort over the machine flags.
	sid2 := h.startWith("codex", "echo two", map[string]any{"model": "fake-other", "effort": "medium"})
	h.waitFinishes(sid2, 1)
	if got := h.waitControls(sid2, 1); got[0] != "map[effort:medium maxMode:auto_edits mode:ask model:fake-other]" {
		t.Fatalf("session model: %v", got)
	}
	// A machine effort the session's model does not offer is not sent to
	// Codex, so the event carries none.
	h2 := newCodexHarness(t, nil)
	h2.codexEffort = "high"
	h2.start()
	sid3 := h2.startWith("codex", "echo three", map[string]any{"model": "fake-other"})
	h2.waitFinishes(sid3, 1)
	if got := h2.waitControls(sid3, 1); got[0] != "map[maxMode:auto_edits mode:ask model:fake-other]" {
		t.Fatalf("machine effort the model lacks: %v", got)
	}
}

// Codex's thread/start answer names the model: when it differs from the
// one asked for (a substitute, a resumed thread), data-session-controls and
// every turn/start carry Codex's model, not the requested one.
func TestControlsSessionControlsCodexReportedModel(t *testing.T) {
	h := newCodexHarness(t, map[string]any{"threadModel": "fake-substitute"})
	h.start()
	sid := h.startWith("codex", "echo one", map[string]any{"model": "fake-other", "effort": "medium"})
	h.waitFinishes(sid, 1)
	if got := h.threadStart()["model"]; got != "fake-other" {
		t.Fatalf("thread/start asked for %v", got)
	}
	if got := h.waitControls(sid, 1); got[0] != "map[effort:medium maxMode:auto_edits mode:ask model:fake-substitute]" {
		t.Fatalf("controls: %v", got)
	}
	ts := h.turnStarts()
	if len(ts) != 1 || ts[0]["model"] != "fake-substitute" {
		t.Fatalf("turn/start %v", ts)
	}
}

// withContext adds each context field only when known and never changes
// its input; other chunks pass unchanged.
func TestWithContext(t *testing.T) {
	usage := func() Chunk {
		return Chunk{"type": "data-usage", "data": map[string]any{"inputTokens": 1, "outputTokens": 2, "model": "m"}}
	}
	for _, c := range []struct {
		u    contextUsage
		want string
	}{
		{contextUsage{}, "map[inputTokens:1 model:m outputTokens:2]"},
		{contextUsage{Tokens: 1208}, "map[contextTokens:1208 inputTokens:1 model:m outputTokens:2]"},
		{contextUsage{Window: 272000}, "map[contextWindow:272000 inputTokens:1 model:m outputTokens:2]"},
		{contextUsage{Tokens: 15, Window: 272000}, "map[contextTokens:15 contextWindow:272000 inputTokens:1 model:m outputTokens:2]"},
		{contextUsage{Tokens: -1, Window: -1}, "map[inputTokens:1 model:m outputTokens:2]"},
	} {
		in := usage()
		out := withContext(in, c.u)
		if got := fmt.Sprint(out["data"]); got != c.want {
			t.Errorf("%+v: %s, want %s", c.u, got, c.want)
		}
		if fmt.Sprint(in["data"]) != "map[inputTokens:1 model:m outputTokens:2]" {
			t.Errorf("%+v: input changed: %v", c.u, in["data"])
		}
	}
	text := Chunk{"type": "text-delta", "id": "a", "delta": "hi"}
	if got := withContext(text, contextUsage{Tokens: 5, Window: 9}); fmt.Sprint(got) != fmt.Sprint(text) {
		t.Fatalf("text chunk changed: %v", got)
	}
}

// A Codex catalogue with more than one default (model/list's isDefault)
// keeps only the first, without --codex-model: the relay refuses a whole
// report with two defaults for one agent.
func TestControlsOneDefaultPerAgent(t *testing.T) {
	d := &Daemon{claude: ClaudeConfig{Bin: "/c"}, codex: CodexConfig{Bin: "/x"},
		maxMode: ModeAsk, codexProbed: make(chan struct{}),
		codexCat: []modelEntry{{ID: "gpt-a"}, {ID: "gpt-b", Default: true}, {ID: "gpt-c", Default: true}, {ID: "gpt-d", Default: true}}}
	d.probeOnce.Do(func() {})
	close(d.codexProbed)
	var f struct {
		Agents []agentControls `json:"agents"`
	}
	if err := json.Unmarshal(d.controls(context.Background()), &f); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Agents {
		var defaults []string
		for _, m := range a.Models {
			if m.Default {
				defaults = append(defaults, m.ID)
			}
		}
		want := "[default]"
		if a.Agent == "codex" {
			want = "[gpt-b]"
		}
		if fmt.Sprint(defaults) != want {
			t.Errorf("%s defaults %v, want %s", a.Agent, defaults, want)
		}
	}
	// The probed catalogue itself is left as it was.
	if !d.codexCat[2].Default || !d.codexCat[3].Default {
		t.Fatal("controls changed the cached catalogue")
	}
}

// A set_mode on a parked Claude session (no process, not running) commits
// the record but sends no data-session-controls: nothing reaches the relay
// before the init proof. The next spawn's running transition reports it,
// exactly once, with the new mode.
func TestControlsParkedSetModeEmitsOnlyWhenRunning(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo one")
	h.waitFinishes(sid, 1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, 10*time.Second, "session reattach", func() bool { return h.relay.dialCount(sid) >= 2 })
	h.setMode(sid, ModeReadOnly)
	waitFor(t, 10*time.Second, "record updated", func() bool { return h.record(sid).Mode == "read_only" })
	h.setMode(sid, ModeFullAuto) // above the ceiling, still parked: refused, nothing sent
	ask := "map[maxMode:auto_edits mode:ask]"
	if got := h.controlsEvents(sid); fmt.Sprint(got) != fmt.Sprint([]string{ask}) {
		t.Fatalf("controls while parked %v", got)
	}
	h.message(sid, "echo two")
	h.waitFinishes(sid, 2)
	if got := h.controlsEvents(sid); fmt.Sprint(got) != fmt.Sprint([]string{ask, "map[maxMode:auto_edits mode:read_only]"}) {
		t.Fatalf("controls after the spawn %v", got)
	}
}
