package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/taskscope"
)

const (
	taskTokenTTL     = 900
	tokenRefreshLead = 2 * time.Minute
	stopGrace        = 5 * time.Second
	coalesceWindow   = 250 * time.Millisecond
	drainTimeout     = 5 * time.Second
	revokeTimeout    = 5 * time.Second
)

// Timings tests shorten; production values never change at runtime.
var (
	// interruptTimeout bounds the wait for a result after a control interrupt.
	interruptTimeout = 10 * time.Second
	// tokenRetryBase is the first task token refresh retry delay (doubling to 1 min).
	tokenRetryBase = 5 * time.Second
)

// sessionCmd is one relay command with the socket that delivered it (acks go
// back on that socket).
type sessionCmd struct {
	l  *live
	in *Inbound
}

// pendingApproval is a can_use_tool request waiting for the relay.
type pendingApproval struct {
	toolName  string
	toolUseID string
	input     json.RawMessage
}

// session drives one relay session: its session socket, its Claude Code
// process (started, resumed after a daemon restart, stopped), approvals,
// interrupts and its task token.
type session struct {
	d      *Daemon
	id     string
	rec    *SessionRecord
	log    *Logger
	outbox *Outbox
	link   *link
	cmds   chan sessionCmd
	tr     *Translator
	co     Coalescer

	proc       *Proc
	procDone   <-chan struct{}
	procLines  <-chan []byte
	turnActive bool
	queue      []string
	pending    map[string]*pendingApproval
	remember   map[string]bool // tool name -> allow (true) or deny (false)
	running    bool            // a passing init was seen for the current process
	held       []Chunk         // turn start chunks waiting for the init proof
	resumedID  string          // Claude session id the current process resumed
	slot       bool            // holds one of the daemon's live-process slots
	fatal      string          // subscription proof failure: no further turns
	stopping   bool
	// early holds user messages that arrived during a Codex sign-in; they
	// follow the first prompt (Story 78.22).
	early []string
	// authFailed marks a failed sign-in or subscription proof (sandbox exit).
	authFailed bool

	interruptSeq   int
	interruptTimer *time.Timer

	// Codex (Story 78.17): the live app-server connection, the translator,
	// the turn's last usage report (sent just before finish/abort) and the
	// descendant snapshot timer.
	cx        *codexRun
	ctr       *CodexTranslator
	lastUsage Chunk
	snapTimer *time.Timer

	token      *client.TaskToken
	tokenFile  string
	mcpFile    string
	tokenTimer *time.Timer
	tokenRetry time.Duration

	linkErr chan error
	done    chan struct{}
}

func newSession(d *Daemon, rec *SessionRecord) *session {
	log := d.log.With("session " + rec.SessionID[:8])
	s := &session{d: d, id: rec.SessionID, rec: rec, log: log,
		cmds: make(chan sessionCmd, 64), pending: map[string]*pendingApproval{},
		remember: map[string]bool{}, linkErr: make(chan error, 1), done: make(chan struct{})}
	s.outbox = NewOutbox(d.parser, log)
	s.tr = NewTranslator()
	s.ctr = NewCodexTranslator()
	runID := s.outbox.RunID()
	s.tr.TurnID = func(n int) string { return "claude-" + runID + "-turn-" + strconv.Itoa(n) }
	s.link = newLink("session socket", SessionSocket, rec.SessionID, d.api, d.relayURL, s.outbox, log)
	s.link.onCommand = func(l *live, in *Inbound) {
		select {
		case s.cmds <- sessionCmd{l, in}:
		case <-s.done:
		}
	}
	if d.dial != nil {
		s.link.dial = d.dial
	}
	return s
}

// run owns all session state. prompt is non-empty for a fresh start.
func (s *session) run(ctx context.Context, prompt string) {
	defer close(s.done)
	defer s.releaseSlot()
	linkCtx, cancelLink := context.WithCancel(context.Background())
	go func() { s.linkErr <- s.link.run(linkCtx) }()
	defer func() {
		cancelLink()
		<-s.linkErr
	}()

	if prompt != "" {
		s.emitStatus("starting", "")
		res := signedIn
		if s.needsSignIn() {
			res = s.signIn(ctx)
		}
		switch res {
		case signInStopped:
			return
		case signedIn:
			if err := s.spawn(ctx, ""); err != nil {
				s.failStart(err)
			} else {
				s.sendUser(prompt)
				for _, text := range s.early {
					s.sendUser(text)
				}
			}
			s.early = nil
		}
	}

	// The coalescing window starts at the first held delta and is not
	// extended by later ones, so a steady stream still flushes every window.
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	armed := false
	for {
		if s.d.sandbox && s.rec.Status == "failed" {
			// Session-bound sandbox mode: a failed session ends the daemon.
			s.flushCoalesced()
			dctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			s.outbox.WaitDrained(dctx)
			cancel()
			return
		}
		if s.co.Pending() && !armed {
			flush.Reset(coalesceWindow)
			armed = true
		}
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case err := <-s.linkErr:
			s.linkErr <- err // keep for the deferred wait
			s.linkEnded(err)
			return
		case c := <-s.cmds:
			if s.handle(ctx, c) {
				return
			}
		case line, ok := <-s.procLines:
			if !ok {
				s.procLines = nil
				continue
			}
			s.handleLine(line)
		case <-s.procDone:
			s.processExited()
		case <-flush.C:
			armed = false
			s.flushCoalesced()
		case <-timerC(s.interruptTimer):
			s.interruptTimedOut()
		case <-timerC(s.tokenTimer):
			s.refreshToken(ctx)
		case <-s.codexWake():
			s.drainCodex()
		case <-s.codexGone():
			// The JSON-RPC connection closed (protocol error or EOF) while
			// the process may still run: treat it as the process ending.
			s.log.Printf("codex connection closed")
			s.processExited()
		case <-timerC(s.snapTimer):
			s.snapshot()
		}
	}
}

// codexWake signals queued Codex events (nil without a Codex process).
func (s *session) codexWake() <-chan struct{} {
	if s.cx == nil {
		return nil
	}
	return s.cx.rpc.q.wake
}

// codexGone is closed when the Codex JSON-RPC connection ends.
func (s *session) codexGone() <-chan struct{} {
	if s.cx == nil {
		return nil
	}
	return s.cx.rpc.conn.DisconnectNotify()
}

// snapshot records the harness's descendants and reaps adopted zombies
// (periodic: Codex sessions, and Claude sessions while the daemon is the
// child subreaper).
func (s *session) snapshot() {
	s.snapTimer = nil
	if s.proc == nil {
		return
	}
	s.proc.Snapshot()
	reapZombies()
	s.snapTimer = time.NewTimer(codexSnapshotEvery)
}

// isCodex reports a Codex session (Story 78.17); everything else is Claude.
func (s *session) isCodex() bool { return s.rec.Agent == "codex" }

// harness names the session's harness in user-facing messages.
func (s *session) harness() string {
	if s.isCodex() {
		return "Codex"
	}
	return "Claude Code"
}

func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// ─── events ─────────────────────────────────────────────────────────────────

func (s *session) emit(chunks ...Chunk) {
	for _, c := range chunks {
		for _, out := range s.co.Add(c) {
			s.outbox.Emit(out)
		}
	}
}

func (s *session) flushCoalesced() {
	for _, out := range s.co.Flush() {
		s.outbox.Emit(out)
	}
}

func (s *session) emitStatus(status, message string) {
	s.flushCoalesced()
	data := map[string]any{"sessionId": s.id, "status": status}
	if message != "" {
		data["message"] = message
	}
	s.outbox.Emit(Chunk{"type": "data-session-status", "data": data})
}

func (s *session) emitError(text string) {
	s.flushCoalesced()
	s.outbox.Emit(Chunk{"type": "error", "errorText": text})
}

// ─── process ────────────────────────────────────────────────────────────────

// spawn starts the session's harness after its subscription login proof;
// resumeID continues an earlier Claude session or Codex thread.
func (s *session) spawn(ctx context.Context, resumeID string) (err error) {
	if !s.slot {
		if !s.d.reserveSlot() {
			return errCapacity
		}
		s.slot = true
	}
	defer func() {
		if err != nil {
			s.releaseSlot()
		}
	}()
	if !s.d.agentUsable(s.rec.Agent) {
		// A record reattached for a harness this run does not drive: not a
		// failed proof, the session stays resumable.
		return fmt.Errorf("%s is not usable in this archivist connect run; fix it, then restart archivist connect", s.harness())
	}
	if err := s.ensureToken(ctx); err != nil {
		return fmt.Errorf("task token: %w", err)
	}
	if s.rec.Cwd == "" {
		return errors.New("session has no working directory")
	}
	if _, err := os.Stat(s.rec.Cwd); err != nil {
		if err := os.MkdirAll(s.rec.Cwd, 0o700); err != nil {
			return fmt.Errorf("working directory: %w", err)
		}
	}
	if s.isCodex() {
		return s.spawnCodex(ctx, resumeID)
	}
	env, err := BuildChildEnv(s.d.environ(), nil)
	if err != nil {
		return err
	}
	cfg := s.d.claude
	st, err := ClaudeAuthStatus(ctx, s.d.runner, env, s.rec.Cwd, cfg.Bin)
	if err != nil {
		return &proofError{"claude auth status failed: " + err.Error()}
	}
	if !st.Subscription() {
		return &proofError{fmt.Sprintf("Claude Code is not logged in with a claude.ai subscription (loggedIn=%v, authMethod=%q)", st.LoggedIn, st.AuthMethod)}
	}
	args := claudeArgs(cfg, s.mcpFile, s.rec.Cwd, resumeID)
	proc, err := StartProc(ProcSpec{Bin: cfg.Bin, Args: args, Env: env, Dir: s.rec.Cwd, Log: s.log})
	if err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	s.log.Printf("claude started (%s, env keys %v, resume=%v)", proc, EnvKeys(env), resumeID != "")
	s.proc, s.procDone, s.procLines = proc, proc.Done(), proc.Lines()
	s.running, s.turnActive, s.resumedID = false, false, resumeID
	s.tr.reset()
	if s.d.reaping {
		// The daemon adopts orphans (Codex configured): reap Claude's too.
		s.snapTimer = time.NewTimer(codexSnapshotEvery)
	}
	return nil
}

// errCapacity means every live-process slot is taken.
var errCapacity = errors.New("too many live sessions")

// emitCapacity reports a full daemon (no error chunk: nothing ran).
func (s *session) emitCapacity() {
	s.emitStatus("failed", fmt.Sprintf("archivist connect already runs %d live sessions.", s.d.maxSessions))
}

// releaseSlot returns this session's live-process slot, once.
func (s *session) releaseSlot() {
	if s.slot {
		s.slot = false
		s.d.releaseSlot()
	}
}

// resumable reports whether a later message can resume the harness.
func (s *session) resumable() bool { return s.resumeID() != "" }

// resumeID is the Claude session id or Codex thread id to resume.
func (s *session) resumeID() string {
	if s.isCodex() {
		return s.rec.CodexThreadID
	}
	return s.rec.ClaudeSessionID
}

// failSession ends a session that cannot continue: the record is failed,
// the token revoked and later messages refused with msg.
func (s *session) failSession(msg string) {
	s.fatal = msg
	s.rec.Status = "failed"
	s.save()
	s.revokeToken()
	s.d.store.RemoveRunDir(s.id)
	removeSessionCwd(s.rec.Cwd)
	if s.isCodex() {
		s.d.store.RemoveCodexHome(s.id)
	}
}

// proofError is a failed subscription proof: fatal for the session.
type proofError struct{ msg string }

func (e *proofError) Error() string { return e.msg }

func (s *session) failStart(err error) {
	var proof *proofError
	var lost *lostError
	msg := Scrub("Could not start " + s.harness() + ": " + err.Error())
	s.log.Printf("%s", msg)
	if errors.Is(err, errCapacity) {
		s.emitCapacity()
		return
	}
	s.emitError(msg)
	switch {
	case errors.As(err, &proof):
		s.authFailed = true
		s.failSession(proof.msg)
		s.emitStatus("failed", msg)
	case errors.As(err, &lost):
		s.failSession(msg + " Start a new session.")
		s.emitStatus("failed", msg+" Start a new session.")
	case s.resumable():
		s.revokeToken()
		s.d.store.RemoveRunDir(s.id) // ensureToken recreates it on resume
		s.emitStatus("failed", msg+" The next message tries to resume the session.")
	default:
		s.failSession(msg)
		s.emitStatus("failed", msg+" Start a new session.")
	}
}

func (s *session) sendUser(text string) {
	if s.proc == nil {
		return
	}
	if s.turnActive {
		s.queue = append(s.queue, text)
		return
	}
	if s.isCodex() {
		s.codexSendUser(text)
		return
	}
	frame := userFrame(text)
	if err := s.proc.WriteJSON(frame); err != nil {
		s.log.Printf("write to claude failed: %v", err)
		return
	}
	s.turnActive = true
	chunks := s.tr.Out(frame)
	if s.running {
		s.emit(chunks...)
	} else {
		s.held = append(s.held, chunks...)
	}
}

func (s *session) write(frame map[string]any) {
	if s.proc == nil {
		return
	}
	if err := s.proc.WriteJSON(frame); err != nil {
		s.log.Printf("write to claude failed: %v", err)
		return
	}
	s.emit(s.tr.Out(frame)...)
}

// stopProcess ends the current Claude process (stdin close, grace, group kill).
func (s *session) stopProcess(kill bool) {
	if s.proc == nil {
		return
	}
	p := s.proc
	s.proc, s.procDone, s.procLines = nil, nil, nil
	if kill {
		p.Kill()
	} else {
		p.Stop(stopGrace)
	}
	s.releaseSlot()
	s.turnActive, s.running = false, false
	s.held = nil
	s.pending = map[string]*pendingApproval{}
	s.queue = nil
	s.stopInterruptTimer()
	if s.cx != nil {
		s.cx.rpc.close()
		s.cx = nil
	}
	s.lastUsage = nil
	if s.snapTimer != nil {
		s.snapTimer.Stop()
		s.snapTimer = nil
	}
}

// nextQueued starts the oldest message queued during a turn.
func (s *session) nextQueued() {
	if len(s.queue) > 0 {
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.sendUser(next)
	}
}

func (s *session) processExited() {
	// The reader closes Lines before Done: finish the buffered frames (the
	// final result may be among them) before treating the exit.
	if lines := s.procLines; lines != nil {
		for line := range lines {
			if s.proc == nil {
				return
			}
			s.handleLine(line)
		}
	}
	if s.cx != nil {
		// Let the JSON-RPC reader finish the buffered messages (the turn may
		// have completed) before treating the exit.
		select {
		case <-s.cx.rpc.conn.DisconnectNotify():
		case <-time.After(2 * time.Second):
		}
		s.drainCodex()
		if s.proc != nil && len(s.queue) > 0 {
			s.emitError(fmt.Sprintf("Codex exited before %d queued message(s) were sent; send them again.", len(s.queue)))
		}
	}
	if s.proc == nil {
		return
	}
	err := s.proc.ExitErr()
	wasTurn := s.turnActive
	s.stopProcess(true)
	if s.stopping {
		return
	}
	if s.isCodex() {
		s.log.Printf("codex exited unexpectedly: %v", err)
	} else {
		s.log.Printf("claude exited unexpectedly: %v", err)
	}
	if wasTurn {
		s.emitError(s.harness() + " exited before the turn finished.")
	}
	s.processGone(s.harness() + " exited")
}

// processGone settles a session whose Claude process went away on its own
// or was killed: the token is revoked (a resume mints a new one) and the
// session stays resumable only when a Claude session id exists.
func (s *session) processGone(what string) {
	if s.resumable() {
		s.revokeToken()
		s.d.store.RemoveRunDir(s.id) // ensureToken recreates it on resume
		s.emitStatus("failed", what+"; the next message resumes the session.")
		return
	}
	msg := what + " before the session started. Start a new session."
	s.failSession(msg)
	s.emitStatus("failed", msg)
}

// handleLine processes one Claude stdout frame.
func (s *session) handleLine(line []byte) {
	var f claudeFrame
	if err := json.Unmarshal(line, &f); err != nil {
		s.log.Printf("unparsed claude line skipped")
		return
	}
	switch {
	case f.Type == "system" && f.Subtype == "init":
		if problem := initProblem(f); problem != "" {
			s.failProof("Claude Code did not start on the subscription login: " + problem)
			return
		}
		if s.resumedID != "" && f.SessionID != s.resumedID {
			s.log.Printf("warning: --resume=%s reported Claude session %q; earlier context may be missing", s.resumedID, f.SessionID)
		}
		s.resumedID = ""
		if f.SessionID != "" && f.SessionID != s.rec.ClaudeSessionID {
			s.rec.ClaudeSessionID = f.SessionID
			s.save()
		}
		if !s.running {
			s.running = true
			s.log.Printf("init ok: claude %s, model %s, apiKeySource none, permissionMode default, tools %v, mcp %s, plugins %s",
				f.Version, f.Model, f.Tools, string(f.MCPServers), string(f.Plugins))
			s.emitStatus("running", "")
			s.emit(s.held...)
			s.held = nil
		}
		return
	case f.Type == "control_request":
		if s.handleControlRequest(f, line) {
			return
		}
	case f.Type == "result":
		if f.SessionID != "" && f.SessionID != s.rec.ClaudeSessionID {
			s.rec.ClaudeSessionID = f.SessionID
			s.save()
		}
	}
	if !s.running && f.Type != "control_response" {
		// Nothing reaches the relay before the init proof passes.
		return
	}
	s.emit(s.tr.In(line)...)
	if f.Type == "result" {
		s.turnActive = false
		aborted := f.TerminalReason == "aborted_streaming"
		if s.interruptTimer != nil {
			s.stopInterruptTimer()
		}
		if aborted {
			s.emitStatus("interrupted", "")
		}
		s.flushCoalesced()
		s.nextQueued()
	}
}

// failProof enforces the fail-closed subscription proof.
func (s *session) failProof(msg string) {
	s.log.Printf("%s; stopping the session", msg)
	s.authFailed = true
	if s.proc != nil && !s.isCodex() {
		_ = s.proc.WriteJSON(interruptFrame("archivist-proof-" + s.outbox.RunID()))
	}
	s.stopProcess(true)
	s.failSession(msg)
	s.emitError(msg)
	s.emitStatus("failed", msg)
}

// handleControlRequest answers or forwards a control request. It returns
// true when the frame must not reach the translator.
func (s *session) handleControlRequest(f claudeFrame, line []byte) bool {
	var req controlRequest
	if err := json.Unmarshal(f.Request, &req); err != nil || f.RequestID == "" {
		s.log.Printf("malformed control_request ignored")
		return true
	}
	if req.Subtype != "can_use_tool" {
		_ = s.proc.WriteJSON(controlError(f.RequestID, "unsupported by archivist connect: "+req.Subtype))
		return true
	}
	if !s.running {
		_ = s.proc.WriteJSON(denyFrame(f.RequestID, "Denied: the session is not verified."))
		return true
	}
	if allow, ok := s.remember[req.ToolName]; ok {
		// Remembered for this session: answer locally, no relay round trip.
		if allow {
			s.write(allowFrame(f.RequestID, req.Input))
		} else {
			s.write(denyFrame(f.RequestID, "Denied: the user rejected "+req.ToolName+" for this session."))
		}
		return true
	}
	s.pending[f.RequestID] = &pendingApproval{toolName: req.ToolName, toolUseID: req.ToolUseID, input: req.Input}
	return false
}

// ─── commands ───────────────────────────────────────────────────────────────

// handle runs one relay command; it returns true when the session ended.
func (s *session) handle(ctx context.Context, c sessionCmd) bool {
	in := c.in
	if !s.admit(c) {
		return false
	}
	switch in.Kind {
	case "user_message":
		s.userMessage(ctx, in.Text)
	case "interrupt":
		s.interrupt()
	case "stop_session":
		s.stop(true)
		return true
	}
	return false
}

// admit acknowledges one relay command and reports whether it is to be
// executed: a command for another session, an approval (answered here), a
// command already handled, or one whose record could not be saved (the
// relay redelivers it) is not.
func (s *session) admit(c sessionCmd) bool {
	in := c.in
	ack := func() { c.l.Send(commandAck(SessionSocket, in.CorrelationID, "")) }
	if in.SessionID != s.id {
		s.log.Printf("refused %s for another session", in.Kind)
		ack()
		return false
	}
	if in.Kind == "approval_response" {
		s.answerApproval(in)
		ack()
		return false
	}
	if s.rec.HasHandled(in.CorrelationID) {
		ack()
		return false
	}
	prev := append([]string(nil), s.rec.Handled...)
	s.rec.MarkHandled(in.CorrelationID)
	if !s.save() {
		s.rec.Handled = prev // remembered only once durable; the relay redelivers
		return false
	}
	ack()
	return true
}

func (s *session) userMessage(ctx context.Context, text string) {
	if s.fatal != "" {
		s.emitStatus("failed", s.fatal)
		return
	}
	if s.proc == nil {
		if !s.resumable() {
			what := "Claude Code session"
			if s.isCodex() {
				what = "Codex thread"
			}
			s.emitError("There is no " + what + " to resume.")
			s.emitStatus("failed", "There is no "+what+" to resume.")
			return
		}
		// Reserve the slot first: a full daemon refuses without "starting".
		if !s.slot {
			if !s.d.reserveSlot() {
				s.emitCapacity()
				return
			}
			s.slot = true
		}
		if s.isCodex() {
			s.emitStatus("starting", "Resuming the Codex thread.")
		} else {
			s.emitStatus("starting", "Resuming the Claude Code session.")
		}
		if err := s.spawn(ctx, s.resumeID()); err != nil {
			s.failStart(err)
			return
		}
	}
	s.sendUser(text)
}

func (s *session) interrupt() {
	if s.proc == nil || !s.turnActive || s.interruptTimer != nil {
		return // no live turn: ack only
	}
	if s.isCodex() {
		s.codexInterrupt()
		s.interruptTimer = time.NewTimer(interruptTimeout)
		return
	}
	s.interruptSeq++
	id := fmt.Sprintf("archivist-interrupt-%s-%d", s.outbox.RunID(), s.interruptSeq)
	if err := s.proc.WriteJSON(interruptFrame(id)); err != nil {
		s.log.Printf("interrupt write failed: %v", err)
		return
	}
	s.interruptTimer = time.NewTimer(interruptTimeout)
}

func (s *session) stopInterruptTimer() {
	if s.interruptTimer != nil {
		s.interruptTimer.Stop()
		s.interruptTimer = nil
	}
}

func (s *session) interruptTimedOut() {
	s.interruptTimer = nil
	if s.isCodex() {
		s.log.Printf("no turn completion %v after interrupt; terminating codex", interruptTimeout)
	} else {
		s.log.Printf("no result %v after interrupt; terminating claude", interruptTimeout)
	}
	s.stopProcess(true)
	s.emitError(s.harness() + " did not stop after the interrupt.")
	s.processGone(s.harness() + " did not stop after the interrupt")
}

// answerApproval turns a relay resolution into Claude's control response.
func (s *session) answerApproval(in *Inbound) {
	if s.isCodex() {
		if s.cx != nil {
			s.codexAnswerApproval(in)
		}
		return
	}
	p, ok := s.pending[in.ApprovalID]
	if !ok || s.proc == nil {
		return // unknown or already answered: ack only
	}
	delete(s.pending, in.ApprovalID)
	if in.Decision == "allow" {
		s.write(allowFrame(in.ApprovalID, p.input))
	} else {
		msg := "Denied: the user rejected this tool call."
		if in.Reason == "timeout" {
			msg = "Denied: no approval arrived before the approval timeout."
		}
		s.write(denyFrame(in.ApprovalID, msg))
	}
	if in.Scope != "" {
		data := map[string]any{"approvalId": in.ApprovalID, "scope": in.Scope}
		if p.toolUseID != "" {
			data["toolCallId"] = p.toolUseID
		}
		s.emit(Chunk{"type": "data-permission-scope", "data": data})
		switch in.Scope {
		case "allow_always":
			s.remember[p.toolName] = true
		case "reject_always":
			s.remember[p.toolName] = false
		}
	}
}

// stop ends the session for good (stop_session).
func (s *session) stop(emitCompleted bool) {
	s.stopping = true
	s.flushCoalesced()
	s.stopProcess(false)
	s.revokeToken()
	if emitCompleted {
		s.emitStatus("completed", "")
	}
	s.rec.Status = "ended"
	s.save()
	s.d.store.RemoveRunDir(s.id)
	removeSessionCwd(s.rec.Cwd)
	if s.isCodex() {
		s.d.store.RemoveCodexHome(s.id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	s.outbox.WaitDrained(ctx)
	s.log.Printf("session stopped")
}

// shutdown leaves the session resumable (daemon exit): stop the process,
// revoke the token, keep the record and working directory.
func (s *session) shutdown() {
	s.stopping = true
	s.flushCoalesced()
	s.stopProcess(false)
	s.revokeToken()
	s.d.store.RemoveRunDir(s.id)
	if s.rec.Status == "active" {
		if s.d.sandbox {
			s.emitStatus("disconnected", "The sandbox stopped.")
		} else {
			s.emitStatus("disconnected", "archivist connect stopped; the next message resumes the session.")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.outbox.WaitDrained(ctx)
}

// linkEnded handles a terminal session socket condition.
func (s *session) linkEnded(err error) {
	switch {
	case errors.Is(err, ErrSessionInactive):
		s.log.Printf("relay reports the session ended")
		s.stop(false)
	case errors.Is(err, ErrSuperseded):
		s.log.Printf("another daemon took this session over; stopping locally")
		s.stopping = true
		s.stopProcess(false)
		s.revokeToken()
		s.d.store.RemoveRunDir(s.id)
	default:
		var fatal *FatalError
		isFatal := errors.As(err, &fatal)
		if isFatal {
			s.log.Printf("session socket stopped: %v", err)
			s.rec.Status = "failed"
			s.save()
			removeSessionCwd(s.rec.Cwd)
		}
		s.stopping = true
		s.stopProcess(false)
		// Only once Codex is gone: a running Codex could recreate the home
		// (and, without the link, write a separate login there).
		if isFatal && s.isCodex() {
			s.d.store.RemoveCodexHome(s.id)
		}
		s.revokeToken()
		s.d.store.RemoveRunDir(s.id)
	}
}

// save persists the record; it reports success (failures are logged).
func (s *session) save() bool {
	if err := s.d.store.Save(s.rec); err != nil {
		s.log.Printf("session record not saved: %v", err)
		return false
	}
	return true
}

// ─── task token ─────────────────────────────────────────────────────────────

// ensureToken mints the session task token and writes the token file and,
// for Claude, the MCP config (both 0600, in the private run dir outside the
// cwd). Codex gets its MCP server through -c overrides instead.
func (s *session) ensureToken(ctx context.Context) error {
	if s.token != nil {
		return nil
	}
	dir, err := s.d.store.RunDir(s.id)
	if err != nil {
		return err
	}
	tok, err := s.d.api.MintTaskToken(ctx, s.id, taskscope.Scopes, taskTokenTTL)
	if err != nil {
		return err
	}
	// Tracked from the mint on, so a failed revoke is retried at exit.
	s.d.trackToken(tok.TokenID, true)
	if !validTaskToken(tok.Token) {
		s.revoke(tok)
		return fmt.Errorf("chat-api returned a malformed task token")
	}
	s.tokenFile = filepath.Join(dir, "task-token")
	if err := writeFileAtomic(s.tokenFile, []byte(tok.Token+"\n")); err != nil {
		s.revoke(tok)
		return err
	}
	if s.isCodex() {
		s.token = tok
		s.log.Printf("task token minted (fp:%s, expires %s)", auth.Fingerprint(tok.Token), time.UnixMilli(tok.ExpiresAt).UTC().Format(time.RFC3339))
		s.scheduleRefresh(time.Until(time.UnixMilli(tok.ExpiresAt)) - tokenRefreshLead)
		return nil
	}
	s.mcpFile = filepath.Join(dir, "mcp.json")
	cfg, err := mcpConfig(s.d.claude, s.tokenFile)
	if err != nil {
		s.revoke(tok)
		return err
	}
	if err := writeFileAtomic(s.mcpFile, cfg); err != nil {
		s.revoke(tok)
		return err
	}
	s.token = tok
	s.log.Printf("task token minted (fp:%s, expires %s)", auth.Fingerprint(tok.Token), time.UnixMilli(tok.ExpiresAt).UTC().Format(time.RFC3339))
	s.scheduleRefresh(time.Until(time.UnixMilli(tok.ExpiresAt)) - tokenRefreshLead)
	return nil
}

func (s *session) scheduleRefresh(in time.Duration) {
	if s.tokenTimer != nil {
		s.tokenTimer.Stop()
	}
	if in < time.Second {
		in = time.Second
	}
	s.tokenTimer = time.NewTimer(in)
}

// refreshToken re-mints before expiry, atomically rewrites the token file
// read by mcp serve per call, then revokes the old token. Failures keep the
// session and retry with backoff.
func (s *session) refreshToken(ctx context.Context) {
	s.tokenTimer = nil
	if s.token == nil {
		return
	}
	old := s.token
	tok, err := s.d.api.MintTaskToken(ctx, s.id, taskscope.Scopes, taskTokenTTL)
	if err == nil {
		s.d.trackToken(tok.TokenID, true)
		if !validTaskToken(tok.Token) {
			s.revoke(tok)
			err = fmt.Errorf("chat-api returned a malformed task token")
		}
	}
	if err == nil {
		err = writeFileAtomic(s.tokenFile, []byte(tok.Token+"\n"))
		if err != nil {
			s.revoke(tok)
		}
	}
	if err != nil {
		s.tokenRetry = nextRetry(s.tokenRetry)
		s.log.Printf("task token refresh failed (current fp:%s), retrying in %v: %v", auth.Fingerprint(old.Token), s.tokenRetry, err)
		s.scheduleRefresh(s.tokenRetry)
		return
	}
	s.tokenRetry = 0
	s.token = tok
	s.log.Printf("task token refreshed (fp:%s replaces fp:%s)", auth.Fingerprint(tok.Token), auth.Fingerprint(old.Token))
	s.revoke(old)
	s.scheduleRefresh(time.Until(time.UnixMilli(tok.ExpiresAt)) - tokenRefreshLead)
}

func (s *session) revokeToken() {
	if s.tokenTimer != nil {
		s.tokenTimer.Stop()
		s.tokenTimer = nil
	}
	if s.token != nil {
		s.revoke(s.token)
		s.token = nil
	}
}

func (s *session) revoke(tok *client.TaskToken) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if err := s.d.api.RevokeTaskToken(ctx, tok.TokenID); err != nil {
		s.log.Printf("task token revoke failed (fp:%s): %v", auth.Fingerprint(tok.Token), err)
		return
	}
	s.d.trackToken(tok.TokenID, false)
	s.log.Printf("task token revoked (fp:%s)", auth.Fingerprint(tok.Token))
}

// nextRetry is the token refresh retry delay after cur: tokenRetryBase,
// then doubling, never above one minute.
func nextRetry(cur time.Duration) time.Duration {
	if cur == 0 {
		return min(tokenRetryBase, time.Minute)
	}
	return min(cur*2, time.Minute)
}

// validTaskToken is the strict mst_ wire check used for every minted token.
func validTaskToken(tok string) bool {
	return auth.IsTaskToken(tok) && auth.ValidateTokenFormat(tok) == nil
}
