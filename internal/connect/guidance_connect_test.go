package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/guidance"
)

// Story 78.31: every spawn injects Mosaic's research guidance (mosaic-ui
// full): Claude through --append-system-prompt-file, Codex through
// developerInstructions. Live from chat-api's GET /agent-guidance, else the
// embedded copy. The fake chat-api answers 404 unless a test serves a text.

const liveGuidance = "live mosaic-ui guidance for this test\n"

func TestClaudeSpawnInjectsEmbeddedGuidanceWhenChatAPIFails(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hello")
	h.waitFinishes(sid, 1)
	runs := fakeRuns(t, h.home)
	if len(runs) != 1 {
		t.Fatalf("claude runs %d", len(runs))
	}
	runDir := filepath.Join(h.home, ".archivist", "connect", "run", sid)
	path := filepath.Join(runDir, guidanceFileName)
	if i := slices.Index(runs[0].Args, "--append-system-prompt-file"); i < 0 || runs[0].Args[i+1] != path {
		t.Fatalf("args %v", runs[0].Args)
	}
	want := guidance.Embedded(guidance.SurfaceMosaicUI, guidance.FormFull)
	if runs[0].AppendSystemPrompt == nil || *runs[0].AppendSystemPrompt != want.Body {
		t.Fatal("Claude did not read the embedded mosaic-ui full guidance")
	}
	st, err := os.Stat(path)
	if err != nil || !modeIs(st.Mode(), 0o600) {
		t.Fatalf("guidance file %v %v", st, err)
	}
	if !strings.Contains(h.log.String(), "research guidance embedded ("+want.Digest+"; live fetch: status 404)") {
		t.Fatalf("log lacks the guidance source:\n%s", h.log.String())
	}
	// The daemon's fetch is unauthenticated (task mode mcp serve fetches its own compact form).
	h.api.mu.Lock()
	seen := slices.Clone(h.api.guidanceSeen)
	h.api.mu.Unlock()
	if !slices.Contains(seen, "mosaic-ui/full auth=") {
		t.Fatalf("guidance requests %v", seen)
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, " auth=") {
			t.Fatalf("guidance request carried credentials: %q", s)
		}
	}
}

func TestClaudeSpawnInjectsLiveGuidance(t *testing.T) {
	h := newHarness(t, nil)
	h.api.guidance = map[string]string{"mosaic-ui/full": liveGuidance}
	h.start()
	sid := h.startSession("echo hello")
	h.waitFinishes(sid, 1)
	runs := fakeRuns(t, h.home)
	if len(runs) != 1 || runs[0].AppendSystemPrompt == nil || *runs[0].AppendSystemPrompt != liveGuidance {
		t.Fatalf("runs %+v", runs)
	}
	if !strings.Contains(h.log.String(), "research guidance live ("+guidance.Digest(liveGuidance)+")") {
		t.Fatalf("log lacks the live guidance digest:\n%s", h.log.String())
	}
}

func TestClaudeGuidanceWriteFailureFailsTheSpawnClosed(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	s := h.api.addSession("claude")
	// A non-empty directory where the guidance file goes: the atomic write's rename fails.
	blocker := filepath.Join(h.home, ".archivist", "connect", "run", s.SessionID, guidanceFileName)
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	cid := "start-" + randomUUID()
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": cid, "sessionId": s.SessionID,
		"agent": "claude", "prompt": "echo hello"})
	waitFor(t, 20*time.Second, "failed", func() bool { return slices.Contains(h.relay.statuses(s.SessionID), "failed") })
	if runs := fakeRuns(t, h.home); len(runs) != 0 {
		t.Fatalf("claude started without its guidance: %d runs", len(runs))
	}
	errs := strings.Join(h.errorTexts(s.SessionID), " ")
	status := ""
	for _, ev := range h.relay.eventsOf(s.SessionID, "data-session-status") {
		if m, ok := ev["payload"].(map[string]any)["data"].(map[string]any)["message"].(string); ok {
			status += m + " "
		}
	}
	if !strings.Contains(errs+status, "research guidance") {
		t.Fatalf("failure does not name the guidance: errors %q status %q", errs, status)
	}
}

func TestCodexSpawnSendsDeveloperInstructions(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.api.guidance = map[string]string{"mosaic-ui/full": liveGuidance}
	h.start()
	sid := h.startCodexSession("echo hello codex")
	h.waitFinishes(sid, 1)
	var params map[string]any
	b, err := os.ReadFile(filepath.Join(h.home, ".fakecodex", "thread-start.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &params); err != nil {
		t.Fatal(err)
	}
	if params["developerInstructions"] != liveGuidance {
		t.Fatalf("thread/start developerInstructions %q", params["developerInstructions"])
	}
	if _, ok := params["baseInstructions"]; ok {
		t.Fatal("baseInstructions replaces Codex's base prompt and must never be sent")
	}

	// Resume after a daemon restart with chat-api's guidance gone: the embedded copy.
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	h.api.mu.Lock()
	h.api.guidance = nil
	h.api.mu.Unlock()
	h.start()
	h.message(sid, "echo again")
	waitFor(t, 20*time.Second, "thread/resume", func() bool {
		_, err := os.Stat(filepath.Join(h.home, ".fakecodex", "thread-resume.json"))
		return err == nil
	})
	b, _ = os.ReadFile(filepath.Join(h.home, ".fakecodex", "thread-resume.json"))
	params = nil
	_ = json.Unmarshal(b, &params)
	if params["developerInstructions"] != guidance.Embedded(guidance.SurfaceMosaicUI, guidance.FormFull).Body {
		t.Fatal("thread/resume did not carry the embedded guidance")
	}
	if _, ok := params["baseInstructions"]; ok {
		t.Fatal("baseInstructions on resume")
	}
}
