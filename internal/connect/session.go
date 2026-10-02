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
	interruptTimeout = 10 * time.Second
	stopGrace        = 5 * time.Second
	coalesceWindow   = 250 * time.Millisecond
	drainTimeout     = 5 * time.Second
	revokeTimeout    = 5 * time.Second
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
	fatal      string          // subscription proof failure: no further turns
	stopping   bool

	interruptSeq   int
	interruptTimer *time.Timer

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
	linkCtx, cancelLink := context.WithCancel(context.Background())
	go func() { s.linkErr <- s.link.run(linkCtx) }()
	defer func() {
		cancelLink()
		<-s.linkErr
	}()

	if prompt != "" {
		s.emitStatus("starting", "")
		if err := s.spawn(ctx, ""); err != nil {
			s.failStart(err)
		} else {
			s.sendUser(prompt)
		}
	}

	// The coalescing window starts at the first held delta and is not
	// extended by later ones, so a steady stream still flushes every window.
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	armed := false
	for {
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
		}
	}
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

// spawn starts Claude Code after the subscription login proof; resumeID
// continues an earlier Claude session.
func (s *session) spawn(ctx context.Context, resumeID string) error {
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
	s.d.processStarted()
	return nil
}

// proofError is a failed subscription proof: fatal for the session.
type proofError struct{ msg string }

func (e *proofError) Error() string { return e.msg }

func (s *session) failStart(err error) {
	var proof *proofError
	msg := "Could not start Claude Code: " + err.Error()
	if errors.As(err, &proof) {
		s.fatal = proof.msg
		s.rec.Status = "failed"
		s.save()
		s.revokeToken()
	}
	s.log.Printf("%s", msg)
	s.emitError(Scrub(msg))
	s.emitStatus("failed", Scrub(msg))
}

func (s *session) sendUser(text string) {
	if s.proc == nil {
		return
	}
	if s.turnActive {
		s.queue = append(s.queue, text)
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
	s.d.processStopped()
	s.turnActive, s.running = false, false
	s.held = nil
	s.pending = map[string]*pendingApproval{}
	s.queue = nil
	s.stopInterruptTimer()
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
	if s.proc == nil {
		return
	}
	err := s.proc.ExitErr()
	wasTurn := s.turnActive
	s.stopProcess(true)
	if s.stopping {
		return
	}
	s.log.Printf("claude exited unexpectedly: %v", err)
	if wasTurn {
		s.emitError("Claude Code exited before the turn finished.")
	}
	s.emitStatus("failed", "Claude Code exited; the next message resumes the session.")
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
		if len(s.queue) > 0 {
			next := s.queue[0]
			s.queue = s.queue[1:]
			s.sendUser(next)
		}
	}
}

// failProof enforces the fail-closed subscription proof.
func (s *session) failProof(msg string) {
	s.log.Printf("%s; stopping the session", msg)
	if s.proc != nil {
		_ = s.proc.WriteJSON(interruptFrame("archivist-proof-" + s.outbox.RunID()))
	}
	s.stopProcess(true)
	s.revokeToken()
	s.fatal = msg
	s.rec.Status = "failed"
	s.save()
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
	s.rec.MarkHandled(in.CorrelationID)
	if !s.save() {
		return false // not durable: the relay redelivers
	}
	ack()
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

func (s *session) userMessage(ctx context.Context, text string) {
	if s.fatal != "" {
		s.emitStatus("failed", s.fatal)
		return
	}
	if s.proc == nil {
		if s.rec.ClaudeSessionID == "" {
			s.emitError("There is no Claude Code session to resume.")
			s.emitStatus("failed", "There is no Claude Code session to resume.")
			return
		}
		if !s.d.capacityAvailable() {
			s.emitStatus("failed", fmt.Sprintf("archivist connect already runs %d live sessions.", s.d.maxSessions))
			return
		}
		s.emitStatus("starting", "Resuming the Claude Code session.")
		if err := s.spawn(ctx, s.rec.ClaudeSessionID); err != nil {
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
	s.log.Printf("no result %v after interrupt; terminating claude", interruptTimeout)
	s.stopProcess(true)
	s.emitError("Claude Code did not stop after the interrupt.")
	s.emitStatus("failed", "Claude Code did not stop after the interrupt; the next message resumes the session.")
}

// answerApproval turns a relay resolution into Claude's control response.
func (s *session) answerApproval(in *Inbound) {
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
	if s.rec.Cwd != "" {
		_ = os.RemoveAll(s.rec.Cwd)
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
		s.emitStatus("disconnected", "archivist connect stopped; the next message resumes the session.")
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
		if errors.As(err, &fatal) {
			s.log.Printf("session socket stopped: %v", err)
		}
		s.stopping = true
		s.stopProcess(false)
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

// ensureToken mints the session task token and writes the token file and
// MCP config (both 0600, in the private run dir outside the cwd).
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
	if err := auth.ValidateTokenFormat(tok.Token); err != nil || !auth.IsTaskToken(tok.Token) {
		s.revoke(tok)
		return fmt.Errorf("chat-api returned a malformed task token")
	}
	s.tokenFile = filepath.Join(dir, "task-token")
	s.mcpFile = filepath.Join(dir, "mcp.json")
	if err := writeFileAtomic(s.tokenFile, []byte(tok.Token+"\n")); err != nil {
		s.revoke(tok)
		return err
	}
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
	s.d.trackToken(tok.TokenID, true)
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
	if err == nil && !auth.IsTaskToken(tok.Token) {
		err = fmt.Errorf("malformed task token")
	}
	if err == nil {
		err = writeFileAtomic(s.tokenFile, []byte(tok.Token+"\n"))
		if err != nil {
			s.revoke(tok)
		}
	}
	if err != nil {
		if s.tokenRetry == 0 {
			s.tokenRetry = 5 * time.Second
		} else if s.tokenRetry < time.Minute {
			s.tokenRetry *= 2
		}
		s.log.Printf("task token refresh failed (current fp:%s), retrying in %v: %v", auth.Fingerprint(old.Token), s.tokenRetry, err)
		s.scheduleRefresh(s.tokenRetry)
		return
	}
	s.tokenRetry = 0
	s.token = tok
	s.d.trackToken(tok.TokenID, true)
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
