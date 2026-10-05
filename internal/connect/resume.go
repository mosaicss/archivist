package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Resume of an earlier sandbox session (Story 78.37). A paused sandbox task
// (session A) leaves its harness conversation in HOME (the Worker's resume
// archive restores .claude/projects, .archivist/connect/sessions and
// .archivist/connect/codex-home); `archivist connect --session B
// --resume-from A` continues it in session B: B reuses A's working directory
// path and harness id, Claude Code through --resume, Codex through
// thread/resume with A's Codex home moved to B's (the per session auth.json
// link is not archived; codexHome relinks it on spawn). When A cannot be
// used, or the resume fails, B starts fresh and its first answer begins with
// resumeMissNote.

// resumeMissNote is the text the first answer carries when the earlier
// conversation could not be restored.
const resumeMissNote = "The earlier conversation could not be restored, so this answer starts without it."

// resumeStart is how a session-bound start relates to an earlier session.
type resumeStart struct {
	// ok: the record carries the earlier session's harness id and cwd, and
	// the first spawn resumes it (falling back to a fresh start).
	ok bool
	// note: the earlier session was asked for but could not be used.
	note bool
}

// resumeSource loads the earlier session's record and checks it can be
// resumed by a new session of agent; it returns the record, or nil and why.
func (d *Daemon) resumeSource(from, agent string) (*SessionRecord, string) {
	rec, err := d.store.Load(from)
	if err != nil {
		if isNotExist(err) {
			return nil, "no record of it"
		}
		return nil, "its record is unreadable: " + err.Error()
	}
	switch {
	case rec.Agent != agent:
		return nil, fmt.Sprintf("it ran the %s agent, not %s", rec.Agent, agent)
	case rec.Status != "active":
		return nil, "it is " + rec.Status
	case !sessionCwdOK(rec.Cwd):
		return nil, "it has no usable working directory"
	}
	if agent == "codex" {
		if rec.CodexThreadID == "" {
			return nil, "it has no Codex thread"
		}
		home, err := d.store.CodexHomePath(from)
		if err != nil {
			return nil, err.Error()
		}
		if st, err := os.Lstat(home); err != nil || !st.IsDir() {
			return nil, "its Codex home is missing"
		}
		return rec, ""
	}
	if rec.ClaudeSessionID == "" {
		return nil, "it has no Claude Code session"
	}
	return rec, ""
}

// takeCodexHome moves the earlier session's Codex home (rollouts, thread
// state) to the new session's.
func (d *Daemon) takeCodexHome(from, to string) error {
	src, err := d.store.CodexHomePath(from)
	if err != nil {
		return err
	}
	dst, err := d.store.CodexHomePath(to)
	if err != nil {
		return err
	}
	d.store.RemoveCodexHome(to) // a new session has none; a leftover is stale
	return os.Rename(src, dst)
}

// giveBackCodexHome undoes takeCodexHome when the new record was not saved.
func (d *Daemon) giveBackCodexHome(from, to string) {
	src, err1 := d.store.CodexHomePath(to)
	dst, err2 := d.store.CodexHomePath(from)
	if err1 == nil && err2 == nil {
		_ = os.Rename(src, dst)
	}
}

// retire ends the earlier session's record once a new session took its
// conversation over: no working directory or harness id is left in it, so
// nothing resumes it again or removes the directory the new session uses.
func (d *Daemon) retire(src *SessionRecord, by string) {
	src.Status = "ended"
	src.Cwd, src.ClaudeSessionID, src.CodexThreadID = "", "", ""
	if err := d.store.Save(src); err != nil {
		d.log.Printf("earlier session record not retired: %v", err)
	}
	d.store.RemoveRunDir(src.SessionID)
	d.log.Printf("session %s continues the conversation of session %s", by, src.SessionID)
}

// firstSpawn starts the harness for a new session: fresh, or resuming the
// earlier session's conversation. A resume that fails synchronously (Codex:
// thread/resume or its proof; either: the launch) starts fresh instead,
// with the note on the first answer.
func (s *session) firstSpawn(ctx context.Context) error {
	if !s.resuming {
		return s.spawn(ctx, "")
	}
	s.log.Printf("resuming the earlier %s conversation", s.harness())
	err := s.spawn(ctx, s.resumeID())
	if err == nil {
		if s.isCodex() {
			s.resumeProved() // thread/resume answered and passed the proof
		}
		return nil // Claude: proved by its init frame (resumeProved), else resumeExited
	}
	s.resuming, s.resumeTexts = false, nil
	if errors.Is(err, errCapacity) || ctx.Err() != nil {
		return err
	}
	s.log.Printf("%s could not resume the earlier conversation, starting fresh: %v", s.harness(), err)
	s.forgetResume()
	return s.spawn(ctx, "")
}

// resumeProved ends the fallback window: the harness took the resume.
func (s *session) resumeProved() {
	if s.resuming {
		s.resuming, s.resumeTexts = false, nil
		s.log.Printf("the earlier %s conversation was resumed", s.harness())
	}
}

// resumeExited handles a resumed Claude Code that exited before its init
// frame (it reports "No conversation found with session ID" and exits when
// the transcript is missing): it starts fresh, resends the texts the
// resumed process never answered and reports true. Otherwise false.
// wasRunning is whether that process had passed its init (read before
// stopProcess clears it).
func (s *session) resumeExited(ctx context.Context, exitErr error, wasRunning bool) bool {
	if !s.resuming || wasRunning || s.isCodex() {
		return false
	}
	texts := s.resumeTexts
	s.resuming, s.resumeTexts = false, nil
	s.log.Printf("claude exited before resuming the earlier conversation (%v); starting fresh", exitErr)
	s.forgetResume()
	if err := s.spawn(ctx, ""); err != nil {
		s.failStart(err)
		return true
	}
	for _, text := range texts {
		s.sendUser(text)
	}
	return true
}

// forgetResume drops the earlier conversation: the session continues as a
// fresh one and its next answer starts with the note.
func (s *session) forgetResume() {
	s.rec.ClaudeSessionID, s.rec.CodexThreadID = "", ""
	s.save()
	s.note = resumeMissNote
}

// emitNote sends the pending note as a text part of the turn just started,
// ending in a blank line so it never runs into the answer's own text.
func (s *session) emitNote() {
	note := s.note
	s.note = ""
	id := "archivist-note-" + s.outbox.RunID()
	s.emit(Chunk{"type": "text-start", "id": id}, Chunk{"type": "text-delta", "id": id, "delta": note + "\n\n"},
		Chunk{"type": "text-end", "id": id})
}
