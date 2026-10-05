package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// Story 78.18: publish_artifact, data-artifact emission and the LRU park.

var testDigest = strings.Repeat("0a", 32)

func TestArtifactTrackerShapes(t *testing.T) {
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	result := `{"artifactId":"3f1c","sessionId":"s","name":"r.pdf","mediaType":"application/pdf","size":9,"digest":"` + testDigest + `","createdAt":1,"url":"https://evil.example/x"}`
	var structured map[string]any
	_ = decodeNumbers([]byte(result), &structured)
	cases := []struct {
		name   string
		tool   string
		output any
		ok     bool
	}{
		{"claude blocks", "mcp__archivist__publish_artifact", []any{map[string]any{"type": "text", "text": result}}, true},
		{"claude string", "mcp__archivist__publish_artifact", result, true},
		{"codex structured", "archivist.publish_artifact", map[string]any{"content": []any{}, "structuredContent": structured}, true},
		{"codex text", "archivist.publish_artifact", map[string]any{"content": []any{map[string]any{"type": "text", "text": result}}}, true},
		{"other tool", "mcp__archivist__search", result, false},
		{"other server", "evil.publish_artifact", result, false},
		{"not json", "mcp__archivist__publish_artifact", "published!", false},
		{"bad digest", "mcp__archivist__publish_artifact", strings.Replace(result, testDigest, "ABC", 1), false},
		{"bad type", "mcp__archivist__publish_artifact", strings.Replace(result, "application/pdf", "image/png", 1), false},
	}
	for _, c := range cases {
		tr := newArtifactTracker("http://chat.test/")
		out := tr.observe([]Chunk{
			{"type": "tool-input-start", "toolCallId": "t1", "toolName": c.tool},
			{"type": "tool-input-available", "toolCallId": "t1", "toolName": c.tool, "input": map[string]any{"path": "r.pdf"}},
			{"type": "tool-output-available", "toolCallId": "t1", "output": c.output},
		})
		if !c.ok {
			if len(out) != 3 {
				t.Errorf("%s: emitted %v", c.name, out[3:])
			}
			continue
		}
		if len(out) != 4 || out[3]["type"] != "data-artifact" {
			t.Fatalf("%s: chunks %v", c.name, out)
		}
		data := out[3]["data"].(map[string]any)
		want := map[string]any{"artifactId": "3f1c", "name": "r.pdf", "mediaType": "application/pdf", "digest": testDigest,
			"url": "http://chat.test/artifacts/3f1c"}
		for k, v := range want {
			if data[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, data[k], v)
			}
		}
		if len(data) != len(want) {
			t.Errorf("%s: extra fields %v", c.name, data)
		}
		raw, _ := json.Marshal(out[3])
		if _, err := parser.Parse(raw, "chunk"); err != nil {
			t.Errorf("%s: data-artifact invalid: %v", c.name, err)
		}
	}
	// A failed or denied call emits nothing, and its id is forgotten.
	for _, end := range []Chunk{
		{"type": "tool-output-error", "toolCallId": "t1", "errorText": "refused"},
		{"type": "tool-output-denied", "toolCallId": "t1"},
	} {
		tr := newArtifactTracker("http://chat.test")
		out := tr.observe([]Chunk{{"type": "tool-input-available", "toolCallId": "t1", "toolName": "mcp__archivist__publish_artifact"}, end,
			{"type": "tool-output-available", "toolCallId": "t1", "output": result}})
		if len(out) != 3 || len(tr.calls) != 0 {
			t.Errorf("%v: chunks %v", end["type"], out)
		}
	}
}

// writeCwd writes a file into the session's working directory.
func (h *harness) writeCwd(sid, name string, data []byte) {
	h.t.Helper()
	p := filepath.Join(h.record(sid).Cwd, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) artifacts(sid string) []map[string]any {
	var out []map[string]any
	for _, e := range h.relay.eventsOf(sid, "data-artifact") {
		out = append(out, e["payload"].(map[string]any)["data"].(map[string]any))
	}
	return out
}

var publishFiles = []struct{ name, mediaType, body string }{
	{"report.pdf", "application/pdf", "%PDF-1.7\n1 0 obj\n"},
	{"notes.txt", "text/plain", "plain notes\n"},
	{"summary.md", "text/markdown", "# Summary\n"},
	{"table.csv", "text/csv", "a,b\n1,2\n"},
	{"out/data.json", "application/json", `{"rows":[1,2]}`},
}

func TestClaudePublishArtifact(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	for _, f := range publishFiles {
		h.writeCwd(sid, f.name, []byte(f.body))
	}
	h.writeCwd(sid, "../outside.txt", []byte("outside"))
	turns, approvals := 1, 0
	for i, f := range publishFiles {
		h.message(sid, "publish "+f.name)
		approvals++
		req := h.approve(sid, approvals, "allow", "user", "allow_once")
		desc, _ := req["approvalDescriptor"].(map[string]any)
		if desc["tool_name"] != "mcp__archivist__publish_artifact" {
			t.Fatalf("approval for %v", desc["tool_name"])
		}
		turns++
		h.waitFinishes(sid, turns)
		arts := h.artifacts(sid)
		if len(arts) != i+1 {
			t.Fatalf("%s: %d data-artifact events", f.name, len(arts))
		}
		ups, _ := h.api.uploads()
		up := ups[len(ups)-1]
		sum := sha256.Sum256([]byte(f.body))
		got := arts[i]
		if got["artifactId"] != up.ID || got["name"] != filepath.Base(f.name) || got["mediaType"] != f.mediaType ||
			got["digest"] != hex.EncodeToString(sum[:]) || got["url"] != h.api.srv.URL+"/artifacts/"+up.ID || len(got) != 5 {
			t.Fatalf("%s: data-artifact %v", f.name, got)
		}
		if strings.Join(up.Fields, ",") != "sessionId,file" || up.SessionID != sid || up.MediaType != f.mediaType || string(up.Data) != f.body {
			t.Fatalf("%s: upload %+v", f.name, up)
		}
	}
	// The data-artifact follows the publish call's tool output.
	types := h.payloadTypes(sid)
	for i, typ := range types {
		if typ == "data-artifact" && types[i-1] != "tool-output-available" {
			t.Fatalf("data-artifact after %s", types[i-1])
		}
	}

	// Denied, refused (outside, symlink) and unknown: no upload, no artifact.
	_ = os.Symlink(filepath.Join(h.record(sid).Cwd, "notes.txt"), filepath.Join(h.record(sid).Cwd, "link.txt"))
	_, before := h.api.uploads()
	h.message(sid, "publish notes.txt")
	approvals++
	h.approve(sid, approvals, "deny", "user", "reject_once")
	turns++
	h.waitFinishes(sid, turns)
	for _, p := range []string{"../outside.txt", "link.txt", "missing.pdf"} {
		h.message(sid, "publish "+p)
		approvals++
		h.approve(sid, approvals, "allow", "user", "allow_once")
		turns++
		h.waitFinishes(sid, turns)
	}
	if _, after := h.api.uploads(); after != before {
		t.Fatalf("refused publishes uploaded (%d -> %d)", before, after)
	}
	if n := len(h.artifacts(sid)); n != len(publishFiles) {
		t.Fatalf("%d data-artifact events after refusals", n)
	}
	if errs := h.relay.eventsOf(sid, "tool-output-error"); len(errs) != 4 {
		t.Fatalf("%d tool errors", len(errs))
	}
	if len(h.relay.invalid) != 0 {
		t.Fatalf("relay saw invalid events: %v", h.relay.invalid)
	}
}

// "Allow for session" remembers publish_artifact like any tool, but nothing
// pre-allows it: the first call of a session always asks.
func TestClaudePublishNeedsApproval(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("echo hi")
	h.waitFinishes(sid, 1)
	runs := fakeRuns(t, h.home)
	if args := strings.Join(runs[0].Args, " "); strings.Contains(args, "publish_artifact") {
		t.Fatalf("publish_artifact pre-allowed: %s", args)
	}
	h.writeCwd(sid, "a.txt", []byte("a"))
	h.message(sid, "publish a.txt")
	waitFor(t, 20*time.Second, "approval request", func() bool { return len(h.relay.eventsOf(sid, "tool-approval-request")) == 1 })
	time.Sleep(300 * time.Millisecond)
	if ups, n := h.api.uploads(); n != 0 || len(ups) != 0 {
		t.Fatal("uploaded before approval")
	}
	h.approve(sid, 1, "allow", "user", "allow_once")
	h.waitFinishes(sid, 2)
	if len(h.artifacts(sid)) != 1 {
		t.Fatal("no artifact after approval")
	}
}

// A token without the publish scope: the MCP server never offers the tool,
// so the call fails with nothing uploaded and no artifact.
func TestClaudePublishWithoutScope(t *testing.T) {
	h := newHarness(t, nil)
	h.api.grantScopes = []string{"search", "read"}
	h.start()
	sid := h.startSession("mcp")
	h.waitFinishes(sid, 1)
	if !strings.Contains(h.relay.text(sid), "tools: companies_search,read_passage,read_section,search,toc") {
		t.Fatalf("tools without publish scope: %q", h.relay.text(sid))
	}
	h.writeCwd(sid, "a.txt", []byte("a"))
	h.message(sid, "publish a.txt")
	h.approve(sid, 1, "allow", "user", "allow_once")
	h.waitFinishes(sid, 2)
	if _, n := h.api.uploads(); n != 0 || len(h.artifacts(sid)) != 0 || !strings.Contains(h.relay.text(sid), "publish failed") {
		t.Fatalf("uploads %d artifacts %d text %q", n, len(h.artifacts(sid)), h.relay.text(sid))
	}
}

func TestCodexPublishArtifact(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("echo hi")
	h.waitFinishes(sid, 1)
	h.writeCwd(sid, "report.pdf", []byte("%PDF-1.4\n"))
	h.writeCwd(sid, "bad.json", []byte("{nope"))
	h.message(sid, "publish report.pdf")
	req := h.approve(sid, 1, "allow", "user", "allow_once")
	desc, _ := req["approvalDescriptor"].(map[string]any)
	meta, _ := desc["_meta"].(map[string]any)
	if meta["tool_name"] != "publish_artifact" {
		t.Fatalf("approval descriptor %v", desc)
	}
	h.waitFinishes(sid, 2)
	arts := h.artifacts(sid)
	ups, _ := h.api.uploads()
	if len(arts) != 1 || len(ups) != 1 || arts[0]["artifactId"] != ups[0].ID || arts[0]["mediaType"] != "application/pdf" ||
		arts[0]["url"] != h.api.srv.URL+"/artifacts/"+ups[0].ID {
		t.Fatalf("artifacts %v uploads %+v", arts, ups)
	}
	// Refused content: a tool error, nothing uploaded, no artifact.
	h.message(sid, "publish bad.json")
	h.approve(sid, 2, "allow", "user", "allow_once")
	h.waitFinishes(sid, 3)
	// Denied: nothing either.
	h.message(sid, "publish report.pdf")
	h.approve(sid, 3, "deny", "user", "reject_once")
	h.waitFinishes(sid, 4)
	if ups, n := h.api.uploads(); len(ups) != 1 || n != 1 || len(h.artifacts(sid)) != 1 {
		t.Fatalf("uploads %d/%d artifacts %d", len(ups), n, len(h.artifacts(sid)))
	}
	runs := codexRuns(t, h.home)
	if args := strings.Join(runs[0].Args, " "); !strings.Contains(args, `mcp_servers.archivist.tools.publish_artifact.approval_mode="prompt"`) ||
		!strings.Contains(args, `"--publish-session","`+sid+`","--publish-dir"`) {
		t.Fatalf("codex args %s", args)
	}
	if len(h.relay.invalid) != 0 {
		t.Fatalf("relay saw invalid events: %v", h.relay.invalid)
	}
}

// ─── LRU idle park ──────────────────────────────────────────────────────────

// waitIdle waits until the daemon sees the session idle (its loop
// published the finished turn).
func (h *harness) waitIdle(sid string) {
	h.t.Helper()
	waitFor(h.t, 10*time.Second, "idle "+sid[:8], func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		s := h.d.sessions[sid]
		return s != nil && s.idle
	})
}

func (h *harness) parked(sid string) int {
	n := 0
	for _, e := range h.relay.eventsOf(sid, "data-session-status") {
		d := e["payload"].(map[string]any)["data"].(map[string]any)
		if d["status"] == "disconnected" && strings.Contains(d["message"].(string), "make room") {
			n++
		}
	}
	return n
}

func TestLRUParksLeastRecentlyActiveIdle(t *testing.T) {
	h := newHarness(t, nil)
	h.max = 2
	h.start()
	a := h.startSession("echo a1")
	h.waitFinishes(a, 1)
	b := h.startSession("remember beta")
	h.waitFinishes(b, 1)
	h.waitIdle(b)
	h.message(a, "echo a2") // a is now more recently active than b
	h.waitFinishes(a, 2)
	h.waitIdle(a)

	c := h.startSession("echo c1")
	h.waitFinishes(c, 1)
	if h.parked(b) != 1 || h.parked(a) != 0 || h.d.slotsInUse() != 2 {
		t.Fatalf("parked b=%d a=%d, slots %d", h.parked(b), h.parked(a), h.d.slotsInUse())
	}
	if h.record(b).Status != "active" {
		t.Fatal("parked session not resumable")
	}
	h.waitIdle(c)

	// b's next message resumes it, parking a (now least recently active).
	h.message(b, "recall")
	h.waitFinishes(b, 2)
	if !strings.Contains(h.relay.text(b), "beta") {
		t.Fatalf("resumed b lost its context: %q", h.relay.text(b))
	}
	if h.parked(a) != 1 || h.parked(c) != 0 || h.d.slotsInUse() != 2 {
		t.Fatalf("parked a=%d c=%d, slots %d", h.parked(a), h.parked(c), h.d.slotsInUse())
	}
	var resumed bool
	for _, r := range fakeRuns(t, h.home) {
		if strings.Contains(strings.Join(r.Args, " "), "--resume=") {
			resumed = true
		}
	}
	if !resumed || len(h.relay.invalid) != 0 {
		t.Fatalf("resumed %v invalid %v", resumed, h.relay.invalid)
	}
}

// Every live session busy (mid turn or waiting for an approval): today's
// capacity refusal, nothing parked.
func TestLRUAllBusyRefuses(t *testing.T) {
	h := newHarness(t, nil)
	h.max = 2
	h.start()
	a := h.startSession("slow")
	b := h.startSession("bash ls")
	waitFor(t, 10*time.Second, "a streaming", func() bool { return strings.Contains(h.relay.text(a), "1\n") })
	waitFor(t, 10*time.Second, "b approval", func() bool { return len(h.relay.eventsOf(b, "tool-approval-request")) == 1 })
	c := h.startSession("echo c")
	h.relay.waitStatus(t, c, "failed", 1)
	last := h.relay.eventsOf(c, "data-session-status")
	if msg := last[len(last)-1]["payload"].(map[string]any)["data"].(map[string]any)["message"]; msg != "archivist connect already runs 2 live sessions." {
		t.Fatalf("refusal %v", msg)
	}
	if h.parked(a)+h.parked(b) != 0 || len(fakeRuns(t, h.home)) != 2 || h.d.slotsInUse() != 2 {
		t.Fatalf("parked %d runs %d slots %d", h.parked(a)+h.parked(b), len(fakeRuns(t, h.home)), h.d.slotsInUse())
	}
	h.approve(b, 1, "allow", "user", "allow_once")
	h.command(a, "interrupt")
	h.waitFinishes(b, 1)
}

func TestCodexLRUParkAndResume(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.max = 1
	h.start()
	a := h.startCodexSession("remember alpha")
	h.waitFinishes(a, 1)
	h.waitIdle(a)
	b := h.startCodexSession("echo b1")
	h.waitFinishes(b, 1)
	if h.parked(a) != 1 || h.d.slotsInUse() != 1 {
		t.Fatalf("parked a=%d slots %d", h.parked(a), h.d.slotsInUse())
	}
	h.waitIdle(b)
	h.message(a, "recall")
	h.waitFinishes(a, 2)
	if !strings.Contains(h.relay.text(a), "alpha") || h.parked(b) != 1 {
		t.Fatalf("text %q parked b=%d", h.relay.text(a), h.parked(b))
	}
	if _, err := os.Stat(filepath.Join(h.home, ".fakecodex", "thread-resume.json")); err != nil {
		t.Fatal("parked Codex session not resumed with thread/resume")
	}
}

// The session-bound sandbox mode (78.22, RunSession) never parks: a full
// daemon refuses at once and leaves the idle session alone.
func TestLRUNeverParksInSandboxMode(t *testing.T) {
	idle := &session{idle: true, done: make(chan struct{}), parkReq: make(chan chan bool)}
	d := &Daemon{maxSessions: 1, procs: 1, sandbox: true, sessions: map[string]*session{"other": idle}}
	got := make(chan bool, 1)
	go func() { got <- d.acquireSlot(nil) }()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("acquired a slot on a full sandbox daemon")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireSlot tried to park in sandbox mode")
	}
	if !idle.idle {
		t.Fatal("the idle session was claimed for parking")
	}
}
