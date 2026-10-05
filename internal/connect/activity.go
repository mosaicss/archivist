package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Sandbox activity file (Story 78.37). In the session-bound mode with
// Config.ActivityFile set (ARCHIVIST_ACTIVITY_FILE), the daemon tells the
// sandbox Worker since when the session has been quiet and since when an
// approval has waited, so the Worker's alarm can pause an idle sandbox:
//
//	{"v":1,"idleSince":<unix ms>|null,"approvalSince":<unix ms>|null,
//	 "login":"claude.ai"|"console"|"api_key"}
//
// login (Story 78.38) is the login a Claude session runs on, present once
// its spawn proof passed (after a sign in or on the login it started with);
// it is absent for Codex and before that. The file is rewritten atomically
// (0600) only when a value changes. The
// Worker measures the windows on its own clock from when it first saw a
// value, so this clock only names the period, never its length.

// activityVersion is the activity file's "v".
const activityVersion = 1

// activityRecord is the activity file's JSON.
type activityRecord struct {
	V             int    `json:"v"`
	IdleSince     *int64 `json:"idleSince"`
	ApprovalSince *int64 `json:"approvalSince"`
	Login         string `json:"login,omitempty"`
}

// sandboxStopDeny answers every pending approval when the sandbox stops
// (a pause TERMs the daemon): the harness ends its turn instead of waiting.
const sandboxStopDeny = "Denied: the sandbox stopped before an approval arrived."

// connectStopDeny is the same answer when the local daemon stops (Ctrl-C).
const connectStopDeny = "Denied: archivist connect stopped before an approval arrived."

// pendingApprovals counts the approvals waiting for the relay (both harnesses).
func (s *session) pendingApprovals() int {
	n := len(s.pending)
	if s.cx != nil {
		n += len(s.cx.pending)
	}
	return n
}

// sandboxQuiet reports a session nobody is waiting on: no harness turn in
// flight (a turn silent for hours still counts as in flight), no pending
// approval, nothing queued or held for the init proof, no interrupt
// outstanding and not stopping. Unlike idleNow it needs no live process: a
// sandbox whose harness exited between turns is quiet too.
func (s *session) sandboxQuiet() bool {
	return !s.turnActive && !s.stopping && s.pendingApprovals() == 0 && len(s.queue) == 0 &&
		len(s.held) == 0 && len(s.early) == 0 && s.interruptTimer == nil
}

// writeActivity updates the idle and approval periods and rewrites the
// activity file when either changed (or was never written). A failed
// write is retried on the next loop pass and logged once per failure run.
func (s *session) writeActivity(now time.Time) {
	path := s.d.activityFile
	if !s.d.sandbox || path == "" {
		return
	}
	if s.sandboxQuiet() {
		if s.idleSince == 0 {
			s.idleSince = now.UnixMilli()
		}
	} else {
		s.idleSince = 0
	}
	if s.pendingApprovals() > 0 {
		if s.approvalSince == 0 {
			s.approvalSince = now.UnixMilli()
		}
	} else {
		s.approvalSince = 0
	}
	login := s.activityLogin()
	if s.actWritten && s.actIdle == s.idleSince && s.actApproval == s.approvalSince && s.actLogin == login {
		return
	}
	rec := activityRecord{V: activityVersion, IdleSince: msOrNil(s.idleSince), ApprovalSince: msOrNil(s.approvalSince),
		Login: login}
	data, _ := json.Marshal(rec)
	if err := writeActivityFile(path, data); err != nil {
		if !s.actFailed {
			s.log.Printf("activity file %s not written: %v", path, err)
		}
		s.actFailed = true
		return
	}
	s.actFailed = false
	s.actWritten, s.actIdle, s.actApproval, s.actLogin = true, s.idleSince, s.approvalSince, login
}

// activityLogin is the activity file's login: the Claude login the session
// runs on once its spawn proof passed, "" for Codex or before that.
func (s *session) activityLogin() string {
	if !s.loginProved || s.isCodex() {
		return ""
	}
	return s.claudeLogin().wire()
}

// writeActivityFile writes the activity file through writeFileAtomic,
// creating its directory (0700) when missing.
func writeActivityFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// msOrNil is a unix ms value, nil for 0 (JSON null).
func msOrNil(ms int64) *int64 {
	if ms == 0 {
		return nil
	}
	return &ms
}
