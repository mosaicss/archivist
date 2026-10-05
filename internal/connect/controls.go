package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/taskscope"
)

// Session controls (Story 78.32): a permission mode, a model and an effort
// per session, chosen by the user on a relay surface and enforced here. The
// relay supplies only mode, model and effort ids; every id is checked
// against the closed sets below or this daemon's own capability report, and
// the harness flags and policies they map to are fixed in this file. The
// ceiling (the highest mode a session may run) is set on this machine
// (archivist connect --max-permission) and a remote surface can never raise
// it.

// Mode is a session permission mode.
type Mode string

// Modes, low to high.
const (
	ModeReadOnly  Mode = "read_only"
	ModeAsk       Mode = "ask"
	ModeAutoEdits Mode = "auto_edits"
	ModeFullAuto  Mode = "full_auto"
)

// Modes lists every mode from the lowest to the highest.
var Modes = []Mode{ModeReadOnly, ModeAsk, ModeAutoEdits, ModeFullAuto}

// Ceilings when --max-permission is not given.
const (
	// DefaultMaxMode is the local daemon's ceiling.
	DefaultMaxMode = ModeAutoEdits
	// SandboxMaxMode is the session-bound (connect --session) ceiling: the
	// Mosaic cloud sandbox is the isolation.
	SandboxMaxMode = ModeFullAuto
)

var modeLabels = map[Mode]string{
	ModeReadOnly:  "Read only",
	ModeAsk:       "Ask every time",
	ModeAutoEdits: "Auto edits",
	ModeFullAuto:  "Full auto",
}

// ParseMode accepts exactly one of the four mode ids.
func ParseMode(s string) (Mode, bool) {
	m := Mode(s)
	return m, m.rank() >= 0
}

// rank is the mode's position from low to high, -1 for an unknown mode.
func (m Mode) rank() int { return slices.Index(Modes, m) }

// Label is the default user facing name of the mode.
func (m Mode) Label() string { return modeLabels[m] }

// ClampMode lowers m to the ceiling. An unknown mode or ceiling never means
// a higher mode: it clamps to read_only.
func ClampMode(m, ceiling Mode) Mode {
	if m.rank() < 0 || ceiling.rank() < 0 {
		return ModeReadOnly
	}
	if m.rank() > ceiling.rank() {
		return ceiling
	}
	return m
}

// ModesUpTo lists the modes a session may use under the ceiling.
func ModesUpTo(ceiling Mode) []Mode {
	if ceiling.rank() < 0 {
		return []Mode{ModeReadOnly}
	}
	return append([]Mode(nil), Modes[:ceiling.rank()+1]...)
}

// ModeHelp lists the mode ids with their labels (flag help and errors).
func ModeHelp() string {
	parts := make([]string, len(Modes))
	for i, m := range Modes {
		parts[i] = fmt.Sprintf("%s (%s)", m, m.Label())
	}
	return strings.Join(parts, ", ")
}

// DefaultStartMode is the mode of a session started without one: ask for
// the local daemon, full_auto in the session-bound sandbox (each is then
// clamped to the ceiling).
func DefaultStartMode(sandbox bool) Mode {
	if sandbox {
		return ModeFullAuto
	}
	return ModeAsk
}

// ─── harness mapping ────────────────────────────────────────────────────────

// claudePermissionMode is Claude Code's --permission-mode for a mode. Never
// auto or dontAsk: plan keeps the model planning, and every edit or
// writing command still reaches can_use_tool, where the backstop denies it.
func claudePermissionMode(m Mode) string {
	switch m {
	case ModeAsk:
		return "default"
	case ModeAutoEdits:
		return "acceptEdits"
	case ModeFullAuto:
		return "bypassPermissions"
	}
	return "plan"
}

// codexApprovalPolicy is Codex's approvalPolicy for a mode (thread/start,
// thread/resume and every turn/start). auto_edits is untrusted like ask
// (Codex asks for every file change and every command no exec policy rule
// allows; codex-rs rust-v0.160.0 core/src/safety.rs assess_patch_safety and
// core/src/exec_policy.rs render_decision_for_unmatched_command_for_platform);
// the daemon then accepts file changes inside the session directory
// (codexFileInCwd). Never on-request: it runs sandboxed commands unasked.
func codexApprovalPolicy(m Mode) string {
	switch m {
	case ModeAsk, ModeAutoEdits:
		return "untrusted"
	}
	return "never"
}

// codexSandboxMode is thread/start's sandbox (a kebab string).
func codexSandboxMode(m Mode) string {
	switch m {
	case ModeAsk, ModeAutoEdits:
		return "workspace-write"
	case ModeFullAuto:
		return "danger-full-access"
	}
	return "read-only"
}

// codexSandboxType is the SandboxPolicy type Codex reports for a mode.
func codexSandboxType(m Mode) string {
	switch m {
	case ModeAsk, ModeAutoEdits:
		return "workspaceWrite"
	case ModeFullAuto:
		return "dangerFullAccess"
	}
	return "readOnly"
}

// codexSandboxPolicy is turn/start's sandboxPolicy (a tagged object). The
// workspaceWrite isolation fields equal the -c sandbox_workspace_write
// overrides: the session cwd only, no network, not /tmp or $TMPDIR.
func codexSandboxPolicy(m Mode) map[string]any {
	switch codexSandboxType(m) {
	case "workspaceWrite":
		return map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "networkAccess": false,
			"excludeSlashTmp": true, "excludeTmpdirEnvVar": true}
	case "dangerFullAccess":
		return map[string]any{"type": "dangerFullAccess"}
	}
	return map[string]any{"type": "readOnly", "networkAccess": false}
}

// ─── approval backstop ──────────────────────────────────────────────────────

// verdict is the daemon's answer to one harness approval request.
type verdict int

const (
	verdictCard  verdict = iota // forward to the relay as an approval card
	verdictAllow                // allow here, no card
	verdictDeny                 // deny here, no card
)

// readOnlyDenied is the message a read_only session's denials carry.
const readOnlyDenied = "Denied: Read only mode lets reads and research run and denies anything that would change something."

// backstop decides an approval request before any session remembered
// rule: Mosaic read tools are allowed in every mode; anything else is
// denied in read_only and shown as a card in every other mode, full_auto
// included (a prompt the harness still raises there, such as Claude Code's
// bypass immune critical path rm, is never silently allowed). Codex
// auto_edits file changes inside the session directory are accepted apart
// (codexFileInCwd).
func backstop(m Mode, mosaicRead bool) verdict {
	switch {
	case mosaicRead:
		return verdictAllow
	case m == ModeReadOnly:
		return verdictDeny
	}
	return verdictCard
}

// claudeMosaicRead reports a Claude Code tool name of an archivist read tool.
func claudeMosaicRead(toolName string) bool {
	name, ok := strings.CutPrefix(toolName, "mcp__archivist__")
	return ok && taskscope.IsReadTool(name)
}

// ─── model catalogue ────────────────────────────────────────────────────────

// modelEntry is one model of the controls report.
type modelEntry struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
	Default       bool     `json:"default,omitempty"`
}

// claudeEfforts are the --effort levels Claude Code accepts.
var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// claudeModels are the documented Claude Code aliases a session may pick
// (Haiku has no effort levels; Fable 5 and 5.1 offer low to max, per
// code.claude.com/docs/en/model-config). "default" is the account's default
// model. Not offered: best, opusplan and the [1m] variants (Opus 5.5 and
// Sonnet 5.x are natively 1M).
var claudeModels = []modelEntry{
	{ID: "default", Label: "Default", Efforts: claudeEfforts},
	{ID: "opus", Label: "Opus", Efforts: claudeEfforts},
	{ID: "sonnet", Label: "Sonnet", Efforts: claudeEfforts},
	{ID: "haiku", Label: "Haiku", Efforts: []string{}},
	{ID: "fable", Label: "Fable", Efforts: claudeEfforts},
}

// Bounded charsets for relay supplied ids (the catalogue decides the rest).
// The relay, chat-api, the sandbox Worker and mosaic-event/3
// data-session-controls repeat these two patterns.
var (
	modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$`)
	effortRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// ValidModelID reports a model id within the bounded charset (connect
// --session --model; the catalogue check follows at the start).
func ValidModelID(s string) bool { return modelIDRe.MatchString(s) }

// ValidEffortID reports an effort id within the bounded charset (connect
// --session --effort; the catalogue check follows at the start).
func ValidEffortID(s string) bool { return effortRe.MatchString(s) }

func findModel(list []modelEntry, id string) (modelEntry, bool) {
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return modelEntry{}, false
}

// claudeCatalogue is the Claude aliases with the machine flag's alias
// marked default ("default" without a flag; none for a full model id, which
// no entry names).
func (d *Daemon) claudeCatalogue() []modelEntry {
	def := "default"
	if d.claude.Model != "" {
		def = d.claude.Model
	}
	out := make([]modelEntry, len(claudeModels))
	for i, m := range claudeModels {
		m.Efforts = append([]string{}, m.Efforts...)
		m.Default = m.ID == def
		out[i] = m
	}
	return out
}

// claudeEffortsFor are the efforts a Claude model offers: an alias's own,
// else (a --claude-model full name) every level Claude Code accepts.
func claudeEffortsFor(model string) []string {
	if model == "" {
		model = "default"
	}
	if m, ok := findModel(claudeModels, model); ok {
		return m.Efforts
	}
	return claudeEfforts
}

// startProblem checks a start's model and effort against this daemon's
// catalogue; "" when they may run. A Codex model or effort waits for the
// model list probe (bounded by its timeout).
func (d *Daemon) startProblem(ctx context.Context, in *Inbound) string {
	if in.Model == "" && in.Effort == "" {
		return ""
	}
	if in.Agent == "claude" {
		if in.Model != "" {
			if _, ok := findModel(claudeModels, in.Model); !ok {
				return fmt.Sprintf("The model %q is not offered for Claude Code here.", in.Model)
			}
		}
		model := in.Model
		if model == "" {
			model = d.claude.Model
		}
		if in.Effort != "" && !slices.Contains(claudeEffortsFor(model), in.Effort) {
			return fmt.Sprintf("The effort %q is not offered for the Claude model %q.", in.Effort, orDefault(model))
		}
		return ""
	}
	cat := d.codexModels(ctx)
	if in.Model != "" {
		if len(cat) == 0 {
			return fmt.Sprintf("The model %q cannot be checked: the Codex model list is unavailable here.", in.Model)
		}
		if _, ok := findModel(cat, in.Model); !ok {
			return fmt.Sprintf("The model %q is not in the Codex model list here.", in.Model)
		}
	}
	if in.Effort != "" {
		m, ok := codexModelFor(cat, in.Model, d.codex.Model)
		if !ok {
			return fmt.Sprintf("The effort %q cannot be checked: the Codex model is not in the model list here.", in.Effort)
		}
		if !slices.Contains(m.Efforts, in.Effort) {
			return fmt.Sprintf("The effort %q is not offered for the Codex model %q.", in.Effort, m.ID)
		}
	}
	return ""
}

func orDefault(model string) string {
	if model == "" {
		return "default"
	}
	return model
}

// codexModelFor is the catalogue entry of the model a session runs: its
// own, else the machine flag's, else the catalogue default.
func codexModelFor(cat []modelEntry, model, machine string) (modelEntry, bool) {
	for _, id := range []string{model, machine} {
		if id != "" {
			return findModel(cat, id)
		}
	}
	for _, m := range cat {
		if m.Default {
			return m, true
		}
	}
	return modelEntry{}, false
}

// codexProbeTimeout bounds the model list probe (a var so tests can shorten it).
var codexProbeTimeout = 20 * time.Second

// startCodexProbe runs the Codex model list probe once, in the background:
// an app-server in a throwaway home (no login linked) answering model/list.
// A failure leaves the catalogue empty.
func (d *Daemon) startCodexProbe(ctx context.Context) {
	if d.codex.Bin == "" {
		return
	}
	d.probeOnce.Do(func() {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer close(d.codexProbed)
			pctx, cancel := context.WithTimeout(ctx, codexProbeTimeout)
			defer cancel()
			models, err := d.probeCodexModels(pctx)
			if err != nil {
				d.log.Printf("codex model list unavailable (session model choice off for Codex): %v", err)
				return
			}
			d.mu.Lock()
			d.codexCat = models
			d.mu.Unlock()
			ids := make([]string, len(models))
			for i, m := range models {
				ids[i] = m.ID
			}
			d.log.Printf("codex %s model list: %s", d.codex.Version, strings.Join(ids, ", "))
		}()
	})
}

// codexModels is the probed Codex catalogue, waiting for the probe (it is
// bounded by codexProbeTimeout) unless ctx ends first.
func (d *Daemon) codexModels(ctx context.Context) []modelEntry {
	if d.codex.Bin == "" {
		return nil
	}
	d.startCodexProbe(ctx)
	select {
	case <-d.codexProbed:
	case <-ctx.Done():
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.codexCat
}

// probeCodexModels asks one short-lived app-server for model/list.
func (d *Daemon) probeCodexModels(ctx context.Context) ([]modelEntry, error) {
	home, err := os.MkdirTemp(d.store.Dir(), "codex-probe-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	env, err := BuildChildEnv(d.environ(), nil)
	if err != nil {
		return nil, err
	}
	env = append(env, "CODEX_HOME="+home)
	proc, err := StartProc(ProcSpec{Bin: d.codex.Bin, Args: codexLoginArgs(), Env: env, Dir: home, Log: d.log})
	if err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}
	rpc := newCodexRPC(proc, d.log)
	defer func() {
		rpc.close()
		proc.Stop(stopGrace)
	}()
	var init codexInitializeResult
	if err := rpc.conn.Call(ctx, "initialize", codexInitializeParams{
		ClientInfo:   codexClientInfo{Name: "archivist_connect", Title: "archivist connect", Version: d.appVersion},
		Capabilities: codexCapabilities{ExperimentalAPI: true},
	}, &init); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if !samePath(init.CodexHome, home) {
		return nil, fmt.Errorf("the probe app-server runs with home %q, not its own", init.CodexHome)
	}
	if err := rpc.conn.Notify(ctx, "initialized", nil); err != nil {
		return nil, fmt.Errorf("initialized: %w", err)
	}
	var out []modelEntry
	var cursor *string
	for range 10 {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		var list codexModelList
		if err := rpc.conn.Call(ctx, "model/list", params, &list); err != nil {
			return nil, fmt.Errorf("model/list: %w", err)
		}
		for _, m := range list.Data {
			if e, ok := codexEntry(m); ok && len(out) < 64 {
				if _, dup := findModel(out, e.ID); !dup {
					out = append(out, e)
				}
			}
		}
		if list.NextCursor == nil || *list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	if len(out) == 0 {
		return nil, errors.New("model/list returned no usable model")
	}
	return out, nil
}

// codexEntry turns one model/list entry into a catalogue entry; hidden
// models and ids outside the bounded charset are left out.
func codexEntry(m codexModel) (modelEntry, bool) {
	if m.Hidden || !modelIDRe.MatchString(m.ID) {
		return modelEntry{}, false
	}
	e := modelEntry{ID: m.ID, Label: cleanLabel(m.DisplayName, m.ID), Efforts: []string{}, Default: m.IsDefault}
	for _, o := range m.SupportedReasoningEfforts {
		if effortRe.MatchString(o.ReasoningEffort) && !slices.Contains(e.Efforts, o.ReasoningEffort) && len(e.Efforts) < 16 {
			e.Efforts = append(e.Efforts, o.ReasoningEffort)
		}
	}
	if m.DefaultReasoningEffort != nil && slices.Contains(e.Efforts, *m.DefaultReasoningEffort) {
		e.DefaultEffort = *m.DefaultReasoningEffort
	}
	return e, true
}

// cleanLabel is a display name without control characters, at most 64
// characters, or fallback.
func cleanLabel(s, fallback string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > 64 {
		s = strings.TrimSpace(string(r[:64]))
	}
	if s == "" {
		return fallback
	}
	return s
}

// ─── controls report ────────────────────────────────────────────────────────

// agentControls is one agent of the controls report.
type agentControls struct {
	Agent  string       `json:"agent"`
	Models []modelEntry `json:"models"`
}

// defaultMode is the clamped mode of a session started without one.
func (d *Daemon) defaultMode() Mode { return ClampMode(DefaultStartMode(d.sandbox), d.maxMode) }

// controls builds the controls report: the ceiling, the default mode and
// the models and efforts of each harness this daemon drives.
func (d *Daemon) controls(ctx context.Context) []byte {
	agents := []agentControls{}
	if d.agentUsable("claude") {
		agents = append(agents, agentControls{Agent: "claude", Models: d.claudeCatalogue()})
	}
	if d.agentUsable("codex") {
		models := []modelEntry{}
		cat := d.codexModels(ctx)
		for _, m := range cat {
			// The machine flag's model is the default; Codex's own default
			// only without a flag; none when the flag names no entry.
			if d.codex.Model != "" {
				m.Default = m.ID == d.codex.Model
			}
			m.Efforts = append([]string{}, m.Efforts...)
			models = append(models, m)
		}
		agents = append(agents, agentControls{Agent: "codex", Models: oneDefault(models)})
	}
	frame, dropped := trimControls(d.maxMode, d.defaultMode(), agents)
	if dropped > 0 {
		d.log.Printf("controls report: %d Codex models left out to fit the relay's %d byte limit", dropped, maxControlsBytes)
	}
	return frame
}

// oneDefault keeps default only on the first entry that has it: the relay
// refuses a whole report with two defaults for one agent, and model/list
// could mark more than one.
func oneDefault(models []modelEntry) []modelEntry {
	seen := false
	for i := range models {
		if models[i].Default && seen {
			models[i].Default = false
		}
		seen = seen || models[i].Default
	}
	return models
}

// maxControlsBytes is the relay's limit for a controls frame as JSON. The
// relay stores the report on its socket attachment, which Cloudflare caps at
// 16 KiB under structured clone (a string with one character above Latin-1
// takes 2 bytes per UTF-16 unit), so the cap leaves room; a real report is
// about 2 KB.
const maxControlsBytes = 6144

// trimControls is the controls frame at most maxControlsBytes long: Codex
// catalogue entries are dropped from the end, the default entry kept, until
// it fits. It returns how many entries were dropped. Go escapes at least as
// much as JavaScript's JSON.stringify, so the relay's JSON measure is never
// larger; the relay can still refuse a report (for example when storing it
// fails), and then keeps no controls for this daemon (fail closed).
func trimControls(maxMode, defaultMode Mode, agents []agentControls) ([]byte, int) {
	agents = slices.Clone(agents)
	dropped := 0
	for {
		frame := controlsFrame(maxMode, defaultMode, agents)
		if len(frame) <= maxControlsBytes {
			return frame, dropped
		}
		i := slices.IndexFunc(agents, func(a agentControls) bool { return a.Agent == "codex" })
		if i < 0 {
			return frame, dropped
		}
		models := agents[i].Models
		last := -1
		for j := len(models) - 1; j >= 0; j-- {
			if !models[j].Default {
				last = j
				break
			}
		}
		if last < 0 {
			return frame, dropped // only the default is left; Claude's list is fixed and small
		}
		agents[i].Models = slices.Delete(slices.Clone(models), last, last+1)
		dropped++
	}
}

// sendControls reports the controls after the capabilities. An older relay
// answers INVALID_FRAME (logged once, onError); the capabilities stand.
func (d *Daemon) sendControls(ctx context.Context, l *live) {
	frame := d.controls(ctx)
	sent := time.Now().UnixNano()
	d.controlsSentAt.Store(sent)
	if !l.Send(frame) {
		d.controlsSentAt.CompareAndSwap(sent, 0)
		d.log.Printf("controls not sent: the user socket closed")
	}
}

// controlsRefusalWindow bounds how long after a controls send an
// INVALID_FRAME counts as its refusal (a var so tests can shorten it).
var controlsRefusalWindow = 10 * time.Second

// relayRefusedControls handles a user socket relay error; true when it is
// the first INVALID_FRAME within controlsRefusalWindow of a controls send
// (an older relay refusing it, logged only once). A relay that accepts the
// frame answers nothing, so every later error takes the usual path.
func (d *Daemon) relayRefusedControls(code string) bool {
	if code != "INVALID_FRAME" {
		return false
	}
	sent := d.controlsSentAt.Load()
	if sent == 0 || time.Since(time.Unix(0, sent)) > controlsRefusalWindow || !d.controlsSentAt.CompareAndSwap(sent, 0) {
		return false
	}
	if d.controlsRefused.CompareAndSwap(false, true) {
		d.log.Printf("the relay refused the controls frame (an older relay): plain capabilities kept, session controls unavailable")
	}
	return true
}

// ─── per session ────────────────────────────────────────────────────────────

// reclamp lowers a stored session mode to the current ceiling (a resumed
// session after the ceiling was lowered); true when the record changed.
func (d *Daemon) reclamp(rec *SessionRecord) bool {
	m, ok := ParseMode(rec.Mode)
	if !ok {
		m = d.defaultMode()
	}
	m = ClampMode(m, d.maxMode)
	if string(m) == rec.Mode {
		return false
	}
	rec.Mode = string(m)
	return true
}

// startMode is a new session's mode: the requested one or the default,
// clamped to the ceiling.
func (d *Daemon) startMode(in *Inbound) Mode {
	m, ok := ParseMode(in.Mode)
	if !ok {
		m = d.defaultMode()
	}
	if c := ClampMode(m, d.maxMode); c != m {
		d.log.Printf("start_session %s: mode %s clamped to this machine's ceiling %s", in.CorrelationID, m, c)
		m = c
	}
	return m
}

// mode is the session's current mode, clamped to the ceiling.
func (s *session) mode() Mode {
	m, ok := ParseMode(s.rec.Mode)
	if !ok {
		m = s.d.defaultMode()
	}
	return ClampMode(m, s.d.maxMode)
}

// claudeLaunchConfig is the Claude adapter config for this session's next
// process: its mode, and its model and effort over the machine flags (a
// machine effort the session's model does not offer is left out).
func (s *session) claudeLaunchConfig() ClaudeConfig {
	cfg := s.d.claude
	cfg.PermissionMode = claudePermissionMode(s.mode())
	cfg.AllowBypass = s.d.maxMode == ModeFullAuto
	cfg.Model, cfg.Effort = s.claudeModelEffort()
	cfg.WebSearch = s.d.webSearch
	if cfg.AllowBypass && os.Geteuid() == 0 {
		s.log.Printf("warning: running as root with the full_auto ceiling: Claude Code refuses bypass permissions as root outside a recognised sandbox, and the session then fails")
	}
	return cfg
}

// codexModelEffort is the model and effort this session asks Codex for
// ("" model: resolve model/list's default at handshake). The catalogue is
// read only when a session model meets a machine effort.
func (s *session) codexModelEffort(ctx context.Context) (model, effort string) {
	model, effort = s.d.codex.Model, s.d.codex.Effort
	if s.rec.Model != "" {
		model = s.rec.Model
		if s.rec.Effort == "" && effort != "" {
			if m, ok := findModel(s.d.codexModels(ctx), model); !ok || !slices.Contains(m.Efforts, effort) {
				effort = ""
			}
		}
	}
	if s.rec.Effort != "" {
		effort = s.rec.Effort
	}
	return model, effort
}

// claudeModelEffort is the model and effort of this session's Claude Code:
// its own over the machine flags ("" = Claude Code's default); a machine
// effort the session's model does not offer is left out.
func (s *session) claudeModelEffort() (model, effort string) {
	model, effort = s.d.claude.Model, s.d.claude.Effort
	if s.rec.Model != "" {
		model = s.rec.Model
		if s.rec.Effort == "" && effort != "" && !slices.Contains(claudeEffortsFor(model), effort) {
			effort = ""
		}
	}
	if s.rec.Effort != "" {
		effort = s.rec.Effort
	}
	return model, effort
}

// emitControls sends data-session-controls (mosaic-event/3): the session's
// mode, this machine's ceiling, and its model and effort when set (Claude:
// the session's choice, else the machine flag; Codex: what the handshake
// negotiated, model/list's default included). Codex applies a mode change
// at its next turn/start, so a running Codex turn keeps its policy while
// the event already reports the new mode. It is sent when the
// session starts running and after every set_mode outcome, so a surface
// learns the session's mode, refusals included. Nothing reaches
// the relay before the init proof passes: while the session is not running
// (parked, spawning, stopped) it is a no-op, and the running transition
// reports the mode then.
func (s *session) emitControls() {
	if !s.running {
		return
	}
	data := map[string]any{"mode": string(s.mode()), "maxMode": string(s.d.maxMode)}
	var model, effort string
	if s.isCodex() {
		if s.cx != nil { // always while running
			model, effort = s.cx.model, s.cx.effort
		}
	} else {
		model, effort = s.claudeModelEffort()
	}
	if model != "" && modelIDRe.MatchString(model) {
		data["model"] = model
	}
	if effort != "" && effortRe.MatchString(effort) {
		data["effort"] = effort
	}
	s.flushCoalesced()
	s.outbox.Emit(Chunk{"type": "data-session-controls", "data": data})
}

// claudeWebSearch reports Claude Code's WebSearch on a session with web
// search (Story 78.33): a read, allowed like the Mosaic read tools in every
// mode. With web search off it is not a read here and the mode decides.
func (s *session) claudeWebSearch(toolName string) bool {
	return s.d.webSearch && toolName == ClaudeWebSearchTool
}

// claudeBackstop answers a can_use_tool request the mode decides (Mosaic
// read tools, WebSearch with web search on, read_only) without a card;
// true when answered.
func (s *session) claudeBackstop(requestID string, req controlRequest) bool {
	var frame map[string]any
	switch backstop(s.mode(), claudeMosaicRead(req.ToolName) || s.claudeWebSearch(req.ToolName)) {
	case verdictAllow:
		frame = allowFrame(requestID, req.Input)
	case verdictDeny:
		frame = denyFrame(requestID, readOnlyDenied)
	default:
		return false
	}
	if err := s.proc.WriteJSON(frame); err != nil {
		s.log.Printf("write to claude failed: %v", err)
	}
	return true
}

// codexBackstop answers a command, file or MCP approval the mode decides
// without a card; true when answered. Codex never asks for Mosaic read
// tools (their approval mode is approve), so an MCP elicitation is never
// treated as one. inCwd marks a file change inside the session directory
// (codexFileInCwd), which auto_edits accepts.
func (s *session) codexBackstop(reply codexReplyFunc, mcp, inCwd bool) bool {
	m := s.mode()
	switch {
	case m == ModeAutoEdits && inCwd && !mcp:
		reply(map[string]any{"decision": "accept"})
		return true
	case backstop(m, false) != verdictDeny:
		return false
	case mcp:
		reply(map[string]any{"action": "decline", "content": nil, "_meta": nil})
	default:
		reply(map[string]any{"decision": "decline"})
	}
	return true
}

// codexReplyFunc answers the server request at hand.
type codexReplyFunc func(result any)

// codexFileInCwd reports an item/fileChange/requestApproval an auto_edits
// session accepts without a card: no grantRoot, the matching fileChange item
// known (files: item id -> changes) and every change path, rename targets
// included, inside the session directory.
func codexFileInCwd(params json.RawMessage, files map[string]json.RawMessage, cwd string) bool {
	var p struct {
		ItemID    string  `json:"itemId"`
		GrantRoot *string `json:"grantRoot"`
	}
	if json.Unmarshal(params, &p) != nil || (p.GrantRoot != nil && *p.GrantRoot != "") {
		return false
	}
	raw, ok := files[p.ItemID]
	if !ok {
		return false
	}
	var changes []struct {
		Path string `json:"path"`
		Kind struct {
			MovePath *string `json:"move_path"`
		} `json:"kind"`
	}
	if json.Unmarshal(raw, &changes) != nil || len(changes) == 0 {
		return false
	}
	for _, c := range changes {
		if !pathInside(c.Path, cwd) {
			return false
		}
		if mp := c.Kind.MovePath; mp != nil && !pathInside(*mp, cwd) {
			return false
		}
	}
	return true
}

// pathInside reports an absolute path strictly inside dir after cleaning
// and resolving symbolic links through the nearest existing ancestor (the
// path itself when it exists), so a link out of dir counts as outside.
func pathInside(path, dir string) bool {
	if path == "" || !filepath.IsAbs(path) || dir == "" {
		return false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	path = filepath.Clean(path)
	existing, rest := path, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false // unreadable: never assume it is absent
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return false
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Join(resolved, rest))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// setMode applies a set_mode command. Above the ceiling it is refused and
// nothing changes. Claude with a live process: set_permission_mode is sent
// and the mode commits on its success response. Codex: the record and the
// backstop change now, Codex at the next turn/start. A parked or not yet
// running session: the record changes and the next spawn applies it.
func (s *session) setMode(raw string) {
	m, ok := ParseMode(raw)
	if !ok {
		s.log.Printf("set_mode %q refused: not a mode", raw)
		s.emitControls()
		return
	}
	if m.rank() > s.d.maxMode.rank() {
		s.log.Printf("set_mode %s refused: above this machine's ceiling %s; mode stays %s", m, s.d.maxMode, s.mode())
		s.emitControls()
		return
	}
	if m == s.mode() && len(s.modeReqs) == 0 {
		s.emitControls() // unchanged: confirm the mode
		return
	}
	if s.isCodex() || s.proc == nil {
		s.commitMode(m)
		s.emitControls()
		return
	}
	s.modeSeq++
	id := fmt.Sprintf("archivist-mode-%s-%d", s.outbox.RunID(), s.modeSeq)
	if err := s.proc.WriteJSON(setModeFrame(id, claudePermissionMode(m))); err != nil {
		// Claude is exiting: keep the acked change, the next spawn applies it.
		s.log.Printf("set_permission_mode write failed (%v); recording %s for the next start", err, m)
		s.commitMode(m)
		s.emitControls()
		return
	}
	s.modeReqs[id] = m
	s.modeLast = m
}

// commitMode records a mode. Lowering to read_only also denies every
// pending approval card the way a relay deny does; other changes leave
// pending cards as they are.
func (s *session) commitMode(m Mode) {
	prev := s.mode()
	s.rec.Mode = string(m)
	s.save()
	s.log.Printf("permission mode %s -> %s", prev, m)
	if m == ModeReadOnly {
		s.denyPending(readOnlyDenied)
	}
}

// denyPending answers every open approval card deny (Claude, with msg) or
// decline (Codex; MCP elicitations decline) through the relay answer path,
// so each card is reported resolved. Lowering to read_only and stopping the
// daemon (Story 78.37) use it.
func (s *session) denyPending(msg string) {
	if s.isCodex() {
		if s.cx == nil {
			return
		}
		for id := range s.cx.pending {
			s.codexAnswerApproval(&Inbound{ApprovalID: id, Decision: "deny", Reason: "user"})
		}
		return
	}
	for id := range s.pending {
		delete(s.pending, id)
		s.write(denyFrame(id, msg))
	}
}

// commitPendingMode records the newest set_permission_mode a stopping
// Claude process never answered, so the next spawn applies it (as for a
// parked session), and reports the outcome, changed or not. The stop path
// calls it after the session left running, so that report is a no-op there
// and the next running transition carries the committed mode.
func (s *session) commitPendingMode() {
	if len(s.modeReqs) == 0 {
		return
	}
	s.modeReqs = map[string]Mode{}
	if m := ClampMode(s.modeLast, s.d.maxMode); m != s.mode() {
		s.commitMode(m)
	}
	s.emitControls()
}

// claudeModeResponse handles Claude's answer to our set_permission_mode;
// true when the frame was one.
func (s *session) claudeModeResponse(line []byte) bool {
	var f struct {
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
			Response  struct {
				Mode string `json:"mode"`
			} `json:"response"`
		} `json:"response"`
	}
	if json.Unmarshal(line, &f) != nil {
		return false
	}
	m, ok := s.modeReqs[f.Response.RequestID]
	if !ok {
		return false
	}
	delete(s.modeReqs, f.Response.RequestID)
	want := claudePermissionMode(m)
	switch {
	case f.Response.Subtype != "success":
		s.log.Printf("Claude Code refused permission mode %s (%s): %s; mode stays %s", want, m, f.Response.Error, s.mode())
	case f.Response.Response.Mode != "" && f.Response.Response.Mode != want:
		s.log.Printf("Claude Code reports permission mode %q after asking for %s; mode stays %s", f.Response.Response.Mode, want, s.mode())
	default:
		s.commitMode(m)
	}
	s.emitControls() // committed or refused: report the mode that runs
	return true
}

// claudeExpectedModes are the permissionMode values an init frame may
// report: the session's mode, or one a pending set_permission_mode asked for.
func (s *session) claudeExpectedModes() []string {
	out := []string{claudePermissionMode(s.mode())}
	for _, m := range s.modeReqs {
		out = append(out, claudePermissionMode(m))
	}
	return out
}

// ─── context usage ──────────────────────────────────────────────────────────

// contextUsage is the harness reported context use of a session: tokens in
// the context after the last model call and the model's window (0 =
// unknown, never guessed). withContext puts it on data-usage
// (mosaic-event/3).
type contextUsage struct {
	Tokens int64
	Window int64
	Model  string
}

// withContext returns a copy of a data-usage chunk carrying contextTokens
// and contextWindow, each only when known (> 0); any other chunk is
// returned as it is.
func withContext(c Chunk, u contextUsage) Chunk {
	if c["type"] != "data-usage" || (u.Tokens <= 0 && u.Window <= 0) {
		return c
	}
	data, ok := c["data"].(map[string]any)
	if !ok {
		return c
	}
	out := make(Chunk, len(c))
	for k, v := range c {
		out[k] = v
	}
	d := make(map[string]any, len(data)+2)
	for k, v := range data {
		d[k] = v
	}
	if u.Tokens > 0 {
		d["contextTokens"] = u.Tokens
	}
	if u.Window > 0 {
		d["contextWindow"] = u.Window
	}
	out["data"] = d
	return out
}

func (u contextUsage) String() string {
	w := "unknown"
	if u.Window > 0 {
		w = fmt.Sprint(u.Window)
	}
	return fmt.Sprintf("context %d tokens of window %s (model %s)", u.Tokens, w, orDefault(u.Model))
}

// claudeAssistantContext reads the context use of a main loop assistant
// frame: input + cache read + cache creation + output tokens.
func claudeAssistantContext(f map[string]any) (tokens int64, model string, ok bool) {
	if f["parent_tool_use_id"] != nil {
		return 0, "", false
	}
	m, _ := f["message"].(map[string]any)
	u, isMap := m["usage"].(map[string]any)
	if !isMap {
		return 0, "", false
	}
	for _, k := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"} {
		tokens += intOf64(u[k])
	}
	model, _ = m["model"].(string)
	return tokens, model, true
}

// claudeContextWindow reads result.modelUsage[model].contextWindow (the
// running total's latest), or the only entry's when model is not listed.
func claudeContextWindow(f map[string]any, model string) (int64, string) {
	mu, _ := f["modelUsage"].(map[string]any)
	entry, ok := mu[model].(map[string]any)
	if !ok && len(mu) == 1 {
		for k, v := range mu {
			model = k
			entry, _ = v.(map[string]any)
		}
	}
	return intOf64(entry["contextWindow"]), model
}

// codexContextUsage reads thread/tokenUsage/updated: tokenUsage.last
// totalTokens and modelContextWindow (null stays unknown).
func codexContextUsage(params json.RawMessage) (contextUsage, bool) {
	var p struct {
		TokenUsage *struct {
			Last *struct {
				TotalTokens int64 `json:"totalTokens"`
			} `json:"last"`
			ModelContextWindow *int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &p) != nil || p.TokenUsage == nil || p.TokenUsage.Last == nil {
		return contextUsage{}, false
	}
	u := contextUsage{Tokens: p.TokenUsage.Last.TotalTokens}
	if w := p.TokenUsage.ModelContextWindow; w != nil && *w > 0 {
		u.Window = *w
	}
	return u, true
}
