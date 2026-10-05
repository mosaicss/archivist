package connect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/fsutil"
)

// Matrix rows not covered by the lifecycle tests: interrupt timeout, task
// token refresh, resume without state, and allow_always.

func setDuration(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func (h *harness) errorTexts(sid string) []string {
	var out []string
	for _, e := range h.relay.eventsOf(sid, "error") {
		out = append(out, e["payload"].(map[string]any)["errorText"].(string))
	}
	return out
}

// Interrupt row, error handling: no result within the interrupt timeout →
// the process group is terminated, failed is emitted, and the next
// user_message resumes the same Claude session.
func TestMatrixInterruptTimeoutKillsAndResumes(t *testing.T) {
	setDuration(t, &interruptTimeout, 300*time.Millisecond)
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo first")
	h.waitFinishes(sid, 1)
	claudeID := h.record(sid).ClaudeSessionID
	first := fakeRuns(t, h.home)[0]

	h.message(sid, "stuck")
	waitFor(t, 10*time.Second, "stuck output", func() bool { return strings.Contains(h.relay.text(sid), "3\n") })
	h.command(sid, "interrupt")
	h.relay.waitStatus(t, sid, "failed", 1)
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "did not stop after the interrupt") {
		t.Fatalf("errors %v", errs)
	}
	waitFor(t, 10*time.Second, "group terminated", func() bool { return !processAlive(first.PID) })
	if len(h.relay.eventsOf(sid, "abort")) != 0 {
		t.Fatal("a hung harness must not be reported as a clean abort")
	}

	h.message(sid, "echo resumed after kill")
	waitFor(t, 20*time.Second, "resumed turn", func() bool { return strings.Contains(h.relay.text(sid), "resumed after kill") })
	runs := fakeRuns(t, h.home)
	if len(runs) != 2 {
		t.Fatalf("claude runs %d", len(runs))
	}
	resumed := false
	for _, r := range runs {
		for _, a := range r.Args {
			resumed = resumed || a == "--resume="+claudeID
		}
	}
	if !resumed {
		t.Fatalf("no run resumed %s", claudeID)
	}
}

// Token refresh row: near expiry the daemon re-mints, atomically rewrites
// the token file mcp serve reads per call, revokes the old token; a failing
// mint keeps the session and retries with backoff until it succeeds.
func TestMatrixTaskTokenRefresh(t *testing.T) {
	setDuration(t, &tokenRetryBase, 200*time.Millisecond)
	h := newHarness(t, nil)
	h.api.tokenTTL = 3 * time.Second // refresh due at once: lead (2 min) > TTL
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	tokenFile := filepath.Join(h.home, ".archivist", "connect", "run", sid, "task-token")

	waitFor(t, 15*time.Second, "two refreshes", func() bool {
		mints, _, _, revoked := h.api.tokenState()
		return mints >= 3 && len(revoked) >= 2
	})
	waitFor(t, 5*time.Second, "token file matches the one live token", func() bool {
		_, _, live, _ := h.api.tokenState()
		b, err := fsutil.ReadFile(tokenFile) // the daemon replaces it atomically
		return err == nil && len(live) == 1 && strings.TrimSpace(string(b)) == live[0]
	})
	if st, err := os.Stat(tokenFile); err != nil || !modeIs(st.Mode(), 0o600) {
		t.Fatalf("token file %v %v", st, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(tokenFile), ".task-token.tmp-*")); len(left) != 0 {
		t.Fatalf("atomic write left temp files %v", left)
	}

	// Failure path: two refresh attempts fail (the client retries a 503 POST
	// once, so 4 refused requests); the session keeps serving turns.
	mintsBefore, _, _, revokedBefore := h.api.tokenState()
	h.api.setFailMints(4)
	waitFor(t, 15*time.Second, "failed refreshes", func() bool {
		_, failed, _, _ := h.api.tokenState()
		return failed >= 4
	})
	if !strings.Contains(h.log.String(), "task token refresh failed") {
		t.Fatal("refresh failure not logged")
	}
	h.message(sid, "echo still serving")
	h.waitFinishes(sid, 2)
	waitFor(t, 15*time.Second, "refresh recovers", func() bool {
		mints, _, live, revoked := h.api.tokenState()
		return mints > mintsBefore && len(revoked) > len(revokedBefore) && len(live) == 1
	})
	if strings.Contains(h.log.String(), "mst_") {
		t.Fatal("log carries a raw task token")
	}

	// Quiet the rotation, then prove mcp serve reads the rewritten file.
	h.api.mu.Lock()
	h.api.tokenTTL = 10 * time.Minute
	h.api.mu.Unlock()
	time.Sleep(1500 * time.Millisecond) // one more refresh mints the long-lived token
	_, _, live, _ := h.api.tokenState()
	if len(live) != 1 {
		t.Fatalf("live tokens %d", len(live))
	}
	h.message(sid, "mcp")
	h.waitFinishes(sid, 3)
	bearers := h.api.researchBearers()
	if len(bearers) != 1 || bearers[0] != "Bearer "+live[0] {
		t.Fatalf("mcp serve used %v, live token fp differs", len(bearers))
	}
	if len(h.relay.eventsOf(sid, "tool-output-error")) != 0 {
		t.Fatal("the MCP call failed after rotation")
	}

	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if _, _, live, _ := h.api.tokenState(); len(live) != 0 {
		t.Fatalf("tokens left live: %d", len(live))
	}
}

// Resume row, error handling: a user_message for a session whose local
// state has no Claude session id fails with an error and starts nothing.
func TestMatrixResumeWithoutStateFails(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.api.addSession("claude")
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(h.tmp, "old-cwd")
	if err := st.Save(&SessionRecord{SessionID: sess.SessionID, Agent: "claude", StartCorrelationID: "start-old",
		Cwd: cwd, ExpiresAt: sess.ExpiresAt, Status: "active", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	h.start()
	sid := sess.SessionID
	waitFor(t, 10*time.Second, "session socket reattached", func() bool { return h.relay.dialCount(sid) >= 1 })
	cid := h.message(sid, "recall")
	waitFor(t, 10*time.Second, "ack", func() bool { return h.relay.acked(cid) })
	h.relay.waitStatus(t, sid, "failed", 1)
	if errs := h.errorTexts(sid); len(errs) != 1 || !strings.Contains(errs[0], "no Claude Code session to resume") {
		t.Fatalf("errors %v", errs)
	}
	time.Sleep(300 * time.Millisecond)
	if runs := fakeRuns(t, h.home); len(runs) != 0 {
		t.Fatalf("claude started without state: %v", runs)
	}
	if mints, _, _, _ := h.api.tokenState(); mints != 0 {
		t.Fatal("a task token was minted for a session that cannot resume")
	}
}

// Approval row: allow_always is remembered per tool name; a later ask for
// that tool is answered locally (no new tool-approval-request reaches the
// relay) while another tool still asks.
func TestMatrixAllowAlwaysRememberedPerTool(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("bash touch one")
	h.approve(sid, 1, "allow", "user", "allow_always")
	h.waitFinishes(sid, 1)

	h.message(sid, "bash touch two")
	h.waitFinishes(sid, 2)
	if n := len(h.relay.eventsOf(sid, "tool-approval-request")); n != 1 {
		t.Fatalf("approval requests %d after a remembered allow", n)
	}
	var outputs []any
	for _, e := range h.relay.eventsOf(sid, "tool-output-available") {
		outputs = append(outputs, e["payload"].(map[string]any)["output"])
	}
	if len(outputs) != 2 || outputs[1] != "ran: touch two" {
		t.Fatalf("outputs %v", outputs)
	}
	responses := h.relay.eventsOf(sid, "tool-approval-response")
	if len(responses) != 2 || responses[1]["payload"].(map[string]any)["approved"] != true {
		t.Fatalf("responses %v", responses)
	}
	scopes := h.relay.eventsOf(sid, "data-permission-scope")
	if len(scopes) != 1 || scopes[0]["payload"].(map[string]any)["data"].(map[string]any)["scope"] != "allow_always" {
		t.Fatalf("scope events %v", scopes)
	}

	// Another tool is not covered by the Bash rule.
	h.message(sid, "write notes.txt")
	h.approve(sid, 2, "deny", "user", "")
	h.waitFinishes(sid, 3)
	reqs := h.relay.eventsOf(sid, "tool-approval-request")
	if len(reqs) != 2 || reqs[1]["payload"].(map[string]any)["approvalDescriptor"].(map[string]any)["tool_name"] != "Write" {
		t.Fatalf("Write did not ask: %v", reqs)
	}
}
