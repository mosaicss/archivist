package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/fsutil"
	"github.com/mosaicss/archivist/internal/guidance"
	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// DefaultMaxSessions caps concurrently running harness processes.
const DefaultMaxSessions = 4

// Config assembles a daemon. Only local configuration lives here; the relay
// never supplies any of it.
type Config struct {
	API      API
	RelayURL string
	StateDir string
	// Claude and Codex configure each adapter; an empty Bin means that
	// harness is not usable here and its sessions are refused.
	Claude      ClaudeConfig
	Codex       CodexConfig
	MaxSessions int
	// AppVersion is this archivist's version (Codex clientInfo).
	AppVersion string
	Log        *Logger
	// Detect re-inventories harnesses for each capabilities report.
	Detect func(ctx context.Context) Detection
	// Runner runs `claude auth status` for the per-session proof.
	Runner Runner
	// Environ is the daemon environment the child allowlist reads.
	Environ func() []string
	// TempDir is the parent of per-session working directories ("" = system).
	TempDir string
	// ChatAPIURL is the chat-api base URL data-artifact read urls hang off
	// ("" = ARCHIVIST_BASE_URL or the production endpoint).
	ChatAPIURL string
	// Dial overrides the websocket dialer (tests).
	Dial func(ctx context.Context, u string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	// Guidance fetches Mosaic's research guidance once per spawn (Story
	// 78.31; web asks for the web variant, Story 78.33); nil =
	// guidance.Fetch or guidance.FetchWeb against ChatAPIURL.
	Guidance func(ctx context.Context, surface, form string, web bool) guidance.Text
	// SignIn names the harness ("claude" or "codex") that is installed but
	// logged out (session-bound sandbox mode, RunSession): the session signs
	// it in (posture 1) before its first turn. Its Bin stays configured.
	SignIn string
	// MaxMode is this machine's permission ceiling (Story 78.32, "" =
	// DefaultMaxMode): no session runs above it, whatever the relay sends.
	MaxMode Mode
	// WebSearch turns on each harness's own provider side web search
	// (Story 78.33): Claude Code's WebSearch tool, Codex web_search "live".
	// A machine setting (archivist connect --web-search; on by default with
	// --session), never chosen by the relay. Web search is a read: it never
	// asks in any mode. Claude's WebFetch stays off either way.
	WebSearch bool
	// ActivityFile is the session-bound mode's activity file (Story 78.37,
	// ARCHIVIST_ACTIVITY_FILE; "" = none): since when the session has been
	// quiet and since when an approval has waited, for the sandbox Worker's
	// idle pause. Ignored outside RunSession.
	ActivityFile string
}

// Daemon is a running `archivist connect`.
type Daemon struct {
	api         API
	relayURL    string
	claude      ClaudeConfig
	codex       CodexConfig
	appVersion  string
	reaping     bool // child subreaper on: sessions reap adopted zombies
	maxSessions int
	log         *Logger
	parser      *mosaicevent.Set
	store       *Store
	detect      func(ctx context.Context) Detection
	runner      Runner
	environ     func() []string
	tempDir     string
	chatAPIURL  string
	// fetchGuidance returns the research guidance for a spawn (Story 78.31).
	fetchGuidance func(ctx context.Context, surface, form string, web bool) guidance.Text
	dial          func(ctx context.Context, u string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	// sandbox is the session-bound mode (RunSession, Story 78.22): no user
	// socket, and a failed session ends the daemon.
	sandbox bool
	// maxMode is the permission ceiling (Story 78.32).
	maxMode Mode
	// webSearch is Config.WebSearch (Story 78.33).
	webSearch bool
	// activityFile is Config.ActivityFile (Story 78.37).
	activityFile string
	// Codex model list probe (Story 78.32): codexProbed closes when it ends;
	// codexCat (guarded by mu) is empty after a failure.
	probeOnce       sync.Once
	codexProbed     chan struct{}
	codexCat        []modelEntry
	controlsRefused atomic.Bool
	// controlsSentAt is the last controls send (unix ns, 0 = none pending).
	controlsSentAt atomic.Int64

	mu       sync.Mutex
	signIn   string // harness awaiting its posture-1 sign-in (guarded by mu)
	sessions map[string]*session
	starting map[string]bool
	procs    int             // reserved live-process slots
	closing  bool            // Run is shutting down: no new start dispatch
	tokens   map[string]bool // live task token ids
	wg       sync.WaitGroup
}

// New validates cfg and opens the state store.
func New(cfg Config) (*Daemon, error) {
	if !Supported() {
		return nil, ErrUnsupportedPlatform
	}
	parser, err := mosaicevent.NewSet()
	if err != nil {
		return nil, fmt.Errorf("load the mosaic-event contracts: %w", err)
	}
	store, err := OpenStore(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open state %s: %w", cfg.StateDir, err)
	}
	d := &Daemon{api: cfg.API, relayURL: cfg.RelayURL, claude: cfg.Claude, codex: cfg.Codex, appVersion: cfg.AppVersion,
		maxSessions: cfg.MaxSessions,
		log:         cfg.Log, parser: parser, store: store, detect: cfg.Detect, runner: cfg.Runner,
		environ: cfg.Environ, tempDir: cfg.TempDir, dial: cfg.Dial, signIn: cfg.SignIn, chatAPIURL: cfg.ChatAPIURL,
		sessions: map[string]*session{}, starting: map[string]bool{}, tokens: map[string]bool{},
		maxMode: cfg.MaxMode, webSearch: cfg.WebSearch, activityFile: cfg.ActivityFile, codexProbed: make(chan struct{})}
	if d.maxMode == "" {
		d.maxMode = DefaultMaxMode
	}
	if _, ok := ParseMode(string(d.maxMode)); !ok {
		return nil, fmt.Errorf("permission ceiling %q is not one of %s", d.maxMode, modeIDs())
	}
	if d.maxSessions <= 0 {
		d.maxSessions = DefaultMaxSessions
	}
	if d.runner == nil {
		d.runner = ExecRunner
	}
	if d.environ == nil {
		d.environ = os.Environ
	}
	if d.chatAPIURL == "" {
		d.chatAPIURL = client.ResolveBaseURL()
	}
	d.fetchGuidance = cfg.Guidance
	if d.fetchGuidance == nil {
		d.fetchGuidance = func(ctx context.Context, surface, form string, web bool) guidance.Text {
			if web {
				return guidance.FetchWeb(ctx, d.chatAPIURL, surface, form)
			}
			return guidance.Fetch(ctx, d.chatAPIURL, surface, form)
		}
	}
	if d.appVersion == "" {
		d.appVersion = "dev"
	}
	// With Codex configured, orphaned harness descendants (Codex detaches
	// commands with setsid) are reparented to the daemon (Linux), so the
	// stop sweep can kill and reap them.
	if d.codex.Bin != "" {
		if err := enableSubreaper(); err != nil {
			if d.log != nil {
				d.log.Printf("warning: could not become the child subreaper: %v", err)
			}
		} else {
			d.reaping = true
		}
	}
	return d, nil
}

// MaxMode is the daemon's effective permission ceiling (Story 78.32).
func (d *Daemon) MaxMode() Mode { return d.maxMode }

// WebSearch reports whether this daemon's sessions have provider side web
// search (Story 78.33).
func (d *Daemon) WebSearch() bool { return d.webSearch }

// Run serves until ctx ends (nil), another daemon supersedes this one
// (ErrSuperseded) or a fatal condition (*FatalError). Every child process is
// stopped and every live task token revoked before it returns.
func (d *Daemon) Run(ctx context.Context) error {
	sessCtx, cancelSessions := context.WithCancel(context.Background())
	defer func() {
		d.beginClosing()
		cancelSessions()
		d.wg.Wait()
		SweepAllOrphans()
	}()

	// Reattach sessions that were active when an earlier run stopped; their
	// next user_message resumes Claude from the stored session id. Leftovers
	// of a crashed run are cleaned: every run dir's task token is revoked and
	// the dir removed (no process exists yet; a resume mints a new token),
	// and inactive or expired records lose their cwd.
	if recs, err := d.store.List(); err == nil {
		now := time.Now().UnixMilli()
		for _, rec := range recs {
			if rec.Status == "active" && rec.ExpiresAt <= now {
				rec.Status = "ended"
				_ = d.store.Save(rec)
			}
			d.revokeLeftoverToken(rec.SessionID)
			d.store.RemoveRunDir(rec.SessionID)
			if rec.Status != "active" {
				removeSessionCwd(rec.Cwd)
				d.store.RemoveCodexHome(rec.SessionID)
				continue
			}
			if d.reclamp(rec) {
				_ = d.store.Save(rec) // a lowered ceiling holds for the resumed session
			}
			d.attach(sessCtx, rec, "", false, resumeStart{})
		}
	}

	d.startCodexProbe(sessCtx)
	user := newLink("user socket", UserSocket, "", d.api, d.relayURL, nil, d.log)
	if d.dial != nil {
		user.dial = d.dial
	}
	user.onError = d.relayRefusedControls
	user.onConnect = func(l *live) {
		det := d.detect(ctx)
		caps := det.Capabilities()
		// Available only for a harness this daemon drives (fixed at
		// startup): a later login is reported, not offered.
		for i := range caps {
			caps[i].Available = caps[i].Available && d.agentUsable(caps[i].Agent)
		}
		l.Send(capabilitiesFrame(caps))
		d.log.Printf("capabilities reported: %+v", caps)
		// Then the session controls (Story 78.32), once the Codex model
		// list probe is done (bounded); the capabilities never wait.
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.sendControls(sessCtx, l)
		}()
	}
	user.onCommand = func(l *live, in *Inbound) {
		switch in.Kind {
		case "start_session":
			d.mu.Lock()
			if d.closing {
				d.mu.Unlock()
				// Not acknowledged: the relay redelivers to the next daemon.
				d.log.Printf("start_session %s ignored: shutting down", in.CorrelationID)
				return
			}
			d.wg.Add(1)
			d.mu.Unlock()
			go func() {
				defer d.wg.Done()
				d.handleStart(sessCtx, l, in, "")
			}()
		case "approval_response":
			// Terminal receipt after the session ended: an audit record, never
			// executed (the decision is not applied, nothing restarts).
			d.log.Printf("terminal approval receipt %s for session %s acknowledged, not executed", in.CorrelationID, in.SessionID)
			l.Send(commandAck(UserSocket, in.CorrelationID, in.SessionID))
		}
	}
	err := user.run(ctx)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		err = nil
	}
	d.log.Printf("stopping: ending %d session(s)", d.sessionCount())
	d.beginClosing()
	cancelSessions()
	d.wg.Wait()
	SweepAllOrphans()
	d.revokeLeftovers()
	return err
}

func (d *Daemon) beginClosing() {
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
}

func (d *Daemon) sessionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

// attach starts a session goroutine; prompt is non-empty for a new session,
// slot passes a live-process slot already reserved for it and res is the
// start's resume of an earlier session (Story 78.37).
func (d *Daemon) attach(ctx context.Context, rec *SessionRecord, prompt string, slot bool, res resumeStart) *session {
	s := newSession(d, rec)
	s.slot = slot
	s.resuming = res.ok
	if res.note {
		s.note = resumeMissNote
	}
	d.mu.Lock()
	d.sessions[rec.SessionID] = s
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		s.run(ctx, prompt)
		d.mu.Lock()
		if d.sessions[rec.SessionID] == s {
			delete(d.sessions, rec.SessionID)
		}
		d.mu.Unlock()
	}()
	return s
}

// handleStart validates and starts one session. It acknowledges only after
// the local record is durable (or the refusal is final), so a crash before
// that point lets the relay redeliver. l is nil in the session-bound mode
// (RunSession: no user socket, nothing to acknowledge), where resumeFrom
// may name an earlier session whose conversation the new one continues
// (Story 78.37). It returns the started session, or nil.
func (d *Daemon) handleStart(ctx context.Context, l *live, in *Inbound, resumeFrom string) *session {
	sid := in.SessionID
	ack := func() {
		if l != nil {
			l.Send(commandAck(UserSocket, in.CorrelationID, sid))
		}
	}
	d.mu.Lock()
	if d.starting[sid] {
		d.mu.Unlock()
		return nil // the attempt in progress acknowledges
	}
	if _, running := d.sessions[sid]; running {
		d.mu.Unlock()
		ack()
		return nil
	}
	d.starting[sid] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.starting, sid)
		d.mu.Unlock()
	}()

	if rec, err := d.store.Load(sid); err == nil {
		d.log.Printf("start_session %s redelivered for known session %s (%s); acknowledged only", in.CorrelationID, sid, rec.Status)
		ack()
		return nil
	} else if errors.Is(err, errCorruptRecord) {
		// Refused without execution and reported, so the relay stops redelivering.
		d.refuseStart(ctx, l, in, "The local session record is unreadable; start a new session.", true)
		return nil
	} else if !isNotExist(err) {
		// Transient read failure: no ack, the relay redelivers.
		d.log.Printf("start_session %s: session record unreadable, awaiting redelivery: %v", in.CorrelationID, err)
		return nil
	}
	if !d.agentUsable(in.Agent) {
		d.refuseStart(ctx, l, in, fmt.Sprintf("The %s agent is not available in this archivist connect.", in.Agent), true)
		return nil
	}
	info, err := d.api.GetAgentSession(ctx, sid)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusBadRequest) {
			d.refuseStart(ctx, l, in, "The session is unknown.", false)
			return nil
		}
		// Transient: no ack, the relay redelivers in 30 s.
		d.log.Printf("start_session %s: session lookup failed, awaiting redelivery: %v", in.CorrelationID, err)
		return nil
	}
	now := time.Now().UnixMilli()
	if info.SessionID != sid || info.Status != "active" || info.ExpiresAt <= now {
		d.refuseStart(ctx, l, in, "The session is not active.", false)
		return nil
	}
	if info.Agent != in.Agent {
		d.refuseStart(ctx, l, in, "The session belongs to another agent.", true)
		return nil
	}
	if problem := d.startProblem(ctx, in); problem != "" {
		d.refuseStart(ctx, l, in, problem, true)
		return nil
	}
	if !d.acquireSlot(nil) {
		d.refuseStart(ctx, l, in, fmt.Sprintf("archivist connect already runs %d live sessions.", d.maxSessions), true)
		return nil
	}
	// Story 78.37: a sandbox start may continue an earlier session's
	// conversation (its cwd path and harness id); an unusable one starts
	// fresh with the note.
	var res resumeStart
	var src *SessionRecord
	if resumeFrom != "" && l == nil && resumeFrom != sid {
		var why string
		if src, why = d.resumeSource(resumeFrom, in.Agent); src == nil {
			d.log.Printf("session %s cannot be resumed (%s); starting fresh", resumeFrom, why)
		} else if in.Agent == "codex" {
			if err := d.takeCodexHome(resumeFrom, sid); err != nil {
				d.log.Printf("session %s cannot be resumed (Codex home not moved: %v); starting fresh", resumeFrom, err)
				src = nil
			}
		}
		res = resumeStart{ok: src != nil, note: src == nil}
	}
	var cwd string
	if src != nil {
		cwd = src.Cwd
	} else {
		var err error
		if cwd, err = d.makeCwd(); err != nil {
			d.releaseSlot()
			d.log.Printf("start_session %s: working directory: %v", in.CorrelationID, err)
			return nil
		}
	}
	rec := &SessionRecord{SessionID: sid, Agent: in.Agent, StartCorrelationID: in.CorrelationID, Cwd: cwd,
		ExpiresAt: info.ExpiresAt, Status: "active", CreatedAt: now,
		Mode: string(d.startMode(in)), Model: in.Model, Effort: in.Effort}
	if src != nil {
		rec.ClaudeSessionID, rec.CodexThreadID = src.ClaudeSessionID, src.CodexThreadID
	}
	if err := d.store.Save(rec); err != nil {
		d.releaseSlot()
		if src == nil {
			_ = os.RemoveAll(cwd)
		} else if in.Agent == "codex" {
			d.giveBackCodexHome(resumeFrom, sid)
		}
		d.log.Printf("start_session %s: session record not saved: %v", in.CorrelationID, err)
		return nil
	}
	if src != nil {
		d.retire(src, sid)
	}
	ack()
	d.log.Printf("session %s starting (cwd %s, mode %s, ceiling %s)", sid, cwd, rec.Mode, d.maxMode)
	return d.attach(ctx, rec, in.Prompt, true, res)
}

// refuseStart acknowledges a start that will not run. When the session is
// active a session socket is possible, so the refusal is also reported as
// a failed status.
func (d *Daemon) refuseStart(ctx context.Context, l *live, in *Inbound, reason string, report bool) {
	d.log.Printf("start_session %s refused: %s", in.CorrelationID, reason)
	rec := &SessionRecord{SessionID: in.SessionID, Agent: in.Agent, StartCorrelationID: in.CorrelationID,
		Status: "failed", CreatedAt: time.Now().UnixMilli()}
	if err := d.store.Save(rec); err != nil {
		d.log.Printf("refusal record not saved: %v", err)
	}
	if l != nil {
		l.Send(commandAck(UserSocket, in.CorrelationID, in.SessionID))
	}
	if !report {
		return
	}
	s := newSession(d, rec)
	s.emitError(reason)
	s.emitStatus("failed", reason)
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	linkErr := make(chan error, 1)
	go func() { linkErr <- s.link.run(rctx) }()
	drained := make(chan struct{})
	go func() {
		s.outbox.WaitDrained(rctx)
		close(drained)
	}()
	select {
	case <-drained:
	case <-linkErr:
	}
	cancel()
	close(s.done)
}

// agentUsable reports whether this daemon drives agent (its harness was
// usable at startup).
func (d *Daemon) agentUsable(agent string) bool {
	switch agent {
	case "claude":
		return d.claude.Bin != ""
	case "codex":
		return d.codex.Bin != ""
	}
	return false
}

// makeCwd creates a private temp working directory, resolved through
// symlinks so the path Claude sees equals the one passed to --add-dir.
func (d *Daemon) makeCwd() (string, error) {
	dir, err := os.MkdirTemp(d.tempDir, cwdPrefix+"*")
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return resolved, nil
}

// reserveSlot claims one live-process slot under the lock (check and act
// together), so concurrent starts and resumes cannot exceed maxSessions.
func (d *Daemon) reserveSlot() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.procs >= d.maxSessions {
		return false
	}
	d.procs++
	return true
}

// acquireSlot reserves a live-process slot for a start or resume (self is
// the resuming session, nil for a new one). On a full daemon it parks the
// least recently active idle session and takes its slot; when every live
// session is busy it fails (the caller reports the capacity refusal). The
// session-bound sandbox mode (RunSession) serves one session and never parks:
// there is no other session to give way to.
func (d *Daemon) acquireSlot(self *session) bool {
	if d.sandbox {
		return d.reserveSlot()
	}
	for range d.maxSessions + 1 {
		if d.reserveSlot() {
			return true
		}
		victim := d.lruIdle(self)
		if victim == nil {
			return false
		}
		victim.requestPark()
	}
	return d.reserveSlot()
}

// lruIdle picks (and claims) the least recently active idle session.
func (d *Daemon) lruIdle(self *session) *session {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return nil
	}
	var best *session
	for _, s := range d.sessions {
		if s == self || !s.idle {
			continue
		}
		if best == nil || s.lastActive.Before(best.lastActive) {
			best = s
		}
	}
	if best != nil {
		best.idle = false // claimed: a concurrent picker takes another
	}
	return best
}

func (d *Daemon) releaseSlot() {
	d.mu.Lock()
	d.procs--
	d.mu.Unlock()
}

// slotsInUse reports reserved slots (tests).
func (d *Daemon) slotsInUse() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.procs
}

func (d *Daemon) trackToken(id string, live bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if live {
		d.tokens[id] = true
	} else {
		delete(d.tokens, id)
	}
}

// taskTokenIDRe takes the token id from a task token file (mst_<uuid>.<secret>).
var taskTokenIDRe = regexp.MustCompile(`^mst_([0-9a-f-]{36})\.`)

// revokeLeftoverToken revokes the task token a crashed run left in a
// session's run dir. A failed revoke is tracked so exit retries it.
func (d *Daemon) revokeLeftoverToken(sessionID string) {
	if !uuidRe.MatchString(sessionID) {
		return
	}
	data, err := fsutil.ReadFile(filepath.Join(d.store.Dir(), "run", sessionID, "task-token"))
	if err != nil {
		return
	}
	m := taskTokenIDRe.FindStringSubmatch(strings.TrimSpace(string(data)))
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if err := d.api.RevokeTaskToken(ctx, m[1]); err != nil {
		d.trackToken(m[1], true)
		d.log.Printf("leftover task token %s not revoked yet: %v", m[1], err)
		return
	}
	d.log.Printf("leftover task token %s revoked", m[1])
}

// revokeLeftovers retries revocation of any task token a session could not
// revoke itself.
func (d *Daemon) revokeLeftovers() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.tokens))
	for id := range d.tokens {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		err := d.api.RevokeTaskToken(ctx, id)
		cancel()
		if err != nil {
			d.log.Printf("task token %s could not be revoked: %v", id, err)
			continue
		}
		d.trackToken(id, false)
	}
}

// LiveTokens returns the ids of task tokens not yet revoked (tests, exit).
func (d *Daemon) LiveTokens() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.tokens))
	for id := range d.tokens {
		ids = append(ids, id)
	}
	return ids
}

// ─── session-bound sandbox mode (Story 78.22) ───────────────────────────────

// SessionStart is the session-bound mode's start (archivist connect
// --session): the sandbox orchestrator supplies the session, agent and
// prompt, so no user socket and no relay start_session are involved.
type SessionStart struct {
	SessionID string
	Agent     string
	Prompt    string
	// Mode is the session's permission mode ("" = the sandbox default),
	// clamped to the ceiling (Story 78.32).
	Mode Mode
	// Model and Effort are the session's model and effort ("" = the
	// machine flags), checked against this daemon's catalogue by
	// handleStart like a relay start's.
	Model, Effort string
	// ResumeFrom is an earlier session (a paused sandbox task, Story 78.37)
	// whose harness conversation this session continues ("" = fresh). An
	// unusable one starts fresh with a note on the first answer.
	ResumeFrom string
}

// ValidSessionID reports a relay session UUID.
func ValidSessionID(id string) bool { return uuidRe.MatchString(id) }

// ValidPrompt reports a prompt the relay accepts (1..32000 UTF-16 units).
func ValidPrompt(p string) bool { return textOK(p) }

// sessionLookupAttempts bounds GET /agent-sessions/:id retries on
// transient failures (a var so tests can shorten the wait).
var (
	sessionLookupAttempts = 4
	sessionLookupBackoff  = time.Second
)

// RunSession serves exactly one session without the user socket: it never
// reports capabilities and never takes a start from the relay, so a
// sandbox daemon neither supersedes the owner's own archivist connect nor
// waits for a start the relay would only forward to a logged-in daemon.
// The session is checked with chat-api (owned, active, the same agent),
// then started on its session socket as a relay start_session would be
// (handleStart); later messages, approvals, interrupts and stop_session
// arrive on that socket. It returns nil after stop_session, the session's
// end or ctx's end, and a *FatalError when the session cannot start, a
// sign-in fails or the subscription proof fails (Auth set for the last two).
func (d *Daemon) RunSession(ctx context.Context, st SessionStart) error {
	d.sandbox = true
	if err := d.checkSession(ctx, st); err != nil {
		return err
	}
	sessCtx, cancelSessions := context.WithCancel(context.Background())
	defer func() {
		d.beginClosing()
		cancelSessions()
		d.wg.Wait()
		SweepAllOrphans()
		d.revokeLeftovers()
	}()
	d.startCodexProbe(sessCtx)
	in := &Inbound{Kind: "start_session", CorrelationID: "sandbox-" + newRunID(), SessionID: st.SessionID,
		Agent: st.Agent, Prompt: st.Prompt, Mode: string(st.Mode), Model: st.Model, Effort: st.Effort}
	s := d.handleStart(sessCtx, nil, in, st.ResumeFrom)
	if s == nil {
		return &FatalError{Code: "START_REFUSED", Message: "the session did not start (see the log above)"}
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		d.log.Printf("stopping: ending the session")
		cancelSessions()
		<-s.done
	}
	if s.rec.Status != "failed" {
		return nil
	}
	msg := s.fatal
	if msg == "" {
		msg = "the session failed"
	}
	return &FatalError{Code: "SESSION_FAILED", Message: msg, Auth: s.authFailed}
}

// checkSession validates a session-bound start with chat-api before any
// socket connects: the session must exist for this key's owner (chat-api
// answers 404 otherwise), be active and belong to the requested agent.
func (d *Daemon) checkSession(ctx context.Context, st SessionStart) error {
	if !ValidSessionID(st.SessionID) {
		return &FatalError{Code: "BAD_SESSION", Message: "the session id is not a session UUID"}
	}
	if st.ResumeFrom != "" && (!ValidSessionID(st.ResumeFrom) || st.ResumeFrom == st.SessionID) {
		return &FatalError{Code: "BAD_RESUME", Message: "the session to resume from is not another session UUID"}
	}
	if !d.agentUsable(st.Agent) {
		return &FatalError{Code: "AGENT_UNAVAILABLE", Message: fmt.Sprintf("the %s agent is not available here", st.Agent)}
	}
	var info *client.AgentSession
	var err error
	for attempt := 1; ; attempt++ {
		info, err = d.api.GetAgentSession(ctx, st.SessionID)
		if err == nil {
			break
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code != "FEATURE_DISABLED" &&
			(apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusBadRequest) {
			return &FatalError{Code: "SESSION_UNKNOWN", Message: "chat-api does not know this session for this API key's owner"}
		}
		if fatal, _ := classifyMint(err, false); fatal != nil {
			return fatal
		}
		if attempt >= sessionLookupAttempts || ctx.Err() != nil {
			return &FatalError{Code: "SESSION_LOOKUP", Message: "the session could not be checked with chat-api: " + err.Error()}
		}
		d.log.Printf("session lookup failed, retrying: %v", err)
		if err := sleepCtx(ctx, sessionLookupBackoff*time.Duration(attempt)); err != nil {
			return &FatalError{Code: "SESSION_LOOKUP", Message: "the session could not be checked with chat-api: " + err.Error()}
		}
	}
	switch {
	case info.SessionID != st.SessionID || info.Status != "active" || info.ExpiresAt <= time.Now().UnixMilli():
		return &FatalError{Code: "SESSION_INACTIVE", Message: "the session is not active"}
	case info.Agent != st.Agent:
		return &FatalError{Code: "SESSION_AGENT", Message: fmt.Sprintf("the session belongs to the %s agent, not %s", info.Agent, st.Agent)}
	}
	return nil
}

// signedIn records a completed sign-in: later starts need none.
func (d *Daemon) signedIn(agent string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.signIn == agent {
		d.signIn = ""
	}
}
