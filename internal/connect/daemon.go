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
	"time"

	"github.com/coder/websocket"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// DefaultMaxSessions caps concurrently running Claude processes.
const DefaultMaxSessions = 4

// Config assembles a daemon. Only local configuration lives here; the relay
// never supplies any of it.
type Config struct {
	API         API
	RelayURL    string
	StateDir    string
	Claude      ClaudeConfig
	MaxSessions int
	Log         *Logger
	// Detect re-inventories harnesses for each capabilities report.
	Detect func(ctx context.Context) Detection
	// Runner runs `claude auth status` for the per-session proof.
	Runner Runner
	// Environ is the daemon environment the child allowlist reads.
	Environ func() []string
	// TempDir is the parent of per-session working directories ("" = system).
	TempDir string
	// Dial overrides the websocket dialer (tests).
	Dial func(ctx context.Context, u string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
}

// Daemon is a running `archivist connect`.
type Daemon struct {
	api         API
	relayURL    string
	claude      ClaudeConfig
	maxSessions int
	log         *Logger
	parser      *mosaicevent.Parser
	store       *Store
	detect      func(ctx context.Context) Detection
	runner      Runner
	environ     func() []string
	tempDir     string
	dial        func(ctx context.Context, u string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

	mu       sync.Mutex
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
	parser, err := mosaicevent.New()
	if err != nil {
		return nil, fmt.Errorf("load mosaic-event/1 contract: %w", err)
	}
	store, err := OpenStore(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open state %s: %w", cfg.StateDir, err)
	}
	d := &Daemon{api: cfg.API, relayURL: cfg.RelayURL, claude: cfg.Claude, maxSessions: cfg.MaxSessions,
		log: cfg.Log, parser: parser, store: store, detect: cfg.Detect, runner: cfg.Runner,
		environ: cfg.Environ, tempDir: cfg.TempDir, dial: cfg.Dial,
		sessions: map[string]*session{}, starting: map[string]bool{}, tokens: map[string]bool{}}
	if d.maxSessions <= 0 {
		d.maxSessions = DefaultMaxSessions
	}
	if d.runner == nil {
		d.runner = ExecRunner
	}
	if d.environ == nil {
		d.environ = os.Environ
	}
	return d, nil
}

// Run serves until ctx ends (nil), another daemon supersedes this one
// (ErrSuperseded) or a fatal condition (*FatalError). Every child process is
// stopped and every live task token revoked before it returns.
func (d *Daemon) Run(ctx context.Context) error {
	sessCtx, cancelSessions := context.WithCancel(context.Background())
	defer func() {
		d.beginClosing()
		cancelSessions()
		d.wg.Wait()
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
				continue
			}
			d.attach(sessCtx, rec, "", false)
		}
	}

	user := newLink("user socket", UserSocket, "", d.api, d.relayURL, nil, d.log)
	if d.dial != nil {
		user.dial = d.dial
	}
	user.onConnect = func(l *live) {
		det := d.detect(ctx)
		caps := det.Capabilities()
		l.Send(capabilitiesFrame(caps))
		d.log.Printf("capabilities reported: %+v", caps)
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
				d.handleStart(sessCtx, l, in)
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
// and slot passes a live-process slot already reserved for it.
func (d *Daemon) attach(ctx context.Context, rec *SessionRecord, prompt string, slot bool) {
	s := newSession(d, rec)
	s.slot = slot
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
}

// handleStart validates and starts one session. It acknowledges only after
// the local record is durable (or the refusal is final), so a crash before
// that point lets the relay redeliver.
func (d *Daemon) handleStart(ctx context.Context, l *live, in *Inbound) {
	sid := in.SessionID
	ack := func() { l.Send(commandAck(UserSocket, in.CorrelationID, sid)) }
	d.mu.Lock()
	if d.starting[sid] {
		d.mu.Unlock()
		return // the attempt in progress acknowledges
	}
	if _, running := d.sessions[sid]; running {
		d.mu.Unlock()
		ack()
		return
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
		return
	} else if errors.Is(err, errCorruptRecord) {
		// Refused without execution and reported, so the relay stops redelivering.
		d.refuseStart(ctx, l, in, "The local session record is unreadable; start a new session.", true)
		return
	} else if !isNotExist(err) {
		// Transient read failure: no ack, the relay redelivers.
		d.log.Printf("start_session %s: session record unreadable, awaiting redelivery: %v", in.CorrelationID, err)
		return
	}
	if in.Agent != "claude" {
		d.refuseStart(ctx, l, in, fmt.Sprintf("The %s agent is not supported by this archivist connect yet.", in.Agent), true)
		return
	}
	info, err := d.api.GetAgentSession(ctx, sid)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusBadRequest) {
			d.refuseStart(ctx, l, in, "The session is unknown.", false)
			return
		}
		// Transient: no ack, the relay redelivers in 30 s.
		d.log.Printf("start_session %s: session lookup failed, awaiting redelivery: %v", in.CorrelationID, err)
		return
	}
	now := time.Now().UnixMilli()
	if info.SessionID != sid || info.Status != "active" || info.ExpiresAt <= now {
		d.refuseStart(ctx, l, in, "The session is not active.", false)
		return
	}
	if info.Agent != "claude" {
		d.refuseStart(ctx, l, in, "The session belongs to another agent.", true)
		return
	}
	if !d.reserveSlot() {
		d.refuseStart(ctx, l, in, fmt.Sprintf("archivist connect already runs %d live sessions.", d.maxSessions), true)
		return
	}
	cwd, err := d.makeCwd()
	if err != nil {
		d.releaseSlot()
		d.log.Printf("start_session %s: working directory: %v", in.CorrelationID, err)
		return
	}
	rec := &SessionRecord{SessionID: sid, Agent: in.Agent, StartCorrelationID: in.CorrelationID, Cwd: cwd,
		ExpiresAt: info.ExpiresAt, Status: "active", CreatedAt: now}
	if err := d.store.Save(rec); err != nil {
		d.releaseSlot()
		_ = os.RemoveAll(cwd)
		d.log.Printf("start_session %s: session record not saved: %v", in.CorrelationID, err)
		return
	}
	ack()
	d.log.Printf("session %s starting (cwd %s)", sid, cwd)
	d.attach(ctx, rec, in.Prompt, true)
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
	l.Send(commandAck(UserSocket, in.CorrelationID, in.SessionID))
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
	data, err := os.ReadFile(filepath.Join(d.store.Dir(), "run", sessionID, "task-token"))
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
